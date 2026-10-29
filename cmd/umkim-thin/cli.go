package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rborso/umkim-thin/internal/app"
	"github.com/rborso/umkim-thin/internal/catalog"
	"github.com/rborso/umkim-thin/internal/identity"
	"github.com/rborso/umkim-thin/internal/update"
)

// This file is the CLI half of the console's non-install surfaces: the running
// version and its release check, the effective config, the aggregate status,
// the install preflight (plan/storage/validate), the media stack's settings,
// indexers and profiles, and the two one-click app actions. The console and the
// CLI call the same installer/directory code, so a feature that exists in one
// belongs in the other.

// ---------------------------------------------------------------------------
// Shared install flags
// ---------------------------------------------------------------------------

// installFlags are the options the console's install form exposes. install,
// plan, storage and validate all take them, so the CLI cannot drift from the
// console's form one flag at a time.
type installFlags struct {
	hosts          hostFlags
	exposures      hostFlags
	certificates   hostFlags
	extraLanguages listFlags
	pvcs           pvcFlags
	resources      resourceFlags

	configClass     *string
	mediaClass      *string
	mediaAccessMode *string
	namespace       *string
	version         *string
	language        *string
	ocrLanguage     *string
	mediaLanguage   *string
	qualityCutoff   *string
	allowLocal      *bool
}

// addInstallFlags registers the shared flags on fs.
func addInstallFlags(fs *flag.FlagSet) *installFlags {
	f := &installFlags{}
	f.configClass = fs.String("config-class", "", "RWO storage class (default: site's config class)")
	f.mediaClass = fs.String("media-class", "", "RWX storage class (default: site's media class)")
	f.mediaAccessMode = fs.String("media-access-mode", "", "media access mode: ReadWriteOnce|ReadWriteMany (default: site's mode)")
	f.namespace = fs.String("namespace", "", "namespace for a new install (default: catalog namespace)")
	f.version = fs.String("version", "", "catalogued chart version (default: stable)")
	f.language = fs.String("language", "", "app language (Nextcloud, Gitea, ...)")
	f.ocrLanguage = fs.String("ocr-language", "", "Paperless OCR language")
	f.mediaLanguage = fs.String("media-language", "", "media stack default language")
	f.qualityCutoff = fs.String("quality-cutoff", "", "media stack quality cutoff (e.g. 720p)")
	f.allowLocal = fs.Bool("allow-local", false, "also allow the site LAN on VPN routes")
	fs.Var(&f.hosts, "host", "route host; for multi-route apps use key=host (repeatable)")
	fs.Var(&f.exposures, "exposure", "public|authelia|vpn; multi-route key=value (repeatable)")
	fs.Var(&f.certificates, "certificate", "acme|default|secret:<name>; multi-route key=value")
	fs.Var(&f.pvcs, "pvc", "override a chart PVC: name=class:size (either part optional, repeatable)")
	fs.Var(&f.resources, "resource", "container resources: key=cpu=500m,memory=1Gi,... (repeatable)")
	fs.Var(&f.extraLanguages, "extra-language", "extra media language variant (repeatable)")
	return f
}

// options builds the installer options from the parsed flags.
func (f *installFlags) options(e catalog.Entry) app.Options {
	opts := app.Options{
		Hosts: f.hosts.m, ConfigClass: *f.configClass, MediaClass: *f.mediaClass,
		MediaAccessMode: *f.mediaAccessMode,
		Namespace:       *f.namespace, PVCs: f.pvcs.m, Resources: f.resources.m,
		Exposure: f.exposures.m, Certificate: f.certificates.m,
		ChartVersion: *f.version, Language: *f.language, OCRLanguage: *f.ocrLanguage,
		MediaLanguage: *f.mediaLanguage, QualityCutoff: *f.qualityCutoff,
		ExtraLanguages: f.extraLanguages.values(),
	}
	applyAllowLocal(e, &opts, *f.allowLocal)
	return opts
}

