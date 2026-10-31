package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeCluster answers the two kubectl reads and records everything else.
type fakeCluster struct {
	class string
	pvcs  []string // "ns/name"
	// apps are "name/namespace/chart" releases; empty means one gitea release.
	apps []string
	// nfs names one PVC that is NFS-backed (not CSI) and must be skipped.
	nfs         string
	applied     []string
	appliedYAML []string
	pods        []string
	// streamData is what a volume copy returns; empty means "volume-data".
	streamData []byte
	// podsJSON is what `get pods -o json` returns; empty means no pods.
	podsJSON string
	// restored is the bytes a volume restore sent to tar.
	restored []byte
	// appliedObjects is the YAML sent to a server-side apply.
	appliedObjects string
	// scaleCalls records the scale commands in order.
	scaleCalls []string
	fail       bool
}

func (f *fakeCluster) run(name, stdin string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "get volumesnapshotclass -o json"):
		if f.class == "" {
			return "", os.ErrNotExist
		}
		return `{"items":[{"metadata":{"name":"` + f.class + `"}}]}`, nil
	case strings.Contains(joined, "get pvc -A"):
		var items []string
		for _, p := range f.pvcs {
			ns, n, _ := strings.Cut(p, "/")
			items = append(items, `{"metadata":{"namespace":"`+ns+`","name":"`+n+`"},"spec":{"volumeName":"pv-`+n+`","storageClassName":"longhorn-rwo","resources":{"requests":{"storage":"1Gi"}}}}`)
		}
		return `{"items":[` + strings.Join(items, ",") + `]}`, nil
	case strings.Contains(joined, "get pv -o json"):
		var items []string
		for _, p := range f.pvcs {
			_, n, _ := strings.Cut(p, "/")
			if f.nfs != "" && n == f.nfs {
				items = append(items, `{"metadata":{"name":"pv-`+n+`"},"spec":{"nfs":{"server":"10.0.0.1"}}}`)
				continue
			}
			items = append(items, `{"metadata":{"name":"pv-`+n+`"},"spec":{"csi":{"driver":"driver.longhorn.io"}}}`)
		}
		return `{"items":[` + strings.Join(items, ",") + `]}`, nil
	case strings.Contains(joined, "get volumesnapshot") && strings.Contains(joined, "jsonpath"):
		return "true", nil
	case strings.Contains(joined, "get pods -o json"):
		if f.podsJSON != "" {
			return f.podsJSON, nil
		}
		return `{"items":[]}`, nil
	case strings.Contains(joined, "get replicaset"):
		return `{"metadata":{"ownerReferences":[{"kind":"Deployment","name":"app"}]}}`, nil
	case strings.Contains(joined, "scale "):
		f.scaleCalls = append(f.scaleCalls, joined)
		return "", nil
	case strings.Contains(joined, "wait --for=delete"):
		return "", nil
	case strings.Contains(joined, "apply --server-side"):
		f.appliedObjects = stdin
		return "deployment.apps/app serverside-applied\n", nil
	case strings.Contains(joined, "get clusterroles.rbac.authorization.k8s.io"):
		return "apiVersion: rbac.authorization.k8s.io/v1\nkind: List\nitems:\n" +
			"- apiVersion: rbac.authorization.k8s.io/v1\n  kind: ClusterRole\n  metadata: {name: umkim-thin-console}\n" +
			"- apiVersion: rbac.authorization.k8s.io/v1\n  kind: ClusterRole\n  metadata: {name: identity-reader}\n", nil
	case strings.Contains(joined, "get deployments.apps"):
		return "apiVersion: apps/v1\nkind: List\nitems:\n" +
			"- apiVersion: apps/v1\n  kind: Deployment\n  metadata: {name: umkim-thin-console, namespace: umkim-thin}\n" +
			"- apiVersion: apps/v1\n  kind: Deployment\n  metadata: {name: app, namespace: gitea}\n", nil
	case strings.Contains(joined, "get namespaces"):
		return "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Namespace\n  metadata:\n    name: demo\n    resourceVersion: \"1\"\n", nil
	case strings.Contains(joined, "wait --for=condition=Ready"):
		return "", nil
	case strings.Contains(joined, "du -sk"):
		return "1024\t/data", nil
	case strings.Contains(joined, "delete pod"):
		return "", nil
	case strings.Contains(joined, "apply -f -"):
		if f.fail {
			return "", os.ErrPermission
		}
		if strings.Contains(stdin, "kind: Pod") {
			f.pods = append(f.pods, stdin)
			return "pod created", nil
		}
		f.applied = append(f.applied, stdin)
		return "applied", nil
	case strings.Contains(joined, "delete volumesnapshot"):
		f.applied = append(f.applied, "delete "+joined)
		return "", nil
	case strings.Contains(joined, "list -A -o json"):
		apps := f.apps
		if len(apps) == 0 {
			apps = []string{"gitea/gitea/gitea-0.1.20"}
		}
		var items []string
		for _, a := range apps {
			parts := strings.SplitN(a, "/", 3)
			items = append(items, `{"name":"`+parts[0]+`","namespace":"`+parts[1]+`","chart":"`+parts[2]+
				`","app_version":"1.0","revision":"1","status":"deployed"}`)
		}
		return `[` + strings.Join(items, ",") + `]`, nil
	}
	return "", nil
}

