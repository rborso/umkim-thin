package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.2.0", 0},
		{"v0.2.0", "0.2.0", 0},
		{"0.2.0", "0.3.0", -1},
		{"0.3.0", "0.2.0", 1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.2", "0.2.0", 0},
		{"0.2.0+build.5", "0.2.0", 0},
		{"0.3.0-rc1", "0.3.0", -1},
		{"0.3.0-rc2", "0.3.0-rc1", 1},
		{"0.3.0-alpha", "0.3.0-rc1", -1},
		{"0.3.0-rc1", "0.3.0-beta", 1},
		{"0.3.0-1", "0.3.0-alpha", -1},
		{"garbage", "0.2.0", 0},
		{"0.2.0", "", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsVersion(t *testing.T) {
	for _, v := range []string{"0.2.0", "v0.2.0", "1.2", "1", "1.2.3-rc.1"} {
		if !IsVersion(v) {
			t.Errorf("IsVersion(%q) = false, want true", v)
		}
	}
	// A `git describe` build of the tagged commit is a pre-release of it: it
	// parses, and sorts before the release.
	if Compare("v0.2.0-3-gabc123", "0.2.0") >= 0 {
		t.Error("a git-describe build should sort before its release")
	}
	for _, v := range []string{"", "dev", "1.2.3.4", "x.y.z", "1.-2.3"} {
		if IsVersion(v) {
			t.Errorf("IsVersion(%q) = true, want false", v)
		}
	}
}

func TestDisabledAndUnknownVersion(t *testing.T) {
	c := New(Config{Current: "0.2.0", Enabled: false})
	if s := c.Status(); s.Enabled || s.Error == "" {
		t.Fatalf("disabled status = %+v, want enabled=false with a reason", s)
	}
	c = New(Config{Current: "dev", Enabled: true})
	if s := c.Status(); s.Enabled || s.Error == "" {
		t.Fatalf("unknown-version status = %+v, want enabled=false with a reason", s)
	}
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh on a disabled checker should report the reason")
	}
}

func TestRefreshFindsNewerRelease(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("User-Agent"); got != "umkim-thin/0.2.0" {
			t.Errorf("User-Agent = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tag_name":     "v0.3.0",
			"html_url":     "https://github.com/rborso/umkim-thin/releases/tag/v0.3.0",
			"published_at": "2026-11-01T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := New(Config{Current: "0.2.0", Enabled: true, Endpoint: srv.URL})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	s := c.Status()
	if !s.Enabled || !s.Available || s.Latest != "0.3.0" {
		t.Fatalf("status = %+v, want an available 0.3.0", s)
	}
	if s.URL == "" || s.CheckedAt.IsZero() || s.Error != "" {
		t.Fatalf("status = %+v, want url, checkedAt and no error", s)
	}

	// A second refresh inside the interval is the caller's job to skip; the
	// checker itself still answers from the endpoint when asked.
	if calls != 1 {
		t.Fatalf("endpoint called %d times, want 1", calls)
	}
}

func TestRefreshUpToDateAndOlderRelease(t *testing.T) {
	for _, tag := range []string{"v0.2.0", "v0.1.9"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
		}))
		c := New(Config{Current: "0.2.0", Enabled: true, Endpoint: srv.URL})
		if err := c.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh(%s): %v", tag, err)
		}
		if s := c.Status(); s.Available {
			t.Fatalf("tag %s: Available = true, want false (%+v)", tag, s)
		}
		srv.Close()
	}
}

func TestRefreshToleratesFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	c := New(Config{Current: "0.2.0", Enabled: true, Endpoint: srv.URL})
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh should report a non-200 response")
	}
	s := c.Status()
	if s.Available || s.Error == "" || !s.Enabled {
		t.Fatalf("status after failure = %+v, want no update and an error", s)
	}
}

func TestRefreshRejectsUnparsableTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "nightly-2026-11-01"})
	}))
	defer srv.Close()
	c := New(Config{Current: "0.2.0", Enabled: true, Endpoint: srv.URL})
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh should reject a non-version tag")
	}
	if s := c.Status(); s.Available || s.Error == "" {
		t.Fatalf("status = %+v, want no update and an error", s)
	}
}

func TestStateFileCachesAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "update-check.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.0"})
	}))
	defer srv.Close()

	first := New(Config{Current: "0.2.0", Enabled: true, Endpoint: srv.URL, StateFile: state})
	if err := first.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	// A fresh process (a pod restart) reuses the cached answer.
	second := New(Config{Current: "0.2.0", Enabled: true, Endpoint: srv.URL, StateFile: state})
	if s := second.Status(); !s.Available || s.Latest != "0.3.0" || s.CheckedAt.IsZero() {
		t.Fatalf("cached status = %+v, want the stored 0.3.0", s)
	}

	// After an upgrade the cache belongs to another version and is ignored.
	third := New(Config{Current: "0.3.0", Enabled: true, Endpoint: srv.URL, StateFile: state})
	if s := third.Status(); s.Latest != "" || s.Available {
		t.Fatalf("status after upgrade = %+v, want an empty cache", s)
	}
}

func TestStartStopsWithContext(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.0"})
	}))
	defer srv.Close()

	c := New(Config{Current: "0.2.0", Enabled: true, Endpoint: srv.URL, Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Start(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for c.Status().Latest == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Status().Latest != "0.3.0" {
		t.Fatalf("start did not warm the cache: %+v", c.Status())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not stop with its context")
	}
	if calls == 0 {
		t.Fatal("endpoint was never called")
	}
}
