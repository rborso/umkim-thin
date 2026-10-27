// Package app installs catalog entries onto the cluster.
//
// An install is: resolve the pinned chart (from a local mirror when the private
// registry is unreachable, otherwise the catalog's OCI ref), render the entry's
// values with the site's templating context, wire directory authentication to
// this cluster's LLDAP, run helm, and publish the route(s) as Ingresses on
// Traefik carrying the Authelia middleware.
package app

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rborso/umkim-thin/internal/catalog"
	"github.com/rborso/umkim-thin/internal/route"
	"github.com/rborso/umkim-thin/internal/wg"
	"gopkg.in/yaml.v3"
)

// Config is everything the installer needs, normally from the environment so
// the same binary works for the API (systemd) and the CLI.
type Config struct {
	Kubeconfig      string
	ChartMirror     string
	RemoteDir       string
	Zone            string
	Tenant          string
	PortalHost      string
	OIDCIssuer      string
	ConfigClass     string
	MediaClass      string
	MediaAccessMode string
	ServiceCIDR     string
	PodCIDR         string
	DNSServiceIP    string
	Middleware      string
	IdentityNS      string
	IdentityChart   string
	IdentityValues  string
	IdentityApps    string
	AppsState       string
	OIDCHmacFile    string
	OIDCKeyFile     string
	TraefikNS       string
	TraefikSvc      string
	BindSecret      string
	BaseDN          string
	LLDAPHost       string
	LLDAPPort       int
	Helm            string
	Kubectl         string
	StateDir        string

	// Console and on-demand bootstrap. The console image is the source of the
	// pinned bootstrap build context, so the builder never accepts one from a
	// caller; the image is imported into the pinned node's containerd and the
	// Job runs there from the local store (no registry).
	ConsoleImage     string
	BootstrapImage   string
	BootstrapContext string
	BuilderImage     string
	BuildNamespace   string
	BootstrapNode    string
	GitSHA           string
}

// ConfigFile is read before the environment, so CLI use on the node does not
// have to re-export anything.
const ConfigFile = "/etc/umkim-thin/config.env"

// ConfigFromEnv builds the config: the config file first, then the environment.
func ConfigFromEnv() Config {
	loadConfigFile(ConfigFile)
	zone := env("UMKIM_THIN_ZONE", "")
	stateDir := env("UMKIM_THIN_STATE_DIR", "/etc/umkim-thin")
	remoteDir := env("UMKIM_THIN_REMOTE_DIR", "/opt/umkim-thin")
	portalHost := env("UMKIM_THIN_PORTAL_HOST", "")
	if portalHost == "" && zone != "" {
		portalHost = "auth." + zone
	}
	return Config{
		Kubeconfig:      env("UMKIM_THIN_KUBECONFIG", "/etc/rancher/k3s/k3s.yaml"),
		ChartMirror:     env("UMKIM_THIN_CHART_MIRROR", remoteDir+"/chart-mirror"),
		RemoteDir:       remoteDir,
		Zone:            zone,
		Tenant:          env("UMKIM_THIN_TENANT", "default-tenant"),
		PortalHost:      portalHost,
		OIDCIssuer:      env("UMKIM_THIN_OIDC_ISSUER", "https://"+portalHost),
		ConfigClass:     env("UMKIM_THIN_STORAGE_CONFIG_CLASS", "local-path"),
		MediaClass:      env("UMKIM_THIN_STORAGE_MEDIA_CLASS", "umkim-thin-nfs"),
		MediaAccessMode: env("UMKIM_THIN_STORAGE_MEDIA_ACCESS_MODE", "ReadWriteMany"),
		ServiceCIDR:     env("UMKIM_THIN_SERVICE_CIDR", "10.43.0.0/16"),
		PodCIDR:         env("UMKIM_THIN_POD_CIDR", "10.42.0.0/16"),
		DNSServiceIP:    env("UMKIM_THIN_DNS_IP", "10.43.0.10"),
		Middleware:      env("UMKIM_THIN_MIDDLEWARE", "identity-authelia@file"),
		IdentityNS:      env("UMKIM_THIN_IDENTITY_NAMESPACE", "identity"),
		IdentityChart:   env("UMKIM_THIN_IDENTITY_CHART", remoteDir+"/charts/identity"),
		IdentityValues:  env("UMKIM_THIN_IDENTITY_VALUES", stateDir+"/identity-values.yml"),
		IdentityApps:    env("UMKIM_THIN_IDENTITY_APPS", stateDir+"/identity-apps.yml"),
		AppsState:       env("UMKIM_THIN_APPS_STATE", stateDir+"/apps-state.json"),
		OIDCHmacFile:    env("UMKIM_THIN_OIDC_HMAC", stateDir+"/authelia-oidc-hmac"),
		OIDCKeyFile:     env("UMKIM_THIN_OIDC_KEY", stateDir+"/authelia-oidc-key.pem"),
		TraefikNS:       env("UMKIM_THIN_TRAEFIK_NAMESPACE", "kube-system"),
		TraefikSvc:      env("UMKIM_THIN_TRAEFIK_SERVICE", "traefik"),
		BindSecret:      env("UMKIM_THIN_BIND_SECRET", "lldap-service-accounts"),
		BaseDN:          env("UMKIM_THIN_BASE_DN", ""),
		LLDAPHost:       env("UMKIM_THIN_LLDAP_HOST", "lldap.identity.svc.cluster.local"),
		LLDAPPort:       atoi(env("UMKIM_THIN_LLDAP_PORT", "3890")),
		Helm:            env("UMKIM_THIN_HELM", "/usr/local/bin/helm"),
		Kubectl:         env("UMKIM_THIN_KUBECTL", "/usr/local/bin/kubectl"),
		StateDir:        stateDir,

		ConsoleImage:     env("UMKIM_THIN_CONSOLE_IMAGE", "localhost/umkim-thin:dev"),
		BootstrapImage:   env("UMKIM_THIN_BOOTSTRAP_IMAGE", "localhost/umkim-thin/servarr-bootstrap"),
		BootstrapContext: env("UMKIM_THIN_BOOTSTRAP_CONTEXT", "/opt/umkim-thin/bootstrap"),
		BuilderImage:     env("UMKIM_THIN_BUILDER_IMAGE", "gcr.io/kaniko-project/executor:v1.23.2-debug@sha256:fb6d29da5792b6b9886bb132d1bb600bec79a47a31e4c152df3e52de9c561a44"),
		BuildNamespace:   env("UMKIM_THIN_BUILD_NAMESPACE", "umkim-thin-build"),
		BootstrapNode:    env("UMKIM_THIN_BOOTSTRAP_NODE", ""),
		GitSHA:           firstNonEmpty(env("UMKIM_THIN_GIT_SHA", ""), readFileTrim("/opt/umkim-thin/git-sha")),
	}
}

