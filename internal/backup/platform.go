package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// platformDataNamespaces hold platform data that lives in a PVC but must
// survive a rebuild: the directory (LLDAP), Authelia's 2FA/session DB and
// Traefik's ACME storage. The console's own PVC is in state/ instead.
var platformDataNamespaces = map[string]bool{
	"identity":    true,
	"kube-system": true,
}

// platformResources is the object set a platform backup exports. Transient or
// derived objects (pods, events, endpoints, replicasets, leases, controller
// revisions) are omitted: the controllers recreate them.
var platformResources = []struct {
	resource   string
	namespaced bool
}{
	{"namespaces", false},
	{"customresourcedefinitions.apiextensions.k8s.io", false},
	{"storageclasses.storage.k8s.io", false},
	{"ingressclasses.networking.k8s.io", false},
	{"clusterroles.rbac.authorization.k8s.io", false},
	{"clusterrolebindings.rbac.authorization.k8s.io", false},
	{"persistentvolumes", false},
	{"volumesnapshotclasses.snapshot.storage.k8s.io", false},
	{"configmaps", true},
	{"secrets", true},
	{"services", true},
	{"serviceaccounts", true},
	{"persistentvolumeclaims", true},
	{"deployments.apps", true},
	{"statefulsets.apps", true},
	{"daemonsets.apps", true},
	{"cronjobs.batch", true},
	{"ingresses.networking.k8s.io", true},
	{"networkpolicies.networking.k8s.io", true},
	{"roles.rbac.authorization.k8s.io", true},
	{"rolebindings.rbac.authorization.k8s.io", true},
	{"ingressroutes.traefik.io", true},
	{"ingressroutetcps.traefik.io", true},
	{"ingressrouteudps.traefik.io", true},
	{"middlewares.traefik.io", true},
	{"middlewaretcps.traefik.io", true},
	{"tlsoptions.traefik.io", true},
	{"tlsstores.traefik.io", true},
	{"serverstransports.traefik.io", true},
	{"traefikservices.traefik.io", true},
	{"helmcharts.helm.cattle.io", true},
	{"helmchartconfigs.helm.cattle.io", true},
}

