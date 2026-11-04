package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutheliaThemeDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity-settings.yml")
	in := New(Config{IdentitySettings: path})
	theme, err := in.AutheliaTheme()
	if err != nil || theme != DefaultAutheliaTheme {
		t.Fatalf("missing theme = %q, %v; want dark", theme, err)
	}
	for _, s := range []string{"DARK", " light ", "grey", "auto"} {
		if !AutheliaThemeValid(s) {
			t.Errorf("theme %q should be valid", s)
		}
	}
	for _, s := range []string{"", "blue", "system"} {
		if AutheliaThemeValid(s) {
			t.Errorf("theme %q should be invalid", s)
		}
	}

	if err := os.WriteFile(path, []byte("authelia:\n  theme: light\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if theme, err := in.AutheliaTheme(); err != nil || theme != "light" {
		t.Fatalf("light theme = %q, %v", theme, err)
	}
	if err := os.WriteFile(path, []byte("authelia:\n  theme: blue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := in.AutheliaTheme(); err == nil || !strings.Contains(err.Error(), "unknown Authelia theme") {
		t.Fatalf("invalid theme error = %v", err)
	}
}

func TestSetAutheliaThemeWritesConsoleSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity-settings.yml")
	in := New(Config{
		IdentitySettings: path,
		IdentityValues:   "/nonexistent/identity-values.yml",
		IdentityChart:    "/nonexistent/identity-chart",
		Helm:             "/bin/true",
	})
	if err := in.SetAutheliaTheme("light"); err != nil {
		t.Fatal(err)
	}
	theme, err := in.AutheliaTheme()
	if err != nil || theme != "light" {
		t.Fatalf("saved theme = %q, %v", theme, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode = %v, %v; want 0600", info, err)
	}
	if err := in.SetAutheliaTheme("blue"); err == nil {
		t.Fatal("an invalid theme was accepted")
	}
	if theme, _ := in.AutheliaTheme(); theme != "light" {
		t.Fatalf("an invalid Set changed the saved theme to %q", theme)
	}
}
