// Package catalog loads and validates the app catalog.
//
// An entry is everything needed to install an app, expose it, protect it and
// verify it. The format is the contract between umkim-thin and umkim: an entry
// means the same thing in both products. See docs/catalog.md.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rborso/umkim-thin/internal/update"
	"gopkg.in/yaml.v3"
)

// Route is one published hostname for an app.
type Route struct {
	Key         string   `yaml:"key,omitempty"`
	Mode        string   `yaml:"mode"` // site (the only published mode)
	Host        string   `yaml:"host,omitempty"`
	VPNHost     string   `yaml:"vpnHost,omitempty"`
	RequireAuth bool     `yaml:"requireAuth"`
	ServiceName string   `yaml:"serviceName,omitempty"`
	ServicePort int      `yaml:"servicePort,omitempty"`
	PublicPaths []string `yaml:"publicPaths,omitempty"`
	// Exposure is the install-time policy. It defaults from RequireAuth for
	// older entries: true = authelia, false = public. `vpn` is never implicit.
	Exposure *ExposurePolicy `yaml:"exposure,omitempty"`
	// Certificate is the install-time certificate policy. The default is ACME.
	Certificate *CertificatePolicy `yaml:"certificate,omitempty"`
	// Trusted opts this route into the management/VPN bypass paths. The public
	// route is unchanged; the bypass paths still require the app's own auth.
	Trusted *TrustedAccess `yaml:"trusted,omitempty"`
	// AppAuth describes the authentication the application itself offers on a
	// trusted route. This is deliberately separate from Auth.Kind, which is the
	// portal (Authelia) policy: an app can use forward-auth on the public host
	// and still have LDAP, OIDC, local or token auth behind a bypass.
	AppAuth *AppAuth `yaml:"appAuth,omitempty"`
}

// TrustedAccess says which non-public networks may reach the route without
// Authelia forward-auth. The app's own authentication still applies.
type TrustedAccess struct {
	Management bool `yaml:"management,omitempty"`
	VPN        bool `yaml:"vpn,omitempty"`
	// SameHost defaults to true for trusted routes: the VPN and management
	// paths use the public hostname. Set it to false only for compatibility
	// with the older <host>.vpn.<zone> layout.
	SameHost *bool `yaml:"sameHost,omitempty"`
}

// AppAuth is the app-level authentication available on a trusted route. It is
// required for trusted routes because removing Authelia must not leave an
// unauthenticated application.
type AppAuth struct {
	Kind string `yaml:"kind"` // ldap | oidc | local | token
	Note string `yaml:"note,omitempty"`
}

// TrustedManagement reports whether the route requests the management bypass.
func (r Route) TrustedManagement() bool {
	return r.Trusted != nil && r.Trusted.Management
}

// TrustedVPN reports whether the route requests the VPN same-host path.
func (r Route) TrustedVPN() bool {
	return r.Trusted != nil && r.Trusted.VPN
}

// TrustedSameHost reports whether a trusted VPN route uses the public hostname.
// Trusted routes default to same-host; the old .vpn.<zone> layout is opt-out.
func (r Route) TrustedSameHost() bool {
	if (r.Trusted == nil) || r.Trusted.SameHost == nil {
		return true
	}
	return *r.Trusted.SameHost
}

// ExposurePolicy says which network boundaries an app may be installed behind.
type ExposurePolicy struct {
	Default    string   `yaml:"default,omitempty"`
	Allowed    []string `yaml:"allowed,omitempty"`
	AllowLocal bool     `yaml:"allowLocal,omitempty"`
}

// CertificatePolicy says how the route may obtain its certificate.
type CertificatePolicy struct {
	Default string   `yaml:"default,omitempty"`
	Allowed []string `yaml:"allowed,omitempty"`
}

// Auth describes how the app is protected.
type Auth struct {
	Kind string `yaml:"kind"` // forward_auth | oidc | none
	LDAP bool   `yaml:"ldap,omitempty"`
}

