package app

import "testing"

func TestParseContainerPlanAndOverride(t *testing.T) {
	manifests := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  replicas: 2
  template:
    spec:
      initContainers:
        - name: init
          image: busybox
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
      containers:
        - name: server
          image: example/app
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              cpu: "1"
              memory: 1Gi
---
apiVersion: v1
kind: Service
metadata:
  name: ignored
`
	plan := parseInstallPlan(manifests, "demo")
	if len(plan.Containers) != 2 {
		t.Fatalf("containers = %+v", plan.Containers)
	}
	server := plan.Containers[1]
	if server.Key != "Deployment/app/server" || server.Replicas != 2 || server.CPURequest != "100m" || server.MemoryLimit != "1Gi" {
		t.Fatalf("server plan = %+v", server)
	}
	plan = applyPlanOverrides(plan, Options{Resources: map[string]ContainerResources{
		server.Key: {CPURequest: "500m", CPULimit: "2", MemoryRequest: "512Mi", MemoryLimit: "2Gi"},
	}})
	if plan.Containers[1].CPURequest != "500m" || plan.Containers[1].CPULimit != "2" {
		t.Fatalf("override = %+v", plan.Containers[1])
	}

	doc := map[string]any{
		"kind":     "Deployment",
		"metadata": map[string]any{"name": "app"},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name": "server",
							"resources": map[string]any{
								"requests": map[string]any{"cpu": "100m"},
							},
						},
					},
				},
			},
		},
	}
	if err := applyContainerOverrides(doc, map[string]ContainerResources{
		"Deployment/app/server": {CPURequest: "250m", MemoryLimit: "1Gi"},
	}); err != nil {
		t.Fatal(err)
	}
	res := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
	requests := res["requests"].(map[string]any)
	limits := res["limits"].(map[string]any)
	if requests["cpu"] != "250m" || limits["memory"] != "1Gi" {
		t.Fatalf("resources = %#v", res)
	}
}

func TestResourceQuantities(t *testing.T) {
	if v, err := parseCPUQuantity("250m"); err != nil || v != 0.25 {
		t.Fatalf("cpu 250m = %v, %v", v, err)
	}
	if v, err := parseMemoryQuantity("1Gi"); err != nil || v != 1024*1024*1024 {
		t.Fatalf("mem 1Gi = %v, %v", v, err)
	}
	if _, err := parseResourceQuantity("memory request", "not-a-size"); err == nil {
		t.Fatal("invalid memory accepted")
	}
}