// pvcFlags parses name=class:size. Either part may be empty, so a size-only or
// class-only override is expressible without a second flag.
type pvcFlags struct{ m map[string]app.PVCOverride }

func (p *pvcFlags) String() string { return "" }

func (p *pvcFlags) Set(v string) error {
	name, spec, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("use name=class:size, e.g. data=fast-ssd:20Gi")
	}
	class, size, _ := strings.Cut(spec, ":")
	if p.m == nil {
		p.m = map[string]app.PVCOverride{}
	}
	p.m[strings.TrimSpace(name)] = app.PVCOverride{StorageClass: strings.TrimSpace(class), Size: strings.TrimSpace(size)}
	return nil
}

// resourceFlags parses key=cpu=500m,memory=1Gi. The key is the container plan
// key (kind/workload/container), which `app plan` prints.
type resourceFlags struct {
	m map[string]app.ContainerResources
}

func (r *resourceFlags) String() string { return "" }

func (r *resourceFlags) Set(v string) error {
	key, spec, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(spec) == "" {
		return fmt.Errorf("use key=cpu=500m,memory=1Gi (see `app plan`)")
	}
	var cr app.ContainerResources
	for _, part := range strings.Split(spec, ",") {
		k, val, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(val) == "" {
			return fmt.Errorf("invalid resource %q: use k=v pairs", part)
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "cpu":
			cr.CPURequest = strings.TrimSpace(val)
		case "cpulimit":
			cr.CPULimit = strings.TrimSpace(val)
		case "memory":
			cr.MemoryRequest = strings.TrimSpace(val)
		case "memorylimit":
			cr.MemoryLimit = strings.TrimSpace(val)
		default:
			return fmt.Errorf("unknown resource %q (cpu, cpuLimit, memory, memoryLimit)", k)
		}
	}
	if r.m == nil {
		r.m = map[string]app.ContainerResources{}
	}
	r.m[strings.TrimSpace(key)] = cr
	return nil
}

// listFlags is a repeatable string flag.
type listFlags []string

func (l *listFlags) String() string { return strings.Join(*l, ",") }

func (l *listFlags) Set(v string) error {
	*l = append(*l, strings.TrimSpace(v))
	return nil
}

func (l *listFlags) values() []string {
	if len(*l) == 0 {
		return nil
	}
	return append([]string(nil), *l...)
}

// ---------------------------------------------------------------------------
// version, config, status
// ---------------------------------------------------------------------------