// Installer performs installs against the cluster.
type Installer struct{ cfg Config }

// New returns an installer for the given configuration.
func New(cfg Config) *Installer { return &Installer{cfg: cfg} }

// Result reports what an install did, for the CLI and the API.
type Result struct {
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	Chart      string   `json:"chart"`
	AppVersion string   `json:"appVersion,omitempty"`
	HelmStatus string   `json:"helmStatus,omitempty"`
	Routes     []string `json:"routes"`
}

// Options are per-install overrides; the zero value uses the site defaults.
type Options struct {
	Hosts       map[string]string // route key (or "" for a single route) -> host
	ConfigClass string            // RWO class for config/databases
	MediaClass  string            // RWX class for shared media
	Progress    func(string)      // optional progress sink (async installs)

	// Per-route exposure/certificate choices. Empty maps use the catalog
	// defaults. The key is the route key ("" for a single-route app).
	Exposure    map[string]string
	AllowLocal  map[string]bool
	Certificate map[string]string
	CertSecret  map[string]string

	// ChartVersion selects a published chart version; empty uses the catalog
	// stable default. Upgrade requires an explicit version and backup
	// confirmation unless SkipBackup is set.
	ChartVersion  string
	Upgrade       bool
	ConfirmBackup bool
	SkipBackup    bool

	// MediaLanguage and QualityCutoff are the media stack's defaults. They are
	// ignored for other apps and default to the catalog values when empty.
	MediaLanguage string
	QualityCutoff string
	// ExtraLanguages creates optional language variants of the target quality
	// after the default stack has been installed.
	ExtraLanguages []string
	// Language is an app-level default for apps that support one. It is
	// currently used by Nextcloud; Immich language is a per-user/browser setting.
	Language string
}

func (o Options) step(format string, a ...any) {
	if o.Progress != nil {
		o.Progress(fmt.Sprintf(format, a...))
	}
}

// siteContext is the templating context for one install: the effective hosts
// (which become {{ .host }} and the route hosts) and storage classes.
type siteContext struct {
	zone            string
	configClass     string
	mediaClass      string
	mediaAccessMode string
	hosts           map[string]string // route key -> effective host
	primary         string
	exposure        map[string]string
	allowLocal      map[string]bool
	certificate     map[string]string
	certSecret      map[string]string
}

func (in *Installer) context(e catalog.Entry, opts Options) (siteContext, error) {
	mediaAccessMode := in.cfg.MediaAccessMode
	if mediaAccessMode == "" {
		mediaAccessMode = "ReadWriteMany"
	}
	// An upgrade keeps the choices the app was installed with unless the
	// operator explicitly overrides them.
	if opts.Upgrade {
		c := in.loadContributions()
		if prev, ok := c[e.Name]; ok {
			for key, rs := range prev.Routes {
				if opts.Exposure == nil {
					opts.Exposure = map[string]string{}
				}
				if _, set := opts.Exposure[key]; !set {
					opts.Exposure[key] = rs.Exposure
				}
				if opts.AllowLocal == nil {
					opts.AllowLocal = map[string]bool{}
				}
				if _, set := opts.AllowLocal[key]; !set {
					opts.AllowLocal[key] = rs.AllowLocal
				}
				if opts.Certificate == nil {
					opts.Certificate = map[string]string{}
				}
				if _, set := opts.Certificate[key]; !set {
					opts.Certificate[key] = rs.Certificate
				}
			}
		}
	}
	c := siteContext{
		zone:            in.cfg.Zone,
		configClass:     firstNonEmpty(opts.ConfigClass, in.cfg.ConfigClass),
		mediaClass:      firstNonEmpty(opts.MediaClass, in.cfg.MediaClass),
		mediaAccessMode: mediaAccessMode,
		hosts:           map[string]string{},
		exposure:        map[string]string{},
		allowLocal:      map[string]bool{},
		certificate:     map[string]string{},
		certSecret:      map[string]string{},
	}
	for _, r := range e.EffectiveRoutes() {
		exp := opts.Exposure[r.Key]
		if exp == "" {
			exp = r.DefaultExposure()
		}
		if !contains(r.AllowedExposures(), exp) {
			return siteContext{}, fmt.Errorf("%s: exposure %q is not allowed for route %q (allowed: %s)", e.Name, exp, r.Key, strings.Join(r.AllowedExposures(), ", "))
		}
		allowLocal := r.AllowsLocal()
		if v, ok := opts.AllowLocal[r.Key]; ok {
			allowLocal = v
		}
		if allowLocal && exp != "vpn" {
			return siteContext{}, fmt.Errorf("%s: allowLocal is only valid with exposure vpn", e.Name)
		}
		cert := opts.Certificate[r.Key]
		if cert == "" {
			cert = r.DefaultCertificate()
		}
		if !certificateAllowed(r, cert) {
			return siteContext{}, fmt.Errorf("%s: certificate %q is not allowed for route %q", e.Name, cert, r.Key)
		}
		if strings.HasPrefix(cert, "secret:") && strings.TrimPrefix(cert, "secret:") == "" {
			return siteContext{}, fmt.Errorf("%s: certificate secret needs a name", e.Name)
		}

		host := strings.ReplaceAll(r.Host, "{{ .zone }}", c.zone)
		if exp == "vpn" {
			if r.VPNHost != "" {
				host = strings.ReplaceAll(r.VPNHost, "{{ .zone }}", c.zone)
			} else {
				host = vpnHost(host, c.zone)
			}
		}
		// An explicit per-key override wins. The install form's hostname (the
		// "" key) is the app's primary hostname: it applies to the first site
		// route only, so an entry's extra routes (e.g. Collabora) keep the
		// hostnames their catalog declares.
		if v, ok := opts.Hosts[r.Key]; ok && v != "" {
			host = v
		} else if v, ok := opts.Hosts[""]; ok && v != "" && c.primary == "" && r.Mode == "site" {
			host = v
		}
		c.hosts[r.Key] = host
		c.exposure[r.Key] = exp
		c.allowLocal[r.Key] = allowLocal
		c.certificate[r.Key] = cert
		if strings.HasPrefix(cert, "secret:") {
			c.certSecret[r.Key] = strings.TrimPrefix(cert, "secret:")
		}
		if c.primary == "" && r.Mode == "site" && host != "" {
			c.primary = host
		}
	}
	return c, nil
}

