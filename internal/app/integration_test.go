package app

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestEncryptSparkySecret(t *testing.T) {
	key := strings.Repeat("ab", 32) // 64 hex characters
	encrypted, iv, tag, err := encryptSparkySecret(key, "mealie-token")
	if err != nil {
		t.Fatalf("encryptSparkySecret: %v", err)
	}
	keyBytes, _ := hex.DecodeString(key)
	block, _ := aes.NewCipher(keyBytes)
	gcm, _ := cipher.NewGCM(block)
	nonce, err := base64.StdEncoding.DecodeString(iv)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	authTag, err := base64.StdEncoding.DecodeString(tag)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, nonce, append(ciphertext, authTag...), nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plain) != "mealie-token" {
		t.Fatalf("plaintext = %q", plain)
	}
}

func TestSparkyProviderSQL(t *testing.T) {
	sql := sparkyProviderSQL("http://mealie.mealie.svc.cluster.local:9000", "ct", "iv", "tag")
	for _, want := range []string{
		`provider_type = 'mealie'`,
		`is_public = TRUE`,
		`encrypted_app_key = 'ct'`,
		`app_key_iv = 'iv'`,
		`app_key_tag = 'tag'`,
		`base_url = 'http://mealie.mealie.svc.cluster.local:9000'`,
		`SELECT id INTO uid FROM "user" WHERE role = 'admin' LIMIT 1`,
		`INSERT INTO external_data_providers`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("provider SQL missing %q:\n%s", want, sql)
		}
	}
}

func TestSparkyJobManifest(t *testing.T) {
	dir := t.TempDir()
	applied := filepath.Join(dir, "applied.yaml")
	kubectl := filepath.Join(dir, "kubectl")
	script := `#!/bin/sh
case "$1" in
  apply) cat > "$APPLIED"; exit 0 ;;
esac
case "$5" in
  get) echo '{"data":{"SPARKY_FITNESS_DB_USER":"sparky_admin","SPARKY_FITNESS_DB_NAME":"sparkyfitness"}}' ;;
  logs) echo "base-url" ;;
esac
exit 0
`
	if err := os.WriteFile(kubectl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPLIED", applied)

	in := New(Config{Kubectl: kubectl, Kubeconfig: "/tmp/kubeconfig"})
	_, err := in.sparkyJob(&Result{Name: "sparkyfitness", Namespace: "sparkyfitness"},
		"umkim-thin-mealie-write", []string{"-v", "ON_ERROR_STOP=1", "-f", "/sql/script.sql"}, "SELECT 1;")
	if err != nil {
		t.Fatalf("sparkyJob: %v", err)
	}
	body, err := os.ReadFile(applied)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := yaml.Unmarshal(body, &job); err != nil {
		t.Fatalf("job manifest is not valid YAML: %v\n%s", err, body)
	}
	if job["kind"] != "Job" {
		t.Fatalf("kind = %v", job["kind"])
	}
	for _, want := range []string{
		"name: umkim-thin-mealie-write",
		"namespace: sparkyfitness",
		"name: PGPASSWORD",
		"key: postgres-password",
		"name: sql",
		"secretName: umkim-thin-mealie-write-sql",
		"mountPath: /sql",
		"runAsNonRoot: true",
		"/sql/script.sql",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("job manifest missing %q:\n%s", want, body)
		}
	}
}

func TestMealieTokenFlow(t *testing.T) {
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse form: %v", err)
			}
			if r.Form.Get("username") != "admin@example.com" || r.Form.Get("password") != "mealie-password" {
				t.Errorf("login form = %v", r.Form)
			}
			writeTestJSON(t, w, map[string]string{"access_token": "mealie-session"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/users/api-tokens":
			if got := r.Header.Get("Authorization"); got != "Bearer mealie-session" {
				t.Errorf("token auth = %q", got)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode token body: %v", err)
			}
			if body["name"] != "sparkyfitness" || body["integrationId"] != "sparkyfitness" {
				t.Errorf("token body = %#v", body)
			}
			writeTestJSON(t, w, map[string]string{"id": "token-1", "token": "mealie-token"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/users/api-tokens/token-1":
			deleted = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected Mealie request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	session, err := mealieLogin(client, server.URL, "admin@example.com", "mealie-password")
	if err != nil {
		t.Fatalf("mealieLogin: %v", err)
	}
	token, id, err := mealieCreateAPIToken(client, server.URL, session)
	if err != nil {
		t.Fatalf("mealieCreateAPIToken: %v", err)
	}
	if token != "mealie-token" || id != "token-1" {
		t.Fatalf("token = %q, id = %q", token, id)
	}
	if err := mealieDeleteAPIToken(client, server.URL, session, id); err != nil {
		t.Fatalf("mealieDeleteAPIToken: %v", err)
	}
	if !deleted {
		t.Fatal("the token delete endpoint was not called")
	}
}

func TestMealieTokenNumericID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/users/api-tokens" {
			http.NotFound(w, r)
			return
		}
		writeTestJSON(t, w, map[string]any{"id": 42, "token": "mealie-token"})
	}))
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	token, id, err := mealieCreateAPIToken(client, server.URL, "session")
	if err != nil {
		t.Fatalf("mealieCreateAPIToken: %v", err)
	}
	if token != "mealie-token" || id != "42" {
		t.Fatalf("token = %q, id = %q", token, id)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
