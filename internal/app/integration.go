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

	mealieBase := fmt.Sprintf("http://%s.%s.svc.cluster.local:9000", mealie.Name, mealie.Namespace)
	secret, ok := in.getSecret(sparky.Namespace, sparky.Name+"-secrets")
	if !ok {
		return MealieSparkyResult{}, fmt.Errorf("the SparkyFitness release Secret is not available")
	}
	encryptionKey := secret["api-encryption-key"]
	if encryptionKey == "" {
		return MealieSparkyResult{}, fmt.Errorf("the SparkyFitness Secret is missing its encryption key")
	}

	existing, err := in.sparkyMealieBaseURL(sparky)
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
	if err := in.writeSparkyMealieProvider(sparky, mealieBase, encrypted, iv, tag); err != nil {
		return fail(err)
	}
	return MealieSparkyResult{
		Connected: true,
		Provider:  "Mealie",
		BaseURL:   mealieBase,
	}, nil
}

const sparkyPostgresImage = "docker.io/library/postgres:18.6"

// sparkyJob runs one short psql Job in the SparkyFitness namespace. The
// console cannot grant itself pods/exec (RBAC escalation prevention), so a Job
// is the supported way to reach the database. The password comes from the
// release Secret; the SQL script (when present) is a Secret mounted read-only.
func (in *Installer) sparkyJob(sparky *Result, name string, args []string, script string) (string, error) {
	ns := sparky.Namespace
	_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
		"delete", "job", name, "--ignore-not-found", "--wait=true")
	secretName := ""
	if script != "" {
		secretName = name + "-sql"
		_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
			"delete", "secret", secretName, "--ignore-not-found")
		secret := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
stringData:
  script.sql: |
%s
`, secretName, ns, indent(script, 4))
		if err := in.apply(secret); err != nil {
			return "", fmt.Errorf("creating the SQL Secret: %w", err)
		}
	}

	dbUser, dbName := in.sparkyDBConfig(sparky)
	var argLines strings.Builder
	for _, a := range args {
		fmt.Fprintf(&argLines, "            - %q\n", a)
	}
	volumes := ""
	mounts := ""
	if secretName != "" {
		volumes = fmt.Sprintf(`      volumes:
        - name: sql
          secret:
            secretName: %s
`, secretName)
		mounts = `          volumeMounts:
            - name: sql
              mountPath: /sql
              readOnly: true
`
	}
	job := fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 180
  template:
    metadata:
      labels:
        app: umkim-thin-mealie-integration
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 999
        runAsGroup: 999
        fsGroup: 999
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: psql
          image: %s
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
          command: ["psql"]
          env:
            - name: HOME
              value: /tmp
            - name: PGHOST
              value: %q
            - name: PGUSER
              value: %q
            - name: PGDATABASE
              value: %q
            - name: PGPASSWORD
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: postgres-password
          args:
%s%s%s
`, name, ns, sparkyPostgresImage, sparky.Name+"-postgres", dbUser, dbName,
		sparky.Name+"-secrets", argLines.String(), mounts, volumes)
	if err := in.apply(job); err != nil {
		if secretName != "" {
			_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
				"delete", "secret", secretName, "--ignore-not-found")
		}
		return "", fmt.Errorf("creating the SparkyFitness database Job: %w", err)
	}
	defer func() {
		_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
			"delete", "job", name, "--ignore-not-found", "--wait=false")
		if secretName != "" {
			_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
				"delete", "secret", secretName, "--ignore-not-found")
		}
	}()

	if _, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
		"wait", "--for=condition=complete", "job/"+name, "--timeout=180s"); err != nil {
		logs, _ := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
			"logs", "job/"+name)
		return "", fmt.Errorf("the SparkyFitness database Job did not complete: %s", strings.TrimSpace(logs))
	}
	out, _ := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
		"logs", "job/"+name)
	return out, nil
}

// sparkyDBConfig reads the release ConfigMap, falling back to the chart
// defaults. The values are not secrets.
func (in *Installer) sparkyDBConfig(sparky *Result) (string, string) {
	user, name := "sparky_admin", "sparkyfitness"
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", sparky.Namespace,
		"get", "configmap", sparky.Name+"-server", "-o", "json")
	if err != nil {
		return user, name
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal([]byte(out), &cm) != nil {
		return user, name
	}
	if v := strings.TrimSpace(cm.Data["SPARKY_FITNESS_DB_USER"]); v != "" {
		user = v
	}
	if v := strings.TrimSpace(cm.Data["SPARKY_FITNESS_DB_NAME"]); v != "" {
		name = v
	}
	return user, name
}

func (in *Installer) sparkyMealieBaseURL(sparky *Result) (string, error) {
	query := `SELECT base_url FROM external_data_providers WHERE provider_type = 'mealie' AND is_public = TRUE AND is_active = TRUE ORDER BY updated_at DESC NULLS LAST LIMIT 1`
	out, err := in.sparkyJob(sparky, "umkim-thin-mealie-check", []string{"-tAc", query}, "")
	if err != nil {
		return "", fmt.Errorf("reading the SparkyFitness Mealie provider: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (in *Installer) writeSparkyMealieProvider(sparky *Result, baseURL, encrypted, iv, tag string) error {
	sql := sparkyProviderSQL(baseURL, encrypted, iv, tag)
	if _, err := in.sparkyJob(sparky, "umkim-thin-mealie-write",
		[]string{"-v", "ON_ERROR_STOP=1", "-f", "/sql/script.sql"}, sql); err != nil {
		return fmt.Errorf("writing the SparkyFitness Mealie provider: %w", err)
	}
	return nil
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
		ID    json.RawMessage `json:"id"`
		Token string          `json:"token"`
	}
	if err := jsonRequest(client, http.MethodPost, base+"/api/users/api-tokens",
		map[string]string{"Authorization": "Bearer " + accessToken}, body, &out); err != nil {
		return "", "", fmt.Errorf("creating the Mealie API token: %w", err)
	}
	if out.Token == "" {
		return "", "", fmt.Errorf("Mealie returned no API token")
	}
	id, err := mealieTokenID(out.ID)
	if err != nil {
		return "", "", err
	}
	return out.Token, id, nil
}

// mealieTokenID accepts both shapes Mealie has used for the token id: a UUID
// string on newer releases and a numeric primary key on older ones.
func mealieTokenID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), nil
	}
	return "", fmt.Errorf("Mealie returned an unexpected API token id")
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
