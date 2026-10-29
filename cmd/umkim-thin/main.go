// Command umkim-thin is the single binary: the API, the console it serves, and
// the CLI. One process, one deployment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rborso/umkim-thin/internal/app"
	"github.com/rborso/umkim-thin/internal/backup"
	"github.com/rborso/umkim-thin/internal/catalog"
	"github.com/rborso/umkim-thin/internal/identity"
	"github.com/rborso/umkim-thin/internal/wg"
)

const usage = `umkim-thin — a self-hosted app platform for a cluster you already have

Usage:
  umkim-thin serve                     run the API and the console
  umkim-thin catalog list              list installable apps
  umkim-thin catalog show <name>       show one entry, with its verification
  umkim-thin app install <name>        install an app from the catalog
  umkim-thin app plan <name>           render the chart's PVCs and containers
  umkim-thin app storage <name>        render the chart's PVCs and classes
  umkim-thin app validate <name>       preflight the install options
  umkim-thin app setup <name>          run the media stack's bootstrap
  umkim-thin app integrate mealie      connect SparkyFitness to Mealie
  umkim-thin app versions <name>       show installed and available versions
  umkim-thin app upgrade <name> --to V upgrade to a catalogued chart version
  umkim-thin app reconcile <name>      republish routes without running Helm
  umkim-thin app list                  list installed apps
  umkim-thin app status <name>         show an app's workloads and routes
  umkim-thin app options <name>        show the values an app was installed with
  umkim-thin app uninstall <name>      remove an installed app (--purge for data)
  umkim-thin storage disks|plan|apply|list|remove  single-node disks/RAID/NFS
  umkim-thin storage-agent             host storage agent (root, single node)
  umkim-thin backup list|create|verify|restore|prune  console state + app inventory
  umkim-thin cert add/list/default/remove  manage manual TLS certificates
  umkim-thin invite create|list|revoke|groups|redeem  invitations and directory groups
  umkim-thin wg client create <name>   create a WireGuard client config
  umkim-thin servarr settings|indexers|profiles  the media stack's settings
  umkim-thin version                   running version and the latest release
  umkim-thin config                    effective configuration
  umkim-thin status                    every installed app, ready state and routes

Global flags:
  --catalog string   catalog directory (default "catalog")
`

func main() {
	catalogDir := "catalog"
	args := os.Args[1:]
	// Accept the global flag before the subcommand.
	for i, a := range args {
		if a == "--catalog" && i+1 < len(args) {
			catalogDir = args[i+1]
			args = append(args[:i], args[i+2:]...)
			break
		}
	}
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(2)
	}

	switch args[0] {
	case "serve":
		if err := serve(catalogDir, args[1:]); err != nil {
			fatal(err)
		}
	case "catalog":
		if err := catalogCmd(catalogDir, args[1:]); err != nil {
			fatal(err)
		}
	case "app":
		if err := appCmd(catalogDir, args[1:]); err != nil {
			fatal(err)
		}
	case "version":
		if err := versionCmd(args[1:]); err != nil {
			fatal(err)
		}
	case "config":
		if err := configCmd(args[1:]); err != nil {
			fatal(err)
		}
	case "status":
		if err := statusCmd(catalogDir, args[1:]); err != nil {
			fatal(err)
		}
	case "servarr":
		if err := servarrCmd(catalogDir, args[1:]); err != nil {
			fatal(err)
		}
	case "storage":
		if err := storageCmd(args[1:]); err != nil {
			fatal(err)
		}
	case "storage-agent":
		if err := storageAgentCmd(args[1:]); err != nil {
			fatal(err)
		}
	case "backup":
		if err := backupCmd(catalogDir, args[1:]); err != nil {
			fatal(err)
		}
	case "cert":
		if err := certCmd(args[1:]); err != nil {
			fatal(err)
		}
	case "invite":
		if err := inviteCmd(args[1:]); err != nil {
			fatal(err)
		}
	case "wg":
		if err := wgCmd(args[1:]); err != nil {
			fatal(err)
		}
	case "helm-postrender":
		if len(args) < 2 {
			fatal(fmt.Errorf("helm-postrender needs an overrides JSON file"))
		}
		if err := app.PostRender(args[1]); err != nil {
			fatal(err)
		}
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}

