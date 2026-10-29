package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCluster answers the two kubectl reads and records everything else.
type fakeCluster struct {
	class       string
	pvcs        []string // "ns/name"
	applied     []string
	appliedYAML []string
	fail        bool
}

func (f *fakeCluster) run(name, stdin string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "get volumesnapshotclass"):
		if f.class == "" {
			return "", os.ErrNotExist
		}
		return `{"items":[{"metadata":{"name":"` + f.class + `"}}]}`, nil
	case strings.Contains(joined, "get pvc -A"):
		var items []string
		for _, p := range f.pvcs {
			ns, n, _ := strings.Cut(p, "/")
			items = append(items, `{"metadata":{"namespace":"`+ns+`","name":"`+n+`"}}`)
		}
		return `{"items":[` + strings.Join(items, ",") + `]}`, nil
	case strings.Contains(joined, "apply -f -"):
		if f.fail {
			return "", os.ErrPermission
		}
		f.applied = append(f.applied, stdin)
		return "applied", nil
	case strings.Contains(joined, "delete volumesnapshot"):
		f.applied = append(f.applied, "delete "+joined)
		return "", nil
	case strings.Contains(joined, "list -A -o json"):
		return `[{"name":"gitea","namespace":"gitea","chart":"gitea-0.1.20","app_version":"1.24.3","revision":"3","status":"deployed"}]`, nil
	}
	return "", nil
}

func newTestManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "data")
	seed := filepath.Join(root, "etc")
	for _, d := range []string{
		state, filepath.Join(state, "apps"), filepath.Join(state, "backups"),
		filepath.Join(state, "chart-mirror"), seed,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(state, "invites.json"), `[{"email":"a@example.com"}]`)
	write(filepath.Join(state, "apps-state.json"), `{"installed":{"gitea":{"namespace":"gitea"}}}`)
	write(filepath.Join(state, "wg-clients.json"), `[{"name":"phone"}]`)
	write(filepath.Join(state, "identity-apps.yml"), "clients: []\n")
	write(filepath.Join(state, "apps", "gitea-values.yaml"), "consoleHost: console.example.com\n")
	write(filepath.Join(state, "chart-mirror", "gitea-0.1.20.tgz"), "not-a-real-chart")
	write(filepath.Join(seed, "secrets.yml"), "secret_lldap_admin_password: ci-only\n")
	write(filepath.Join(seed, "identity-values.yml"), "zone: example.com\n")
	write(filepath.Join(seed, "config.env"), "UMKIM_THIN_ZONE=example.com\n")

	fake := &fakeCluster{class: "longhorn-snapclass", pvcs: []string{"gitea/gitea-data", "umkim-thin/umkim-thin-state"}}
	m := New(Config{
		Dir:        filepath.Join(state, "backups"),
		StateDir:   state,
		SeedDir:    seed,
		Kubeconfig: "/tmp/kubeconfig",
		Version:    "0.3.0",
		Zone:       "example.com",
		Keep:       2,
		Snapshots:  true,
		Run:        fake.run,
		Now:        func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) },
	})
	return m, state, seed
}

func TestCreateListVerify(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()
	a, err := m.Create(ctx, "nightly")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Name != "umkim-thin-20261102T120000Z.tar.gz" {
		t.Fatalf("name = %q", a.Name)
	}
	if a.Apps != 1 || a.Snapshots != 1 {
		t.Fatalf("archive = %+v, want one app and one snapshot", a)
	}
	// The console's own state and the seed are in; the chart mirror is not.
	if a.Files < 6 {
		t.Fatalf("files = %d, want at least the six state/seed files", a.Files)
	}

	list, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != a.Name || list[0].Files != a.Files {
		t.Fatalf("list = %+v", list)
	}

	v, err := m.Verify(a.Name)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !v.Verified {
		t.Fatalf("verify = %+v, want verified", v)
	}
}

func TestArchiveExcludesChartMirrorAndBackups(t *testing.T) {
	m, _, _ := newTestManager(t)
	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	mf, _, err := scanArchive(filepath.Join(m.Dir(), a.Name))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range mf.Files {
		if strings.Contains(f.Path, "chart-mirror") || strings.HasPrefix(f.Path, "state/backups") {
			t.Fatalf("archive contains %s", f.Path)
		}
	}
	if mf.Version != "0.3.0" || mf.Zone != "example.com" || mf.Schema != Schema {
		t.Fatalf("manifest = %+v", mf)
	}
	if len(mf.Apps) != 1 || mf.Apps[0].ChartVersion != "0.1.20" || mf.Apps[0].Name != "gitea" {
		t.Fatalf("apps = %+v", mf.Apps)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	m, _, _ := newTestManager(t)
	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(m.Dir(), a.Name)
	// Flip one byte in the gzip stream: the archive no longer matches.
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-8] ^= 0xff
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(a.Name); err == nil {
		t.Fatal("Verify accepted a tampered archive")
	}
}

