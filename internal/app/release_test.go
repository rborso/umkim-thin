package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
)

func TestReleaseReadsHelmValuesAndRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	helm := filepath.Join(dir, "helm")
	script := `#!/bin/sh
case "$1" in
  status)
    cat <<'JSON'
{"name":"demo","namespace":"demo","version":4,"info":{"status":"deployed","last_deployed":"2026-10-29T10:00:00Z"},"chart":{"metadata":{"name":"demo","version":"0.1.2","appVersion":"1.0.0"}}}
JSON
    ;;
  get)
    cat <<'JSON'
{"auth":{"password":"hunter2","existingSecret":"demo-secret"},"media":{"accessMode":"ReadWriteMany"},"nested":[{"token":"abc"}]}
JSON
    ;;
esac
`
	if err := os.WriteFile(helm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := New(Config{
		Helm:       helm,
		Kubeconfig: "/tmp/kubeconfig",
		AppsState:  filepath.Join(dir, "apps-state.json"),
	})
	info, err := in.Release(catalog.Entry{Name: "demo", Namespace: "demo"}, "")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if info.Chart != "demo" || info.ChartVersion != "0.1.2" || info.AppVersion != "1.0.0" {
		t.Fatalf("chart metadata = %+v", info)
	}
	if info.Revision != 4 || info.Status != "deployed" {
		t.Fatalf("release metadata = %+v", info)
	}
	auth, _ := info.Values["auth"].(map[string]any)
	if auth["password"] != "***" || auth["existingSecret"] != "***" {
		t.Fatalf("auth values were not redacted: %+v", auth)
	}
	nested, _ := info.Values["nested"].([]any)
	first, _ := nested[0].(map[string]any)
	if first["token"] != "***" {
		t.Fatalf("nested token was not redacted: %+v", first)
	}
	joined := strings.Join(info.Redacted, ",")
	for _, want := range []string{"auth.password", "auth.existingSecret", "nested[0].token"} {
		if !strings.Contains(joined, want) {
			t.Errorf("redacted paths %q do not contain %q", joined, want)
		}
	}
	media, _ := info.Values["media"].(map[string]any)
	if media["accessMode"] != "ReadWriteMany" {
		t.Fatalf("non-secret value changed: %+v", media)
	}
}

func TestReleaseRejectsInvalidStatusJSON(t *testing.T) {
	dir := t.TempDir()
	helm := filepath.Join(dir, "helm")
	script := "#!/bin/sh\necho 'not json'\n"
	if err := os.WriteFile(helm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := New(Config{Helm: helm, Kubeconfig: "/tmp/kubeconfig"})
	if _, err := in.Release(catalog.Entry{Name: "demo", Namespace: "demo"}, ""); err == nil {
		t.Fatal("expected a JSON parse error")
	}
}
