package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", http.NewServeMux())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 ||
		srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("server timeouts are not all set: %+v", srv)
	}
	if srv.MaxHeaderBytes <= 0 {
		t.Fatal("MaxHeaderBytes is not set")
	}
	if srv.ReadHeaderTimeout > srv.ReadTimeout {
		t.Fatal("ReadHeaderTimeout exceeds ReadTimeout")
	}
}

func TestMutatingRejectsForeignOrigin(t *testing.T) {
	mux := http.NewServeMux()
	mutating(mux, "POST /x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name   string
		origin string
		want   int
	}{
		{"foreign", "https://evil.example", http.StatusForbidden},
		{"same", "https://console.example", http.StatusNoContent},
		{"curl or CLI", "", http.StatusNoContent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://console.example/x", nil)
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func TestAuditLineHasActorAndAction(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	a := &consoleAuth{key: []byte("test-key")}
	r := httptest.NewRequest(http.MethodPost, "/api/invites", nil)
	r.AddCookie(&http.Cookie{
		Name:  sessionCookie,
		Value: a.sign("admin", time.Now().Add(time.Hour).Unix()),
	})
	audit(r, a, "invite-create", "someone@example.com", "group=default-tenant")

	line := buf.String()
	for _, part := range []string{
		`user="admin"`,
		"action=invite-create",
		`app="someone@example.com"`,
		"group=default-tenant",
	} {
		if !strings.Contains(line, part) {
			t.Fatalf("audit line %q is missing %q", line, part)
		}
	}
}

func TestTokenPrefixDoesNotLogTheToken(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := tokenPrefix(token); got != "01234567" || strings.Contains(got, token) {
		t.Fatalf("tokenPrefix = %q", got)
	}
	if got := tokenPrefix("short"); got != "[short]" {
		t.Fatalf("short token prefix = %q", got)
	}
}

// Ensure the public redemption endpoint is not wrapped by the origin check:
// the signup page posts it cross-origin from auth.<zone>.
func TestRedemptionRouteIsNotOriginChecked(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `mutating(mux, "POST /api/invites/redeem"`) {
		t.Fatal("the redemption route is wrapped by mutating()")
	}
}
