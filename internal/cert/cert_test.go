package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func makeCert(t *testing.T, names []string, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestParseValid(t *testing.T) {
	certPEM, keyPEM := makeCert(t, []string{"*.example.com", "example.com"}, time.Now().Add(24*time.Hour))
	info, err := Parse(certPEM, keyPEM, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Hosts) != 2 || info.NotAfter.IsZero() {
		t.Fatalf("info = %+v", info)
	}
	if !Covers(&x509.Certificate{DNSNames: []string{"*.example.com"}}, "app.example.com") {
		t.Fatal("wildcard did not cover a subdomain")
	}
	if Covers(&x509.Certificate{DNSNames: []string{"*.example.com"}}, "a.b.example.com") {
		t.Fatal("wildcard covered two labels")
	}
}

func TestParseExpired(t *testing.T) {
	certPEM, keyPEM := makeCert(t, []string{"app.example.com"}, time.Now().Add(-time.Hour))
	if _, err := Parse(certPEM, keyPEM, nil); err == nil {
		t.Fatal("expired certificate accepted")
	}
}

func TestParseMismatchedKey(t *testing.T) {
	certPEM, _ := makeCert(t, []string{"app.example.com"}, time.Now().Add(time.Hour))
	_, otherKey := makeCert(t, []string{"app.example.com"}, time.Now().Add(time.Hour))
	if _, err := Parse(certPEM, otherKey, nil); err == nil {
		t.Fatal("mismatched key accepted")
	}
}

func TestParseHostCoverage(t *testing.T) {
	certPEM, keyPEM := makeCert(t, []string{"app.example.com"}, time.Now().Add(time.Hour))
	if _, err := Parse(certPEM, keyPEM, []string{"other.example.com"}); err == nil {
		t.Fatal("certificate accepted for a host it does not cover")
	}
}
