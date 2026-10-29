// Package update reports whether a newer umkim-thin release exists.
//
// The catalog and the version are baked into the console image, so a running
// console cannot know that a newer release exists on its own. This package
// asks the public release metadata for the newest tag and compares it with the
// running version.
//
// It is deliberately read-only: it never downloads or installs anything, it
// caches its answer (also across restarts), and a failed check is reported as
// "unknown" instead of surfacing an error to the operator. Air-gapped installs
// turn it off with UMKIM_THIN_UPDATE_CHECK=false.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultEndpoint is the release metadata the console reads by default. It is
// the public GitHub API for the project's latest release; a mirror can serve
// the same shape (tag_name/html_url) at another URL.
const DefaultEndpoint = "https://api.github.com/repos/rborso/umkim-thin/releases/latest"

// DefaultInterval is how long a result is reused before the next check.
const DefaultInterval = 6 * time.Hour

// DefaultTimeout bounds one request. A console page load must never wait on it:
// checks run in the background.
const DefaultTimeout = 8 * time.Second

// Config configures a Checker.
type Config struct {
	// Current is the running platform version, e.g. "0.2.0". Empty, "dev" or
	// "unknown" disables the comparison (a locally built image has no tag).
	Current string
	// Enabled turns the check on. The console sets it from
	// UMKIM_THIN_UPDATE_CHECK (default true).
	Enabled bool
	// Endpoint is the release metadata URL. Empty uses DefaultEndpoint.
	Endpoint string
	// Interval is the cache lifetime. Zero uses DefaultInterval.
	Interval time.Duration
	// Timeout bounds one request. Zero uses DefaultTimeout.
	Timeout time.Duration
	// StateFile caches the last result on disk so a pod restart does not
	// trigger a new request. Empty disables persistence.
	StateFile string
	// UserAgent identifies the caller. Empty uses "umkim-thin/<Current>".
	UserAgent string
	// HTTP is the client to use. Zero uses a client with Timeout.
	HTTP *http.Client
}

// Status is the answer the console renders. It is safe to serve as JSON.
type Status struct {
	// Enabled is false when the check is switched off or the running version
	// is unknown; Error then says why.
	Enabled bool `json:"enabled"`
	// Current is the running platform version.
	Current string `json:"current,omitempty"`
	// Latest is the newest published version, when known.
	Latest string `json:"latest,omitempty"`
	// Available is true when Latest is newer than Current.
	Available bool `json:"available"`
	// URL is the release page for Latest.
	URL string `json:"url,omitempty"`
	// Published is the release date, when the endpoint reports one.
	Published string `json:"published,omitempty"`
	// CheckedAt is when the last successful check ran.
	CheckedAt time.Time `json:"checkedAt,omitempty"`
	// Error is the reason the last check did not produce an answer. It is
	// informational: the console shows the rest of the page normally.
	Error string `json:"error,omitempty"`
}

// Checker holds the cached status and refreshes it in the background.
type Checker struct {
	cfg Config

	mu         sync.Mutex
	status     Status
	refreshing bool
}

// New returns a checker. It loads a cached result from cfg.StateFile when one
// is present and matches the running version.
func New(cfg Config) *Checker {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "umkim-thin/" + firstNonEmpty(cfg.Current, "unknown")
	}
	c := &Checker{cfg: cfg, status: initialStatus(cfg)}
	c.loadState()
	return c
}

// Status returns the last result without blocking on the network.
func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Start refreshes in the background until ctx is cancelled: once immediately
// (when the cache is empty or stale), then every Interval.
func (c *Checker) Start(ctx context.Context) {
	if !c.enabled() {
		return
	}
	if s := c.Status(); s.CheckedAt.IsZero() || time.Since(s.CheckedAt) >= c.cfg.Interval {
		go func() { _ = c.Refresh(ctx) }()
	}
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			go func() { _ = c.Refresh(ctx) }()
		}
	}
}

// Refresh checks the endpoint now. Concurrent calls collapse into the running
// one, so a burst of requests cannot stampede the endpoint.
func (c *Checker) Refresh(ctx context.Context) error {
	if !c.enabled() {
		return errors.New(c.Status().Error)
	}
	c.mu.Lock()
	if c.refreshing {
		c.mu.Unlock()
		return nil
	}
	c.refreshing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()

	rel, err := c.fetch(ctx)
	if err != nil {
		c.set(func(s *Status) { s.Error = err.Error() })
		return err
	}
	latest := normalize(rel.tag())
	if !IsVersion(latest) {
		err := fmt.Errorf("release %q is not a version", rel.tag())
		c.set(func(s *Status) { s.Error = err.Error() })
		return err
	}
	c.set(func(s *Status) {
		s.Enabled = true
		s.Latest = latest
		s.URL = rel.link()
		s.Published = firstNonEmpty(rel.PublishedAt, rel.Published)
		s.Available = Compare(latest, c.cfg.Current) > 0
		s.CheckedAt = time.Now().UTC()
		s.Error = ""
	})
	c.saveState()
	return nil
}

