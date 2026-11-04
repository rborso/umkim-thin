package app

import (
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
)

func TestCheckMinimumVersion(t *testing.T) {
	entry := catalog.Entry{
		Name:  "demo",
		Chart: catalog.ChartRef{Version: "0.1.0", MinimumUmkimThinVersion: "0.3.4"},
	}
	if err := New(Config{Version: "0.3.3"}).checkMinimumVersion(entry); err == nil ||
		!strings.Contains(err.Error(), "requires umkim-thin 0.3.4") {
		t.Fatalf("old platform = %v, want a compatibility refusal", err)
	}
	if err := New(Config{Version: "0.3.4"}).checkMinimumVersion(entry); err != nil {
		t.Fatalf("matching platform was refused: %v", err)
	}
	if err := New(Config{Version: "dev+abc"}).checkMinimumVersion(entry); err != nil {
		t.Fatalf("a dev build should be allowed: %v", err)
	}
	entry.Chart.Versions = []catalog.ChartVersion{
		{Version: "0.1.0", MinimumUmkimThinVersion: "0.4.0"},
	}
	if err := New(Config{Version: "0.3.4"}).checkMinimumVersion(entry); err == nil {
		t.Fatal("a per-version minimum did not override the chart minimum")
	}
}
