package route

import (
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
	"gopkg.in/yaml.v3"
)

func entry(route catalog.Route) catalog.Entry {
	return catalog.Entry{
		Name:      "nextcloud",
		Namespace: "nextcloud",
		Route:     &route,
	}
}

func TestRenderSiteRoute(t *testing.T) {
	e := entry(catalog.Route{
		Mode:        "site",
		Host:        "nextcloud.{{ .zone }}",
		RequireAuth: true,
		ServiceName: "nextcloud",
		ServicePort: 8080,
	})
	out, err := Render(e, *e.Route, Context{Zone: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: Ingress",
		"namespace: nextcloud",
		"name: nextcloud",
		`host: "nextcloud.example.com"`,
		`traefik.ingress.kubernetes.io/router.middlewares: "identity-authelia@file"`,
		"router.entrypoints: websecure",
		"certresolver: letsencrypt",
		"ingressClassName: traefik",
		"name: nextcloud",
		"number: 8080",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Render() missing %q:\n%s", want, out)
		}
	}
}

func TestRenderDefaults(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}"})
	out, err := Render(e, *e.Route, Context{Zone: "example.com", Release: "myapp"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: myapp") || !strings.Contains(out, "number: 80") {
		t.Fatalf("service defaults not applied:\n%s", out)
	}
	if strings.Contains(out, "middlewares") {
		t.Fatalf("a route without requireAuth got a middleware:\n%s", out)
	}
}