// versionCmd prints the running version and, unless --offline, whether a newer
// release exists. It is the CLI half of the console's update chip.
func versionCmd(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	offline := fs.Bool("offline", false, "do not ask for the latest release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := app.ConfigFromEnv()
	st := update.Status{Current: cfg.Version, Enabled: false, Error: "update check skipped"}
	if !*offline {
		checker := update.New(update.Config{
			Current:  cfg.Version,
			Enabled:  true,
			Endpoint: envOr("UMKIM_THIN_UPDATE_CHECK_URL", update.DefaultEndpoint),
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = checker.Refresh(ctx)
		st = checker.Status()
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(st)
	}
	fmt.Printf("umkim-thin %s\n", choose(st.Current, "dev (no release tag)"))
	switch {
	case st.Available:
		fmt.Printf("  latest:  %s — update available\n", st.Latest)
		if st.URL != "" {
			fmt.Printf("  notes:   %s\n", st.URL)
		}
		fmt.Printf("  upgrade: sudo ./install.sh --reuse-k3s\n")
	case st.Latest != "":
		fmt.Printf("  latest:  %s — up to date\n", st.Latest)
	case *offline:
		fmt.Printf("  latest:  not checked (--offline)\n")
	case st.Error != "":
		fmt.Printf("  latest:  unknown (%s)\n", st.Error)
	}
	return nil
}

// configCmd prints the effective configuration the console shows: the hosts,
// storage defaults, backup schedule and trusted access.
func configCmd(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := app.ConfigFromEnv()
	in := app.New(cfg)
	classes, _ := in.StorageClasses()
	type classView struct {
		Name  string   `json:"name"`
		Modes []string `json:"accessModes,omitempty"`
	}
	view := struct {
		Version           string      `json:"version"`
		Zone              string      `json:"zone"`
		PortalHost        string      `json:"portalHost"`
		HomeHost          string      `json:"homeHost"`
		HomeURL           string      `json:"homeUrl"`
		ConfigClass       string      `json:"configClass"`
		MediaClass        string      `json:"mediaClass"`
		MediaAccessMode   string      `json:"mediaAccessMode"`
		StorageClasses    []classView `json:"storageClasses"`
		BackupDir         string      `json:"backupDir"`
		BackupKeep        int         `json:"backupKeep"`
		BackupInterval    string      `json:"backupInterval"`
		BackupSnapshots   bool        `json:"backupSnapshots"`
		UpdateCheck       bool        `json:"updateCheck"`
		TrustedAccess     bool        `json:"trustedAccessEnabled"`
		TrustedManagement bool        `json:"trustedManagementEnabled"`
		TrustedCIDRs      []string    `json:"trustedManagementCIDRs,omitempty"`
	}{
		Version: cfg.Version, Zone: cfg.Zone, PortalHost: cfg.PortalHost,
		HomeHost: cfg.HomeHost, HomeURL: cfg.HomeURL(),
		ConfigClass: cfg.ConfigClass, MediaClass: cfg.MediaClass,
		MediaAccessMode: cfg.MediaAccessMode,
		BackupDir:       cfg.BackupDir, BackupKeep: cfg.BackupKeep,
		BackupInterval: cfg.BackupInterval.String(), BackupSnapshots: cfg.BackupSnapshots,
		UpdateCheck:       envBool("UMKIM_THIN_UPDATE_CHECK", true),
		TrustedAccess:     cfg.TrustedAccessEnabled,
		TrustedManagement: cfg.TrustedManagementEnabled,
		TrustedCIDRs:      cfg.TrustedManagementCIDRs,
	}
	for _, c := range classes {
		view.StorageClasses = append(view.StorageClasses, classView{Name: c.Name, Modes: c.AccessModes})
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(view)
	}
	fmt.Printf("version        %s\n", choose(view.Version, "dev"))
	fmt.Printf("zone           %s\n", choose(view.Zone, "-"))
	if view.PortalHost != "" {
		fmt.Printf("portal         https://%s\n", view.PortalHost)
	} else {
		fmt.Printf("portal         -\n")
	}
	fmt.Printf("home           %s\n", choose(view.HomeURL, "-"))
	fmt.Printf("config class   %s\n", view.ConfigClass)
	fmt.Printf("media class    %s (%s)\n", view.MediaClass, view.MediaAccessMode)
	if len(view.StorageClasses) > 0 {
		names := make([]string, 0, len(view.StorageClasses))
		for _, c := range view.StorageClasses {
			names = append(names, c.Name)
		}
		fmt.Printf("classes        %s\n", strings.Join(names, ", "))
	}
	fmt.Printf("backups        %s every %s, keep %d, snapshots %t\n",
		view.BackupDir, choose(humanInterval(cfg.BackupInterval), "off"), view.BackupKeep, view.BackupSnapshots)
	fmt.Printf("update check   %t\n", view.UpdateCheck)
	fmt.Printf("trusted access %t (management %t)\n", view.TrustedAccess, view.TrustedManagement)
	return nil
}

// statusCmd is the aggregate the console's Overview shows: every installed
// catalog app with its Helm state, readiness and routes.
func statusCmd(dir string, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	entries, err := catalog.Load(dir)
	if err != nil {
		return err
	}
	byName := map[string]catalog.Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	in := app.New(app.ConfigFromEnv())
	installed, err := in.List()
	if err != nil {
		return err
	}
	type row struct {
		Name       string   `json:"name"`
		Namespace  string   `json:"namespace"`
		Chart      string   `json:"chart"`
		AppVersion string   `json:"appVersion,omitempty"`
		HelmStatus string   `json:"helmStatus,omitempty"`
		Ready      bool     `json:"ready"`
		Routes     []string `json:"routes,omitempty"`
		Latest     string   `json:"latestChart,omitempty"`
		Upgrade    bool     `json:"upgradeAvailable"`
	}
	var rows []row
	for _, item := range installed {
		e, ok := byName[item.Name]
		if !ok {
			continue // a platform release, not a catalog app
		}
		st, err := in.Status(item.Name, &e)
		if err != nil {
			return err
		}
		rows = append(rows, row{
			Name: item.Name, Namespace: st.Namespace, Chart: st.Chart,
			AppVersion: st.AppVersion, HelmStatus: st.HelmStatus, Ready: st.Ready,
			Routes: st.Routes, Latest: e.Chart.Version,
			Upgrade: updateAvailableCLI(st.Chart, e.Chart.Version),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("no catalog apps installed")
		return nil
	}
	fmt.Printf("%-14s %-16s %-12s %-10s %-8s %s\n", "APP", "NAMESPACE", "CHART", "STATUS", "READY", "ROUTES")
	for _, r := range rows {
		chart := r.Chart
		if r.Upgrade {
			chart += " -> " + r.Latest
		}
		fmt.Printf("%-14s %-16s %-12s %-10s %-8s %s\n",
			r.Name, r.Namespace, chart, choose(r.HelmStatus, "-"), readyWord(r.Ready), strings.Join(r.Routes, " "))
	}
	return nil
}

// updateAvailableCLI mirrors the API's comparison: a newer catalogued chart.
func updateAvailableCLI(installed, catalogued string) bool {
	if installed == "" || catalogued == "" {
		return false
	}
	return update.Compare(installed, catalogued) < 0
}

// ---------------------------------------------------------------------------
// app plan | storage | validate | setup | integrate
// ---------------------------------------------------------------------------

func appPlanCmd(e catalog.Entry, in *app.Installer, f *installFlags, asJSON bool) error {
	plan, err := in.RenderPlan(e, f.options(e))
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(plan)
	}
	fmt.Printf("%s: %d PVC(s), %d container(s)\n", e.Name, len(plan.PVCs), len(plan.Containers))
	if len(plan.PVCs) > 0 {
		fmt.Printf("\n%-28s %-14s %-10s %s\n", "PVC", "CLASS", "SIZE", "MODES")
		for _, p := range plan.PVCs {
			fmt.Printf("%-28s %-14s %-10s %s\n", p.Name, choose(p.StorageClass, "-"), choose(p.Size, "-"), strings.Join(p.AccessModes, ","))
		}
	}
	if len(plan.Containers) > 0 {
		fmt.Printf("\n%-42s %-10s %-12s %-12s %s\n", "CONTAINER KEY", "REPLICAS", "CPU", "MEMORY", "IMAGE")
		for _, c := range plan.Containers {
			cpu := c.CPURequest
			if c.CPULimit != "" {
				cpu += "/" + c.CPULimit
			}
			mem := c.MemoryRequest
			if c.MemoryLimit != "" {
				mem += "/" + c.MemoryLimit
			}
			fmt.Printf("%-42s %-10d %-12s %-12s %s\n", c.Key, c.Replicas, choose(cpu, "-"), choose(mem, "-"), c.Image)
		}
	}
	return nil
}

func appStorageCmd(e catalog.Entry, in *app.Installer, f *installFlags, asJSON bool) error {
	pvcs, err := in.StoragePlan(e, f.options(e))
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(pvcs)
	}
	if len(pvcs) == 0 {
		fmt.Printf("%s renders no PVCs\n", e.Name)
		return nil
	}
	fmt.Printf("%-28s %-14s %-10s %s\n", "PVC", "CLASS", "SIZE", "MODES")
	for _, p := range pvcs {
		fmt.Printf("%-28s %-14s %-10s %s\n", p.Name, choose(p.StorageClass, "-"), choose(p.Size, "-"), strings.Join(p.AccessModes, ","))
	}
	return nil
}

func appValidateCmd(e catalog.Entry, in *app.Installer, f *installFlags, asJSON bool) error {
	res, err := in.Validate(e, f.options(e))
	if err != nil {
		return err
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(res)
	} else {
		for _, w := range res.Warnings {
			fmt.Printf("warning [%s] %s\n", w.Scope, w.Message)
		}
		for _, e := range res.Errors {
			fmt.Printf("error   [%s] %s\n", e.Scope, e.Message)
		}
		if res.OK {
			fmt.Printf("%s: the install plan is valid (%d PVCs, %d containers)\n",
				e.Name, len(res.Plan.PVCs), len(res.Plan.Containers))
		}
	}
	if !res.OK {
		return fmt.Errorf("%s: the install plan has errors", e.Name)
	}
	return nil
}

func appSetupCmd(e catalog.Entry, in *app.Installer) error {
	if e.Name != app.ServarrApp {
		return fmt.Errorf("setup is not implemented for %s (only the media stack has one)", e.Name)
	}
	settings, err := in.ServarrSettings()
	if err != nil {
		return err
	}
	res, err := in.SetupServarr(e, settings, func(s string) { fmt.Printf("  %s\n", s) })
	if err != nil {
		return err
	}
	fmt.Printf("media stack setup complete: %d step(s)\n", len(res))
	return nil
}

func appIntegrateCmd(in *app.Installer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("app integrate needs an integration: mealie")
	}
	switch args[0] {
	case "mealie":
		res, err := in.ConnectMealieToSparky()
		if err != nil {
			return err
		}
		if res.AlreadyConnected {
			fmt.Printf("SparkyFitness already uses %s at %s\n", res.Provider, res.BaseURL)
			return nil
		}
		fmt.Printf("connected SparkyFitness to %s at %s\n", res.Provider, res.BaseURL)
		return nil
	default:
		return fmt.Errorf("unknown integration %q (available: mealie)", args[0])
	}
}

// ---------------------------------------------------------------------------
// servarr settings | indexers | profiles
// ---------------------------------------------------------------------------

// servarrCmd covers the media stack's settings, indexers and quality profiles.
// The console calls the same installer methods; `--namespace` selects a second
// release, as the console's namespace query does.
func servarrCmd(dir string, args []string) error {
	if wantsHelp(args) {
		fmt.Print(`usage: umkim-thin servarr <command> [--namespace NS]

  settings [--media-language L] [--quality-cutoff Q]
  indexers list | search <q> | add --definition D [--name N] [--field k=v]...
  profiles list | add --language L [--quality-cutoff Q] | delete --profile P
`)
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("servarr needs a subcommand: settings | indexers | profiles")
	}
	entries, err := catalog.Load(dir)
	if err != nil {
		return err
	}
	var entry catalog.Entry
	found := false
	for _, e := range entries {
		if e.Name == app.ServarrApp {
			entry, found = e, true
		}
	}
	if !found {
		return fmt.Errorf("the catalog has no %q entry", app.ServarrApp)
	}
	in := app.New(app.ConfigFromEnv())
	namespace := entry.Namespace
	switch args[0] {
	case "settings":
		fs := flag.NewFlagSet("servarr settings", flag.ContinueOnError)
		lang := fs.String("media-language", "", "default media language code (e.g. en, hu)")
		quality := fs.String("quality-cutoff", "", "quality cutoff (e.g. 720p, 1080p)")
		ns := fs.String("namespace", "", "servarr namespace (default: catalog namespace)")
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *ns != "" {
			namespace = *ns
		}
		current, err := in.ServarrSettingsFor(app.ServarrApp, namespace)
		if err != nil {
			return err
		}
		if *lang == "" && *quality == "" {
			if *asJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"mediaLanguage": current.MediaLanguage, "qualityCutoff": current.QualityCutoff,
					"languages": app.ServarrLanguageOptions(), "qualities": app.ServarrQualityOptions(),
				})
			}
			fmt.Printf("media language  %s\nquality cutoff  %s\n", current.MediaLanguage, current.QualityCutoff)
			fmt.Printf("\nlanguages: %s\n", optionValues(app.ServarrLanguageOptions()))
			fmt.Printf("qualities: %s\n", optionValues(app.ServarrQualityOptions()))
			return nil
		}
		next := current
		if *lang != "" {
			next.MediaLanguage = *lang
		}
		if *quality != "" {
			next.QualityCutoff = *quality
		}
		normalized, err := app.NormalizeServarrSettings(next)
		if err != nil {
			return err
		}
		res, err := in.SetupServarr(entry, normalized, func(s string) { fmt.Printf("  %s\n", s) })
		if err != nil {
			return err
		}
		fmt.Printf("applied %s/%s to the media stack (%d step(s))\n", normalized.MediaLanguage, normalized.QualityCutoff, len(res))
		return nil
	case "indexers":
		return servarrIndexersCmd(in, namespace, args[1:])
	case "profiles":
		return servarrProfilesCmd(in, namespace, args[1:])
	default:
		return fmt.Errorf("unknown servarr subcommand %q", args[0])
	}
}

