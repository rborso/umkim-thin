package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduleValidate(t *testing.T) {
	good := Schedule{Enabled: true, Interval: "24h", Keep: 7, Snapshots: true}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid schedule rejected: %v", err)
	}
	off := Schedule{Enabled: false, Interval: "", Keep: 0}
	if err := off.Validate(); err != nil {
		t.Fatalf("a disabled schedule may keep an empty interval: %v", err)
	}
	for _, s := range []Schedule{
		{Enabled: true, Interval: "24h", Keep: -1},
		{Enabled: true, Interval: ""},
		{Enabled: true, Interval: "half a day"},
		{Enabled: true, Interval: "30m"},
		{Enabled: true, Interval: "800h"},
	} {
		if err := s.Validate(); err == nil {
			t.Fatalf("schedule %+v was accepted", s)
		}
	}
}

func TestFormatInterval(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "",
		24 * time.Hour:   "24h",
		90 * time.Minute: "90m",
		36 * time.Hour:   "36h",
		90 * time.Second: "1m30s",
	}
	for in, want := range cases {
		if got := FormatInterval(in); got != want {
			t.Errorf("FormatInterval(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := Schedule{Enabled: true, Interval: "24h"}
	if got := NextRun(Schedule{Enabled: false, Interval: "24h"}, now, now); !got.IsZero() {
		t.Fatalf("a disabled schedule has no next run, got %v", got)
	}
	if got := NextRun(s, time.Time{}, now); !got.Equal(now) {
		t.Fatalf("no archive should be due now, got %v", got)
	}
	if got, want := NextRun(s, now.Add(-time.Hour), now), now.Add(23*time.Hour); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	if got, want := NextRun(s, now.Add(-48*time.Hour), now), now.Add(-24*time.Hour); !got.Equal(want) {
		t.Fatalf("an overdue schedule = %v, want %v", got, want)
	}
}

func TestScheduleStorePersistsAndSignals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup-schedule.json")
	def := Schedule{Enabled: true, Interval: "24h", Keep: 7, Snapshots: true}
	st := NewScheduleStore(path, def)
	if got := st.Get(); got != def {
		t.Fatalf("default = %+v, want %+v", got, def)
	}
	changed := st.Changed()
	want := Schedule{Enabled: false, Interval: "12h", Keep: 3, Snapshots: false, DataMode: DataOff}
	if err := st.Set(want); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("Set did not signal the running loop")
	}
	if got := st.Get(); got != want {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}
	// A new store (a console restart) reads the saved value, not the default.
	if got := NewScheduleStore(path, def).Get(); got != want {
		t.Fatalf("reloaded = %+v, want %+v", got, want)
	}
	// An invalid schedule is refused and leaves the saved one alone.
	if err := st.Set(Schedule{Enabled: true, Interval: "nope"}); err == nil {
		t.Fatal("an invalid interval was accepted")
	}
	if got := st.Get(); got != want {
		t.Fatalf("a rejected Set changed the schedule: %+v", got)
	}
}

func TestScheduleStoreReloadsExternalEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup-schedule.json")
	st := NewScheduleStore(path, Schedule{Enabled: false, Interval: "24h", Keep: 7, Snapshots: true})
	// Simulate the CLI writing the file from another process.
	other := NewScheduleStore(path, Schedule{})
	if err := other.Set(Schedule{Enabled: true, Interval: "6h", Keep: 2, Snapshots: false}); err != nil {
		t.Fatal(err)
	}
	if !st.Reload() {
		t.Fatal("Reload did not notice the external edit")
	}
	if got := st.Get(); got.Interval != "6h" || !got.Enabled || got.Keep != 2 {
		t.Fatalf("after reload = %+v", got)
	}
	if st.Reload() {
		t.Fatal("Reload reported a change twice")
	}
}

