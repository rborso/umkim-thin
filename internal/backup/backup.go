// Package backup creates, verifies and restores umkim-thin backups.
//
// A backup is one gzipped tar archive with a manifest: the console's own state
// (invites, app state, WireGuard peers, per-app Helm values), the installer
// seed (secrets, identity values), the installed app inventory, and — when the
// cluster has a CSI snapshot class — a VolumeSnapshot per application PVC.
//
// It deliberately does not copy application PVC data itself. Mounting every
// app volume to read it would fight the running workloads and their access
// modes; the snapshot is the data half, the archive is the metadata half.
//
// Restore is two-step on purpose: it first writes a safety archive of the
// current state, then puts the archived state back. Application data comes back
// from the snapshots, which the console reports but does not silently apply.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Schema is the manifest version. A restore refuses a newer schema.
const Schema = 1

// ErrSelection marks a backup request whose selection cannot be fulfilled
// (empty, unknown names, or volumes the storage cannot snapshot). Callers map
// it to a client error instead of an internal failure.
var ErrSelection = errors.New("invalid backup selection")

// Archive kinds. A data backup carries the console state, the app inventory
// and the selected app volumes; a platform backup carries the whole cluster
// (object export, host/console state and the identity stack's volumes).
const (
	KindData     = "data"
	KindPlatform = "platform"
)

// nameRE is the only archive name shape the API and CLI accept. It rules out
// path separators before any path is built from user input.
var nameRE = regexp.MustCompile(`^umkim-thin-[0-9]{8}T[0-9]{6}Z(-platform)?(-[0-9]+)?\.tar\.gz$`)

// seedRestoreFiles are the seed files a restore puts back. config.env is
// archived for reference but not restored: the installer owns it, and
// overwriting it would silently revert an operator's settings.
var seedRestoreFiles = map[string]bool{
	"secrets.yml":         true,
	"identity-values.yml": true,
	"identity-apps.yml":   true,
}

// stateSkipDirs are console state directories that are rebuilt, not restored.
var stateSkipDirs = map[string]bool{
	"backups":      true, // the archives themselves
	"chart-mirror": true, // re-packaged by the installer
	"lost+found":   true,
}

// Config points the manager at the console's directories and tools.
type Config struct {
	// Dir is where archives are written. Default <StateDir>/backups.
	Dir string
	// StateDir is the console PVC (/data).
	StateDir string
	// SeedDir is the installer seed (/etc/umkim-thin).
	SeedDir string
	// Helm and Kubectl are the binaries the inventory and snapshots use.
	Helm       string
	Kubectl    string
	Kubeconfig string
	// Version and Zone are recorded in the manifest.
	Version string
	Zone    string
	// Keep is the retention count. Zero keeps everything.
	Keep int
	// Snapshots enables the CSI VolumeSnapshot half.
	Snapshots bool
	// Schedule, when set, supplies the live retention and snapshot switches the
	// console edits; it overrides Keep and Snapshots.
	Schedule *ScheduleStore
	// TargetRoot holds the extra targets the chart mounts (/backups/<name>).
	TargetRoot string
	// SelfNamespace is the console's own namespace. A platform apply skips the
	// Deployment there so restoring objects cannot restart the process doing
	// it. Defaults to the pod's service-account namespace.
	SelfNamespace string
	// HelperImage runs the volume file copy (tar). Defaults to the console
	// image, which the installer imports on the node that runs the console.
	HelperImage string
	// SnapshotWait bounds how long Create waits for the snapshots to become
	// ready (default 90s), polling every SnapshotPoll (default 3s).
	SnapshotWait time.Duration
	SnapshotPoll time.Duration
	// Now is injectable for tests.
	Now func() time.Time
	// Run executes a tool (with optional stdin). Injectable for tests.
	Run func(name, stdin string, args ...string) (string, error)
	// RunStream executes a tool and copies its stdout into w (the volume copy).
	RunStream func(ctx context.Context, name string, w io.Writer, args ...string) error
	// RunStreamIn executes a tool with r as stdin (the volume restore).
	RunStreamIn func(ctx context.Context, name string, r io.Reader, args ...string) error
}

