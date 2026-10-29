package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rborso/umkim-thin/internal/catalog"
	"gopkg.in/yaml.v3"
)

// PVCOverride is a per-PVC install-time override. Empty fields keep the
// chart's rendered value. The key is the PVC's metadata.name, which is what
// the console shows before the install.
type PVCOverride struct {
	StorageClass string `json:"storageClass,omitempty"`
	Size         string `json:"size,omitempty"`
}

type postRenderOverrides struct {
	PVCs      map[string]PVCOverride        `json:"pvcs,omitempty"`
	Resources map[string]ContainerResources `json:"resources,omitempty"`
}

// PVCPlan describes one PVC rendered by a chart. It is returned by the storage
// planner and is also the input for the console's advanced install table.
type PVCPlan struct {
	Name         string   `json:"name"`
	Namespace    string   `json:"namespace,omitempty"`
	StorageClass string   `json:"storageClass,omitempty"`
	Size         string   `json:"size,omitempty"`
	AccessModes  []string `json:"accessModes,omitempty"`
}

var (
	namespaceRE  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	pvcNameRE    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	sizeRE       = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E)?$`)
	storageClsRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// ValidateNamespace checks a namespace before it is used in a Helm release.
// The platform's own namespaces and Kubernetes system namespaces are not valid
// targets for an application: moving an infrastructure release there would
// break the services that expect to find it.
func ValidateNamespace(ns string) error {
	if ns == "" {
		return errors.New("namespace is empty")
	}
	if len(ns) > 63 || !namespaceRE.MatchString(ns) {
		return fmt.Errorf("invalid namespace %q: use a DNS label (lowercase letters, digits and hyphens)", ns)
	}
	switch ns {
	case "default", "kube-system", "kube-public", "kube-node-lease",
		"longhorn-system", "identity", "umkim-thin", "wireguard", "nfs-provisioner":
		return fmt.Errorf("namespace %q is reserved for the platform", ns)
	}
	return nil
}

func validatePVCOverrides(overrides map[string]PVCOverride) error {
	for name, o := range overrides {
		if !pvcNameRE.MatchString(name) || len(name) > 253 {
			return fmt.Errorf("invalid PVC name %q", name)
		}
		if o.StorageClass != "" && !storageClsRE.MatchString(o.StorageClass) {
			return fmt.Errorf("invalid storage class %q for PVC %s", o.StorageClass, name)
		}
		if o.Size != "" && !sizeRE.MatchString(o.Size) {
			return fmt.Errorf("invalid size %q for PVC %s (use e.g. 1Gi or 500Mi)", o.Size, name)
		}
	}
	return nil
}

// installedNamespace returns the namespace recorded for an app, falling back
// to the catalog namespace for installs created before the namespace override
// existed.
func (in *Installer) installedNamespace(name, fallback string) string {
	if c := in.loadContributions()[name]; c.Namespace != "" {
		return c.Namespace
	}
	return fallback
}

// resolveNamespace applies the install-time namespace choice, validates it,
// and refuses to move an already-installed release. The namespace is persisted
// by saveRoutes after a successful install.
func (in *Installer) resolveNamespace(e *catalog.Entry, opts *Options) error {
	existing := in.loadContributions()[e.Name].Namespace
	requested := strings.TrimSpace(opts.Namespace)
	if requested == "" {
		requested = existing
	}
	if requested == "" {
		requested = e.Namespace
	}
	if existing != "" && requested != existing {
		return fmt.Errorf("%s is already installed in namespace %q; uninstall it before moving it to %q", e.Name, existing, requested)
	}
	// The catalog's own namespace is allowed even for the platform's
	// infrastructure entries; ValidateNamespace only guards a deliberate move
	// into a system namespace.
	if requested == e.Namespace {
		e.Namespace = requested
		opts.Namespace = requested
		return nil
	}
	if err := ValidateNamespace(requested); err != nil {
		return err
	}
	// The media stack and the gateway have cluster-level wiring that is not
	// parameterised by the chart namespace yet.
	switch e.Name {
	case ServarrApp, "wireguard":
		if requested != firstNonEmpty(existing, e.Namespace) {
			return fmt.Errorf("%s must stay in namespace %q; its bootstrap/gateway wiring is namespace-specific", e.Name, firstNonEmpty(existing, e.Namespace))
		}
	}
	e.Namespace = requested
	opts.Namespace = requested
	return nil
}

// StoragePlan renders a chart and returns its PersistentVolumeClaims. It is a
// read-only preview for the console's advanced install form: the install uses
// the same PVC names and applies the operator's overrides with a Helm
// post-renderer.
func (in *Installer) StoragePlan(e catalog.Entry, opts Options) ([]PVCPlan, error) {
	plan, err := in.RenderPlan(e, opts)
	if err != nil {
		return nil, err
	}
	return plan.PVCs, nil
}

// RenderPlan renders a chart into its PVC and container plan. It is used by
// the console's advanced form and by preflight validation.
func (in *Installer) RenderPlan(e catalog.Entry, opts Options) (InstallPlan, error) {
	if err := e.Validate(); err != nil {
		return InstallPlan{}, err
	}
	if err := in.resolveNamespace(&e, &opts); err != nil {
		return InstallPlan{}, err
	}
	if opts.ChartVersion != "" {
		e.Chart.Version = opts.ChartVersion
	}
	ctx, err := in.context(e, opts)
	if err != nil {
		return InstallPlan{}, err
	}
	values := in.renderValues(e, ctx)
	raw, err := yaml.Marshal(values)
	if err != nil {
		return InstallPlan{}, err
	}
	dir, err := os.MkdirTemp("", "umkim-thin-plan-*")
	if err != nil {
		return InstallPlan{}, err
	}
	defer os.RemoveAll(dir)
	valuesFile := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesFile, raw, 0o600); err != nil {
		return InstallPlan{}, err
	}

	chart := in.chartRef(e)
	args := []string{"template", e.Name, chart,
		"--namespace", e.Namespace,
		"--values", valuesFile}
	if strings.HasPrefix(chart, "oci://") {
		args = append(args, "--version", e.Chart.Version)
	}
	out, err := in.run(in.cfg.Helm, args...)
	if err != nil {
		return InstallPlan{}, fmt.Errorf("rendering install plan: %w\n%s", err, out)
	}
	manifests := []byte(out)
	// Apply the same overrides the install's post-renderer will apply, so a
	// plan/storage preview shows the class, size and resources that will exist.
	if len(opts.PVCs) > 0 || len(opts.Resources) > 0 {
		manifests, err = applyPostRender(bytes.NewReader(manifests), postRenderOverrides{
			PVCs: opts.PVCs, Resources: opts.Resources,
		})
		if err != nil {
			return InstallPlan{}, err
		}
	}
	plan := parseInstallPlan(string(manifests), e.Namespace)
	// A typo in a PVC or container name must not look like a successful
	// install: the override would silently do nothing.
	if err := plan.checkOverrides(opts.PVCs, opts.Resources); err != nil {
		return InstallPlan{}, err
	}
	return plan, nil
}

// checkOverrides rejects an override that matches nothing in the render.
func (p InstallPlan) checkOverrides(pvcs map[string]PVCOverride, resources map[string]ContainerResources) error {
	if len(pvcs) > 0 {
		names := make([]string, 0, len(p.PVCs))
		known := map[string]bool{}
		for _, c := range p.PVCs {
			names = append(names, c.Name)
			known[c.Name] = true
		}
		for name := range pvcs {
			if !known[name] {
				return fmt.Errorf("no PVC named %q in this chart (rendered: %s)", name, strings.Join(names, ", "))
			}
		}
	}
	if len(resources) > 0 {
		keys := make([]string, 0, len(p.Containers))
		known := map[string]bool{}
		for _, c := range p.Containers {
			keys = append(keys, c.Key)
			known[c.Key] = true
		}
		for key := range resources {
			if !known[key] {
				return fmt.Errorf("no container %q in this chart (rendered: %s)", key, strings.Join(keys, ", "))
			}
		}
	}
	return nil
}

// storagePostRenderer writes a tiny Helm post-renderer. Helm invokes the
// executable with the rendered manifests on stdin; the hidden helm-postrender
// command rewrites only the named PVCs, so chart-specific value paths do not
// have to be known by the platform.
func (in *Installer) storagePostRenderer(pvcs map[string]PVCOverride, resources map[string]ContainerResources) (string, func(), error) {
	cleanup := func() {}
	cleanPVCs := make(map[string]PVCOverride, len(pvcs))
	for name, o := range pvcs {
		if o.StorageClass != "" || o.Size != "" {
			cleanPVCs[name] = o
		}
	}
	cleanResources := make(map[string]ContainerResources, len(resources))
	for key, o := range resources {
		if o.CPURequest != "" || o.CPULimit != "" || o.MemoryRequest != "" || o.MemoryLimit != "" {
			cleanResources[key] = o
		}
	}
	if len(cleanPVCs) == 0 && len(cleanResources) == 0 {
		return "", cleanup, nil
	}
	if err := validatePVCOverrides(cleanPVCs); err != nil {
		return "", cleanup, err
	}
	if err := validateResourceOverrides(cleanResources); err != nil {
		return "", cleanup, err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", cleanup, fmt.Errorf("locating the umkim-thin executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", cleanup, err
	}
	dir, err := os.MkdirTemp("", "umkim-thin-postrender-*")
	if err != nil {
		return "", cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	overridesFile := filepath.Join(dir, "postrender-overrides.json")
	body, err := json.Marshal(postRenderOverrides{PVCs: cleanPVCs, Resources: cleanResources})
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := os.WriteFile(overridesFile, body, 0o600); err != nil {
		cleanup()
		return "", func() {}, err
	}
	script := filepath.Join(dir, "postrender.sh")
	content := "#!/bin/sh\nexec " + shellQuote(exe) + " helm-postrender " + shellQuote(overridesFile) + "\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return script, cleanup, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// PostRender is the hidden `helm-postrender` command used by Helm. It reads
// the rendered multi-document YAML and applies the PVC overrides from the JSON
// file. It is deliberately small and dependency-free so it also works inside
// the console image.
func PostRender(overridesFile string) error {
	raw, err := os.ReadFile(overridesFile)
	if err != nil {
		return fmt.Errorf("reading PVC overrides: %w", err)
	}
	var overrides postRenderOverrides
	if err := json.Unmarshal(raw, &overrides); err != nil {
		return fmt.Errorf("parsing post-render overrides: %w", err)
	}
	out, err := applyPostRender(os.Stdin, overrides)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}

// applyPostRender rewrites rendered manifests with the install's PVC and
// container-resource overrides. Helm calls it through PostRender; the install
// plan calls it directly so a preview shows what Helm will actually create.
func applyPostRender(in io.Reader, overrides postRenderOverrides) ([]byte, error) {
	if err := validatePVCOverrides(overrides.PVCs); err != nil {
		return nil, err
	}
	if err := validateResourceOverrides(overrides.Resources); err != nil {
		return nil, err
	}
	var docs bytes.Buffer
	dec := yaml.NewDecoder(in)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing rendered manifest: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		if err := applyPVCOverride(doc, overrides.PVCs); err != nil {
			return nil, err
		}
		if err := applyContainerOverrides(doc, overrides.Resources); err != nil {
			return nil, err
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, err
		}
		docs.WriteString("---\n")
		docs.Write(out)
	}
	return docs.Bytes(), nil
}

func applyPVCOverride(doc map[string]any, overrides map[string]PVCOverride) error {
	kind, _ := doc["kind"].(string)
	if kind != "PersistentVolumeClaim" {
		return nil
	}
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	o, ok := overrides[name]
	if !ok {
		return nil
	}
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		return fmt.Errorf("PVC %s has no spec", name)
	}
	if o.StorageClass != "" {
		spec["storageClassName"] = o.StorageClass
	}
	if o.Size != "" {
		resources, _ := spec["resources"].(map[string]any)
		if resources == nil {
			resources = map[string]any{}
			spec["resources"] = resources
		}
		requests, _ := resources["requests"].(map[string]any)
		if requests == nil {
			requests = map[string]any{}
			resources["requests"] = requests
		}
		requests["storage"] = o.Size
	}
	return nil
}

func applyContainerOverrides(doc map[string]any, overrides map[string]ContainerResources) error {
	if len(overrides) == 0 {
		return nil
	}
	kind, _ := doc["kind"].(string)
	if !isWorkloadKind(kind) {
		return nil
	}
	meta, _ := doc["metadata"].(map[string]any)
	workload, _ := meta["name"].(string)
	if workload == "" {
		return nil
	}
	spec, _ := doc["spec"].(map[string]any)
	podSpec := podSpecFor(kind, spec)
	if podSpec == nil {
		return nil
	}
	for _, item := range []struct {
		key  string
		init bool
	}{
		{"initContainers", true},
		{"containers", false},
	} {
		containers, _ := podSpec[item.key].([]any)
		for _, raw := range containers {
			container, _ := raw.(map[string]any)
			name, _ := container["name"].(string)
			if name == "" {
				continue
			}
			o, ok := overrides[containerKey(kind, workload, name, item.init)]
			if !ok {
				continue
			}
			resources, _ := container["resources"].(map[string]any)
			if resources == nil {
				resources = map[string]any{}
				container["resources"] = resources
			}
			setResourceValue(resources, "requests", "cpu", o.CPURequest)
			setResourceValue(resources, "limits", "cpu", o.CPULimit)
			setResourceValue(resources, "requests", "memory", o.MemoryRequest)
			setResourceValue(resources, "limits", "memory", o.MemoryLimit)
		}
	}
	return nil
}

func setResourceValue(resources map[string]any, section, name, value string) {
	if value == "" {
		return
	}
	m, _ := resources[section].(map[string]any)
	if m == nil {
		m = map[string]any{}
		resources[section] = m
	}
	m[name] = value
}

func parsePVCPlans(manifests, namespace string) []PVCPlan {
	plans := []PVCPlan{}
	dec := yaml.NewDecoder(strings.NewReader(manifests))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		kind, _ := doc["kind"].(string)
		if kind != "PersistentVolumeClaim" {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name == "" {
			continue
		}
		ns, _ := meta["namespace"].(string)
		if ns == "" {
			ns = namespace
		}
		spec, _ := doc["spec"].(map[string]any)
		class, _ := spec["storageClassName"].(string)
		size := ""
		if resources, ok := spec["resources"].(map[string]any); ok {
			if requests, ok := resources["requests"].(map[string]any); ok {
				size, _ = requests["storage"].(string)
			}
		}
		var modes []string
		if rawModes, ok := spec["accessModes"].([]any); ok {
			for _, m := range rawModes {
				if s, ok := m.(string); ok {
					modes = append(modes, s)
				}
			}
		}
		plans = append(plans, PVCPlan{Name: name, Namespace: ns, StorageClass: class, Size: size, AccessModes: modes})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Name < plans[j].Name })
	return plans
}
