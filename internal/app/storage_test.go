package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
	"gopkg.in/yaml.v3"
)

func TestApplyPVCOverride(t *testing.T) {
	manifests := `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: app-data
spec:
  storageClassName: old-class
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi
---
apiVersion: v1
kind: Service
metadata:
  name: unchanged
`
	plans := parsePVCPlans(manifests, "demo")
	if len(plans) != 1 {
		t.Fatalf("plans = %+v", plans)
	}
	if plans[0].Name != "app-data" || plans[0].StorageClass != "old-class" || plans[0].Size != "10Gi" {
		t.Fatalf("plan = %+v", plans[0])
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(strings.Split(manifests, "---")[0]), &doc); err != nil {
		t.Fatal(err)
	}
	if err := applyPVCOverride(doc, map[string]PVCOverride{
		"app-data": {StorageClass: "new-class", Size: "2Gi"},
	}); err != nil {
		t.Fatal(err)
	}
	spec := doc["spec"].(map[string]any)
	if spec["storageClassName"] != "new-class" {
		t.Fatalf("class = %#v", spec["storageClassName"])
	}
	resources := spec["resources"].(map[string]any)
	requests := resources["requests"].(map[string]any)
	if requests["storage"] != "2Gi" {
		t.Fatalf("size = %#v", requests["storage"])
	}
}

func TestResolveNamespaceKeepsInstalledNamespace(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "apps-state.json")
	if err := os.WriteFile(state, []byte(`{"demo":{"namespace":"custom"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in := New(Config{AppsState: state})
	e := catalog.Entry{Name: "demo", Namespace: "demo"}
	opts := Options{}
	if err := in.resolveNamespace(&e, &opts); err != nil {
		t.Fatal(err)
	}
	if e.Namespace != "custom" || opts.Namespace != "custom" {
		t.Fatalf("namespace = %q/%q", e.Namespace, opts.Namespace)
	}
	opts = Options{Namespace: "other"}
	if err := in.resolveNamespace(&e, &opts); err == nil {
		t.Fatal("moving an installed namespace was accepted")
	}
}

func TestValidateNamespaceAndPVCs(t *testing.T) {
	for _, ns := range []string{"servarr", "app-one", "a1"} {
		if err := ValidateNamespace(ns); err != nil {
			t.Fatalf("valid namespace %q: %v", ns, err)
		}
	}
	for _, ns := range []string{"", "Kube", "kube-system", "default", "has space", "-bad"} {
		if err := ValidateNamespace(ns); err == nil {
			t.Fatalf("namespace %q accepted", ns)
		}
	}
	if err := validatePVCOverrides(map[string]PVCOverride{
		"app-data": {StorageClass: "longhorn-rwo", Size: "2Gi"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := validatePVCOverrides(map[string]PVCOverride{
		"app-data": {Size: "2GiB"},
	}); err == nil {
		t.Fatal("invalid size accepted")
	}
}
