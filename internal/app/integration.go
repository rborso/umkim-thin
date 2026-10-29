package app

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MealieSparkyResult reports a manual, optional integration between the Mealie
// recipe manager and SparkyFitness's built-in Mealie food provider. The
// integration is deliberately not part of either app's Helm release: it
// changes the running SparkyFitness database and can be run or skipped from
// the console.
type MealieSparkyResult struct {
	Connected        bool   `json:"connected"`
	AlreadyConnected bool   `json:"alreadyConnected,omitempty"`
	Provider         string `json:"provider"`
	BaseURL          string `json:"baseUrl"`
}

// ConnectMealieToSparky is the console's one-click Mealie integration. It
// creates a Mealie API token and writes the global provider straight into
// SparkyFitness, using SparkyFitness's own database and encryption key.
//
// There is no supported service-account/API-key handoff in SparkyFitness, so
// the alternative would be asking the operator to paste an administrator API
// key. The write is limited to the provider row and uses the application's own
// AES-GCM key, exactly as SparkyFitness would store it.
func (in *Installer) ConnectMealieToSparky() (MealieSparkyResult, error) {
	installed, err := in.List()
	if err != nil {
		return MealieSparkyResult{}, err
	}
	var mealie, sparky *Result
	for i := range installed {
		switch installed[i].Name {
		case "mealie":
			mealie = &installed[i]
		case "sparkyfitness":
			sparky = &installed[i]
		}
	}
	if mealie == nil {
		return MealieSparkyResult{}, fmt.Errorf("Mealie is not installed; install it before connecting")
	}
	if sparky == nil {
		return MealieSparkyResult{}, fmt.Errorf("SparkyFitness is not installed")
	}

	// The platform's default-deny policies block cross-namespace traffic. Add
	// the two narrow allowances the integration needs, so it also works on an
	// installation provisioned before these namespaces were wired.
	if err := in.ensureIntegrationNetworkPolicies(mealie.Namespace, sparky.Namespace); err != nil {
		return MealieSparkyResult{}, fmt.Errorf("allowing the Mealie/SparkyFitness integration: %w", err)
	}
	if err := in.ensureSparkyExecRole(sparky.Namespace); err != nil {
		return MealieSparkyResult{}, fmt.Errorf("allowing the console to write the SparkyFitness provider: %w", err)
	}

	mealieBase := fmt.Sprintf("http://%s.%s.svc.cluster.local:9000", mealie.Name, mealie.Namespace)
	secret, ok := in.getSecret(sparky.Namespace, sparky.Name+"-secrets")
	if !ok {
		return MealieSparkyResult{}, fmt.Errorf("the SparkyFitness release Secret is not available")
	}
	dbPassword := secret["postgres-password"]
	encryptionKey := secret["api-encryption-key"]
	if dbPassword == "" || encryptionKey == "" {
		return MealieSparkyResult{}, fmt.Errorf("the SparkyFitness Secret is missing its database password or encryption key")
	}

	existing, err := in.sparkyMealieBaseURL(sparky, dbPassword)
	if err != nil {
		return MealieSparkyResult{}, err
	}
	if strings.TrimRight(existing, "/") == mealieBase {
		return MealieSparkyResult{
			Connected:        true,
			AlreadyConnected: true,
			Provider:         "Mealie",
			BaseURL:          mealieBase,
		}, nil
	}

	email, err := in.mealieAdminEmail(mealie.Name, mealie.Namespace)
	if err != nil {
		return MealieSparkyResult{}, err
	}
	adminPassword, err := in.mealieAdminPassword(mealie.Name, mealie.Namespace)
	if err != nil {
		return MealieSparkyResult{}, err
	}

	client := &http.Client{Timeout: 25 * time.Second}
	accessToken, err := mealieLogin(client, mealieBase, email, adminPassword)
	if err != nil {
		return MealieSparkyResult{}, err
	}
	token, tokenID, err := mealieCreateAPIToken(client, mealieBase, accessToken)
	if err != nil {
		return MealieSparkyResult{}, err
	}
	fail := func(err error) (MealieSparkyResult, error) {
		_ = mealieDeleteAPIToken(client, mealieBase, accessToken, tokenID)
		return MealieSparkyResult{}, err
	}

	encrypted, iv, tag, err := encryptSparkySecret(encryptionKey, token)
	if err != nil {
		return fail(fmt.Errorf("encrypting the Mealie token for SparkyFitness: %w", err))
	}
	if err := in.writeSparkyMealieProvider(sparky, dbPassword, mealieBase, encrypted, iv, tag); err != nil {
		return fail(err)
	}
	return MealieSparkyResult{
		Connected: true,
		Provider:  "Mealie",
		BaseURL:   mealieBase,
	}, nil
}

