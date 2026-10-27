// Package identity is the operator surface for the directory and the portal.
//
// One directory per cluster. An invitation always creates a *new* account and
// refuses an existing username: adopting a user is a separate, deliberate
// operation, never a side effect of an invitation.
package identity

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is how to reach the directory, from the environment like the installer.
type Config struct {
	Kubeconfig     string
	Kubectl        string
	Namespace      string // where LLDAP runs
	AdminSecret    string // Secret holding the directory admin password
	AdminSecretKey string
	AdminUser      string
	BaseDN         string
	HTTPPort       int
	PortalHost     string
	Zone           string
	DefaultGroup   string
	StateFile      string // invite store
	TTL            time.Duration
}

// ConfigFromEnv mirrors app.ConfigFromEnv for the identity surface.
func ConfigFromEnv() Config {
	loadConfigFile("/etc/umkim-thin/config.env")
	stateDir := env("UMKIM_THIN_STATE_DIR", "/etc/umkim-thin")
	zone := env("UMKIM_THIN_ZONE", "")
	portal := env("UMKIM_THIN_PORTAL_HOST", "")
	if portal == "" && zone != "" {
		portal = "auth." + zone
	}
	return Config{
		Kubeconfig:     env("UMKIM_THIN_KUBECONFIG", "/etc/rancher/k3s/k3s.yaml"),
		Kubectl:        env("UMKIM_THIN_KUBECTL", "/usr/local/bin/kubectl"),
		Namespace:      env("UMKIM_THIN_IDENTITY_NAMESPACE", "identity"),
		AdminSecret:    env("UMKIM_THIN_LLDAP_SECRET", "lldap-credentials"),
		AdminSecretKey: env("UMKIM_THIN_LLDAP_SECRET_KEY", "LLDAP_LDAP_USER_PASS"),
		AdminUser:      env("UMKIM_THIN_LLDAP_ADMIN_USER", "admin"),
		BaseDN:         env("UMKIM_THIN_BASE_DN", ""),
		HTTPPort:       atoi(env("UMKIM_THIN_LLDAP_HTTP_PORT", "17170")),
		PortalHost:     portal,
		Zone:           zone,
		DefaultGroup:   env("UMKIM_THIN_TENANT", "default-tenant"),
		StateFile:      env("UMKIM_THIN_INVITES", stateDir+"/invites.json"),
		TTL:            time.Duration(atoi(env("UMKIM_THIN_INVITE_TTL_HOURS", "168"))) * time.Hour,
	}
}

// Directory is the LLDAP instance deployed by the identity chart.
type Directory struct {
	cfg Config
	mu  sync.Mutex // serialises the invite store
}

// New returns a directory handle.
func New(cfg Config) *Directory { return &Directory{cfg: cfg} }

// Invite is a single-use token with an expiry.
type Invite struct {
	Token       string     `json:"token"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName,omitempty"`
	Group       string     `json:"group"`
	Expires     time.Time  `json:"expires"`
	Used        bool       `json:"used"`
	ClaimedAt   *time.Time `json:"claimedAt,omitempty"`
}

// claimLease bounds a redemption. A claim older than this is abandoned (the
// process crashed or the directory was unreachable) and may be taken over.
const claimLease = 5 * time.Minute

// Link is the URL an invitee opens.
func (d *Directory) Link(token string) string {
	host := d.cfg.PortalHost
	if host == "" {
		host = "auth.example.com"
	}
	return fmt.Sprintf("https://%s/invite?token=%s", host, token)
}

// CreateInvite issues a single-use invitation. Email is optional: an invitation
// is just a link, and the invitee chooses their own username and password.
func (d *Directory) CreateInvite(email, displayName, group string) (Invite, string, error) {
	if group == "" {
		group = d.cfg.DefaultGroup
	}
	tok, err := randomToken()
	if err != nil {
		return Invite{}, "", err
	}
	inv := Invite{
		Token:       tok,
		Email:       email,
		DisplayName: displayName,
		Group:       group,
		Expires:     time.Now().Add(d.cfg.TTL),
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	store, err := d.load()
	if err != nil {
		return Invite{}, "", err
	}
	store[inv.Token] = &inv
	if err := d.save(store); err != nil {
		return Invite{}, "", err
	}
	return inv, d.Link(inv.Token), nil
}

// ListInvites returns every invitation, newest expiry first.
func (d *Directory) ListInvites() ([]Invite, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	store, err := d.load()
	if err != nil {
		return nil, err
	}
	out := make([]Invite, 0, len(store))
	for _, inv := range store {
		out = append(out, *inv)
	}
	return out, nil
}

// Revoke removes an invitation so its link no longer works.
func (d *Directory) Revoke(token string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	store, err := d.load()
	if err != nil {
		return err
	}
	if _, ok := store[token]; !ok {
		return fmt.Errorf("unknown invitation")
	}
	delete(store, token)
	return d.save(store)
}

// DeleteUser removes an account from the directory. A missing account is not
// an error: the CI cleanup calls this after a failed run and must be idempotent.
func (d *Directory) DeleteUser(username string) error {
	if username == "" {
		return fmt.Errorf("username is required")
	}
	base, token, err := d.auth()
	if err != nil {
		return err
	}
	var out json.RawMessage
	if err := d.gql(base, token, `mutation($u:String!){deleteUser(userId:$u){__typename}}`,
		map[string]any{"u": username}, &out); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil
		}
		return fmt.Errorf("deleting %q: %w", username, err)
	}
	return nil
}

