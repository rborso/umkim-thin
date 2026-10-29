// Package route renders the Kubernetes objects and the Authelia policy for one
// app route.
//
// Auth is a property of the route, not of the app: the Ingress carries the
// forwardAuth middleware, and the paths that must stay reachable without a
// session are exempted in Authelia's own access control, where the tenant and
// group rules already live.
package route

import (
	"fmt"
	"strings"

	"github.com/rborso/umkim-thin/internal/catalog"
)

const (
	// The Traefik file-provider middleware the identity chart publishes.
	authMiddleware = "identity-authelia@file"
)

// Context is what an entry is rendered against.
type Context struct {
	Zone         string
	Release      string
	Tenant       string // the directory group that may reach authed routes
	Middleware   string // forward-auth middleware; empty uses the identity default
	Entrypoint   string // Traefik entrypoint; empty uses websecure
	CertResolver string // ACME resolver; empty uses letsencrypt

	// Exposure is the install-time choice for this route: public, authelia or
	// vpn. Empty derives it from the catalog (requireAuth).
	Exposure   string
	AllowLocal bool
	// Certificate is acme, default or secret:<name>.
	Certificate string
	CertSecret  string // resolved name for secret:<name>
	// VPNEntrypoint is the internal Traefik entrypoint (default "vpn").
	VPNEntrypoint string
	// VPNOnlyMiddleware / VPNLocalMiddleware are the Traefik file-provider
	// allowlists for VPN-only and VPN+LAN routes.
	VPNOnlyMiddleware  string
	VPNLocalMiddleware string

	// ManagementCIDRs are the source ranges that may use the management
	// bypass. They are used by ClientIP and are never taken from a request
	// header. ExcludeCIDRs removes infrastructure addresses that appear in
	// the range only because a public proxy masquerades to them.
	ManagementCIDRs        []string
	ManagementExcludeCIDRs []string
	// VPNSubnet is the tunnel network used by the same-host trusted VPN
	// route. It is matched with ClientIP on websecure; the gateway DNATs the
	// VPN's :443 to that entrypoint and preserves the peer source.
	VPNSubnet string
	// NameSuffix distinguishes an additional rendered object (for example the
	// same-host VPN Ingress) from the primary route. Empty is the primary.
	NameSuffix string
}

func (c Context) middleware() string {
	if c.Middleware != "" {
		return c.Middleware
	}
	return authMiddleware
}

func (c Context) entrypoint() string {
	if c.Entrypoint != "" {
		return c.Entrypoint
	}
	return "websecure"
}

func (c Context) certResolver() string {
	if c.CertResolver != "" {
		return c.CertResolver
	}
	return "letsencrypt"
}

func (c Context) vpnEntrypoint() string {
	if c.VPNEntrypoint != "" {
		return c.VPNEntrypoint
	}
	return "vpn"
}

func (c Context) vpnMiddleware() string {
	if c.AllowLocal {
		if c.VPNLocalMiddleware != "" {
			return c.VPNLocalMiddleware
		}
		return "vpn-local@file"
	}
	if c.VPNOnlyMiddleware != "" {
		return c.VPNOnlyMiddleware
	}
	return "vpn-only@file"
}

func exposure(r catalog.Route, c Context) string {
	if c.Exposure != "" {
		return c.Exposure
	}
	return r.DefaultExposure()
}

func certificateMode(r catalog.Route, c Context) string {
	if c.Certificate != "" {
		return c.Certificate
	}
	return r.DefaultCertificate()
}

// ObjectName is the Ingress name for one route. Multi-route apps key it by the
// route so their Ingresses do not collide.
func ObjectName(e catalog.Entry, r catalog.Route) string {
	if r.Key != "" {
		return sanitize(e.Name + "-" + r.Key)
	}
	return sanitize(e.Name)
}

func objectName(e catalog.Entry, r catalog.Route, suffix string) string {
	if suffix == "" {
		return ObjectName(e, r)
	}
	return sanitize(ObjectName(e, r) + "-" + suffix)
}

