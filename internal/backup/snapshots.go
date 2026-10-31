package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
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

// pvcRecord is one PersistentVolumeClaim the backup can offer or snapshot.
type pvcRecord struct {
	Namespace string
	Name      string
	Volume    string // the bound PV, used to decide CSI support
	Size      string
	Class     string
}

// pvcs lists every PVC in the cluster.
func (m *Manager) pvcs() ([]pvcRecord, error) {
	out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"get", "pvc", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				VolumeName       string `json:"volumeName"`
				StorageClassName string `json:"storageClassName"`
				Resources        struct {
					Requests map[string]string `json:"requests"`
				} `json:"resources"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, err
	}
	recs := make([]pvcRecord, 0, len(doc.Items))
	for _, p := range doc.Items {
		recs = append(recs, pvcRecord{
			Namespace: p.Metadata.Namespace,
			Name:      p.Metadata.Name,
			Volume:    p.Spec.VolumeName,
			Size:      p.Spec.Resources.Requests["storage"],
			Class:     p.Spec.StorageClassName,
		})
	}
	return recs, nil
}

// Scope lists what a selective backup can include: application releases and
// application PVCs. Platform namespaces are excluded: their data is the
// console archive itself or is recreated by the installer.
func (m *Manager) Scope() (ScopeView, error) {
	view := ScopeView{Apps: []AppRecord{}, Volumes: []VolumeRecord{}}
	apps, err := m.apps()
	if err != nil {
		return view, fmt.Errorf("reading the app inventory: %w", err)
	}
	appByNamespace := map[string]string{}
	for _, a := range apps {
		if platformNamespaces[a.Namespace] {
			continue
		}
		view.Apps = append(view.Apps, a)
		if _, ok := appByNamespace[a.Namespace]; !ok {
			appByNamespace[a.Namespace] = a.Name
		}
	}
	pvcs, err := m.pvcs()
	if err != nil {
		return view, fmt.Errorf("listing volumes: %w", err)
	}
	csi, err := m.csiVolumes()
	if err != nil {
		return view, fmt.Errorf("listing persistent volumes: %w", err)
	}
	for _, p := range pvcs {
		if p.Namespace == "" || p.Name == "" || platformNamespaces[p.Namespace] {
			continue
		}
		view.Volumes = append(view.Volumes, VolumeRecord{
			Namespace: p.Namespace,
			PVC:       p.Name,
			App:       appByNamespace[p.Namespace],
			Size:      p.Size,
			Class:     p.Class,
			CSI:       csi[p.Volume],
		})
	}
	sort.Slice(view.Volumes, func(i, j int) bool {
		if view.Volumes[i].Namespace != view.Volumes[j].Namespace {
			return view.Volumes[i].Namespace < view.Volumes[j].Namespace
		}
		return view.Volumes[i].PVC < view.Volumes[j].PVC
	})
	return view, nil
}

// backupVolumes backs up volume data according to mode. A CSI snapshot is the
// fast, storage-level copy; when the storage cannot snapshot (NFS, local-path,
// no VolumeSnapshotClass) the fallback is a full file copy streamed out of the
// volume. The selection limits both. Unknown selected volumes are an error;
// everything else is a warning, so a metadata archive is still written.
func (m *Manager) backupVolumes(ctx context.Context, ts time.Time, sel *Selection, mode, archiveBase string) ([]SnapshotRecord, []DataRecord, []string, error) {
	if mode == DataOff || (sel != nil && len(sel.Volumes) == 0) {
		return nil, nil, nil, nil
	}
	pvcs, err := m.pvcs()
	if err != nil {
		return nil, nil, []string{"volume data was not backed up: " + shortErr(err)}, nil
	}
	csi, err := m.csiVolumes()
	if err != nil {
		return nil, nil, []string{"volume data was not backed up: " + shortErr(err)}, nil
	}
	selected := map[string]bool{}
	if sel != nil {
		for _, v := range sel.Volumes {
			selected[v] = true
		}
		known := map[string]bool{}
		for _, p := range pvcs {
			if p.Namespace == "" || p.Name == "" || platformNamespaces[p.Namespace] {
				continue
			}
			known[p.Namespace+"/"+p.Name] = true
		}
		var unknown []string
		for _, v := range sel.Volumes {
			if !known[v] {
				unknown = append(unknown, v)
			}
		}
		if len(unknown) > 0 {
			return nil, nil, nil, fmt.Errorf("%w: unknown volume(s): %s (application PVCs are namespace/name)", ErrSelection, strings.Join(unknown, ", "))
		}
	}
	var targets []pvcRecord
	for _, p := range pvcs {
		if p.Namespace == "" || p.Name == "" || platformNamespaces[p.Namespace] {
			continue
		}
		if sel != nil && !selected[p.Namespace+"/"+p.Name] {
			continue
		}
		targets = append(targets, p)
	}

	var warns []string
	var snaps []SnapshotRecord
	snapshotted := map[string]bool{}
	if mode == DataSnapshot || mode == DataAuto {
		class, classErr := m.snapshotClass()
		if classErr != nil {
			if mode == DataSnapshot {
				warns = append(warns, "application data was not snapshotted: "+classErr.Error())
			}
			// auto: every volume falls through to the file copy below.
		} else {
			stamp := ts.Format("20060102T150405Z")
			for _, p := range targets {
				if !csi[p.Volume] {
					continue
				}
				snap := snapshotName(stamp, p.Name)
				doc := snapshotYAML(snap, p.Namespace, p.Name, class)
				if _, err := m.cfg.Run(m.cfg.Kubectl, doc, "--kubeconfig", m.cfg.Kubeconfig,
					"apply", "-f", "-"); err != nil {
					snaps = append(snaps, SnapshotRecord{Namespace: p.Namespace, PVC: p.Name, Class: class, Error: shortErr(err)})
					continue
				}
				snaps = append(snaps, SnapshotRecord{Namespace: p.Namespace, PVC: p.Name, Snapshot: snap, Class: class})
				snapshotted[p.Namespace+"/"+p.Name] = true
			}
			if len(snaps) > 0 {
				m.waitReady(snaps)
				for i := range snaps {
					if snaps[i].Snapshot != "" && snaps[i].Error == "" && !snaps[i].Ready {
						snapshotted[snaps[i].Namespace+"/"+snaps[i].PVC] = false
					}
				}
			}
			if mode == DataSnapshot {
				var skipped []string
				for _, p := range targets {
					if !csi[p.Volume] {
						skipped = append(skipped, p.Namespace+"/"+p.Name)
					}
				}
				if len(skipped) > 0 {
					warns = append(warns, "these volumes are not CSI-backed and were not snapshotted (back them up at the storage, or choose the full-copy data mode): "+strings.Join(skipped, ", "))
				}
				if len(targets) == 0 {
					warns = append(warns, "no application PVCs to snapshot")
				}
			}
		}
	}

	var data []DataRecord
	if mode == DataCopy || mode == DataAuto {
		for _, p := range targets {
			key := p.Namespace + "/" + p.Name
			if snapshotted[key] {
				continue
			}
			rec := m.copyVolume(ctx, p.Namespace, p.Name, archiveBase)
			data = append(data, rec)
			if rec.Error != "" {
				warns = append(warns, "copying "+key+": "+rec.Error)
			}
		}
	}
	return snaps, data, warns, nil
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
	mf, err := readManifest(filepath.Join(m.displayDir(), a.Name))
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