func appCmd(dir string, args []string) error {
	if wantsHelp(args) {
		fmt.Print(`usage: umkim-thin app <command> [flags]

  install <name>     install a catalog app (see app install --help)
  plan <name>        render the chart's PVCs and containers
  storage <name>     render the chart's PVCs and classes
  validate <name>    preflight the install options
  setup <name>       run the media stack's bootstrap
  integrate mealie   connect SparkyFitness to Mealie
  versions <name>    installed and available chart versions
  upgrade <name>     upgrade to a catalogued chart version (--to)
  reconcile <name>   republish routes without running Helm
  list               installed apps
  status <name>      one app's workloads and routes
  options <name>     the values an app was installed with
  uninstall <name>   remove an app (--purge for data)
`)
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("app needs a subcommand: install | plan | storage | validate | setup | integrate | versions | upgrade | reconcile | list | status | options | uninstall")
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

	switch args[0] {
	case "list":
		installed, err := in.List()
		if err != nil {
			return err
		}
		for _, r := range installed {
			fmt.Printf("%-14s %-16s %-10s %s\n", r.Name, r.Namespace, r.Chart, strings.Join(r.Routes, " "))
		}
		return nil
	case "install":
		if len(args) < 2 {
			return fmt.Errorf("app install needs a name")
		}
		e, ok := byName[args[1]]
		if !ok {
			return fmt.Errorf("no catalog entry named %q", args[1])
		}
		fs := flag.NewFlagSet("app install", flag.ContinueOnError)
		f := addInstallFlags(fs)
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		opts := f.options(e)
		fmt.Printf("installing %s (chart %s)...\n", e.Name, choose(opts.ChartVersion, e.Chart.Version))
		res, err := in.Install(e, opts)
		if err != nil {
			return err
		}
		fmt.Printf("installed %s in namespace %s\n", res.Name, res.Namespace)
		for _, h := range res.Routes {
			fmt.Printf("  https://%s\n", h)
		}
		return nil
	case "plan", "storage", "validate":
		// The name may come before or after the flags: Go's flag package stops
		// at the first positional, so take it out first.
		name, rest := splitName(args[1:])
		fs := flag.NewFlagSet("app "+args[0], flag.ContinueOnError)
		f := addInstallFlags(fs)
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if name == "" && fs.NArg() > 0 {
			name = fs.Arg(0)
		}
		if name == "" {
			return fmt.Errorf("app %s needs a name", args[0])
		}
		e, ok := byName[name]
		if !ok {
			return fmt.Errorf("no catalog entry named %q", name)
		}
		switch args[0] {
		case "plan":
			return appPlanCmd(e, in, f, *asJSON)
		case "storage":
			return appStorageCmd(e, in, f, *asJSON)
		default:
			return appValidateCmd(e, in, f, *asJSON)
		}
	case "setup":
		fs := flag.NewFlagSet("app setup", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("app setup needs a name")
		}
		e, ok := byName[fs.Arg(0)]
		if !ok {
			return fmt.Errorf("no catalog entry named %q", fs.Arg(0))
		}
		return appSetupCmd(e, in)
	case "integrate":
		return appIntegrateCmd(in, args[1:])
	case "reconcile":
		if len(args) < 2 {
			return fmt.Errorf("app reconcile needs a name")
		}
		e, ok := byName[args[1]]
		if !ok {
			return fmt.Errorf("no catalog entry named %q", args[1])
		}
		res, err := in.ReconcileRoutes(e, app.Options{})
		if err != nil {
			return err
		}
		fmt.Printf("reconciled routes for %s\n", res.Name)
		for _, h := range res.Routes {
			fmt.Printf("  https://%s\n", h)
		}
		return nil
	case "versions":
		if len(args) < 2 {
			return fmt.Errorf("app versions needs a name")
		}
		e, ok := byName[args[1]]
		if !ok {
			return fmt.Errorf("no catalog entry named %q", args[1])
		}
		vi, err := in.Versions(e)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", vi.Name)
		if vi.Installed != "" {
			fmt.Printf("  installed  %-12s (%s)\n", vi.Installed, vi.InstalledApp)
		} else {
			fmt.Printf("  installed  -\n")
		}
		for _, v := range vi.Versions {
			mark := " "
			if v.Version == vi.Installed {
				mark = "*"
			}
			fmt.Printf("  %s %-9s %-12s (%s)\n", mark, choose(v.Channel, "stable"), v.Version, v.AppVersion)
		}
		return nil
	case "upgrade":
		if len(args) < 2 {
			return fmt.Errorf("app upgrade needs a name")
		}
		e, ok := byName[args[1]]
		if !ok {
			return fmt.Errorf("no catalog entry named %q", args[1])
		}
		fs := flag.NewFlagSet("app upgrade", flag.ContinueOnError)
		to := fs.String("to", "", "target chart version (required)")
		yes := fs.Bool("yes", false, "confirm the pre-upgrade backup")
		skip := fs.Bool("skip-backup", false, "skip the backup confirmation (dangerous)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *to == "" {
			return fmt.Errorf("app upgrade needs --to <chart-version>")
		}
		if !*yes && !*skip {
			return fmt.Errorf("take a backup/VM snapshot first, then re-run with --yes (or --skip-backup)")
		}
		fmt.Printf("upgrading %s to chart %s...\n", e.Name, *to)
		res, err := in.Install(e, app.Options{
			ChartVersion: *to, Upgrade: true,
			ConfirmBackup: *yes, SkipBackup: *skip,
		})
		if err != nil {
			return err
		}
		fmt.Printf("upgraded %s to %s\n", res.Name, res.Chart)
		return nil
	case "uninstall":
		if len(args) < 2 {
			return fmt.Errorf("app uninstall needs a name")
		}
		fs := flag.NewFlagSet("app uninstall", flag.ContinueOnError)
		purge := fs.Bool("purge", false, "also delete the namespace, PVCs and the bind Secret")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		var entry *catalog.Entry
		if e, ok := byName[args[1]]; ok {
			entry = &e
		}
		if err := in.Uninstall(args[1], entry, *purge); err != nil {
			return err
		}
		if *purge {
			fmt.Printf("uninstalled %s (purged namespace)\n", args[1])
		} else {
			fmt.Printf("uninstalled %s (data kept; use --purge to remove it)\n", args[1])
		}
		return nil
	case "status":
		if len(args) < 2 {
			return fmt.Errorf("app status needs a name")
		}
		var entry *catalog.Entry
		if e, ok := byName[args[1]]; ok {
			entry = &e
		}
		st, err := in.Status(args[1], entry)
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s) — helm %s, %s\n", st.Name, st.Namespace, st.HelmStatus, readyWord(st.Ready))
		for _, c := range st.Components {
			fmt.Printf("  %-12s %-24s %s %s\n", c.Kind, c.Name, c.Ready, c.Status)
		}
		for _, h := range st.Routes {
			fmt.Printf("  route        https://%s\n", h)
		}
		return nil
	case "options":
		if len(args) < 2 {
			return fmt.Errorf("app options needs a name")
		}
		e, ok := byName[args[1]]
		if !ok {
			return fmt.Errorf("no catalog entry named %q", args[1])
		}
		fs := flag.NewFlagSet("app options", flag.ContinueOnError)
		namespace := fs.String("namespace", "", "installed namespace (default: recorded/catalog namespace)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		info, err := in.Release(e, *namespace)
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s) — %s %s, app %s, revision %d, %s\n",
			info.Name, info.Namespace, info.Chart, info.ChartVersion,
			info.AppVersion, info.Revision, info.Status)
		if info.Updated != "" {
			fmt.Printf("  updated      %s\n", info.Updated)
		}
		if len(info.Redacted) > 0 {
			fmt.Printf("  redacted     %s\n", strings.Join(info.Redacted, ", "))
		}
		raw, err := json.MarshalIndent(info.Values, "  ", "  ")
		if err != nil {
			return err
		}
		if len(info.Values) == 0 {
			fmt.Println("  values       none (chart defaults)")
		} else {
			fmt.Printf("  values\n  %s\n", raw)
		}
		return nil
	default:
		return fmt.Errorf("unknown app subcommand %q", args[0])
	}
}