// ObjectNameWithSuffix is the exported form used to clean up additional route
// objects on uninstall.
func ObjectNameWithSuffix(e catalog.Entry, r catalog.Route, suffix string) string {
	return objectName(e, r, suffix)
}

func serviceName(e catalog.Entry, r catalog.Route, ctx Context) string {
	if r.ServiceName != "" {
		return r.ServiceName
	}
	return ctx.Release
}

func servicePort(r catalog.Route) int {
	if r.ServicePort != 0 {
		return r.ServicePort
	}
	return 80
}

// Render returns the primary Ingress YAML for a site route. The primary route
// is normally the public/authelia route; for a VPN-only exposure it is the
// existing vpn entrypoint route. Additional trusted routes are rendered by
// RenderTrustedManagement and by calling Render with Exposure "vpn" and a
// NameSuffix.
func Render(e catalog.Entry, r catalog.Route, ctx Context) (string, error) {
	if r.Mode != "site" {
		return "", nil
	}
	exp := exposure(r, ctx)
	host := strings.ReplaceAll(r.Host, "{{ .zone }}", ctx.Zone)
	svc := serviceName(e, r, ctx)
	port := servicePort(r)
	entrypoint := ctx.entrypoint()
	if exp == "vpn" {
		entrypoint = ctx.vpnEntrypoint()
	}
	cert := certificateMode(r, ctx)
	secret := ctx.CertSecret
	if secret == "" && strings.HasPrefix(cert, "secret:") {
		secret = strings.TrimPrefix(cert, "secret:")
	}

	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: %s
  namespace: %s
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: %s
    traefik.ingress.kubernetes.io/router.tls: "true"
`, objectName(e, r, ctx.NameSuffix), e.Namespace, entrypoint)
	if cert == "acme" {
		fmt.Fprintf(&b, `    traefik.ingress.kubernetes.io/router.tls.certresolver: %s
`, ctx.certResolver())
	}

	// Middleware is a route property: public has none, Authelia has the
	// forward-auth middleware, and VPN routes have only the source allowlist.
	// The allowlist is defence in depth; the vpn entrypoint is not public.
	switch exp {
	case "authelia":
		fmt.Fprintf(&b, `    traefik.ingress.kubernetes.io/router.middlewares: %q
`, ctx.middleware())
	case "vpn":
		fmt.Fprintf(&b, `    traefik.ingress.kubernetes.io/router.middlewares: %q
`, ctx.vpnMiddleware())
	}

	if cert == "secret" || strings.HasPrefix(cert, "secret:") {
		fmt.Fprintf(&b, `spec:
  ingressClassName: traefik
  tls:
    - hosts:
        - %q
      secretName: %q
`, host, secret)
	} else {
		fmt.Fprintf(&b, `spec:
  ingressClassName: traefik
  tls:
    - hosts:
        - %q
`, host)
	}
	fmt.Fprintf(&b, `  rules:
    - host: %q
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: %s
                port:
                  number: %d
`, host, svc, port)
	return b.String(), nil
}

// RenderTrustedManagement returns the high-priority Traefik IngressRoute that
// lets a configured management source reach the app without Authelia. The
// source is matched from the TCP peer address by ClientIP; no request header is
// trusted.
func RenderTrustedManagement(e catalog.Entry, r catalog.Route, ctx Context) (string, error) {
	if r.Mode != "site" || len(ctx.ManagementCIDRs) == 0 {
		return "", nil
	}
	host := strings.ReplaceAll(r.Host, "{{ .zone }}", ctx.Zone)
	if host == "" {
		return "", fmt.Errorf("%s: trusted management route needs a host", e.Name)
	}
	svc := serviceName(e, r, ctx)
	port := servicePort(r)
	cert := certificateMode(r, ctx)
	secret := ctx.CertSecret
	if secret == "" && strings.HasPrefix(cert, "secret:") {
		secret = strings.TrimPrefix(cert, "secret:")
	}

	cidrs := make([]string, 0, len(ctx.ManagementCIDRs))
	for _, c := range ctx.ManagementCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		cidrs = append(cidrs, "`"+c+"`")
	}
	if len(cidrs) == 0 {
		return "", fmt.Errorf("%s: trusted management route has no source CIDRs", e.Name)
	}
	var excludes []string
	for _, c := range ctx.ManagementExcludeCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		excludes = append(excludes, "!ClientIP(`"+c+"`)")
	}
	match := fmt.Sprintf("Host(`%s`) && ClientIP(%s)", host, strings.Join(cidrs, ", "))
	if len(excludes) > 0 {
		match += " && " + strings.Join(excludes, " && ")
	}

	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: %s
  namespace: %s
spec:
  entryPoints: [websecure]
  routes:
    - kind: Rule
      match: %s
      priority: 1000
      services:
        - name: %s
          port: %d
`, objectName(e, r, "trusted-management"), e.Namespace, match, svc, port)
	switch {
	case cert == "acme":
		fmt.Fprintf(&b, "  tls:\n    certResolver: %s\n", ctx.certResolver())
	case cert == "secret" || strings.HasPrefix(cert, "secret:"):
		fmt.Fprintf(&b, "  tls:\n    secretName: %q\n", secret)
	default:
		b.WriteString("  tls: {}\n")
	}
	return b.String(), nil
}

