package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
)

// The AI stack's install-time settings.
//
// The console has to expose these because the catalog alone cannot: the model,
// the context window and the GPU opt-in are per-install decisions about one
// machine's memory and one household's hardware, not about which app to run.
//
// The single most important rule here is that a settings edit RENDERS A COMPLETE
// backend entry. A catalog `values.backends[]` item REPLACES the chart's list
// item rather than merging into it, so a partial entry silently drops whatever it
// omits - omitting `models` once produced a stack that answered for no model at
// all. Every value below is therefore written explicitly, with the chart's own
// default as the fallback, and never left out.

// AIApp is the catalog name of the stack.
const AIApp = "ai"

// AIContextChoice is one selectable context window, with the memory it costs
// expressed in a way a person can judge. `BytesPerToken` comes from the KV cache
// geometry, not a guess: see aiKVBytesPerToken.
type AIContextChoice struct {
	// Value is what goes into `ctxSize`.
	Value int `json:"value"`
	// Label is what the console shows, e.g. "4096 tokens (about 0.5 GiB)".
	Label string `json:"label"`
	// Hint explains the trade in one sentence.
	Hint string `json:"hint"`
}

// AIModelOption is a model the console offers. `Name` is the GGUF file name
// without .gguf, which is also the id llama.cpp answers to in router mode, so
// the gateway entry is derived from it rather than typed twice.
type AIModelOption struct {
	// Name is the GGUF file stem: the backend's model id.
	Name string `json:"name"`
	// Label is the friendly name shown in the console.
	Label string `json:"label"`
	// URL is where the weights are fetched from.
	URL string `json:"url"`
	// ApproxBytes is the download size, for the "this will take a while" hint.
	ApproxBytes int64 `json:"approxBytes"`
	// MinVRAMBytes is roughly what it takes to run with a small context on a GPU.
	MinVRAMBytes int64 `json:"minVramBytes"`
	// Notes is a one-line description of the trade (quality vs speed).
	Notes string `json:"notes"`
}

// AISettings is the editable part of one AI install.
type AISettings struct {
	// Models is the ordered list of GGUF file stems this backend serves.
	Models []string `json:"models"`
	// ContextSize is `ctxSize`.
	ContextSize int `json:"contextSize"`
	// Parallel is how many requests one backend handles at once.
	Parallel int `json:"parallel"`
	// UseGPU moves the backend to the CUDA image and requests a card.
	UseGPU bool `json:"useGpu"`
	// GPUsPerBackend is `gpu.count`; only meaningful with UseGPU.
	GPUsPerBackend int `json:"gpusPerBackend"`
	// SplitMode is `gpu.splitMode`, for a host with more than one card.
	SplitMode string `json:"splitMode"`
}

// aiModelCatalogue is the built-in set the console offers. It is deliberately
// short and each entry is sized for a small host: a 4 GiB card runs the 0.5B
// model comfortably, the 1.5B with a small context, and the 3B not at all.
func aiModelCatalogue() []AIModelOption {
	return []AIModelOption{
		{
			Name:         "qwen2.5-0.5b-instruct-q4_k_m",
			Label:        "Qwen2.5 0.5B (fastest)",
			URL:          "https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf",
			ApproxBytes:  469 << 20, // measured from the URL's Content-Length
			MinVRAMBytes: 700 << 20,
			Notes:        "Answers in well under a second on a small GPU. Short answers, weaker reasoning.",
		},
		{
			Name:         "qwen2.5-1.5b-instruct-q4_k_m",
			Label:        "Qwen2.5 1.5B (balanced)",
			URL:          "https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf",
			ApproxBytes:  1066 << 20,
			MinVRAMBytes: 1600 << 20,
			Notes:        "Noticeably better writing and reasoning, still quick on a 4 GiB card.",
		},
		{
			Name:         "qwen2.5-3b-instruct-q4_k_m",
			Label:        "Qwen2.5 3B (best quality)",
			URL:          "https://huggingface.co/Qwen/Qwen2.5-3B-Instruct-GGUF/resolve/main/qwen2.5-3b-instruct-q4_k_m.gguf",
			ApproxBytes:  2007 << 20,
			MinVRAMBytes: 2600 << 20,
			Notes:        "Needs about 3 GiB of VRAM and most of a 4 GiB card; slower to load.",
		},
	}
}

