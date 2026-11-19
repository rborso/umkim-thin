package app

import (
	"os"
	"path/filepath"
	"testing"
)

// An installer/Ansible run reinstalls the identity release from the base values
// only, which drops everything the console contributed: every app's OIDC client
// and public-path bypass rule (identity-apps.yml), the theme and the default
// certificate. ReconcileIdentityOverlays is the recovery, and it used to run only
// when the settings or certs file existed - so a cluster with just app
// contributions kept the reverted config until the next console-driven install.
// Symptom on ha-dev: every Nextcloud public path challenged by the portal, and
// no OIDC app able to log in.
func TestReconcileIdentityOverlaysRunsForAppContributions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  bool
	}{
		{name: "nothing to reconcile", want: false},
		{name: "apps only", files: []string{"identity-apps.yml"}, want: true},
		{name: "settings only", files: []string{"identity-settings.yml"}, want: true},
		{name: "certs only", files: []string{"identity-certs.yml"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			apps := filepath.Join(dir, "identity-apps.yml")
			settings := filepath.Join(dir, "identity-settings.yml")
			certs := filepath.Join(dir, "identity-certs.yml")
			for _, f := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("authelia: {}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			in := New(Config{
				IdentityApps:     apps,
				IdentitySettings: settings,
				IdentityCerts:    certs,
				// A helm that always fails: the point is whether the reconcile
				// is attempted at all, which shows up as an error. Returning
				// nil means it decided there was nothing to do.
				Helm: "/nonexistent-helm",
			})
			err := in.ReconcileIdentityOverlays()
			if tc.want && err == nil {
				t.Fatalf("expected a reconcile attempt (error from the missing helm), got nil")
			}
			if !tc.want && err != nil {
				t.Fatalf("expected no reconcile, got %v", err)
			}
		})
	}
}
