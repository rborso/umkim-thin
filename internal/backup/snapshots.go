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
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pvcs); err != nil {
		return nil, "application data was not snapshotted: " + shortErr(err)
	}
	stamp := ts.Format("20060102T150405Z")
	var recs []SnapshotRecord
	for _, p := range pvcs.Items {
		ns, name := p.Metadata.Namespace, p.Metadata.Name
		if ns == "" || name == "" || platformNamespaces[ns] {
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
		return nil, "no application PVCs to snapshot"
	}
	return recs, ""
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
