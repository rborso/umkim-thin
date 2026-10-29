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

// Verify is what "tested" means for this entry.
type Verify struct {
	Playwright string   `yaml:"playwright"`
	Checks     []string `yaml:"checks"`
}

// Entry is one catalog file.
type Entry struct {
	Name        string         `yaml:"name"`
	DisplayName string         `yaml:"displayName,omitempty"`
	Description string         `yaml:"description,omitempty"`
	Chart       ChartRef       `yaml:"chart"`
	Namespace   string         `yaml:"namespace"`
	Route       *Route         `yaml:"route,omitempty"`
	Routes      []Route        `yaml:"routes,omitempty"`
	Auth        Auth           `yaml:"auth"`
	License     *License       `yaml:"license"`
	Links       *Links         `yaml:"links"`
	OIDC        *OIDC          `yaml:"oidc,omitempty"`
	Exposure    map[string]any `yaml:"exposure,omitempty"`
	Values      map[string]any `yaml:"values,omitempty"`
	Verify      Verify         `yaml:"verify"`
}

// ChartRef pins the chart and where it comes from. Charts live in their own
// public repositories; the catalog pins the version, not the location.
type ChartRef struct {
	Ref     string `yaml:"ref"`
	Version string `yaml:"version"`
	// Versions is the tested set; Version is the stable default. A missing
	// list is a single-version catalog entry (backward compatible).
	Versions []ChartVersion `yaml:"versions,omitempty"`
}

// ChartVersion is one published chart/app pair.
type ChartVersion struct {
	Version    string `yaml:"version"`
	AppVersion string `yaml:"appVersion,omitempty"`
	Channel    string `yaml:"channel,omitempty"` // stable | candidate
	Notes      string `yaml:"notes,omitempty"`
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
	for _, v := range e.Chart.Versions {
		if v.Version == "" {
			return fmt.Errorf("%s: every chart version needs a version", e.Name)
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
	if e.Verify.Playwright == "" || len(e.Verify.Checks) == 0 {
		return fmt.Errorf("%s: an entry without a verification suite is not publishable", e.Name)
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