func (f *fakeCluster) streamIn(_ context.Context, _ string, r io.Reader, _ ...string) error {
	b, err := io.ReadAll(r)
	f.restored = append(f.restored, b...)
	return err
}

func (f *fakeCluster) stream(_ context.Context, _ string, w io.Writer, _ ...string) error {
	if len(f.streamData) == 0 {
		_, err := w.Write([]byte("volume-data"))
		return err
	}
	_, err := w.Write(f.streamData)
	return err
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
		Dir:         filepath.Join(state, "backups"),
		StateDir:    state,
		SeedDir:     seed,
		Kubeconfig:  "/tmp/kubeconfig",
		Version:     "0.3.0",
		Zone:        "example.com",
		Keep:        2,
		Snapshots:   true,
		HelperImage: "test-image",
		Run:         fake.run,
		RunStream:   fake.stream,
		Now:         func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) },
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
	// Kubernetes object names are lowercase RFC 1123: the archive stamp's T and
	// Z must not leak in (the lab rejected every snapshot before this).
	if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(n) {
		t.Fatalf("snapshot name %q is not a lowercase RFC 1123 name", n)
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

// A ConfigMap mount is a directory of "..<timestamp>" entries plus a stable
// symlink per key. The archive must carry the stable name, not the internals.
func TestCreateFollowsConfigMapSymlinks(t *testing.T) {
	m, _, seed := newTestManager(t)
	// Replace the plain config.env with the projected shape Kubernetes uses.
	if err := os.Remove(filepath.Join(seed, "config.env")); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(seed, "..2026_11_02_12_00_00.000000000")
	if err := os.MkdirAll(stamp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stamp, "config.env"), []byte("UMKIM_THIN_ZONE=example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(stamp), filepath.Join(seed, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/config.env", filepath.Join(seed, "config.env")); err != nil {
		t.Fatal(err)
	}

	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	mf, _, err := scanArchive(filepath.Join(m.Dir(), a.Name))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range mf.Files {
		paths = append(paths, f.Path)
	}
	found := false
	for _, p := range paths {
		if p == "seed/config.env" {
			found = true
		}
		if strings.Contains(p, "..2026") || strings.Contains(p, "..data") {
			t.Fatalf("archive contains ConfigMap internals: %v", paths)
		}
	}
	if !found {
		t.Fatalf("seed/config.env is missing: %v", paths)
	}
}

// A failed snapshot apply is kept in the manifest for the operator, but the
// archive's snapshot count must only include the ones that exist.
func TestArchiveCountsOnlyCreatedSnapshots(t *testing.T) {
	a := archiveFrom("x.tar.gz", 1, Manifest{Snapshots: []SnapshotRecord{
		{Namespace: "gitea", PVC: "data", Snapshot: "ut-1-data-abc", Ready: true},
		{Namespace: "gitea", PVC: "notready", Snapshot: "ut-1-notready-def"},
		{Namespace: "gitea", PVC: "postgres", Error: "invalid name"},
	}})
	if a.Snapshots != 1 {
		t.Fatalf("snapshots = %d, want 1 (a not-ready snapshot is not counted)", a.Snapshots)
	}
}

// The list must use the same snapshot count as create/verify: an archive whose
// applies all failed lists zero snapshots, not the number of attempts.
func TestListCountsOnlyCreatedSnapshots(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.cfg.Run = func(name, stdin string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get volumesnapshotclass -o json"):
			return `{"items":[{"metadata":{"name":"longhorn-snapshot"}}]}`, nil
		case strings.Contains(joined, "get pvc -A"):
			return `{"items":[{"metadata":{"namespace":"gitea","name":"gitea-data"},"spec":{"volumeName":"pv-gitea"}}]}`, nil
		case strings.Contains(joined, "get pv -o json"):
			return `{"items":[{"metadata":{"name":"pv-gitea"},"spec":{"csi":{"driver":"driver.longhorn.io"}}}]}`, nil
		case strings.Contains(joined, "apply -f -"):
			return "", os.ErrInvalid
		}
		return "", nil
	}
	if _, err := m.Create(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Snapshots != 0 {
		t.Fatalf("list = %+v, want one archive with 0 snapshots", list)
	}
	mf, err := m.Manifest(list[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Snapshots) != 1 || mf.Snapshots[0].Error == "" {
		t.Fatalf("manifest lost the failed apply: %+v", mf.Snapshots)
	}
}

// NFS (or local-path) PVCs have no CSI source; the snapshot controller refuses
// them, so they are skipped with a warning instead of creating broken objects.
func TestSnapshotsSkipNonCSIVolumes(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data", "nextcloud/nextcloud-data"},
		nfs:   "nextcloud-data",
	}
	m.cfg.Run = f.run
	a, err := m.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Snapshots != 1 {
		t.Fatalf("snapshots = %d, want only the CSI-backed PVC", a.Snapshots)
	}
	if len(a.Warnings) == 0 || !strings.Contains(strings.Join(a.Warnings, " "), "nextcloud/nextcloud-data") {
		t.Fatalf("warnings = %v, want the skipped NFS PVC named", a.Warnings)
	}
	mf, _, err := scanArchive(filepath.Join(m.Dir(), a.Name))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range mf.Snapshots {
		if s.PVC == "nextcloud-data" {
			t.Fatalf("an NFS PVC was snapshotted: %+v", s)
		}
	}
}

// A selective backup records only the chosen apps and snapshots only the
// chosen volumes; the rest of the cluster is untouched.
func TestCreateSelectedFiltersAppsAndVolumes(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data", "immich/immich-data", "umkim-thin/umkim-thin-state"},
		apps:  []string{"gitea/gitea/gitea-0.1.20", "immich/immich/immich-0.1.9"},
	}
	m.cfg.Run = f.run
	a, err := m.CreateSelected(context.Background(), "selected", &Selection{
		Apps:    []string{"immich"},
		Volumes: []string{"immich/immich-data"},
	})
	if err != nil {
		t.Fatalf("CreateSelected: %v", err)
	}
	if !a.Partial || a.Selection == nil {
		t.Fatalf("archive is not marked partial: %+v", a)
	}
	if a.Apps != 1 || a.Snapshots != 1 {
		t.Fatalf("archive counts = %d apps, %d snapshots, want 1/1", a.Apps, a.Snapshots)
	}
	mf, err := m.Manifest(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Apps) != 1 || mf.Apps[0].Name != "immich" {
		t.Fatalf("manifest apps = %+v, want only immich", mf.Apps)
	}
	if mf.Selection == nil || len(mf.Selection.Volumes) != 1 || mf.Selection.Volumes[0] != "immich/immich-data" {
		t.Fatalf("manifest selection = %+v", mf.Selection)
	}
	if len(f.applied) != 1 || !strings.Contains(f.applied[0], "immich-data") {
		t.Fatalf("applied snapshots = %v, want only immich-data", f.applied)
	}
}

