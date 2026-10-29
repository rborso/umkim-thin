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

// An override that names nothing in the render is an error: a typo must not
// silently install the chart defaults (the console's advanced form and the CLI
// both feed this).
func TestPlanCheckOverrides(t *testing.T) {
	plan := InstallPlan{
		PVCs: []PVCPlan{
			{Name: "gitea-data", StorageClass: "local-path", Size: "5Gi"},
			{Name: "gitea-postgres", StorageClass: "local-path", Size: "2Gi"},
		},
		Containers: []ContainerPlan{
			{Key: "Deployment/gitea/gitea"},
			{Key: "Job/gitea-bootstrap/bootstrap"},
		},
	}
	ok := map[string]PVCOverride{"gitea-data": {StorageClass: "fast", Size: "20Gi"}}
	if err := plan.checkOverrides(ok, nil); err != nil {
		t.Fatalf("a known PVC was rejected: %v", err)
	}
	if err := plan.checkOverrides(nil, map[string]ContainerResources{
		"Deployment/gitea/gitea": {CPURequest: "200m"},
	}); err != nil {
		t.Fatalf("a known container was rejected: %v", err)
	}

	err := plan.checkOverrides(map[string]PVCOverride{"data": {Size: "20Gi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no PVC named") || !strings.Contains(err.Error(), "gitea-data") {
		t.Fatalf("unknown PVC error = %v, want the rendered names listed", err)
	}
	err = plan.checkOverrides(nil, map[string]ContainerResources{"gitea": {CPURequest: "1"}})
	if err == nil || !strings.Contains(err.Error(), "no container") {
		t.Fatalf("unknown container error = %v", err)
	}
}
