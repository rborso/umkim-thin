package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// dataMargin is the minimum free space a file copy must leave on the backup
// filesystem; 10% of the filesystem wins when it is larger. A full copy is a
// last resort, not a reason to fill the disk.
const dataMargin = 2 << 30 // 2 GiB

// helperPodName is the short-lived pod that mounts one PVC read-only and runs
// tar. The name is deterministic per volume so a crashed run is cleaned up.
func helperPodName(namespace, pvc string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + pvc))
	return "ut-backup-" + hex.EncodeToString(sum[:6])
}

// helperPodYAML mounts the claim read-only and waits for the exec. Root is
// needed to read files owned by the application's UID (Postgres data is 0700);
// the volume is read-only, the token is not mounted and the root filesystem is
// read-only, so the pod can read and nothing else.
func helperPodYAML(name, namespace, pvc, image string, readOnly bool) string {
	readOnlyAttr := "false"
	if readOnly {
		readOnlyAttr = "true"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: umkim-thin
    umkim-thin/backup: "true"
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 0
  securityContext:
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: copy
      image: %q
      imagePullPolicy: IfNotPresent
      command: ["/bin/sh", "-c", "sleep 86400"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        # The console image runs as 65532; app data is often owned by another
        # UID (or root), so the copy helper must be root to read it. The volume
        # is mounted read-only and nothing else is mounted.
        runAsUser: 0
        runAsGroup: 0
      volumeMounts:
        - name: data
          mountPath: /data
          readOnly: %s
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %s
        readOnly: %s
`, name, namespace, image, readOnlyAttr, pvc, readOnlyAttr)
}

// copyVolume streams one PVC into <archiveBase>-data-<ns>-<pvc>.tar.gz. The
// copy is crash-consistent: a running application may write during it, so a
// consistent copy needs the app stopped or a storage snapshot.
func (m *Manager) copyVolume(ctx context.Context, ns, pvc, archiveBase string) DataRecord {
	rec := DataRecord{Namespace: ns, PVC: pvc, Method: "copy"}
	if m.cfg.HelperImage == "" {
		rec.Error = "no helper image configured (set UMKIM_THIN_CONSOLE_IMAGE)"
		return rec
	}
	name := helperPodName(ns, pvc)
	m.deleteHelperPod(ns, name)
	doc := helperPodYAML(name, ns, pvc, m.cfg.HelperImage, true)
	if _, err := m.cfg.Run(m.cfg.Kubectl, doc, "--kubeconfig", m.cfg.Kubeconfig, "apply", "-f", "-"); err != nil {
		rec.Error = "creating the copy helper: " + shortErr(err)
		return rec
	}
	defer m.deleteHelperPod(ns, name)
	if _, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"-n", ns, "wait", "--for=condition=Ready", "--timeout=180s", "pod/"+name); err != nil {
		rec.Error = "the copy helper did not start: " + shortErr(err)
		return rec
	}
	// Size the volume first: a copy that cannot fit must fail before it fills
	// the disk, not halfway through.
	out, err := m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"-n", ns, "exec", name, "-c", "copy", "--", "du", "-sk", "/data")
	if err != nil {
		rec.Error = "measuring the volume: " + shortErr(err)
		return rec
	}
	need := duKB(out) * 1024
	dir, err := m.targetDir()
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	avail, total, err := statfs(dir)
	if err != nil {
		rec.Error = "checking free space: " + shortErr(err)
		return rec
	}
	margin := int64(dataMargin)
	if tenth := total / 10; tenth > margin {
		margin = tenth
	}
	if need > 0 && need+margin > avail {
		rec.Error = fmt.Sprintf("not enough space: the volume needs %s, %s free (keeping %s)",
			humanBytes(need), humanBytes(avail), humanBytes(margin))
		return rec
	}
	file := archiveBase + "-data-" + ns + "-" + pvc + ".tar.gz"
	dest := filepath.Join(dir, file)
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		rec.Error = shortErr(err)
		return rec
	}
	h := sha256.New()
	err = m.cfg.RunStream(ctx, m.cfg.Kubectl, io.MultiWriter(f, h),
		"--kubeconfig", m.cfg.Kubeconfig, "-n", ns, "exec", name, "-c", "copy",
		"--", "tar", "czf", "-", "-C", "/data", ".")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		rec.Error = "copying the volume: " + shortErr(err)
		return rec
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		rec.Error = shortErr(err)
		return rec
	}
	info, err := os.Stat(dest)
	if err != nil {
		rec.Error = shortErr(err)
		return rec
	}
	rec.File = file
	rec.Size = info.Size()
	rec.SHA256 = hex.EncodeToString(h.Sum(nil))
	return rec
}

// deleteHelperPod removes the copy pod (best effort). A force delete keeps a
// failed mount from holding the volume open for a whole grace period.
func (m *Manager) deleteHelperPod(ns, name string) {
	_, _ = m.cfg.Run(m.cfg.Kubectl, "", "--kubeconfig", m.cfg.Kubeconfig,
		"-n", ns, "delete", "pod", name, "--ignore-not-found", "--grace-period=0", "--force")
}

// duKB parses busybox `du -sk` output ("12345\t/data").
func duKB(out string) int64 {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// statfs returns the free and total bytes of the filesystem holding path.
func statfs(path string) (avail, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := int64(st.Bsize)
	return int64(st.Bavail) * bsize, int64(st.Blocks) * bsize, nil
}

// humanBytes renders a size for warnings and the console.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// runToolStreamIn runs a command with r as stdin (the volume restore), keeping
// stderr for the error.
func runToolStreamIn(ctx context.Context, kubeconfig, name string, r io.Reader, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if kubeconfig != "" {
		cmd.Env = append(cmd.Environ(), "KUBECONFIG="+kubeconfig)
	}
	var errb bytes.Buffer
	cmd.Stdin = r
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// runToolStream runs a command and copies its stdout into w, keeping stderr for
// the error. The volume copy streams through here: buffering a multi-gigabyte
// tar in memory is not an option.
func runToolStream(ctx context.Context, kubeconfig, name string, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if kubeconfig != "" {
		cmd.Env = append(cmd.Environ(), "KUBECONFIG="+kubeconfig)
	}
	var errb bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// deleteData removes the sidecar copies an archive recorded.
func (m *Manager) deleteData(a Archive) {
	mf, err := readManifest(filepath.Join(m.displayDir(), a.Name))
	if err != nil {
		return
	}
	for _, d := range mf.Data {
		if d.File == "" {
			continue
		}
		_ = os.Remove(filepath.Join(m.displayDir(), filepath.Base(d.File)))
	}
}

// verifyData checks that every recorded copy exists and matches its checksum.
func (m *Manager) verifyData(mf Manifest) error {
	for _, d := range mf.Data {
		if d.File == "" || d.Error != "" {
			continue
		}
		p := filepath.Join(m.displayDir(), filepath.Base(d.File))
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("data file %s is missing", d.File)
		}
		if d.Size > 0 && info.Size() != d.Size {
			return fmt.Errorf("data file %s is %d bytes, the manifest says %d", d.File, info.Size(), d.Size)
		}
		if d.SHA256 != "" {
			sum, err := fileSHA256(p)
			if err != nil {
				return err
			}
			if sum != d.SHA256 {
				return fmt.Errorf("data file %s does not match its checksum", d.File)
			}
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HumanBytes renders a byte count for the console and the CLI.
func HumanBytes(n int64) string { return humanBytes(n) }
