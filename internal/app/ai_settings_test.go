package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rborso/umkim-thin/internal/catalog"
)

// The rule this file enforces: a settings edit produces a COMPLETE backend entry
// plus a matching gateway list. The chart REPLACES a catalog backend item rather
// than merging it, so anything omitted is silently dropped — omitting `models`
// once produced a stack that answered for no model at all.

func TestDefaultAISettingsInstallCleanly(t *testing.T) {
	s, err := NormalizeAISettings(DefaultAISettings())
	if err != nil {
		t.Fatalf("the defaults must be valid: %v", err)
	}
	if len(s.Models) == 0 {
		t.Fatal("the defaults must name a model")
	}
	if s.ContextSize != 2048 {
		t.Fatalf("default context = %d, want 2048 (the catalog's value)", s.ContextSize)
	}
	if s.UseGPU {
		t.Fatal("the default must be CPU-only: a cluster with no driver has to install unchanged")
	}
}

// The backend entry must carry every field the chart reads. A missing numeric
// one is worse than a missing string: llama.cpp parses several with stoi, so an
// empty value is a hard startup error, not a default.
func TestAIValuesEmitEveryFieldTheChartNeeds(t *testing.T) {
	values, err := AIValuesForBackend(DefaultAISettings(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	backends, ok := values["backends"].([]any)
	if !ok || len(backends) != 1 {
		t.Fatalf("want exactly one backend, got %#v", values["backends"])
	}
	b, ok := backends[0].(map[string]any)
	if !ok {
		t.Fatal("the backend entry must be a map")
	}
	for _, key := range []string{
		"name", "image", "imagePullPolicy", "models", "modelsMax",
		"modelsMemoryMargin", "ctxSize", "parallel", "threads",
		"preload", "persistence", "resources", "gpu",
	} {
		if _, ok := b[key]; !ok {
			t.Errorf("backend entry is missing %q; the chart replaces the list item, so it will not fall back", key)
		}
	}
	// The numeric ones the chart passes straight to llama.cpp.
	for _, key := range []string{"modelsMax", "modelsMemoryMargin", "ctxSize", "parallel"} {
		if _, ok := toInt(b[key]); !ok {
			t.Errorf("%q must render as a number, got %#v (llama.cpp parses it with stoi)", key, b[key])
		}
	}
	if margin, _ := toInt(b["modelsMemoryMargin"]); margin == 0 {
		t.Error("modelsMemoryMargin must not be empty: llama.cpp fails to start on an empty value")
	}
}

// The pair that must agree. This is the bug a hand-written UI would ship: the
// backend's model id and the gateway's upstreamModel are the same string, and a
// mismatch makes every request 404.
func TestAIGatewayMatchesBackendModelIDs(t *testing.T) {
	for _, models := range [][]string{
		{"qwen2.5-0.5b-instruct-q4_k_m"},
		{"qwen2.5-0.5b-instruct-q4_k_m", "qwen2.5-1.5b-instruct-q4_k_m"},
	} {
		s := DefaultAISettings()
		s.Models = models
		values, err := AIValuesForBackend(s, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}

		backend := values["backends"].([]any)[0].(map[string]any)
		backendModels := backend["models"].([]any)
		gateway := values["models"].([]any)

		if len(backendModels) != len(models) || len(gateway) != len(models) {
			t.Fatalf("models=%v: backend has %d, gateway has %d, want %d",
				models, len(backendModels), len(gateway), len(models))
		}
		for i, name := range models {
			gotBackend := backendModels[i].(map[string]any)["name"]
			gotUpstream := gateway[i].(map[string]any)["upstreamModel"]
			if gotBackend != name {
				t.Errorf("backend model %d = %v, want %q", i, gotBackend, name)
			}
			if gotUpstream != name {
				t.Errorf("gateway upstreamModel %d = %v, want %q (a mismatch 404s every request)", i, gotUpstream, name)
			}
			if url, _ := backendModels[i].(map[string]any)["url"].(string); !strings.HasSuffix(url, ".gguf") {
				t.Errorf("model %q has no .gguf url: %q", name, url)
			}
		}
	}
}

// GPU on means the CUDA image, a card request, and the offload flag's inputs.
// GPU off must carry NO card request, or a CPU-only cluster cannot install.
func TestAIGPUFlagDrivesImageAndResource(t *testing.T) {
	on := DefaultAISettings()
	on.UseGPU = true
	values, err := AIValuesForBackend(on, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	backend := values["backends"].([]any)[0].(map[string]any)
	gpu := backend["gpu"].(map[string]any)
	if enabled, _ := gpu["enabled"].(bool); !enabled {
		t.Error("gpu.enabled must be true when UseGPU is set")
	}
	if img, _ := gpu["image"].(string); !strings.Contains(img, "server-cuda") {
		t.Errorf("the GPU path must name the CUDA image, got %q", img)
	}
	if count, _ := toInt(gpu["count"]); count < 1 {
		t.Errorf("gpu.count must be at least 1, got %v", gpu["count"])
	}

	off := DefaultAISettings()
	off.UseGPU = false
	values, err = AIValuesForBackend(off, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	backend = values["backends"].([]any)[0].(map[string]any)
	gpu = backend["gpu"].(map[string]any)
	if enabled, _ := gpu["enabled"].(bool); enabled {
		t.Error("gpu.enabled must be false on the CPU path")
	}
	// The image must be the CPU build: the CPU image cannot use a card, and the
	// CUDA one on a node with no driver is a Pending pod.
	if img, _ := backend["image"].(string); strings.Contains(img, "cuda") {
		t.Errorf("the CPU path must not use a CUDA image, got %q", img)
	}
}

// A settings edit must not reset the parts it does not own (the web UI, the
// gateway, redis). Otherwise changing the context size silently drops OIDC.
func TestAIValuesPreserveUnrelatedSections(t *testing.T) {
	base := map[string]any{
		"webui": map[string]any{
			"oidc": map[string]any{"enabled": true},
		},
		"gateway": map[string]any{"sessionAffinity": true},
		"redis":   map[string]any{"enabled": false},
	}
	values, err := AIValuesForBackend(DefaultAISettings(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["webui"]; !ok {
		t.Error("webui settings were dropped: OIDC would be lost by a context-size change")
	}
	if _, ok := values["gateway"]; !ok {
		t.Error("gateway settings were dropped")
	}
	if _, ok := values["redis"]; !ok {
		t.Error("redis settings were dropped")
	}
}

// Validation must reject the things that install cleanly and then fail at
// runtime, and accept what is merely unusual.
func TestAINormalizeRejectsWhatWouldBreakAtRuntime(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*AISettings)
		wantErr string
	}{
		{"unknown model", func(s *AISettings) { s.Models = []string{"gpt-4"} }, "unknown model"},
		{"no models", func(s *AISettings) { s.Models = []string{"  "} }, "at least one model"},
		{"context too small", func(s *AISettings) { s.ContextSize = 64 }, "out of range"},
		{"context too large", func(s *AISettings) { s.ContextSize = 1 << 20 }, "out of range"},
		{"context not a power of two", func(s *AISettings) { s.ContextSize = 40000 }, "not a power of two"},
		{"parallel zero", func(s *AISettings) { s.Parallel = -1 }, "out of range"},
		{"parallel too high", func(s *AISettings) { s.Parallel = 99 }, "out of range"},
		{"bad split mode", func(s *AISettings) { s.SplitMode = "diagonal" }, "must be one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultAISettings()
			tc.mutate(&s)
			_, err := NormalizeAISettings(s)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestAINormalizeAcceptsAndCanonicalises(t *testing.T) {
	s := AISettings{
		// A duplicate and an empty entry: both are harmless and should be cleaned.
		Models:         []string{"qwen2.5-0.5b-instruct-q4_k_m", " ", "qwen2.5-0.5b-instruct-q4_k_m"},
		ContextSize:    0, // -> default
		Parallel:       0, // -> default
		UseGPU:         true,
		GPUsPerBackend: 2,
		SplitMode:      "", // -> default
	}
	got, err := NormalizeAISettings(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 {
		t.Fatalf("duplicates must collapse, got %v", got.Models)
	}
	if got.ContextSize != 2048 || got.Parallel != 1 || got.SplitMode != "layer" {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if got.GPUsPerBackend != 2 {
		t.Fatalf("an explicit non-default was overwritten: %d", got.GPUsPerBackend)
	}
}

// The console must propose what is RUNNING, not the catalog. Round-tripping the
// rendered values has to give back the settings that produced them.
func TestAISettingsRoundTripThroughValues(t *testing.T) {
	want := AISettings{
		Models:         []string{"qwen2.5-1.5b-instruct-q4_k_m", "qwen2.5-0.5b-instruct-q4_k_m"},
		ContextSize:    8192,
		Parallel:       2,
		UseGPU:         true,
		GPUsPerBackend: 1,
		SplitMode:      "layer",
	}
	values, err := AIValuesForBackend(want, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	// Through JSON, the way helm get values -> the API -> the console sees it.
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	got := AISettingsFromValues(decoded)
	got, err = NormalizeAISettings(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextSize != want.ContextSize {
		t.Errorf("context = %d, want %d", got.ContextSize, want.ContextSize)
	}
	if got.Parallel != want.Parallel {
		t.Errorf("parallel = %d, want %d", got.Parallel, want.Parallel)
	}
	if got.UseGPU != want.UseGPU {
		t.Errorf("useGpu = %v, want %v", got.UseGPU, want.UseGPU)
	}
	if strings.Join(got.Models, ",") != strings.Join(want.Models, ",") {
		t.Errorf("models = %v, want %v", got.Models, want.Models)
	}
}

// The volume must be big enough for the models chosen: too small is a failed
// download at install time, which reads as a broken app.
func TestAIStorageSizeCoversTheModels(t *testing.T) {
	s := DefaultAISettings()
	s.Models = []string{"qwen2.5-0.5b-instruct-q4_k_m", "qwen2.5-1.5b-instruct-q4_k_m"}
	values, err := AIValuesForBackend(s, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	backend := values["backends"].([]any)[0].(map[string]any)
	persistence := backend["persistence"].(map[string]any)
	size, _ := persistence["size"].(string)
	if !strings.HasSuffix(size, "Gi") {
		t.Fatalf("unexpected storage size %q", size)
	}
	var gib int
	if _, err := fmt.Sscanf(size, "%dGi", &gib); err != nil {
		t.Fatalf("parsing %q: %v", size, err)
	}
	var need int64
	for _, name := range s.Models {
		opt, _ := aiModelByName(name)
		need += opt.ApproxBytes
	}
	if int64(gib)<<30 < need {
		t.Fatalf("a %dGi volume cannot hold %d bytes of models", gib, need)
	}
}

// A warning, never a refusal: the operator may know better than the estimate.
//
// The estimate is calibrated against the real measurement on the lab's 4 GiB
// card: 0.5B q4 at 2048 context was 491 MiB resident, which this model predicts
// as 400 MiB of weights + 28 MiB of cache + overhead. So a 3B at a 32k context
// (2.98 GiB) correctly does NOT warn — it fits.
func TestAIModelFitsVRAMWarnsButDoesNotBlock(t *testing.T) {
	s := DefaultAISettings()
	s.UseGPU = true
	s.Models = []string{"qwen2.5-3b-instruct-q4_k_m"}

	// 3B weights plus a 128k cache is ~4.3 GiB: genuinely past a 4 GiB card.
	s.ContextSize = 131072
	warn := AIModelFitsVRAM(s, 4<<30)
	if warn == "" {
		t.Fatal("expected a warning for a 3B model at a 128k context on a 4 GiB card")
	}
	if !strings.Contains(warn, "reduce") {
		t.Errorf("the warning should say what to do, got %q", warn)
	}
	// The same model at the default context does fit, so no warning: a checker
	// that cries wolf on a working setup is worse than none.
	fits := s
	fits.ContextSize = 2048
	if w := AIModelFitsVRAM(fits, 4<<30); w != "" {
		t.Errorf("3B at 2048 context fits a 4 GiB card, got warning %q", w)
	}

	// The small model at the default context fits, so no warning on the GPU path.
	ok := DefaultAISettings()
	ok.UseGPU = true
	if w := AIModelFitsVRAM(ok, 4<<30); w != "" {
		t.Errorf("the default model must fit a 4 GiB card, got %q", w)
	}

	// CPU mode never warns: there is no card to exceed.
	cpu := DefaultAISettings()
	cpu.ContextSize = 32768
	if w := AIModelFitsVRAM(cpu, 4<<30); w != "" {
		t.Errorf("CPU mode must not produce a VRAM warning, got %q", w)
	}

	// An unknown VRAM means no claim.
	if w := AIModelFitsVRAM(s, 0); w != "" {
		t.Errorf("no VRAM reading must not produce a warning, got %q", w)
	}
}

// The context labels must show a real cost, not a bare number: the whole point is
// that 32768 is not free. Checked against the arithmetic, because an earlier
// version shipped "28.0 GiB" for a 28 MiB cache — a unit slip the loose
// "contains cache" assertion did not catch.
func TestAIContextOptionsCarryTheirCost(t *testing.T) {
	choices := AIContextOptions()
	if len(choices) < 3 {
		t.Fatalf("want a few choices, got %d", len(choices))
	}
	labels := map[int]string{}
	for _, c := range choices {
		if c.Value <= 0 {
			t.Errorf("context choice %+v has no value", c)
		}
		if !strings.Contains(c.Label, "cache") {
			t.Errorf("choice %d label %q does not mention its memory cost", c.Value, c.Label)
		}
		labels[c.Value] = c.Label
	}

	// 2048 tokens is 28 MiB at this geometry: it must say MiB, and must not claim
	// GiB for something a fraction of a GiB.
	if got := labels[2048]; !strings.Contains(got, "28 MiB") {
		t.Errorf("2048 tokens should label 28 MiB of cache, got %q", got)
	}
	// 32768 is 448 MiB: still MiB, still under a GiB.
	if got := labels[32768]; !strings.Contains(got, "448 MiB") {
		t.Errorf("32768 tokens should label 448 MiB of cache, got %q", got)
	}
	// No label may claim GiB at these sizes — the largest is 448 MiB.
	for size, label := range labels {
		if strings.Contains(label, "GiB") {
			t.Errorf("choice %d claims GiB of cache, which is off by 1024x: %q", size, label)
		}
	}
	if _, ok := labels[32768]; !ok {
		t.Error("expected a large context option so the cost is visible")
	}
}

// Every offered model must be usable: a name, a .gguf URL and a size.
func TestAIModelCatalogueIsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range AIModelOptions() {
		if m.Name == "" || m.Label == "" {
			t.Errorf("model %+v needs a name and a label", m)
		}
		if !strings.HasSuffix(m.URL, ".gguf") {
			t.Errorf("model %q url is not a .gguf: %q", m.Name, m.URL)
		}
		if m.ApproxBytes <= 0 || m.MinVRAMBytes <= 0 {
			t.Errorf("model %q needs a size and a VRAM estimate", m.Name)
		}
		if seen[m.Name] {
			t.Errorf("duplicate model %q", m.Name)
		}
		seen[m.Name] = true
	}
}

// A friendly name in the picker must not leak the quantisation suffix, while the
// upstream id keeps it.
func TestAIFriendlyModelName(t *testing.T) {
	cases := map[string]string{
		"qwen2.5-0.5b-instruct-q4_k_m": "qwen2.5-0.5b-instruct",
		"qwen2.5-1.5b-instruct-q5_k_m": "qwen2.5-1.5b-instruct",
		"plain-model":                  "plain-model",
	}
	for in, want := range cases {
		if got := aiFriendlyModelName(in); got != want {
			t.Errorf("aiFriendlyModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The install path renders the catalog's values through {{ .storage.configClass }}
// and friends, and the AI merge happens AFTER that. So the merge's own
// placeholders have to be rendered too — a real reinstall failed with
// 'spec.storageClassName: Invalid value: "{{ .storage.configClass }}"' because
// they were not.
//
// This calls mergeAISettings, the same function Install calls, so deleting the
// render step inside it fails here. An earlier version of this test made the
// render call itself and therefore passed against the bug.
func TestAISettingsPlaceholdersAreRenderedByTheInstallPath(t *testing.T) {
	store, err := catalog.NewStore("../../catalog", t.TempDir())
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	e, ok := store.Entry(AIApp)
	if !ok {
		t.Fatal("the catalog has no ai entry")
	}

	in := testInstaller()
	ctx, err := in.context(e, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Exactly what Install does around the AI merge.
	s := DefaultAISettings()
	rendered, err := in.mergeAISettings(e, ctx, in.renderValues(e, ctx), Options{AISettings: &s})
	if err != nil {
		t.Fatal(err)
	}

	backend := rendered["backends"].([]any)[0].(map[string]any)
	class, _ := backend["persistence"].(map[string]any)["storageClass"].(string)
	// testInstaller's site class; the point is that it is a real class and not the
	// template string Helm rejected.
	if class != in.cfg.ConfigClass {
		t.Errorf("storageClass = %q, want the site class %q", class, in.cfg.ConfigClass)
	}

	// A recursive sweep, so a placeholder added later cannot slip through.
	var walk func(any)
	walk = func(v any) {
		switch vv := v.(type) {
		case string:
			if strings.Contains(vv, "{{") {
				t.Errorf("unrendered placeholder in the values: %q", vv)
			}
		case map[string]any:
			for _, x := range vv {
				walk(x)
			}
		case []any:
			for _, x := range vv {
				walk(x)
			}
		}
	}
	walk(rendered)
}

// A release that is not the AI stack must be refused, not silently given AI
// values that its chart does not understand.
func TestAISettingsRefusedForOtherApps(t *testing.T) {
	in := testInstaller()
	s := DefaultAISettings()
	_, err := in.mergeAISettings(
		catalog.Entry{Name: "nextcloud"}, siteContext{}, map[string]any{}, Options{AISettings: &s})
	if err == nil || !strings.Contains(err.Error(), "does not take AI settings") {
		t.Fatalf("expected a refusal, got %v", err)
	}
}

// Sampling defaults must reach the chart, because a catalog override that omits
// them falls back to the chart silently while the console shows something else.
func TestAISamplingIsRendered(t *testing.T) {
	s := DefaultAISettings()
	s.Temperature = 0.2
	s.TopP = 0.9
	s.TopK = 20
	s.MinP = 0.05

	values, err := AIValuesForBackend(s, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	backend := values["backends"].([]any)[0].(map[string]any)
	sampling, ok := backend["sampling"].(map[string]any)
	if !ok {
		t.Fatal("the backend entry must carry a sampling block")
	}
	for key, want := range map[string]float64{
		"temperature": 0.2, "topP": 0.9, "topK": 20, "minP": 0.05,
	} {
		got, _ := toFloat(sampling[key])
		if got != want {
			t.Errorf("sampling.%s = %v, want %v", key, got, want)
		}
	}
}

// Every sampling field must survive the values -> JSON -> settings round trip, or
// reopening the panel would show the defaults and quietly overwrite the choice.
func TestAISamplingRoundTrips(t *testing.T) {
	want := DefaultAISettings()
	want.Temperature = 1.1
	want.TopP = 0.98
	want.TopK = 80
	want.MinP = 0.02

	values, err := AIValuesForBackend(want, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(values)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeAISettings(AISettingsFromValues(decoded))
	if err != nil {
		t.Fatal(err)
	}
	if got.Temperature != want.Temperature || got.TopP != want.TopP ||
		got.TopK != want.TopK || got.MinP != want.MinP {
		t.Fatalf("sampling did not round-trip: got %+v want %+v", got, want)
	}
}

// GPU layers is the offload knob. It must reach the chart, and it must change the
// VRAM estimate - otherwise the warning ignores the one lever that fixes the
// problem it is warning about.
func TestAIGPULayersDriveOffloadAndEstimate(t *testing.T) {
	full := DefaultAISettings()
	full.UseGPU = true
	full.Models = []string{"qwen2.5-3b-instruct-q4_k_m"}
	full.ContextSize = 32768
	full.GPULayers = 999

	partial := full
	partial.GPULayers = 8

	vf, err := AIValuesForBackend(full, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	vp, err := AIValuesForBackend(partial, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	gotFull, _ := toInt(vf["backends"].([]any)[0].(map[string]any)["gpuLayers"])
	gotPartial, _ := toInt(vp["backends"].([]any)[0].(map[string]any)["gpuLayers"])
	if gotFull != 999 || gotPartial != 8 {
		t.Fatalf("gpuLayers = %d/%d, want 999/8", gotFull, gotPartial)
	}

	// The estimate must move with it, and by roughly the layer fraction.
	const card = 4 << 30
	ef := AIEstimateVRAM(full, card)
	ep := AIEstimateVRAM(partial, card)
	if ep.TotalBytes >= ef.TotalBytes {
		t.Fatalf("offloading 8 layers (%d bytes) must need less VRAM than all (%d bytes)",
			ep.TotalBytes, ef.TotalBytes)
	}
	// 8/32 of the weights, plus the cache. The weights are the dominant term.
	if ep.WeightsBytes > ef.WeightsBytes/3 {
		t.Fatalf("8 layers should be about a quarter of the weights, got %d of %d",
			ep.WeightsBytes, ef.WeightsBytes)
	}
}

// A CPU backend must not carry a layer count: llama.cpp ignores it there, and a
// value in the values file invites the reader to think it matters.
func TestAIGPULayersAbsentOnCPU(t *testing.T) {
	s := DefaultAISettings()
	s.UseGPU = false
	values, err := AIValuesForBackend(s, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	backend := values["backends"].([]any)[0].(map[string]any)
	if _, ok := backend["gpuLayers"]; ok {
		t.Error("a CPU backend must not carry gpuLayers")
	}
}

// The presets must be usable as-is and must be distinguishable: three presets
// that produce the same numbers would be a decoration.
func TestAISamplingPresetsAreDistinctAndValid(t *testing.T) {
	presets := AISamplingPresets()
	if len(presets) < 2 {
		t.Fatalf("want a few presets, got %d", len(presets))
	}
	seen := map[string]bool{}
	for _, p := range presets {
		if p.Name == "" || p.Label == "" || p.Hint == "" {
			t.Errorf("preset %+v needs a name, label and hint", p)
		}
		if seen[p.Name] {
			t.Errorf("duplicate preset %q", p.Name)
		}
		seen[p.Name] = true

		// Applying the preset must produce settings that validate.
		s := DefaultAISettings()
		s.Temperature, s.TopP, s.TopK, s.MinP = p.Temperature, p.TopP, p.TopK, p.MinP
		if _, err := NormalizeAISettings(s); err != nil {
			t.Errorf("preset %q does not validate: %v", p.Name, err)
		}
	}

	// "Precise" must actually be more deterministic than "creative".
	byName := map[string]AISamplingPreset{}
	for _, p := range presets {
		byName[p.Name] = p
	}
	if byName["precise"].Temperature >= byName["creative"].Temperature {
		t.Error("precise must have a lower temperature than creative")
	}
}

// A hand-set value that matches no preset must still highlight the closest one,
// rather than leaving the panel with nothing selected.
func TestAISamplingPresetForFallsBackToClosest(t *testing.T) {
	s := DefaultAISettings()
	s.Temperature, s.TopP, s.TopK, s.MinP = 0.8, 0.95, 40, 0.05
	if p, exact := AISamplingPresetFor(s); !exact || p.Name != "balanced" {
		t.Errorf("the llama.cpp defaults must match the balanced preset, got %q exact=%v", p.Name, exact)
	}

	s.Temperature = 0.75
	p, exact := AISamplingPresetFor(s)
	if exact {
		t.Error("a non-preset temperature must not report an exact match")
	}
	if p.Name != "balanced" {
		t.Errorf("0.75 is closest to balanced, got %q", p.Name)
	}
}

// The new ranges must reject what would produce a broken or useless backend.
func TestAINormalizeRejectsBadSampling(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*AISettings)
		wantErr string
	}{
		{"temperature too high", func(s *AISettings) { s.Temperature = 3 }, "temperature"},
		{"negative temperature", func(s *AISettings) { s.Temperature = -0.5 }, "temperature"},
		{"top-p above one", func(s *AISettings) { s.TopP = 1.5 }, "top-p"},
		{"negative top-p", func(s *AISettings) { s.TopP = -0.1 }, "top-p"},
		{"top-k negative", func(s *AISettings) { s.TopK = -5 }, "top-k"},
		{"min-p above one", func(s *AISettings) { s.MinP = 2 }, "min-p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultAISettings()
			tc.mutate(&s)
			_, err := NormalizeAISettings(s)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// Boundary values must be accepted: temperature 0 (deterministic) and top-k 0
// (disabled) are legitimate, not typos.
func TestAINormalizeAcceptsSamplingBoundaries(t *testing.T) {
	s := DefaultAISettings()
	s.Temperature = 0
	s.TopK = 0
	s.MinP = 0
	s.TopP = 1.0
	got, err := NormalizeAISettings(s)
	if err != nil {
		t.Fatalf("boundary sampling values must be accepted: %v", err)
	}
	if got.Temperature != 0 || got.TopK != 0 {
		t.Fatalf("boundaries were rewritten: %+v", got)
	}
}
