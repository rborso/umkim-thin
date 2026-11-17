package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rborso/umkim-thin/internal/identity"
)

// sessionCookie names the console's own signed session.
const sessionCookie = "umkim_thin_session"

// consoleAuth locks the console behind its own login, independent of Authelia:
// the directory admin (validated against LLDAP) and a local breakglass account
// that works even when the directory or portal is unavailable.
type consoleAuth struct {
	adminUser     string
	localUser     string
	localPass     string
	key           []byte
	ttl           time.Duration
	directory     *identity.Directory
	limiter       *loginLimiter
	redeemLimiter *loginLimiter
}

// loginLimiter is a small in-memory failed-login window. The console is a
// single replica; a restart clears it, which is acceptable for a breakglass
// login path.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	max      int
	window   time.Duration
	// pruneAt bounds the map: a busy or hostile network can present many source
	// addresses, and entries were previously only removed when that same key
	// tried again.
	pruneAt int
}

func newLoginLimiter() *loginLimiter { return newLoginLimiterN(5) }

func newLoginLimiterN(max int) *loginLimiter {
	return &loginLimiter{
		failures: map[string][]time.Time{},
		max:      max,
		window:   5 * time.Minute,
		pruneAt:  4096,
	}
}

// prune drops entries with no failure inside the window. Caller holds the lock.
func (l *loginLimiter) prune(now time.Time) {
	for key, times := range l.failures {
		keep := times[:0]
		for _, t := range times {
			if now.Sub(t) < l.window {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 {
			delete(l.failures, key)
		} else {
			l.failures[key] = keep
		}
	}
}

func (l *loginLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) > l.pruneAt {
		l.prune(now)
	}
	recent := l.failures[key][:0]
	for _, t := range l.failures[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(l.failures, key)
	} else {
		l.failures[key] = recent
	}
	return len(recent) < l.max
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[key] = append(l.failures[key], time.Now())
	if len(l.failures) > l.pruneAt {
		l.prune(time.Now())
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// clientKey identifies a client for the login and redeem limiters.
//
// The console only ever sees its immediate peer: Traefik (in-cluster), or an
// external reverse proxy in front of it. Trusting the *first* X-Forwarded-For
// entry, as this used to, lets a client pick its own limiter bucket by sending
// a fresh X-Forwarded-For on every request and brute-force the login, so the
// bucket has to be derived from the part of the chain the client cannot forge.
//
// Rules:
//   - peer is not private/loopback -> nothing in XFF is trustworthy; use the
//     peer address (a direct, non-proxied caller).
//   - peer is private/loopback (Traefik/proxy) -> walk XFF from the right and
//     skip private/loopback hops; the first public address is the client. If
//     every hop is private, use the leftmost (a fully private lab chain).
//
// A client can still forge entries to the *left* of a proxy, but the rightmost
// public hop is written by the proxy, so it stays stable per client.
func clientKey(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		peer = host
	}
	if !isPrivateOrLoopback(peer) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	var hops []string
	for _, h := range strings.Split(xff, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hops = append(hops, h)
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !isPrivateOrLoopback(hops[i]) {
			return hops[i]
		}
	}
	if len(hops) > 0 {
		return hops[0]
	}
	return peer
}

// isPrivateOrLoopback reports whether an address is one we would accept as a
// proxy hop (RFC1918, IPv6 ULA, link-local or loopback). Anything else is a
// real client, which is what the limiter buckets on.
func isPrivateOrLoopback(s string) bool {
	ip := net.ParseIP(strings.Trim(s, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func newConsoleAuth() *consoleAuth {
	secrets := envOr("UMKIM_THIN_SECRETS_FILE", "/etc/umkim-thin/secrets.yml")
	localPass := secretValue(secrets, "secret_console_localadmin_password")
	if localPass == "" {
		// Fall back to the older standalone file, if present.
		localPass = readFileTrim(envOr("UMKIM_THIN_CONSOLE_LOCAL_PASSWORD_FILE", "/etc/umkim-thin/console-localadmin-password"))
	}
	keyFile := envOr("UMKIM_THIN_CONSOLE_SESSION_KEY_FILE", "/etc/umkim-thin/console-session-key")
	key := []byte(readFileTrim(keyFile))
	if len(key) == 0 {
		// Fail closed: never sign sessions with an empty HMAC key. A standalone
		// run without the provisioned state gets a random key instead, which
		// invalidates sessions on restart but cannot be forged.
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			log.Fatalf("generating an ephemeral console session key: %v", err)
		}
		log.Printf("WARNING: no console session key at %s; using an ephemeral key (sessions end on restart)", keyFile)
	}
	return &consoleAuth{
		adminUser:     envOr("UMKIM_THIN_CONSOLE_ADMIN_USER", "admin"),
		localUser:     envOr("UMKIM_THIN_CONSOLE_LOCAL_USER", "localadmin"),
		localPass:     localPass,
		key:           key,
		ttl:           12 * time.Hour,
		directory:     identity.New(identity.ConfigFromEnv()),
		limiter:       newLoginLimiter(),
		redeemLimiter: newLoginLimiterN(10),
	}
}

// secretValue reads `key: "value"` from a simple secrets file.
func secretValue(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// valid accepts the breakglass account or the directory admin.
func (a *consoleAuth) valid(user, pass string) bool {
	if a.localUser != "" && user == a.localUser && a.localPass != "" && constTimeEqual(pass, a.localPass) {
		return true
	}
	if user != "" && user == a.adminUser {
		return a.directory.Authenticate(user, pass)
	}
	return false
}

// constTimeEqual compares two secrets without leaking their length through a
// short-circuit. Hashing first gives both sides a fixed size.
func constTimeEqual(a, b string) bool {
	ah, bh := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return hmac.Equal(ah[:], bh[:])
}

func (a *consoleAuth) sign(user string, exp int64) string {
	mac := hmac.New(sha256.New, a.key)
	fmt.Fprintf(mac, "%s|%d", user, exp)
	return fmt.Sprintf("%s|%d|%s", user, exp, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

func (a *consoleAuth) verify(val string) (string, bool) {
	parts := strings.Split(val, "|")
	if len(parts) != 3 {
		return "", false
	}
	mac := hmac.New(sha256.New, a.key)
	fmt.Fprintf(mac, "%s|%s", parts[0], parts[1])
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[2]), []byte(want)) {
		return "", false
	}
	var exp int64
	if _, err := fmt.Sscanf(parts[1], "%d", &exp); err != nil {
		return "", false
	}
	if time.Now().Unix() > exp {
		return "", false
	}
	return parts[0], true
}

func (a *consoleAuth) currentUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	return a.verify(c.Value)
}

// requireAuth guards the console and its API. The login endpoints and the
// invite redemption endpoint are open: redemption is authenticated by the
// invite token the invitee holds, not by a console session.
func (a *consoleAuth) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login", "/logout", "/api/invites/redeem", "/healthz", "/readyz":
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.currentUser(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (a *consoleAuth) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user := strings.TrimSpace(r.FormValue("username"))
	pass := r.FormValue("password")
	key := clientKey(r)
	if !a.limiter.allow(key) {
		http.Error(w, "too many login attempts; try again later", http.StatusTooManyRequests)
		return
	}
	if !a.valid(user, pass) {
		a.limiter.fail(key)
		log.Printf("audit user=%q action=login-failed remote=%s", user, key)
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	a.limiter.reset(key)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.sign(user, time.Now().Add(a.ttl).Unix()),
		Path:     "/",
		MaxAge:   int(a.ttl.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *consoleAuth) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reads a boolean env var. An unparsable value keeps the default, so a
// typo in config.env cannot silently switch a feature on or off.
func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
