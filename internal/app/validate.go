package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// Validate renders a chart with the same install choices and checks whether the
// requested PVC sizes and container resources fit the live cluster. Warnings
// are advisory; errors are intended to stop the Helm install before it creates
// a pending release.
func (in *Installer) Validate(e catalog.Entry, opts Options) (ValidationResult, error) {
	if err := in.resolveNamespace(&e, &opts); err != nil {
		return ValidationResult{}, err
	}
	plan, err := in.RenderPlan(e, opts)
	if err != nil {
		return ValidationResult{}, err
	}
	plan = applyPlanOverrides(plan, opts)
	result := ValidationResult{Plan: plan}
	addErr := func(scope, format string, a ...any) {
		result.Errors = append(result.Errors, Check{Scope: scope, Message: fmt.Sprintf(format, a...)})
	}
	addWarn := func(scope, format string, a ...any) {
		result.Warnings = append(result.Warnings, Check{Scope: scope, Message: fmt.Sprintf(format, a...)})
	}

	in.validatePVCs(e, opts, plan.PVCs, addErr, addWarn)
	in.validateResources(e, opts, plan.Containers, addErr, addWarn)

	sort.Slice(result.Errors, func(i, j int) bool {
		if result.Errors[i].Scope == result.Errors[j].Scope {
			return result.Errors[i].Message < result.Errors[j].Message
		}
		return result.Errors[i].Scope < result.Errors[j].Scope
	})
	sort.Slice(result.Warnings, func(i, j int) bool {
		if result.Warnings[i].Scope == result.Warnings[j].Scope {
			return result.Warnings[i].Message < result.Warnings[j].Message
		}
		return result.Warnings[i].Scope < result.Warnings[j].Scope
	})
	result.OK = len(result.Errors) == 0
	return result, nil
}

// ErrorString formats the blocking checks for the CLI and the install job.
func (v ValidationResult) ErrorString() string {
	var b strings.Builder
	for i, e := range v.Errors {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %s: %s", e.Scope, e.Message)
	}
	return b.String()
}

type addCheck func(scope, format string, a ...any)

func (in *Installer) validatePVCs(e catalog.Entry, opts Options, pvcs []PVCPlan, addErr, addWarn addCheck) {
	classes, err := in.StorageClasses()
	if err != nil {
		addWarn("storage", "could not list StorageClasses: %v", err)
	}
	classByName := map[string]StorageClass{}
	defaultClass := ""
	for _, c := range classes {
		classByName[c.Name] = c
		if c.Default {
			defaultClass = c.Name
		}
	}
	existing := in.existingPVCs(e.Namespace)
	replicas := in.longhornReplicaCount()
	requiredLonghorn := int64(0)
	nfsWarned := map[string]bool{}
	for _, p := range pvcs {
		class := p.StorageClass
		if class == "" {
			class = defaultClass
		}
		if class == "" {
			addErr(p.Name, "no storage class selected and the cluster has no default StorageClass")
			continue
		}
		sc, ok := classByName[class]
		if !ok {
			// If StorageClasses could not be listed, do not turn a temporary
			// API problem into a false install blocker.
			if len(classByName) > 0 {
				addErr(p.Name, "storage class %q does not exist", class)
			}
			continue
		}
		size, err := parseMemoryQuantity(p.Size)
		if err != nil || size <= 0 {
			addErr(p.Name, "invalid or empty size %q", p.Size)
			continue
		}
		if _, exists := existing[p.Name]; exists {
			addWarn(p.Name, "claim already exists; capacity and class changes are not re-estimated")
			continue
		}
		if strings.Contains(strings.ToLower(sc.Provisioner), "longhorn") || strings.Contains(sc.Name, "longhorn") {
			requiredLonghorn += size * int64(replicas)
		} else if strings.Contains(strings.ToLower(sc.Provisioner), "nfs") || strings.Contains(sc.Name, "nfs") {
			if !nfsWarned[class] {
				addWarn(p.Name, "class %q is NFS-backed; the external export's free space cannot be checked from Kubernetes", class)
				nfsWarned[class] = true
			}
		} else {
			addWarn(p.Name, "class %q has no capacity probe; free space is not checked", class)
		}
	}
	if requiredLonghorn > 0 {
		freeNominal, actualFree, err := in.longhornCapacity()
		if err != nil {
			addWarn("longhorn", "could not read Longhorn capacity: %v", err)
			return
		}
		if requiredLonghorn > freeNominal {
			addErr("longhorn", "new Longhorn claims need %s nominal (%d replica(s)), but only %s nominal is free", formatBytes(requiredLonghorn), replicas, formatBytes(freeNominal))
		} else if requiredLonghorn > actualFree {
			addWarn("longhorn", "new Longhorn claims need %s nominal; actual free space is %s. Thin volumes may still be schedulable, but watch the disk", formatBytes(requiredLonghorn), formatBytes(actualFree))
		}
	}
}