// vpnHost derives a VPN-only name from the public name, e.g.
// app.example.com -> app.vpn.example.com. The public and VPN DNS zones can
// then be resolved differently without a second catalog host field.
func vpnHost(host, zone string) string {
	suffix := "." + zone
	if zone != "" && strings.HasSuffix(host, suffix) {
		return strings.TrimSuffix(host, suffix) + ".vpn." + zone
	}
	return host
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func certificateAllowed(r catalog.Route, v string) bool {
	for _, a := range r.AllowedCertificates() {
		if a == v || (a == "secret" && strings.HasPrefix(v, "secret:")) || (a == "secret:*" && strings.HasPrefix(v, "secret:")) {
			return true
		}
	}
	return false
}

func (in *Installer) repl(e catalog.Entry, c siteContext) map[string]string {
	return map[string]string{
		"{{ .zone }}":                    c.zone,
		"{{ .host }}":                    c.primary,
		"{{ .release }}":                 e.Name,
		"{{ .namespace }}":               e.Namespace,
		"{{ .storage.configClass }}":     c.configClass,
		"{{ .storage.mediaClass }}":      c.mediaClass,
		"{{ .storage.mediaAccessMode }}": c.mediaAccessMode,
		"{{ .cluster.serviceCIDR }}":     in.cfg.ServiceCIDR,
		"{{ .cluster.podCIDR }}":         in.cfg.PodCIDR,
		"{{ .cluster.dnsServiceIP }}":    in.cfg.DNSServiceIP,
	}
}

// Install installs or upgrades one catalog entry with the given overrides.
func (in *Installer) Install(e catalog.Entry, opts Options) (Result, error) {
	if err := e.Validate(); err != nil {
		return Result{}, err
	}
	if opts.ChartVersion != "" {
		found := false
		for _, v := range e.Chart.Versions {
			if v.Version == opts.ChartVersion {
				found = true
				break
			}
		}
		if len(e.Chart.Versions) == 0 && opts.ChartVersion != e.Chart.Version {
			return Result{}, fmt.Errorf("%s: chart version %s is not available", e.Name, opts.ChartVersion)
		}
		if len(e.Chart.Versions) > 0 && !found {
			return Result{}, fmt.Errorf("%s: chart version %s is not available", e.Name, opts.ChartVersion)
		}
		e.Chart.Version = opts.ChartVersion
	}
	if opts.Upgrade && opts.ChartVersion == "" {
		return Result{}, fmt.Errorf("%s: an upgrade needs an explicit --to chart version", e.Name)
	}
	if opts.Upgrade && !opts.ConfirmBackup && !opts.SkipBackup {
		return Result{}, fmt.Errorf("%s: confirm the pre-upgrade backup (or pass --skip-backup) before upgrading", e.Name)
	}
	if opts.Upgrade {
		st, err := in.Status(e.Name, &e)
		if err != nil {
			return Result{}, err
		}
		if st.Chart == "" {
			return Result{}, fmt.Errorf("%s is not installed; use app install", e.Name)
		}
		if st.Chart == e.Chart.Version {
			return Result{}, fmt.Errorf("%s is already on chart %s", e.Name, e.Chart.Version)
		}
	}
	ns := e.Namespace
	ctx, err := in.context(e, opts)
	if err != nil {
		return Result{}, err
	}
	for _, r := range e.EffectiveRoutes() {
		if name := ctx.certSecret[r.Key]; name != "" {
			if _, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
				"-n", ns, "get", "secret", name, "-o", "name"); err != nil {
				return Result{}, fmt.Errorf("certificate secret %s/%s is missing", ns, name)
			}
		}
	}

	opts.step("creating namespace %s", ns)
	if err := in.apply(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", ns)); err != nil {
		return Result{}, err
	}

	// ConfigMaps are namespaced, and apps that call back into the cluster over
	// HTTPS (an OIDC issuer, for example) need the CA that signs the routes.
	// The traefik role publishes it once in kube-system; copy it here so a
	// catalog entry can mount "umkim-thin-ca" from its own namespace.
	if err := in.ensureCAConfigMap(ns, opts); err != nil {
		return Result{}, err
	}

	// The media stack's bootstrap Secret holds the app passwords the chart's
	// initContainers consume (qBittorrent in particular): create it before the
	// workload starts, or the WebUI keeps its built-in default. The chart API
	// keys are copied in after the install, when that Secret exists.
	if e.Name == ServarrApp {
		opts.step("preparing the media stack credentials")
		if err := in.EnsureServarrSecret(); err != nil {
			return Result{}, err
		}
	}

	// The WireGuard gateway needs its keypair and peer Secret before it starts.
	if proto, _ := e.Exposure["protocol"].(string); proto == "udp" {
		opts.step("preparing the WireGuard gateway")
		if err := wg.New(wg.ConfigFromEnv()).EnsureServer(); err != nil {
			return Result{}, fmt.Errorf("preparing the WireGuard gateway: %w", err)
		}
	}

	if e.Auth.LDAP {
		opts.step("wiring directory authentication")
		if err := in.applyBindSecret(e); err != nil {
			return Result{}, err
		}
	}

	values := in.renderValues(e, ctx)
	if opts.Language != "" {
		if e.Name != "nextcloud" {
			return Result{}, fmt.Errorf("%s does not support an install-time language", e.Name)
		}
		code, err := normalizeAppLanguage(opts.Language)
		if err != nil {
			return Result{}, err
		}
		bootstrap, ok := values["bootstrap"].(map[string]any)
		if !ok {
			bootstrap = map[string]any{}
			values["bootstrap"] = bootstrap
		}
		bootstrap["language"] = code
		opts.Language = code
	}
	if opts.Upgrade {
		if err := in.preserveStorageClasses(e.Name, e.Namespace, values); err != nil {
			return Result{}, err
		}
	}
	if e.Name == ServarrApp {
		// Upgrades keep the operator's current media defaults unless the caller
		// explicitly overrides them. A fresh install starts from the catalog.
		if opts.Upgrade {
			current, err := in.ServarrSettings()
			if err != nil {
				return Result{}, err
			}
			if opts.MediaLanguage == "" {
				opts.MediaLanguage = current.MediaLanguage
			}
			if opts.QualityCutoff == "" {
				opts.QualityCutoff = current.QualityCutoff
			}
		} else if bootstrap, ok := values["bootstrap"].(map[string]any); ok {
			if opts.MediaLanguage == "" {
				if v, ok := bootstrap["mediaLanguage"].(string); ok {
					opts.MediaLanguage = v
				}
			}
			if opts.QualityCutoff == "" {
				if v, ok := bootstrap["qualityCutoff"].(string); ok {
					opts.QualityCutoff = v
				}
			}
		}
		settings, err := NormalizeServarrSettings(ServarrSettings{
			MediaLanguage: opts.MediaLanguage,
			QualityCutoff: opts.QualityCutoff,
		})
		if err != nil {
			return Result{}, err
		}
		opts.MediaLanguage = settings.MediaLanguage
		opts.QualityCutoff = settings.QualityCutoff
		set(values, "bootstrap.mediaLanguage", settings.MediaLanguage)
		set(values, "bootstrap.qualityCutoff", settings.QualityCutoff)
	}
	if e.Auth.LDAP {
		set(values, "auth.ldap", map[string]any{
			"enabled": true,
			"host":    in.cfg.LLDAPHost,
			"port":    in.cfg.LLDAPPort,
			"bindDn":  "uid=svc-bind,ou=people," + in.cfg.BaseDN,
			// Charts name the search base differently: Gitea uses
			// userSearchBase, Nextcloud baseDn, Jellyfin accepts either. Set all
			// of them from the site's directory or a new install silently falls
			// back to the chart's example base (the bind still succeeds, so it
			// looks like a wrong password).
			"userSearchBase":    "ou=people," + in.cfg.BaseDN,
			"baseDn":            "ou=people," + in.cfg.BaseDN,
			"groupBaseDn":       "ou=groups," + in.cfg.BaseDN,
			"existingSecret":    e.Name + "-lldap-bind",
			"existingSecretKey": "LLDAP_LDAP_USER_PASS",
		})
	}

	// OIDC clients and public-path bypass rules live in the identity release;
	// sync them before installing the app so its SSO settings are valid.
	if e.Auth.Kind == "oidc" || len(in.publicPathsFor(e, ctx)) > 0 {
		opts.step("updating the identity release (SSO / public paths)")
	}
	if _, err := in.prepare(e, values, ctx); err != nil {
		return Result{}, err
	}
	// Let the app reach its own public URL (and the portal for OIDC) inside the
	// cluster; we do not use the charts' own Ingress, so we set this ourselves.
	in.applyHostAliases(e, values, ctx)

	raw, err := yaml.Marshal(values)
	if err != nil {
		return Result{}, err
	}
	valuesDir := filepath.Join(in.cfg.StateDir, "apps")
	if err := os.MkdirAll(valuesDir, 0o755); err != nil {
		return Result{}, err
	}
	valuesFile := filepath.Join(valuesDir, e.Name+"-values.yaml")
	// Keep the last successful values in place until Helm succeeds. A failed
	// upgrade used to overwrite this file before Helm rolled back, which made
	// the next attempt unable to recover immutable PVC storage classes.
	tmpValues, err := os.CreateTemp(valuesDir, "."+e.Name+"-values-*.yaml")
	if err != nil {
		return Result{}, err
	}
	tmpValuesName := tmpValues.Name()
	defer os.Remove(tmpValuesName)
	if _, err := tmpValues.Write(raw); err != nil {
		tmpValues.Close()
		return Result{}, err
	}
	if err := tmpValues.Chmod(0o600); err != nil {
		tmpValues.Close()
		return Result{}, err
	}
	if err := tmpValues.Close(); err != nil {
		return Result{}, err
	}

	chart := in.chartRef(e)
	action := "installing"
	if opts.Upgrade {
		action = "upgrading"
	}
	opts.step("%s chart %s (%s)", action, e.Chart.Version, filepath.Base(chart))
	args := []string{"upgrade", "--install", e.Name, chart,
		"--namespace", ns, "--create-namespace",
		"--kubeconfig", in.cfg.Kubeconfig,
		"--values", tmpValuesName,
		"--atomic", "--cleanup-on-fail",
		"--wait", "--timeout", "15m"}
	if strings.HasPrefix(chart, "oci://") {
		args = append(args, "--version", e.Chart.Version)
	}
	if out, err := in.runStream(in.cfg.Helm, args, opts.Progress); err != nil {
		return Result{}, fmt.Errorf("helm: %w\n%s", err, out)
	}
	if err := os.Rename(tmpValuesName, valuesFile); err != nil {
		return Result{}, fmt.Errorf("keeping the rendered values for %s: %w", e.Name, err)
	}

	rc := route.Context{
		Zone:       ctx.zone,
		Release:    e.Name,
		Tenant:     in.cfg.Tenant,
		Middleware: in.cfg.Middleware,
	}
	var hosts []string
	for _, r := range e.EffectiveRoutes() {
		r.Host = ctx.hosts[r.Key] // effective host, tokens already resolved
		rc.Exposure = ctx.exposure[r.Key]
		rc.AllowLocal = ctx.allowLocal[r.Key]
		rc.Certificate = ctx.certificate[r.Key]
		rc.CertSecret = ctx.certSecret[r.Key]
		y, err := route.Render(e, r, rc)
		if err != nil {
			return Result{}, err
		}
		if y == "" {
			continue
		}
		opts.step("publishing https://%s", ctx.hosts[r.Key])
		if err := in.apply(y); err != nil {
			return Result{}, err
		}
		hosts = append(hosts, ctx.hosts[r.Key])
	}

	if e.Name == ServarrApp {
		// Now the chart-owned API keys exist; the bootstrap Job reads them from
		// the same Secret.
		if err := in.EnsureServarrSecret(); err != nil {
			return Result{}, err
		}
	}
	if err := in.saveRoutes(e, ctx); err != nil {
		return Result{}, err
	}

	appVersion := ""
	for _, v := range e.Chart.Versions {
		if v.Version == e.Chart.Version {
			appVersion = v.AppVersion
		}
	}
	return Result{Name: e.Name, Namespace: ns, Chart: e.Chart.Version, AppVersion: appVersion, Routes: hosts}, nil
}