func catalogCmd(dir string, args []string) error {
	if wantsHelp(args) {
		fmt.Print("usage: umkim-thin catalog list|show <name>\n")
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("catalog needs a subcommand: list | show")
	}
	entries, err := catalog.Load(dir)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		for _, e := range entries {
			auth := e.Auth.Kind
			fmt.Printf("%-12s %-22s %-14s %s\n", e.Name, e.DisplayName, auth, e.Chart.Version)
		}
		return nil
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("catalog show needs a name")
		}
		for _, e := range entries {
			if e.Name != args[1] {
				continue
			}
			fmt.Printf("%s (%s) — %s\n", e.DisplayName, e.Name, e.Description)
			fmt.Printf("  chart:    %s %s\n", e.Chart.Ref, e.Chart.Version)
			fmt.Printf("  auth:     %s\n", e.Auth.Kind)
			fmt.Printf("  license:  %s (chart %s)\n", e.License.Application, e.License.Chart)
			if e.License.URL != "" {
				fmt.Printf("            %s\n", e.License.URL)
			}
			if e.License.Note != "" {
				fmt.Printf("            %s\n", e.License.Note)
			}
			if e.Links.Homepage != "" {
				fmt.Printf("  homepage: %s\n", e.Links.Homepage)
			}
			if e.Links.Source != "" {
				fmt.Printf("  source:   %s\n", e.Links.Source)
			}
			for _, r := range e.EffectiveRoutes() {
				fmt.Printf("  route:    %s %s\n", r.Mode, r.Host)
				for _, p := range r.PublicPaths {
					fmt.Printf("            public: %s\n", p)
				}
			}
			fmt.Printf("  verify:   %s\n", e.Verify.Playwright)
			for _, c := range e.Verify.Checks {
				fmt.Printf("            %s\n", c)
			}
			return nil
		}
		return fmt.Errorf("no catalog entry named %q", args[1])
	default:
		return fmt.Errorf("unknown catalog subcommand %q", args[0])
	}
}

