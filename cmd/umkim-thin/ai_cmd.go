package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/rborso/umkim-thin/internal/app"
	"github.com/rborso/umkim-thin/internal/catalog"
)

// The AI stack's settings surface, for the CLI.
//
// It exists alongside the console panel for the reason the project keeps pairing
// them: a feature that only one of them has drifts. Both write through the same
// app.AISettings and app.AIValuesForBackend, so neither can produce a values file
// the other would not.

func aiCmd(dir string, args []string) error {
	if wantsHelp(args) {
		fmt.Print(`usage: umkim-thin ai <command>

  settings                       show the current model, context and GPU choice
  settings --model M             set the models (repeatable; first is preloaded)
  settings --context N           set the context window (power of two)
  settings --parallel N          concurrent requests per backend (1-8)
  settings --gpu | --no-gpu      use the GPU (CUDA image + a card), or not
  settings --gpus-per-backend N  cards per backend with --gpu
  settings --gpu-layers N        layers to offload (999 = all; fewer fits a bigger model)
  settings --preset P            sampling preset: precise | balanced | creative
  settings --temperature N       sampling temperature (0-2)
  settings --top-p N             nucleus sampling (0-1, 1.0 disables)
  settings --top-k N             candidate tokens per step (0-1000, 0 disables)
  settings --min-p N             drop tokens below this fraction (0-1, 0 disables)
  presets                        list the sampling presets
  models                         list the models this console offers

Changing settings reinstalls the stack with the new values. The model list and the
gateway entries are rewritten together, because they have to agree.
`)
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("ai needs a subcommand: settings | models | presets")
	}

	in := app.New(app.ConfigFromEnv())
	switch args[0] {
	case "presets":
		fmt.Printf("%-10s %-24s %-6s %-6s %-5s %-6s\n", "NAME", "LABEL", "TEMP", "TOP-P", "TOP-K", "MIN-P")
		for _, p := range app.AISamplingPresets() {
			fmt.Printf("%-10s %-24s %-6.2f %-6.2f %-5d %-6.2f\n",
				p.Name, p.Label, p.Temperature, p.TopP, p.TopK, p.MinP)
			fmt.Printf("  %s\n", p.Hint)
		}
		return nil

	case "models":
		fmt.Printf("%-34s %-24s %8s %10s\n", "NAME", "LABEL", "DOWNLOAD", "MIN VRAM")
		for _, m := range app.AIModelOptions() {
			fmt.Printf("%-34s %-24s %8s %10s\n",
				m.Name, m.Label, humanBytes(m.ApproxBytes), humanBytes(m.MinVRAMBytes))
			fmt.Printf("  %s\n", m.Notes)
		}
		return nil

	case "settings":
		if err := aiSettingsCmd(dir, in, args[1:]); err != nil {
			return err
		}
		return nil

	default:
		return fmt.Errorf("unknown ai command %q: settings | models", args[0])
	}
}

