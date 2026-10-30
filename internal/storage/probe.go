package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The plan can only check TCP/2049 and what showmount advertises; neither
// proves the export is mountable (a stale export entry, a subdirectory of a
// mergerfs pool, or an export whose client list excludes the node all pass).
// Probing mounts it in a throwaway pod, which is the same path the provisioner
// uses, so a broken export fails in seconds instead of after Helm's 5-minute
// wait.

// ProbeMount mounts the export in a short-lived pod and reads it. A definitive
// mount failure is an error; a probe that could not run (image pull, timeout)
// is a warning, so it never blocks an install that would otherwise work.
func (c *Config) ProbeMount(server, export, mountOptions string) ([]string, error) {
	if !validNFSServer(server) {
		return nil, fmt.Errorf("server must be an NFS hostname or IP address")
	}
	if strings.TrimSpace(export) == "" {
		return nil, fmt.Errorf("export must be an absolute path")
	}
	ns := c.ProbeNamespace
	if ns == "" {
		ns = "nfs-provisioner"
	}
	name := probeName(server, export)
	image := c.ProbeImage
	if image == "" {
		image = defaultProbeImage
	}

	// The namespace only exists after the first install; create it so the probe
	// can run before Helm does (idempotent).
	if out, err := c.commandInput(namespaceDoc(ns), c.Kubectl, "--kubeconfig", c.Kubeconfig,
		"apply", "-f", "-"); err != nil {
		return []string{"could not prepare the probe namespace: " + strings.TrimSpace(out)}, nil
	}
	// A probe from an earlier attempt (or a crash) must not block the name.
	_, _ = c.command(c.Kubectl, "--kubeconfig", c.Kubeconfig,
		"-n", ns, "delete", "pod", name, "--ignore-not-found", "--wait=false")
	doc := probePod(name, ns, server, export, image)
	if out, err := c.commandInput(doc, c.Kubectl, "--kubeconfig", c.Kubeconfig,
		"-n", ns, "create", "-f", "-"); err != nil {
		return []string{"could not start the mount probe: " + strings.TrimSpace(out)}, nil
	}
	defer func() {
		_, _ = c.command(c.Kubectl, "--kubeconfig", c.Kubeconfig,
			"-n", ns, "delete", "pod", name, "--ignore-not-found", "--wait=false")
	}()

	timeout := c.ProbeTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		phase, message, getErr := c.probePhase(ns, name)
		if getErr == nil {
			switch phase {
			case "Succeeded":
				return nil, nil
			case "Failed":
				if strings.TrimSpace(message) == "" {
					message = "the probe container failed"
				}
				return nil, fmt.Errorf("the export %s:%s could not be read: %s", server, export, message)
			}
		}
		if mountErr, pull := c.probeEvents(ns, name); mountErr != "" {
			return nil, fmt.Errorf("the export %s:%s could not be mounted: %s", server, export, mountErr)
		} else if pull != "" {
			return []string{"could not verify the export (the probe image did not start): " + pull}, nil
		}
		if time.Now().After(deadline) {
			return []string{fmt.Sprintf("could not verify the export within %s (the probe pod did not finish)", timeout)}, nil
		}
		time.Sleep(2 * time.Second)
	}
}

// probePhase reads the pod's phase and, for a failed container, its message.
func (c *Config) probePhase(ns, name string) (phase, message string, err error) {
	out, err := c.command(c.Kubectl, "--kubeconfig", c.Kubeconfig,
		"-n", ns, "get", "pod", name, "-o", "json")
	if err != nil {
		return "", "", err
	}
	var pod struct {
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				State struct {
					Terminated *struct {
						Message string `json:"message"`
						Reason  string `json:"reason"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &pod); err != nil {
		return "", "", err
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			message = strings.TrimSpace(t.Message)
			if message == "" {
				message = t.Reason
			}
		}
	}
	return pod.Status.Phase, message, nil
}

// probeEvents returns the kubelet's mount error, or an image-pull problem.
func (c *Config) probeEvents(ns, name string) (mountErr, pull string) {
	out, err := c.command(c.Kubectl, "--kubeconfig", c.Kubeconfig,
		"-n", ns, "get", "events", "--field-selector", "involvedObject.name="+name, "-o", "json")
	if err != nil {
		return "", ""
	}
	var events struct {
		Items []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &events); err != nil {
		return "", ""
	}
	for _, e := range events.Items {
		switch e.Reason {
		case "FailedMount":
			return trimMountMessage(e.Message), ""
		case "Failed", "ErrImagePull", "ImagePullBackOff", "FailedToRetrieveImagePullSecret":
			if strings.Contains(e.Message, "pull") || e.Reason != "Failed" {
				return "", shortText(e.Message, 200)
			}
		}
	}
	return "", ""
}

// trimMountMessage drops the "MountVolume.SetUp failed for volume ..." preamble
// and keeps the server's reason.
func trimMountMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.Index(msg, " : "); i > 0 {
		msg = strings.TrimSpace(msg[i+3:])
	}
	return shortText(msg, 300)
}

func shortText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func probeName(server, export string) string {
	sum := sha256.Sum256([]byte(server + ":" + export))
	return "umkim-nfs-probe-" + hex.EncodeToString(sum[:4])
}

func namespaceDoc(ns string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n  labels:\n    app.kubernetes.io/managed-by: umkim-thin\n", ns)
}

func probePod(name, ns, server, export, image string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: umkim-thin
    app.kubernetes.io/component: nfs-probe
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: probe
      image: %q
      imagePullPolicy: IfNotPresent
      command: ["/bin/sh", "-c", "test -d /mnt/probe && ls -A /mnt/probe >/dev/null && echo ok"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: export
          mountPath: /mnt/probe
          readOnly: true
  volumes:
    - name: export
      nfs:
        server: %s
        path: %s
        readOnly: true
`, name, ns, image, server, export)
}

// defaultProbeImage is the pinned Alpine the WireGuard chart already uses; it
// only needs /bin/sh and ls.
const defaultProbeImage = "docker.io/library/alpine:3.20.10@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// commandInput runs a command with stdin, for `kubectl create -f -`.
func (c *Config) commandInput(stdin, name string, args ...string) (string, error) {
	if c.runInput != nil {
		return c.runInput(stdin, name, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}
