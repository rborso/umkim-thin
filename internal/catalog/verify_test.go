package catalog

import (
	"strings"
	"testing"
)

// The category is a statement about evidence, so it must be derived. Before this,
// the entry carried a free-text checks list and no category at all, and four
// entries named checks that appeared in no test.
func TestCategoryIsDerivedFromEvidence(t *testing.T) {
	base := validEntry()
	base.Chart.Version = "1.2.3"

	for _, tc := range []struct {
		name          string
		auto          bool
		informational bool
		human         *HumanVerification
		want          string
	}{
		{name: "gating tests", auto: true, informational: true, want: CategoryAutoTested},
		{name: "informational only", informational: true, want: CategoryAutoChecked},
		{name: "human only", human: &HumanVerification{Version: "1.2.3"}, want: CategoryVerified},
		{name: "nothing", want: CategoryUntested},
		// A gating suite outranks a human log: the machine proof is stronger.
		{name: "both", auto: true, informational: true,
			human: &HumanVerification{Version: "1.2.3"}, want: CategoryAutoTested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			e.Verify.Human = tc.human
			if got := e.Category(tc.auto, tc.informational); got != tc.want {
				t.Fatalf("Category(%v, %v) = %q, want %q", tc.auto, tc.informational, got, tc.want)
			}
		})
	}
}

// A human pass is pinned to the chart version it covered. When the chart moves the
// claim is reported as stale rather than silently inherited or dropped, which was
// the chosen behaviour for this field.
func TestVerificationStaleTracksTheChartVersion(t *testing.T) {
	e := validEntry()
	e.Chart.Version = "0.2.0"
	e.Verify.Human = &HumanVerification{Date: "2026-11-19", Version: "0.1.9", By: "x", Did: []string{"did a thing"}}
	if !e.VerificationStale() {
		t.Fatal("a log for 0.1.9 against chart 0.2.0 must be stale")
	}
	e.Verify.Human.Version = "0.2.0"
	if e.VerificationStale() {
		t.Fatal("a log for the current chart version must not be stale")
	}
	e.Verify.Human = nil
	if e.VerificationStale() {
		t.Fatal("no log cannot be stale")
	}
}

// A manual log must say what was done. "Logged in" alone is not a verification, so
// an empty did list is rejected at load time instead of producing a hollow badge.
func TestHumanVerificationValidation(t *testing.T) {
	cases := []struct {
		name    string
		human   *HumanVerification
		wantErr string
	}{
		{
			name:  "complete",
			human: &HumanVerification{Date: "2026-11-19", Version: "1.2.3", By: "x", Targets: []string{"single", "ha"}, Did: []string{"created a repo"}},
		},
		{name: "no date", human: &HumanVerification{Version: "1.2.3", By: "x", Did: []string{"a"}}, wantErr: "date must be YYYY-MM-DD"},
		{name: "bad date", human: &HumanVerification{Date: "19-11-2026", Version: "1.2.3", By: "x", Did: []string{"a"}}, wantErr: "date must be YYYY-MM-DD"},
		{name: "no version", human: &HumanVerification{Date: "2026-11-19", By: "x", Did: []string{"a"}}, wantErr: "version is required"},
		{name: "no author", human: &HumanVerification{Date: "2026-11-19", Version: "1.2.3", Did: []string{"a"}}, wantErr: "by is required"},
		{name: "no steps", human: &HumanVerification{Date: "2026-11-19", Version: "1.2.3", By: "x"}, wantErr: "did needs at least one step"},
		{
			name:    "unknown target",
			human:   &HumanVerification{Date: "2026-11-19", Version: "1.2.3", By: "x", Targets: []string{"laptop"}, Did: []string{"a"}},
			wantErr: "unknown target",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := validEntry()
			e.Verify.Human = c.human
			err := e.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want %q", err, c.wantErr)
			}
		})
	}
}

// An app with no browser surface declares a harness instead of a Playwright suite.
// Requiring playwright==="" would have made the WireGuard gateway unpublishable.
func TestHarnessReplacesPlaywrightButNotChecks(t *testing.T) {
	e := validEntry()
	e.Verify.Playwright = ""
	e.Verify.Harness = "ci/vpn-check.sh"
	if err := e.Validate(); err != nil {
		t.Fatalf("a harness-only entry must validate, got %v", err)
	}
	e.Verify.Checks = nil
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "checks is empty") {
		t.Fatalf("an entry with no checks must be rejected, got %v", err)
	}
}
