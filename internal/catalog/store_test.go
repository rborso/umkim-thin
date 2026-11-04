package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
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

func TestStoreExternalWinsAndKeepsLastGoodSnapshot(t *testing.T) {
	bundled := t.TempDir()
	external := t.TempDir()
	writeCatalog(t, bundled, "demo.yaml", testEntry)
	writeCatalog(t, external, "demo.yaml", testEntry)
	writeCatalog(t, external, "VERSION", "7.1.0\n")

	s, err := NewStore(bundled, external)
	if err != nil {
		t.Fatal(err)
	}
	info := s.Info()
	if info.Source != "external" || info.Version != "7.1.0" || info.Revision == "" {
		t.Fatalf("info = %+v, want external version 7.1.0", info)
	}
	if _, ok := s.Entry("demo"); !ok {
		t.Fatal("external entry is missing")
	}

	// A malformed external refresh must not replace the working snapshot.
	writeCatalog(t, external, "broken.yaml", "not: [a catalog\n")
	if err := s.Reload(); err == nil {
		t.Fatal("malformed external catalog was accepted")
	}
	if info := s.Info(); info.Source != "external" || info.Version != "7.1.0" || info.Error == "" {
		t.Fatalf("after failed refresh = %+v, want the previous external snapshot", info)
	}

	// Removing the external entries falls back to the bundled catalog.
	if err := os.Remove(filepath.Join(external, "demo.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(external, "broken.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.Info().Source; got != "bundled" {
		t.Fatalf("source = %q, want bundled fallback", got)
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
