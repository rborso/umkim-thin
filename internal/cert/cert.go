// Package cert validates and stores manual TLS certificates.
//
// A manual certificate is a Kubernetes TLS Secret plus metadata. Per-route
// certificates live in the app namespace (Ingress tls.secretName); the default
// wildcard lives in the Traefik namespace and is wired into Traefik's default
// TLS store through the identity file-provider ConfigMap.
package cert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Info is certificate metadata. It never contains the private key.
type Info struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Hosts     []string  `json:"hosts"`
	Issuer    string    `json:"issuer,omitempty"`
	NotAfter  time.Time `json:"notAfter"`
	Default   bool      `json:"default,omitempty"`
}

// Parse validates a PEM certificate/key pair and returns its metadata.
func Parse(certPEM, keyPEM []byte, hosts []string) (Info, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return Info{}, fmt.Errorf("certificate is not PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Info{}, fmt.Errorf("parsing certificate: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return Info{}, fmt.Errorf("private key is not PEM")
	}
	switch kb.Type {
	case "RSA PRIVATE KEY":
		if _, err := x509.ParsePKCS1PrivateKey(kb.Bytes); err != nil {
			return Info{}, fmt.Errorf("parsing RSA key: %w", err)
		}
	case "EC PRIVATE KEY":
		if _, err := x509.ParseECPrivateKey(kb.Bytes); err != nil {
			return Info{}, fmt.Errorf("parsing EC key: %w", err)
		}
	case "PRIVATE KEY":
		if _, err := x509.ParsePKCS8PrivateKey(kb.Bytes); err != nil {
			return Info{}, fmt.Errorf("parsing PKCS8 key: %w", err)
		}
	default:
		return Info{}, fmt.Errorf("unsupported private key type %q", kb.Type)
	}
	// tls.X509KeyPair checks that the key belongs to the certificate.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return Info{}, fmt.Errorf("certificate/key mismatch: %w", err)
	}
	if time.Now().After(c.NotAfter) {
		return Info{}, fmt.Errorf("certificate expired at %s", c.NotAfter.Format(time.RFC3339))
	}
	all := append([]string(nil), hosts...)
	if len(all) == 0 {
		all = append(all, c.DNSNames...)
		if c.Subject.CommonName != "" {
			all = append(all, c.Subject.CommonName)
		}
	}
	all = compact(all)
	if len(all) == 0 {
		return Info{}, fmt.Errorf("certificate has no DNS names")
	}
	for _, h := range all {
		if !Covers(c, h) {
			return Info{}, fmt.Errorf("certificate does not cover %s", h)
		}
	}
	info := Info{Hosts: all, NotAfter: c.NotAfter, Issuer: c.Issuer.CommonName}
	return info, nil
}

// Covers reports whether the certificate covers host, including wildcards.
func Covers(c *x509.Certificate, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if net.ParseIP(host) == nil {
		if c.VerifyHostname(host) == nil {
			return true
		}
	}
	for _, n := range c.DNSNames {
		if strings.EqualFold(n, host) {
			return true
		}
	}
	return strings.EqualFold(c.Subject.CommonName, host)
}