// StorageClass is what the console needs to pick a class: the name, whether
// the cluster marks it default, and the access modes we can vouch for.
//
// Kubernetes does not declare access modes on a StorageClass (they are a PVC
// property), so this is a best-effort answer. An explicit platform mapping wins,
// then the conventions for classes this platform creates (an NFS class is RWX
// even if a particular PVC asked for RWO), then modes observed on bound
// PersistentVolumes for unknown classes. An empty list means "unknown", and the
// console warns rather than inventing a guarantee.
type StorageClass struct {
	Name         string   `json:"name"`
	Default      bool     `json:"default"`
	Provisioner  string   `json:"provisioner,omitempty"`
	AccessModes  []string `json:"accessModes,omitempty"`
	AccessSource string   `json:"accessSource,omitempty"` // observed | configured | known | unknown
}

// StorageClasses lists the cluster's StorageClasses with default/access-mode
// hints for the console's pickers.
func (in *Installer) StorageClasses() ([]StorageClass, error) {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"get", "storageclass", "-o", "json")
	if err != nil {
		return nil, err
	}
	classes, err := parseStorageClasses(out)
	if err != nil {
		return nil, err
	}
	modes := in.observedAccessModes()
	configured := accessModeMap(os.Getenv("UMKIM_THIN_STORAGE_ACCESS_MODES"))
	for i := range classes {
		sc := &classes[i]
		sc.AccessModes, sc.AccessSource = accessModesFor(*sc, modes, configured)
	}
	return classes, nil
}

