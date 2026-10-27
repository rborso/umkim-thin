package wg

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManager(subnet, server string) *Manager {
	return New(Config{
		Subnet:     subnet,
		ServerIP:   server,
		DNS:        "10.43.0.10",
		Endpoint:   "vpn.example.com:51820",
		AllowedIPs: []string{"10.43.0.0/16"},
	})
}

func TestNextAddress(t *testing.T) {
	m := testManager("192.168.168.0/24", "192.168.168.1")

	st := &state{Clients: map[string]peerRec{}}
	got, err := m.nextAddress(st)
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.168.2" {
		t.Fatalf("first address = %q, want 192.168.168.2", got)
	}

	st.Clients["laptop"] = peerRec{Address: "192.168.168.2"}
	st.Clients["phone"] = peerRec{Address: "192.168.168.3/32"}
	got, err = m.nextAddress(st)
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.168.4" {
		t.Fatalf("next address = %q, want 192.168.168.4", got)
	}
}

func TestNextAddressRejectsBadSubnet(t *testing.T) {
	m := testManager("not-a-subnet", "192.168.168.1")
	if _, err := m.nextAddress(&state{Clients: map[string]peerRec{}}); err == nil {
		t.Fatal("nextAddress accepted an invalid subnet")
	}
}

func TestNextAddressFullSubnet(t *testing.T) {
	m := testManager("192.168.168.0/30", "192.168.168.1")
	// A /30 has room for .1 (server) and .2 only.
	st := &state{Clients: map[string]peerRec{"one": {Address: "192.168.168.2"}}}
	if _, err := m.nextAddress(st); err == nil {
		t.Fatal("nextAddress did not report a full subnet")
	}
}

func TestClientConfig(t *testing.T) {
	m := testManager("192.168.168.0/24", "192.168.168.1")
	cfg := m.clientConfig("private-key", "192.168.168.2", "server-public-key")
	for _, want := range []string{
		"PrivateKey = private-key",
		"Address = 192.168.168.2/32",
		"DNS = 10.43.0.10",
		"PublicKey = server-public-key",
		"Endpoint = vpn.example.com:51820",
		"AllowedIPs = 192.168.168.0/24, 10.43.0.0/16",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("client config missing %q:\n%s", want, cfg)
		}
	}
}

func TestKeypair(t *testing.T) {
	priv, pub, err := keypair()
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"private": priv, "public": pub} {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			t.Fatalf("%s key is not base64: %v", name, err)
		}
		if len(raw) != 32 {
			t.Fatalf("%s key is %d bytes, want 32", name, len(raw))
		}
	}
	if priv == pub {
		t.Fatal("private and public keys are identical")
	}
	priv2, _, err := keypair()
	if err != nil {
		t.Fatal(err)
	}
	if priv == priv2 {
		t.Fatal("keypair returned the same private key twice")
	}
}

func TestLoadState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	m := testManager("192.168.168.0/24", "192.168.168.1")
	m.cfg.StateFile = path

	st, err := m.load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Clients == nil || len(st.Clients) != 0 {
		t.Fatalf("a missing state file should give an empty state: %+v", st)
	}

	body := `{"serverPrivate":"priv","serverPublic":"pub","clients":{"laptop":{"publicKey":"k","address":"192.168.168.2"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = m.load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ServerPrivate != "priv" || st.Clients["laptop"].Address != "192.168.168.2" {
		t.Fatalf("state not decoded: %+v", st)
	}
}

func TestSortedNames(t *testing.T) {
	got := sortedNames(map[string]peerRec{
		"phone":  {},
		"laptop": {},
		"tablet": {},
	})
	if strings.Join(got, ",") != "laptop,phone,tablet" {
		t.Fatalf("sortedNames() = %v", got)
	}
}