// Groups lists the directory's groups, for the invite form.
func (d *Directory) Groups() ([]string, error) {
	base, token, err := d.auth()
	if err != nil {
		return nil, err
	}
	var out struct {
		Groups []struct {
			ID          int    `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"groups"`
	}
	if err := d.gql(base, token, `{groups{id displayName}}`, nil, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, g := range out.Groups {
		names = append(names, g.DisplayName)
	}
	sort.Strings(names)
	return names, nil
}

// Redeem creates the account behind an invitation. It refuses a username that
// already exists rather than taking the account over, and never resets an
// existing password.
func (d *Directory) Redeem(token, username, password, displayName, email string) error {
	if token == "" || username == "" || len(password) < 8 {
		return fmt.Errorf("token, username and a password of at least 8 characters are required")
	}
	// The password is fed to the directory helper on stdin, one line per value;
	// control characters would break that protocol (and are not valid input).
	if strings.ContainsAny(password, "\r\n\x00") {
		return fmt.Errorf("the password must not contain newlines or NUL bytes")
	}
	inv, err := d.claim(token)
	if err != nil {
		return err
	}
	// Any failure after the claim releases it so the invitee can retry. The
	// terminal success marks it used; a crash leaves a claim that expires.
	release := func() { _ = d.release(token) }
	if email == "" {
		email = inv.Email
	}
	if email == "" {
		// No email anywhere: LLDAP needs one, so synthesize from the username.
		email = username + "@" + firstNonEmpty(d.cfg.Zone, "local")
	}
	if displayName == "" {
		displayName = inv.DisplayName
	}
	if displayName == "" {
		displayName = username
	}

	base, token2, err := d.auth()
	if err != nil {
		release()
		return err
	}

	// Refuse to adopt an existing account. LLDAP answers a missing user with an
	// "Entity not found" error rather than null, so that is not a failure.
	var exists struct {
		User *struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := d.gql(base, token2, `query($u:String!){user(userId:$u){id}}`, map[string]any{"u": username}, &exists); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			release()
			return err
		}
	}
	if exists.User != nil && exists.User.ID != "" {
		release()
		return fmt.Errorf("username %q already exists; an invitation never takes over an account", username)
	}

	// Create the account, add it to its group, set the password.
	create := struct {
		CreateUser struct {
			ID string `json:"id"`
		} `json:"createUser"`
	}{}
	vars := map[string]any{"u": map[string]any{"id": username, "email": email, "displayName": displayName}}
	if err := d.gql(base, token2, `mutation($u:CreateUserInput!){createUser(user:$u){id}}`, vars, &create); err != nil {
		release()
		return fmt.Errorf("creating the account: %w", err)
	}
	if gid := d.groupID(base, token2, inv.Group); gid != 0 {
		var added json.RawMessage
		_ = d.gql(base, token2, `mutation($u:String!,$g:Int!){addUserToGroup(userId:$u,groupId:$g){__typename}}`,
			map[string]any{"u": username, "g": gid}, &added)
	}
	if err := d.setPassword(username, password, token2); err != nil {
		// Compensate: do not leave an account without a password behind.
		var del json.RawMessage
		_ = d.gql(base, token2, `mutation($u:String!){deleteUser(userId:$u){__typename}}`, map[string]any{"u": username}, &del)
		release()
		return fmt.Errorf("setting the password: %w", err)
	}

	if err := d.markUsed(token); err != nil {
		return fmt.Errorf("the account was created, but the invocation could not be finalised: %w", err)
	}
	return nil
}

// claim reserves an invitation for one redemption. A used or expired invite is
// refused; a claim younger than claimLease blocks a second attempt.
func (d *Directory) claim(token string) (Invite, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	store, err := d.load()
	if err != nil {
		return Invite{}, err
	}
	inv, ok := store[token]
	if !ok {
		return Invite{}, fmt.Errorf("unknown invitation")
	}
	if inv.Used {
		return Invite{}, fmt.Errorf("this invitation has already been used")
	}
	if time.Now().After(inv.Expires) {
		return Invite{}, fmt.Errorf("this invitation has expired")
	}
	if inv.ClaimedAt != nil && time.Since(*inv.ClaimedAt) < claimLease {
		return Invite{}, fmt.Errorf("this invitation is already being redeemed")
	}
	now := time.Now()
	inv.ClaimedAt = &now
	if err := d.save(store); err != nil {
		return Invite{}, err
	}
	return *inv, nil
}

func (d *Directory) release(token string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	store, err := d.load()
	if err != nil {
		return err
	}
	inv, ok := store[token]
	if !ok || inv.Used {
		return nil
	}
	inv.ClaimedAt = nil
	return d.save(store)
}

func (d *Directory) markUsed(token string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	store, err := d.load()
	if err != nil {
		return err
	}
	inv, ok := store[token]
	if !ok {
		return fmt.Errorf("unknown invitation")
	}
	inv.Used = true
	inv.ClaimedAt = nil
	return d.save(store)
}

// auth logs in as the directory admin and returns the API base and token.
func (d *Directory) auth() (string, string, error) {
	base, err := d.lldapBase()
	if err != nil {
		return "", "", err
	}
	pw, err := d.adminPassword()
	if err != nil {
		return "", "", err
	}
	token, err := d.login(base, d.cfg.AdminUser, pw)
	if err != nil {
		return "", "", err
	}
	return base, token, nil
}

// Authenticate checks a directory username/password against LLDAP's own login
// endpoint. Used by the console's login (admin only, by the caller's check).
func (d *Directory) Authenticate(username, password string) bool {
	if username == "" || password == "" {
		return false
	}
	base, err := d.lldapBase()
	if err != nil {
		return false
	}
	_, err = d.login(base, username, password)
	return err == nil
}

// lldapBase resolves the in-cluster LLDAP HTTP API address.
func (d *Directory) lldapBase() (string, error) {
	ip, err := d.kubectl("get", "svc", "lldap", "-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		return "", err
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "", fmt.Errorf("lldap service has no cluster IP")
	}
	return fmt.Sprintf("http://%s:%d", ip, d.cfg.HTTPPort), nil
}

// adminPassword reads the directory admin password from its Secret.
func (d *Directory) adminPassword() (string, error) {
	pw, err := d.kubectl("get", "secret", d.cfg.AdminSecret, "-o", "jsonpath={.data."+d.cfg.AdminSecretKey+"}")
	if err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pw))
	if err != nil {
		return "", fmt.Errorf("decoding admin password: %w", err)
	}
	return string(decoded), nil
}

