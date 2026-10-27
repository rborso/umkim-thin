// Package wg manages WireGuard peers for the in-cluster gateway.
//
// The gateway is a workload in the cluster, not a host daemon: the server key
// and the peer list live in a Secret the gateway loads, and this package writes
// it. Peer addresses are pinned per client so adding a client never renumbers
// the existing ones.
package wg

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Config is everything needed to reach and address the gateway.
type Config struct {
	Kubeconfig string
	Kubectl    string
	Namespace  string
	Secret     string
	Subnet     string // tunnel network, e.g. 192.168.168.0/24
	ServerIP   string // gateway's own address in the tunnel, e.g. 192.168.168.1
	Port       int
	Endpoint   string // host:port clients dial
	DNS        string
	AllowedIPs []string // extra networks clients route over the tunnel
	StateFile  string
}

// ConfigFromEnv builds the config from the environment (and config.env).
func ConfigFromEnv() Config {
	loadConfigFile("/etc/umkim-thin/config.env")
	stateDir := env("UMKIM_THIN_STATE_DIR", "/etc/umkim-thin")
	extra := splitCSV(env("UMKIM_THIN_WG_ALLOWED_IPS", ""))
	return Config{
		Kubeconfig: env("UMKIM_THIN_KUBECONFIG", "/etc/rancher/k3s/k3s.yaml"),
		Kubectl:    env("UMKIM_THIN_KUBECTL", "/usr/local/bin/kubectl"),
		Namespace:  env("UMKIM_THIN_WG_NAMESPACE", "wireguard"),
		Secret:     env("UMKIM_THIN_WG_SECRET", "wireguard-peers"),
		Subnet:     env("UMKIM_THIN_WG_SUBNET", "192.168.168.0/24"),
		ServerIP:   env("UMKIM_THIN_WG_SERVER_IP", "192.168.168.1"),
		Port:       atoi(env("UMKIM_THIN_WG_PORT", "51820")),
		Endpoint:   env("UMKIM_THIN_WG_ENDPOINT", ""),
		DNS:        env("UMKIM_THIN_DNS_IP", "10.43.0.10"),
		AllowedIPs: extra,
		StateFile:  env("UMKIM_THIN_WG_STATE", stateDir+"/wg-clients.json"),
	}
}

// Manager reads and writes the peers Secret.
type Manager struct{ cfg Config }

// New returns a manager.
func New(cfg Config) *Manager { return &Manager{cfg: cfg} }

// Peer is one client's public identity.
type Peer struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	PublicKey string `json:"publicKey"`
}

// Client is a created peer plus the config the client uses.
type Client struct {
	Peer
	Config string `json:"config"`
}

type peerRec struct {
	PublicKey string `json:"publicKey"`
	Address   string `json:"address"`
}

type state struct {
	ServerPrivate string             `json:"serverPrivate"`
	ServerPublic  string             `json:"serverPublic"`
	Clients       map[string]peerRec `json:"clients"`
}

func (m *Manager) load() (*state, error) {
	st := &state{Clients: map[string]peerRec{}}
	raw, err := os.ReadFile(m.cfg.StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	if st.Clients == nil {
		st.Clients = map[string]peerRec{}
	}
	return st, nil
}

func (m *Manager) save(st *state) error {
	if err := ensureServer(st); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.cfg.StateFile, raw, 0o600); err != nil {
		return err
	}
	return m.writeSecret(st)
}

// ensureServer creates the gateway keypair once.
func ensureServer(st *state) error {
	if st.ServerPrivate != "" {
		return nil
	}
	priv, pub, err := keypair()
	if err != nil {
		return err
	}
	st.ServerPrivate, st.ServerPublic = priv, pub
	return nil
}

// EnsureServer makes sure the gateway Secret exists (called at install).
func (m *Manager) EnsureServer() error {
	st, err := m.load()
	if err != nil {
		return err
	}
	return m.save(st)
}

// Create adds a client and returns its config.
func (m *Manager) Create(name string) (Client, error) {
	if name == "" {
		return Client{}, fmt.Errorf("a client name is required")
	}
	st, err := m.load()
	if err != nil {
		return Client{}, err
	}
	if _, ok := st.Clients[name]; ok {
		return Client{}, fmt.Errorf("client %q already exists", name)
	}
	addr, err := m.nextAddress(st)
	if err != nil {
		return Client{}, err
	}
	priv, pub, err := keypair()
	if err != nil {
		return Client{}, err
	}
	st.Clients[name] = peerRec{PublicKey: pub, Address: addr}
	if err := m.save(st); err != nil {
		return Client{}, err
	}
	return Client{
		Peer:   Peer{Name: name, Address: addr, PublicKey: pub},
		Config: m.clientConfig(priv, addr, st.ServerPublic),
	}, nil
}

