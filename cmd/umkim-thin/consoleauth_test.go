package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < l.max; i++ {
		if !l.allow("client") {
			t.Fatalf("attempt %d denied before the limit", i+1)
		}
		l.fail("client")
	}
	if l.allow("client") {
		t.Fatal("the limiter allowed an attempt over the limit")
	}
	if !l.allow("other") {
		t.Fatal("one client's failures locked out another client")
	}
	l.reset("client")
	if !l.allow("client") {
		t.Fatal("reset did not clear the client's failures")
	}
}

// A hostile network can present many source addresses; the limiter must not
// grow without bound.
func TestLoginLimiterPrunesStaleKeys(t *testing.T) {
	l := newLoginLimiterN(5)
	l.pruneAt = 8
	for i := 0; i < 8; i++ {
		l.fail(fmt.Sprintf("198.51.100.%d", i))
	}
	// Age every failure past the window, then trip the threshold.
	l.mu.Lock()
	for k := range l.failures {
		l.failures[k] = []time.Time{time.Now().Add(-2 * l.window)}
	}
	l.mu.Unlock()
	l.fail("203.0.113.1")
	l.fail("203.0.113.2")
	l.mu.Lock()
	n := len(l.failures)
	l.mu.Unlock()
	if n > l.pruneAt {
		t.Fatalf("the limiter kept %d entries; stale keys were not pruned", n)
	}
	if n != 2 {
		t.Fatalf("want the two fresh keys, got %d entries", n)
	}
}

func TestJobStoreEvictsFinishedJobs(t *testing.T) {
	s := newJobStore()
	for i := 0; i < maxFinishedJobs+25; i++ {
		s.start("install", "app", func(progress func(string)) (any, error) {
			return nil, nil
		})
	}
	// Count completions rather than expecting every job to remain in the map:
	// the point is that the history is capped, so old ones are evicted.
	var done atomic.Int64
	for i := 0; i < maxFinishedJobs+25; i++ {
		s.start("install", "app", func(progress func(string)) (any, error) {
			done.Add(1)
			return nil, nil
		})
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if done.Load() == int64(maxFinishedJobs+25) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if done.Load() != int64(maxFinishedJobs+25) {
		t.Fatalf("only %d/%d jobs finished", done.Load(), maxFinishedJobs+25)
	}
	s.mu.Lock()
	n := len(s.m)
	s.mu.Unlock()
	if n > maxFinishedJobs {
		t.Fatalf("job history kept %d entries; bound is %d", n, maxFinishedJobs)
	}
}

func TestRandomIDIsNotZero(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := randomID()
		if id == "" || id == "0000000000000000" {
			t.Fatalf("randomID returned %q", id)
		}
		if seen[id] {
			t.Fatalf("randomID repeated %q", id)
		}
		seen[id] = true
	}
}

func TestClientKey(t *testing.T) {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "10.244.0.9:1234"
	if got := clientKey(r); got != "10.244.0.9" {
		t.Errorf("clientKey without XFF = %q", got)
	}

	// Traefik appends the address it saw, so the rightmost public hop is the
	// client and cannot be forged.
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.244.0.9")
	if got := clientKey(r); got != "203.0.113.7" {
		t.Errorf("clientKey with XFF = %q", got)
	}

	// A client that prepends a fresh spoofed entry on every request must still
	// land in one stable bucket: the limiter depends on it.
	first := ""
	for _, spoof := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		r.Header.Set("X-Forwarded-For", spoof+", 198.51.100.9, 10.244.0.9")
		got := clientKey(r)
		if got != "198.51.100.9" {
			t.Errorf("spoofed XFF %q gave bucket %q; want the proxy-written hop", spoof, got)
		}
		if first == "" {
			first = got
		}
		if got != first {
			t.Errorf("spoofing moved the bucket from %q to %q", first, got)
		}
	}

	// A direct, non-proxied caller cannot pick a bucket at all.
	direct := httptest.NewRequest("POST", "/login", nil)
	direct.RemoteAddr = "203.0.113.44:5555"
	direct.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := clientKey(direct); got != "203.0.113.44" {
		t.Errorf("direct client trusted XFF: %q", got)
	}

	// An external proxy in front of Traefik: both hops are private, so the
	// leftmost entry (the original client) is the bucket.
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 172.16.10.5, 10.244.0.9")
	if got := clientKey(r); got != "203.0.113.9" {
		t.Errorf("external proxy chain = %q", got)
	}
}