// AppRecord is one installed catalog release at backup time.
type AppRecord struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace,omitempty"`
	Chart        string `json:"chart,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`
	AppVersion   string `json:"appVersion,omitempty"`
	Revision     string `json:"revision,omitempty"`
	Status       string `json:"status,omitempty"`
}

// Selection narrows a backup to specific apps and volumes. A nil selection is
// a full backup: every application release and every application PVC. Apps are
// Helm release names (the catalog app name); volumes are "namespace/pvc".
//
// The console state is always archived whole. It is small, global and shared
// between apps (invites, peers, app state); filtering it would restore a
// console that disagrees with itself. The selection controls the app inventory
// recorded for a reinstall and which volumes get a storage snapshot.
type Selection struct {
	Apps    []string `json:"apps,omitempty"`
	Volumes []string `json:"volumes,omitempty"`
}

// Empty reports whether the selection names nothing.
func (s *Selection) Empty() bool {
	return s == nil || (len(s.Apps) == 0 && len(s.Volumes) == 0)
}

// VolumeRecord is one application PVC offered for a selective backup.
type VolumeRecord struct {
	Namespace string `json:"namespace"`
	PVC       string `json:"pvc"`
	App       string `json:"app,omitempty"`
	Size      string `json:"size,omitempty"`
	Class     string `json:"class,omitempty"`
	// CSI is false for NFS and local-path volumes: the snapshot API cannot
	// copy those, so they are listed (and selectable) with that caveat.
	CSI bool `json:"csi"`
}

// ScopeView is what a selective backup can include.
type ScopeView struct {
	Apps    []AppRecord    `json:"apps"`
	Volumes []VolumeRecord `json:"volumes"`
}

// FileRecord is one archived file with its checksum.
type FileRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SnapshotRecord is one CSI snapshot taken with the backup.
type SnapshotRecord struct {
	Namespace string `json:"namespace"`
	PVC       string `json:"pvc"`
	Snapshot  string `json:"snapshot,omitempty"`
	Class     string `json:"class,omitempty"`
	// Ready is set when the snapshot reached readyToUse before the backup
	// finished; only those are counted in the archive summary.
	Ready bool   `json:"ready,omitempty"`
	Error string `json:"error,omitempty"`
}

// DataRecord is one file copy of a volume, used when the storage cannot
// snapshot it. The copy is a crash-consistent tar of the mounted files.
type DataRecord struct {
	Namespace string `json:"namespace"`
	PVC       string `json:"pvc"`
	// File is the sidecar archive next to the metadata archive.
	File   string `json:"file,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Method string `json:"method"` // "copy"
	Error  string `json:"error,omitempty"`
}

// Manifest is the archive's index. It is the first entry in the tar.
type Manifest struct {
	Schema  int       `json:"schema"`
	Created time.Time `json:"created"`
	Version string    `json:"version,omitempty"`
	Zone    string    `json:"zone,omitempty"`
	Note    string    `json:"note,omitempty"`
	// Kind is "data" or "platform"; empty means data (archives written before
	// the two kinds were separated).
	Kind      string           `json:"kind,omitempty"`
	Apps      []AppRecord      `json:"apps,omitempty"`
	Snapshots []SnapshotRecord `json:"snapshots,omitempty"`
	Data      []DataRecord     `json:"data,omitempty"`
	Files     []FileRecord     `json:"files"`
	// Selection is set for a selective backup; nil means the archive covers
	// every app and every application volume.
	Selection *Selection `json:"selection,omitempty"`
	// Warnings are non-fatal gaps (no snapshot class, an unreadable directory).
	Warnings []string `json:"warnings,omitempty"`
}

// Archive is the list/verify view of one backup.
type Archive struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Created   time.Time `json:"created"`
	Version   string    `json:"version,omitempty"`
	Zone      string    `json:"zone,omitempty"`
	Note      string    `json:"note,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Apps      int       `json:"apps"`
	Files     int       `json:"files"`
	Snapshots int       `json:"snapshots"`
	// Data counts the volume file copies (when the storage cannot snapshot).
	Data        int          `json:"data,omitempty"`
	DataBytes   int64        `json:"dataBytes,omitempty"`
	DataVolumes []DataRecord `json:"dataVolumes,omitempty"`
	// Partial marks a selective backup, with the selection that produced it.
	Partial   bool       `json:"partial,omitempty"`
	Selection *Selection `json:"selection,omitempty"`
	Warnings  []string   `json:"warnings,omitempty"`
	// Verified is set by Verify: the archive matches its manifest.
	Verified bool   `json:"verified"`
	Error    string `json:"error,omitempty"`

	// modTime orders archives written in the same second (the safety archive a
	// restore writes is newer than the one it protects).
	modTime time.Time
}

