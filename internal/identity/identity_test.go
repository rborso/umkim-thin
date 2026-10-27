package identity

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDirectory(t *testing.T, portal string) *Directory {
	t.Helper()
	return New(Config{
		PortalHost:   portal,
		DefaultGroup: "default-tenant",
		StateFile:    filepath.Join(t.TempDir(), "invites.json"),
		TTL:          24 * time.Hour,
		Zone:         "example.com",
	})
}

func TestLink(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	if got := d.Link("abc"); got != "https://auth.example.com/invite?token=abc" {
		t.Fatalf("Link() = %q", got)
	}
	d = testDirectory(t, "")
	if got := d.Link("abc"); got != "https://auth.example.com/invite?token=abc" {
		t.Fatalf("fallback Link() = %q", got)
	}
}

func TestInviteLifecycle(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	inv, link, err := d.CreateInvite("someone@example.com", "Someone", "")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Group != "default-tenant" {
		t.Fatalf("empty group should use the default, got %q", inv.Group)
	}
	if len(inv.Token) != 48 {
		t.Fatalf("token length = %d, want 48 hex characters", len(inv.Token))
	}
	if !strings.Contains(link, inv.Token) {
		t.Fatalf("link %q does not contain the token", link)
	}
	if got := time.Until(inv.Expires); got < 23*time.Hour || got > 25*time.Hour {
		t.Fatalf("expiry is %v, want about 24h", got)
	}

	info, err := os.Stat(d.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("invite store mode = %o, want 0600", info.Mode().Perm())
	}

	invites, err := d.ListInvites()
	if err != nil {
		t.Fatal(err)
	}
	if len(invites) != 1 || invites[0].Token != inv.Token {
		t.Fatalf("ListInvites() = %+v", invites)
	}

	if err := d.Revoke(inv.Token); err != nil {
		t.Fatal(err)
	}
	invites, err = d.ListInvites()
	if err != nil {
		t.Fatal(err)
	}
	if len(invites) != 0 {
		t.Fatalf("revoked invite still listed: %+v", invites)
	}
	if err := d.Revoke("missing"); err == nil {
		t.Fatal("revoking an unknown invitation succeeded")
	}
}

func TestCreateInviteUniqueness(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	a, _, err := d.CreateInvite("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := d.CreateInvite("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Token == b.Token {
		t.Fatal("two invitations received the same token")
	}
}

func TestListInvitesRejectsBrokenState(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	if err := os.WriteFile(d.cfg.StateFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListInvites(); err == nil {
		t.Fatal("ListInvites() accepted broken JSON")
	}
}

func TestRedeemValidation(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	cases := []struct {
		name     string
		token    string
		username string
		password string
		want     string
	}{
		{"missing token", "", "user", "password123", "required"},
		{"missing username", "token", "", "password123", "required"},
		{"short password", "token", "user", "short", "required"},
		{"unknown token", "unknown", "user", "password123", "unknown invitation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := d.Redeem(c.token, c.username, c.password, "", "")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Redeem() = %v, want error containing %q", err, c.want)
			}
		})
	}
}

func TestRedeemRefusesUsedOrExpiredInvite(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	inv, _, err := d.CreateInvite("", "", "")
	if err != nil {
		t.Fatal(err)
	}

	store, err := d.load()
	if err != nil {
		t.Fatal(err)
	}
	store[inv.Token].Used = true
	if err := d.save(store); err != nil {
		t.Fatal(err)
	}
	if err := d.Redeem(inv.Token, "user", "password123", "", ""); err == nil || !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("used invite: %v", err)
	}

	store[inv.Token].Used = false
	store[inv.Token].Expires = time.Now().Add(-time.Minute)
	if err := d.save(store); err != nil {
		t.Fatal(err)
	}
	if err := d.Redeem(inv.Token, "user", "password123", "", ""); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired invite: %v", err)
	}
}

func fakeKubectl(t *testing.T, body string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	stdinFile := filepath.Join(dir, "stdin")
	script := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGV_FILE", argvFile)
	t.Setenv("STDIN_FILE", stdinFile)
	return script, argvFile, stdinFile
}

