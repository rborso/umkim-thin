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

// nameRE is the only archive name shape the API and CLI accept. It rules out
// path separators before any path is built from user input.
var nameRE = regexp.MustCompile(`^umkim-thin-[0-9]{8}T[0-9]{6}Z(-[0-9]+)?\.tar\.gz$`)

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
	// Now is injectable for tests.
	Now func() time.Time
	// Run executes a tool (with optional stdin). Injectable for tests.
	Run func(name, stdin string, args ...string) (string, error)
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
	Error     string `json:"error,omitempty"`
}

// Manifest is the archive's index. It is the first entry in the tar.
type Manifest struct {
	Schema    int              `json:"schema"`
	Created   time.Time        `json:"created"`
	Version   string           `json:"version,omitempty"`
	Zone      string           `json:"zone,omitempty"`
	Note      string           `json:"note,omitempty"`
	Apps      []AppRecord      `json:"apps,omitempty"`
	Snapshots []SnapshotRecord `json:"snapshots,omitempty"`
	Files     []FileRecord     `json:"files"`
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
	Apps      int       `json:"apps"`
	Files     int       `json:"files"`
	Snapshots int       `json:"snapshots"`
	Warnings  []string  `json:"warnings,omitempty"`
	// Verified is set by Verify: the archive matches its manifest.
	Verified bool   `json:"verified"`
	Error    string `json:"error,omitempty"`

	// modTime orders archives written in the same second (the safety archive a
	// restore writes is newer than the one it protects).
	modTime time.Time
}

// RestoreReport says what a restore did.
type RestoreReport struct {
	SafetyArchive string           `json:"safetyArchive,omitempty"`
	Restored      []string         `json:"restored"`
	Skipped       []string         `json:"skipped,omitempty"`
	Snapshots     []SnapshotRecord `json:"snapshots,omitempty"`
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
	if cfg.Run == nil {
		cfg.Run = func(name, stdin string, args ...string) (string, error) {
			return runTool(cfg.Kubeconfig, name, stdin, args...)
		}
	}
	return &Manager{cfg: cfg}
}

// Dir is the archive directory.
func (m *Manager) Dir() string { return m.cfg.Dir }