// VolumeRestore is the outcome of extracting one recorded volume copy back
// into its PVC.
type VolumeRestore struct {
	Namespace string `json:"namespace"`
	PVC       string `json:"pvc"`
	File      string `json:"file,omitempty"`
	Restored  bool   `json:"restored,omitempty"`
	Error     string `json:"error,omitempty"`
}

// RestoreReport says what a restore did.
type RestoreReport struct {
	SafetyArchive string           `json:"safetyArchive,omitempty"`
	Restored      []string         `json:"restored"`
	Skipped       []string         `json:"skipped,omitempty"`
	Snapshots     []SnapshotRecord `json:"snapshots,omitempty"`
	// Platform marks a platform archive.
	Platform bool `json:"platform,omitempty"`
	// Volumes is the outcome of --data (the volume copies put back).
	Volumes []VolumeRestore `json:"volumes,omitempty"`
	// Applied lists the object kinds a platform restore applied.
	Applied []string `json:"applied,omitempty"`
	// Data lists the volume file copies; the operator extracts them into a
	// volume, the console never overwrites live data.
	Data []DataRecord `json:"data,omitempty"`
}

// Manager reads and writes the backup directory.
type Manager struct{ cfg Config }

// New returns a manager with defaults filled in.
func New(cfg Config) *Manager {
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(cfg.StateDir, "backups")
	}
	if cfg.Helm == "" {
		cfg.Helm = "helm"
	}
	if cfg.Kubectl == "" {
		cfg.Kubectl = "kubectl"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SnapshotWait <= 0 {
		cfg.SnapshotWait = 90 * time.Second
	}
	if cfg.SnapshotPoll <= 0 {
		cfg.SnapshotPoll = 3 * time.Second
	}
	if cfg.TargetRoot == "" {
		cfg.TargetRoot = "/backups"
	}
	if cfg.SelfNamespace == "" {
		if raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			cfg.SelfNamespace = strings.TrimSpace(string(raw))
		}
	}
	if cfg.Run == nil {
		cfg.Run = func(name, stdin string, args ...string) (string, error) {
			return runTool(cfg.Kubeconfig, name, stdin, args...)
		}
	}
	if cfg.RunStream == nil {
		cfg.RunStream = func(ctx context.Context, name string, w io.Writer, args ...string) error {
			return runToolStream(ctx, cfg.Kubeconfig, name, w, args...)
		}
	}
	if cfg.RunStreamIn == nil {
		cfg.RunStreamIn = func(ctx context.Context, name string, r io.Reader, args ...string) error {
			return runToolStreamIn(ctx, cfg.Kubeconfig, name, r, args...)
		}
	}
	return &Manager{cfg: cfg}
}

// Dir is the archive directory.
func (m *Manager) Dir() string { return m.displayDir() }

// Create builds one full archive and prunes older ones.
func (m *Manager) Create(ctx context.Context, note string) (Archive, error) {
	return m.CreateSelected(ctx, note, nil)
}

// policy returns the retention and data mode in force: the console schedule
// when present, otherwise the installer's defaults.
func (m *Manager) policy() (keep int, mode string) {
	keep = m.cfg.Keep
	mode = DataOff
	if m.cfg.Snapshots {
		mode = DataSnapshot
	}
	if m.cfg.Schedule != nil {
		keep, mode = m.cfg.Schedule.Policy()
	}
	return keep, mode
}