// Apps-only selection keeps the metadata and takes no snapshots.
func TestCreateSelectedAppsOnly(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data"},
		apps:  []string{"gitea/gitea/gitea-0.1.20", "immich/immich/immich-0.1.9"},
	}
	m.cfg.Run = f.run
	a, err := m.CreateSelected(context.Background(), "apps", &Selection{Apps: []string{"gitea"}})
	if err != nil {
		t.Fatalf("CreateSelected: %v", err)
	}
	if a.Snapshots != 0 || len(f.applied) != 0 {
		t.Fatalf("apps-only backup took %d snapshots (%v)", a.Snapshots, f.applied)
	}
	if len(a.Warnings) != 0 {
		t.Fatalf("apps-only backup warned: %v", a.Warnings)
	}
}

func TestCreateSelectedRejectsUnknownAndEmpty(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data"},
		apps:  []string{"gitea/gitea/gitea-0.1.20"},
	}
	m.cfg.Run = f.run
	cases := []*Selection{
		{},
		{Apps: []string{"nope"}},
		{Volumes: []string{"gitea/nope"}},
		{Apps: []string{"gitea"}, Volumes: []string{"umkim-thin/umkim-thin-state"}},
	}
	for _, sel := range cases {
		if _, err := m.CreateSelected(context.Background(), "bad", sel); !errors.Is(err, ErrSelection) {
			t.Fatalf("selection %+v: err = %v, want ErrSelection", sel, err)
		}
	}
	if list, err := m.List(); err != nil || len(list) != 0 {
		t.Fatalf("a rejected selection wrote an archive: %v %v", list, err)
	}
}

