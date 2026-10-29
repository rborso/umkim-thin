package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/rborso/umkim-thin/internal/catalog"
	"gopkg.in/yaml.v3"
)

// autheliaClient is one OIDC client in the identity release's values (the shape
// charts/identity renders into Authelia's identity_providers.oidc.clients).
type autheliaClient struct {
	ClientID                string   `yaml:"clientId"`
	ClientName              string   `yaml:"clientName"`
	ClientSecret            string   `yaml:"clientSecret"`
	AuthorizationPolicy     string   `yaml:"authorizationPolicy"`
	RequirePKCE             *bool    `yaml:"requirePkce,omitempty"`
	RedirectURIs            []string `yaml:"redirectUris"`
	Scopes                  []string `yaml:"scopes,omitempty"`
	ConsentMode             string   `yaml:"consentMode,omitempty"`
	TokenEndpointAuthMethod string   `yaml:"tokenEndpointAuthMethod,omitempty"`
}

// publicPathRule exempts a host's paths from the session challenge.
type publicPathRule struct {
	Domain string   `yaml:"domain"`
	Paths  []string `yaml:"paths"`
}

// routeSelection is the per-route install state that an upgrade must preserve.
type routeSelection struct {
	Exposure    string `json:"exposure"`
	AllowLocal  bool   `json:"allowLocal,omitempty"`
	Certificate string `json:"certificate"`
	Host        string `json:"host,omitempty"`
	VPNHost     string `json:"vpnHost,omitempty"`
	TrustedVPN  bool   `json:"trustedVpn,omitempty"`
	SameHost    bool   `json:"sameHost,omitempty"`
}

// contribution is what one app adds to the identity release.
type contribution struct {
	OIDC        *autheliaClient           `json:"oidc,omitempty"`
	Namespace   string                    `json:"namespace,omitempty"`
	PublicPaths []publicPathRule          `json:"publicPaths,omitempty"`
	Routes      map[string]routeSelection `json:"routes,omitempty"`
}

type contributions map[string]contribution

// identityValues is the app-contributed slice of the identity chart's values.
type identityValues struct {
	Authelia struct {
		OIDC struct {
			Enabled          bool             `yaml:"enabled"`
			HMACSecret       string           `yaml:"hmacSecret,omitempty"`
			IssuerPrivateKey string           `yaml:"issuerPrivateKey,omitempty"`
			Clients          []autheliaClient `yaml:"clients,omitempty"`
		} `yaml:"oidc"`
		PublicPathRules []publicPathRule `yaml:"publicPathRules,omitempty"`
	} `yaml:"authelia"`
}