// hostFlags collects repeated --host flags: "host" for a single route, or
// "key=host" for multi-route apps.
type hostFlags struct{ m map[string]string }

func (h *hostFlags) String() string { return "" }

func (h *hostFlags) Set(v string) error {
	if h.m == nil {
		h.m = map[string]string{}
	}
	if k, val, ok := strings.Cut(v, "="); ok {
		h.m[k] = val
	} else {
		h.m[""] = v
	}
	return nil
}

func inviteCmd(args []string) error {
	if wantsHelp(args) {
		fmt.Print(`usage: umkim-thin invite <command>

  create --email E [--name N] [--group G]   issue an invitation
  list                                      pending and used invitations
  revoke <token>                            revoke an invitation
  groups                                    directory groups for --group
  redeem --token T --username U --password P  accept an invitation
`)
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("invite needs a subcommand: create | list | revoke | groups | redeem")
	}
	d := identity.New(identity.ConfigFromEnv())
	switch args[0] {
	case "list":
		invites, err := d.ListInvites()
		if err != nil {
			return err
		}
		for _, i := range invites {
			status := "expires " + i.Expires.Format("2006-01-02 15:04")
			switch {
			case i.Used:
				status = "used"
			case time.Now().After(i.Expires):
				status = "expired"
			}
			fmt.Printf("%-28s %-16s %-22s %s\n", i.Email, i.Group, status, i.Token[:8])
		}
		return nil
	case "create":
		fs := flag.NewFlagSet("invite create", flag.ContinueOnError)
		email := fs.String("email", "", "optional label for the invite (not required)")
		name := fs.String("name", "", "optional display name")
		group := fs.String("group", "", "directory group to add them to")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		inv, link, err := d.CreateInvite(*email, *name, *group)
		if err != nil {
			return err
		}
		if inv.Email != "" {
			fmt.Printf("invited %s to %s\n", inv.Email, inv.Group)
		} else {
			fmt.Printf("invitation for %s\n", inv.Group)
		}
		fmt.Printf("  link:    %s\n", link)
		fmt.Printf("  expires: %s\n", inv.Expires.Format(time.RFC3339))
		return nil
	case "groups":
		return inviteGroupsCmd(d)
	case "redeem":
		return inviteRedeemCmd(d, args[1:])
	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("invite revoke needs a token")
		}
		if err := d.Revoke(args[1]); err != nil {
			return err
		}
		fmt.Printf("revoked invitation %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown invite subcommand %q", args[0])
	}
}