// AIModelOptions returns the built-in catalogue.
func AIModelOptions() []AIModelOption { return aiModelCatalogue() }

// aiKVBytesPerToken is the KV cache cost of one token for a Qwen2.5 model under
// llama.cpp's defaults (f16 cache, 28 layers x 2 K/V heads x 128 head dim ->
// 14336 values per token). 28*2*128*2 bytes = 14336. It is used only to LABEL a
// context choice and to warn, never to reject: the point is that "32768" is not
// a free number, which a bare text box would imply.
func aiKVBytesPerToken() int64 { return 28 * 2 * 128 * 2 }

// aiContextChoices are the offered context windows, with their KV cost.
func aiContextChoices() []AIContextChoice {
	out := make([]AIContextChoice, 0, 5)
	for _, size := range []int{2048, 4096, 8192, 16384, 32768} {
		// Work in BYTES and convert once at the end: an earlier version kept KiB
		// in a variable named for bytes and printed "28.0 GiB" for 28 MiB.
		bytes := int64(size) * aiKVBytesPerToken()
		mib := float64(bytes) / (1 << 20)
		// MiB below a GiB, so adjacent choices never round to the same label:
		// "0.1 GiB" for both 4096 and 8192 tells a person nothing.
		var cost string
		if mib < 1024 {
			cost = fmt.Sprintf("%.0f MiB", mib)
		} else {
			cost = fmt.Sprintf("%.1f GiB", mib/1024)
		}
		label := fmt.Sprintf("%d tokens (%s of cache)", size, cost)
		hint := "Larger contexts remember more of a conversation and cost memory per request."
		if size >= 16384 {
			hint = "Large contexts need a model trained for them; most small models are trained far shorter."
		}
		out = append(out, AIContextChoice{Value: size, Label: label, Hint: hint})
	}
	return out
}

// AIContextOptions returns the selectable context windows.
func AIContextOptions() []AIContextChoice { return aiContextChoices() }

// DefaultAISettings is what a fresh install gets: the catalog's own defaults, so
// the console and the catalog cannot disagree.
func DefaultAISettings() AISettings {
	return AISettings{
		Models:         []string{"qwen2.5-0.5b-instruct-q4_k_m"},
		ContextSize:    2048,
		Parallel:       1,
		UseGPU:         false,
		GPUsPerBackend: 1,
		SplitMode:      "layer",
	}
}

// aiSplitModes are the values the chart accepts for `gpu.splitMode`.
var aiSplitModes = map[string]bool{"layer": true, "tensor": true, "row": true}

