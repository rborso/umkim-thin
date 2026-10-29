package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validEntry() Entry {
	return Entry{
		Name:      "nextcloud",
		Chart:     ChartRef{Ref: "oci://example.com/charts/nextcloud", Version: "0.1.0"},
		Namespace: "nextcloud",
		Route: &Route{
			Mode:        "site",
			Host:        "nextcloud.{{ .zone }}",
			RequireAuth: true,
			ServiceName: "nextcloud",
			ServicePort: 80,
		},
		Auth: Auth{Kind: "forward_auth", LDAP: true},
		License: &License{
			Chart:       "Apache-2.0",
			Application: "AGPL-3.0",
			URL:         "https://example.com/license",
		},
		Links: &Links{
			Homepage: "https://example.com",
			Source:   "https://github.com/example/app",
		},
		Verify: Verify{
			Playwright: "tests/playwright/journeys/nextcloud.spec.ts",
			Checks:     []string{"route_reachable"},
		},
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Entry)
		wantErr string
	}{
		{name: "valid", mutate: func(*Entry) {}},
		{name: "missing name", mutate: func(e *Entry) { e.Name = "" }, wantErr: "name is required"},
		{name: "missing chart ref", mutate: func(e *Entry) { e.Chart.Ref = "" }, wantErr: "chart.ref is required"},
		{name: "floating chart version", mutate: func(e *Entry) { e.Chart.Version = "" }, wantErr: "chart.version must be pinned"},
		{name: "missing namespace", mutate: func(e *Entry) { e.Namespace = "" }, wantErr: "namespace is required"},
		{name: "missing links", mutate: func(e *Entry) { e.Links = nil }, wantErr: "links.homepage or links.source is required"},
		{name: "invalid homepage link", mutate: func(e *Entry) { e.Links.Homepage = "javascript:alert(1)" }, wantErr: "must be an http(s) URL"},
		{name: "missing license", mutate: func(e *Entry) { e.License = nil }, wantErr: "license is required"},
		{name: "missing chart license", mutate: func(e *Entry) { e.License.Chart = "" }, wantErr: "license.chart is required"},
		{name: "missing application license", mutate: func(e *Entry) { e.License.Application = "" }, wantErr: "license.application is required"},
		{name: "missing auth", mutate: func(e *Entry) { e.Auth.Kind = "" }, wantErr: "auth.kind is required"},
		{name: "unknown auth", mutate: func(e *Entry) { e.Auth.Kind = "magic" }, wantErr: "unknown auth.kind"},
		{name: "oidc without client", mutate: func(e *Entry) { e.Auth.Kind = "oidc" }, wantErr: "needs an oidc.clientId"},
		{name: "site route without host", mutate: func(e *Entry) { e.Route.Host = "" }, wantErr: "a site route needs a host"},
		{name: "unknown route mode", mutate: func(e *Entry) { e.Route.Mode = "public" }, wantErr: "unknown route mode"},
		{
			name: "unanchored public path",
			mutate: func(e *Entry) {
				e.Route.PublicPaths = []string{"/s/"}
			},
			wantErr: "must be anchored",
		},
		{
			name: "no verification suite",
			mutate: func(e *Entry) {
				e.Verify = Verify{}
			},
			wantErr: "without a verification suite",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := validEntry()
			c.mutate(&e)
			err := e.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

func TestOIDCClientIDRequired(t *testing.T) {
	e := validEntry()
	e.Auth.Kind = "oidc"
	e.OIDC = &OIDC{ClientID: "nextcloud", RedirectURIs: []string{"https://nextcloud.example.com/callback"}}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid OIDC entry rejected: %v", err)
	}
	e.OIDC.ClientID = ""
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "clientId") {
		t.Fatalf("empty client id accepted: %v", err)
	}
}

func TestEffectiveRoutes(t *testing.T) {
	single := validEntry()
	routes := single.EffectiveRoutes()
	if len(routes) != 1 || routes[0].Mode != "site" {
		t.Fatalf("single route = %+v", routes)
	}

	multi := validEntry()
	multi.Route = nil
	multi.Routes = []Route{
		{Key: "web", Host: "web.{{ .zone }}"},
		{Key: "api", Mode: "site", Host: "api.{{ .zone }}"},
		{Key: "admin", Mode: "site", Host: "admin.{{ .zone }}"},
	}
	routes = multi.EffectiveRoutes()
	if len(routes) != 3 {
		t.Fatalf("got %d routes, want 3", len(routes))
	}
	if routes[0].Mode != "site" {
		t.Fatalf("an omitted mode should mean site, got %q", routes[0].Mode)
	}
	if routes[1].Mode != "site" || routes[2].Mode != "site" {
		t.Fatalf("explicit modes changed: %+v", routes)
	}
	if routes := (Entry{}).EffectiveRoutes(); len(routes) != 0 {
		t.Fatalf("an entry without routes returned %+v", routes)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("zeta.yaml", `
chart: {ref: oci://example.com/charts/zeta, version: 1.0.0}
namespace: zeta
route: {mode: site, host: "zeta.{{ .zone }}"}
auth: {kind: none}
license: {chart: Apache-2.0, application: MIT}
links: {homepage: https://example.com, source: https://github.com/example/app}
verify: {playwright: tests/zeta.spec.ts, checks: [route_reachable]}
`)
	write("alpha.yaml", `
name: alpha
chart: {ref: oci://example.com/charts/alpha, version: 1.0.0}
namespace: alpha
route: {mode: site, host: "alpha.{{ .zone }}"}
auth: {kind: none}
license: {chart: Apache-2.0, application: MIT}
links: {homepage: https://example.com, source: https://github.com/example/app}
verify: {playwright: tests/alpha.spec.ts, checks: [route_reachable]}
`)

	entries, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Name != "alpha" || entries[1].Name != "zeta" {
		t.Fatalf("entries are not sorted by name: %q, %q", entries[0].Name, entries[1].Name)
	}
	if entries[1].Name != "zeta" {
		t.Fatalf("a nameless file should use its filename, got %q", entries[1].Name)
	}
}

func TestLoadRejectsInvalidEntry(t *testing.T) {
	dir := t.TempDir()
	body := `
name: broken
chart: {ref: oci://example.com/charts/broken}
namespace: broken
auth: {kind: none}
license: {chart: Apache-2.0, application: MIT}
links: {homepage: https://example.com, source: https://github.com/example/app}
verify: {playwright: tests/broken.spec.ts, checks: [route_reachable]}
`
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "chart.version") {
		t.Fatalf("Load() = %v, want a pinned-version error", err)
	}
}

