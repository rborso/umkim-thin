package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// platformNamespaces are never snapshotted: their data is either the console's
// own archive (state) or recreated by the installer (identity, cluster addons).
var platformNamespaces = map[string]bool{
	"kube-system":      true,
	"kube-public":      true,
	"kube-node-lease":  true,
	"umkim-thin":       true,
	"umkim-thin-build": true,
	"identity":         true,
	"longhorn-system":  true,
	"nfs-provisioner":  true,
	"cert-manager":     true,
	"traefik":          true,
}

// snapshotPVCs takes one VolumeSnapshot per application PVC. The data half of
// a backup is the storage's job: mounting every volume here would fight the
// running workloads and their access modes. A cluster without the snapshot CRD
// or a class is not an error — the archive is still valid, with a warning.
func (m *Manager) snapshotPVCs(_ context.Context, ts time.Time) ([]SnapshotRecord, string) {
	class, err := m.snapshotClass()
	if err != nil {
		return nil, "application data was not snapshotted: " + err.Error()
	}
	out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"get", "pvc", "-A", "-o", "json")
	if err != nil {
		return nil, "application data was not snapshotted: " + shortErr(err)
	}
	var pvcs struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				VolumeName string `json:"volumeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pvcs); err != nil {
		return nil, "application data was not snapshotted: " + shortErr(err)
	}
	csi, err := m.csiVolumes()
	if err != nil {
		return nil, "application data was not snapshotted: " + shortErr(err)
	}
	stamp := ts.Format("20060102T150405Z")
	var recs []SnapshotRecord
	var skipped []string
	for _, p := range pvcs.Items {
		ns, name := p.Metadata.Namespace, p.Metadata.Name
		if ns == "" || name == "" || platformNamespaces[ns] {
			continue
		}
		// The snapshot API is a CSI feature: an NFS (or local-path) volume has
		// no CSI source and the controller refuses it. Back those up where the
		// data lives (the NFS server or a VM snapshot) and say so.
		if !csi[p.Spec.VolumeName] {
			skipped = append(skipped, ns+"/"+name)
			continue
		}
		snap := snapshotName(stamp, name)
		doc := snapshotYAML(snap, ns, name, class)
		if _, err := m.cfg.Run(m.cfg.Kubectl, doc, "--kubeconfig", m.cfg.Kubeconfig,
			"apply", "-f", "-"); err != nil {
			recs = append(recs, SnapshotRecord{Namespace: ns, PVC: name, Class: class, Error: shortErr(err)})
			continue
		}
		recs = append(recs, SnapshotRecord{Namespace: ns, PVC: name, Snapshot: snap, Class: class})
	}
	if len(recs) == 0 {
		if len(skipped) > 0 {
			return nil, "application data was not snapshotted: no CSI-backed PVC (skipped " + strings.Join(skipped, ", ") + ")"
		}
		return nil, "no application PVCs to snapshot"
	}
	warn := ""
	if len(skipped) > 0 {
		warn = "these PVCs are not CSI-backed and were not snapshotted (back them up at the storage): " + strings.Join(skipped, ", ")
	}
	m.waitReady(recs)
	return recs, warn
}

// csiVolumes returns the persistent volumes that are CSI-backed, keyed by PV
// name. Everything else (NFS, local-path) cannot be snapshotted through the
// VolumeSnapshot API.
func (m *Manager) csiVolumes() (map[string]bool, error) {
	out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"get", "pv", "-o", "json")
	if err != nil {
		return nil, err
	}
	var pvs struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				CSI *struct {
					Driver string `json:"driver"`
				} `json:"csi"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pvs); err != nil {
		return nil, err
	}
	csi := make(map[string]bool, len(pvs.Items))
	for _, pv := range pvs.Items {
		csi[pv.Metadata.Name] = pv.Spec.CSI != nil
	}
	return csi, nil
}

// waitReady polls the snapshots it just created so the manifest records what
// actually completed, not what was requested. A snapshot that is still not
// ready keeps its error and is not counted.
func (m *Manager) waitReady(recs []SnapshotRecord) {
	deadline := time.Now().Add(m.cfg.SnapshotWait)
	for {
		pending := 0
		for i := range recs {
			if recs[i].Snapshot == "" || recs[i].Error != "" || recs[i].Ready {
				continue
			}
			out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
				"get", "volumesnapshot", recs[i].Snapshot, "-n", recs[i].Namespace,
				"-o", "jsonpath={.status.readyToUse}")
			if err != nil {
				recs[i].Error = shortErr(err)
				continue
			}
			if strings.TrimSpace(out) == "true" {
				recs[i].Ready = true
				continue
			}
			pending++
		}
		if pending == 0 || time.Now().After(deadline) {
			for i := range recs {
				if recs[i].Snapshot != "" && recs[i].Error == "" && !recs[i].Ready {
					recs[i].Error = "not ready before the backup finished"
				}
			}
			return
		}
		time.Sleep(m.cfg.SnapshotPoll)
	}
}

// snapshotClass picks the first VolumeSnapshotClass. Multiple classes are an
// operator decision; the console does not guess between drivers.
func (m *Manager) snapshotClass() (string, error) {
	out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"get", "volumesnapshotclass", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("no VolumeSnapshotClass (the CSI driver has no snapshot support): %s", shortErr(err))
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return "", fmt.Errorf("reading VolumeSnapshotClass: %s", shortErr(err))
	}
	if len(list.Items) == 0 {
		return "", fmt.Errorf("no VolumeSnapshotClass is defined")
	}
	return list.Items[0].Metadata.Name, nil
}

// snapshotName is short enough for the 63-character limit even for a long PVC
// name, and stable for a given backup timestamp.
func snapshotName(stamp, pvc string) string {
	sum := sha256.Sum256([]byte(pvc))
	suffix := hex.EncodeToString(sum[:3])
	base := pvc
	if len(base) > 34 {
		base = base[:34]
	}
	base = strings.TrimRight(base, "-")
	// Kubernetes object names are lowercase RFC 1123; the archive stamp has an
	// uppercase T and Z.
	return "ut-" + strings.ToLower(stamp) + "-" + base + "-" + suffix
}

func snapshotYAML(name, namespace, pvc, class string) string {
	return fmt.Sprintf(`apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: umkim-thin
spec:
  volumeSnapshotClassName: %s
  source:
    persistentVolumeClaimName: %s
`, name, namespace, class, pvc)
}

// deleteSnapshots removes the snapshots an archive recorded. It is best effort:
// a pruned archive must not fail because a snapshot was already gone.
func (m *Manager) deleteSnapshots(a Archive) {
	mf, err := readManifest(filepath.Join(m.cfg.Dir, a.Name))
	if err != nil {
		return
	}
	for _, s := range mf.Snapshots {
		if s.Snapshot == "" || s.Namespace == "" {
			continue
		}
		_, _ = m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
			"delete", "volumesnapshot", s.Snapshot, "-n", s.Namespace, "--ignore-not-found")
	}
}

// shortErr keeps kubectl's multi-line stderr out of the manifest.
func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// runTool runs a command with the console's kubeconfig, feeding stdin when the
// caller passes a document (kubectl apply -f -).
func runTool(kubeconfig, name, stdin string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if kubeconfig != "" {
		cmd.Env = append(cmd.Environ(), "KUBECONFIG="+kubeconfig)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