func normalizeAppLanguage(raw string) (string, error) {
	code := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.IndexAny(code, "-_"); i > 0 {
		code = code[:i]
	}
	if code == "" {
		return "", fmt.Errorf("language is empty")
	}
	for _, option := range servarrLanguageOptions {
		if option.Value == code {
			return code, nil
		}
	}
	return "", fmt.Errorf("unsupported language %q", raw)
}

func accessModesFor(sc StorageClass, observed, configured map[string][]string) ([]string, string) {
	if m := configured[sc.Name]; len(m) > 0 {
		return shortAccessModes(m), "configured"
	}
	if m := knownAccessModes(sc.Name, sc.Provisioner); len(m) > 0 {
		return m, "known"
	}
	if m := observed[sc.Name]; len(m) > 0 {
		return shortAccessModes(m), "observed"
	}
	return nil, "unknown"
}

func shortAccessModes(modes []string) []string {
	out := make([]string, 0, len(modes))
	for _, mode := range modes {
		switch mode {
		case "ReadWriteMany":
			mode = "RWX"
		case "ReadWriteOnce":
			mode = "RWO"
		case "ReadOnlyMany":
			mode = "ROX"
		case "ReadWriteOncePod":
			mode = "RWOP"
		}
		out = append(out, mode)
	}
	return out
}