// CreatePlatform writes one platform archive: a Kubernetes object export, the
// console state and host seed, and the platform volumes (LLDAP, Authelia,
// Traefik ACME). It is separate from the data backup on purpose: restoring it
// rebuilds the platform, while the data backup carries the applications.
func (m *Manager) CreatePlatform(ctx context.Context, note string) (Archive, error) {
	dir, err := m.targetDir()
	if err != nil {
		return Archive{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Archive{}, fmt.Errorf("backup directory: %w", err)
	}
	now := m.cfg.Now().UTC().Truncate(time.Second)
	keep := m.cfg.Keep
	if m.cfg.Schedule != nil {
		keep, _ = m.cfg.Schedule.Policy()
	}
	name := uniqueArchiveName(dir, now, KindPlatform)
	staging, err := os.MkdirTemp(dir, ".staging-")
	if err != nil {
		return Archive{}, err
	}
	defer os.RemoveAll(staging)

	mf := Manifest{
		Schema:  Schema,
		Created: now,
		Version: m.cfg.Version,
		Zone:    m.cfg.Zone,
		Note:    note,
		Kind:    KindPlatform,
	}
	if err := m.stageTree(staging, "state", m.cfg.StateDir, stateSkipDirs, &mf); err != nil {
		return Archive{}, err
	}
	if filepath.Clean(m.cfg.SeedDir) != filepath.Clean(m.cfg.StateDir) {
		if err := m.stageTree(staging, "seed", m.cfg.SeedDir, nil, &mf); err != nil {
			return Archive{}, err
		}
	}
	objects, warns := m.platformObjects()
	mf.Warnings = append(mf.Warnings, warns...)
	if err := stageBytes(staging, "platform", "k8s-objects.yaml", []byte(objects), &mf); err != nil {
		return Archive{}, err
	}
	if err := stageBytes(staging, "platform", "README.txt", platformReadme(), &mf); err != nil {
		return Archive{}, err
	}

	// Platform volumes: the identity stack's databases and Traefik's ACME
	// storage. They are copied with the same helper pod as app data, because a
	// single-node local-path/NFS volume cannot be snapshotted.
	base := strings.TrimSuffix(name, ".tar.gz")
	pvcs, err := m.pvcs()
	if err != nil {
		mf.Warnings = append(mf.Warnings, "platform volumes were not copied: "+shortErr(err))
	} else {
		for _, p := range pvcs {
			if !platformDataNamespaces[p.Namespace] {
				continue
			}
			rec := m.copyVolume(ctx, p.Namespace, p.Name, base)
			mf.Data = append(mf.Data, rec)
			if rec.Error != "" {
				mf.Warnings = append(mf.Warnings, "copying "+p.Namespace+"/"+p.Name+": "+rec.Error)
			}
		}
	}

	cleanupData := func() {
		for _, d := range mf.Data {
			if d.File != "" {
				os.Remove(filepath.Join(dir, d.File))
			}
		}
	}
	raw, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		cleanupData()
		return Archive{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), raw, 0o600); err != nil {
		cleanupData()
		return Archive{}, err
	}
	target := filepath.Join(dir, name)
	if err := writeArchive(target, staging); err != nil {
		os.Remove(target)
		cleanupData()
		return Archive{}, err
	}
	if keep > 0 {
		if _, err := m.prune(keep, name); err != nil {
			mf.Warnings = append(mf.Warnings, "pruning old backups: "+err.Error())
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		return Archive{}, err
	}
	a := archiveFrom(name, info.Size(), mf)
	a.modTime = info.ModTime()
	return a, nil
}

// platformObjects exports the object set. A missing resource type (a CRD that
// is not installed) is skipped silently; any other error is a warning, because
// an incomplete object export is still useful next to the volume copies.
//
// The output is sanitized so it can be replayed: server-assigned metadata,
// status, controller ownership, allocated Service IPs and PVC bindings are
// removed. PersistentVolumes and VolumeSnapshots are not exported: they are
// recreated by the provisioner/snapshot controller, and the data half is the
// per-volume copy.
func (m *Manager) platformObjects() (string, []string) {
	var b strings.Builder
	var warns []string
	for _, r := range platformResources {
		args := []string{"--kubeconfig", m.cfg.Kubeconfig, "get", r.resource, "-o", "yaml", "--ignore-not-found"}
		if r.namespaced {
			args = []string{"--kubeconfig", m.cfg.Kubeconfig, "get", r.resource, "-A", "-o", "yaml", "--ignore-not-found"}
		}
		out, err := m.cfg.Run(m.cfg.Kubectl, "", args...)
		if err != nil {
			if strings.Contains(err.Error(), "doesn't have a resource type") ||
				strings.Contains(err.Error(), "not found") {
				continue
			}
			warns = append(warns, "exporting "+r.resource+": "+shortErr(err))
			continue
		}
		clean, err := sanitizeObjects(out)
		if err != nil {
			warns = append(warns, "cleaning "+r.resource+": "+shortErr(err))
			continue
		}
		if clean == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n---\n")
		}
		b.WriteString(clean)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "", warns
	}
	return "# Kubernetes objects exported by umkim-thin.\n" +
		"# Apply with: kubectl apply --server-side --force-conflicts -f k8s-objects.yaml\n" + b.String(), warns
}