func readyWord(ready bool) string {
	if ready {
		return "ready"
	}
	return "not ready"
}

func wgCmd(args []string) error {
	if wantsHelp(args) {
		fmt.Print("usage: umkim-thin wg client create <name> | list | delete <name>\n")
		return nil
	}
	if len(args) == 0 || args[0] != "client" {
		return fmt.Errorf("wg needs 'client': wg client create|list|delete")
	}
	if len(args) < 2 {
		return fmt.Errorf("wg client needs a subcommand: create | list | delete")
	}
	m := wg.New(wg.ConfigFromEnv())
	switch args[1] {
	case "list":
		peers, err := m.List()
		if err != nil {
			return err
		}
		for _, p := range peers {
			fmt.Printf("%-16s %-16s %s\n", p.Name, p.Address, p.PublicKey)
		}
		return nil
	case "create":
		if len(args) < 3 {
			return fmt.Errorf("wg client create needs a name")
		}
		c, err := m.Create(args[2])
		if err != nil {
			return err
		}
		fmt.Printf("created %s (%s)\n\n%s", c.Name, c.Address, c.Config)
		return nil
	case "delete":
		if len(args) < 3 {
			return fmt.Errorf("wg client delete needs a name")
		}
		if err := m.Delete(args[2]); err != nil {
			return err
		}
		fmt.Printf("revoked %s\n", args[2])
		return nil
	default:
		return fmt.Errorf("unknown wg client subcommand %q", args[1])
	}
}