func TestLoadEmptyDirectory(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load() accepted an empty directory")
	}
}

func TestChartVersions(t *testing.T) {
	e := validEntry()
	e.Chart.Versions = []ChartVersion{
		{Version: "0.1.0", AppVersion: "1.0.0", Channel: "stable"},
		{Version: "0.1.1", AppVersion: "1.1.0", Channel: "candidate"},
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid version list rejected: %v", err)
	}
	e.Chart.Version = "0.2.0"
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "not in chart.versions") {
		t.Fatalf("default version outside the list = %v", err)
	}
	e = validEntry()
	e.Chart.Versions = []ChartVersion{{Version: "0.1.0", Channel: "edge"}}
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "unknown channel") {
		t.Fatalf("unknown channel = %v", err)
	}
}

func TestExposurePolicy(t *testing.T) {
	e := validEntry()
	e.Route.Exposure = &ExposurePolicy{Default: "authelia", Allowed: []string{"authelia", "vpn"}, AllowLocal: true}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid exposure policy rejected: %v", err)
	}
	if got := e.Route.DefaultExposure(); got != "authelia" {
		t.Fatalf("default exposure = %q", got)
	}
	if !e.Route.AllowsLocal() {
		t.Fatal("allowLocal not reported")
	}
	e.Route.Exposure.Default = "public"
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "not in allowed") {
		t.Fatalf("default outside allowed = %v", err)
	}
	e = validEntry()
	e.Route.Exposure = &ExposurePolicy{Default: "magic"}
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "unknown exposure") {
		t.Fatalf("unknown exposure = %v", err)
	}
}

func TestCertificatePolicy(t *testing.T) {
	e := validEntry()
	e.Route.Certificate = &CertificatePolicy{Default: "default", Allowed: []string{"acme", "default", "secret:*"}}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid certificate policy rejected: %v", err)
	}
	e.Route.Certificate.Default = "secret:wildcard"
	if err := e.Validate(); err != nil {
		t.Fatalf("secret default rejected: %v", err)
	}
	e.Route.Certificate.Default = "bogus"
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "unknown certificate") {
		t.Fatalf("unknown certificate = %v", err)
	}
}

func TestTrustedAccessRequiresAppAuth(t *testing.T) {
	e := validEntry()
	e.Route.Trusted = &TrustedAccess{Management: true, VPN: true}
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "appAuth.kind") {
		t.Fatalf("trusted route without appAuth = %v", err)
	}
	e.Route.AppAuth = &AppAuth{Kind: "none"}
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "unknown appAuth.kind") {
		t.Fatalf("appAuth none = %v", err)
	}
	e.Route.AppAuth = &AppAuth{Kind: "ldap"}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid trusted route = %v", err)
	}
}

func TestTrustedAccessRejectsPublicDefault(t *testing.T) {
	e := validEntry()
	e.Route.RequireAuth = false
	e.Route.Exposure = nil
	e.Route.Trusted = &TrustedAccess{VPN: true}
	e.Route.AppAuth = &AppAuth{Kind: "local"}
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("trusted public route = %v", err)
	}
}

func TestTrustedSameHost(t *testing.T) {
	r := Route{}
	if !r.TrustedSameHost() {
		t.Fatal("trusted routes should default to same host")
	}
	no := false
	r.Trusted = &TrustedAccess{SameHost: &no}
	if r.TrustedSameHost() {
		t.Fatal("sameHost false was ignored")
	}
	yes := true
	r.Trusted.SameHost = &yes
	if !r.TrustedSameHost() {
		t.Fatal("sameHost true was ignored")
	}
}

func TestLegacyExposureDefaults(t *testing.T) {
	e := validEntry()
	if got := e.Route.DefaultExposure(); got != "authelia" {
		t.Fatalf("requireAuth true default = %q", got)
	}
	for _, v := range e.Route.AllowedExposures() {
		if v == "public" {
			t.Fatal("public must be opt-in for an authenticated route")
		}
	}
	e.Route.RequireAuth = false
	if got := e.Route.DefaultExposure(); got != "public" {
		t.Fatalf("public default = %q", got)
	}
}