func TestRestoreWritesStateAndSkipsConfig(t *testing.T) {
	m, state, seed := newTestManager(t)
	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the loss the restore has to fix.
	if err := os.WriteFile(filepath.Join(state, "invites.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(state, "apps", "gitea-values.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "config.env"), []byte("UMKIM_THIN_ZONE=changed.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := m.Restore(a.Name)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rep.SafetyArchive == "" {
		t.Fatal("restore did not write a safety archive")
	}
	invites, err := os.ReadFile(filepath.Join(state, "invites.json"))
	if err != nil || string(invites) != `[{"email":"a@example.com"}]` {
		t.Fatalf("invites = %q, err %v", invites, err)
	}
	if _, err := os.Stat(filepath.Join(state, "apps", "gitea-values.yaml")); err != nil {
		t.Fatalf("app values were not restored: %v", err)
	}
	// config.env belongs to the installer: archived, never restored.
	cfg, err := os.ReadFile(filepath.Join(seed, "config.env"))
	if err != nil || !strings.Contains(string(cfg), "changed.example") {
		t.Fatalf("config.env was restored (%q, %v)", cfg, err)
	}
	if len(rep.Skipped) == 0 {
		t.Fatal("restore did not report skipped files")
	}

	// The safety archive must contain the pre-restore state.
	list, err := m.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list after restore = %+v, %v", list, err)
	}
}

func TestRestoreRefusesCorruptArchive(t *testing.T) {
	m, _, _ := newTestManager(t)
	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(m.Dir(), a.Name))
	raw[len(raw)-8] ^= 0xff
	if err := os.WriteFile(filepath.Join(m.Dir(), a.Name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restore(a.Name); err == nil {
		t.Fatal("Restore accepted a corrupt archive")
	}
}

func TestRestoreRejectsPathTraversal(t *testing.T) {
	m, state, _ := newTestManager(t)
	name := "umkim-thin-20260101T000000Z.tar.gz"
	if err := os.MkdirAll(m.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(m.Dir(), name))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	manifest, _ := json.Marshal(Manifest{Schema: Schema, Files: []FileRecord{{Path: "state/../../etc/passwd"}}})
	write := func(n, body string) {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	write("manifest.json", string(manifest))
	write("state/../../etc/passwd", "root:x:0:0")
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()

	if _, err := m.Restore(name); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("Restore error = %v, want an unsafe-path refusal", err)
	}
	if _, err := os.Stat(filepath.Join(state, "..", "..", "etc", "passwd")); err == nil {
		t.Fatal("restore wrote outside the state directory")
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	m, _, _ := newTestManager(t)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		m.cfg.Now = func() time.Time { return base.Add(time.Duration(i) * time.Hour) }
		if _, err := m.Create(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	// Keep=2 in the fixture: every Create prunes, so three remain at most.
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > 2 {
		t.Fatalf("list = %d archives, want <= 2", len(list))
	}
	removed, err := m.Prune(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != len(list)-1 {
		t.Fatalf("removed %v, want %d", removed, len(list)-1)
	}
}

func TestNameValidation(t *testing.T) {
	m, _, _ := newTestManager(t)
	for _, bad := range []string{
		"../umkim-thin-20260101T000000Z.tar.gz",
		"umkim-thin-20260101T000000Z.tar.gz/../../x",
		"umkim-thin-evil.tar.gz",
		"umkim-thin-20260101T000000Z.tgz",
	} {
		if _, err := m.Path(bad); err == nil {
			t.Errorf("Path(%q) accepted an invalid name", bad)
		}
		if _, err := m.Verify(bad); err == nil {
			t.Errorf("Verify(%q) accepted an invalid name", bad)
		}
	}
	if _, err := m.Path("umkim-thin-20260101T000000Z.tar.gz"); err == nil {
		t.Error("Path accepted a name that does not exist")
	}
}

func TestSnapshotsRecordApplicationPVCsOnly(t *testing.T) {
	m, _, _ := newTestManager(t)
	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	mf, _, err := scanArchive(filepath.Join(m.Dir(), a.Name))
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Snapshots) != 1 {
		t.Fatalf("snapshots = %+v, want only the gitea PVC", mf.Snapshots)
	}
	s := mf.Snapshots[0]
	if s.Namespace != "gitea" || s.Snapshot == "" || len(s.Snapshot) > 63 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestSnapshotsWarnWithoutClass(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.cfg.Run = (&fakeCluster{class: ""}).run
	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Warnings) == 0 || !strings.Contains(strings.Join(a.Warnings, " "), "snapshotted") {
		t.Fatalf("warnings = %v, want a snapshot warning", a.Warnings)
	}
	if a.Snapshots != 0 {
		t.Fatalf("snapshots = %d, want 0", a.Snapshots)
	}
}

func TestSnapshotNameFitsTheLimit(t *testing.T) {
	long := strings.Repeat("a", 120)
	n := snapshotName("20261102T120000Z", long)
	if len(n) > 63 {
		t.Fatalf("snapshot name %q is %d characters", n, len(n))
	}
	if n != snapshotName("20261102T120000Z", long) {
		t.Fatal("snapshot name is not deterministic")
	}
	if snapshotName("20261102T120000Z", "data") == snapshotName("20261102T120000Z", "other") {
		t.Fatal("different PVCs got the same snapshot name")
	}
}

// With a retention count of one, the safety archive written by a restore would
// prune the archive being restored; the restore must still read it.
func TestRestoreWithKeepOne(t *testing.T) {
	m, state, _ := newTestManager(t)
	m.cfg.Keep = 1
	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "invites.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := m.Restore(a.Name)
	if err != nil {
		t.Fatalf("Restore with keep=1: %v", err)
	}
	invites, err := os.ReadFile(filepath.Join(state, "invites.json"))
	if err != nil || string(invites) != `[{"email":"a@example.com"}]` {
		t.Fatalf("invites = %q, err %v", invites, err)
	}
	if rep.SafetyArchive == a.Name {
		t.Fatal("the safety archive reused the restored name")
	}
}
