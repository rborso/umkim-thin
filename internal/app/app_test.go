package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
)

func testInstaller() *Installer {
	return New(Config{
		Zone:         "example.com",
		ConfigClass:  "local-path",
		MediaClass:   "umkim-thin-nfs",
		ServiceCIDR:  "10.43.0.0/16",
		PodCIDR:      "10.244.0.0/16",
		DNSServiceIP: "10.43.0.10",
	})
}

func multiRouteEntry() catalog.Entry {
	return catalog.Entry{
		Name:      "nextcloud",
		Namespace: "nextcloud",
		Routes: []catalog.Route{
			{Key: "site", Host: "nextcloud.{{ .zone }}", Mode: "site"},
			{Key: "collabora", Host: "collabora.{{ .zone }}", Mode: "site"},
		},
	}
}

func TestContextHostOverrides(t *testing.T) {
	in := testInstaller()
	e := multiRouteEntry()

	ctx, err := in.context(e, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.hosts["site"] != "nextcloud.example.com" || ctx.hosts["collabora"] != "collabora.example.com" {
		t.Fatalf("default hosts = %+v", ctx.hosts)
	}
	if ctx.primary != "nextcloud.example.com" {
		t.Fatalf("primary host = %q", ctx.primary)
	}

	// The install form's hostname (the "" key) applies to the first site route
	// only; extra routes keep their declared hostnames.
	ctx, err = in.context(e, Options{Hosts: map[string]string{"": "cloud.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.hosts["site"] != "cloud.example.com" {
		t.Fatalf("primary override not applied: %+v", ctx.hosts)
	}
	if ctx.hosts["collabora"] != "collabora.example.com" {
		t.Fatalf("primary override leaked to a secondary route: %+v", ctx.hosts)
	}

	// An explicit per-key override wins over the primary hostname.
	ctx, err = in.context(e, Options{Hosts: map[string]string{
		"":          "cloud.example.com",
		"collabora": "docs.example.com",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.hosts["site"] != "cloud.example.com" || ctx.hosts["collabora"] != "docs.example.com" {
		t.Fatalf("per-key override lost: %+v", ctx.hosts)
	}
}

func TestPreserveStorageClasses(t *testing.T) {
	dir := t.TempDir()
	in := testInstaller()
	in.cfg.StateDir = dir
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := []byte(`
nextcloud:
  persistence:
    data:
      storageClass: external-nfs
    media:
      storageClass: ""
postgres:
  persistence:
    storageClass: external-nfs
`)
	if err := os.WriteFile(filepath.Join(dir, "apps", "nextcloud-values.yaml"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	current := map[string]any{
		"nextcloud": map[string]any{
			"persistence": map[string]any{
				"data":  map[string]any{"storageClass": "longhorn-rwx"},
				"media": map[string]any{"storageClass": "longhorn-rwx"},
			},
		},
		"postgres": map[string]any{
			"persistence": map[string]any{"storageClass": "longhorn-rwo"},
		},
	}
	if err := in.preserveStorageClasses("nextcloud", "nextcloud", current); err != nil {
		t.Fatal(err)
	}
	nested := current["nextcloud"].(map[string]any)["persistence"].(map[string]any)
	if got := nested["data"].(map[string]any)["storageClass"]; got != "external-nfs" {
		t.Fatalf("data storage class = %v", got)
	}
	if got := nested["media"].(map[string]any)["storageClass"]; got != "longhorn-rwx" {
		t.Fatalf("empty old storage class should not overwrite the new one: %v", got)
	}
	if got := current["postgres"].(map[string]any)["persistence"].(map[string]any)["storageClass"]; got != "external-nfs" {
		t.Fatalf("postgres storage class = %v", got)
	}
}

func TestRenderValues(t *testing.T) {
	in := testInstaller()
	e := catalog.Entry{
		Name:      "nextcloud",
		Namespace: "nextcloud",
		Route:     &catalog.Route{Mode: "site", Host: "nextcloud.{{ .zone }}"},
		Values: map[string]any{
			"externalUrl": "https://{{ .host }}",
			"nested":      map[string]any{"zone": "{{ .zone }}", "release": "{{ .release }}"},
			"list":        []any{"{{ .namespace }}", 42, true},
			"storage":     "{{ .storage.configClass }}/{{ .storage.mediaClass }}/{{ .storage.mediaAccessMode }}",
			"cluster":     "{{ .cluster.dnsServiceIP }}",
		},
	}
	ctx, err := in.context(e, Options{Hosts: map[string]string{"": "cloud.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	got := in.renderValues(e, ctx)

	if got["externalUrl"] != "https://cloud.example.com" {
		t.Errorf("externalUrl = %v", got["externalUrl"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["zone"] != "example.com" || nested["release"] != "nextcloud" {
		t.Errorf("nested = %+v", got["nested"])
	}
	list, ok := got["list"].([]any)
	if !ok || list[0] != "nextcloud" || list[1] != 42 || list[2] != true {
		t.Errorf("list = %+v", got["list"])
	}
	if got["storage"] != "local-path/umkim-thin-nfs/ReadWriteMany" || got["cluster"] != "10.43.0.10" {
		t.Errorf("storage/cluster = %v / %v", got["storage"], got["cluster"])
	}
}

func TestChartRefUsesMirror(t *testing.T) {
	dir := t.TempDir()
	in := New(Config{ChartMirror: dir})
	e := catalog.Entry{Name: "nextcloud", Chart: catalog.ChartRef{
		Ref:     "oci://ghcr.io/example/charts/nextcloud",
		Version: "0.1.0",
	}}

	if got := in.chartRef(e); got != e.Chart.Ref {
		t.Fatalf("chartRef without a package = %q, want %q", got, e.Chart.Ref)
	}

	packaged := filepath.Join(dir, "nextcloud-0.1.0.tgz")
	if err := os.WriteFile(packaged, []byte("not a real chart"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := in.chartRef(e); got != packaged {
		t.Fatalf("chartRef with a package = %q, want %q", got, packaged)
	}
}

func TestClientForReusesSecret(t *testing.T) {
	in := testInstaller()
	e := catalog.Entry{
		Name:        "immich",
		DisplayName: "Immich",
		OIDC: &catalog.OIDC{
			ClientID:     "immich",
			RedirectURIs: []string{"https://{{ .host }}/auth/login"},
		},
	}
	repl := map[string]string{"{{ .host }}": "immich.example.com"}

	first, err := in.clientFor(e, contributions{}, repl)
	if err != nil {
		t.Fatal(err)
	}
	if first.ClientID != "immich" || first.RedirectURIs[0] != "https://immich.example.com/auth/login" {
		t.Fatalf("client = %+v", first)
	}
	if first.ClientSecret == "" || first.AuthorizationPolicy != "one_factor" {
		t.Fatalf("client secret/policy = %q/%q", first.ClientSecret, first.AuthorizationPolicy)
	}

	second, err := in.clientFor(e, contributions{"immich": {OIDC: first}}, repl)
	if err != nil {
		t.Fatal(err)
	}
	if second.ClientSecret != first.ClientSecret {
		t.Fatal("a re-install rotated the OIDC client secret")
	}

	e.OIDC = nil
	if _, err := in.clientFor(e, contributions{}, repl); err == nil {
		t.Fatal("clientFor accepted an entry without oidc.clientId")
	}
}

func TestPublicPathsFor(t *testing.T) {
	in := testInstaller()
	e := catalog.Entry{
		Name: "nextcloud",
		Routes: []catalog.Route{
			{Key: "site", Mode: "site", Host: "nextcloud.{{ .zone }}", RequireAuth: true, PublicPaths: []string{"^/s/"}},
			{Key: "public", Mode: "site", Host: "public.{{ .zone }}", RequireAuth: false, PublicPaths: []string{"^/x/"}},
		},
	}
	ctx := siteContext{hosts: map[string]string{
		"site":   "nextcloud.example.com",
		"public": "public.example.com",
	}}
	rules := in.publicPathsFor(e, ctx)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want only the authenticated route: %+v", len(rules), rules)
	}
	if rules[0].Domain != "nextcloud.example.com" || rules[0].Paths[0] != "^/s/" {
		t.Fatalf("rule = %+v", rules[0])
	}
}

func TestFrac(t *testing.T) {
	if a, b, ok := frac("3/4"); !ok || a != 3 || b != 4 {
		t.Fatalf("frac(3/4) = %d,%d,%v", a, b, ok)
	}
	if _, _, ok := frac("3"); ok {
		t.Fatal("frac(3) should fail")
	}
}

func TestSetPath(t *testing.T) {
	root := map[string]any{}
	set(root, "a.b.c", "value")
	set(root, "a.b.d", 2)
	a, ok := root["a"].(map[string]any)
	if !ok {
		t.Fatalf("a = %+v", root["a"])
	}
	b, ok := a["b"].(map[string]any)
	if !ok || b["c"] != "value" || b["d"] != 2 {
		t.Fatalf("b = %+v", b)
	}
}

func TestKnownAccessModes(t *testing.T) {
	cases := []struct{ name, provisioner, want string }{
		{"media-rwx", "example.com/thing", "RWX"},
		{"nfs", "nfs.csi.k8s.io", "RWX"},
		{"local-path", "rancher.io/local-path", "RWO"},
		{"longhorn", "driver.longhorn.io", "RWO"},
		{"weird", "example.com/thing", ""},
	}
	for _, c := range cases {
		if got := strings.Join(knownAccessModes(c.name, c.provisioner), ","); got != c.want {
			t.Errorf("knownAccessModes(%q, %q) = %q, want %q", c.name, c.provisioner, got, c.want)
		}
	}
}

func TestAccessModesFor(t *testing.T) {
	cases := []struct {
		name       string
		sc         StorageClass
		observed   map[string][]string
		configured map[string][]string
		wantModes  string
		wantSource string
	}{
		{
			name:      "NFS convention wins over a RWO PVC on the same class",
			sc:        StorageClass{Name: "nfs-external-nfs", Provisioner: "cluster.local/nfs-external-nfs-nfs-subdir-external-provisioner"},
			observed:  map[string][]string{"nfs-external-nfs": {"ReadWriteOnce"}},
			wantModes: "RWX", wantSource: "known",
		},
		{
			name:      "observed mode is used for an unknown class",
			sc:        StorageClass{Name: "custom", Provisioner: "example.com/thing"},
			observed:  map[string][]string{"custom": {"ReadWriteOnce"}},
			wantModes: "RWO", wantSource: "observed",
		},
		{
			name:       "explicit configuration wins",
			sc:         StorageClass{Name: "nfs-external-nfs", Provisioner: "cluster.local/nfs-subdir-external-provisioner"},
			configured: map[string][]string{"nfs-external-nfs": {"RWO"}},
			wantModes:  "RWO", wantSource: "configured",
		},
		{
			name:       "unknown",
			sc:         StorageClass{Name: "custom", Provisioner: "example.com/thing"},
			wantSource: "unknown",
		},
	}
	for _, c := range cases {
		gotModes, gotSource := accessModesFor(c.sc, c.observed, c.configured)
		if modeStr := strings.Join(gotModes, ","); modeStr != c.wantModes || gotSource != c.wantSource {
			t.Errorf("%s: got %q (%s), want %q (%s)", c.name, modeStr, gotSource, c.wantModes, c.wantSource)
		}
	}
}

func TestContextVPN(t *testing.T) {
	in := testInstaller()
	e := catalog.Entry{Name: "app", Namespace: "app", Route: &catalog.Route{
		Mode: "site", Host: "app.{{ .zone }}", RequireAuth: true,
	}}
	ctx, err := in.context(e, Options{
		Exposure:    map[string]string{"": "vpn"},
		AllowLocal:  map[string]bool{"": true},
		Certificate: map[string]string{"": "secret:wildcard-vpn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ctx.hosts[""]; got != "app.vpn.example.com" {
		t.Fatalf("vpn host = %q", got)
	}
	if ctx.exposure[""] != "vpn" || !ctx.allowLocal[""] || ctx.certificate[""] != "secret:wildcard-vpn" {
		t.Fatalf("selection = %+v", ctx)
	}
	if _, err := in.context(e, Options{Exposure: map[string]string{"": "public"}}); err == nil {
		t.Fatal("public was accepted for an authelia-only route")
	}
	if _, err := in.context(e, Options{Exposure: map[string]string{"": "vpn"}, AllowLocal: map[string]bool{"": true}}); err != nil {
		t.Fatalf("VPN + local rejected: %v", err)
	}
}

func TestUpgradeKeepsRouteSelection(t *testing.T) {
	dir := t.TempDir()
	in := New(Config{Zone: "example.com", StateDir: dir, AppsState: filepath.Join(dir, "apps-state.json")})
	e := catalog.Entry{Name: "app", Namespace: "app", Route: &catalog.Route{
		Mode: "site", Host: "app.{{ .zone }}", RequireAuth: true,
	}}
	first, err := in.context(e, Options{
		Exposure:    map[string]string{"": "vpn"},
		AllowLocal:  map[string]bool{"": true},
		Certificate: map[string]string{"": "secret:app-cert"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.saveRoutes(e, first); err != nil {
		t.Fatal(err)
	}
	up, err := in.context(e, Options{Upgrade: true})
	if err != nil {
		t.Fatal(err)
	}
	if up.exposure[""] != "vpn" || !up.allowLocal[""] || up.certificate[""] != "secret:app-cert" {
		t.Fatalf("upgrade lost the selection: %+v", up)
	}
}