// CreateSelected builds one archive with the current policy. A nil selection
// covers every app and every application volume. A non-nil selection records
// only the named apps in the inventory and snapshots only the named volumes;
// unknown names are an error, so a typo cannot silently produce an archive
// that covers nothing.
func (m *Manager) CreateSelected(ctx context.Context, note string, sel *Selection) (Archive, error) {
	keep, mode := m.policy()
	return m.createSelected(ctx, note, sel, keep, mode)
}

// createSelected is CreateSelected with an explicit policy, so a safety archive
// can be metadata-only and must not prune anything.
func (m *Manager) createSelected(ctx context.Context, note string, sel *Selection, keep int, mode string) (Archive, error) {
	if sel != nil {
		sel = cleanSelection(sel)
		if sel.Empty() {
			return Archive{}, fmt.Errorf("%w: choose at least one app or volume, or take a full backup", ErrSelection)
		}
	}
	dir, err := m.targetDir()
	if err != nil {
		return Archive{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Archive{}, fmt.Errorf("backup directory: %w", err)
	}
	now := m.cfg.Now().UTC().Truncate(time.Second)
	name := uniqueArchiveName(dir, now, KindData)
	staging, err := os.MkdirTemp(dir, ".staging-")
	if err != nil {
		return Archive{}, err
	}
	defer os.RemoveAll(staging)

	mf := Manifest{
		Schema:    Schema,
		Created:   now,
		Version:   m.cfg.Version,
		Zone:      m.cfg.Zone,
		Note:      note,
		Kind:      KindData,
		Selection: sel,
	}

	if err := m.stageTree(staging, "state", m.cfg.StateDir, stateSkipDirs, &mf); err != nil {
		return Archive{}, err
	}
	// On the host the CLI runs with StateDir and SeedDir on the same directory
	// (/etc/umkim-thin); archiving it twice would only duplicate the entries.
	if filepath.Clean(m.cfg.SeedDir) != filepath.Clean(m.cfg.StateDir) {
		if err := m.stageTree(staging, "seed", m.cfg.SeedDir, nil, &mf); err != nil {
			return Archive{}, err
		}
	}

	apps, appErr := m.apps()
	if sel == nil {
		// A full backup is still useful without the inventory; say so instead
		// of pretending the archive knows every app.
		if appErr != nil {
			mf.Warnings = append(mf.Warnings, "the app inventory is unavailable: "+shortErr(appErr))
		}
		mf.Apps = apps
	} else {
		if appErr != nil {
			return Archive{}, fmt.Errorf("reading the app inventory to validate the selection: %w", appErr)
		}
		if mf.Apps, err = selectApps(apps, sel.Apps); err != nil {
			return Archive{}, err
		}
	}

	snaps, data, warns, err := m.backupVolumes(ctx, now, sel, mode, strings.TrimSuffix(name, ".tar.gz"))
	if err != nil {
		return Archive{}, err
	}
	mf.Snapshots, mf.Data = snaps, data
	mf.Warnings = append(mf.Warnings, warns...)
	// A sidecar copy is only useful next to the archive; remove the copies if
	// the metadata archive cannot be written.
	cleanupData := func() {
		for _, d := range mf.Data {
			if d.File != "" {
				os.Remove(filepath.Join(dir, d.File))
			}
		}
	}

	raw, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		cleanupData()
		return Archive{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), raw, 0o600); err != nil {
		cleanupData()
		return Archive{}, err
	}
	target := filepath.Join(dir, name)
	if err := writeArchive(target, staging); err != nil {
		os.Remove(target)
		return Archive{}, err
	}
	if keep > 0 {
		if _, err := m.prune(keep, name); err != nil {
			mf.Warnings = append(mf.Warnings, "pruning old backups: "+err.Error())
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		return Archive{}, err
	}
	a := archiveFrom(name, info.Size(), mf)
	a.modTime = info.ModTime()
	return a, nil
}

// cleanSelection trims and dedupes a selection, returning nil when it names
// nothing.
func cleanSelection(sel *Selection) *Selection {
	out := &Selection{Apps: cleanNames(sel.Apps), Volumes: cleanNames(sel.Volumes)}
	if out.Empty() {
		return nil
	}
	return out
}

func cleanNames(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// selectApps keeps the requested releases and rejects names the cluster does
// not know (or platform releases, which are not application data).
func selectApps(all []AppRecord, want []string) ([]AppRecord, error) {
	if len(want) == 0 {
		return nil, nil
	}
	byName := make(map[string]AppRecord, len(all))
	for _, a := range all {
		byName[a.Name] = a
	}
	var out []AppRecord
	var unknown []string
	for _, name := range want {
		a, ok := byName[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		if platformNamespaces[a.Namespace] {
			unknown = append(unknown, name+" (platform release)")
			continue
		}
		out = append(out, a)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("%w: unknown app(s): %s", ErrSelection, strings.Join(unknown, ", "))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// uniqueArchiveName returns the first free name for a kind, appending -2, -3
// when two runs land in the same second.
func uniqueArchiveName(dir string, t time.Time, kind string) string {
	name := archiveName(t, kind)
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		return name
	}
	base := strings.TrimSuffix(name, ".tar.gz")
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d.tar.gz", base, i)
		if _, err := os.Stat(filepath.Join(dir, candidate)); err != nil {
			return candidate
		}
	}
}

func archiveName(t time.Time, kind string) string {
	base := "umkim-thin-" + t.Format("20060102T150405Z")
	if kind == KindPlatform {
		base += "-platform"
	}
	return base + ".tar.gz"
}

// stageTree copies a directory into the staging area, recording checksums.
// Missing directories are a warning, not a failure: a fresh install may not
// have every file yet.
func (m *Manager) stageTree(staging, prefix, dir string, skip map[string]bool, mf *Manifest) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			mf.Warnings = append(mf.Warnings, prefix+" directory is missing")
			return nil
		}
		return err
	}
	for _, e := range entries {
		// Kubernetes projects a ConfigMap/Secret as "..<timestamp>" plus the
		// "..data" symlink; only the stable link names are worth archiving.
		if skip[e.Name()] || strings.HasPrefix(e.Name(), "..") {
			continue
		}
		src := filepath.Join(dir, e.Name())
		// Stat (not the directory entry) so a mounted ConfigMap key, which is a
		// symlink to ..data/<key>, is archived under its stable name.
		if info, err := os.Stat(src); err != nil {
			mf.Warnings = append(mf.Warnings, "reading "+src+": "+err.Error())
			continue
		} else if info.IsDir() {
			err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					mf.Warnings = append(mf.Warnings, "reading "+p+": "+err.Error())
					return nil
				}
				if info.IsDir() {
					if skip[info.Name()] && p != src {
						return filepath.SkipDir
					}
					return nil
				}
				if !info.Mode().IsRegular() {
					return nil
				}
				return stageFile(staging, prefix, dir, p, info, mf)
			})
			if err != nil {
				return err
			}
			continue
		}
		if info, err := os.Stat(src); err == nil && info.Mode().IsRegular() {
			if err := stageFile(staging, prefix, dir, src, info, mf); err != nil {
				return err
			}
		}
	}
	return nil
}