// NormalizeAISettings validates and fills in an AI settings value. It is strict
// about anything that would produce a stack which installs and then does not
// work, because those failures are invisible until a user asks a question.
func NormalizeAISettings(s AISettings) (AISettings, error) {
	def := DefaultAISettings()

	// Models: at least one, known, de-duplicated, order preserved.
	if len(s.Models) == 0 {
		s.Models = def.Models
	}
	known := map[string]AIModelOption{}
	for _, m := range aiModelCatalogue() {
		known[m.Name] = m
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(s.Models))
	for _, name := range s.Models {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := known[name]; !ok {
			return AISettings{}, fmt.Errorf("unknown model %q: pick one of %s", name, strings.Join(aiModelNames(), ", "))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return AISettings{}, fmt.Errorf("at least one model is required")
	}
	s.Models = out

	if s.ContextSize == 0 {
		s.ContextSize = def.ContextSize
	}
	if s.ContextSize < 512 || s.ContextSize > 131072 {
		return AISettings{}, fmt.Errorf("context size %d is out of range (512-131072)", s.ContextSize)
	}
	// A power of two is not required by llama.cpp, but an operator typing 40000
	// usually means 40960; refusing the typo is friendlier than accepting it.
	if s.ContextSize&(s.ContextSize-1) != 0 {
		return AISettings{}, fmt.Errorf("context size %d is not a power of two; try %d", s.ContextSize, nextPowerOfTwo(s.ContextSize))
	}

	if s.Parallel == 0 {
		s.Parallel = def.Parallel
	}
	if s.Parallel < 1 || s.Parallel > 8 {
		return AISettings{}, fmt.Errorf("parallel requests %d is out of range (1-8)", s.Parallel)
	}

	if s.GPUsPerBackend == 0 {
		s.GPUsPerBackend = def.GPUsPerBackend
	}
	if s.GPUsPerBackend < 1 {
		return AISettings{}, fmt.Errorf("GPUs per backend must be at least 1")
	}
	if s.SplitMode == "" {
		s.SplitMode = def.SplitMode
	}
	if !aiSplitModes[s.SplitMode] {
		return AISettings{}, fmt.Errorf("split mode %q must be one of layer, row, tensor", s.SplitMode)
	}
	return s, nil
}

func aiModelNames() []string {
	names := make([]string, 0, len(aiModelCatalogue()))
	for _, m := range aiModelCatalogue() {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	return names
}

func nextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// GPUInfo is what the cluster can tell the console about its cards. The console
// needs it to offer a GPU toggle honestly: a toggle that is on with no card just
// produces a Pending pod, and a context size that cannot fit produces a crash
// loop at model load.
type GPUInfo struct {
	// Count is the total nvidia.com/gpu the nodes advertise.
	Count int `json:"count"`
	// Nodes lists the nodes advertising a card.
	Nodes []string `json:"nodes"`
	// VRAMBytes is the largest card's memory, when the device plugin labels it.
	VRAMBytes int64 `json:"vramBytes"`
}

// Available reports whether a GPU is schedulable at all.
func (g GPUInfo) Available() bool { return g.Count > 0 }

// GPUs reads the cluster's advertised GPU resource. An error is returned only
// when the question cannot be asked; no GPU is a normal answer, not an error.
func (in *Installer) GPUs() (GPUInfo, error) {
	out, err := in.run(in.cfg.Kubectl, "--kubeconfig", in.cfg.Kubeconfig,
		"get", "nodes", "-o", "json")
	if err != nil {
		return GPUInfo{}, err
	}
	var doc struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return GPUInfo{}, fmt.Errorf("parsing nodes: %w", err)
	}
	var info GPUInfo
	for _, node := range doc.Items {
		// The allocatable value is a Kubernetes quantity; for a count it is a
		// plain integer string.
		n, ok := toInt(node.Status.Allocatable["nvidia.com/gpu"])
		if !ok || n <= 0 {
			continue
		}
		info.Count += n
		info.Nodes = append(info.Nodes, node.Metadata.Name)
		// The GPU feature discovery labels are only present when NFD runs, which
		// this platform does not install, so VRAM is best-effort.
		if v := node.Metadata.Labels["nvidia.com/gpu.memory"]; v != "" {
			if mib, ok := toInt(v); ok && int64(mib)<<20 > info.VRAMBytes {
				info.VRAMBytes = int64(mib) << 20
			}
		}
	}
	return info, nil
}

// AISettingsFor reads the CURRENT settings of an installed AI release, so the
// console proposes what is running rather than what the catalog says. A panel
// that reopens on catalog defaults would silently revert an operator's choice on
// the next save.
func (in *Installer) AISettingsFor(release, namespace string) (AISettings, error) {
	out, err := in.run(in.cfg.Helm, "get", "values", release,
		"-n", namespace, "--kubeconfig", in.cfg.Kubeconfig, "-o", "json")
	if err != nil || strings.TrimSpace(out) == "" {
		return DefaultAISettings(), nil
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(out), &values); err != nil {
		return DefaultAISettings(), fmt.Errorf("reading %s values: %w", release, err)
	}
	current := AISettingsFromValues(values)
	// Normalize so the console gets a value it can render, even if the release
	// was installed from a catalog entry that named a model outside this list.
	normalized, err := NormalizeAISettings(current)
	if err != nil {
		return DefaultAISettings(), nil
	}
	return normalized, nil
}

// toInt reads a number from decoded YAML/JSON, where an integer may arrive as
// int, int64 or float64 depending on the decoder.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// aiModelByName resolves a catalogued model.
func aiModelByName(name string) (AIModelOption, bool) {
	for _, m := range aiModelCatalogue() {
		if m.Name == name {
			return m, true
		}
	}
	return AIModelOption{}, false
}

// AISettingsFromValues reads the editable settings out of a rendered values map.
// It is what makes the console show the CURRENT state rather than the catalog's:
// after an operator changes a model, reopening the panel must not silently
// propose the old one.
func AISettingsFromValues(values map[string]any) AISettings {
	s := DefaultAISettings()
	backends, ok := values["backends"].([]any)
	if !ok || len(backends) == 0 {
		return s
	}
	first, ok := backends[0].(map[string]any)
	if !ok {
		return s
	}
	if n, ok := first["ctxSize"]; ok {
		if v, ok := toInt(n); ok && v > 0 {
			s.ContextSize = v
		}
	}
	if n, ok := first["parallel"]; ok {
		if v, ok := toInt(n); ok && v > 0 {
			s.Parallel = v
		}
	}
	if gpu, ok := first["gpu"].(map[string]any); ok {
		if b, ok := gpu["enabled"].(bool); ok {
			s.UseGPU = b
		}
		if n, ok := gpu["count"]; ok {
			if v, ok := toInt(n); ok && v > 0 {
				s.GPUsPerBackend = v
			}
		}
		if v, ok := gpu["splitMode"].(string); ok && v != "" {
			s.SplitMode = v
		}
	}
	if models, ok := first["models"].([]any); ok {
		names := make([]string, 0, len(models))
		for _, m := range models {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			// The model id is the upstream ALIAS when one is set, because that is
			// what the backend answers to; the file name is the fallback.
			name, _ := mm["name"].(string)
			if alias := strings.TrimSpace(fmt.Sprint(mm["alias"])); alias != "" && alias != "<nil>" {
				name = alias
			}
			if name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			s.Models = names
		}
	}
	return s
}

// AIValuesForBackend renders ONE complete backend entry and the gateway model
// list that must agree with it.
//
// Both halves are produced here, together, because they are the pair that has to
// match: `backends[].models[].name` is the id llama.cpp answers to, and
// `models[].upstreamModel` must be that same id or every request 404s. A UI that
// let a person edit one and retype the other would ship that bug eventually.
func AIValuesForBackend(s AISettings, base map[string]any) (map[string]any, error) {
	s, err := NormalizeAISettings(s)
	if err != nil {
		return nil, err
	}

	// Start from the chart's own backend defaults so nothing is left unset.
	backend := map[string]any{
		"name":            "llama-a",
		"image":           "ghcr.io/ggml-org/llama.cpp:server",
		"imagePullPolicy": "IfNotPresent",
		"modelsMax":       len(s.Models),
		// llama.cpp parses this with stoi: an empty value is a hard startup
		// error, so it is always a number.
		"modelsMemoryMargin": 256,
		"ctxSize":            s.ContextSize,
		"parallel":           s.Parallel,
		"threads":            0,
		"persistence": map[string]any{
			"size":         aiModelStorageSize(s.Models),
			"storageClass": "{{ .storage.configClass }}",
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "250m", "memory": "512Mi"},
			"limits":   map[string]any{"memory": "1536Mi"},
		},
	}

	models := make([]any, 0, len(s.Models))
	gateway := make([]any, 0, len(s.Models))
	for _, name := range s.Models {
		opt, ok := aiModelByName(name)
		if !ok {
			return nil, fmt.Errorf("unknown model %q", name)
		}
		models = append(models, map[string]any{"name": name, "url": opt.URL})
		// The gateway publishes a friendly name, but points at the id the backend
		// answers to. Deriving both from one list is the whole point.
		gateway = append(gateway, map[string]any{
			"name":          aiFriendlyModelName(name),
			"upstreamModel": name,
		})
	}
	backend["models"] = models
	// Preload the first model so the first user request does not pay the load.
	backend["preload"] = s.Models[0]

	if s.UseGPU {
		backend["gpu"] = map[string]any{
			"enabled":     true,
			"image":       "ghcr.io/ggml-org/llama.cpp:server-cuda",
			"count":       s.GPUsPerBackend,
			"splitMode":   s.SplitMode,
			"tensorSplit": "",
		}
		// A GPU backend loads the model into VRAM; the pod's memory limit must
		// cover the tokenizer and the host-side buffer, not the weights.
		backend["resources"] = map[string]any{
			"requests": map[string]any{"cpu": "250m", "memory": "512Mi"},
			"limits":   map[string]any{"memory": "2Gi"},
		}
	} else {
		backend["gpu"] = map[string]any{"enabled": false}
	}

	out := map[string]any{
		"backends": []any{backend},
		"models":   gateway,
	}
	// Carry through anything the caller already had that this function does not
	// own (webui settings, the gateway, redis) so a settings edit is not a reset.
	for k, v := range base {
		if k == "backends" || k == "models" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// aiFriendlyModelName turns a GGUF stem into something readable in the picker:
// "qwen2.5-0.5b-instruct-q4_k_m" -> "qwen2.5-0.5b-instruct".
func aiFriendlyModelName(name string) string {
	base := strings.TrimSuffix(path.Base(name), ".gguf")
	for _, suffix := range []string{"-q4_k_m", "-q4_K_M", "-q5_k_m", "-q8_0", "-f16"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base
}

// aiModelStorageSize sizes the model volume from the models chosen, with room
// for a partial download and a second model. Too small is a failed fetch at
// install time, which reads as "the app is broken".
func aiModelStorageSize(models []string) string {
	var total int64
	for _, name := range models {
		if opt, ok := aiModelByName(name); ok {
			total += opt.ApproxBytes
		}
	}
	// A byte count with no models would be a zero-size claim.
	if total == 0 {
		return "3Gi"
	}
	// 1 GiB floor plus 40% headroom, rounded up to whole GiB.
	gib := (total/(1<<30) + 1) * 14 / 10
	if gib < 3 {
		gib = 3
	}
	return fmt.Sprintf("%dGi", gib)
}

// AIModelFitsVRAM reports whether the chosen models and context plausibly fit
// the card. It returns a warning, never an error: the number is an estimate and
// the operator may know better.
func AIModelFitsVRAM(s AISettings, vramBytes int64) string {
	if !s.UseGPU || vramBytes <= 0 {
		return ""
	}
	var weights int64
	for _, name := range s.Models {
		if opt, ok := aiModelByName(name); ok {
			weights += opt.MinVRAMBytes
		}
	}
	cache := int64(s.ContextSize) * aiKVBytesPerToken() * int64(s.Parallel)
	need := weights + cache
	if need <= vramBytes {
		return ""
	}
	return fmt.Sprintf(
		"this needs about %.1f GiB but the card has %.1f GiB: reduce the context size or choose a smaller model",
		float64(need)/(1<<30), float64(vramBytes)/(1<<30))
}

// AIDefaultHostPath is a small helper for the console's model URL preview.
func AIModelURL(name string) string {
	if opt, ok := aiModelByName(name); ok {
		if u, err := url.Parse(opt.URL); err == nil {
			return u.String()
		}
		return opt.URL
	}
	return ""
}