func TestScheduleStoreMissingOrCorruptFileUsesDefault(t *testing.T) {
	def := Schedule{Enabled: true, Interval: "24h", Keep: 7, Snapshots: true}
	dir := t.TempDir()
	if got := NewScheduleStore(filepath.Join(dir, "missing.json"), def).Get(); got != def {
		t.Fatalf("missing file = %+v, want the default", got)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := NewScheduleStore(bad, def).Get(); got != def {
		t.Fatalf("corrupt file = %+v, want the default", got)
	}
}

// The console's schedule owns retention and the snapshot switch, so a change
// applies to the next backup without a restart.
func TestCreateUsesTheSchedulePolicy(t *testing.T) {
	m, _, _ := newTestManager(t)
	f := &fakeCluster{
		class: "longhorn-snapshot",
		pvcs:  []string{"gitea/gitea-data"},
	}
	m.cfg.Run = f.run
	m.cfg.Schedule = NewScheduleStore("", Schedule{Enabled: true, Interval: "24h", Keep: 1, Snapshots: false})
	ctx := context.Background()
	if _, err := m.Create(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("kept %d archives, want the schedule's keep=1", len(list))
	}
	if len(f.applied) != 0 {
		t.Fatalf("snapshots were taken although the schedule disabled them: %v", f.applied)
	}
}

func TestTargetsListsMountedDirectories(t *testing.T) {
	m, _, _ := newTestManager(t)
	root := t.TempDir()
	for _, name := range []string{"nfs", "usb"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m.cfg.TargetRoot = root
	targets := m.Targets()
	if len(targets) != 3 {
		t.Fatalf("targets = %+v, want console + two mounts", targets)
	}
	if targets[0].Name != "console" || !targets[0].Default || !targets[0].Writable {
		t.Fatalf("first target = %+v", targets[0])
	}
	if targets[1].Name != "nfs" || targets[2].Name != "usb" {
		t.Fatalf("mounts are not sorted: %+v", targets[1:])
	}
	if !targets[1].Writable || targets[1].FreeBytes == 0 {
		t.Fatalf("mounted target = %+v", targets[1])
	}
}

func TestCreateUsesTheSelectedTarget(t *testing.T) {
	m, _, _ := newTestManager(t)
	root := t.TempDir()
	target := filepath.Join(root, "nfs")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	m.cfg.TargetRoot = root
	m.cfg.Schedule = NewScheduleStore("", Schedule{
		Enabled: true, Interval: "24h", Keep: 7, DataMode: DataOff, Target: target,
	})
	a, err := m.Create(context.Background(), "on-nfs")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, a.Name)); err != nil {
		t.Fatalf("the archive is not on the target: %v", err)
	}
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the console directory is not empty: %v", entries)
	}
	list, err := m.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v; want the target's archive", list, err)
	}
	if got, want := m.Dir(), target; got != want {
		t.Fatalf("Dir = %q, want the target %q", got, want)
	}
}

func TestTargetOutsideTheMountRootIsRejected(t *testing.T) {
	m, _, _ := newTestManager(t)
	root := t.TempDir()
	m.cfg.TargetRoot = root
	m.cfg.Schedule = NewScheduleStore("", Schedule{
		Enabled: true, Interval: "24h", DataMode: DataOff, Target: "/etc",
	})
	if _, err := m.Create(context.Background(), "escape"); err == nil ||
		!strings.Contains(err.Error(), "not mounted under") {
		t.Fatalf("Create = %v, want a target rejection", err)
	}
	if _, err := m.List(); err == nil {
		t.Fatal("List accepted a target outside the mount root")
	}
}

func TestScheduleTargetValidation(t *testing.T) {
	if err := (Schedule{Enabled: true, Interval: "24h", Target: "relative"}).Validate(); err == nil {
		t.Fatal("a relative target was accepted")
	}
	if err := (Schedule{Enabled: true, Interval: "24h", Target: "/backups/nfs"}).Validate(); err != nil {
		t.Fatalf("an absolute target was rejected: %v", err)
	}
}