// License records both the chart licence and the licence of the application
// the chart deploys. They are deliberately separate: the platform's charts are
// Apache-2.0, while the upstream application may be AGPL, GPL, source-available
// or something else.
type License struct {
	// Chart is the licence of the Helm chart itself.
	Chart string `yaml:"chart" json:"Chart"`
	// Application is the licence of the deployed application. Use a short
	// expression for a mixed stack, e.g. "Mixed: MIT, GPL-2.0, GPL-3.0".
	Application string `yaml:"application" json:"Application"`
	// URL points at the authoritative licence text or notice.
	URL string `yaml:"url,omitempty" json:"URL,omitempty"`
	// Note carries a short warning, such as a non-commercial restriction.
	Note string `yaml:"note,omitempty" json:"Note,omitempty"`
}

// Links points at the upstream application. Homepage is the official project
// site; Source is the upstream source repository. At least one is required so
// the console can always offer the operator a way back to the original app.
type Links struct {
	Homepage string `yaml:"homepage,omitempty" json:"Homepage,omitempty"`
	Source   string `yaml:"source,omitempty" json:"Source,omitempty"`
}

// OIDC is present when the app authenticates against Authelia itself.
type OIDC struct {
	ClientID     string   `yaml:"clientId"`
	RedirectURIs []string `yaml:"redirectUris"`
	RequirePKCE  *bool    `yaml:"requirePkce,omitempty"`
	// Scopes requested by the app. Defaults to openid profile email; add
	// "groups" when the app maps directory groups onto its own roles.
	Scopes []string `yaml:"scopes,omitempty"`
}

// Test categories. The console and docs show one of these per app, and it is
// derived from the entry (Category) rather than declared: a category is a
// statement about evidence, so it must come from the evidence.
const (
	// CategoryAutoTested: every declared check resolves to a gating Playwright
	// test, so a regression fails CI without a human involved.
	CategoryAutoTested = "auto-tested"
	// CategoryAutoChecked: a suite exists but it is informational (excluded from
	// the gate), so a failure is reported rather than failed. This is deliberately
	// distinct from auto-tested; conflating them was the old behaviour, where
	// verify.playwright always pointed at journeys/.
	CategoryAutoChecked = "auto-checked"
	// CategoryVerified: a person used the app and logged what they did. This is
	// independent of the automatic tiers: an app can be both.
	CategoryVerified = "verified"
	// CategoryUntested: installable, but nothing is claimed.
	CategoryUntested = "untested"
)

// GatingSuites are the Playwright files whose failures fail CI. A check tag found
// only outside these is informational (see CategoryAutoChecked).
var GatingSuites = []string{
	"apps.spec.ts", "ai.spec.ts", "auth.spec.ts", "tls.spec.ts", "console.spec.ts",
	"home.spec.ts", "lib.spec.ts", "journeys/tenant.spec.ts",
}

// Category derives the app's test status. auto says whether its checks resolved
// to gating tests, informational says whether they resolved to any test at all;
// the caller (ci/static.sh via `catalog verify`) does the resolving, because that
// needs the test tree.
func (e Entry) Category(auto, informational bool) string {
	switch {
	case auto:
		return CategoryAutoTested
	case informational:
		return CategoryAutoChecked
	case e.Verify.Human != nil:
		return CategoryVerified
	default:
		return CategoryUntested
	}
}

// VerificationStale reports whether the human log predates the pinned chart
// version. It does not drop the claim: it is shown as "verified on <old version>"
// so a reader knows the check happened before the current release. Rewriting the
// log to the new version without re-testing is what this is for.
func (e Entry) VerificationStale() bool {
	h := e.Verify.Human
	return h != nil && h.Version != "" && h.Version != e.Chart.Version
}

// The category is derived, never typed in (see Entry.Category): an entry is
// "auto-tested" when it declares checks and they all resolve to gating Playwright
// tests, and "verified" when a human log exists for it. Declaring a label by hand
// is what let the old `checks` list drift - four entries named checks that appear
// in no test at all.
type Verify struct {
	// Playwright is the suite that exercises this app. A suite under
	// tests/playwright/journeys/ is informational; the gating suites are named
	// in GatingSuites.
	Playwright string `yaml:"playwright"`
	// Harness names a non-Playwright check when the app has no browser surface
	// (the WireGuard gateway is verified by ci/vpn-check.sh). Its value is the
	// script, so a reader can run it themselves.
	Harness string `yaml:"harness,omitempty"`
	// Checks are the acceptance checks that must exist as `@check:<name>` tags in
	// the Playwright suites. ci/static.sh resolves every one of them, so a check
	// that is only a comment cannot be declared.
	Checks []string `yaml:"checks"`
	// Human logs the last time a person actually used the app, and on which chart
	// version. It does not expire the badge (that would require re-verifying every
	// bump); it is reported as "verified on <old version>" when the chart moved.
	Human *HumanVerification `yaml:"human,omitempty"`
}