// release is the subset of the GitHub releases API the check needs. The
// version/url/published aliases let a plain static manifest serve the same
// check without a GitHub-compatible API.
type release struct {
	TagName     string `json:"tag_name"`
	Version     string `json:"version"`
	HTMLURL     string `json:"html_url"`
	URL         string `json:"url"`
	PublishedAt string `json:"published_at"`
	Published   string `json:"published"`
}

func (r release) tag() string { return strings.TrimSpace(firstNonEmpty(r.TagName, r.Version)) }

// link prefers the human release page over the API URL.
func (r release) link() string { return firstNonEmpty(r.HTMLURL, r.URL) }

func (c *Checker) fetch(ctx context.Context) (release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Endpoint, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return release{}, fmt.Errorf("release check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return release{}, fmt.Errorf("release check failed: HTTP %d", resp.StatusCode)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return release{}, fmt.Errorf("release check failed: %w", err)
	}
	return rel, nil
}

func (c *Checker) enabled() bool {
	return c.cfg.Enabled && IsVersion(c.cfg.Current)
}

func (c *Checker) set(mutate func(*Status)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mutate(&c.status)
}

func (c *Checker) loadState() {
	if c.cfg.StateFile == "" || !c.enabled() {
		return
	}
	raw, err := os.ReadFile(c.cfg.StateFile)
	if err != nil {
		return
	}
	var s Status
	if err := json.Unmarshal(raw, &s); err != nil {
		return
	}
	// A cache from another version (an upgrade) says nothing about this one.
	if s.Current != c.cfg.Current || s.Latest == "" {
		return
	}
	s.Enabled = true
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

func (c *Checker) saveState() {
	if c.cfg.StateFile == "" {
		return
	}
	raw, err := json.MarshalIndent(c.Status(), "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(c.cfg.StateFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".update-check-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	_ = os.Chmod(name, 0o644)
	_ = os.Rename(name, c.cfg.StateFile)
}

func initialStatus(cfg Config) Status {
	s := Status{Enabled: true, Current: cfg.Current}
	switch {
	case !cfg.Enabled:
		s.Enabled, s.Error = false, "update check disabled"
	case cfg.Current == "":
		s.Enabled, s.Error = false, "running version is unknown"
	case !IsVersion(cfg.Current):
		s.Enabled, s.Error = false, fmt.Sprintf("running version %q is not a release version", cfg.Current)
	}
	return s
}

// IsVersion reports whether s parses as a version (an optional leading "v",
// one to three numeric parts, an optional pre-release).
func IsVersion(s string) bool { return parse(s).ok }

// Compare orders two versions: -1 when a < b, 0 when they are equal (or either
// is not a version), 1 when a > b. It follows semver precedence: a pre-release
// sorts before its release, and numeric identifiers before alphanumeric ones.
func Compare(a, b string) int {
	va, vb := parse(a), parse(b)
	if !va.ok || !vb.ok {
		return 0
	}
	for i := range va.core {
		if va.core[i] != vb.core[i] {
			return sign(va.core[i] - vb.core[i])
		}
	}
	switch {
	case len(va.pre) == 0 && len(vb.pre) == 0:
		return 0
	case len(va.pre) == 0:
		return 1
	case len(vb.pre) == 0:
		return -1
	}
	for i := 0; i < len(va.pre) && i < len(vb.pre); i++ {
		x, y := va.pre[i], vb.pre[i]
		xn, xnum := strconv.Atoi(x)
		yn, ynum := strconv.Atoi(y)
		switch {
		case xnum == nil && ynum == nil:
			if xn != yn {
				return sign(xn - yn)
			}
		case xnum == nil:
			return -1 // numeric identifiers sort before alphanumeric ones
		case ynum == nil:
			return 1
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	if len(va.pre) != len(vb.pre) {
		return sign(len(va.pre) - len(vb.pre))
	}
	return 0
}

type version struct {
	core [3]int
	pre  []string
	ok   bool
}

func parse(s string) version {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return version{}
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre []string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = strings.Split(s[i+1:], ".")
		s = s[:i]
		for _, p := range pre {
			if p == "" {
				return version{}
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return version{}
	}
	var core [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}
		}
		core[i] = n
	}
	return version{core: core, pre: pre, ok: true}
}

// normalize strips a leading "v" so the UI never shows "vv0.2.0" or "v0.2.0"
// next to an un-prefixed current version.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	return s
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