// RenderTrustedVPN returns the same-host IngressRoute for WireGuard clients.
// It runs on websecure so the HA VIP/NodePort path can serve it without a
// separate Traefik entrypoint. ClientIP is the tunnel subnet; the gateway
// preserves the peer source, so public requests cannot match this rule.
func RenderTrustedVPN(e catalog.Entry, r catalog.Route, ctx Context) (string, error) {
	if r.Mode != "site" || ctx.VPNSubnet == "" {
		return "", nil
	}
	host := strings.ReplaceAll(r.Host, "{{ .zone }}", ctx.Zone)
	if host == "" {
		return "", fmt.Errorf("%s: trusted VPN route needs a host", e.Name)
	}
	svc := serviceName(e, r, ctx)
	port := servicePort(r)
	cert := certificateMode(r, ctx)
	secret := ctx.CertSecret
	if secret == "" && strings.HasPrefix(cert, "secret:") {
		secret = strings.TrimPrefix(cert, "secret:")
	}
	match := fmt.Sprintf("Host(`%s`) && ClientIP(`%s`)", host, strings.TrimSpace(ctx.VPNSubnet))

	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: %s
  namespace: %s
spec:
  entryPoints: [websecure]
  routes:
    - kind: Rule
      match: %s
      priority: 2000
      services:
        - name: %s
          port: %d
`, objectName(e, r, "trusted-vpn"), e.Namespace, match, svc, port)
	switch {
	case cert == "acme":
		fmt.Fprintf(&b, "  tls:\n    certResolver: %s\n", ctx.certResolver())
	case cert == "secret" || strings.HasPrefix(cert, "secret:"):
		fmt.Fprintf(&b, "  tls:\n    secretName: %q\n", secret)
	default:
		b.WriteString("  tls: {}\n")
	}
	return b.String(), nil
}

// AutheliaRule returns the access-control rule for the route, with any public
// paths bypassed ahead of the authenticated rule. Authelia evaluates rules top
// down and the first match wins, so the bypass must come first.
func AutheliaRule(e catalog.Entry, r catalog.Route, ctx Context) (string, error) {
	if r.Mode != "site" || exposure(r, ctx) != "authelia" {
		return "", nil
	}
	host := strings.ReplaceAll(r.Host, "{{ .zone }}", ctx.Zone)
	var b strings.Builder
	if len(r.PublicPaths) > 0 {
		fmt.Fprintf(&b, `    - domain: %q
      resources:
`, host)
		for _, p := range r.PublicPaths {
			fmt.Fprintf(&b, "        - %q\n", p)
		}
		b.WriteString("      policy: bypass\n")
	}
	fmt.Fprintf(&b, "    - domain: %q\n", host)
	if ctx.Tenant != "" {
		fmt.Fprintf(&b, "      subject:\n        - %q\n", "group:"+ctx.Tenant)
	}
	b.WriteString("      policy: one_factor\n")
	return b.String(), nil
}

// sanitize makes a name usable as an object name.
func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
