package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testEntry = `name: demo
displayName: Demo
description: test entry
chart:
  ref: oci://example.invalid/charts/demo
  version: 0.1.0
namespace: demo
auth:
  kind: none
license:
  chart: MIT
  application: MIT
links:
  source: https://example.invalid/demo
verify:
  playwright: tests/demo.spec.ts
  checks: [loads]
`

func writeCatalog(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The external catalog is ADDITIVE: a custom app joins the bundled set instead of
// replacing it. This is the whole point of the feature — before it, uploading a
// bundle containing one app left the console with exactly that one app.
func TestStoreMergesCustomEntriesIntoTheBundledCatalog(t *testing.T) {
	bundled := t.TempDir()
	external := t.TempDir()
	writeCatalog(t, bundled, "official.yaml", strings.Replace(testEntry, "name: demo", "name: official", 1))
	writeCatalog(t, external, "custom.yaml", strings.Replace(testEntry, "name: demo", "name: custom", 1))
	writeCatalog(t, external, "VERSION", "7.1.0\n")

	s, err := NewStore(bundled, external)
	if err != nil {
		t.Fatal(err)
	}
	// Both are present. This is what "add a custom app" means.
	if _, ok := s.Entry("official"); !ok {
		t.Error("the bundled entry disappeared when a custom entry was added")
	}
	if _, ok := s.Entry("custom"); !ok {
		t.Fatal("the custom entry is missing")
	}
	if n := len(s.Entries()); n != 2 {
		t.Fatalf("entries = %d, want 2 (bundled + custom)", n)
	}
	if info := s.Info(); info.Source != "mixed" || info.Version != "7.1.0" {
		t.Fatalf("info = %+v, want mixed version 7.1.0", info)
	}

	// Provenance is set by the loader, so the console can mark a custom app.
	if e, _ := s.Entry("custom"); !e.IsExternal() || e.Origin != OriginExternal {
		t.Errorf("custom entry origin = %q, want %q", e.Origin, OriginExternal)
	}
	if e, _ := s.Entry("official"); e.IsExternal() || e.Origin != OriginBundled {
		t.Errorf("bundled entry origin = %q, want %q", e.Origin, OriginBundled)
	}

	// A malformed custom entry must not take the platform's catalog down with it:
	// the bundled set is still serviceable, and the error is reported.
	writeCatalog(t, external, "broken.yaml", "not: [a catalog\n")
	if err := s.Reload(); err == nil {
		t.Fatal("a malformed custom catalog was accepted")
	}
	if info := s.Info(); info.Error == "" {
		t.Errorf("info = %+v, want the error surfaced", info)
	}
	if _, ok := s.Entry("official"); !ok {
		t.Error("the bundled catalog was lost because a custom entry was malformed")
	}

	// Removing the custom entries returns to the bundled catalog alone.
	for _, f := range []string{"custom.yaml", "broken.yaml"} {
		if err := os.Remove(filepath.Join(external, f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.Info().Source; got != "bundled" {
		t.Fatalf("source = %q, want bundled", got)
	}
	if n := len(s.Entries()); n != 1 {
		t.Fatalf("entries = %d after removing the custom entry, want 1", n)
	}
}

// A name collision is refused, not silently resolved. Two apps called "gitea" is
// either a mistake or an attempt to substitute a chart the platform ships, and
// picking one quietly is how an operator runs something they did not choose.
func TestStoreRefusesACustomNameCollision(t *testing.T) {
	bundled := t.TempDir()
	external := t.TempDir()
	writeCatalog(t, bundled, "demo.yaml", testEntry)
	writeCatalog(t, external, "demo.yaml", testEntry)

	// Reload reports it; NewStore must still start, because a bad custom entry
	// cannot be allowed to stop the console from serving the platform catalog.
	s := &Store{bundled: bundled, external: external}
	err := s.Reload()
	if err == nil {
		t.Fatal("a colliding custom entry was accepted")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Fatalf("error %q does not explain the collision", err)
	}

	started, err := NewStore(bundled, external)
	if err != nil {
		t.Fatalf("the console must start with a bad custom entry: %v", err)
	}
	if started.Info().Error == "" {
		t.Error("the collision was not surfaced in Info().Error")
	}
	if _, ok := started.Entry("demo"); !ok {
		t.Error("the bundled entry is missing after a refused collision")
	}
}

// ...and an operator who means it can say so, which is the deliberate path to
// replacing a bundled entry.
func TestStoreHonoursAnExplicitOverride(t *testing.T) {
	bundled := t.TempDir()
	external := t.TempDir()
	writeCatalog(t, bundled, "demo.yaml", testEntry)
	overridden := strings.Replace(testEntry, "version: 0.1.0", "version: 9.9.9", 1) + "overrides: true\n"
	writeCatalog(t, external, "demo.yaml", overridden)

	s, err := NewStore(bundled, external)
	if err != nil {
		t.Fatalf("an explicit override was refused: %v", err)
	}
	e, ok := s.Entry("demo")
	if !ok {
		t.Fatal("entry missing")
	}
	if e.Chart.Version != "9.9.9" {
		t.Errorf("chart version = %q, want the override's 9.9.9", e.Chart.Version)
	}
	if e.Origin != OriginExternal {
		t.Errorf("origin = %q, want external — an override is the operator's entry now", e.Origin)
	}
	if n := len(s.Entries()); n != 1 {
		t.Errorf("entries = %d, want 1: an override replaces rather than duplicates", n)
	}
}

func TestStoreStartsWithBundledWhenExternalIsInvalid(t *testing.T) {
	bundled := t.TempDir()
	external := t.TempDir()
	writeCatalog(t, bundled, "demo.yaml", testEntry)
	writeCatalog(t, external, "broken.yaml", "not: [a catalog\n")
	s, err := NewStore(bundled, external)
	if err != nil {
		t.Fatalf("NewStore refused the bundled fallback: %v", err)
	}
	info := s.Info()
	if info.Source != "bundled" || info.Error == "" {
		t.Fatalf("info = %+v, want bundled with an error", info)
	}
	if _, ok := s.Entry("demo"); !ok {
		t.Fatal("bundled entry is missing")
	}
}

func TestExtractAndPackageCatalog(t *testing.T) {
	src := t.TempDir()
	writeCatalog(t, src, "demo.yaml", testEntry)
	writeCatalog(t, src, "VERSION", "1.2.3\n")
	var buf bytes.Buffer
	if err := Package(src, &buf); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Extract(bytes.NewReader(buf.Bytes()), dst); err != nil {
		t.Fatal(err)
	}
	entries, err := Load(dst)
	if err != nil || len(entries) != 1 || entries[0].Name != "demo" {
		t.Fatalf("Load = %v, %v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(dst, "VERSION"))
	if err != nil || string(raw) != "1.2.3\n" {
		t.Fatalf("VERSION = %q, %v", raw, err)
	}
}

func TestExtractRejectsNestedEntries(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(testEntry)
	if err := tw.WriteHeader(&tar.Header{Name: "nested/demo.yaml", Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Extract(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil {
		t.Fatal("a nested catalog entry was accepted")
	}
}