func TestRenderOverrides(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", RequireAuth: true})
	out, err := Render(e, *e.Route, Context{
		Zone:         "example.com",
		Middleware:   "custom-auth@file",
		Entrypoint:   "web",
		CertResolver: "staging",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"custom-auth@file", "router.entrypoints: web", "certresolver: staging"} {
		if !strings.Contains(out, want) {
			t.Errorf("override %q missing:\n%s", want, out)
		}
	}
}

func TestAutheliaRulePublicPathsComeFirst(t *testing.T) {
	e := entry(catalog.Route{
		Mode:        "site",
		Host:        "nextcloud.{{ .zone }}",
		RequireAuth: true,
		PublicPaths: []string{"^/s/", "^/public\\.php"},
	})
	out, err := AutheliaRule(e, *e.Route, Context{Zone: "example.com", Tenant: "default-tenant"})
	if err != nil {
		t.Fatal(err)
	}
	bypass := strings.Index(out, "policy: bypass")
	auth := strings.Index(out, "policy: one_factor")
	if bypass < 0 || auth < 0 {
		t.Fatalf("missing bypass or auth rule:\n%s", out)
	}
	if bypass > auth {
		t.Fatalf("the bypass rule must come before the authenticated rule:\n%s", out)
	}
	for _, want := range []string{`- "^/s/"`, `- "^/public\\.php"`, `- "group:default-tenant"`, `domain: "nextcloud.example.com"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rule missing %q:\n%s", want, out)
		}
	}
}

func TestAutheliaRuleWithoutAuth(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "public.{{ .zone }}", RequireAuth: false})
	out, err := AutheliaRule(e, *e.Route, Context{Zone: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("an unauthenticated route produced a rule: %q", out)
	}
}

func TestObjectName(t *testing.T) {
	e := catalog.Entry{Name: "Next Cloud!"}
	if got := ObjectName(e, catalog.Route{}); got != "next-cloud-" {
		t.Fatalf("ObjectName() = %q", got)
	}
	if got := ObjectName(e, catalog.Route{Key: "Jelly Fin"}); got != "next-cloud--jelly-fin" {
		t.Fatalf("ObjectName() with key = %q", got)
	}
}

func TestRenderVPNOnly(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", RequireAuth: true, ServiceName: "svc"})
	out, err := Render(e, *e.Route, Context{Zone: "example.com", Exposure: "vpn"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"router.entrypoints: vpn",
		`middlewares: "vpn-only@file"`,
		"certresolver: letsencrypt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("VPN route missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"router.entrypoints: websecure", "identity-authelia@file"} {
		if strings.Contains(out, bad) {
			t.Errorf("VPN route leaked %q:\n%s", bad, out)
		}
	}
}

func TestRenderVPNLocalAndManualCert(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", RequireAuth: true, ServiceName: "svc"})
	out, err := Render(e, *e.Route, Context{
		Zone: "example.com", Exposure: "vpn", AllowLocal: true,
		Certificate: "secret:wildcard-example",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`middlewares: "vpn-local@file"`,
		`secretName: "wildcard-example"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("VPN/local/manual route missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "certresolver:") {
		t.Errorf("manual certificate still has a certresolver:\n%s", out)
	}
}

func TestRenderDefaultCertificate(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", RequireAuth: false, ServiceName: "svc"})
	out, err := Render(e, *e.Route, Context{Zone: "example.com", Certificate: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "certresolver:") || strings.Contains(out, "secretName:") {
		t.Fatalf("default certificate should inherit the TLS store:\n%s", out)
	}
	if strings.Contains(out, "identity-authelia@file") {
		t.Fatalf("public route has auth middleware:\n%s", out)
	}
}

func TestAutheliaRuleSkipsVPN(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", RequireAuth: true})
	out, err := AutheliaRule(e, *e.Route, Context{Zone: "example.com", Exposure: "vpn"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("VPN route produced an Authelia rule: %q", out)
	}
}

func TestRenderTrustedManagement(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", RequireAuth: true, ServiceName: "svc", ServicePort: 8080})
	out, err := RenderTrustedManagement(e, *e.Route, Context{
		Zone:                   "example.com",
		ManagementCIDRs:        []string{"172.16.10.0/24", "192.0.2.10/32"},
		ManagementExcludeCIDRs: []string{"172.16.10.1/32"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apiVersion: traefik.io/v1alpha1",
		"kind: IngressRoute",
		"name: nextcloud-trusted-management",
		"entryPoints: [websecure]",
		"Host(`app.example.com`)",
		"ClientIP(`172.16.10.0/24`, `192.0.2.10/32`)",
		"!ClientIP(`172.16.10.1/32`)",
		"priority: 1000",
		"name: svc",
		"port: 8080",
		"certResolver: letsencrypt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("management route missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "identity-authelia@file") {
		t.Errorf("management route has Authelia:\n%s", out)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("management route is not valid YAML: %v\n%s", err, out)
	}
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("management route has no spec:\n%s", out)
	}
	if _, ok := spec["tls"]; !ok {
		t.Fatalf("management route has no tls block:\n%s", out)
	}
}

func TestRenderTrustedManagementCertificateModes(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", ServiceName: "svc"})
	out, err := RenderTrustedManagement(e, *e.Route, Context{
		Zone: "example.com", ManagementCIDRs: []string{"172.16.10.0/24"},
		Certificate: "secret:wildcard-example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `secretName: "wildcard-example"`) || strings.Contains(out, "certResolver:") {
		t.Fatalf("secret certificate not mirrored:\n%s", out)
	}
	out, err = RenderTrustedManagement(e, *e.Route, Context{
		Zone: "example.com", ManagementCIDRs: []string{"172.16.10.0/24"},
		Certificate: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "tls: {}") {
		t.Fatalf("default certificate should use the TLS store:\n%s", out)
	}
	if out, err := RenderTrustedManagement(e, *e.Route, Context{Zone: "example.com"}); err != nil || out != "" {
		t.Fatalf("no CIDRs should render no object: %q, %v", out, err)
	}
}

func TestRenderTrustedVPN(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", ServiceName: "svc"})
	out, err := RenderTrustedVPN(e, *e.Route, Context{
		Zone: "example.com", VPNSubnet: "192.168.169.0/24",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: IngressRoute",
		"name: nextcloud-trusted-vpn",
		"entryPoints: [websecure]",
		"Host(`app.example.com`)",
		"ClientIP(`192.168.169.0/24`)",
		"priority: 2000",
		"name: svc",
		"certResolver: letsencrypt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("trusted VPN route missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"identity-authelia@file", "vpn-only@file", "entryPoints: [vpn]"} {
		if strings.Contains(out, bad) {
			t.Errorf("trusted VPN route leaked %q:\n%s", bad, out)
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("trusted VPN route is not valid YAML: %v\n%s", err, out)
	}
}

func TestRenderSameHostVPN(t *testing.T) {
	e := entry(catalog.Route{Mode: "site", Host: "app.{{ .zone }}", RequireAuth: true, ServiceName: "svc"})
	out, err := Render(e, *e.Route, Context{
		Zone: "example.com", Exposure: "vpn", NameSuffix: "trusted-vpn",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: nextcloud-trusted-vpn",
		`host: "app.example.com"`,
		"router.entrypoints: vpn",
		`middlewares: "vpn-only@file"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("same-host VPN route missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "identity-authelia@file") || strings.Contains(out, "router.entrypoints: websecure") {
		t.Fatalf("same-host VPN route leaked the public gate:\n%s", out)
	}
}