func serve(dir string, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	homeAddr := fs.String("home-addr", ":8081", "user-facing home listen address (empty disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return serveAPI(dir, *addr, *homeAddr)
}

func fatal(err error) {
	// `--help` on a subcommand is a request, not a failure: flag.Parse prints
	// the usage and returns ErrHelp.
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// wantsHelp reports a top-level --help for a command group that has no flag
// set of its own (app, invite, servarr, ...). The subcommand help itself comes
// from flag.Parse, which prints the flags.
func wantsHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help")
}

func choose(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func applyAllowLocal(e catalog.Entry, opts *app.Options, allow bool) {
	if !allow {
		return
	}
	if opts.AllowLocal == nil {
		opts.AllowLocal = map[string]bool{}
	}
	for _, r := range e.EffectiveRoutes() {
		exp := ""
		if opts.Exposure != nil {
			exp = opts.Exposure[r.Key]
		}
		if exp == "" {
			exp = r.DefaultExposure()
		}
		if exp == "vpn" {
			opts.AllowLocal[r.Key] = true
		}
	}
}

// backupCmd is the CLI half of the console's Backups section: the same manager,
// the same directory. `restore --apps` is the disaster-recovery path: it puts
// the console state back and reinstalls the catalog apps that are missing.
func backupCmd(dir string, args []string) error {
	if wantsHelp(args) {
		fmt.Print(`usage: umkim-thin backup <command>

  list                       archives in the backup directory
  create                     write one now (state, seed, app inventory, snapshots)
  verify <name>              recompute every checksum
  restore <name> [--apps]    restore state (+ reinstall missing apps)
  prune [n]                  keep the newest n (default: the configured keep)
`)
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("backup needs a subcommand: list | create | verify | restore | prune")
	}
	cfg := app.ConfigFromEnv()
	m := backup.New(backup.Config{
		Dir:        cfg.BackupDir,
		StateDir:   cfg.StateDir,
		SeedDir:    cfg.BackupSeedDir,
		Helm:       cfg.Helm,
		Kubectl:    cfg.Kubectl,
		Kubeconfig: cfg.Kubeconfig,
		Version:    cfg.Version,
		Zone:       cfg.Zone,
		Keep:       cfg.BackupKeep,
		Snapshots:  cfg.BackupSnapshots,
	})
	switch args[0] {
	case "list":
		list, err := m.List()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Printf("no backups in %s\n", m.Dir())
			return nil
		}
		fmt.Printf("%-38s %-8s %-9s %-6s %s\n", "NAME", "VERSION", "APPS", "FILES", "SNAPSHOTS")
		for _, a := range list {
			fmt.Printf("%-38s %-8s %-9d %-6d %d\n", a.Name, a.Version, a.Apps, a.Files, a.Snapshots)
		}
		return nil
	case "create":
		a, err := m.Create(context.Background(), "cli")
		if err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d files, %d snapshots)\n", a.Name, a.Files, a.Snapshots)
		for _, w := range a.Warnings {
			fmt.Printf("  warning: %s\n", w)
		}
		return nil
	case "verify":
		if len(args) < 2 {
			return fmt.Errorf("backup verify needs an archive name")
		}
		a, err := m.Verify(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("%s: ok (%d files, %d snapshots)\n", a.Name, a.Files, a.Snapshots)
		return nil
	case "restore":
		if len(args) < 2 {
			return fmt.Errorf("backup restore needs an archive name")
		}
		fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
		apps := fs.Bool("apps", false, "also reinstall catalog apps that are missing")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		mf, err := m.Manifest(args[1])
		if err != nil {
			return err
		}
		rep, err := m.Restore(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("restored %d files; safety archive %s\n", len(rep.Restored), rep.SafetyArchive)
		if len(rep.Snapshots) > 0 {
			fmt.Printf("application data snapshots (create a PVC from them before reinstalling):\n")
			for _, s := range rep.Snapshots {
				if s.Snapshot == "" {
					continue
				}
				fmt.Printf("  %s/%s -> %s\n", s.Namespace, s.PVC, s.Snapshot)
			}
		}
		if !*apps {
			return nil
		}
		entries, err := catalog.Load(dir)
		if err != nil {
			return err
		}
		byName := map[string]catalog.Entry{}
		for _, e := range entries {
			byName[e.Name] = e
		}
		in := app.New(cfg)
		installed, err := in.List()
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, a := range installed {
			have[a.Name] = true
		}
		for _, rec := range mf.Apps {
			e, ok := byName[rec.Name]
			if !ok || have[rec.Name] {
				continue
			}
			fmt.Printf("reinstalling %s (chart %s)...\n", rec.Name, rec.ChartVersion)
			res, err := in.Install(e, app.Options{ChartVersion: rec.ChartVersion, ConfirmBackup: true})
			if err != nil {
				return fmt.Errorf("reinstalling %s: %w", rec.Name, err)
			}
			fmt.Printf("  %s is back at chart %s\n", res.Name, res.Chart)
		}
		return nil
	case "prune":
		keep := cfg.BackupKeep
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 0 {
				return fmt.Errorf("keep count must be a non-negative integer")
			}
			keep = n
		}
		removed, err := m.Prune(keep)
		if err != nil {
			return err
		}
		fmt.Printf("kept %d, removed %d\n", keep, len(removed))
		return nil
	default:
		return fmt.Errorf("unknown backup subcommand %q", args[0])
	}
}