func servarrIndexersCmd(in *app.Installer, namespace string, args []string) error {
	if wantsHelp(args) {
		fmt.Print("usage: umkim-thin servarr indexers list | search <q> | add --definition D [--name N] [--field k=v]... [--flaresolverr]\n")
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("servarr indexers needs a subcommand: list | search | add")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("servarr indexers list", flag.ContinueOnError)
		ns := fs.String("namespace", "", "servarr namespace")
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *ns != "" {
			namespace = *ns
		}
		indexers, err := in.ListProwlarrIndexers(namespace)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(indexers)
		}
		if len(indexers) == 0 {
			fmt.Println("no indexers")
			return nil
		}
		fmt.Printf("%-4s %-30s %-24s %-8s %s\n", "ID", "NAME", "IMPLEMENTATION", "PROTOCOL", "ENABLED")
		for _, i := range indexers {
			fmt.Printf("%-4d %-30s %-24s %-8s %t\n", i.ID, i.Name, i.Implementation, choose(i.Protocol, "-"), i.Enable)
		}
		return nil
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("servarr indexers search needs a query")
		}
		asJSON := false
		query := args[1]
		if len(args) > 2 && args[2] == "--json" {
			asJSON = true
		}
		defs, err := in.SearchProwlarrDefinitions(namespace, query)
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(defs)
		}
		for _, d := range defs {
			fmt.Printf("%-34s %-30s %s\n", d.DefinitionName, d.Name, d.ImplementationName)
		}
		return nil
	case "add":
		fs := flag.NewFlagSet("servarr indexers add", flag.ContinueOnError)
		def := fs.String("definition", "", "Prowlarr definition name (from `indexers search`)")
		name := fs.String("name", "", "indexer name (default: the definition name)")
		flaresolverr := fs.Bool("flaresolverr", false, "route the indexer through the FlareSolverr proxy")
		ns := fs.String("namespace", "", "servarr namespace")
		var fields hostFlags
		fs.Var(&fields, "field", "definition field: key=value (repeatable; credentials are not stored)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *def == "" {
			return fmt.Errorf("servarr indexers add needs --definition")
		}
		if *ns != "" {
			namespace = *ns
		}
		values := map[string]any{}
		for k, v := range fields.m {
			if k == "" {
				continue
			}
			values[k] = v
		}
		indexer, err := in.AddProwlarrIndexer(namespace, app.IndexerRequest{
			DefinitionName: *def, Name: *name, Fields: values, FlareSolverr: *flaresolverr,
		})
		if err != nil {
			return err
		}
		fmt.Printf("added %s (%s)\n", indexer.Name, indexer.Implementation)
		return nil
	default:
		return fmt.Errorf("unknown servarr indexers subcommand %q", args[0])
	}
}