func parseStorageClasses(out string) ([]StorageClass, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Provisioner string `json:"provisioner"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing storageclasses: %w", err)
	}
	classes := make([]StorageClass, 0, len(list.Items))
	for _, c := range list.Items {
		sc := StorageClass{Name: c.Metadata.Name, Provisioner: c.Provisioner}
		sc.Default = c.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
			c.Metadata.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
		classes = append(classes, sc)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })
	return classes, nil
}

// observedAccessModes aggregates the access modes of bound PVs per class. It is
// the only authoritative source (the PV records what the provisioner actually
// handed out); an empty cluster simply yields nothing.
func (in *Installer) observedAccessModes() map[string][]string {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "get", "pv", "-o", "json")
	if err != nil {
		return nil
	}
	return parsePVAccessModes(out)
}

func parsePVAccessModes(out string) map[string][]string {
	var list struct {
		Items []struct {
			Spec struct {
				StorageClassName string   `json:"storageClassName"`
				AccessModes      []string `json:"accessModes"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return nil
	}
	seen := map[string]map[string]bool{}
	for _, pv := range list.Items {
		if pv.Spec.StorageClassName == "" {
			continue
		}
		if seen[pv.Spec.StorageClassName] == nil {
			seen[pv.Spec.StorageClassName] = map[string]bool{}
		}
		for _, m := range pv.Spec.AccessModes {
			seen[pv.Spec.StorageClassName][m] = true
		}
	}
	modes := map[string][]string{}
	for class, set := range seen {
		for m := range set {
			modes[class] = append(modes[class], m)
		}
		sort.Strings(modes[class])
	}
	return modes
}

func accessModeMap(raw string) map[string][]string {
	out := map[string][]string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, modes, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		var list []string
		for _, m := range strings.Split(modes, "+") {
			if m = strings.TrimSpace(m); m != "" {
				list = append(list, m)
			}
		}
		sort.Strings(list)
		out[strings.TrimSpace(name)] = list
	}
	return out
}

// knownAccessModes encodes the platform's own class conventions. Names win over
// provisioners because Longhorn serves both RWO and RWX from the same driver.
func knownAccessModes(name, provisioner string) []string {
	switch {
	case strings.Contains(name, "rwx"):
		return []string{"RWX"}
	case name == "nfs" || name == "nfs-client" || strings.Contains(provisioner, "nfs"):
		return []string{"RWX"}
	case name == "local-path" || strings.Contains(provisioner, "local-path"):
		return []string{"RWO"}
	case strings.Contains(provisioner, "longhorn"):
		return []string{"RWO"}
	}
	return nil
}

// chartVersion strips Helm's "<release>-" prefix from a list entry.
func chartVersion(release, chart string) string {
	if strings.HasPrefix(chart, release+"-") {
		return strings.TrimPrefix(chart, release+"-")
	}
	return chart
}

// List reports the releases installed from the catalog.
func (in *Installer) List() ([]Result, error) {
	out, err := in.run(in.cfg.Helm, "list", "--all-namespaces", "--output", "json")
	if err != nil {
		return nil, err
	}
	var releases []struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		Chart      string `json:"chart"`
		AppVersion string `json:"app_version"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &releases); err != nil {
		return nil, fmt.Errorf("parsing helm list: %w", err)
	}
	var results []Result
	for _, r := range releases {
		results = append(results, Result{
			Name:       r.Name,
			Namespace:  r.Namespace,
			Chart:      chartVersion(r.Name, r.Chart),
			AppVersion: r.AppVersion,
			HelmStatus: r.Status,
			Routes:     in.routes(r.Namespace),
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results, nil
}

// routes returns the hosts published by the Ingresses in a namespace. The
// installer owns those Ingresses, so this is the app's public surface.
func (in *Installer) routes(ns string) []string {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
		"get", "ingress", "-o", "jsonpath={range .items[*]}{.spec.rules[*].host}{\"\\n\"}{end}")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// Uninstall removes an installed app. With purge it also deletes the namespace
// (and therefore the PVCs and the bind Secret); without it, data is kept so the
// app can be reinstalled.
func (in *Installer) Uninstall(name string, e *catalog.Entry, purge bool) error {
	ns := name
	if e != nil {
		ns = e.Namespace
	}
	var firstErr error
	if out, err := in.run(in.cfg.Helm, "uninstall", name, "--namespace", ns, "--kubeconfig", in.cfg.Kubeconfig); err != nil {
		firstErr = fmt.Errorf("helm uninstall: %w\n%s", err, out)
	}
	if e != nil {
		for _, r := range e.EffectiveRoutes() {
			obj := route.ObjectName(*e, r)
			_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns, "delete", "ingress", obj, "--ignore-not-found")
		}
	}
	if err := in.dropContribution(name); err != nil && firstErr == nil {
		firstErr = err
	}
	if purge {
		_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "delete", "namespace", ns,
			"--ignore-not-found", "--wait=true", "--timeout=300s")
	}
	return firstErr
}

// Component is one workload of an installed app.
type Component struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Ready  string `json:"ready"`
	Status string `json:"status"`
}

// Status is the health of an installed app.
type Status struct {
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace"`
	Chart      string      `json:"chart"`
	AppVersion string      `json:"appVersion,omitempty"`
	HelmStatus string      `json:"helmStatus"`
	Ready      bool        `json:"ready"`
	Routes     []string    `json:"routes"`
	Components []Component `json:"components"`
}