// A selected volume the storage cannot snapshot is a warning: the archive is
// still written, and it names what it does not cover.
func TestCreateSelectedWarnsOnNonCSIVolume(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"nextcloud/nextcloud-data"},
		apps:  []string{"nextcloud/nextcloud/nextcloud-0.1.9"},
		nfs:   "nextcloud-data",
	}
	m.cfg.Run = f.run
	a, err := m.CreateSelected(context.Background(), "nfs", &Selection{
		Volumes: []string{"nextcloud/nextcloud-data"},
	})
	if err != nil {
		t.Fatalf("CreateSelected: %v", err)
	}
	if a.Snapshots != 0 {
		t.Fatalf("snapshots = %d, want 0", a.Snapshots)
	}
	if len(a.Warnings) == 0 || !strings.Contains(strings.Join(a.Warnings, " "), "nextcloud/nextcloud-data") {
		t.Fatalf("warnings = %v, want the selected NFS volume named", a.Warnings)
	}
	if len(f.applied) != 0 {
		t.Fatalf("applied = %v, want no snapshot for an NFS volume", f.applied)
	}
}

// The scope view offers application releases and application PVCs only, and
// maps each volume to the app in its namespace.
func TestScopeListsAppsAndVolumes(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data", "immich/immich-data", "umkim-thin/umkim-thin-state", "nextcloud/nextcloud-data"},
		apps:  []string{"gitea/gitea/gitea-0.1.20", "immich/immich/immich-0.1.9", "identity/identity/identity-0.1.9", "umkim-thin/umkim-thin/umkim-thin-0.1.5"},
		nfs:   "nextcloud-data",
	}
	m.cfg.Run = f.run
	view, err := m.Scope()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range view.Apps {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "gitea,immich" {
		t.Fatalf("scope apps = %v, want gitea,immich", names)
	}
	if len(view.Volumes) != 3 {
		t.Fatalf("scope volumes = %+v, want three application volumes", view.Volumes)
	}
	byPVC := map[string]VolumeRecord{}
	for _, v := range view.Volumes {
		byPVC[v.Namespace+"/"+v.PVC] = v
	}
	if v := byPVC["immich/immich-data"]; v.App != "immich" || !v.CSI || v.Size != "1Gi" || v.Class != "longhorn-rwo" {
		t.Fatalf("immich volume = %+v", v)
	}
	if v := byPVC["nextcloud/nextcloud-data"]; v.CSI {
		t.Fatalf("an NFS volume was reported as CSI: %+v", v)
	}
	if _, ok := byPVC["umkim-thin/umkim-thin-state"]; ok {
		t.Fatal("a platform volume leaked into the scope")
	}
}

// A copy-mode backup streams the selected volume into a sidecar file and
// records it in the manifest.
func TestCreateCopyModeWritesDataFile(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data", "umkim-thin/umkim-thin-state"},
		apps:  []string{"gitea/gitea/gitea-0.1.20"},
	}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.Schedule = NewScheduleStore("", Schedule{Enabled: true, Interval: "24h", Keep: 7, DataMode: DataCopy})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data"}})
	if err != nil {
		t.Fatalf("CreateSelected: %v", err)
	}
	if a.Data != 1 || a.DataBytes == 0 {
		t.Fatalf("archive data = %d records, %d bytes", a.Data, a.DataBytes)
	}
	mf, err := m.Manifest(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Data) != 1 || mf.Data[0].Method != "copy" || mf.Data[0].File == "" {
		t.Fatalf("manifest data = %+v", mf.Data)
	}
	p := filepath.Join(m.Dir(), mf.Data[0].File)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("the sidecar copy is missing: %v", err)
	}
	if info.Size() != mf.Data[0].Size {
		t.Fatalf("sidecar size = %d, manifest says %d", info.Size(), mf.Data[0].Size)
	}
	if len(f.applied) != 0 {
		t.Fatalf("copy mode created snapshots: %v", f.applied)
	}
	if len(f.pods) != 1 {
		t.Fatalf("helper pods = %d, want 1", len(f.pods))
	}
}

