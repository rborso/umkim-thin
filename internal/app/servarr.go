package app

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// The media stack is the only app with a post-install configuration Job: it
// wires Prowlarr/Sonarr/Radarr/qBittorrent/Jellyfin through their APIs, which
// needs credentials the chart cannot invent. The console owns the Secret and
// can (re)build and run the Job on demand.
const (
	ServarrApp       = "servarr"
	ServarrNamespace = "servarr"

	servarrBootstrapSecret = "servarr-bootstrap"
	servarrAPIKeysSecret   = "servarr-api-keys"

	bootstrapProbeName = "servarr-bootstrap-probe"
	bootstrapBuildName = "servarr-bootstrap-build"
)

// ServarrSettings are the operator-selectable defaults for the media stack.
// They become chart values (`bootstrap.mediaLanguage`/`bootstrap.qualityCutoff`)
// and are reconciled by the bootstrap into Sonarr, Radarr, Seerr and Jellyfin.
type ServarrSettings struct {
	MediaLanguage string `json:"mediaLanguage"`
	QualityCutoff string `json:"qualityCutoff"`
}

// SettingOption is one value/label pair for the console's selectors.
type SettingOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

var servarrLanguageOptions = []SettingOption{
	{Value: "en", Label: "English"},
	{Value: "hu", Label: "Hungarian"},
	{Value: "de", Label: "German"},
	{Value: "fr", Label: "French"},
	{Value: "es", Label: "Spanish"},
	{Value: "it", Label: "Italian"},
	{Value: "pt", Label: "Portuguese"},
	{Value: "pl", Label: "Polish"},
	{Value: "cs", Label: "Czech"},
	{Value: "sk", Label: "Slovak"},
	{Value: "ro", Label: "Romanian"},
	{Value: "ru", Label: "Russian"},
	{Value: "uk", Label: "Ukrainian"},
	{Value: "nl", Label: "Dutch"},
	{Value: "sv", Label: "Swedish"},
	{Value: "da", Label: "Danish"},
	{Value: "fi", Label: "Finnish"},
	{Value: "no", Label: "Norwegian"},
	{Value: "tr", Label: "Turkish"},
	{Value: "el", Label: "Greek"},
	{Value: "ja", Label: "Japanese"},
	{Value: "ko", Label: "Korean"},
	{Value: "zh", Label: "Chinese"},
	{Value: "ar", Label: "Arabic"},
	{Value: "he", Label: "Hebrew"},
}

var servarrQualityOptions = []SettingOption{
	{Value: "480p", Label: "480p (SD)"},
	{Value: "720p", Label: "720p (HD)"},
	{Value: "1080p", Label: "1080p (Full HD)"},
	{Value: "2160p", Label: "2160p (4K)"},
}

func ServarrLanguageOptions() []SettingOption {
	return append([]SettingOption(nil), servarrLanguageOptions...)
}

func ServarrQualityOptions() []SettingOption {
	return append([]SettingOption(nil), servarrQualityOptions...)
}

func DefaultServarrSettings() ServarrSettings {
	return ServarrSettings{MediaLanguage: "en", QualityCutoff: "720p"}
}

// NormalizeServarrSettings accepts the console's short language code and target
// resolution. It is deliberately strict: a typo must not silently turn into an
// English/720p install.
func NormalizeServarrSettings(s ServarrSettings) (ServarrSettings, error) {
	s.MediaLanguage = strings.ToLower(strings.TrimSpace(s.MediaLanguage))
	if i := strings.IndexAny(s.MediaLanguage, "-_"); i > 0 {
		s.MediaLanguage = s.MediaLanguage[:i]
	}
	if s.MediaLanguage == "" {
		s.MediaLanguage = "en"
	}
	knownLanguage := false
	for _, option := range servarrLanguageOptions {
		if option.Value == s.MediaLanguage {
			knownLanguage = true
			break
		}
	}
	if !knownLanguage {
		return ServarrSettings{}, fmt.Errorf("unsupported media language %q", s.MediaLanguage)
	}

	s.QualityCutoff = strings.ToLower(strings.TrimSpace(s.QualityCutoff))
	if s.QualityCutoff == "" {
		s.QualityCutoff = "720p"
	}
	if !strings.HasSuffix(s.QualityCutoff, "p") {
		s.QualityCutoff += "p"
	}
	knownQuality := false
	for _, option := range servarrQualityOptions {
		if option.Value == s.QualityCutoff {
			knownQuality = true
			break
		}
	}
	if !knownQuality {
		return ServarrSettings{}, fmt.Errorf("unsupported target quality %q", s.QualityCutoff)
	}
	return s, nil
}