// VersionInfo is the installed and available chart versions for one app.
type VersionInfo struct {
	Name         string                 `json:"name"`
	Installed    string                 `json:"installed,omitempty"`
	InstalledApp string                 `json:"installedAppVersion,omitempty"`
	Stable       string                 `json:"stable"`
	DefaultApp   string                 `json:"defaultAppVersion,omitempty"`
	Versions     []catalog.ChartVersion `json:"versions"`
}

// Versions reports the installed chart and the catalog's available versions.
func (in *Installer) Versions(e catalog.Entry) (VersionInfo, error) {
	st, err := in.Status(e.Name, &e)
	if err != nil {
		return VersionInfo{}, err
	}
	vi := VersionInfo{Name: e.Name, Installed: st.Chart, InstalledApp: st.AppVersion, Stable: e.Chart.Version}
	vi.Versions = append([]catalog.ChartVersion(nil), e.Chart.Versions...)
	if len(vi.Versions) == 0 {
		vi.Versions = []catalog.ChartVersion{{Version: e.Chart.Version, Channel: "stable"}}
	}
	for _, v := range vi.Versions {
		if v.Version == e.Chart.Version {
			vi.DefaultApp = v.AppVersion
		}
	}
	return vi, nil
}

// Status reports an app's Helm state, workloads and published routes.
func (in *Installer) Status(name string, e *catalog.Entry) (Status, error) {
	ns := name
	if e != nil {
		ns = e.Namespace
	}
	st := Status{Name: name, Namespace: ns, Routes: []string{}, Components: []Component{}}

	if out, err := in.run(in.cfg.Helm, "status", name, "--namespace", ns, "--kubeconfig", in.cfg.Kubeconfig, "-o", "json"); err == nil {
		var hs struct {
			Info struct {
				Status string `json:"status"`
			} `json:"info"`
			Chart struct {
				Metadata struct {
					Version    string `json:"version"`
					AppVersion string `json:"appVersion"`
				} `json:"metadata"`
			} `json:"chart"`
		}
		if json.Unmarshal([]byte(out), &hs) == nil {
			st.HelmStatus = hs.Info.Status
			st.Chart = hs.Chart.Metadata.Version
			st.AppVersion = hs.Chart.Metadata.AppVersion
		}
	}

	// `helm status -o json` does not carry the chart metadata in Helm 3.16;
	// fall back to `helm list`, which reports chart and app_version.
	if st.Chart == "" {
		if list, err := in.List(); err == nil {
			for _, r := range list {
				if r.Name == name {
					st.Chart, st.AppVersion = r.Chart, r.AppVersion
					break
				}
			}
		}
	}

	st.Components = append(st.Components, in.workloads(ns, "deploy", "Deployment")...)
	st.Components = append(st.Components, in.workloads(ns, "statefulset", "StatefulSet")...)
	st.Components = append(st.Components, in.workloads(ns, "daemonset", "DaemonSet")...)
	st.Components = append(st.Components, in.workloads(ns, "job", "Job")...)

	st.Routes = append(st.Routes, in.routes(ns)...)
	st.Ready = st.HelmStatus == "deployed"
	for _, c := range st.Components {
		if c.Status != "ready" && c.Status != "complete" {
			st.Ready = false
		}
	}
	return st, nil
}

func (in *Installer) workloads(ns, resource, kind string) []Component {
	path := "{range .items[*]}{.metadata.name}={.status.readyReplicas}/{.spec.replicas}{\"\\n\"}{end}"
	if resource == "job" {
		path = "{range .items[*]}{.metadata.name}={.status.succeeded}/{.spec.completions}{\"\\n\"}{end}"
	}
	if resource == "daemonset" {
		path = "{range .items[*]}{.metadata.name}={.status.numberReady}/{.status.desiredNumberScheduled}{\"\\n\"}{end}"
	}
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns, "get", resource, "-o", "jsonpath="+path)
	if err != nil {
		return nil
	}
	var comps []Component
	for _, line := range strings.Fields(out) {
		name, ready, _ := strings.Cut(line, "=")
		status := "progressing"
		if a, b, ok := frac(ready); ok && a == b {
			status = "ready"
			if resource == "job" {
				status = "complete"
			}
		}
		comps = append(comps, Component{Kind: kind, Name: name, Ready: ready, Status: status})
	}
	return comps
}

// Ready reports whether the cluster API is reachable (the /readyz probe).
func (in *Installer) Ready() error {
	if _, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"get", "--raw", "/readyz", "--request-timeout=5s"); err != nil {
		return fmt.Errorf("kubernetes API not ready: %w", err)
	}
	return nil
}

func frac(s string) (int, int, bool) {
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, false
	}
	return atoi(a), atoi(b), true
}

// runStream runs a command, forwarding each output line to log when set.
func (in *Installer) runStream(name string, args []string, log func(string)) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+in.cfg.Kubeconfig)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var buf strings.Builder
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		buf.WriteString(line + "\n")
		if log != nil {
			log(line)
		}
	}
	if err := cmd.Wait(); err != nil {
		return buf.String(), fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return buf.String(), nil
}

