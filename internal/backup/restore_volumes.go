package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// workloadRef is a Deployment/StatefulSet scaled to zero around a volume
// restore, with the replica count to put back.
type workloadRef struct {
	Kind      string
	Name      string
	Namespace string
	Replicas  int
}

// RestoreVolumes extracts every recorded volume copy back into its PVC. It is
// the data half of a restore: the console state is restored by Restore, this
// puts the bytes back. Workloads that mount a volume are scaled to zero first
// and restored afterwards; a workload that cannot be stopped (a DaemonSet, a
// running Job, a bare pod) blocks that volume instead of writing under a live
// app.
func (m *Manager) RestoreVolumes(ctx context.Context, name string) ([]VolumeRestore, error) {
	return m.RestoreVolumesSelected(ctx, name, nil)
}

// RestoreVolumesSelected restores only the listed namespace/pvc volumes when
// only is non-empty. It is what the CLI's --volume flag uses to retry a subset
// without re-copying every volume in the archive.
func (m *Manager) RestoreVolumesSelected(ctx context.Context, name string, only []string) ([]VolumeRestore, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid backup name %q", name)
	}
	_, mf, err := m.verifyArchive(name, false)
	if err != nil {
		return nil, fmt.Errorf("refusing to restore %s: %w", name, err)
	}
	want := make(map[string]bool, len(only))
	for _, v := range only {
		want[v] = true
	}
	out := make([]VolumeRestore, 0, len(mf.Data))
	for _, d := range mf.Data {
		if len(want) > 0 && !want[d.Namespace+"/"+d.PVC] {
			continue
		}
		if d.File == "" || d.Error != "" {
			reason := d.Error
			if reason == "" {
				reason = "no file copy was recorded"
			}
			out = append(out, VolumeRestore{Namespace: d.Namespace, PVC: d.PVC, File: d.File, Error: reason})
			continue
		}
		refs, blocked := m.stopForPVC(ctx, d.Namespace, d.PVC)
		if blocked != "" {
			out = append(out, VolumeRestore{Namespace: d.Namespace, PVC: d.PVC, File: d.File, Error: blocked})
			continue
		}
		rec := m.restoreVolume(ctx, d)
		m.startWorkloads(ctx, refs)
		out = append(out, rec)
	}
	return out, nil
}

// restoreVolume streams one sidecar copy into its PVC through a read-write
// helper pod. tar runs as root so the archived owners are restored.
func (m *Manager) restoreVolume(ctx context.Context, d DataRecord) VolumeRestore {
	rec := VolumeRestore{Namespace: d.Namespace, PVC: d.PVC, File: d.File}
	if m.cfg.HelperImage == "" {
		rec.Error = "no helper image configured (set UMKIM_THIN_CONSOLE_IMAGE)"
		return rec
	}
	dir, err := m.targetDir()
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	src := filepath.Join(dir, filepath.Base(d.File))
	f, err := os.Open(src)
	if err != nil {
		rec.Error = "opening the copy: " + shortErr(err)
		return rec
	}
	defer f.Close()
	pod := helperPodName(d.Namespace, d.PVC)
	m.deleteHelperPod(d.Namespace, pod)
	doc := helperPodYAML(pod, d.Namespace, d.PVC, m.cfg.HelperImage, false)
	if _, err := m.cfg.Run(m.cfg.Kubectl, doc, "--kubeconfig", m.cfg.Kubeconfig, "apply", "-f", "-"); err != nil {
		rec.Error = "creating the restore helper: " + shortErr(err)
		return rec
	}
	defer m.deleteHelperPod(d.Namespace, pod)
	if _, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"-n", d.Namespace, "wait", "--for=condition=Ready", "--timeout=180s", "pod/"+pod); err != nil {
		rec.Error = "the restore helper did not start: " + shortErr(err)
		return rec
	}
	err = m.cfg.RunStreamIn(ctx, m.cfg.Kubectl, f,
		"--kubeconfig", m.cfg.Kubeconfig, "-n", d.Namespace, "exec", "-i", pod, "-c", "copy",
		"--", "tar", "xzf", "-", "-C", "/data")
	if err != nil {
		rec.Error = "extracting the copy: " + shortErr(err)
		return rec
	}
	rec.Restored = true
	return rec
}

