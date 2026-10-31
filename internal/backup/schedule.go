package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Data backup modes for a schedule (and a one-off run).
const (
	DataOff      = "off"      // metadata only
	DataSnapshot = "snapshot" // CSI snapshots only; warn when unsupported
	DataAuto     = "auto"     // snapshot where possible, file-copy the rest
	DataCopy     = "copy"     // always file-copy the volume contents
)

// Schedule is the automatic-backup configuration the console edits. It lives
// on the console PVC (state/backup-schedule.json) so a restart keeps it; the
// installer's environment variables are only the initial default.
type Schedule struct {
	Enabled   bool   `json:"enabled"`
	Interval  string `json:"interval"`  // Go duration ("24h"); kept while disabled
	Keep      int    `json:"keep"`      // 0 keeps everything
	Snapshots bool   `json:"snapshots"` // legacy mirror of DataMode
	// DataMode is off, snapshot, auto or copy. Empty means "derive from the
	// legacy Snapshots flag" (true = snapshot).
	DataMode string `json:"dataMode,omitempty"`
	// Target is the archive directory (a Target.Path). Empty means the
	// console's own PVC directory.
	Target string `json:"target,omitempty"`
}

// Mode returns the effective data-backup mode, migrating a schedule written
// before DataMode existed.
func (s Schedule) Mode() string {
	switch s.DataMode {
	case DataOff, DataSnapshot, DataAuto, DataCopy:
		return s.DataMode
	}
	if s.Snapshots {
		return DataSnapshot
	}
	return DataOff
}

// normalized fills DataMode and keeps the legacy Snapshots flag in step.
func (s Schedule) normalized() Schedule {
	s.DataMode = s.Mode()
	s.Snapshots = s.DataMode != DataOff
	return s
}

// Validate rejects a schedule the loop could not run. The interval only
// matters when the schedule is enabled, so a disabled schedule may keep the
// last interval for a later re-enable.
func (s Schedule) Validate() error {
	if s.Keep < 0 {
		return fmt.Errorf("keep must be zero or more")
	}
	switch s.DataMode {
	case "", DataOff, DataSnapshot, DataAuto, DataCopy:
	default:
		return fmt.Errorf("unknown data mode %q (use off, snapshot, auto or copy)", s.DataMode)
	}
	if s.Target != "" {
		if !filepath.IsAbs(s.Target) || filepath.Clean(s.Target) != s.Target {
			return fmt.Errorf("invalid backup target %q", s.Target)
		}
	}
	if !s.Enabled {
		return nil
	}
	d, err := s.Every()
	if err != nil {
		return err
	}
	if d < time.Hour {
		return fmt.Errorf("the interval must be at least 1h")
	}
	if d > 30*24*time.Hour {
		return fmt.Errorf("the interval must be at most 720h")
	}
	return nil
}

// Every parses the interval.
func (s Schedule) Every() (time.Duration, error) {
	raw := strings.TrimSpace(s.Interval)
	if raw == "" {
		return 0, fmt.Errorf("an interval is required (for example 24h)")
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q (use a duration such as 12h or 24h)", raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("the interval must be positive")
	}
	return d, nil
}

// FormatInterval renders a duration as the shortest hour/minute form, so a
// default read from the environment shows as "24h" instead of "24h0m0s".
func FormatInterval(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// NextRun is when the next automatic backup is due: the newest archive plus the
// interval, or now when there is no archive yet (enabling the schedule takes
// one promptly) or that time is already past. A disabled or invalid schedule
// returns the zero time.
func NextRun(s Schedule, newest, now time.Time) time.Time {
	if !s.Enabled {
		return time.Time{}
	}
	d, err := s.Every()
	if err != nil {
		return time.Time{}
	}
	if newest.IsZero() {
		return now
	}
	return newest.Add(d)
}

// ScheduleStore persists the schedule and wakes the running loop when it
// changes. It is safe for concurrent use: the API writes it, the loop reads it.
type ScheduleStore struct {
	mu   sync.Mutex
	path string
	s    Schedule
	ch   chan struct{}
}

// NewScheduleStore loads the schedule from path, falling back to def when the
// file is missing or invalid. An empty path keeps the store in memory.
func NewScheduleStore(path string, def Schedule) *ScheduleStore {
	st := &ScheduleStore{path: path, s: def, ch: make(chan struct{})}
	if path == "" {
		return st
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	var s Schedule
	if err := json.Unmarshal(raw, &s); err != nil {
		return st
	}
	if err := s.Validate(); err != nil {
		return st
	}
	st.s = s.normalized()
	return st
}

// Get returns the current schedule.
func (st *ScheduleStore) Get() Schedule {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

// Set validates, saves and signals the loop. A failed save leaves the previous
// schedule in place.
func (st *ScheduleStore) Set(s Schedule) error {
	s.Interval = strings.TrimSpace(s.Interval)
	if err := s.Validate(); err != nil {
		return err
	}
	s = s.normalized()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.path != "" {
		raw, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
			return err
		}
		tmp := st.path + ".tmp"
		if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, st.path); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	st.s = s
	close(st.ch)
	st.ch = make(chan struct{})
	return nil
}

// Changed returns a channel that is closed when the schedule next changes.
func (st *ScheduleStore) Changed() <-chan struct{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.ch
}

// Reload re-reads the file and reports whether it differs from the in-memory
// schedule. It lets a CLI edit (a separate process) take effect without a
// console restart.
func (st *ScheduleStore) Reload() bool {
	if st.path == "" {
		return false
	}
	raw, err := os.ReadFile(st.path)
	if err != nil {
		return false
	}
	var s Schedule
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	if err := s.Validate(); err != nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if s == st.s {
		return false
	}
	st.s = s
	close(st.ch)
	st.ch = make(chan struct{})
	return true
}

// Policy returns the retention and snapshot switches every backup uses, manual
// or scheduled. The console screen owns them, so a change applies immediately.
func (st *ScheduleStore) Policy() (keep int, mode string) {
	s := st.Get()
	return s.Keep, s.Mode()
}

// RunSchedule takes automatic backups until ctx is cancelled. It waits from the
// newest archive, so a restart or a manual backup does not duplicate a run, and
// re-evaluates whenever the schedule changes. report may be nil.
func (m *Manager) RunSchedule(ctx context.Context, st *ScheduleStore, report func(Archive, error)) {
	notify := func(a Archive, err error) {
		if report != nil {
			report(a, err)
		}
	}
	for {
		s := st.Get()
		if !s.Enabled {
			select {
			case <-ctx.Done():
				return
			case <-st.Changed():
			}
			continue
		}
		now := m.cfg.Now()
		var newest time.Time
		if list, err := m.List(); err == nil && len(list) > 0 {
			newest = list[0].Created
		}
		next := NextRun(s, newest, now)
		if next.IsZero() {
			// An unparseable interval: wait for a change instead of spinning.
			select {
			case <-ctx.Done():
				return
			case <-st.Changed():
			}
			continue
		}
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		// Wake at least once a minute: a CLI edit to the schedule file is a
		// different process, so the loop polls it instead of relying only on
		// the in-process change channel.
		const poll = time.Minute
		polling := wait > poll
		if polling {
			wait = poll
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-st.Changed():
			timer.Stop()
		case <-timer.C:
			if st.Reload() {
				continue
			}
			if polling && time.Now().Before(next) {
				continue
			}
			a, err := m.Create(ctx, "scheduled")
			notify(a, err)
		}
	}
}