// renderValues applies the site templating context to the entry's values.
func (in *Installer) renderValues(e catalog.Entry, ctx siteContext) map[string]any {
	repl := in.repl(e, ctx)
	out := map[string]any{}
	for k, v := range e.Values {
		out[k] = renderAny(v, repl)
	}
	return out
}

// preserveStorageClasses keeps the previous install's storageClass values on
// an upgrade. PVC storageClassName is immutable, so replacing it with the
// current site default makes Helm fail at apply time even when nothing about
// the app's data needs to change. Prefer the live Helm release values: a failed
// upgrade can overwrite the local values file before Helm rolls back.
func (in *Installer) preserveStorageClasses(name, namespace string, values map[string]any) error {
	if in.cfg.Helm != "" {
		raw, err := in.run(in.cfg.Helm, "get", "values", name, "--namespace", namespace,
			"--kubeconfig", in.cfg.Kubeconfig, "-o", "yaml")
		if err == nil {
			var old map[string]any
			if yaml.Unmarshal([]byte(raw), &old) == nil && len(old) > 0 {
				copyStorageClasses(old, values)
				return nil
			}
		}
	}

	path := filepath.Join(in.cfg.StateDir, "apps", name+"-values.yaml")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading previous values for %s: %w", name, err)
	}
	var old map[string]any
	if err := yaml.Unmarshal(raw, &old); err != nil {
		return fmt.Errorf("parsing previous values for %s: %w", name, err)
	}
	copyStorageClasses(old, values)
	return nil
}

func copyStorageClasses(old, current any) {
	oldMap, ok := old.(map[string]any)
	if !ok {
		return
	}
	currentMap, ok := current.(map[string]any)
	if !ok {
		return
	}
	for key, oldValue := range oldMap {
		if key == "storageClass" {
			if class, ok := oldValue.(string); ok && class != "" {
				currentMap[key] = class
			}
			continue
		}
		if newValue, ok := currentMap[key]; ok {
			copyStorageClasses(oldValue, newValue)
		}
	}
}

// chartRef resolves the chart: a packaged chart from the local mirror when
// present (the private registry is not reachable from the cluster), else the
// catalog's own ref (an OCI URL). The mirror file is named after the chart,
// which is the last segment of the ref (e.g. .../wireguard-gateway).
func (in *Installer) chartRef(e catalog.Entry) string {
	name := e.Name
	if ref := strings.TrimSuffix(e.Chart.Ref, "/"); ref != "" {
		if i := strings.LastIndex(ref, "/"); i >= 0 && i+1 < len(ref) {
			name = ref[i+1:]
		}
	}
	if in.cfg.ChartMirror != "" {
		p := filepath.Join(in.cfg.ChartMirror, fmt.Sprintf("%s-%s.tgz", name, e.Chart.Version))
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return e.Chart.Ref
}

func (in *Installer) applyBindSecret(e catalog.Entry) error {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", in.cfg.IdentityNS, "get", "secret", in.cfg.BindSecret,
		"-o", "jsonpath={.data.password}")
	if err != nil {
		return fmt.Errorf("reading bind account: %w\n%s", err, out)
	}
	pw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil || len(pw) == 0 {
		return fmt.Errorf("bind account secret %s/%s has no password", in.cfg.IdentityNS, in.cfg.BindSecret)
	}
	return in.apply(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s-lldap-bind
  namespace: %s
type: Opaque
stringData:
  LLDAP_LDAP_USER_PASS: %q
`, e.Name, e.Namespace, string(pw)))
}

func (in *Installer) apply(doc string) error {
	cmd := exec.Command(in.cfg.Kubectl, "apply", "-f", "-")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+in.cfg.Kubeconfig)
	cmd.Stdin = strings.NewReader(doc)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubectl apply: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureCAConfigMap copies the cluster CA (published by the traefik role in
// kube-system) into an app's namespace, when it exists. Apps that fetch the
// OIDC issuer over HTTPS mount it and point NODE_EXTRA_CA_CERTS at it; without
// it their TLS client rejects the lab CA. Absent (a cluster whose routes use a
// public certificate) is not an error.
func (in *Installer) ensureCAConfigMap(ns string, opts Options) error {
	ca, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", "kube-system", "get", "configmap", "umkim-thin-ca",
		"-o", `go-template={{index .data "ca.crt"}}`)
	if err != nil || strings.TrimSpace(ca) == "" {
		return nil // no CA published: nothing to trust
	}
	opts.step("publishing the cluster CA in %s", ns)
	return in.apply(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: umkim-thin-ca
  namespace: %s
data:
  ca.crt: %q
`, ns, ca))
}

func (in *Installer) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+in.cfg.Kubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// renderAny replaces templating tokens in strings, maps and lists.
func renderAny(v any, repl map[string]string) any {
	switch t := v.(type) {
	case string:
		for k, val := range repl {
			t = strings.ReplaceAll(t, k, val)
		}
		return t
	case map[string]any:
		for k, vv := range t {
			t[k] = renderAny(vv, repl)
		}
		return t
	case []any:
		for i := range t {
			t[i] = renderAny(t[i], repl)
		}
		return t
	default:
		return v
	}
}

// set assigns value at a path, creating intermediate maps.
func set(root map[string]any, path string, value any) {
	setPath(root, strings.Split(path, "."), value)
}

func setPath(m map[string]any, path []string, value any) {
	if len(path) == 1 {
		m[path[0]] = value
		return
	}
	next, ok := m[path[0]].(map[string]any)
	if !ok {
		next = map[string]any{}
		m[path[0]] = next
	}
	setPath(next, path[1:], value)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// loadConfigFile parses simple KEY=VALUE lines; existing environment wins.
func loadConfigFile(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
}
