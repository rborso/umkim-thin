package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rborso/umkim-thin/internal/update"
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

func TestUpdateAvailableComparesVersions(t *testing.T) {
	cases := []struct {
		installed, catalogued string
		want                  bool
	}{
		{"0.1.4", "0.1.5", true},
		{"0.1.5", "0.1.5", false},
		{"0.1.10", "0.1.9", false}, // ahead of the catalog: no downgrade
		{"0.2.0-rc1", "0.1.9", false},
		{"0.1.9", "0.2.0-rc1", true}, // a catalogued candidate is still newer
		{"", "0.1.5", false},
		{"0.1.4", "", false},
		{"dev", "0.1.5", false},
	}
	for _, c := range cases {
		if got := updateAvailable(c.installed, c.catalogued); got != c.want {
			t.Errorf("updateAvailable(%q, %q) = %v, want %v", c.installed, c.catalogued, got, c.want)
		}
	}
}

func TestEnvBoolKeepsDefaultOnGarbage(t *testing.T) {
	cases := []struct {
		value string
		def   bool
		want  bool
	}{
		{"", true, true},
		{"", false, false},
		{"true", false, true},
		{"1", false, true},
		{"false", true, false},
		{"0", true, false},
		{"yes please", true, true},
		{"yes please", false, false},
	}
	for _, c := range cases {
		t.Setenv("UMKIM_THIN_TEST_BOOL", c.value)
		if got := envBool("UMKIM_THIN_TEST_BOOL", c.def); got != c.want {
			t.Errorf("envBool(%q, %v) = %v, want %v", c.value, c.def, got, c.want)
		}
	}
}

func TestVersionHandlerServesTheCachedCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.0","html_url":"https://example.com/releases/v0.3.0"}`))
	}))
	defer srv.Close()

	checker := update.New(update.Config{Current: "0.2.0", Enabled: true, Endpoint: srv.URL})
	if err := checker.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	rec := httptest.NewRecorder()
	versionHandler(checker).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Current   string `json:"current"`
		Latest    string `json:"latest"`
		Available bool   `json:"available"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if got.Current != "0.2.0" || got.Latest != "0.3.0" || !got.Available || got.URL == "" {
		t.Fatalf("response = %+v", got)
	}
}