// Auto mode snapshots what the storage can snapshot and copies the rest.
func TestCreateAutoModeSnapshotsThenCopies(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data", "immich/immich-data"},
		apps:  []string{"gitea/gitea/gitea-0.1.20", "immich/immich/immich-0.1.9"},
		nfs:   "immich-data",
	}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.Schedule = NewScheduleStore("", Schedule{Enabled: true, Interval: "24h", Keep: 7, DataMode: DataAuto})
	a, err := m.CreateSelected(context.Background(), "auto", &Selection{Volumes: []string{
		"gitea/gitea-data", "immich/immich-data",
	}})
	if err != nil {
		t.Fatalf("CreateSelected: %v", err)
	}
	if a.Snapshots != 1 {
		t.Fatalf("snapshots = %d, want the CSI volume", a.Snapshots)
	}
	if a.Data != 1 {
		t.Fatalf("copies = %d, want the NFS volume", a.Data)
	}
	mf, err := m.Manifest(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Data) != 1 || mf.Data[0].PVC != "immich-data" {
		t.Fatalf("copied %+v, want immich-data", mf.Data)
	}
	if len(f.applied) != 1 || !strings.Contains(f.applied[0], "gitea-data") {
		t.Fatalf("snapshots = %v, want gitea-data", f.applied)
	}
}

