package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

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