func TestLocalBreakglassPassword(t *testing.T) {
	a := &consoleAuth{localUser: "localadmin", localPass: "correct horse battery"}
	if !a.valid("localadmin", "correct horse battery") {
		t.Fatal("the correct breakglass password was rejected")
	}
	for _, pass := range []string{"", "wrong", "correct horse batter", "correct horse battery "} {
		if a.valid("localadmin", pass) {
			t.Fatalf("password %q was accepted", pass)
		}
	}
	a.localPass = ""
	if a.valid("localadmin", "") {
		t.Fatal("an empty configured password was accepted")
	}
}

func TestSessionSignVerify(t *testing.T) {
	a := &consoleAuth{key: []byte("test-session-key")}
	value := a.sign("admin", time.Now().Add(time.Hour).Unix())

	user, ok := a.verify(value)
	if !ok || user != "admin" {
		t.Fatalf("verify() = %q, %v", user, ok)
	}

	if _, ok := a.verify(value + "x"); ok {
		t.Fatal("a tampered signature was accepted")
	}
	if _, ok := a.verify("someone|1|abc"); ok {
		t.Fatal("a fabricated signature was accepted")
	}
	if _, ok := a.verify("admin|not-a-number|abc"); ok {
		t.Fatal("a malformed expiry was accepted")
	}

	expired := a.sign("admin", time.Now().Add(-time.Minute).Unix())
	if _, ok := a.verify(expired); ok {
		t.Fatal("an expired session was accepted")
	}
}

func TestSessionKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UMKIM_THIN_CONSOLE_SESSION_KEY_FILE", filepath.Join(dir, "missing-key"))
	t.Setenv("UMKIM_THIN_SECRETS_FILE", filepath.Join(dir, "missing-secrets"))
	t.Setenv("UMKIM_THIN_CONSOLE_LOCAL_PASSWORD_FILE", filepath.Join(dir, "missing-password"))

	a := newConsoleAuth()
	if len(a.key) != 32 {
		t.Fatalf("ephemeral session key is %d bytes, want 32", len(a.key))
	}

	b := newConsoleAuth()
	if string(a.key) == string(b.key) {
		t.Fatal("two starts reused the same ephemeral key")
	}
}

func TestSecretValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.yml")
	body := "# generated\nsecret_console_localadmin_password: \"s3cret\"\nother: 'x'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := secretValue(path, "secret_console_localadmin_password"); got != "s3cret" {
		t.Fatalf("secretValue() = %q", got)
	}
	if got := secretValue(path, "missing"); got != "" {
		t.Fatalf("missing key = %q", got)
	}
	if got := secretValue(filepath.Join(t.TempDir(), "nope"), "x"); got != "" {
		t.Fatalf("missing file = %q", got)
	}
}

func TestValidLocalAccount(t *testing.T) {
	a := &consoleAuth{
		adminUser: "admin",
		localUser: "localadmin",
		localPass: "breakglass",
	}
	if !a.valid("localadmin", "breakglass") {
		t.Fatal("the breakglass account was rejected")
	}
	if a.valid("localadmin", "wrong") {
		t.Fatal("the breakglass account accepted a wrong password")
	}
	if a.valid("", "") || a.valid("someone", "anything") {
		t.Fatal("an unknown account was accepted")
	}
}

func TestRequireAuth(t *testing.T) {
	a := &consoleAuth{key: []byte("test-session-key"), ttl: time.Hour}
	h := a.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// Open endpoints.
	for _, path := range []string{"/login", "/logout", "/api/invites/redeem", "/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s = %d, want %d", path, rec.Code, http.StatusNoContent)
		}
	}

	// An API request without a session is a 401, not a redirect.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/apps", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous API request = %d, want 401", rec.Code)
	}

	// A browser request is redirected to the login page.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous UI request = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// A valid session is let through.
	req := httptest.NewRequest(http.MethodGet, "/api/apps", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.sign("admin", time.Now().Add(time.Hour).Unix())})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("authenticated request = %d, want 204", rec.Code)
	}
}

func TestSecurityHeadersOnAPI(t *testing.T) {
	a := &consoleAuth{key: []byte("test-session-key")}
	h := securityHeaders(a.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/apps", nil))
	if !strings.HasPrefix(rec.Header().Get("Content-Security-Policy"), "default-src") {
		t.Fatalf("API response is missing the CSP: %q", rec.Header().Get("Content-Security-Policy"))
	}
}
