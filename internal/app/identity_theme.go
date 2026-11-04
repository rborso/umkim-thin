package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// AutheliaThemes are the theme values understood by Authelia 4.38.
var AutheliaThemes = []string{"dark", "light", "grey", "auto"}

// DefaultAutheliaTheme keeps the portal dark when no console setting exists.
const DefaultAutheliaTheme = "dark"

// NormalizeAutheliaTheme lowercases and trims a theme name.
func NormalizeAutheliaTheme(theme string) string {
	return strings.ToLower(strings.TrimSpace(theme))
}

// AutheliaThemeValid reports whether Authelia accepts the theme.
func AutheliaThemeValid(theme string) bool {
	theme = NormalizeAutheliaTheme(theme)
	for _, t := range AutheliaThemes {
		if theme == t {
			return true
		}
	}
	return false
}

type autheliaSettings struct {
	Authelia struct {
		Theme string `yaml:"theme"`
	} `yaml:"authelia"`
}

// AutheliaTheme reads the console-owned theme override. Missing or empty means
// the chart default (dark).
func (in *Installer) AutheliaTheme() (string, error) {
	if in.cfg.IdentitySettings == "" {
		return DefaultAutheliaTheme, nil
	}
	raw, err := os.ReadFile(in.cfg.IdentitySettings)
	if os.IsNotExist(err) {
		return DefaultAutheliaTheme, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the Authelia theme: %w", err)
	}
	var s autheliaSettings
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("reading the Authelia theme: %w", err)
	}
	theme := NormalizeAutheliaTheme(s.Authelia.Theme)
	if theme == "" {
		return DefaultAutheliaTheme, nil
	}
	if !AutheliaThemeValid(theme) {
		return "", fmt.Errorf("unknown Authelia theme %q", theme)
	}
	return theme, nil
}

// SetAutheliaTheme persists the theme in the console-owned values file (not
// identity-values.yml: the console seed overwrites that on every restart) and
// rolls the identity release. All independent values layers are included so a
// theme change cannot drop OIDC clients or the default certificate.
func (in *Installer) SetAutheliaTheme(theme string) error {
	theme = NormalizeAutheliaTheme(theme)
	if !AutheliaThemeValid(theme) {
		return fmt.Errorf("unknown Authelia theme %q (use dark, light, grey or auto)", theme)
	}
	if in.cfg.IdentitySettings == "" {
		return fmt.Errorf("identity settings path is not configured")
	}
	var s autheliaSettings
	s.Authelia.Theme = theme
	raw, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(in.cfg.IdentitySettings), 0o755); err != nil {
		return err
	}
	prev, prevErr := os.ReadFile(in.cfg.IdentitySettings)
	hadPrev := prevErr == nil
	tmp := in.cfg.IdentitySettings + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, in.cfg.IdentitySettings); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := in.applyIdentityOverlays(); err != nil {
		if hadPrev {
			_ = os.WriteFile(in.cfg.IdentitySettings, prev, 0o600)
		} else {
			_ = os.Remove(in.cfg.IdentitySettings)
		}
		return err
	}
	return nil
}

// ReconcileIdentityOverlays reapplies console-owned identity values after an
// installer/Ansible run. Those runs use identity-values.yml (installer-owned)
// and cannot see the console PVC, so without this a theme or default
// certificate would silently revert on the next platform update.
func (in *Installer) ReconcileIdentityOverlays() error {
	hasSettings := fileExists(in.cfg.IdentitySettings)
	hasCerts := fileExists(in.cfg.IdentityCerts)
	if !hasSettings && !hasCerts {
		return nil
	}
	if hasSettings {
		if _, err := in.AutheliaTheme(); err != nil {
			return err
		}
	}
	return in.applyIdentityOverlays()
}

func (in *Installer) applyIdentityOverlays() error {
	args := in.identityHelmArgs()
	args = append(args, "--wait", "--timeout", "15m")
	if out, err := in.run(in.cfg.Helm, args...); err != nil {
		return fmt.Errorf("applying identity overlays: %w\n%s", err, out)
	}
	return nil
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// identityHelmArgs builds the complete identity upgrade command. The base
// values are installer-owned; apps/certs/settings are console-owned layers.
func (in *Installer) identityHelmArgs() []string {
	args := []string{"upgrade", "--install", "identity", in.cfg.IdentityChart,
		"--namespace", in.cfg.IdentityNS,
		"--kubeconfig", in.cfg.Kubeconfig,
		"--values", in.cfg.IdentityValues}
	for _, p := range []string{in.cfg.IdentityApps, in.cfg.IdentityCerts, in.cfg.IdentitySettings} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			args = append(args, "--values", p)
		}
	}
	return args
}
