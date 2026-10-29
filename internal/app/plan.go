package app

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ContainerResources is the advanced per-container override. Empty fields keep
// the chart's rendered value.
type ContainerResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// ContainerPlan is one rendered application container. Key is stable across
// the plan and the install post-renderer.
type ContainerPlan struct {
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	Workload      string `json:"workload"`
	Container     string `json:"container"`
	Init          bool   `json:"init,omitempty"`
	Replicas      int    `json:"replicas"`
	Image         string `json:"image,omitempty"`
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// InstallPlan is the read-only render of a chart for the advanced install
// form. It contains both storage claims and workload containers.
type InstallPlan struct {
	PVCs       []PVCPlan       `json:"pvcs"`
	Containers []ContainerPlan `json:"containers"`
}

// Check is one preflight result. Warnings do not block an install; errors do.
type Check struct {
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// ValidationResult is returned by the Validate button and used internally
// before Helm runs.
type ValidationResult struct {
	OK       bool        `json:"ok"`
	Errors   []Check     `json:"errors"`
	Warnings []Check     `json:"warnings"`
	Plan     InstallPlan `json:"plan"`
}

func containerKey(kind, workload, container string, init bool) string {
	key := kind + "/" + workload + "/" + container
	if init {
		key = kind + "/" + workload + "/init/" + container
	}
	return key
}

func parseInstallPlan(manifests, namespace string) InstallPlan {
	return InstallPlan{
		PVCs:       parsePVCPlans(manifests, namespace),
		Containers: parseContainerPlans(manifests, namespace),
	}
}

func parseContainerPlans(manifests, namespace string) []ContainerPlan {
	plans := []ContainerPlan{}
	dec := yaml.NewDecoder(strings.NewReader(manifests))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		kind, _ := doc["kind"].(string)
		if !isWorkloadKind(kind) {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		workload, _ := meta["name"].(string)
		if workload == "" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		replicas := 1
		if n, ok := integerValue(spec["replicas"]); ok && n >= 0 {
			replicas = n
		}
		podSpec := podSpecFor(kind, spec)
		if podSpec == nil {
			continue
		}
		addContainers := func(listKey string, init bool) {
			raw, _ := podSpec[listKey].([]any)
			for _, item := range raw {
				cm, _ := item.(map[string]any)
				name, _ := cm["name"].(string)
				if name == "" {
					continue
				}
				res, _ := cm["resources"].(map[string]any)
				plans = append(plans, ContainerPlan{
					Key:           containerKey(kind, workload, name, init),
					Kind:          kind,
					Workload:      workload,
					Container:     name,
					Init:          init,
					Replicas:      replicas,
					Image:         stringValue(cm["image"]),
					CPURequest:    resourceValue(res, "requests", "cpu"),
					CPULimit:      resourceValue(res, "limits", "cpu"),
					MemoryRequest: resourceValue(res, "requests", "memory"),
					MemoryLimit:   resourceValue(res, "limits", "memory"),
				})
			}
		}
		addContainers("initContainers", true)
		addContainers("containers", false)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Key < plans[j].Key })
	return plans
}

func isWorkloadKind(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Job":
		return true
	}
	return false
}

func podSpecFor(kind string, spec map[string]any) map[string]any {
	if spec == nil {
		return nil
	}
	template, _ := spec["template"].(map[string]any)
	pod, _ := template["spec"].(map[string]any)
	return pod
}

func resourceValue(res map[string]any, section, key string) string {
	if res == nil {
		return ""
	}
	m, _ := res[section].(map[string]any)
	if m == nil {
		return ""
	}
	return stringValue(m[key])
}

func stringValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func integerValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}

// applyPlanOverrides applies the form's choices to a rendered plan without
// touching the cluster. It is used for preflight validation.
func applyPlanOverrides(plan InstallPlan, opts Options) InstallPlan {
	for i := range plan.PVCs {
		if o, ok := opts.PVCs[plan.PVCs[i].Name]; ok {
			if o.StorageClass != "" {
				plan.PVCs[i].StorageClass = o.StorageClass
			}
			if o.Size != "" {
				plan.PVCs[i].Size = o.Size
			}
		}
	}
	for i := range plan.Containers {
		if o, ok := opts.Resources[plan.Containers[i].Key]; ok {
			if o.CPURequest != "" {
				plan.Containers[i].CPURequest = o.CPURequest
			}
			if o.CPULimit != "" {
				plan.Containers[i].CPULimit = o.CPULimit
			}
			if o.MemoryRequest != "" {
				plan.Containers[i].MemoryRequest = o.MemoryRequest
			}
			if o.MemoryLimit != "" {
				plan.Containers[i].MemoryLimit = o.MemoryLimit
			}
		}
	}
	return plan
}

func validateResourceOverrides(overrides map[string]ContainerResources) error {
	for key, o := range overrides {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("resource override has an empty container key")
		}
		for name, value := range map[string]string{
			"cpu request":    o.CPURequest,
			"cpu limit":      o.CPULimit,
			"memory request": o.MemoryRequest,
			"memory limit":   o.MemoryLimit,
		} {
			if value != "" {
				if _, err := parseResourceQuantity(name, value); err != nil {
					return fmt.Errorf("%s for %s: %w", name, key, err)
				}
			}
		}
	}
	return nil
}

func parseResourceQuantity(scope, raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	if scope == "cpu request" || scope == "cpu limit" {
		return parseCPUQuantity(raw)
	}
	bytes, err := parseMemoryQuantity(raw)
	if err != nil {
		return 0, err
	}
	return float64(bytes), nil
}

func parseCPUQuantity(raw string) (float64, error) {
	mult := 1.0
	switch {
	case strings.HasSuffix(raw, "m"):
		mult = 0.001
		raw = strings.TrimSuffix(raw, "m")
	case strings.HasSuffix(raw, "u"):
		mult = 0.000001
		raw = strings.TrimSuffix(raw, "u")
	case strings.HasSuffix(raw, "n"):
		mult = 0.000000001
		raw = strings.TrimSuffix(raw, "n")
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid CPU quantity %q", raw)
	}
	return n * mult, nil
}

// parseMemoryQuantity returns bytes. It accepts the Kubernetes quantity forms
// used by the catalog charts: plain bytes, decimal K/M/G/T and binary Ki/Mi/Gi.
func parseMemoryQuantity(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	units := []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1024}, {"Mi", 1024 * 1024}, {"Gi", 1024 * 1024 * 1024}, {"Ti", 1024 * 1024 * 1024 * 1024},
		{"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}, {"T", 1000 * 1000 * 1000 * 1000},
		{"k", 1000},
	}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(raw, u.suffix) {
			mult = u.mult
			raw = strings.TrimSuffix(raw, u.suffix)
			break
		}
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid memory quantity %q", raw)
	}
	return int64(n * mult), nil
}
