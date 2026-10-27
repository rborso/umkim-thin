package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
	"gopkg.in/yaml.v3"
)

// TestCatalogChartsRender is the cross-project contract: every shipped catalog
// entry is rendered with the same values the console injects (storage classes,
// LDAP/OIDC and hostAliases), against the chart pinned by that entry. Chart
// defaults alone are not enough; the actual consumer values are.
func TestCatalogChartsRender(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := catalog.Load(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	in := New(Config{
		Zone:        "example.com",
		ConfigClass: "longhorn-rwo",
		MediaClass:  "external-nfs",
		BaseDN:      "dc=example,dc=com",
		LLDAPHost:   "lldap.identity.svc.cluster.local",
		LLDAPPort:   3890,
		OIDCIssuer:  "https://auth.example.com",
	})

	for _, entry := range entries {
		entry := entry
		t.Run(entry.Name, func(t *testing.T) {
			ctx, err := in.context(entry, Options{})
			if err != nil {
				t.Fatal(err)
			}
			values := in.renderValues(entry, ctx)
			if entry.Auth.LDAP {
				set(values, "auth.ldap", map[string]any{
					"enabled":           true,
					"host":              in.cfg.LLDAPHost,
					"port":              in.cfg.LLDAPPort,
					"bindDn":            "uid=svc-bind,ou=people," + in.cfg.BaseDN,
					"userSearchBase":    "ou=people," + in.cfg.BaseDN,
					"baseDn":            "ou=people," + in.cfg.BaseDN,
					"groupBaseDn":       "ou=groups," + in.cfg.BaseDN,
					"existingSecret":    entry.Name + "-lldap-bind",
					"existingSecretKey": "LLDAP_LDAP_USER_PASS",
				})
			}
			if entry.Auth.Kind == "oidc" && entry.OIDC != nil {
				in.applyOIDCValues(entry, values, &autheliaClient{
					ClientID:                entry.OIDC.ClientID,
					ClientSecret:            "ci-only-client-secret",
					RedirectURIs:            entry.OIDC.RedirectURIs,
					RequirePKCE:             entry.OIDC.RequirePKCE,
					TokenEndpointAuthMethod: "client_secret_basic",
				})
			}
			var hosts []any
			for _, route := range entry.EffectiveRoutes() {
				if host := ctx.hosts[route.Key]; host != "" {
					hosts = append(hosts, host)
				}
			}
			set(values, "hostAliases", []any{
				map[string]any{"ip": "10.43.0.1", "hostnames": hosts},
			})

			raw, err := yaml.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			valuesFile := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(valuesFile, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			chartDir := catalogChartDir(root, entry)
			if chartDir == "" {
				t.Fatalf("no local chart for %s", entry.Chart.Ref)
			}
			cmd := exec.Command(helm, "template", entry.Name, chartDir,
				"--namespace", entry.Namespace, "--values", valuesFile)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("helm template %s with catalog values: %v\n%s", entry.Name, err, out)
			}
		})
	}
}

func catalogChartDir(root string, entry catalog.Entry) string {
	ref := strings.TrimSuffix(entry.Chart.Ref, "/")
	name := ref[strings.LastIndex(ref, "/")+1:]
	for _, base := range []string{"charts/apps", "charts"} {
		dir := filepath.Join(root, base, name)
		if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err == nil {
			return dir
		}
	}
	return ""
}
