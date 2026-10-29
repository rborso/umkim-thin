package app

import (
	"strings"
	"testing"
	"time"

	"github.com/rborso/umkim-thin/internal/catalog"
)

func upgradeEntry() catalog.Entry {
	return catalog.Entry{
		Name:      "gitea",
		Namespace: "gitea",
		Chart: catalog.ChartRef{
			Ref:     "oci://example.invalid/charts/gitea",
			Version: "0.1.20",
			Versions: []catalog.ChartVersion{
				{Version: "0.1.20", AppVersion: "1.24.3", Channel: "stable"},
				{Version: "0.1.19", AppVersion: "1.24.2", Channel: "stable"},
			},
		},
		Auth:    catalog.Auth{Kind: "forward_auth"},
		License: &catalog.License{Chart: "MIT", Application: "MIT"},
		Links:   &catalog.Links{Homepage: "https://example.invalid"},
		Verify:  catalog.Verify{Playwright: "tests/playwright/journeys/gitea.spec.ts", Checks: []string{"route"}},
	}
}

// The upgrade gates must fail before any cluster call: a missing target version
// and an unconfirmed backup are both cheap, deterministic refusals.
func TestUpgradeRequiresExplicitTarget(t *testing.T) {
	in := testInstaller()
	_, err := in.Install(upgradeEntry(), Options{Upgrade: true, ConfirmBackup: true})
	if err == nil || !strings.Contains(err.Error(), "needs an explicit --to chart version") {
		t.Fatalf("error = %v, want a missing-target refusal", err)
	}
}

func TestUpgradeRequiresBackupConfirmation(t *testing.T) {
	in := testInstaller()
	_, err := in.Install(upgradeEntry(), Options{Upgrade: true, ChartVersion: "0.1.19"})
	if err == nil || !strings.Contains(err.Error(), "confirm the pre-upgrade backup") {
		t.Fatalf("error = %v, want a backup-confirmation refusal", err)
	}
}

// A version the catalog does not list is refused even when the operator confirms
// the backup: the console must not install an untested chart.
func TestUpgradeRejectsUncataloguedVersion(t *testing.T) {
	in := testInstaller()
	_, err := in.Install(upgradeEntry(), Options{
		Upgrade: true, ChartVersion: "9.9.9", ConfirmBackup: true,
	})
	if err == nil || !strings.Contains(err.Error(), "is not available") {
		t.Fatalf("error = %v, want an unavailable-version refusal", err)
	}
}

func TestBackupIntervalParsing(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"6h30m", 6*time.Hour + 30*time.Minute},
		{"0", 0},
		{"off", 0},
		{"", 0},
		{"disabled", 0},
		{"nonsense", 24 * time.Hour}, // a typo must not silently stop backups
	}
	for _, c := range cases {
		if got := duration(c.in); got != c.want {
			t.Errorf("duration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBackupSettingsDefaults(t *testing.T) {
	t.Setenv("UMKIM_THIN_STATE_DIR", "/data")
	t.Setenv("UMKIM_THIN_BACKUP_DIR", "")
	t.Setenv("UMKIM_THIN_BACKUP_KEEP", "")
	t.Setenv("UMKIM_THIN_BACKUP_INTERVAL", "")
	t.Setenv("UMKIM_THIN_BACKUP_SNAPSHOTS", "")
	cfg := ConfigFromEnv()
	if cfg.BackupDir != "/data/backups" {
		t.Errorf("BackupDir = %q, want /data/backups", cfg.BackupDir)
	}
	if cfg.BackupKeep != 7 {
		t.Errorf("BackupKeep = %d, want 7", cfg.BackupKeep)
	}
	if cfg.BackupInterval != 24*time.Hour {
		t.Errorf("BackupInterval = %v, want 24h", cfg.BackupInterval)
	}
	if !cfg.BackupSnapshots {
		t.Error("BackupSnapshots should default to true")
	}

	t.Setenv("UMKIM_THIN_BACKUP_INTERVAL", "off")
	t.Setenv("UMKIM_THIN_BACKUP_SNAPSHOTS", "false")
	t.Setenv("UMKIM_THIN_BACKUP_KEEP", "3")
	cfg = ConfigFromEnv()
	if cfg.BackupInterval != 0 || cfg.BackupSnapshots || cfg.BackupKeep != 3 {
		t.Errorf("explicit settings were not honoured: %+v", cfg)
	}
}