func TestVerifyDetectsMissingDataFile(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{class: "longhorn-snapshot", pvcs: []string{"gitea/gitea-data"}}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.Schedule = NewScheduleStore("", Schedule{Enabled: true, Interval: "24h", DataMode: DataCopy})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data"}})
	if err != nil {
		t.Fatal(err)
	}
	mf, err := m.Manifest(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(m.Dir(), mf.Data[0].File)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(a.Name); err == nil || !strings.Contains(err.Error(), "data file") {
		t.Fatalf("Verify = %v, want a missing-data error", err)
	}
}

func TestDeleteRemovesDataFile(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{class: "longhorn-snapshot", pvcs: []string{"gitea/gitea-data"}}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.Schedule = NewScheduleStore("", Schedule{Enabled: true, Interval: "24h", DataMode: DataCopy})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data"}})
	if err != nil {
		t.Fatal(err)
	}
	mf, err := m.Manifest(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(m.Dir(), mf.Data[0].File)
	if err := m.Delete(a.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("the sidecar copy survived Delete: %v", err)
	}
}

func TestScheduleDataMode(t *testing.T) {
	if got := (Schedule{Snapshots: true}).Mode(); got != DataSnapshot {
		t.Fatalf("legacy snapshots=true mode = %q", got)
	}
	if got := (Schedule{Snapshots: false}).Mode(); got != DataOff {
		t.Fatalf("legacy snapshots=false mode = %q", got)
	}
	if got := (Schedule{DataMode: DataAuto}).Mode(); got != DataAuto {
		t.Fatalf("explicit mode = %q", got)
	}
	if err := (Schedule{Enabled: true, Interval: "24h", DataMode: "bogus"}).Validate(); err == nil {
		t.Fatal("an unknown data mode was accepted")
	}
}

// A platform backup exports the cluster objects, keeps the console state and
// copies the platform volumes (identity/Traefik), separately from app data.
func TestCreatePlatform(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data", "identity/lldap-data", "umkim-thin/umkim-thin-state"},
	}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	a, err := m.CreatePlatform(context.Background(), "platform")
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}
	if a.Kind != KindPlatform {
		t.Fatalf("kind = %q, want %q", a.Kind, KindPlatform)
	}
	if !strings.Contains(a.Name, "-platform.tar.gz") {
		t.Fatalf("name = %q, want a -platform archive", a.Name)
	}
	if a.Data != 1 {
		t.Fatalf("copies = %d, want only the identity volume", a.Data)
	}
	mf, err := m.Manifest(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if mf.Kind != KindPlatform {
		t.Fatalf("manifest kind = %q", mf.Kind)
	}
	var paths []string
	for _, file := range mf.Files {
		paths = append(paths, file.Path)
	}
	joined := strings.Join(paths, ",")
	if !strings.Contains(joined, "platform/k8s-objects.yaml") || !strings.Contains(joined, "platform/README.txt") {
		t.Fatalf("platform files = %v", paths)
	}
	if len(mf.Data) != 1 || mf.Data[0].PVC != "lldap-data" {
		t.Fatalf("platform copies = %+v, want lldap-data", mf.Data)
	}
	if _, err := m.Verify(a.Name); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestRestorePlatformRestoresStateAndSkipsObjects(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{class: "longhorn-snapshot", pvcs: []string{"identity/lldap-data"}}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	a, err := m.CreatePlatform(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := m.Restore(a.Name)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !rep.Platform {
		t.Fatal("the restore report does not mark a platform archive")
	}
	if len(rep.Restored) == 0 {
		t.Fatal("the platform restore restored nothing")
	}
	var skipped []string
	for _, s := range rep.Skipped {
		if strings.HasPrefix(s, "platform/") {
			skipped = append(skipped, s)
		}
	}
	if len(skipped) == 0 {
		t.Fatalf("the object export was not skipped: %v", rep.Skipped)
	}
}

func TestArchiveNameKinds(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if got := archiveName(now, KindData); got != "umkim-thin-20261102T120000Z.tar.gz" {
		t.Fatalf("data name = %q", got)
	}
	if got := archiveName(now, KindPlatform); got != "umkim-thin-20261102T120000Z-platform.tar.gz" {
		t.Fatalf("platform name = %q", got)
	}
	for _, name := range []string{
		"umkim-thin-20261102T120000Z.tar.gz",
		"umkim-thin-20261102T120000Z-2.tar.gz",
		"umkim-thin-20261102T120000Z-platform.tar.gz",
		"umkim-thin-20261102T120000Z-platform-2.tar.gz",
	} {
		if !nameRE.MatchString(name) {
			t.Fatalf("nameRE rejected %q", name)
		}
	}
	if nameRE.MatchString("umkim-thin-20261102T120000Z-platform.tar.gz.evil") {
		t.Fatal("nameRE accepted a suffix escape")
	}
}

// The platform object export must be replayable: server-assigned metadata,
// status, controller ownership, Service IPs and PVC bindings are stripped.
func TestSanitizePlatformObjects(t *testing.T) {
	raw := `apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: s
    resourceVersion: "1"
    uid: abc
    creationTimestamp: "2026-01-01T00:00:00Z"
    managedFields: []
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: huge
      keep: yes
  data: {k: dmFsdWU=}
  status: {}
- apiVersion: v1
  kind: Service
  metadata: {name: svc, namespace: ns}
  spec: {clusterIP: 10.0.0.1, clusterIPs: [10.0.0.1], ports: []}
- apiVersion: v1
  kind: Service
  metadata: {name: headless, namespace: ns}
  spec: {clusterIP: None, ports: []}
- apiVersion: v1
  kind: Pod
  metadata:
    name: owned
    ownerReferences: [{kind: ReplicaSet, name: rs, uid: x}]
- apiVersion: v1
  kind: PersistentVolumeClaim
  metadata:
    name: pvc
    annotations:
      pv.kubernetes.io/bind-completed: "yes"
      volume.kubernetes.io/selected-node: node-1
      keep: yes
  spec: {volumeName: pv-1, accessModes: [ReadWriteOnce]}
- apiVersion: apps/v1
  kind: Deployment
  metadata: {name: d}
  spec:
    template:
      metadata:
        annotations: {kubectl.kubernetes.io/restartedAt: "2026-01-01T00:00:00Z", keep: yes}
      spec: {containers: []}
- apiVersion: v1
  kind: PersistentVolume
  metadata:
    name: static-pv
    annotations: {keep: yes}
  spec:
    capacity: {storage: 10Mi}
    claimRef: {namespace: nfs, name: root, uid: old}
- apiVersion: v1
  kind: PersistentVolume
  metadata:
    name: dynamic-pv
    annotations: {pv.kubernetes.io/provisioned-by: rancher.io/local-path}
  spec: {capacity: {storage: 1Gi}}
- apiVersion: v1
  kind: Secret
  metadata: {name: k3s-serving, namespace: kube-system}
  type: kubernetes.io/tls
  data: {tls.crt: b2xk, tls.key: b2xk}
- apiVersion: v1
  kind: ConfigMap
  metadata: {name: kube-root-ca.crt, namespace: ns}
  data: {ca.crt: b2xk}
`
	out, err := sanitizeObjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"resourceVersion", "creationTimestamp", "managedFields", "status:", "last-applied-configuration", "10.0.0.1", "volumeName", "bind-completed", "selected-node", "restartedAt", "owned"} {
		if strings.Contains(out, bad) {
			t.Fatalf("sanitized export still contains %q:\n%s", bad, out)
		}
	}
	if strings.Contains(out, "dynamic-pv") {
		t.Fatal("a dynamically provisioned PV was exported")
	}
	if strings.Contains(out, "k3s-serving") || strings.Contains(out, "kube-root-ca.crt") {
		t.Fatalf("cluster identity was exported:\n%s", out)
	}
	if !strings.Contains(out, "clusterIP: None") {
		t.Fatalf("the headless Service lost clusterIP: None:\n%s", out)
	}
	if !strings.Contains(out, "static-pv") || strings.Contains(out, "claimRef") {
		t.Fatalf("the static PV was dropped or kept its claimRef:\n%s", out)
	}
	for _, want := range []string{"keep:", "dmFsdWU=", "name: svc", "name: pvc"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sanitized export lost %q:\n%s", want, out)
		}
	}
}

func TestRestoreVolumesExtractsTheCopy(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{class: "longhorn-snapshot", pvcs: []string{"gitea/gitea-data"}}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.RunStreamIn = f.streamIn
	m.cfg.Schedule = NewScheduleStore("", Schedule{DataMode: DataCopy, Keep: 7})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data"}})
	if err != nil {
		t.Fatal(err)
	}
	vols, err := m.RestoreVolumes(context.Background(), a.Name)
	if err != nil {
		t.Fatalf("RestoreVolumes: %v", err)
	}
	if len(vols) != 1 || !vols[0].Restored || vols[0].PVC != "gitea-data" {
		t.Fatalf("volumes = %+v", vols)
	}
	if string(f.restored) != "volume-data" {
		t.Fatalf("restore streamed %q, want the sidecar contents", f.restored)
	}
}

func TestRestoreVolumesStopsAndRestartsWorkloads(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data"},
		podsJSON: `{"items":[{"metadata":{"name":"app-1","ownerReferences":[{"kind":"ReplicaSet","name":"app-rs"}]},` +
			`"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"gitea-data"}}]}}]}`,
	}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.RunStreamIn = f.streamIn
	m.cfg.Schedule = NewScheduleStore("", Schedule{DataMode: DataCopy, Keep: 7})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RestoreVolumes(context.Background(), a.Name); err != nil {
		t.Fatal(err)
	}
	if len(f.scaleCalls) != 2 ||
		!strings.Contains(f.scaleCalls[0], "deployment/app --replicas=0") ||
		!strings.Contains(f.scaleCalls[1], "deployment/app --replicas=1") {
		t.Fatalf("scale calls = %v, want stop then start", f.scaleCalls)
	}
}