func stageFile(staging, prefix, root, src string, info os.FileInfo, mf *Manifest) error {
	rel, err := filepath.Rel(root, src)
	if err != nil {
		return err
	}
	dest := filepath.Join(staging, prefix, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	mf.Files = append(mf.Files, FileRecord{
		Path:   path.Join(prefix, filepath.ToSlash(rel)),
		Size:   n,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	})
	return nil
}

// apps records the installed catalog releases from Helm.
func (m *Manager) apps() ([]AppRecord, error) {
	out, err := m.cfg.Run(m.cfg.Helm, "", "list", "-A", "-o", "json", "--kubeconfig", m.cfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	var releases []struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		Chart      string `json:"chart"`
		AppVersion string `json:"app_version"`
		Revision   string `json:"revision"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &releases); err != nil {
		return nil, fmt.Errorf("helm list: %w", err)
	}
	var recs []AppRecord
	for _, r := range releases {
		chart, version := splitChart(r.Chart)
		recs = append(recs, AppRecord{
			Name: r.Name, Namespace: r.Namespace, Chart: chart,
			ChartVersion: version, AppVersion: r.AppVersion,
			Revision: r.Revision, Status: r.Status,
		})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	return recs, nil
}

// splitChart turns "gitea-0.1.20" into ("gitea", "0.1.20"). The version is the
// last dash-separated segment.
func splitChart(s string) (string, string) {
	i := strings.LastIndexByte(s, '-')
	if i <= 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func writeArchive(target, staging string) error {
	tmp := target + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.Walk(staging, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		hdr.ModTime = time.Time{}
		hdr.Mode = 0o600
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		fh, err := os.Open(p)
		if err != nil {
			return err
		}
		defer fh.Close()
		_, err = io.Copy(tw, fh)
		return err
	})
	if err != nil {
		tw.Close()
		gz.Close()
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, target)
}

// List returns the archives newest first. A corrupt archive is listed with its
// error instead of hiding it.
func (m *Manager) List() ([]Archive, error) {
	dir, err := m.targetDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Archive{}, nil
		}
		return nil, err
	}
	out := make([]Archive, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !nameRE.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		a := Archive{Name: e.Name(), Size: info.Size(), Created: info.ModTime(), modTime: info.ModTime()}
		mf, err := readManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			a.Error = err.Error()
		} else {
			// Same counting rule as Create/Verify: a failed snapshot apply stays
			// in the manifest but is not a snapshot.
			a = archiveFrom(e.Name(), info.Size(), mf)
			a.modTime = info.ModTime()
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		if !out[i].modTime.Equal(out[j].modTime) {
			return out[i].modTime.After(out[j].modTime)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// readManifest reads the manifest without unpacking the archive. The manifest
// is written first, so a corrupt or truncated tail does not hide it.
func readManifest(file string) (Manifest, error) {
	f, err := os.Open(file)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Manifest{}, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return Manifest{}, fmt.Errorf("manifest.json is missing")
		}
		if err != nil {
			return Manifest{}, err
		}
		if hdr.Name != "manifest.json" {
			continue
		}
		var mf Manifest
		if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&mf); err != nil {
			return Manifest{}, fmt.Errorf("manifest.json: %w", err)
		}
		return mf, nil
	}
}

// Manifest returns the archive's index after validating the name.
func (m *Manager) Manifest(name string) (Manifest, error) {
	if !nameRE.MatchString(name) {
		return Manifest{}, fmt.Errorf("invalid backup name %q", name)
	}
	dir, err := m.targetDir()
	if err != nil {
		return Manifest{}, err
	}
	return readManifest(filepath.Join(dir, name))
}

// Verify recomputes every checksum (the archive and the sidecar copies).
func (m *Manager) Verify(name string) (Archive, error) {
	a, _, err := m.verifyArchive(name, true)
	return a, err
}

// verifyArchive checks the archive's own files. data also hashes the sidecar
// copies; a restore skips that, because the extraction reads them anyway and
// a corrupt copy fails the tar/gzip CRC.
func (m *Manager) verifyArchive(name string, data bool) (Archive, Manifest, error) {
	if !nameRE.MatchString(name) {
		return Archive{}, Manifest{}, fmt.Errorf("invalid backup name %q", name)
	}
	dir, err := m.targetDir()
	if err != nil {
		return Archive{}, Manifest{}, err
	}
	file := filepath.Join(dir, name)
	info, err := os.Stat(file)
	if err != nil {
		return Archive{}, Manifest{}, err
	}
	mf, sums, err := scanArchive(file)
	if err != nil {
		return Archive{Name: name, Size: info.Size(), Error: err.Error()}, Manifest{}, err
	}
	a := archiveFrom(name, info.Size(), mf)
	if mf.Schema > Schema {
		a.Error = fmt.Sprintf("archive schema %d is newer than this console (%d)", mf.Schema, Schema)
		return a, mf, fmt.Errorf("%s", a.Error)
	}
	want := make(map[string]string, len(mf.Files))
	for _, f := range mf.Files {
		want[f.Path] = f.SHA256
	}
	for p, sum := range sums {
		if w, ok := want[p]; !ok || w != sum {
			a.Error = "checksum mismatch: " + p
			return a, mf, fmt.Errorf("%s", a.Error)
		}
	}
	if len(sums) != len(want) {
		a.Error = "the archive is missing files its manifest lists"
		return a, mf, fmt.Errorf("%s", a.Error)
	}
	if data {
		if err := m.verifyData(mf); err != nil {
			a.Error = err.Error()
			return a, mf, err
		}
	}
	a.Verified = true
	return a, mf, nil
}

// safeEntry rejects archive paths that could escape the restore target.
// Verification runs it too: a malicious archive is refused before any file is
// written, not halfway through the extraction.
func safeEntry(name string) bool {
	clean := path.Clean(name)
	if name == "manifest.json" {
		return true
	}
	if clean != name || strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
		return false
	}
	return strings.HasPrefix(clean, "state/") || strings.HasPrefix(clean, "seed/") ||
		strings.HasPrefix(clean, "platform/")
}

// scanArchive returns the manifest and the checksums it actually contains.
func scanArchive(file string) (Manifest, map[string]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return Manifest{}, nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Manifest{}, nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var mf Manifest
	sums := map[string]string{}
	seen := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !safeEntry(hdr.Name) {
			return Manifest{}, nil, fmt.Errorf("archive contains an unsafe path %q", hdr.Name)
		}
		if hdr.Name == "manifest.json" {
			if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&mf); err != nil {
				return Manifest{}, nil, err
			}
			seen = true
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			return Manifest{}, nil, err
		}
		sums[hdr.Name] = hex.EncodeToString(h.Sum(nil))
	}
	if !seen {
		return Manifest{}, nil, fmt.Errorf("manifest.json is missing")
	}
	// tar stops at its end-of-archive blocks, before gzip has verified its own
	// CRC and length. Drain the stream so a flipped byte in the trailer is a
	// verification failure rather than a silent pass.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return Manifest{}, nil, fmt.Errorf("archive is corrupt: %w", err)
	}
	return mf, sums, nil
}

// Restore puts the archived console state and seed back. It writes a safety
// archive of the current state first, so a restore can itself be undone.
func (m *Manager) Restore(name string) (RestoreReport, error) {
	if !nameRE.MatchString(name) {
		return RestoreReport{}, fmt.Errorf("invalid backup name %q", name)
	}
	if _, err := m.Verify(name); err != nil {
		return RestoreReport{}, fmt.Errorf("refusing to restore %s: %w", name, err)
	}
	dir, err := m.targetDir()
	if err != nil {
		return RestoreReport{}, err
	}
	mf, _, err := scanArchive(filepath.Join(dir, name))
	if err != nil {
		return RestoreReport{}, err
	}
	if mf.Schema > Schema {
		return RestoreReport{}, fmt.Errorf("archive schema %d is newer than this console (%d)", mf.Schema, Schema)
	}
	// Hold the archive open across the safety backup: with a small retention
	// count the safety archive itself can prune the one being restored, and the
	// open handle keeps the data readable.
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return RestoreReport{}, err
	}
	defer f.Close()
	// The safety archive is metadata only and does not prune: a restore must
	// not spend hours copying every volume, nor delete the archive it protects.
	safety, err := m.createSelected(context.Background(), "pre-restore of "+name, nil, 0, DataOff)
	if err != nil {
		return RestoreReport{}, fmt.Errorf("safety backup before restore: %w", err)
	}
	rep := RestoreReport{SafetyArchive: safety.Name, Snapshots: mf.Snapshots, Data: mf.Data, Platform: mf.Kind == KindPlatform}
	if err := m.extract(f, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func (m *Manager) extract(f *os.File, rep *RestoreReport) error {
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !safeEntry(hdr.Name) {
			return fmt.Errorf("archive contains an unsafe path %q", hdr.Name)
		}
		clean := path.Clean(hdr.Name)
		switch {
		case strings.HasPrefix(clean, "state/"):
			rel := strings.TrimPrefix(clean, "state/")
			if rel == "" || stateSkipDirs[strings.SplitN(rel, "/", 2)[0]] {
				rep.Skipped = append(rep.Skipped, clean)
				continue
			}
			if err := writeAtomic(filepath.Join(m.cfg.StateDir, filepath.FromSlash(rel)), tr); err != nil {
				return err
			}
			rep.Restored = append(rep.Restored, clean)
		case strings.HasPrefix(clean, "seed/"):
			rel := strings.TrimPrefix(clean, "seed/")
			if !seedRestoreFiles[rel] {
				rep.Skipped = append(rep.Skipped, clean)
				continue
			}
			if err := writeAtomic(filepath.Join(m.cfg.SeedDir, filepath.FromSlash(rel)), tr); err != nil {
				return err
			}
			rep.Restored = append(rep.Restored, clean)
		default:
			rep.Skipped = append(rep.Skipped, clean)
		}
	}
}

// writeAtomic writes a tar entry to path via a temp file in the same directory,
// so a failure mid-restore cannot leave a half-written state file.
func writeAtomic(target string, r io.Reader) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".restore-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Prune keeps the newest n archives and deletes the rest (and their snapshots).
func (m *Manager) Prune(keep int) ([]string, error) { return m.prune(keep, "") }

// prune never deletes protect: a run must not prune the archive it just wrote,
// and a restore must not prune the archive it is reading.
func (m *Manager) prune(keep int, protect string) ([]string, error) {
	if keep < 0 {
		return nil, fmt.Errorf("keep must be >= 0")
	}
	dir, err := m.targetDir()
	if err != nil {
		return nil, err
	}
	list, err := m.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for i := keep; i < len(list); i++ {
		a := list[i]
		if a.Name == protect {
			continue
		}
		// Read the manifest before deleting the archive: it lists the
		// snapshots and data copies that go with it.
		m.deleteSnapshots(a)
		m.deleteData(a)
		if err := os.Remove(filepath.Join(dir, a.Name)); err != nil {
			return removed, err
		}
		removed = append(removed, a.Name)
	}
	return removed, nil
}

// Delete removes one archive, its snapshots and its data copies.
func (m *Manager) Delete(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid backup name %q", name)
	}
	dir, err := m.targetDir()
	if err != nil {
		return err
	}
	list, err := m.List()
	if err != nil {
		return err
	}
	for _, a := range list {
		if a.Name == name {
			m.deleteSnapshots(a)
			m.deleteData(a)
			return os.Remove(filepath.Join(dir, name))
		}
	}
	return fmt.Errorf("no backup named %s", name)
}

// Path returns the archive's path after validating the name.
func (m *Manager) Path(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid backup name %q", name)
	}
	dir, err := m.targetDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

func archiveFrom(name string, size int64, mf Manifest) Archive {
	// Count the snapshots that were actually created: a failed apply is kept in
	// the manifest for the operator, but it is not a snapshot.
	ok := 0
	for _, s := range mf.Snapshots {
		if s.Snapshot != "" && s.Ready && s.Error == "" {
			ok++
		}
	}
	// File copies are counted the same way: a record with an error is a gap.
	data := 0
	var dataBytes int64
	var dataVolumes []DataRecord
	for _, d := range mf.Data {
		if d.File == "" || d.Error != "" {
			continue
		}
		data++
		dataBytes += d.Size
		dataVolumes = append(dataVolumes, d)
	}
	kind := mf.Kind
	if kind == "" {
		kind = KindData
	}
	return Archive{
		Name: name, Size: size, Created: mf.Created, Version: mf.Version,
		Zone: mf.Zone, Note: mf.Note, Kind: kind, Apps: len(mf.Apps), Files: len(mf.Files),
		Snapshots: ok, Data: data, DataBytes: dataBytes, DataVolumes: dataVolumes,
		Partial: mf.Selection != nil, Selection: mf.Selection,
		Warnings: mf.Warnings,
	}
}
