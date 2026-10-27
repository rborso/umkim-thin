package route

import (
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
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