// Create builds one archive and prunes older ones.
func (m *Manager) Create(ctx context.Context, note string) (Archive, error) {
	if err := os.MkdirAll(m.cfg.Dir, 0o755); err != nil {
		return Archive{}, fmt.Errorf("backup directory: %w", err)
	}
	now := m.cfg.Now().UTC().Truncate(time.Second)
	staging, err := os.MkdirTemp(m.cfg.Dir, ".staging-")
	if err != nil {
		return Archive{}, err
	}
	defer os.RemoveAll(staging)

	mf := Manifest{
		Schema:  Schema,
		Created: now,
		Version: m.cfg.Version,
		Zone:    m.cfg.Zone,
		Note:    note,
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
	mf.Apps = m.apps()

	if m.cfg.Snapshots {
		recs, warn := m.snapshotPVCs(ctx, now)
		mf.Snapshots = recs
		if warn != "" {
			mf.Warnings = append(mf.Warnings, warn)
		}
	}

	name := archiveName(now)
	if _, err := os.Stat(filepath.Join(m.cfg.Dir, name)); err == nil {
		for i := 2; ; i++ {
			candidate := fmt.Sprintf("umkim-thin-%s-%d.tar.gz", now.Format("20060102T150405Z"), i)
			if _, err := os.Stat(filepath.Join(m.cfg.Dir, candidate)); err != nil {
				name = candidate
				break
			}
		}
	}

	raw, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return Archive{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), raw, 0o600); err != nil {
		return Archive{}, err
	}
	target := filepath.Join(m.cfg.Dir, name)
	if err := writeArchive(target, staging); err != nil {
		os.Remove(target)
		return Archive{}, err
	}
	if m.cfg.Keep > 0 {
		if _, err := m.prune(m.cfg.Keep, name); err != nil {
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

func archiveName(t time.Time) string {
	return "umkim-thin-" + t.Format("20060102T150405Z") + ".tar.gz"
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
		if skip[e.Name()] {
			continue
		}
		src := filepath.Join(dir, e.Name())
		if e.IsDir() {
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
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if err := stageFile(staging, prefix, dir, src, info, mf); err != nil {
			return err
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

// apps records the installed catalog releases from Helm. A cluster that is
// unreachable (or a test) simply yields no app inventory.
func (m *Manager) apps() []AppRecord {
	out, err := m.cfg.Run(m.cfg.Helm, "", "list", "-A", "-o", "json", "--kubeconfig", m.cfg.Kubeconfig)
	if err != nil {
		return nil
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
		return nil
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
	return recs
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
	entries, err := os.ReadDir(m.cfg.Dir)
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
		mf, err := readManifest(filepath.Join(m.cfg.Dir, e.Name()))
		if err != nil {
			a.Error = err.Error()
		} else {
			a.Created = mf.Created
			a.Version = mf.Version
			a.Zone = mf.Zone
			a.Note = mf.Note
			a.Apps = len(mf.Apps)
			a.Files = len(mf.Files)
			a.Snapshots = len(mf.Snapshots)
			a.Warnings = mf.Warnings
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
	return readManifest(filepath.Join(m.cfg.Dir, name))
}

// Verify recomputes every checksum and compares it with the manifest.
func (m *Manager) Verify(name string) (Archive, error) {
	if !nameRE.MatchString(name) {
		return Archive{}, fmt.Errorf("invalid backup name %q", name)
	}
	file := filepath.Join(m.cfg.Dir, name)
	info, err := os.Stat(file)
	if err != nil {
		return Archive{}, err
	}
	mf, sums, err := scanArchive(file)
	if err != nil {
		return Archive{Name: name, Size: info.Size(), Error: err.Error()}, err
	}
	a := archiveFrom(name, info.Size(), mf)
	if mf.Schema > Schema {
		a.Error = fmt.Sprintf("archive schema %d is newer than this console (%d)", mf.Schema, Schema)
		return a, fmt.Errorf("%s", a.Error)
	}
	want := make(map[string]string, len(mf.Files))
	for _, f := range mf.Files {
		want[f.Path] = f.SHA256
	}
	for p, sum := range sums {
		if w, ok := want[p]; !ok || w != sum {
			a.Error = "checksum mismatch: " + p
			return a, fmt.Errorf("%s", a.Error)
		}
	}
	if len(sums) != len(want) {
		a.Error = "the archive is missing files its manifest lists"
		return a, fmt.Errorf("%s", a.Error)
	}
	a.Verified = true
	return a, nil
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
	return strings.HasPrefix(clean, "state/") || strings.HasPrefix(clean, "seed/")
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
	mf, _, err := scanArchive(filepath.Join(m.cfg.Dir, name))
	if err != nil {
		return RestoreReport{}, err
	}
	if mf.Schema > Schema {
		return RestoreReport{}, fmt.Errorf("archive schema %d is newer than this console (%d)", mf.Schema, Schema)
	}
	// Hold the archive open across the safety backup: with a small retention
	// count the safety archive itself can prune the one being restored, and the
	// open handle keeps the data readable.
	f, err := os.Open(filepath.Join(m.cfg.Dir, name))
	if err != nil {
		return RestoreReport{}, err
	}
	defer f.Close()
	safety, err := m.Create(context.Background(), "pre-restore of "+name)
	if err != nil {
		return RestoreReport{}, fmt.Errorf("safety backup before restore: %w", err)
	}
	rep := RestoreReport{SafetyArchive: safety.Name, Snapshots: mf.Snapshots}
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
		if err := os.Remove(filepath.Join(m.cfg.Dir, a.Name)); err != nil {
			return removed, err
		}
		removed = append(removed, a.Name)
		m.deleteSnapshots(a)
	}
	return removed, nil
}

// Delete removes one archive and its snapshots.
func (m *Manager) Delete(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid backup name %q", name)
	}
	list, err := m.List()
	if err != nil {
		return err
	}
	for _, a := range list {
		if a.Name == name {
			m.deleteSnapshots(a)
			return os.Remove(filepath.Join(m.cfg.Dir, name))
		}
	}
	return fmt.Errorf("no backup named %s", name)
}

// Path returns the archive's path after validating the name.
func (m *Manager) Path(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("invalid backup name %q", name)
	}
	p := filepath.Join(m.cfg.Dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

func archiveFrom(name string, size int64, mf Manifest) Archive {
	return Archive{
		Name: name, Size: size, Created: mf.Created, Version: mf.Version,
		Zone: mf.Zone, Note: mf.Note, Apps: len(mf.Apps), Files: len(mf.Files),
		Snapshots: len(mf.Snapshots), Warnings: mf.Warnings,
	}
}
