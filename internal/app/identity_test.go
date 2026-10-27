package app

import (
	"strings"
	"testing"
)

func TestHashClientSecretWithSalt(t *testing.T) {
	// Vector generated with Python's hashlib.pbkdf2_hmac('sha512', ...), which
	// is the construction Authelia validates.
	got := hashClientSecretWithSalt("secret", []byte("0123456789abcdef"))
	want := "$pbkdf2-sha512$310000$MDEyMzQ1Njc4OWFiY2RlZg$FadXUUEuEeFcotBtAQM66ddXiYZCdLGmol8RH72kBoD4nBf4K8948NqTB4Bq0fPC9LuXqK42yzGd73l570bpfA"
	if got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}
}

func TestHashClientSecretFormat(t *testing.T) {
	h, err := hashClientSecret("secret")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "pbkdf2-sha512" || parts[2] != "310000" {
		t.Fatalf("unexpected hash format: %q", h)
	}
	if parts[3] == "" || parts[4] == "" {
		t.Fatalf("empty salt or digest in %q", h)
	}
	if strings.ContainsAny(parts[3]+parts[4], "+=-_") {
		t.Fatalf("salt/digest use a character outside the adapted base64 alphabet: %q", h)
	}
}

func TestHashClientSecretAdaptedBase64(t *testing.T) {
	// These salt bytes encode to "+///" in standard base64; go-crypt expects
	// "./ //" in its adapted alphabet.
	h := hashClientSecretWithSalt("secret", []byte{0xfb, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	if strings.Contains(h, "+") {
		t.Fatalf("hash still contains '+': %q", h)
	}
	if !strings.Contains(h, "./") {
		t.Fatalf("hash does not use the adapted alphabet: %q", h)
	}
}