func (d *Directory) login(base, user, pass string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	resp, err := directoryHTTPClient.Post(base+"/auth/simple/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		return "", err
	}
	if login.Token == "" {
		return "", fmt.Errorf("directory login failed for %q", user)
	}
	return login.Token, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (d *Directory) groupID(base, token, name string) int {
	if name == "" {
		return 0
	}
	var out struct {
		Groups []struct {
			ID          int    `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"groups"`
	}
	if err := d.gql(base, token, `{groups{id displayName}}`, nil, &out); err != nil {
		return 0
	}
	for _, g := range out.Groups {
		if g.DisplayName == name {
			return g.ID
		}
	}
	return 0
}

// setPassword uses LLDAP's own helper inside the pod (there is no GraphQL
// mutation for it). The password and the admin token are written to the
// helper's stdin, never to kubectl's argv, so they do not appear in `ps` on the
// host or in an error message.
func (d *Directory) setPassword(username, password, token string) error {
	script := `IFS= read -r LLDAP_USER_PASSWORD || exit 1
IFS= read -r token || exit 1
export LLDAP_USER_PASSWORD
exec /app/lldap_set_password --base-url http://localhost:17170 --token "$token" --username "$1"`
	out, err := d.kubectlInput(password+"\n"+token+"\n",
		"exec", "-i", "deploy/lldap", "--",
		"sh", "-c", script, "sh", username)
	if err != nil {
		return fmt.Errorf("setting the password for %q: %w: %s",
			username, err, redact(strings.TrimSpace(out), password, token))
	}
	return nil
}

// redact removes secret values from command output before it reaches an error,
// a log line, or the console. The caller still receives a useful message.
func redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// directoryHTTPClient bounds every call to LLDAP. A hung directory must turn
// into an error in the console, not a request that never finishes.
var directoryHTTPClient = &http.Client{Timeout: 20 * time.Second}

func (d *Directory) gql(base, token, query string, vars map[string]any, out any) error {
	payload := map[string]any{"query": query}
	if vars != nil {
		payload["variables"] = vars
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, base+"/api/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := directoryHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("%s", envelope.Errors[0].Message)
	}
	if out != nil {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

func (d *Directory) kubectl(args ...string) (string, error) {
	cmd := exec.Command(d.cfg.Kubectl, append([]string{"--kubeconfig", d.cfg.Kubeconfig, "-n", d.cfg.Namespace}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Never format the whole argv: it can contain a password or token.
		return string(out), fmt.Errorf("kubectl %s: %w", args[0], err)
	}
	return string(out), nil
}

// kubectlInput is kubectl with stdin for values that must not be arguments.
func (d *Directory) kubectlInput(stdin string, args ...string) (string, error) {
	cmd := exec.Command(d.cfg.Kubectl, append([]string{"--kubeconfig", d.cfg.Kubeconfig, "-n", d.cfg.Namespace}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w", args[0], err)
	}
	return string(out), nil
}

func (d *Directory) load() (map[string]*Invite, error) {
	store := map[string]*Invite{}
	raw, err := os.ReadFile(d.cfg.StateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil, err
	}
	return store, nil
}

func (d *Directory) save(store map[string]*Invite) error {
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(d.cfg.StateFile)
	f, err := os.CreateTemp(dir, ".invites-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, d.cfg.StateFile)
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