// stopForPVC scales the workloads that mount a PVC to zero and returns them so
// they can be restarted. A workload that cannot be scaled blocks the restore.
func (m *Manager) stopForPVC(ctx context.Context, ns, pvc string) ([]workloadRef, string) {
	out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"-n", ns, "get", "pods", "-o", "json")
	if err != nil {
		return nil, "listing the pods that use the volume: " + shortErr(err)
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name            string `json:"name"`
				OwnerReferences []struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Volumes []struct {
					PersistentVolumeClaim *struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
				} `json:"volumes"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pods); err != nil {
		return nil, "reading the pod list: " + shortErr(err)
	}
	var refs []workloadRef
	seen := map[string]bool{}
	var podNames []string
	for _, p := range pods.Items {
		// A finished Job's pod still references the claim but cannot write.
		// Refusing to restore because of it would block every reinstall's
		// bootstrap Job until an operator deleted it by hand.
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		uses := false
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvc {
				uses = true
				break
			}
		}
		if !uses {
			continue
		}
		if len(p.Metadata.OwnerReferences) == 0 {
			return nil, "a bare pod (" + p.Metadata.Name + ") mounts the volume; delete it first"
		}
		owner := p.Metadata.OwnerReferences[0]
		kind, name := strings.ToLower(owner.Kind), owner.Name
		if kind == "replicaset" {
			top, err := m.topOwner(ns, name)
			if err != nil {
				return nil, "finding the owner of " + p.Metadata.Name + ": " + shortErr(err)
			}
			kind, name = top.Kind, top.Name
		}
		switch kind {
		case "deployment", "statefulset":
		case "daemonset":
			return nil, "a DaemonSet mounts the volume; stop it manually first"
		default:
			return nil, fmt.Sprintf("a %s mounts the volume; stop it manually first", kind)
		}
		key := kind + "/" + name
		if !seen[key] {
			seen[key] = true
			replicas := 1
			if out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
				"-n", ns, "get", kind, name, "-o", "jsonpath={.spec.replicas}"); err == nil {
				fmt.Sscanf(strings.TrimSpace(out), "%d", &replicas)
			}
			refs = append(refs, workloadRef{Kind: kind, Name: name, Namespace: ns, Replicas: replicas})
		}
		podNames = append(podNames, p.Metadata.Name)
	}
	for _, ref := range refs {
		if _, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
			"-n", ns, "scale", ref.Kind+"/"+ref.Name, "--replicas=0"); err != nil {
			m.startWorkloads(ctx, refs)
			return nil, "stopping " + ref.Kind + "/" + ref.Name + ": " + shortErr(err)
		}
	}
	for _, pod := range podNames {
		if _, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
			"-n", ns, "wait", "--for=delete", "pod/"+pod, "--timeout=90s"); err != nil {
			m.startWorkloads(ctx, refs)
			return nil, "the pod " + pod + " did not stop: " + shortErr(err)
		}
	}
	return refs, ""
}

// topOwner resolves a ReplicaSet to its Deployment.
func (m *Manager) topOwner(ns, rs string) (workloadRef, error) {
	out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"-n", ns, "get", "replicaset", rs, "-o", "json")
	if err != nil {
		return workloadRef{}, err
	}
	var obj struct {
		Metadata struct {
			OwnerReferences []struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		return workloadRef{}, err
	}
	if len(obj.Metadata.OwnerReferences) == 0 {
		return workloadRef{}, fmt.Errorf("the ReplicaSet has no owner")
	}
	o := obj.Metadata.OwnerReferences[0]
	return workloadRef{Kind: strings.ToLower(o.Kind), Name: o.Name, Namespace: ns}, nil
}

// startWorkloads puts the scaled-down workloads back to their replica count.
func (m *Manager) startWorkloads(_ context.Context, refs []workloadRef) {
	for _, ref := range refs {
		_, _ = m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
			"-n", ref.Namespace, "scale", ref.Kind+"/"+ref.Name,
			fmt.Sprintf("--replicas=%d", ref.Replicas))
	}
}

// ApplyPlatform applies a platform archive's object export with server-side
// apply (the Helm release Secrets are too large for client-side apply). It
// returns the applied object lines; a partial apply is an error with the
// output attached.
func (m *Manager) ApplyPlatform(ctx context.Context, name string) ([]string, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid backup name %q", name)
	}
	_, mf, err := m.verifyArchive(name, false)
	if err != nil {
		return nil, fmt.Errorf("refusing to restore %s: %w", name, err)
	}
	if mf.Kind != KindPlatform {
		return nil, fmt.Errorf("%s is a %s backup, not a platform backup", name, orData(mf.Kind))
	}
	dir, err := m.targetDir()
	if err != nil {
		return nil, err
	}
	objects, err := readArchiveEntry(filepath.Join(dir, name), "platform/k8s-objects.yaml")
	if err != nil {
		return nil, err
	}
	if objects, err = dropSelfObjects(objects, m.cfg.SelfNamespace); err != nil {
		return nil, err
	}
	out, err := m.cfg.Run(m.cfg.Kubectl, string(objects), "--kubeconfig", m.cfg.Kubeconfig,
		"apply", "--server-side", "--force-conflicts", "-f", "-")
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if err != nil {
		return lines, fmt.Errorf("applying the object export: %w", err)
	}
	return lines, nil
}

// dropSelfObjects removes the console's own Deployment and RBAC from the export:
// the installer already created them, and applying an archived spec would roll
// the pod running the restore or downgrade its permissions.
func dropSelfObjects(raw []byte, ns string) ([]byte, error) {
	if ns == "" {
		return raw, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var out bytes.Buffer
	changed := false
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return raw, nil
		}
		items, _ := doc["items"].([]any)
		kept := make([]any, 0, len(items))
		for _, it := range items {
			obj, ok := it.(map[string]any)
			if !ok {
				kept = append(kept, it)
				continue
			}
			kind, _ := obj["kind"].(string)
			meta, _ := obj["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			if kind == "Deployment" && meta != nil && meta["namespace"] == ns {
				changed = true
				continue
			}
			if (kind == "ClusterRole" || kind == "ClusterRoleBinding") && strings.HasPrefix(name, "umkim-thin") {
				changed = true
				continue
			}
			kept = append(kept, it)
		}
		doc["items"] = kept
		b, err := yaml.Marshal(doc)
		if err != nil {
			return raw, nil
		}
		if out.Len() > 0 {
			out.WriteString("\n---\n")
		}
		out.Write(b)
	}
	if !changed {
		return raw, nil
	}
	return out.Bytes(), nil
}

func orData(kind string) string {
	if kind == "" {
		return KindData
	}
	return kind
}

// readArchiveEntry reads one file from an archive without unpacking it.
func readArchiveEntry(file, want string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s is missing from the archive", want)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != want {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, 64<<20))
	}
}
