package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestClientKey(t *testing.T) {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "10.244.0.9:1234"
	if got := clientKey(r); got != "10.244.0.9" {
		t.Errorf("clientKey without XFF = %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.244.0.9")
	if got := clientKey(r); got != "203.0.113.7" {
		t.Errorf("clientKey with XFF = %q", got)
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
