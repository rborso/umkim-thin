package catalog

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the console's live catalog. The bundled catalog is the fallback; an
// external directory (the console PVC by default) can be replaced without
// rebuilding the image. A failed refresh keeps the last good snapshot.
type Store struct {
	mu       sync.RWMutex
	bundled  string
	external string
	entries  []Entry
	byName   map[string]Entry
	source   string
	version  string
	revision string
	loadedAt time.Time
	lastErr  error
}

// Info describes where the current snapshot came from.
type Info struct {
	Source   string    `json:"source"`
	Version  string    `json:"version,omitempty"`
	Revision string    `json:"revision,omitempty"`
	LoadedAt time.Time `json:"loadedAt,omitempty"`
	External string    `json:"external,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// NewStore loads the external catalog when it has YAML entries, otherwise the
// bundled catalog. An invalid external catalog falls back to the bundled one;
// an invalid bundled catalog is a startup error.
func NewStore(bundled, external string) (*Store, error) {
	s := &Store{bundled: bundled, external: external}
	if err := s.Reload(); err != nil {
		s.mu.RLock()
		haveSnapshot := len(s.entries) > 0
		s.mu.RUnlock()
		if !haveSnapshot {
			return nil, err
		}
		// A bad external bundle must not prevent the console from starting: the
		// bundled fallback is live and Info().Error tells the operator.
		return s, nil
	}
	return s, nil
}

// Reload re-reads the external catalog (or the bundled fallback when there is
// no external YAML) and swaps the snapshot only after validation.
func (s *Store) Reload() error {
	path, source := s.bundled, "bundled"
	if hasYAML(s.external) {
		path, source = s.external, "external"
	}
	entries, err := Load(path)
	if err != nil {
		if source == "external" {
			// A bad refresh never replaces a working external snapshot. Only a
			// first load (no in-memory entries) falls back to the bundled copy.
			s.mu.RLock()
			haveSnapshot := len(s.entries) > 0
			s.mu.RUnlock()
			if haveSnapshot {
				s.mu.Lock()
				s.lastErr = err
				s.mu.Unlock()
				return fmt.Errorf("external catalog %s: %w (keeping the previous snapshot)", s.external, err)
			}
			fallback, ferr := Load(s.bundled)
			if ferr != nil {
				return fmt.Errorf("external catalog: %w; bundled fallback: %v", err, ferr)
			}
			s.set(fallback, "bundled", s.bundled, err)
			return fmt.Errorf("external catalog %s: %w (using the bundled fallback)", s.external, err)
		}
		return err
	}
	s.set(entries, source, path, nil)
	return nil
}

func (s *Store) set(entries []Entry, source, path string, lastErr error) {
	byName := make(map[string]Entry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	version := readVersion(path)
	if source == "external" && version == "" {
		version = readVersion(s.bundled)
	}
	rev := revisionOf(path)
	if rev == "" {
		rev = revisionOf(s.bundled)
	}
	s.mu.Lock()
	s.entries = entries
	s.byName = byName
	s.source = source
	s.version = version
	s.revision = rev
	s.loadedAt = time.Now()
	s.lastErr = lastErr
	s.mu.Unlock()
}

// Entries returns a copy of the current catalog, sorted by name.
func (s *Store) Entries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Entry(nil), s.entries...)
}

// Entry returns one current entry by name.
func (s *Store) Entry(name string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byName[name]
	return e, ok
}

// Info returns the current snapshot metadata.
func (s *Store) Info() Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := Info{
		Source: s.source, Version: s.version, Revision: s.revision,
		LoadedAt: s.loadedAt, External: s.external,
	}
	if s.lastErr != nil {
		info.Error = s.lastErr.Error()
	}
	return info
}

// ExternalDir is where an uploaded catalog is unpacked.
func (s *Store) ExternalDir() string { return s.external }

func hasYAML(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	return len(paths) > 0
}

func readVersion(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func revisionOf(dir string) string {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s\x00", filepath.Base(p))
		h.Write(raw)
	}
	if len(paths) == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Extract unpacks a catalog tarball into dir. Only top-level *.yaml files and
// VERSION are accepted; links, nested paths and other files are rejected so a
// catalog upload cannot write outside its directory.
func Extract(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the catalog must be a .tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("catalog entry %q is not a regular file", hdr.Name)
		}
		rawName := strings.TrimPrefix(hdr.Name, "./")
		name := filepath.Base(rawName)
		if name != rawName || name == "." || name == "" {
			return fmt.Errorf("catalog entry %q must be at the archive root", hdr.Name)
		}
		if name != "VERSION" && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		if hdr.Size > 1<<20 {
			return fmt.Errorf("catalog entry %q is too large", name)
		}
		dest := filepath.Join(dir, name)
		raw, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return err
		}
		if int64(len(raw)) != hdr.Size {
			return fmt.Errorf("catalog entry %q is truncated", name)
		}
		if err := os.WriteFile(dest, raw, 0o600); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("the catalog archive contains no *.yaml entries")
	}
	return nil
}

// Package writes a deterministic catalog tarball (the upload format). It
// includes the top-level *.yaml entries and VERSION.
func Package(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no catalog entries in %s", dir)
	}
	if v := filepath.Join(dir, "VERSION"); fileExists(v) {
		paths = append(paths, v)
	}
	sort.Strings(paths)
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		name := filepath.Base(p)
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(raw)), ModTime: time.Time{}}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(raw); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
