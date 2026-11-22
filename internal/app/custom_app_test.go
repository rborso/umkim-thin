package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// A custom app is one that an operator added, not one the platform ships. These
// tests pin the two things that make it usable: it JOINS the catalog, and the
// console can tell it apart from a tested entry.

// customEntry is a minimal but complete private entry: no verification suite,
// because a private app is not the platform's to test.
const customEntry = `name: sillytavern
displayName: SillyTavern
description: Private chat frontend with local tool servers.
chart:
  ref: oci://example.invalid/charts/sillytavern
  version: 0.1.0
namespace: sillytavern
route:
  mode: site
  host: "silly.{{ .zone }}"
  requireAuth: true
  exposure:
    default: vpn
    allowed: [vpn]
  serviceName: sillytavern
  servicePort: 8000
auth:
  kind: none
license:
  chart: MIT
  application: AGPL-3.0
links:
  source: https://github.com/SillyTavern/SillyTavern
exposure:
  default: vpn
verify:
  harness: tests/private/sillytavern.sh
  checks: [route_reachable]
`

// The bundled catalog and a private bundle must merge. This is the "add a custom
// app" contract: the platform's fourteen apps stay, and one more appears.
func TestCatalogMergesABundledCatalogWithACustomApp(t *testing.T) {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "bundled")
	custom := filepath.Join(dir, "custom")
	for _, d := range []string{bundled, custom} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Two "official" entries, so the merge has something to preserve.
	for _, name := range []string{"gitea", "immich"} {
		body := strings.Replace(customEntry, "name: sillytavern", "name: "+name, 1)
		body = strings.Replace(body, "sillystaven", name, 1)
		if err := os.WriteFile(filepath.Join(bundled, name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(custom, "sillytavern.yaml"), []byte(customEntry), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := catalog.NewStore(bundled, custom)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, e := range store.Entries() {
		names[e.Name] = true
	}
	for _, want := range []string{"gitea", "immich", "sillytavern"} {
		if !names[want] {
			t.Errorf("%q is missing from the merged catalog (have %v)", want, names)
		}
	}

	// The private app is identifiable, which is what the console badge needs.
	e, ok := store.Entry("sillytavern")
	if !ok {
		t.Fatal("the custom entry is missing")
	}
	if !e.IsExternal() {
		t.Errorf("origin = %q, want %q", e.Origin, catalog.OriginExternal)
	}
	// ...and the bundled ones are not.
	if b, _ := store.Entry("gitea"); b.IsExternal() {
		t.Errorf("a bundled entry was marked external: %q", b.Name)
	}

	// It is installed exactly like any other app: the exposure policy the entry
	// asks for is what the installer will use.
	routes := e.EffectiveRoutes()
	if len(routes) == 0 {
		t.Fatal("the custom entry has no routes")
	}
	if got := routes[0].DefaultExposure(); got != "vpn" {
		t.Errorf("default exposure = %q, want vpn: a private app must not be published", got)
	}
}

// An entry without a verification suite is refused, which is what stops a custom
// app being added with no way to check it. The private path uses a harness rather
// than a Playwright suite the platform does not have.
func TestCustomEntryStillNeedsAVerificationHook(t *testing.T) {
	e := catalog.Entry{
		Name:      "sillytavern",
		Namespace: "sillytavern",
		Chart:     catalog.ChartRef{Ref: "oci://example.invalid/x", Version: "0.1.0"},
		Auth:      catalog.Auth{Kind: "none"},
		License:   &catalog.License{Chart: "MIT", Application: "AGPL-3.0"},
		Links:     &catalog.Links{Source: "https://example.invalid"},
	}
	err := e.Validate()
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("an entry with no verification hook was accepted: %v", err)
	}

	// With a harness it validates, so a private app is expressible.
	e.Verify = catalog.Verify{Harness: "tests/private/sillytavern.sh", Checks: []string{"route_reachable"}}
	if err := e.Validate(); err != nil {
		t.Fatalf("a private entry with a harness was refused: %v", err)
	}
}