func (in *Installer) validateResources(e catalog.Entry, opts Options, containers []ContainerPlan, addErr, addWarn addCheck) {
	nodes, err := in.clusterNodes()
	if err != nil {
		addWarn("resources", "could not read node allocatable resources: %v", err)
		return
	}
	if len(nodes) == 0 {
		addWarn("resources", "no schedulable nodes found; resource fit not checked")
		return
	}
	var allocCPU, allocMem float64
	for _, n := range nodes {
		allocCPU += n.CPU
		allocMem += float64(n.Memory)
	}
	existingCPU, existingMem, err := in.existingRequests(e.Namespace)
	if err != nil {
		addWarn("resources", "could not read existing pod requests: %v", err)
	}
	if existingCPU > allocCPU || existingMem > allocMem {
		addWarn("resources", "existing cluster requests already exceed current allocatable capacity; scheduling is already under pressure")
	}

	replicaCount := map[string]int{}
	for _, c := range containers {
		key := c.Key
		replicas := c.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		if c.Kind == "DaemonSet" {
			replicas = len(nodes)
		}
		replicaCount[key] = replicas
	}

	var newCPU, newMem, newCPULimit, newMemLimit float64
	missingRequests := 0
	for _, c := range containers {
		if c.Init {
			continue
		}
		replicas := replicaCount[c.Key]
		if err := checkRequestLimit(c.Key, "cpu", c.CPURequest, c.CPULimit); err != nil {
			addErr(c.Key, "%v", err)
		}
		if err := checkRequestLimit(c.Key, "memory", c.MemoryRequest, c.MemoryLimit); err != nil {
			addErr(c.Key, "%v", err)
		}
		if c.CPURequest == "" && c.MemoryRequest == "" {
			missingRequests++
		}
		if c.CPURequest != "" {
			v, err := parseCPUQuantity(c.CPURequest)
			if err != nil {
				addErr(c.Key, "invalid CPU request %q", c.CPURequest)
			} else {
				newCPU += v * float64(replicas)
			}
		}
		if c.MemoryRequest != "" {
			v, err := parseMemoryQuantity(c.MemoryRequest)
			if err != nil {
				addErr(c.Key, "invalid memory request %q", c.MemoryRequest)
			} else {
				newMem += float64(v) * float64(replicas)
			}
		}
		if c.CPULimit != "" {
			v, err := parseCPUQuantity(c.CPULimit)
			if err != nil {
				addErr(c.Key, "invalid CPU limit %q", c.CPULimit)
			} else {
				newCPULimit += v * float64(replicas)
			}
		}
		if c.MemoryLimit != "" {
			v, err := parseMemoryQuantity(c.MemoryLimit)
			if err != nil {
				addErr(c.Key, "invalid memory limit %q", c.MemoryLimit)
			} else {
				newMemLimit += float64(v) * float64(replicas)
			}
		}
	}
	if missingRequests > 0 {
		addWarn("resources", "%d container(s) have no CPU/memory request; the scheduler will not reserve capacity for them", missingRequests)
	}
	freeCPU := allocCPU - existingCPU
	freeMem := allocMem - existingMem
	if newCPU > freeCPU+0.001 {
		addErr("cpu", "new workloads request %.3f CPU, but only %.3f CPU is free after existing requests (cluster allocatable %.3f)", newCPU, freeCPU, allocCPU)
	} else if newCPU > freeCPU*0.8 && freeCPU > 0 {
		addWarn("cpu", "new workloads use %.1f%% of the remaining CPU (%.3f of %.3f)", newCPU/freeCPU*100, newCPU, freeCPU)
	}
	if newMem > freeMem+1 {
		addErr("memory", "new workloads request %s, but only %s is free after existing requests (cluster allocatable %s)", formatBytes(int64(newMem)), formatBytes(int64(freeMem)), formatBytes(int64(allocMem)))
	} else if newMem > freeMem*0.8 && freeMem > 0 {
		addWarn("memory", "new workloads use %.1f%% of the remaining memory (%s of %s)", newMem/freeMem*100, formatBytes(int64(newMem)), formatBytes(int64(freeMem)))
	}
	if newCPULimit > allocCPU && allocCPU > 0 {
		addWarn("cpu", "new CPU limits total %.3f, above allocatable %.3f; this is overcommit and may throttle under load", newCPULimit, allocCPU)
	}
	if newMemLimit > allocMem && allocMem > 0 {
		addWarn("memory", "new memory limits total %s, above allocatable %s; overcommit can trigger OOM under pressure", formatBytes(int64(newMemLimit)), formatBytes(int64(allocMem)))
	}
}