// ServarrSettings reads the current defaults from the Helm release. It returns
// the platform defaults when the release does not exist yet, which lets the
// install flow show a sensible choice before the first install.
func (in *Installer) ServarrSettings() (ServarrSettings, error) {
	return in.ServarrSettingsFor(ServarrApp, ServarrNamespace)
}

// ServarrSettingsFor reads the defaults for one installed Servarr release. The
// app-scoped profile manager uses this so two namespaces do not share a default.
func (in *Installer) ServarrSettingsFor(release, namespace string) (ServarrSettings, error) {
	s := DefaultServarrSettings()
	out, err := in.run(in.cfg.Helm, "get", "values", release,
		"-n", namespace, "--kubeconfig", in.cfg.Kubeconfig, "-o", "json")
	if err != nil || strings.TrimSpace(out) == "" {
		return s, nil
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(out), &values); err != nil {
		return s, fmt.Errorf("reading servarr values: %w", err)
	}
	if bootstrap, ok := values["bootstrap"].(map[string]any); ok {
		if v, ok := bootstrap["mediaLanguage"].(string); ok && v != "" {
			s.MediaLanguage = v
		}
		if v, ok := bootstrap["qualityCutoff"].(string); ok && v != "" {
			s.QualityCutoff = v
		}
	}
	return NormalizeServarrSettings(s)
}