func (in *Installer) loadContributions() contributions {
	c := contributions{}
	raw, err := os.ReadFile(in.cfg.AppsState)
	if err == nil {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

func (in *Installer) saveContributions(c contributions) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(in.cfg.AppsState, raw, 0o600)
}

// clientFor builds the OIDC client for an entry, reusing the secret already on
// file so re-installs do not invalidate the app's sessions.
func (in *Installer) clientFor(e catalog.Entry, c contributions, repl map[string]string) (*autheliaClient, error) {
	if e.OIDC == nil || e.OIDC.ClientID == "" {
		return nil, fmt.Errorf("%s: oidc auth needs oidc.clientId", e.Name)
	}
	client := &autheliaClient{
		ClientID:                e.OIDC.ClientID,
		ClientName:              firstNonEmpty(e.DisplayName, e.OIDC.ClientID),
		AuthorizationPolicy:     "one_factor",
		RequirePKCE:             e.OIDC.RequirePKCE,
		RedirectURIs:            renderStrings(e.OIDC.RedirectURIs, repl),
		Scopes:                  []string{"openid", "profile", "email"},
		ConsentMode:             "implicit",
		TokenEndpointAuthMethod: "client_secret_basic",
	}
	if len(e.OIDC.Scopes) > 0 {
		client.Scopes = e.OIDC.Scopes
	}
	if prev, ok := c[e.Name]; ok && prev.OIDC != nil && prev.OIDC.ClientID == client.ClientID {
		client.ClientSecret = prev.OIDC.ClientSecret
	}
	if client.ClientSecret == "" {
		secret, err := randomSecret()
		if err != nil {
			return nil, err
		}
		client.ClientSecret = secret
	}
	return client, nil
}

// publicPathsFor collects the bypass rules an entry declares.
func (in *Installer) publicPathsFor(e catalog.Entry, ctx siteContext) []publicPathRule {
	var rules []publicPathRule
	for _, r := range e.EffectiveRoutes() {
		exp := ctx.exposure[r.Key]
		if exp == "" {
			exp = r.DefaultExposure()
		}
		if exp == "authelia" && len(r.PublicPaths) > 0 {
			rules = append(rules, publicPathRule{
				Domain: ctx.hosts[r.Key],
				Paths:  r.PublicPaths,
			})
		}
	}
	return rules
}

// syncIdentity writes the app-contributed identity values and, when they
// changed, upgrades the identity release so Authelia picks them up.
func (in *Installer) syncIdentity(c contributions) error {
	iv := identityValues{}
	var clients []autheliaClient
	var rules []publicPathRule
	for _, name := range sortedKeys(c) {
		contrib := c[name]
		if contrib.OIDC != nil {
			// Authelia accepts PBKDF2 hashes for client secrets; the app still
			// receives the plaintext secret in its own chart values/Secret.
			hashed, err := hashClientSecret(contrib.OIDC.ClientSecret)
			if err != nil {
				return err
			}
			client := *contrib.OIDC
			client.ClientSecret = hashed
			clients = append(clients, client)
		}
		rules = append(rules, contrib.PublicPaths...)
	}
	if len(clients) > 0 {
		hmac, err := os.ReadFile(in.cfg.OIDCHmacFile)
		if err != nil {
			return fmt.Errorf("reading OIDC hmac: %w", err)
		}
		key, err := os.ReadFile(in.cfg.OIDCKeyFile)
		if err != nil {
			return fmt.Errorf("reading OIDC signing key: %w", err)
		}
		iv.Authelia.OIDC.Enabled = true
		iv.Authelia.OIDC.HMACSecret = strings.TrimSpace(string(hmac))
		iv.Authelia.OIDC.IssuerPrivateKey = string(key)
		iv.Authelia.OIDC.Clients = clients
	}
	iv.Authelia.PublicPathRules = rules

	raw, err := yaml.Marshal(iv)
	if err != nil {
		return err
	}
	if existing, err := os.ReadFile(in.cfg.IdentityApps); err == nil && string(existing) == string(raw) {
		return nil
	}
	if err := os.WriteFile(in.cfg.IdentityApps, raw, 0o600); err != nil {
		return err
	}
	args := []string{"upgrade", "--install", "identity", in.cfg.IdentityChart,
		"--namespace", "identity",
		"--kubeconfig", in.cfg.Kubeconfig,
		"--values", in.cfg.IdentityValues,
		"--values", in.cfg.IdentityApps,
		"--wait", "--timeout", "15m"}
	if out, err := in.run(in.cfg.Helm, args...); err != nil {
		return fmt.Errorf("updating identity release: %w\n%s", err, out)
	}
	return nil
}

// dropContribution forgets an app's identity additions, syncing the release.
func (in *Installer) dropContribution(name string) error {
	c := in.loadContributions()
	if _, ok := c[name]; !ok {
		return nil
	}
	delete(c, name)
	if err := in.saveContributions(c); err != nil {
		return err
	}
	return in.syncIdentity(c)
}

// saveInstalledNamespace records the namespace as soon as Helm has accepted
// the release, so a later route failure cannot make status/uninstall look in
// the catalog namespace again.
func (in *Installer) saveInstalledNamespace(name, namespace string) error {
	c := in.loadContributions()
	contrib := c[name]
	contrib.Namespace = namespace
	c[name] = contrib
	return in.saveContributions(c)
}

// saveRoutes records the effective exposure/certificate choices after a
// successful install. A failed install must not overwrite the previous state.
func (in *Installer) saveRoutes(e catalog.Entry, ctx siteContext) error {
	c := in.loadContributions()
	contrib := c[e.Name]
	contrib.Namespace = e.Namespace
	contrib.Routes = map[string]routeSelection{}
	for _, r := range e.EffectiveRoutes() {
		contrib.Routes[r.Key] = routeSelection{
			Exposure:    ctx.exposure[r.Key],
			AllowLocal:  ctx.allowLocal[r.Key],
			Certificate: ctx.certificate[r.Key],
			Host:        ctx.hosts[r.Key],
			VPNHost:     ctx.vpnHosts[r.Key],
			TrustedVPN:  ctx.trustedVPN[r.Key],
			SameHost:    ctx.trustedSameHost[r.Key],
		}
	}
	c[e.Name] = contrib
	return in.saveContributions(c)
}

// prepare builds the app's contribution (OIDC client, bypass rules) and applies
// the OIDC wiring to the chart values.
func (in *Installer) prepare(e catalog.Entry, values map[string]any, ctx siteContext) (*autheliaClient, error) {
	c := in.loadContributions()

	var client *autheliaClient
	if e.Auth.Kind == "oidc" {
		var err error
		client, err = in.clientFor(e, c, in.repl(e, ctx))
		if err != nil {
			return nil, err
		}
		in.applyOIDCValues(e, values, client)
	}

	prev := c[e.Name]
	contrib := contribution{
		OIDC:        client,
		PublicPaths: in.publicPathsFor(e, ctx),
		Routes:      prev.Routes,
	}
	if contrib.OIDC != nil || len(contrib.PublicPaths) > 0 {
		c[e.Name] = contrib
		if err := in.saveContributions(c); err != nil {
			return nil, err
		}
		if err := in.syncIdentity(c); err != nil {
			return nil, err
		}
	}
	return client, nil
}

// applyOIDCValues maps an entry's client onto the chart's SSO settings. The
// charts are umkim-tailored, so the exact keys differ per app.
func (in *Installer) applyOIDCValues(e catalog.Entry, values map[string]any, client *autheliaClient) {
	issuer := in.cfg.OIDCIssuer
	switch e.Name {
	case "immich":
		set(values, "bootstrap.oauth", map[string]any{
			"enabled":      true,
			"issuerUrl":    issuer,
			"clientId":     client.ClientID,
			"clientSecret": client.ClientSecret,
			"scope":        "openid email profile",
		})
	case "paperless":
		set(values, "oidc", map[string]any{
			"enabled":            true,
			"providerId":         "authelia",
			"providerName":       "Authelia",
			"clientId":           client.ClientID,
			"clientSecret":       client.ClientSecret,
			"issuerUrl":          issuer,
			"tokenAuthMethod":    "client_secret_basic",
			"redirectLoginToSso": true,
		})
	case "gitea":
		set(values, "auth.oidc", map[string]any{
			"enabled":      true,
			"name":         "authelia",
			"clientId":     client.ClientID,
			"clientSecret": client.ClientSecret,
			"issuerUrl":    issuer,
			"discoveryUrl": issuer + "/.well-known/openid-configuration",
			"scopes":       "openid email profile",
		})
	case "nextcloud":
		set(values, "oidc", map[string]any{
			"enabled":            true,
			"providerId":         "authelia",
			"providerName":       "Authelia",
			"clientId":           client.ClientID,
			"clientSecret":       client.ClientSecret,
			"issuerUrl":          issuer,
			"scopes":             "openid email profile",
			"mappingUid":         "preferred_username",
			"mappingEmail":       "email",
			"mappingDisplayName": "name",
		})
	default:
		set(values, "oidc", map[string]any{
			"enabled":      true,
			"clientId":     client.ClientID,
			"clientSecret": client.ClientSecret,
			"issuerUrl":    issuer,
		})
	}
}

// applyHostAliases points the app's own public hosts (and the portal, for OIDC)
// at Traefik inside the cluster, so self-referential calls resolve.
func (in *Installer) applyHostAliases(e catalog.Entry, values map[string]any, ctx siteContext) {
	ip := in.traefikClusterIP()
	if ip == "" {
		return
	}
	var hosts []any
	for _, r := range e.EffectiveRoutes() {
		if r.Mode == "site" && ctx.hosts[r.Key] != "" {
			hosts = append(hosts, ctx.hosts[r.Key])
		}
	}
	if e.Auth.Kind == "oidc" && in.cfg.PortalHost != "" {
		hosts = append(hosts, in.cfg.PortalHost)
	}
	if len(hosts) == 0 {
		return
	}
	set(values, "hostAliases", []any{map[string]any{"ip": ip, "hostnames": hosts}})
}

func (in *Installer) traefikClusterIP() string {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", in.cfg.TraefikNS, "get", "svc", in.cfg.TraefikSvc,
		"-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(out)
	if ip == "" || ip == "None" {
		return ""
	}
	return ip
}

func renderStrings(in []string, repl map[string]string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		for k, v := range repl {
			s = strings.ReplaceAll(s, k, v)
		}
		out = append(out, s)
	}
	return out
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pbkdf2Iterations matches Authelia's PBKDF2-SHA512 default. The format is the
// one produced by `authelia crypto hash generate pbkdf2 --variant sha512`:
// $pbkdf2-sha512$<iterations>$<salt>$<digest>, with raw (unpadded) base64.
const pbkdf2Iterations = 310000

func hashClientSecret(secret string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return hashClientSecretWithSalt(secret, salt), nil
}

func hashClientSecretWithSalt(secret string, salt []byte) string {
	digest := pbkdf2SHA512([]byte(secret), salt, pbkdf2Iterations, 64)
	// go-crypt (which Authelia uses for PasswordDigest) encodes PBKDF2 salt and
	// key with its adapted base64 alphabet: standard base64 with `+` replaced by
	// `.`, unpadded. URL-safe `-`/`_` and standard `+` are both rejected.
	adapted := func(b []byte) string {
		return strings.ReplaceAll(base64.RawStdEncoding.EncodeToString(b), "+", ".")
	}
	return fmt.Sprintf("$pbkdf2-sha512$%d$%s$%s",
		pbkdf2Iterations, adapted(salt), adapted(digest))
}

// pbkdf2SHA512 is a small, dependency-free PBKDF2-HMAC-SHA512. The standard
// library does not ship PBKDF2 and the app only needs this one construction.
func pbkdf2SHA512(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha512.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	var counter [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		prf.Write(counter[:])
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func sortedKeys(c contributions) []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