func checkRequestLimit(key, resource, request, limit string) error {
	if request == "" || limit == "" {
		return nil
	}
	var req, lim float64
	var err error
	if resource == "cpu" {
		req, err = parseCPUQuantity(request)
		if err == nil {
			lim, err = parseCPUQuantity(limit)
		}
	} else {
		var r, l int64
		r, err = parseMemoryQuantity(request)
		if err == nil {
			l, err = parseMemoryQuantity(limit)
		}
		req, lim = float64(r), float64(l)
	}
	if err != nil {
		return err
	}
	if req > lim {
		return fmt.Errorf("%s request %s is greater than limit %s", resource, request, limit)
	}
	return nil
}

type clusterNode struct {
	Name   string
	CPU    float64
	Memory int64
}

func (in *Installer) clusterNodes() ([]clusterNode, error) {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "get", "nodes", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	var doc struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, err
	}
	nodes := make([]clusterNode, 0, len(doc.Items))
	for _, item := range doc.Items {
		cpu, err := parseCPUQuantity(item.Status.Allocatable["cpu"])
		if err != nil {
			continue
		}
		mem, err := parseMemoryQuantity(item.Status.Allocatable["memory"])
		if err != nil {
			continue
		}
		nodes = append(nodes, clusterNode{Name: item.Metadata.Name, CPU: cpu, Memory: mem})
	}
	return nodes, nil
}

func (in *Installer) existingRequests(excludeNamespace string) (float64, float64, error) {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "get", "pods", "-A", "-o", "json")
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %s", err, out)
	}
	var doc struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
			Spec struct {
				Containers []struct {
					Resources struct {
						Requests map[string]string `json:"requests"`
					} `json:"resources"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return 0, 0, err
	}
	var cpu, mem float64
	for _, pod := range doc.Items {
		if pod.Metadata.Namespace == excludeNamespace {
			continue
		}
		if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		for _, c := range pod.Spec.Containers {
			if v, err := parseCPUQuantity(c.Resources.Requests["cpu"]); err == nil {
				cpu += v
			}
			if v, err := parseMemoryQuantity(c.Resources.Requests["memory"]); err == nil {
				mem += float64(v)
			}
		}
	}
	return cpu, mem, nil
}

func (in *Installer) existingPVCs(namespace string) map[string]struct{} {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", namespace, "get", "pvc", "-o", "json")
	if err != nil {
		return map[string]struct{}{}
	}
	var doc struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &doc) != nil {
		return map[string]struct{}{}
	}
	set := map[string]struct{}{}
	for _, item := range doc.Items {
		set[item.Metadata.Name] = struct{}{}
	}
	return set
}

func (in *Installer) longhornReplicaCount() int {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", "longhorn-system", "get", "settings.longhorn.io", "default-replica-count",
		"-o", "jsonpath={.value}")
	if err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

func (in *Installer) longhornCapacity() (int64, int64, error) {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", "longhorn-system", "get", "nodes.longhorn.io", "-o", "json")
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %s", err, out)
	}
	var doc struct {
		Items []struct {
			Spec struct {
				Disks map[string]struct {
					StorageReserved int64 `json:"storageReserved"`
				} `json:"disks"`
			} `json:"spec"`
			Status struct {
				DiskStatus map[string]struct {
					StorageMaximum   int64 `json:"storageMaximum"`
					StorageAvailable int64 `json:"storageAvailable"`
					StorageScheduled int64 `json:"storageScheduled"`
				} `json:"diskStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return 0, 0, err
	}
	var free, actual int64
	for _, n := range doc.Items {
		for name, d := range n.Status.DiskStatus {
			reserved := int64(0)
			if spec, ok := n.Spec.Disks[name]; ok {
				reserved = spec.StorageReserved
			}
			f := d.StorageMaximum - reserved - d.StorageScheduled
			if f > 0 {
				free += f
			}
			if d.StorageAvailable > 0 {
				actual += d.StorageAvailable
			}
		}
	}
	return free, actual, nil
}

func formatBytes(v int64) string {
	if v < 0 {
		return "0"
	}
	const (
		ki = 1024
		mi = ki * 1024
		gi = mi * 1024
		ti = gi * 1024
	)
	switch {
	case v >= ti:
		return fmt.Sprintf("%.1fTi", float64(v)/ti)
	case v >= gi:
		return fmt.Sprintf("%.1fGi", float64(v)/gi)
	case v >= mi:
		return fmt.Sprintf("%.1fMi", float64(v)/mi)
	case v >= ki:
		return fmt.Sprintf("%.1fKi", float64(v)/ki)
	default:
		return fmt.Sprintf("%dB", v)
	}
}