// EnsureServarrSecret creates/updates the Secret the chart's initContainers and
// the bootstrap Job read. Passwords are generated once and then preserved, so a
// reinstall does not reset qBittorrent/Jellyfin behind the operator's back; the
// chart-owned API keys are copied in whenever they are available.
func (in *Installer) EnsureServarrSecret() error {
	data, _ := in.getSecret(ServarrNamespace, servarrBootstrapSecret)
	if data == nil {
		data = map[string]string{}
	}
	changed := false
	set := func(key, value string) {
		if data[key] == "" {
			data[key] = value
			changed = true
		}
	}
	set("QBITTORRENT_USERNAME", "admin")
	set("SERVARR_USERNAME", "admin")
	set("JELLYFIN_ADMIN_USER", "jellyfin-admin")
	set("QBITTORRENT_PASSWORD", randomPassword())
	set("SERVARR_PASSWORD", randomPassword())
	set("JELLYFIN_ADMIN_PASSWORD", randomPassword())

	if keys, ok := in.getSecret(ServarrNamespace, servarrAPIKeysSecret); ok {
		for secretKey, envKey := range map[string]string{
			"sonarr-api-key":   "SONARR_API_KEY",
			"radarr-api-key":   "RADARR_API_KEY",
			"prowlarr-api-key": "PROWLARR_API_KEY",
			"lidarr-api-key":   "LIDARR_API_KEY",
		} {
			if v := keys[secretKey]; v != "" && data[envKey] != v {
				data[envKey] = v
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return in.applySecret(ServarrNamespace, servarrBootstrapSecret, data)
}

// ServarrBootstrapState turns the Job and the bootstrap Secret into the state
// the console shows. "not-configured" means the console has not created the
// Secret yet; otherwise it reports the Job's state.
func (in *Installer) ServarrBootstrapState() string {
	if _, ok := in.getSecret(ServarrNamespace, servarrBootstrapSecret); !ok {
		return "not-configured"
	}
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", ServarrNamespace, "get", "job", "servarr-bootstrap", "--ignore-not-found", "-o", "json")
	if err != nil || strings.TrimSpace(out) == "" {
		return "pending"
	}
	var job struct {
		Status struct {
			Active     int `json:"active"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal([]byte(out), &job) != nil {
		return "pending"
	}
	for _, c := range job.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			return "succeeded"
		case "Failed":
			return "failed"
		}
	}
	if job.Status.Active > 0 {
		return "running"
	}
	return "pending"
}

// SetupServarr is the on-demand path: probe the pinned node for the image,
// build it there when it is missing, then enable and run the chart's bootstrap
// Job. It is idempotent: a present image and a completed Job are no-ops.
func (in *Installer) SetupServarr(e catalog.Entry, settings ServarrSettings, progress func(string)) (map[string]any, error) {
	step := func(format string, a ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, a...))
		}
	}
	if e.Name != ServarrApp {
		return nil, fmt.Errorf("setup is only implemented for %s", ServarrApp)
	}
	settings, err := NormalizeServarrSettings(settings)
	if err != nil {
		return nil, err
	}
	step("preparing the media stack secret")
	if err := in.EnsureServarrSecret(); err != nil {
		return nil, err
	}

	node, err := in.bootstrapNode()
	if err != nil {
		return nil, err
	}
	tag, err := in.BootstrapImageTag()
	if err != nil {
		return nil, err
	}
	// The chart's bootstrap is mostly idempotent but not entirely (Jellyfin's
	// wizard cannot be re-run). If this exact image already completed with the
	// same defaults, do not run it again; a changed source, image or setting
	// triggers a fresh run.
	current, _ := in.ServarrSettings()
	if state := in.ServarrBootstrapState(); state == "succeeded" &&
		in.bootstrapJobImage() == tag &&
		current == settings {
		step("the bootstrap already succeeded with this image and defaults")
		return map[string]any{"image": tag, "node": node, "bootstrap": "succeeded", "skipped": true, "settings": settings}, nil
	}
	// The qBittorrent password lives in its config, written by an initContainer
	// at pod start. A freshly created or changed Secret only takes effect after
	// a restart; without this the bootstrap cannot log in to qBittorrent.
	step("restarting qBittorrent so the WebUI password matches the Secret")
	if out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", ServarrNamespace, "rollout", "restart", "deploy/qbittorrent"); err != nil {
		return nil, fmt.Errorf("restarting qBittorrent: %w\n%s", err, out)
	}
	if out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", ServarrNamespace, "rollout", "status", "deploy/qbittorrent", "--timeout=300s"); err != nil {
		return nil, fmt.Errorf("waiting for qBittorrent: %w\n%s", err, out)
	}
	step("checking %s on %s", tag, node)
	present, err := in.probeBootstrapImage(node, tag)
	if err != nil {
		return nil, err
	}
	if !present {
		step("image missing; building it on %s", node)
		if err := in.buildBootstrapImage(node, tag, step); err != nil {
			return nil, err
		}
	}
	step("enabling the bootstrap Job")
	if err := in.enableBootstrap(e, node, tag, settings, step); err != nil {
		return nil, err
	}
	state := in.ServarrBootstrapState()
	if state == "failed" {
		return nil, fmt.Errorf("the bootstrap Job failed:\n%s", in.jobLogs(ServarrNamespace, "servarr-bootstrap"))
	}
	if state == "succeeded" {
		step("the media stack is configured")
	}
	return map[string]any{"image": tag, "node": node, "bootstrap": state, "settings": settings}, nil
}

// bootstrapJobImage reports the image of the (successful) bootstrap Job, if it
// still exists. The hook's delete policy keeps it until the next upgrade.
func (in *Installer) bootstrapJobImage() string {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", ServarrNamespace, "get", "job", "servarr-bootstrap", "--ignore-not-found",
		"-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// BootstrapImageTag derives the tag from the vendored source: the version file
// plus the platform commit. The same inputs always get the same tag, so a
// rebuild is skipped and a stale build is never reused. Without a baked commit
// (a local build), a content hash keeps the tag honest.
func (in *Installer) BootstrapImageTag() (string, error) {
	root := in.cfg.BootstrapContext
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", fmt.Errorf("bootstrap build context %s is not available", root)
	}
	ver := readFileTrim(filepath.Join(root, "VERSION"))
	if ver == "" {
		ver = "dev"
	}
	rev := in.cfg.GitSHA
	if rev == "" {
		sum, err := hashDir(root)
		if err != nil {
			return "", err
		}
		rev = sum
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return fmt.Sprintf("%s:%s-%s", strings.TrimSuffix(in.cfg.BootstrapImage, ":"), ver, rev), nil
}

func (in *Installer) bootstrapNode() (string, error) {
	// The configured value may be an inventory address (the site files name
	// hosts by IP), while the nodeSelector needs the Kubernetes node name.
	// Accept it only when it actually is a node; otherwise fall through.
	if in.cfg.BootstrapNode != "" {
		out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
			"get", "node", in.cfg.BootstrapNode, "--ignore-not-found", "-o", "name")
		if err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "node/")), nil
		}
	}
	out, _ := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
	names := strings.Fields(out)
	if len(names) == 1 {
		return names[0], nil
	}
	if out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", "umkim-thin", "get", "pod", "-l", "app=umkim-thin-console",
		"-o", "jsonpath={.items[0].spec.nodeName}"); err == nil {
		if node := strings.TrimSpace(out); node != "" {
			return node, nil
		}
	}
	if len(names) > 0 {
		return names[0], nil
	}
	return "", fmt.Errorf("no node to run the bootstrap on")
}

// probeBootstrapImage schedules a throwaway Pod with the image on the pinned
// node and imagePullPolicy Never. ErrImageNeverPull means the kubelet has no
// local copy; Succeeded means it does. The image is never pulled from a
// registry.
func (in *Installer) probeBootstrapImage(node, tag string) (bool, error) {
	_ = in.deletePod(ServarrNamespace, bootstrapProbeName)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/component: bootstrap-probe
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  nodeSelector:
    kubernetes.io/hostname: %s
  containers:
    - name: probe
      image: %q
      imagePullPolicy: Never
      command: ["/bin/true"]
`, bootstrapProbeName, ServarrNamespace, yamlString(node), tag)
	if err := in.apply(manifest); err != nil {
		return false, err
	}
	defer func() { _ = in.deletePod(ServarrNamespace, bootstrapProbeName) }()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
			"-n", ServarrNamespace, "get", "pod", bootstrapProbeName, "--ignore-not-found", "-o", "json")
		if err == nil && strings.TrimSpace(out) != "" {
			var pod struct {
				Status struct {
					Phase             string `json:"phase"`
					ContainerStatuses []struct {
						State struct {
							Waiting struct {
								Reason string `json:"reason"`
							} `json:"waiting"`
							Terminated struct {
								Reason string `json:"reason"`
							} `json:"terminated"`
						} `json:"state"`
					} `json:"containerStatuses"`
				} `json:"status"`
			}
			if json.Unmarshal([]byte(out), &pod) == nil {
				if pod.Status.Phase == "Succeeded" {
					return true, nil
				}
				if pod.Status.Phase == "Failed" {
					reason := ""
					if len(pod.Status.ContainerStatuses) > 0 {
						reason = pod.Status.ContainerStatuses[0].State.Terminated.Reason
					}
					if reason == "ErrImageNeverPull" || reason == "ImagePullBackOff" {
						return false, nil
					}
					return false, fmt.Errorf("image probe failed on %s: %s", node, reason)
				}
				for _, cs := range pod.Status.ContainerStatuses {
					switch cs.State.Waiting.Reason {
					case "ErrImageNeverPull", "ImagePullBackOff", "ErrImagePull":
						return false, nil
					}
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return false, fmt.Errorf("timed out probing %s on %s", tag, node)
}

func (in *Installer) buildBootstrapImage(node, tag string, step func(string, ...any)) error {
	ns := in.cfg.BuildNamespace
	if err := in.apply(fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
`, ns)); err != nil {
		return err
	}
	// The builder pulls only the pinned upstream base image and npm packages:
	// DNS plus 80/443. Nothing in the cluster is reachable on those ports from
	// this namespace except through the default deny, which we keep.
	if err := in.apply(fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: builder-egress
  namespace: %s
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
      ports:
        - {protocol: UDP, port: 53}
        - {protocol: TCP, port: 53}
    - ports:
        - {protocol: TCP, port: 80}
        - {protocol: TCP, port: 443}
`, ns)); err != nil {
		return err
	}
	_, _ = in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
		"delete", "job", bootstrapBuildName, "--ignore-not-found")

	job := fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/component: bootstrap-builder
  annotations:
    umkim-thin/image: %q
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 1800
  ttlSecondsAfterFinished: 900
  template:
    metadata:
      labels:
        app.kubernetes.io/component: bootstrap-builder
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      nodeSelector:
        kubernetes.io/hostname: %s
      initContainers:
        - name: context
          image: %q
          imagePullPolicy: Never
          command: ["/bin/sh", "-c", "mkdir -p /ctx && cp -a /opt/umkim-thin/bootstrap/. /ctx/"]
          volumeMounts:
            - {name: context, mountPath: /ctx}
      containers:
        - name: build
          image: %q
          imagePullPolicy: IfNotPresent
          env:
            - {name: TAG, value: %q}
          securityContext:
            runAsUser: 0
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
              add: [CHOWN, DAC_OVERRIDE, FOWNER, MKNOD, SETFCAP, SETGID, SETUID, SYS_CHROOT]
            seccompProfile:
              type: RuntimeDefault
          command: ["/busybox/sh", "-ec"]
          args:
            - |
              set -eu
              echo ">> building $TAG"
              /kaniko/executor --context=dir:///ctx --dockerfile=/ctx/Dockerfile \
                --destination="$TAG" --no-push --tar-path=/out/image.tar --verbosity=info
              echo ">> importing into the node's containerd"
              /host/k3s ctr -n k8s.io images import /out/image.tar
              echo ">> done"
          volumeMounts:
            - {name: context, mountPath: /ctx, readOnly: true}
            - {name: out, mountPath: /out}
            - {name: k3s, mountPath: /host/k3s, readOnly: true}
            - {name: k3s-data, mountPath: /var/lib/rancher/k3s, readOnly: true}
            - {name: containerd, mountPath: /run/k3s/containerd/containerd.sock}
      volumes:
        - {name: context, emptyDir: {}}
        - {name: out, emptyDir: {}}
        - {name: k3s, hostPath: {path: /usr/local/bin/k3s, type: File}}
        - {name: k3s-data, hostPath: {path: /var/lib/rancher/k3s, type: Directory}}
        - {name: containerd, hostPath: {path: /run/k3s/containerd/containerd.sock, type: Socket}}
`, bootstrapBuildName, ns, tag, yamlString(node), in.cfg.ConsoleImage, in.cfg.BuilderImage, tag)
	if err := in.apply(job); err != nil {
		return err
	}

	step("build Job started in %s on %s", ns, node)
	deadline := time.Now().Add(35 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig, "-n", ns,
			"get", "job", bootstrapBuildName, "--ignore-not-found",
			"-o", "jsonpath={range .status.conditions[*]}{.type}={.status},{end}")
		if err == nil {
			switch {
			case strings.Contains(out, "Complete=True"):
				step("image %s imported on %s", tag, node)
				return nil
			case strings.Contains(out, "Failed=True"):
				return fmt.Errorf("the bootstrap build failed:\n%s", in.jobLogs(ns, bootstrapBuildName))
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("the bootstrap build timed out; see %s/%s", ns, bootstrapBuildName)
}

// enableBootstrap upgrades the release with the chart's post-install Job
// enabled. Helm waits for the hook, so the console's job log follows it.
func (in *Installer) enableBootstrap(e catalog.Entry, node, tag string, settings ServarrSettings, step func(string, ...any)) error {
	if out, err := in.run(in.cfg.Helm, "status", e.Name, "-n", e.Namespace, "--kubeconfig", in.cfg.Kubeconfig, "-o", "json"); err == nil {
		var st struct {
			Info struct {
				Status string `json:"status"`
			} `json:"info"`
		}
		if json.Unmarshal([]byte(out), &st) == nil && strings.HasPrefix(st.Info.Status, "pending-") {
			step("clearing a %s release", st.Info.Status)
			_, _ = in.run(in.cfg.Helm, "rollback", e.Name, "-n", e.Namespace, "--kubeconfig", in.cfg.Kubeconfig)
		}
	}
	args := []string{"upgrade", e.Name, in.chartRef(e),
		"--namespace", e.Namespace,
		"--kubeconfig", in.cfg.Kubeconfig,
		"--reuse-values",
		"--set", "bootstrap.enabled=true",
		"--set", "bootstrap.image=" + tag,
		"--set", "bootstrap.imagePullPolicy=Never",
		"--set", "bootstrap.secretName=" + servarrBootstrapSecret,
		"--set", "bootstrap.mediaLanguage=" + settings.MediaLanguage,
		"--set", "bootstrap.qualityCutoff=" + settings.QualityCutoff,
		"--set", `bootstrap.nodeSelector.kubernetes\.io/hostname=` + node,
		"--wait", "--timeout", "30m",
	}
	out, err := in.runStream(in.cfg.Helm, args, func(line string) { step("%s", line) })
	if err != nil {
		return fmt.Errorf("helm upgrade: %w\n%s", err, out)
	}
	return nil
}

func (in *Installer) jobLogs(ns, name string) string {
	out, _ := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", ns, "logs", "job/"+name, "--tail=40")
	return strings.TrimSpace(out)
}

func (in *Installer) deletePod(ns, name string) error {
	_, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", ns, "delete", "pod", name, "--ignore-not-found", "--grace-period=0", "--force", "--wait=false")
	return err
}

// getSecret reads a Secret's data as plain strings; absent is not an error.
func (in *Installer) getSecret(ns, name string) (map[string]string, bool) {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"-n", ns, "get", "secret", name, "--ignore-not-found", "-o", "json")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, false
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal([]byte(out), &s) != nil {
		return nil, false
	}
	data := map[string]string{}
	for k, v := range s.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err == nil {
			data[k] = string(b)
		}
	}
	return data, true
}

// applySecret writes the Secret through stdin. Values never appear on a
// command line and never enter the console's log.
func (in *Installer) applySecret(ns, name string, data map[string]string) error {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\nstringData:\n", name, ns)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %q\n", k, data[k])
	}
	return in.apply(b.String())
}

func randomPassword() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashDir(root string) (string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(body))
		_, _ = h.Write(body)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func yamlString(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}