// The directory password and the admin token must never be kubectl arguments:
// they would show up in ps and in any error that formats the command.
func TestSetPasswordUsesStdinNotArgv(t *testing.T) {
	script, argvFile, stdinFile := fakeKubectl(t, `
printf '%s\n' "$*" > "$ARGV_FILE"
cat > "$STDIN_FILE"
`)
	d := New(Config{
		Kubectl:    script,
		Kubeconfig: "/dev/null",
		Namespace:  "identity",
	})
	password := "s3cret pass"
	token := "admin-token-123"
	if err := d.setPassword("alice", password, token); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), password) || strings.Contains(string(argv), token) {
		t.Fatalf("secret in kubectl argv: %s", argv)
	}
	if !strings.Contains(string(argv), "-i") {
		t.Fatalf("kubectl exec is not interactive: %s", argv)
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != password+"\n"+token+"\n" {
		t.Fatalf("stdin = %q", stdin)
	}
}

func TestSetPasswordRedactsOutputOnFailure(t *testing.T) {
	script, _, _ := fakeKubectl(t, `
cat > "$STDIN_FILE"
echo "the helper rejected s3cret-pass"
exit 1
`)
	d := New(Config{Kubectl: script, Kubeconfig: "/dev/null", Namespace: "identity"})
	err := d.setPassword("alice", "s3cret-pass", "admin-token-123")
	if err == nil {
		t.Fatal("setPassword() succeeded")
	}
	if strings.Contains(err.Error(), "s3cret-pass") {
		t.Fatalf("password leaked in the error: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error does not show the redaction: %v", err)
	}
}

func TestKubectlErrorOmitsArguments(t *testing.T) {
	script, _, _ := fakeKubectl(t, `exit 1`)
	d := New(Config{Kubectl: script, Kubeconfig: "/dev/null", Namespace: "identity"})
	_, err := d.kubectl("get", "secret", "my-private-secret")
	if err == nil {
		t.Fatal("kubectl() succeeded")
	}
	if strings.Contains(err.Error(), "my-private-secret") {
		t.Fatalf("kubectl error includes the argument: %v", err)
	}
}

func TestRedeemRejectsControlCharactersInPassword(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	inv, _, err := d.CreateInvite("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"password\n123", "password\r123", "password\x00123"} {
		if err := d.Redeem(inv.Token, "user", password, "", ""); err == nil ||
			!strings.Contains(err.Error(), "must not contain") {
			t.Fatalf("Redeem(%q) = %v", password, err)
		}
	}
}

func TestDirectoryClientTimeout(t *testing.T) {
	old := directoryHTTPClient
	defer func() { directoryHTTPClient = old }()
	directoryHTTPClient = &http.Client{Timeout: 10 * time.Millisecond}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	d := New(Config{})
	if err := d.gql(srv.URL, "token", "{groups{id}}", nil, nil); err == nil {
		t.Fatal("gql() succeeded against a slow directory")
	}
}

func TestClaimIsSingleUse(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	inv, _, err := d.CreateInvite("", "", "")
	if err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.claim(inv.Token)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d concurrent claims succeeded, want 1", successes)
	}

	// A released claim can be taken over by the retry.
	if err := d.release(inv.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := d.claim(inv.Token); err != nil {
		t.Fatalf("released invite could not be claimed again: %v", err)
	}
}

func TestConcurrentCreateInvite(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := d.CreateInvite("", "", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	invites, err := d.ListInvites()
	if err != nil {
		t.Fatal(err)
	}
	if len(invites) != n {
		t.Fatalf("stored %d invites, want %d", len(invites), n)
	}
	seen := map[string]bool{}
	for _, inv := range invites {
		if seen[inv.Token] {
			t.Fatalf("duplicate token %q", inv.Token)
		}
		seen[inv.Token] = true
	}
}

func TestSaveIsAtomicAndMode600(t *testing.T) {
	d := testDirectory(t, "auth.example.com")
	if _, _, err := d.CreateInvite("", "", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(d.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("invite store mode = %o", info.Mode().Perm())
	}
	dir := filepath.Dir(d.cfg.StateFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".invites-") {
			t.Fatalf("temporary invite file left behind: %s", e.Name())
		}
	}
}