func TestRestoreVolumesBlocksOnDaemonSet(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data"},
		podsJSON: `{"items":[{"metadata":{"name":"agent","ownerReferences":[{"kind":"DaemonSet","name":"agent"}]},` +
			`"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"gitea-data"}}]}}]}`,
	}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.RunStreamIn = f.streamIn
	m.cfg.Schedule = NewScheduleStore("", Schedule{DataMode: DataCopy, Keep: 7})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data"}})
	if err != nil {
		t.Fatal(err)
	}
	vols, err := m.RestoreVolumes(context.Background(), a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 || vols[0].Restored || !strings.Contains(vols[0].Error, "DaemonSet") {
		t.Fatalf("volumes = %+v, want a DaemonSet block", vols)
	}
	if len(f.restored) != 0 {
		t.Fatal("a blocked volume was extracted anyway")
	}
}

func TestRestoreVolumesIgnoresCompletedJobPods(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data"},
		podsJSON: `{"items":[{"metadata":{"name":"gitea-bootstrap-abc","ownerReferences":[{"kind":"Job","name":"gitea-bootstrap"}]},` +
			`"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"gitea-data"}}]},"status":{"phase":"Succeeded"}}]}`,
	}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.RunStreamIn = f.streamIn
	m.cfg.Schedule = NewScheduleStore("", Schedule{DataMode: DataCopy, Keep: 7})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data"}})
	if err != nil {
		t.Fatal(err)
	}
	vols, err := m.RestoreVolumes(context.Background(), a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 || !vols[0].Restored {
		t.Fatalf("volumes = %+v, want a restore despite the completed bootstrap Job", vols)
	}
}

