// Command umkim-thin is the single binary: the API, the console it serves, and
// the CLI. One process, one deployment.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rborso/umkim-thin/internal/app"
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
  umkim-thin app versions <name>       show installed and available versions
  umkim-thin app upgrade <name> --to V upgrade to a catalogued chart version
  umkim-thin app list                  list installed apps
  umkim-thin app status <name>         show an app's workloads and routes
  umkim-thin app uninstall <name>      remove an installed app (--purge for data)
  umkim-thin storage disks|plan|apply|list|remove  single-node disks/RAID/NFS
  umkim-thin storage-agent             host storage agent (root, single node)
  umkim-thin cert add/list/default/remove  manage manual TLS certificates
  umkim-thin invite create --email E   invite someone to the directory
  umkim-thin wg client create <name>   create a WireGuard client config

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
	case "storage":
		if err := storageCmd(args[1:]); err != nil {
			fatal(err)
		}
	case "storage-agent":
		if err := storageAgentCmd(args[1:]); err != nil {
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
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}

func appCmd(dir string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("app needs a subcommand: install | versions | upgrade | list | status | uninstall")
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
		configClass := fs.String("config-class", "", "RWO storage class (default: site's config class)")
		mediaClass := fs.String("media-class", "", "RWX storage class (default: site's media class)")
		version := fs.String("version", "", "catalogued chart version (default: stable)")
		allowLocal := fs.Bool("allow-local", false, "also allow the site LAN on VPN routes")
		var hosts hostFlags
		fs.Var(&hosts, "host", "route host; for multi-route apps use key=host (repeatable)")
		var exposures, certificates hostFlags
		fs.Var(&exposures, "exposure", "public|authelia|vpn; multi-route key=value (repeatable)")
		fs.Var(&certificates, "certificate", "acme|default|secret:<name>; multi-route key=value")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		opts := app.Options{Hosts: hosts.m, ConfigClass: *configClass, MediaClass: *mediaClass,
			Exposure: exposures.m, Certificate: certificates.m, ChartVersion: *version}
		applyAllowLocal(e, &opts, *allowLocal)
		fmt.Printf("installing %s (chart %s)...\n", e.Name, choose(*version, e.Chart.Version))
		res, err := in.Install(e, opts)
		if err != nil {
			return err
		}
		fmt.Printf("installed %s in namespace %s\n", res.Name, res.Namespace)
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
	default:
		return fmt.Errorf("unknown app subcommand %q", args[0])
	}
}

func catalogCmd(dir string, args []string) error {
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
	if len(args) == 0 {
		return fmt.Errorf("invite needs a subcommand: create | list")
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
	if len(args) == 0 || args[0] != "client" {
		return fmt.Errorf("wg needs 'client': wg client create <name> | wg client list")
	}
	if len(args) < 2 {
		return fmt.Errorf("wg client needs a subcommand: create | list")
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	return serveAPI(dir, *addr)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
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