func servarrProfilesCmd(in *app.Installer, namespace string, args []string) error {
	if wantsHelp(args) {
		fmt.Print("usage: umkim-thin servarr profiles list | add --language L [--quality-cutoff Q] | delete --profile P\n")
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("servarr profiles needs a subcommand: list | add | delete")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("servarr profiles list", flag.ContinueOnError)
		ns := fs.String("namespace", "", "servarr namespace")
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *ns != "" {
			namespace = *ns
		}
		profiles, err := in.ListServarrProfiles(namespace, app.ServarrApp)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(profiles)
		}
		if len(profiles) == 0 {
			fmt.Println("no media profiles")
			return nil
		}
		fmt.Printf("%-28s %-16s %-12s %s\n", "PROFILE", "LANGUAGE", "QUALITY", "DEFAULT")
		for _, p := range profiles {
			fmt.Printf("%-28s %-16s %-12s %t\n", p.Name, p.Language, p.Quality, p.Default)
		}
		return nil
	case "add":
		fs := flag.NewFlagSet("servarr profiles add", flag.ContinueOnError)
		lang := fs.String("language", "", "language code for the new profile")
		quality := fs.String("quality-cutoff", "", "quality cutoff (default: the stack's)")
		ns := fs.String("namespace", "", "servarr namespace")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *lang == "" {
			return fmt.Errorf("servarr profiles add needs --language")
		}
		if *ns != "" {
			namespace = *ns
		}
		settings, err := in.ServarrSettingsFor(app.ServarrApp, namespace)
		if err != nil {
			return err
		}
		cutoff := *quality
		if cutoff == "" {
			cutoff = settings.QualityCutoff
		}
		if err := in.EnsureServarrProfiles(namespace, app.ServarrApp, cutoff, settings.MediaLanguage,
			[]string{*lang}, func(s string) { fmt.Printf("  %s\n", s) }); err != nil {
			return err
		}
		fmt.Printf("added the %s profile at %s\n", *lang, cutoff)
		return nil
	case "delete":
		fs := flag.NewFlagSet("servarr profiles delete", flag.ContinueOnError)
		profile := fs.String("profile", "", "profile name (from `profiles list`)")
		ns := fs.String("namespace", "", "servarr namespace")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *profile == "" {
			return fmt.Errorf("servarr profiles delete needs --profile")
		}
		if *ns != "" {
			namespace = *ns
		}
		if err := in.DeleteServarrProfile(namespace, app.ServarrApp, *profile); err != nil {
			return err
		}
		fmt.Printf("deleted the %s profile\n", *profile)
		return nil
	default:
		return fmt.Errorf("unknown servarr profiles subcommand %q", args[0])
	}
}