// sanitizeObjects cleans one kubectl List so it can be applied to a fresh
// cluster. Server-assigned fields make a client-side apply fail (a stale
// resourceVersion conflicts, a controller-owned Job cannot be created), and
// Helm release Secrets exceed the client-side apply annotation limit, so the
// README uses server-side apply.
func sanitizeObjects(raw string) (string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return "", err
	}
	items, _ := doc["items"].([]any)
	kept := make([]any, 0, len(items))
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok || !sanitizeObject(obj) {
			continue
		}
		kept = append(kept, obj)
	}
	if len(kept) == 0 {
		return "", nil
	}
	doc["items"] = kept
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sanitizeObject strips fields that must not be replayed. It returns false for
// an object owned by a controller: the controller recreates it.
func sanitizeObject(obj map[string]any) bool {
	if kind, _ := obj["kind"].(string); kind == "" {
		return false
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta != nil {
		if refs, ok := meta["ownerReferences"].([]any); ok && len(refs) > 0 {
			return false
		}
		for _, k := range []string{
			"resourceVersion", "uid", "creationTimestamp", "generation",
			"selfLink", "managedFields", "ownerReferences",
			"deletionTimestamp", "deletionGracePeriodSeconds",
		} {
			delete(meta, k)
		}
		if ann, ok := meta["annotations"].(map[string]any); ok {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
			if len(ann) == 0 {
				delete(meta, "annotations")
			}
		}
	}
	delete(obj, "status")
	kind, _ := obj["kind"].(string)
	// Cluster identity is regenerated by the installer; replaying it makes the
	// fresh API server present the old cluster's certificate (or a stale root
	// CA) and every client fails TLS verification.
	if meta != nil {
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		if kind == "Secret" && ns == "kube-system" && name == "k3s-serving" {
			return false
		}
		if kind == "ConfigMap" && name == "kube-root-ca.crt" {
			return false
		}
	}
	if kind == "PersistentVolume" {
		// Dynamic PVs are recreated by their provisioner; a static PV (the NFS
		// provisioner's root, for example) must be restored or its PVC never
		// binds. The claimRef is dropped so the selector can bind it.
		if meta != nil {
			if ann, ok := meta["annotations"].(map[string]any); ok {
				if _, dynamic := ann["pv.kubernetes.io/provisioned-by"]; dynamic {
					return false
				}
			}
		}
		if spec, ok := obj["spec"].(map[string]any); ok {
			delete(spec, "claimRef")
		}
	}
	if spec, ok := obj["spec"].(map[string]any); ok {
		// A pod-template restart marker is a rollout trigger, not state: keeping
		// it makes applying the export restart the workload (the console
		// included, which can kill the restore itself).
		if tmpl, ok := spec["template"].(map[string]any); ok {
			if tmeta, ok := tmpl["metadata"].(map[string]any); ok {
				if ann, ok := tmeta["annotations"].(map[string]any); ok {
					delete(ann, "kubectl.kubernetes.io/restartedAt")
					if len(ann) == 0 {
						delete(tmeta, "annotations")
					}
				}
			}
		}
		switch kind {
		case "Service":
			// A headless Service ("None") is a selector contract, not an
			// allocation; dropping it turns it into a ClusterIP and breaks
			// anything that resolves the pods through it.
			if ip, _ := spec["clusterIP"].(string); ip != "None" {
				delete(spec, "clusterIP")
				delete(spec, "clusterIPs")
			}
			delete(spec, "healthCheckNodePort")
		case "PersistentVolumeClaim":
			delete(spec, "volumeName")
			// Binding annotations from the old cluster make the new binder
			// report the claim as Lost (bind-completed without a volume).
			if ann, ok := meta["annotations"].(map[string]any); ok {
				for _, k := range []string{
					"pv.kubernetes.io/bind-completed",
					"pv.kubernetes.io/bound-by-controller",
					"volume.beta.kubernetes.io/storage-provisioner",
					"volume.kubernetes.io/storage-provisioner",
					"volume.kubernetes.io/selected-node",
				} {
					delete(ann, k)
				}
			}
		}
	}
	return true
}

func platformReadme() []byte {
	return []byte(`umkim-thin platform backup

Contents
  state/                       the console PVC (state, secrets, per-app values)
  seed/                        the host seed mounted into the console
  platform/k8s-objects.yaml    every Kubernetes object, sanitized for re-apply
  <archive>-data-*.tar.gz      platform volumes (LLDAP, Authelia, Traefik ACME)

The console restores state/ and seed/ (as for a data backup) but never applies
the object export or the volume copies: replacing live objects or volumes is a
manual, deliberate operation.

Rebuild a cluster
  1. install a fresh platform of the same version
  2. kubectl apply --server-side --force-conflicts -f platform/k8s-objects.yaml
  3. extract the volume copies into their PVCs (stop the identity stack first)
  4. umkim-thin backup restore <archive> --apps

Application data is not here: use a data backup (the Backups screen) for apps
and their volumes.
`)
}

// stageBytes writes one generated file into the staging tree and records it in
// the manifest, so verification covers it like a staged file.
func stageBytes(staging, prefix, name string, data []byte, mf *Manifest) error {
	rel := path.Join(prefix, name)
	dest := filepath.Join(staging, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	mf.Files = append(mf.Files, FileRecord{
		Path:   rel,
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]),
	})
	return nil
}