func TestRestoreVolumesSelectedFilters(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{class: "longhorn-snapshot", pvcs: []string{"gitea/gitea-data", "gitea/gitea-other"}}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.RunStreamIn = f.streamIn
	m.cfg.Schedule = NewScheduleStore("", Schedule{DataMode: DataCopy, Keep: 7})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data", "gitea/gitea-other"}})
	if err != nil {
		t.Fatal(err)
	}
	vols, err := m.RestoreVolumesSelected(context.Background(), a.Name, []string{"gitea/gitea-other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 || vols[0].PVC != "gitea-other" || !vols[0].Restored {
		t.Fatalf("volumes = %+v, want only gitea-other", vols)
	}
}

func TestApplyPlatform(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{class: "longhorn-snapshot", pvcs: []string{"identity/lldap-data"}}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.RunStreamIn = f.streamIn
	a, err := m.CreatePlatform(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := m.ApplyPlatform(context.Background(), a.Name)
	if err != nil {
		t.Fatalf("ApplyPlatform: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("ApplyPlatform returned no applied objects")
	}
	if !strings.Contains(f.appliedObjects, "kind: List") {
		t.Fatalf("the object export was not sent to apply: %q", f.appliedObjects)
	}
	// A data archive must not be applied as a platform export.
	d, err := m.CreateSelected(context.Background(), "data", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyPlatform(context.Background(), d.Name); err == nil {
		t.Fatal("ApplyPlatform accepted a data archive")
	}
}

// A restore's safety archive is metadata only: it must not copy every volume
// (hours) nor prune anything.
func TestRestoreSafetyArchiveIsMetadataOnly(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{class: "longhorn-snapshot", pvcs: []string{"gitea/gitea-data"}}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.RunStreamIn = f.streamIn
	m.cfg.Schedule = NewScheduleStore("", Schedule{DataMode: DataCopy, Keep: 7})
	a, err := m.CreateSelected(context.Background(), "copy", &Selection{Volumes: []string{"gitea/gitea-data"}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := m.Restore(a.Name)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	mf, err := m.Manifest(rep.SafetyArchive)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Data) != 0 {
		t.Fatalf("the safety archive copied volumes: %+v", mf.Data)
	}
	if mf.Kind != KindData {
		t.Fatalf("safety kind = %q", mf.Kind)
	}
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	sidecars := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), "-data-") {
			sidecars++
		}
	}
	if sidecars != 1 {
		t.Fatalf("sidecars = %d, want only the original archive's copy", sidecars)
	}
}

// A platform apply must not roll the console Deployment that is running it.
func TestApplyPlatformSkipsItsOwnDeployment(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{class: "longhorn-snapshot", pvcs: []string{"identity/lldap-data"}}
	m.cfg.Run = f.run
	m.cfg.RunStream = f.stream
	m.cfg.RunStreamIn = f.streamIn
	m.cfg.SelfNamespace = "umkim-thin"
	a, err := m.CreatePlatform(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyPlatform(context.Background(), a.Name); err != nil {
		t.Fatalf("ApplyPlatform: %v", err)
	}
	if strings.Contains(f.appliedObjects, "umkim-thin-console") {
		t.Fatal("the console Deployment was applied (it would restart the console)")
	}
	if !strings.Contains(f.appliedObjects, "name: app") {
		t.Fatalf("the other Deployment was dropped too: %s", f.appliedObjects)
	}
	if strings.Contains(f.appliedObjects, "umkim-thin-console") {
		t.Fatal("the console ClusterRole was applied (it would downgrade its RBAC)")
	}
	if !strings.Contains(f.appliedObjects, "identity-reader") {
		t.Fatal("an unrelated ClusterRole was dropped")
	}
}