// aiSettingsCmd parses the flags, and either prints or applies.
func aiSettingsCmd(dir string, in *app.Installer, args []string) error {
	var (
		models         []string
		contextSize    int
		parallel       int
		gpusPerBackend int
		gpuLayers      int
		useGPU         *bool
		preset         string
		temperature    float64
		topP           float64
		minP           float64
		topK           int
		changed        bool
	)
	fs := flag.NewFlagSet("ai settings", flag.ContinueOnError)
	fs.Var((*stringList)(&models), "model", "model name (repeatable; the first is preloaded)")
	fs.IntVar(&contextSize, "context", 0, "context window in tokens (a power of two)")
	fs.IntVar(&parallel, "parallel", 0, "concurrent requests per backend (1-8)")
	fs.IntVar(&gpusPerBackend, "gpus-per-backend", 0, "cards per backend (with --gpu)")
	fs.IntVar(&gpuLayers, "gpu-layers", 0, "layers to offload to the GPU (999 = all)")
	fs.StringVar(&preset, "preset", "", "sampling preset: precise|balanced|creative")
	fs.Float64Var(&temperature, "temperature", -1, "sampling temperature (0-2)")
	fs.Float64Var(&topP, "top-p", -1, "nucleus sampling (0-1)")
	fs.IntVar(&topK, "top-k", -1, "candidate tokens per step (0-1000)")
	fs.Float64Var(&minP, "min-p", -1, "drop tokens below this fraction (0-1)")
	fs.BoolFunc("gpu", "use the GPU (CUDA image, requests a card)", func(v string) error {
		b, err := parseBoolFlag(v)
		if err != nil {
			return err
		}
		useGPU = &b
		return nil
	})
	fs.BoolFunc("no-gpu", "run on CPU only", func(v string) error {
		b, err := parseBoolFlag(v)
		if err != nil {
			return err
		}
		no := !b
		useGPU = &no
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) { changed = true })

	entry, err := aiCatalogEntry(dir)
	if err != nil {
		return err
	}

	installed, namespace := aiInstalled(in)

	// Start from what is running, so a single flag does not reset the rest.
	current := app.DefaultAISettings()
	if installed {
		if cur, err := in.AISettingsFor("ai", namespace); err == nil {
			current = cur
		}
	}
	if len(models) > 0 {
		current.Models = models
	}
	if contextSize > 0 {
		current.ContextSize = contextSize
	}
	if parallel > 0 {
		current.Parallel = parallel
	}
	if gpusPerBackend > 0 {
		current.GPUsPerBackend = gpusPerBackend
	}
	if gpuLayers > 0 {
		current.GPULayers = gpuLayers
	}
	// A preset is shorthand for the four sampling numbers; explicit flags win,
	// because someone who typed both meant the specific value.
	if preset != "" {
		found := false
		for _, p := range app.AISamplingPresets() {
			if p.Name == preset {
				current.Temperature, current.TopP, current.TopK, current.MinP =
					p.Temperature, p.TopP, p.TopK, p.MinP
				found = true
				break
			}
		}
		if !found {
			names := make([]string, 0, len(app.AISamplingPresets()))
			for _, p := range app.AISamplingPresets() {
				names = append(names, p.Name)
			}
			return fmt.Errorf("unknown preset %q: %s", preset, strings.Join(names, ", "))
		}
	}
	if temperature >= 0 {
		current.Temperature = temperature
	}
	if topP >= 0 {
		current.TopP = topP
	}
	if topK >= 0 {
		current.TopK = topK
	}
	if minP >= 0 {
		current.MinP = minP
	}
	if useGPU != nil {
		current.UseGPU = *useGPU
	}
	normalized, err := app.NormalizeAISettings(current)
	if err != nil {
		return err
	}

	if !changed {
		return printAISettings(in, installed, normalized)
	}

	gpu, _ := in.GPUs()
	if normalized.UseGPU && !gpu.Available() {
		return fmt.Errorf("--gpu was asked for but no node advertises nvidia.com/gpu; " +
			"install the driver and device plugin first (see docs/gpu-agent.md)")
	}
	if warn := app.AIModelFitsVRAM(normalized, gpu.VRAMBytes); warn != "" {
		fmt.Printf("warning: %s\n", warn)
	}

	if !installed {
		fmt.Printf("installing ai with the new settings...\n")
	} else {
		fmt.Printf("reinstalling ai to apply the new settings...\n")
	}
	res, err := in.Install(entry, app.Options{
		AISettings:   &normalized,
		GPUVRAMBytes: gpu.VRAMBytes,
		Upgrade:      installed,
		Progress: func(s string) {
			fmt.Printf("  %s\n", s)
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("applied: %s in %s\n", res.Name, res.Namespace)
	for _, h := range res.Routes {
		fmt.Printf("  https://%s\n", h)
	}
	return nil
}

// printAISettings renders the current choice with the memory it implies.
func printAISettings(in *app.Installer, installed bool, s app.AISettings) error {
	state := "not installed (these are the defaults a fresh install would use)"
	if installed {
		state = "installed"
	}
	fmt.Printf("ai stack: %s\n", state)
	fmt.Printf("  models:    %s\n", strings.Join(s.Models, ", "))
	for _, name := range s.Models {
		for _, m := range app.AIModelOptions() {
			if m.Name == name {
				fmt.Printf("             %s, %s download\n", m.Label, humanBytes(m.ApproxBytes))
			}
		}
	}
	fmt.Printf("  context:   %d tokens\n", s.ContextSize)
	fmt.Printf("  parallel:  %d request(s)\n", s.Parallel)
	presetName, exact := app.AISamplingPresetFor(s)
	label := presetName.Label
	if !exact {
		label = fmt.Sprintf("custom (closest preset: %s)", presetName.Label)
	}
	fmt.Printf("  sampling:  %s — temp %.2f, top-p %.2f, top-k %d, min-p %.2f\n",
		label, s.Temperature, s.TopP, s.TopK, s.MinP)
	if s.UseGPU {
		gpu, _ := in.GPUs()
		layers := "all"
		if s.GPULayers > 0 && s.GPULayers < 999 {
			layers = fmt.Sprintf("%d", s.GPULayers)
		}
		fmt.Printf("  compute:   GPU x%d per backend (split %s, %s layers offloaded)",
			s.GPUsPerBackend, s.SplitMode, layers)
		if gpu.Available() {
			fmt.Printf(", %d card(s) on %s", gpu.Count, strings.Join(gpu.Nodes, ","))
		} else {
			fmt.Printf(" — WARNING: no node advertises nvidia.com/gpu")
		}
		fmt.Printf("\n")
		if warn := app.AIModelFitsVRAM(s, gpu.VRAMBytes); warn != "" {
			fmt.Printf("  warning:   %s\n", warn)
		}
	} else {
		fmt.Printf("  compute:   CPU only\n")
	}
	return nil
}

// aiInstalled reports whether the stack is installed, and where.
func aiInstalled(in *app.Installer) (bool, string) {
	st, err := in.List()
	if err != nil {
		return false, "ai"
	}
	for _, item := range st {
		if item.Name == app.AIApp {
			ns := item.Namespace
			if ns == "" {
				ns = "ai"
			}
			return true, ns
		}
	}
	return false, "ai"
}

func aiCatalogEntry(dir string) (catalog.Entry, error) {
	entries, err := catalog.Load(dir)
	if err != nil {
		return catalog.Entry{}, err
	}
	for _, e := range entries {
		if e.Name == app.AIApp {
			return e, nil
		}
	}
	return catalog.Entry{}, fmt.Errorf("the catalog has no %q entry", app.AIApp)
}

// humanBytes formats a size for a person.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func parseBoolFlag(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("expected true or false, got %q", v)
}