func optionValues(opts []app.SettingOption) string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Value)
	}
	return strings.Join(out, ", ")
}

// ---------------------------------------------------------------------------
// invite groups | redeem
// ---------------------------------------------------------------------------

func inviteGroupsCmd(d *identity.Directory) error {
	groups, err := d.Groups()
	if err != nil {
		return err
	}
	for _, g := range groups {
		fmt.Println(g)
	}
	return nil
}

func inviteRedeemCmd(d *identity.Directory, args []string) error {
	fs := flag.NewFlagSet("invite redeem", flag.ContinueOnError)
	token := fs.String("token", "", "invitation token (the part after #token= in the link)")
	user := fs.String("username", "", "username to create")
	password := fs.String("password", "", "password for the new account")
	name := fs.String("name", "", "display name")
	email := fs.String("email", "", "email address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" || *user == "" || *password == "" {
		return fmt.Errorf("invite redeem needs --token, --username and --password")
	}
	if err := d.Redeem(*token, *user, *password, *name, *email); err != nil {
		return err
	}
	fmt.Printf("created %s from the invitation\n", *user)
	return nil
}

// humanInterval prints 24h rather than 24h0m0s.
func humanInterval(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dh", int(d/(24*time.Hour))*24)
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// atoiOrZero is used by the flag parsers that accept a count.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
