package backup

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// targetNameRE is the only target name shape AddTarget accepts. It keeps the
// name a single, safe directory below TargetRoot.
var targetNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Target is one place backups can be written: the console PVC (the default) or
// a directory the chart mounted under TargetRoot.
type Target struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Default marks the console's own archive directory.
	Default bool `json:"default,omitempty"`
	// Writable is a best-effort access check; a false value usually means the
	// mount is missing or read-only.
	Writable   bool  `json:"writable"`
	FreeBytes  int64 `json:"freeBytes,omitempty"`
	TotalBytes int64 `json:"totalBytes,omitempty"`
}

// Targets lists the archive directory and every mounted target. The chart
// mounts each configured target at <TargetRoot>/<name>; the filesystem is the
// registry, so adding one needs no console configuration.
func (m *Manager) Targets() []Target {
	out := make([]Target, 0, 4)
	// The default directory may not exist before the first backup; create it so
	// the screen shows it as usable.
	_ = os.MkdirAll(m.cfg.Dir, 0o755)
	out = append(out, m.target(m.cfg.Dir, "console", true))
	root := strings.TrimSpace(m.cfg.TargetRoot)
	if root == "" {
		return out
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	var extra []Target
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		extra = append(extra, m.target(filepath.Join(root, e.Name()), e.Name(), false))
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Name < extra[j].Name })
	return append(out, extra...)
}

// AddTarget creates a directory below TargetRoot and proves it is writable by
// creating, writing and syncing a probe file before the target is offered to
// the schedule. TargetRoot is a mounted backup root (a host path on a single
// node or a shared NFS/Samba/RWX mount on HA); the console never chooses an
// arbitrary host path.
func (m *Manager) AddTarget(name string) (Target, error) {
	name = strings.TrimSpace(name)
	if !targetNameRE.MatchString(name) || len(name) > 63 {
		return Target{}, fmt.Errorf("invalid backup target name %q (use lowercase letters, digits and dashes)", name)
	}
	if name == "console" {
		return Target{}, fmt.Errorf("the console target already exists")
	}
	root := filepath.Clean(strings.TrimSpace(m.cfg.TargetRoot))
	if root == "" || root == "." {
		return Target{}, fmt.Errorf("no backup root is configured")
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return Target{}, fmt.Errorf("backup root %s is not mounted; configure a backupRoot hostPath or shared NFS/Samba mount", root)
	}
	target := filepath.Join(root, name)
	if filepath.Dir(target) != root {
		return Target{}, fmt.Errorf("invalid backup target %q", name)
	}
	created := false
	if fi, err := os.Lstat(target); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return Target{}, fmt.Errorf("backup target %s must not be a symlink", target)
		}
		if !fi.IsDir() {
			return Target{}, fmt.Errorf("backup target %s exists and is not a directory", target)
		}
	} else if !os.IsNotExist(err) {
		return Target{}, fmt.Errorf("checking backup target %s: %w", target, err)
	} else {
		if err := os.Mkdir(target, 0o755); err != nil {
			return Target{}, fmt.Errorf("creating backup target %s: %w", target, err)
		}
		created = true
	}
	removeOnError := func() {
		if created {
			_ = os.Remove(target)
		}
	}
	probe, err := os.CreateTemp(target, ".umkim-thin-write-*")
	if err != nil {
		removeOnError()
		return Target{}, fmt.Errorf("backup target %s is not writable: %w", target, err)
	}
	probeName := probe.Name()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		probe.Close()
		os.Remove(probeName)
		removeOnError()
		return Target{}, fmt.Errorf("backup target write probe: %w", err)
	}
	if _, err := probe.Write(buf); err != nil {
		probe.Close()
		os.Remove(probeName)
		removeOnError()
		return Target{}, fmt.Errorf("backup target %s is not writable: %w", target, err)
	}
	if err := probe.Sync(); err != nil {
		probe.Close()
		os.Remove(probeName)
		removeOnError()
		return Target{}, fmt.Errorf("backup target %s failed the write probe: %w", target, err)
	}
	if err := probe.Close(); err != nil {
		os.Remove(probeName)
		removeOnError()
		return Target{}, fmt.Errorf("backup target %s failed the write probe: %w", target, err)
	}
	if err := os.Remove(probeName); err != nil {
		removeOnError()
		return Target{}, fmt.Errorf("backup target %s failed the write probe: %w", target, err)
	}
	t := m.target(target, name, false)
	if !t.Writable {
		removeOnError()
		return Target{}, fmt.Errorf("backup target %s is not writable by the console", target)
	}
	return t, nil
}

func (m *Manager) target(path, name string, def bool) Target {
	t := Target{Name: name, Path: path, Default: def}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return t
	}
	t.Writable = syscall.Access(path, 2) == nil // W_OK
	if avail, total, err := statfs(path); err == nil {
		t.FreeBytes, t.TotalBytes = avail, total
	}
	return t
}

// displayDir is the configured target even when it is missing, so the UI can
// show what the schedule points at instead of silently pretending otherwise.
func (m *Manager) displayDir() string {
	if m.cfg.Schedule != nil {
		if t := strings.TrimSpace(m.cfg.Schedule.Get().Target); t != "" {
			return filepath.Clean(t)
		}
	}
	return m.cfg.Dir
}

// targetDir validates the selected target and returns the directory to use.
// A schedule that points at a target the chart no longer mounts is an error,
// not a silent fallback: writing a backup somewhere else is worse.
func (m *Manager) targetDir() (string, error) {
	if m.cfg.Schedule == nil {
		return m.cfg.Dir, nil
	}
	t := strings.TrimSpace(m.cfg.Schedule.Get().Target)
	if t == "" {
		return m.cfg.Dir, nil
	}
	t = filepath.Clean(t)
	if t != filepath.Clean(m.cfg.Dir) {
		root := filepath.Clean(m.cfg.TargetRoot)
		if root == "" || filepath.Dir(t) != root {
			return "", fmt.Errorf("backup target %q is not mounted under %s", t, root)
		}
	}
	if fi, err := os.Stat(t); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("backup target %q is not available (is the mount still there?)", t)
	}
	return t, nil
}
