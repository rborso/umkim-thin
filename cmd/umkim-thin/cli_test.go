package main

import (
	"testing"
	"time"
)

func TestPVCFlagParsing(t *testing.T) {
	var f pvcFlags
	for _, v := range []string{"data=fast-ssd:20Gi", "logs=:", "cache=:5Gi", "db=slow"} {
		if err := f.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	if got := f.m["data"]; got.StorageClass != "fast-ssd" || got.Size != "20Gi" {
		t.Errorf("data = %+v", got)
	}
	if got := f.m["cache"]; got.StorageClass != "" || got.Size != "5Gi" {
		t.Errorf("cache = %+v", got)
	}
	if got := f.m["db"]; got.StorageClass != "slow" || got.Size != "" {
		t.Errorf("db = %+v", got)
	}
	if err := f.Set("no-equals-sign"); err == nil {
		t.Error("a PVC override without = was accepted")
	}
}

func TestResourceFlagParsing(t *testing.T) {
	var f resourceFlags
	if err := f.Set("Deployment/gitea/gitea=cpu=200m,memory=512Mi,cpuLimit=2,memoryLimit=2Gi"); err != nil {
		t.Fatal(err)
	}
	got := f.m["Deployment/gitea/gitea"]
	if got.CPURequest != "200m" || got.MemoryRequest != "512Mi" || got.CPULimit != "2" || got.MemoryLimit != "2Gi" {
		t.Fatalf("resources = %+v", got)
	}
	for _, bad := range []string{
		"Deployment/gitea/gitea",           // no spec
		"Deployment/gitea/gitea=cpu",       // no value
		"Deployment/gitea/gitea=disk=10Gi", // unknown field
	} {
		var f resourceFlags
		if err := f.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted an invalid resource", bad)
		}
	}
}

func TestListFlags(t *testing.T) {
	var l listFlags
	for _, v := range []string{"hu", "de"} {
		if err := l.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if got := l.values(); len(got) != 2 || got[0] != "hu" || got[1] != "de" {
		t.Fatalf("values = %v", got)
	}
	var empty listFlags
	if empty.values() != nil {
		t.Fatalf("empty values = %v, want nil", empty.values())
	}
}

func TestHumanInterval(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{24 * time.Hour, "24h"},
		{48 * time.Hour, "48h"},
		{6 * time.Hour, "6h"},
		{90 * time.Minute, "90m"},
		{0, ""},
		{-time.Hour, ""},
	}
	for _, c := range cases {
		if got := humanInterval(c.in); got != c.want {
			t.Errorf("humanInterval(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The CLI must agree with the console about what "an update is available"
// means; both call this comparison.
func TestUpdateAvailableMatchesTheAPI(t *testing.T) {
	if !updateAvailableCLI("0.1.4", "0.1.5") {
		t.Error("a newer catalogued chart was not reported")
	}
	if updateAvailableCLI("0.1.5", "0.1.5") {
		t.Error("the same chart was reported as an update")
	}
	if updateAvailableCLI("0.1.10", "0.1.9") {
		t.Error("an install ahead of the catalog was offered a downgrade")
	}
}