func compact(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(strings.ToLower(v))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// Config is the store's kubectl/helm context.
type Config struct {
	Kubectl        string
	Kubeconfig     string
	TraefikNS      string
	Helm           string
	IdentityNS     string
	IdentityChart  string
	IdentityValues string
	IdentityApps   string
	IdentityCerts  string
}

// Store manages TLS Secrets and the default-certificate values.
type Store struct{ cfg Config }

// New returns a certificate store.
func New(cfg Config) *Store {
	if cfg.Kubectl == "" {
		cfg.Kubectl = "kubectl"
	}
	if cfg.Kubeconfig == "" {
		cfg.Kubeconfig = "/etc/rancher/k3s/k3s.yaml"
	}
	if cfg.TraefikNS == "" {
		cfg.TraefikNS = "kube-system"
	}
	if cfg.Helm == "" {
		cfg.Helm = "helm"
	}
	if cfg.IdentityNS == "" {
		cfg.IdentityNS = "identity"
	}
	if cfg.IdentityCerts == "" {
		cfg.IdentityCerts = "/etc/umkim-thin/identity-certs.yml"
	}
	return &Store{cfg: cfg}
}

// Add stores a TLS Secret and validates the pair before writing anything.
func (s *Store) Add(name, namespace string, certPEM, keyPEM []byte, hosts []string, setDefault bool) (Info, error) {
	info, err := Parse(certPEM, keyPEM, hosts)
	if err != nil {
		return Info{}, err
	}
	if namespace == "" {
		namespace = s.cfg.TraefikNS
	}
	if setDefault && namespace != s.cfg.TraefikNS {
		return Info{}, fmt.Errorf("the default certificate must live in %s (Traefik's namespace)", s.cfg.TraefikNS)
	}
	if name == "" {
		return Info{}, fmt.Errorf("certificate name is required")
	}
	info.Name, info.Namespace, info.Default = name, namespace, setDefault
	hostsJSON, _ := json.Marshal(info.Hosts)
	doc := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
  labels:
    umkim-thin.io/certificate: "true"
  annotations:
    umkim-thin.io/hosts: %q
    umkim-thin.io/not-after: %q
    umkim-thin.io/default: %q
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
`, name, namespace, string(hostsJSON), info.NotAfter.Format(time.RFC3339), fmt.Sprintf("%t", setDefault),
		base64.StdEncoding.EncodeToString(certPEM), base64.StdEncoding.EncodeToString(keyPEM))
	if _, err := s.kubectlInput(doc); err != nil {
		return Info{}, err
	}
	if setDefault {
		if err := s.writeDefaultValues(name); err != nil {
			return Info{}, err
		}
	}
	return info, nil
}

// List returns certificate metadata across namespaces.
func (s *Store) List() ([]Info, error) {
	out, err := s.run(s.cfg.Kubectl, "--kubeconfig", s.cfg.Kubeconfig, "get", "secret", "-A",
		"-l", "umkim-thin.io/certificate=true", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	var infos []Info
	for _, it := range list.Items {
		var hosts []string
		_ = json.Unmarshal([]byte(it.Metadata.Annotations["umkim-thin.io/hosts"]), &hosts)
		notAfter, _ := time.Parse(time.RFC3339, it.Metadata.Annotations["umkim-thin.io/not-after"])
		defaultCert := it.Metadata.Annotations["umkim-thin.io/default"] == "true"
		infos = append(infos, Info{Name: it.Metadata.Name, Namespace: it.Metadata.Namespace,
			Hosts: hosts, NotAfter: notAfter, Default: defaultCert})
	}
	return infos, nil
}

// Remove deletes a certificate Secret.
func (s *Store) Remove(name, namespace string) error {
	if namespace == "" {
		namespace = s.cfg.TraefikNS
	}
	_, err := s.run(s.cfg.Kubectl, "--kubeconfig", s.cfg.Kubeconfig, "-n", namespace,
		"delete", "secret", name, "--ignore-not-found")
	return err
}

// SetDefault points Traefik's default TLS store at a Secret in its namespace.
func (s *Store) SetDefault(name string) error {
	if _, err := s.run(s.cfg.Kubectl, "--kubeconfig", s.cfg.Kubeconfig, "-n", s.cfg.TraefikNS,
		"get", "secret", name, "-o", "name"); err != nil {
		return fmt.Errorf("default certificate %s/%s: %w", s.cfg.TraefikNS, name, err)
	}
	return s.writeDefaultValues(name)
}

func (s *Store) writeDefaultValues(name string) error {
	doc := map[string]any{"traefik": map[string]any{"defaultCertificateSecret": name}}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.IdentityCerts), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.cfg.IdentityCerts, raw, 0o600); err != nil {
		return err
	}
	args := []string{"upgrade", "--install", "identity", s.cfg.IdentityChart,
		"--namespace", s.cfg.IdentityNS, "--kubeconfig", s.cfg.Kubeconfig,
		"--values", s.cfg.IdentityValues}
	if _, err := os.Stat(s.cfg.IdentityApps); err == nil {
		args = append(args, "--values", s.cfg.IdentityApps)
	}
	args = append(args, "--values", s.cfg.IdentityCerts, "--wait", "--timeout", "10m")
	_, err = s.run(s.cfg.Helm, args...)
	return err
}

func (s *Store) kubectlInput(doc string) (string, error) {
	return s.runInput(doc, s.cfg.Kubectl, "--kubeconfig", s.cfg.Kubeconfig, "apply", "-f", "-")
}

func (s *Store) run(name string, args ...string) (string, error) {
	cmd := execCommand(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

func (s *Store) runInput(input, name string, args ...string) (string, error) {
	cmd := execCommand(name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// execCommand is a variable so tests can observe command invocations.
var execCommand = func(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