// ensureIntegrationNetworkPolicies allows the console to reach both apps and
// SparkyFitness to reach Mealie. It is additive: an existing default-deny
// policy stays in force for every other source.
func (in *Installer) ensureIntegrationNetworkPolicies(mealieNS, sparkyNS string) error {
	if err := in.apply(fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: umkim-thin-mealie-integration
  namespace: %s
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: %s
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: umkim-thin
      ports:
        - protocol: TCP
          port: 9000
`, mealieNS, sparkyNS)); err != nil {
		return fmt.Errorf("Mealie network policy: %w", err)
	}
	if err := in.apply(fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: umkim-thin-sparkyfitness-integration
  namespace: %s
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: umkim-thin
      ports:
        - protocol: TCP
          port: 80
`, sparkyNS)); err != nil {
		return fmt.Errorf("SparkyFitness network policy: %w", err)
	}
	return nil
}

// ensureSparkyExecRole grants the console pods/exec only in the SparkyFitness
// namespace. The console already manages the release there; this keeps the
// exec grant out of the cluster-wide role.
func (in *Installer) ensureSparkyExecRole(namespace string) error {
	return in.apply(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: umkim-thin-mealie-integration
  namespace: %s
rules:
  - apiGroups: [""]
    resources: [pods/exec]
    verbs: [create, get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: umkim-thin-mealie-integration
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: umkim-thin-mealie-integration
subjects:
  - kind: ServiceAccount
    name: umkim-thin-console
    namespace: umkim-thin
`, namespace, namespace))
}

func (in *Installer) sparkyMealieBaseURL(sparky *Result, dbPassword string) (string, error) {
	query := `SELECT base_url FROM external_data_providers WHERE provider_type = 'mealie' AND is_public = TRUE AND is_active = TRUE ORDER BY updated_at DESC NULLS LAST LIMIT 1`
	out, err := in.sparkyPSQL(sparky, dbPassword, []string{"-tAc", query}, "")
	if err != nil {
		return "", fmt.Errorf("reading the SparkyFitness Mealie provider: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (in *Installer) writeSparkyMealieProvider(sparky *Result, dbPassword, baseURL, encrypted, iv, tag string) error {
	sql := sparkyProviderSQL(baseURL, encrypted, iv, tag)
	if _, err := in.sparkyPSQL(sparky, dbPassword, []string{"-v", "ON_ERROR_STOP=1", "-f", "-"}, sql); err != nil {
		return fmt.Errorf("writing the SparkyFitness Mealie provider: %w", err)
	}
	return nil
}

// sparkyPSQL runs psql inside the release's PostgreSQL pod. The password is
// sent on stdin, never in argv. `sql` may be empty when psqlArgs already point
// at a file/stdin.
func (in *Installer) sparkyPSQL(sparky *Result, dbPassword string, psqlArgs []string, sql string) (string, error) {
	args := []string{"--kubeconfig", in.cfg.Kubeconfig, "-n", sparky.Namespace,
		"exec", "-i", "deploy/" + sparky.Name + "-postgres", "--", "sh", "-c",
		`set -eu
IFS= read -r pw
PGPASSWORD="$pw" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" "$@"`,
		"psql"}
	args = append(args, psqlArgs...)
	input := dbPassword + "\n" + sql
	out, err := in.runInput(input, in.cfg.Kubectl, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// sparkyProviderSQL upserts the global Mealie provider. SparkyFitness stores
// provider credentials with AES-256-GCM under SPARKY_FITNESS_API_ENCRYPTION_KEY;
// the console encrypts the Mealie token before it reaches the database.
func sparkyProviderSQL(baseURL, encrypted, iv, tag string) string {
	return fmt.Sprintf(`DO $$
DECLARE uid uuid;
BEGIN
  SELECT id INTO uid FROM "user" WHERE role = 'admin' LIMIT 1;
  IF uid IS NULL THEN
    SELECT id INTO uid FROM "user" LIMIT 1;
  END IF;
  IF uid IS NULL THEN
    RAISE EXCEPTION 'SparkyFitness has no user to own the global provider';
  END IF;
  UPDATE external_data_providers
     SET provider_name = 'Mealie',
         base_url = %s,
         is_active = TRUE,
         is_public = TRUE,
         encrypted_app_id = NULL,
         app_id_iv = NULL,
         app_id_tag = NULL,
         encrypted_app_key = %s,
         app_key_iv = %s,
         app_key_tag = %s,
         sync_frequency = 'manual',
         updated_at = now()
   WHERE provider_type = 'mealie' AND is_public = TRUE;
  IF NOT FOUND THEN
    INSERT INTO external_data_providers (
      provider_name, provider_type, user_id, is_active, base_url, is_public,
      encrypted_app_key, app_key_iv, app_key_tag, sync_frequency, created_at, updated_at
    ) VALUES (
      'Mealie', 'mealie', uid, TRUE, %s, TRUE,
      %s, %s, %s, 'manual', now(), now()
    );
  END IF;
END $$;
`, sqlQuote(baseURL), sqlQuote(encrypted), sqlQuote(iv), sqlQuote(tag),
		sqlQuote(baseURL), sqlQuote(encrypted), sqlQuote(iv), sqlQuote(tag))
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// encryptSparkySecret mirrors SparkyFitness's encryption.ts: a 64-character
// hex key (or a 44-character base64 key decoded to bytes), AES-256-GCM with a
// 12-byte IV, and base64 ciphertext/IV/tag.
func encryptSparkySecret(rawKey, plaintext string) (string, string, string, error) {
	keyHex := rawKey
	if len(rawKey) == 44 {
		decoded, err := base64.StdEncoding.DecodeString(rawKey)
		if err != nil {
			return "", "", "", fmt.Errorf("decoding the SparkyFitness encryption key: %w", err)
		}
		keyHex = hex.EncodeToString(decoded)
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return "", "", "", fmt.Errorf("the SparkyFitness encryption key is not 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	tagSize := gcm.Overhead()
	return base64.StdEncoding.EncodeToString(sealed[:len(sealed)-tagSize]),
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(sealed[len(sealed)-tagSize:]), nil
}

func (in *Installer) mealieAdminEmail(release, namespace string) (string, error) {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", namespace, "get", "deployment", release, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("reading the Mealie deployment: %w", err)
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &dep); err != nil {
		return "", fmt.Errorf("parsing the Mealie deployment: %w", err)
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, env := range c.Env {
			if env.Name == "DEFAULT_EMAIL" && strings.TrimSpace(env.Value) != "" {
				return strings.TrimSpace(env.Value), nil
			}
		}
	}
	// Mealie's chart default. It is still valid for older installs that did
	// not override bootstrap.adminEmail.
	return "changeme@example.com", nil
}

func (in *Installer) mealieAdminPassword(release, namespace string) (string, error) {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", namespace, "get", "secret", release+"-secrets",
		"-o", "jsonpath={.data.admin-password}")
	if err != nil {
		return "", fmt.Errorf("reading the Mealie admin Secret: %w", err)
	}
	raw := strings.TrimSpace(out)
	if raw == "" {
		return "", fmt.Errorf("the Mealie admin Secret has no password")
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decoding the Mealie admin password: %w", err)
	}
	return string(decoded), nil
}

func mealieLogin(client *http.Client, base, email, password string) (string, error) {
	form := url.Values{}
	form.Set("username", email)
	form.Set("password", password)
	resp, err := client.PostForm(base+"/api/auth/token", form)
	if err != nil {
		return "", fmt.Errorf("Mealie admin login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Mealie admin login failed (HTTP %d)", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("parsing the Mealie login response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("Mealie returned no access token")
	}
	return body.AccessToken, nil
}

func mealieCreateAPIToken(client *http.Client, base, accessToken string) (string, string, error) {
	body := map[string]string{
		"name":          "sparkyfitness",
		"integrationId": "sparkyfitness",
	}
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := jsonRequest(client, http.MethodPost, base+"/api/users/api-tokens",
		map[string]string{"Authorization": "Bearer " + accessToken}, body, &out); err != nil {
		return "", "", fmt.Errorf("creating the Mealie API token: %w", err)
	}
	if out.Token == "" {
		return "", "", fmt.Errorf("Mealie returned no API token")
	}
	return out.Token, out.ID, nil
}

func mealieDeleteAPIToken(client *http.Client, base, accessToken, tokenID string) error {
	if tokenID == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodDelete,
		base+"/api/users/api-tokens/"+url.PathEscape(tokenID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("delete Mealie token: HTTP %d", resp.StatusCode)
	}
	return nil
}

// jsonRequest performs a JSON request and deliberately reports only the status
// on failure. Response bodies can contain diagnostics but must not end up in
// console logs or UI errors carrying credentials.
func jsonRequest(client *http.Client, method, endpoint string, headers map[string]string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decoding the response: %w", err)
	}
	return nil
}