// Delete revokes a client: it is removed from the peer list, so the gateway
// stops accepting it.
func (m *Manager) Delete(name string) error {
	st, err := m.load()
	if err != nil {
		return err
	}
	if _, ok := st.Clients[name]; !ok {
		return fmt.Errorf("no client %q", name)
	}
	delete(st.Clients, name)
	return m.save(st)
}

// List returns the existing peers.
func (m *Manager) List() ([]Peer, error) {
	st, err := m.load()
	if err != nil {
		return nil, err
	}
	var out []Peer
	for name, c := range st.Clients {
		out = append(out, Peer{Name: name, Address: c.Address, PublicKey: c.PublicKey})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// nextAddress returns the lowest free host address in the tunnel subnet.
func (m *Manager) nextAddress(st *state) (string, error) {
	prefix, err := netip.ParsePrefix(m.cfg.Subnet)
	if err != nil {
		return "", fmt.Errorf("bad tunnel subnet %q: %w", m.cfg.Subnet, err)
	}
	used := map[netip.Addr]bool{}
	if a, err := netip.ParseAddr(m.cfg.ServerIP); err == nil {
		used[a] = true
	}
	for _, c := range st.Clients {
		a := c.Address
		if i := strings.IndexByte(a, '/'); i >= 0 {
			a = a[:i]
		}
		if ip, err := netip.ParseAddr(a); err == nil {
			used[ip] = true
		}
	}
	if !prefix.Addr().Is4() {
		return "", fmt.Errorf("tunnel subnet %q must be IPv4", m.cfg.Subnet)
	}
	// Walk the usable host range: skip the network address and, for a subnet
	// with a broadcast address, the broadcast address too. This keeps the
	// allocator correct for anything from /30 to /24 (the default), not just
	// the last octet.
	base := prefix.Masked().Addr()
	last := lastIPv4(prefix)
	for cand := base.Next(); cand.IsValid() && prefix.Contains(cand); cand = cand.Next() {
		if cand == last && prefix.Bits() < 31 {
			break
		}
		if !used[cand] {
			return cand.String(), nil
		}
	}
	return "", fmt.Errorf("tunnel subnet %s is full", m.cfg.Subnet)
}

// lastIPv4 returns the last address in an IPv4 prefix.
func lastIPv4(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(b[:])
	if hostBits := 32 - p.Bits(); hostBits > 0 {
		v |= (uint32(1) << hostBits) - 1
	}
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	return netip.AddrFrom4(out)
}

func (m *Manager) clientConfig(priv, addr, serverPub string) string {
	allowed := append([]string{m.cfg.Subnet}, m.cfg.AllowedIPs...)
	var b strings.Builder
	fmt.Fprintf(&b, "[Interface]\nPrivateKey = %s\nAddress = %s/32\n", priv, addr)
	if m.cfg.DNS != "" {
		fmt.Fprintf(&b, "DNS = %s\n", m.cfg.DNS)
	}
	fmt.Fprintf(&b, "\n[Peer]\nPublicKey = %s\n", serverPub)
	if m.cfg.Endpoint != "" {
		fmt.Fprintf(&b, "Endpoint = %s\n", m.cfg.Endpoint)
	}
	fmt.Fprintf(&b, "AllowedIPs = %s\nPersistentKeepalive = 25\n", strings.Join(allowed, ", "))
	return b.String()
}

// writeSecret renders the gateway Secret: its key, its address, and the peers.
func (m *Manager) writeSecret(st *state) error {
	prefix, err := netip.ParsePrefix(m.cfg.Subnet)
	if err != nil {
		return err
	}
	var peers strings.Builder
	for _, name := range sortedNames(st.Clients) {
		c := st.Clients[name]
		fmt.Fprintf(&peers, "%s %s/32\n", c.PublicKey, c.Address)
	}
	doc := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  server-private-key: %q
  server-public-key: %q
  server-address: %q
  peers: %q
`, m.cfg.Secret, m.cfg.Namespace, st.ServerPrivate, st.ServerPublic,
		fmt.Sprintf("%s/%d", m.cfg.ServerIP, prefix.Bits()), peers.String())

	cmd := exec.Command(m.cfg.Kubectl, "--kubeconfig", m.cfg.Kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(doc)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing %s/%s: %v: %s", m.cfg.Namespace, m.cfg.Secret, err, strings.TrimSpace(string(out)))
	}
	// A mounted Secret can take a minute to propagate; nudge the gateway so a
	// new peer is usable immediately. Best effort: it may not be installed yet.
	restart := exec.Command(m.cfg.Kubectl, "--kubeconfig", m.cfg.Kubeconfig,
		"-n", m.cfg.Namespace, "rollout", "restart", "daemonset/wireguard-gateway")
	_ = restart.Run()
	return nil
}

// keypair returns a WireGuard private/public keypair (base64).
func keypair() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", "", err
	}
	return encode(raw), encode(k.PublicKey().Bytes()), nil
}

func encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortedNames(m map[string]peerRec) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	n := 0
	fmt.Sscanf(s, "%d", &n)
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