// HumanVerification records a manual pass. Pinned to a chart version so a later
// bump is visible as stale rather than silently inheriting the claim.
type HumanVerification struct {
	// Date is the day the pass finished, as YYYY-MM-DD.
	Date string `yaml:"date"`
	// Version is chart.version that was exercised.
	Version string `yaml:"version"`
	// By is who did it (a handle, not an email - this file is public).
	By string `yaml:"by"`
	// Target names where it was exercised: "single", "ha", or both.
	Targets []string `yaml:"targets"`
	// Did is what the person actually did, in their words. Keep it to steps a
	// user would recognise; "logged in" alone is not a verification.
	Did []string `yaml:"did"`
}

// Entry is one catalog file.
type Entry struct {
	Name        string   `yaml:"name"`
	DisplayName string   `yaml:"displayName,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Chart       ChartRef `yaml:"chart"`
	Namespace   string   `yaml:"namespace"`
	Route       *Route   `yaml:"route,omitempty"`
	Routes      []Route  `yaml:"routes,omitempty"`
	// Home optionally splits one installed app into several user-facing home
	// page entries. Each entry points at one of this app's route keys.
	Home     []HomeLink     `yaml:"home,omitempty"`
	Auth     Auth           `yaml:"auth"`
	License  *License       `yaml:"license"`
	Links    *Links         `yaml:"links"`
	OIDC     *OIDC          `yaml:"oidc,omitempty"`
	Exposure map[string]any `yaml:"exposure,omitempty"`
	Values   map[string]any `yaml:"values,omitempty"`
	Verify   Verify         `yaml:"verify"`
	// Origin is set by the loader, never by the YAML: "bundled" for an entry that
	// shipped with the platform, "external" for one an operator added. The console
	// uses it to mark private apps, and so that an unverified custom app is not
	// mistaken for a tested one.
	Origin string `yaml:"-" json:"origin,omitempty"`
	// Override lets a custom entry replace a bundled one of the same name on
	// purpose. Without it a collision is refused, because silently shadowing a
	// platform entry is how an operator ends up running a chart they did not
	// choose.
	Override bool `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

// OriginBundled and OriginExternal are the values Entry.Origin can hold.
const (
	OriginBundled  = "bundled"
	OriginExternal = "external"
)

// IsExternal reports whether this entry came from an operator's own catalog
// rather than the platform's. A private app is not worse, it is just not the
// platform's to guarantee.
func (e Entry) IsExternal() bool { return e.Origin == OriginExternal }

// HomeLink is one user-facing entry on the household home page. It lets a
// multi-route app (the Servarr stack) present its movie player and request UI
// as separate cards instead of one ambiguous app link.
type HomeLink struct {
	Route       string `yaml:"route"`
	DisplayName string `yaml:"displayName"`
	Description string `yaml:"description,omitempty"`
}

// ChartRef pins the chart and where it comes from. Charts live in their own
// public repositories; the catalog pins the version, not the location.
type ChartRef struct {
	Ref     string `yaml:"ref"`
	Version string `yaml:"version"`
	// Versions is the tested set; Version is the stable default. A missing
	// list is a single-version catalog entry (backward compatible).
	Versions []ChartVersion `yaml:"versions,omitempty"`
	// MinimumUmkimThinVersion is the oldest platform this chart version
	// supports. Per-version values override it. A dev build is allowed with a
	// warning; a release build is refused.
	MinimumUmkimThinVersion string `yaml:"minimumUmkimThinVersion,omitempty"`
}

// ChartVersion is one published chart/app pair.
type ChartVersion struct {
	Version                 string `yaml:"version"`
	AppVersion              string `yaml:"appVersion,omitempty"`
	Channel                 string `yaml:"channel,omitempty"` // stable | candidate
	Notes                   string `yaml:"notes,omitempty"`
	MinimumUmkimThinVersion string `yaml:"minimumUmkimThinVersion,omitempty"`
}

// EffectiveRoutes returns the routes to publish, whether the entry used the
// single `route` form or the `routes` list. An omitted mode means `site`: a
// route that is listed is meant to be published, and the multi-route form
// should not have to repeat it.
func (e Entry) EffectiveRoutes() []Route {
	var out []Route
	if len(e.Routes) > 0 {
		out = append(out, e.Routes...)
	} else if e.Route != nil {
		out = append(out, *e.Route)
	}
	for i := range out {
		if out[i].Mode == "" {
			out[i].Mode = "site"
		}
	}
	return out
}

// Validate reports the first problem that would make an entry unpublishable.
func (e Entry) Validate() error {
	switch {
	case e.Name == "":
		return fmt.Errorf("name is required")
	case e.Chart.Ref == "":
		return fmt.Errorf("%s: chart.ref is required", e.Name)
	case e.Chart.Version == "":
		return fmt.Errorf("%s: chart.version must be pinned", e.Name)
	case e.Namespace == "":
		return fmt.Errorf("%s: namespace is required", e.Name)
	case e.Auth.Kind == "":
		return fmt.Errorf("%s: auth.kind is required", e.Name)
	}
	if e.Chart.MinimumUmkimThinVersion != "" && !update.IsVersion(e.Chart.MinimumUmkimThinVersion) {
		return fmt.Errorf("%s: invalid chart.minimumUmkimThinVersion %q", e.Name, e.Chart.MinimumUmkimThinVersion)
	}
	for _, v := range e.Chart.Versions {
		if v.Version == "" {
			return fmt.Errorf("%s: every chart version needs a version", e.Name)
		}
		if v.MinimumUmkimThinVersion != "" && !update.IsVersion(v.MinimumUmkimThinVersion) {
			return fmt.Errorf("%s: chart %s has invalid minimumUmkimThinVersion %q", e.Name, v.Version, v.MinimumUmkimThinVersion)
		}
		switch v.Channel {
		case "", "stable", "candidate":
		default:
			return fmt.Errorf("%s: chart %s has unknown channel %q", e.Name, v.Version, v.Channel)
		}
	}
	if len(e.Chart.Versions) > 0 {
		found := false
		seen := map[string]bool{}
		for _, v := range e.Chart.Versions {
			if seen[v.Version] {
				return fmt.Errorf("%s: duplicate chart version %s", e.Name, v.Version)
			}
			seen[v.Version] = true
			if v.Version == e.Chart.Version {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: chart.version %s is not in chart.versions", e.Name, e.Chart.Version)
		}
	}
	if e.License == nil {
		return fmt.Errorf("%s: license is required (chart and application)", e.Name)
	}
	if e.License.Chart == "" {
		return fmt.Errorf("%s: license.chart is required", e.Name)
	}
	if e.License.Application == "" {
		return fmt.Errorf("%s: license.application is required", e.Name)
	}
	if e.Links == nil || (e.Links.Homepage == "" && e.Links.Source == "") {
		return fmt.Errorf("%s: links.homepage or links.source is required", e.Name)
	}
	for field, value := range map[string]string{"links.homepage": e.Links.Homepage, "links.source": e.Links.Source} {
		if value == "" {
			continue
		}
		if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
			return fmt.Errorf("%s: %s must be an http(s) URL", e.Name, field)
		}
	}
	switch e.Auth.Kind {
	case "forward_auth", "oidc", "none":
	default:
		return fmt.Errorf("%s: unknown auth.kind %q", e.Name, e.Auth.Kind)
	}
	if e.Auth.Kind == "oidc" && (e.OIDC == nil || e.OIDC.ClientID == "") {
		return fmt.Errorf("%s: auth.kind oidc needs an oidc.clientId", e.Name)
	}
	for _, r := range e.EffectiveRoutes() {
		switch r.Mode {
		case "site":
			if r.Host == "" {
				return fmt.Errorf("%s: a site route needs a host", e.Name)
			}
		default:
			return fmt.Errorf("%s: unknown route mode %q (use exposure vpn for private routes)", e.Name, r.Mode)
		}
		if err := validateExposure(r); err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
		if err := validateCertificate(r); err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
		if err := validateTrusted(r); err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
		// Public paths are exempted from auth, so they must be anchored:
		// an unanchored pattern silently exempts more than intended.
		for _, p := range r.PublicPaths {
			if !strings.HasPrefix(p, "^") {
				return fmt.Errorf("%s: public path %q must be anchored with ^", e.Name, p)
			}
		}
	}
	if len(e.Home) > 0 {
		routes := e.EffectiveRoutes()
		keys := make(map[string]bool, len(routes))
		for _, r := range routes {
			keys[r.Key] = true
		}
		seen := make(map[string]bool, len(e.Home))
		for _, h := range e.Home {
			if h.DisplayName == "" {
				return fmt.Errorf("%s: a home entry needs displayName", e.Name)
			}
			if h.Route == "" && len(routes) != 1 {
				return fmt.Errorf("%s: a multi-route home entry needs a route key", e.Name)
			}
			if !keys[h.Route] {
				return fmt.Errorf("%s: home entry references unknown route %q", e.Name, h.Route)
			}
			if seen[h.Route] {
				return fmt.Errorf("%s: duplicate home entry for route %q", e.Name, h.Route)
			}
			seen[h.Route] = true
		}
	}
	// An entry must declare how it is checked: a Playwright suite, or a harness
	// script for an app with no browser surface (the WireGuard gateway). Claiming
	// nothing is not publishable, and claiming a suite that does not exist is
	// caught by ci/static.sh (see `catalog verify`).
	if e.Verify.Playwright == "" && e.Verify.Harness == "" {
		return fmt.Errorf("%s: an entry without a verification suite is not publishable", e.Name)
	}
	if len(e.Verify.Checks) == 0 {
		return fmt.Errorf("%s: verify.checks is empty (an entry without checks is not publishable)", e.Name)
	}
	// A manual log must be specific enough to be useful: a date, the chart
	// version it covers, and at least one thing the person actually did. "Logged
	// in" alone is not a verification, so a log with no steps is rejected.
	if h := e.Verify.Human; h != nil {
		if _, err := time.Parse("2006-01-02", h.Date); err != nil {
			return fmt.Errorf("%s: verify.human.date must be YYYY-MM-DD", e.Name)
		}
		if h.Version == "" {
			return fmt.Errorf("%s: verify.human.version is required (the chart version tested)", e.Name)
		}
		if h.By == "" {
			return fmt.Errorf("%s: verify.human.by is required", e.Name)
		}
		if len(h.Did) == 0 {
			return fmt.Errorf("%s: verify.human.did needs at least one step", e.Name)
		}
		for _, t := range h.Targets {
			switch t {
			case "single", "ha":
			default:
				return fmt.Errorf("%s: verify.human.targets has unknown target %q", e.Name, t)
			}
		}
	}
	return nil
}

var validExposures = map[string]bool{"public": true, "authelia": true, "vpn": true}

var validAppAuthKinds = map[string]bool{
	"ldap":  true,
	"oidc":  true,
	"local": true,
	"token": true,
}

func validateTrusted(r Route) error {
	if r.AppAuth != nil {
		switch r.AppAuth.Kind {
		case "":
			return fmt.Errorf("route %q: appAuth.kind is required when appAuth is set", r.Key)
		default:
			if !validAppAuthKinds[r.AppAuth.Kind] {
				return fmt.Errorf("route %q: unknown appAuth.kind %q", r.Key, r.AppAuth.Kind)
			}
		}
	}
	if !r.TrustedManagement() && !r.TrustedVPN() {
		return nil
	}
	if r.Mode != "site" {
		return fmt.Errorf("route %q: trusted access is only valid for a site route", r.Key)
	}
	if r.AppAuth == nil || r.AppAuth.Kind == "" {
		return fmt.Errorf("route %q: trusted access needs appAuth.kind (ldap, oidc, local or token)", r.Key)
	}
	// Public means unauthenticated at the platform edge. Trusted access is a
	// deliberate bypass of Authelia, so it is only compatible with the
	// authenticated public route (or a VPN-only route where there is no public
	// host at all).
	if r.DefaultExposure() == "public" {
		return fmt.Errorf("route %q: trusted access requires authelia or vpn exposure, not public", r.Key)
	}
	return nil
}

func validateExposure(r Route) error {
	p := r.Exposure
	if p == nil {
		return nil // derived from RequireAuth
	}
	if p.Default != "" && !validExposures[p.Default] {
		return fmt.Errorf("unknown exposure default %q", p.Default)
	}
	allowed := p.Allowed
	if len(allowed) == 0 && p.Default != "" {
		allowed = []string{p.Default}
	}
	for _, v := range allowed {
		if !validExposures[v] {
			return fmt.Errorf("unknown exposure %q", v)
		}
	}
	if p.Default != "" && len(allowed) > 0 {
		found := false
		for _, v := range allowed {
			if v == p.Default {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("exposure default %q is not in allowed", p.Default)
		}
	}
	if p.Default == "vpn" && r.VPNHost == "" {
		// The same host may be resolved through split DNS; don't require a
		// separate VPN hostname.
	}
	return nil
}

func validateCertificate(r Route) error {
	p := r.Certificate
	if p == nil {
		return nil
	}
	if p.Default != "" && !validCertificateValue(p.Default, false) {
		return fmt.Errorf("unknown certificate default %q", p.Default)
	}
	for _, v := range p.Allowed {
		if !validCertificateValue(v, true) {
			return fmt.Errorf("unknown certificate mode %q", v)
		}
	}
	if p.Default != "" && len(p.Allowed) > 0 {
		found := false
		for _, v := range p.Allowed {
			if p.Default == v || (v == "secret:*" && strings.HasPrefix(p.Default, "secret:")) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("certificate default %q is not in allowed", p.Default)
		}
	}
	return nil
}

func validCertificateValue(v string, allowWildcard bool) bool {
	switch v {
	case "acme", "default":
		return true
	case "secret:*":
		return allowWildcard
	}
	return strings.HasPrefix(v, "secret:") && len(v) > len("secret:")
}

// DefaultExposure is the catalog default, with the old requireAuth mapping.
func (r Route) DefaultExposure() string {
	if r.Exposure != nil && r.Exposure.Default != "" {
		return r.Exposure.Default
	}
	if r.RequireAuth {
		return "authelia"
	}
	return "public"
}

// AllowedExposures is the install-time choice set. Public is opt-in for a
// route that was already behind Authelia; VPN is always more restrictive.
func (r Route) AllowedExposures() []string {
	if r.Exposure != nil && len(r.Exposure.Allowed) > 0 {
		return append([]string(nil), r.Exposure.Allowed...)
	}
	if r.RequireAuth {
		return []string{"authelia", "vpn"}
	}
	return []string{"public", "authelia", "vpn"}
}

// DefaultCertificate is the catalog default certificate mode.
func (r Route) DefaultCertificate() string {
	if r.Certificate != nil && r.Certificate.Default != "" {
		return r.Certificate.Default
	}
	return "acme"
}

// AllowedCertificates is the install-time certificate choice set.
func (r Route) AllowedCertificates() []string {
	if r.Certificate != nil && len(r.Certificate.Allowed) > 0 {
		return append([]string(nil), r.Certificate.Allowed...)
	}
	return []string{"acme", "default", "secret"}
}

// AllowsLocal reports whether the catalog permits a VPN route to also serve
// the site LAN. The install can only narrow it, never silently widen it.
func (r Route) AllowsLocal() bool {
	return r.Exposure != nil && r.Exposure.AllowLocal
}

// Load reads every *.yaml in dir, validates it, and returns the entries sorted
// by name.
func Load(dir string) ([]Entry, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no catalog entries in %s", dir)
	}
	var out []Entry
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var e Entry
		if err := yaml.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if e.Name == "" {
			e.Name = strings.TrimSuffix(filepath.Base(p), ".yaml")
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
