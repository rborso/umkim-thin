package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A catalog upload must never leave the console without a catalog: if the swap
// fails after the old directory was moved aside, the old one goes back.
func TestReplaceDirectoryKeepsTheOldCatalogOnFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "catalog")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.yaml"), []byte("name: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A staged path that does not exist makes the second rename fail.
	if err := replaceDirectory(filepath.Join(root, "does-not-exist"), target); err == nil {
		t.Fatal("replaceDirectory accepted a missing staged directory")
	}
	if _, err := os.Stat(filepath.Join(target, "old.yaml")); err != nil {
		t.Fatalf("the previous catalog was not restored: %v", err)
	}
}

func TestReplaceDirectoryInstallsTheNewCatalog(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "catalog")
	staged := filepath.Join(root, ".catalog-upload-x")
	for _, d := range []string{target, staged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(target, "old.yaml"), []byte("name: old\n"), 0o644)
	os.WriteFile(filepath.Join(staged, "new.yaml"), []byte("name: new\n"), 0o644)

	if err := replaceDirectory(staged, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "new.yaml")); err != nil {
		t.Fatalf("the new catalog is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "old.yaml")); !os.IsNotExist(err) {
		t.Fatal("the old catalog is still present")
	}
	// No staging or backup directories left behind.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "catalog" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("leftover directories: %v", names)
	}
}

// A fresh install has no catalog directory yet; the swap must still work.
func TestReplaceDirectoryWithNoExistingCatalog(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "catalog")
	staged := filepath.Join(root, ".catalog-upload-y")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(staged, "a.yaml"), []byte("name: a\n"), 0o644)
	if err := replaceDirectory(staged, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "a.yaml")); err != nil {
		t.Fatalf("the catalog was not installed: %v", err)
	}
}
