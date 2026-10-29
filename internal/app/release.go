package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// ReleaseInfo is the read-only view of an installed release: Helm's metadata
// plus the user-supplied values it was installed with. Secrets are redacted.
type ReleaseInfo struct {
	Name         string         `json:"name"`
	Namespace    string         `json:"namespace"`
	Chart        string         `json:"chart"`
	ChartVersion string         `json:"chartVersion"`
	AppVersion   string         `json:"appVersion"`
	Revision     int            `json:"revision"`
	Status       string         `json:"status"`
	Updated      string         `json:"updated"`
	Values       map[string]any `json:"values"`
	Redacted     []string       `json:"redacted,omitempty"`
}

var secretValueKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|credential|api[_-]?key|hmac|private[_-]?key)`)

// Release returns how an app was installed. It reads Helm's release status and
// the user-supplied values (not the chart defaults) and redacts secret-looking
// keys before the values leave the process.
func (in *Installer) Release(e catalog.Entry, namespace string) (ReleaseInfo, error) {
	ns := firstNonEmpty(namespace, in.installedNamespace(e.Name, e.Namespace))

	statusOut, err := in.run(in.cfg.Helm, "status", e.Name, "-n", ns,
		"--kubeconfig", in.cfg.Kubeconfig, "-o", "json")
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("reading release %s/%s: %w", ns, e.Name, err)
	}
	var st struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status       string `json:"status"`
			LastDeployed string `json:"last_deployed"`
		} `json:"info"`
		Chart struct {
			Metadata struct {
				Name       string `json:"name"`
				Version    string `json:"version"`
				AppVersion string `json:"appVersion"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if err := json.Unmarshal([]byte(statusOut), &st); err != nil {
		return ReleaseInfo{}, fmt.Errorf("parsing Helm status for %s/%s: %w", ns, e.Name, err)
	}

	valuesOut, err := in.run(in.cfg.Helm, "get", "values", e.Name, "-n", ns,
		"--kubeconfig", in.cfg.Kubeconfig, "-o", "json")
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("reading values for %s/%s: %w", ns, e.Name, err)
	}
	values := map[string]any{}
	if trimmed := strings.TrimSpace(valuesOut); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &values); err != nil {
			return ReleaseInfo{}, fmt.Errorf("parsing values for %s/%s: %w", ns, e.Name, err)
		}
	}
	var redacted []string
	if cleaned, ok := redactValues(values, "", &redacted).(map[string]any); ok {
		values = cleaned
	}
	sort.Strings(redacted)

	return ReleaseInfo{
		Name:         firstNonEmpty(st.Name, e.Name),
		Namespace:    firstNonEmpty(st.Namespace, ns),
		Chart:        st.Chart.Metadata.Name,
		ChartVersion: st.Chart.Metadata.Version,
		AppVersion:   st.Chart.Metadata.AppVersion,
		Revision:     st.Version,
		Status:       st.Info.Status,
		Updated:      st.Info.LastDeployed,
		Values:       values,
		Redacted:     redacted,
	}, nil
}

func redactValues(v any, path string, redacted *[]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if secretValueKey.MatchString(k) {
				x[k] = "***"
				*redacted = append(*redacted, p)
				continue
			}
			x[k] = redactValues(val, p, redacted)
		}
		return x
	case []any:
		for i := range x {
			x[i] = redactValues(x[i], fmt.Sprintf("%s[%d]", path, i), redacted)
		}
		return x
	default:
		return v
	}
}
