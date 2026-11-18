package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// The catalog lists the chart history for rollback, but only a packaged version
// can be installed. Advertising an unpackaged one makes the console offer it and
// Helm then fetches the chart ref (a GitHub tree URL), failing with
// "does not appear to be a gzipped archive" -- the exact error a user sees when
// picking an older version.
func TestVersionsOnlyAdvertisesPackagedCharts(t *testing.T) {
	dir := t.TempDir()
	// Only 0.1.22 and 0.1.23 exist in the mirror.
	for _, v := range []string{"0.1.22", "0.1.23"} {
		if err := os.WriteFile(filepath.Join(dir, "gitea-"+v+".tgz"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in := New(Config{ChartMirror: dir, Helm: "/nonexistent-helm"})
	e := catalog.Entry{
		Name:      "gitea",
		Namespace: "gitea",
		Chart: catalog.ChartRef{
			Ref:     "https://github.com/rborso/umkim-charts/tree/main/gitea",
			Version: "0.1.23",
			Versions: []catalog.ChartVersion{
				{Version: "0.1.23", AppVersion: "28.4.0", Channel: "stable"},
				{Version: "0.1.22", AppVersion: "28.4.0", Channel: "stable"},
				{Version: "0.1.21", AppVersion: "28.3.0", Channel: "stable"},
				{Version: "0.1.20", AppVersion: "28.1.0", Channel: "stable"},
			},
		},
	}
	vi, err := in.Versions(e)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range vi.Versions {
		got[v.Version] = true
	}
	for _, want := range []string{"0.1.23", "0.1.22"} {
		if !got[want] {
			t.Errorf("%s is packaged but not advertised", want)
		}
	}
	for _, unwanted := range []string{"0.1.21", "0.1.20"} {
		if got[unwanted] {
			t.Errorf("%s is advertised but has no package; selecting it fails", unwanted)
		}
	}
	if vi.Versions[0].Version != "0.1.23" {
		t.Errorf("the stable pointer must be first, got %s", vi.Versions[0].Version)
	}
}

// With no mirror configured (a standalone/custom setup) the catalog is the
// source and nothing is filtered.
func TestVersionsWithoutMirrorKeepsEverything(t *testing.T) {
	in := New(Config{Helm: "/nonexistent-helm"})
	e := catalog.Entry{
		Name:      "gitea",
		Namespace: "gitea",
		Chart: catalog.ChartRef{
			Version:  "0.1.23",
			Versions: []catalog.ChartVersion{{Version: "0.1.23"}, {Version: "0.1.19"}},
		},
	}
	vi, err := in.Versions(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(vi.Versions) != 2 {
		t.Fatalf("got %d versions, want both", len(vi.Versions))
	}
}
