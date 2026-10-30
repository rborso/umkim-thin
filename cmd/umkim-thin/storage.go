package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rborso/umkim-thin/internal/storage"
)

const storageUsage = `storage — single-node disks, RAID, NFS exports and StorageClasses

Usage:
  umkim-thin storage disks
  umkim-thin storage list
  umkim-thin storage plan  --name NAME --disks /dev/disk/by-id/... [--raid none|0|1|5]
  umkim-thin storage apply --name NAME --disks ... --confirm FORMAT
  umkim-thin storage import-dir --name NAME --path /srv/existing
  umkim-thin storage plan  --name NAME --kind external --server NAS --export /media
  umkim-thin storage apply --name NAME --kind external --server NAS --export /media
  umkim-thin storage verify --server HOST --export PATH
  umkim-thin storage remove --name NAME
  umkim-thin storage-agent --socket /run/umkim-thin/storage.sock

Common flags:
  --clients CIDR[,CIDR]   export clients (default: site LAN, pod, VPN)
  --options OPTS          default rw,sync,no_subtree_check,no_root_squash
  --storage-class NAME    default nfs-<name>
  --mount PATH            disk mount (default /srv/nfs/<name>)
  --filesystem xfs
  --server HOST           external NFS server (kind=external)
  --export PATH           external NFS export path (kind=external)
  --mount-options OPTS    mount options (default nfsvers=4.2,rw,nolock)
`

type storageFlags struct {
	name, kind, raid, fs, mount, directory, path, options, class, confirm string
	server, export, mountOptions                                          string
	disks, clients                                                        stringList
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func (f *storageFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.name, "name", "", "export/array name")
	fs.StringVar(&f.kind, "kind", "disk", "disk, directory or external")
	fs.StringVar(&f.raid, "raid", "none", "none, 0, 1 or 5")
	fs.StringVar(&f.fs, "filesystem", "xfs", "filesystem (xfs)")
	fs.StringVar(&f.mount, "mount", "", "mountpoint (default /srv/nfs/<name>)")
	fs.StringVar(&f.directory, "directory", "", "existing directory for kind=directory")
	fs.StringVar(&f.path, "path", "", "existing directory for import-dir (alias of --directory)")
	fs.StringVar(&f.server, "server", "", "external NFS server (kind=external)")
	fs.StringVar(&f.export, "export", "", "external NFS export path (kind=external)")
	fs.StringVar(&f.mountOptions, "mount-options", "", "mount options for an external NFS export")
	fs.StringVar(&f.options, "options", "", "NFS export options")
	fs.StringVar(&f.class, "storage-class", "", "StorageClass name (default nfs-<name>)")
	fs.StringVar(&f.confirm, "confirm", "", "type FORMAT to allow formatting")
	fs.Var(&f.disks, "disk", "disk path or /dev/disk/by-id (repeatable)")
	fs.Var(&f.clients, "client", "client CIDR (repeatable)")
	fs.Var(&f.clients, "clients", "alias of --client (comma-separated)")
}

func (f *storageFlags) request() storage.Request {
	return storage.Request{
		Name: f.name, Kind: f.kind, Disks: f.disks, RAID: f.raid,
		Filesystem: f.fs, Mountpoint: f.mount, Directory: f.directory,
		Server: f.server, Export: f.export, MountOptions: f.mountOptions,
		Clients: f.clients, Options: f.options, StorageClass: f.class, Confirm: f.confirm,
	}
}

func storageCmd(args []string) error {
	if len(args) == 0 || wantsHelp(args) {
		fmt.Print(storageUsage)
		return nil
	}
	socket := os.Getenv("UMKIM_THIN_STORAGE_SOCKET")
	if socket == "" {
		socket = "/run/umkim-thin/storage.sock"
	}
	client := &storageAPI{socket: socket, http: unixHTTPClient(socket)}
	direct := func() *storage.Config {
		return storage.New(storage.Config{
			StateFile:  envOr("UMKIM_THIN_STORAGE_STATE", "/etc/umkim-thin/storage.yml"),
			Helm:       envOr("UMKIM_THIN_HELM", "/usr/local/bin/helm"),
			Kubectl:    envOr("UMKIM_THIN_KUBECTL", "/usr/local/bin/kubectl"),
			Kubeconfig: envOr("UMKIM_THIN_KUBECONFIG", "/etc/rancher/k3s/k3s.yaml"),
			NFSServer:  os.Getenv("UMKIM_THIN_NFS_SERVER"),
		})
	}
	switch args[0] {
	case "disks":
		if client.available() {
			var disks []storage.Disk
			if err := client.get("/disks", &disks); err != nil {
				return err
			}
			printDisks(disks)
			return nil
		}
		disks, err := direct().Discover()
		if err != nil {
			return err
		}
		printDisks(disks)
		return nil
	case "list":
		if client.available() {
			var records []storage.Record
			if err := client.get("/state", &records); err != nil {
				return err
			}
			printRecords(records)
			return nil
		}
		printRecords(direct().List())
		return nil
	case "plan", "apply":
		fs := flag.NewFlagSet("storage "+args[0], flag.ContinueOnError)
		var f storageFlags
		f.register(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if f.name == "" {
			return fmt.Errorf("--name is required")
		}
		if f.kind == "external" && f.export == "" {
			f.export = f.directory
		}
		if args[0] == "apply" && f.kind == "disk" && f.confirm != "FORMAT" {
			return fmt.Errorf("refusing to format without --confirm FORMAT")
		}
		req := f.request()
		var plan storage.Plan
		var err error
		// External exports are managed by the console (state on its PVC), not by
		// the host agent; the agent socket is for local disks and directories.
		if client.available() && req.Kind != "external" {
			err = client.post("/"+args[0], req, &plan)
		} else if args[0] == "plan" {
			plan, err = direct().Plan(req)
			if err == nil && req.Kind == "external" {
				// Same check as the console's Plan button: reachability and the
				// export list do not prove the export is mountable.
				var warnings []string
				warnings, err = direct().ProbeMount(plan.Request.Server, plan.Export, plan.MountOptions)
				plan.Warnings = append(plan.Warnings, warnings...)
				if err == nil {
					fmt.Printf("mount test: %s:%s is readable\n", plan.Request.Server, plan.Export)
				}
			}
		} else {
			plan, err = direct().Apply(req)
		}
		if err != nil {
			return err
		}
		printPlan(plan)
		return nil
	case "verify":
		fs := flag.NewFlagSet("storage verify", flag.ContinueOnError)
		var f storageFlags
		f.register(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if f.server == "" || f.export == "" {
			return fmt.Errorf("storage verify needs --server and --export")
		}
		opts := f.mountOptions
		if opts == "" {
			opts = "nfsvers=4.2,rw,nolock"
		}
		warnings, err := direct().ProbeMount(f.server, f.export, opts)
		for _, w := range warnings {
			fmt.Printf("warning: %s\n", w)
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s:%s mounted and is readable\n", f.server, f.export)
		return nil
	case "import-dir":
		fs := flag.NewFlagSet("storage import-dir", flag.ContinueOnError)
		var f storageFlags
		f.register(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		f.kind = "directory"
		if f.directory == "" {
			f.directory = f.path
		}
		if f.directory == "" {
			f.directory = f.mount
		}
		if f.name == "" || f.directory == "" {
			return fmt.Errorf("--name and --path/--directory are required")
		}
		req := f.request()
		var plan storage.Plan
		var err error
		if client.available() {
			err = client.post("/apply", req, &plan)
		} else {
			plan, err = direct().Apply(req)
		}
		if err != nil {
			return err
		}
		printPlan(plan)
		return nil
	case "remove":
		fs := flag.NewFlagSet("storage remove", flag.ContinueOnError)
		name := fs.String("name", "", "export name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("--name is required")
		}
		if client.available() {
			return client.post("/remove", map[string]string{"name": *name}, nil)
		}
		if err := direct().Remove(*name); err != nil {
			return err
		}
		fmt.Printf("removed export %s (data kept)\n", *name)
		return nil
	default:
		return fmt.Errorf("unknown storage subcommand %q", args[0])
	}
}

func printDisks(disks []storage.Disk) {
	fmt.Printf("%-24s %-12s %-8s %-5s %-4s %s\n", "PATH", "BY-ID", "SIZE", "ROTA", "OK", "REASON")
	for _, d := range disks {
		fmt.Printf("%-24s %-12s %-8d %-5t %-4t %s\n", d.Path, filepath.Base(d.ByID), d.Size, d.Rotational, d.Eligible, d.Reason)
	}
}

func printRecords(rs []storage.Record) {
	for _, r := range rs {
		fmt.Printf("%-10s %-8s %-14s %-20s %-16s %s\n", r.Name, r.Kind, r.StorageClass, r.Mountpoint, r.Device, r.Created)
	}
}

func printPlan(p storage.Plan) {
	fmt.Printf("plan %s (%s)\n", p.Request.Name, p.Request.Kind)
	fmt.Printf("  device:  %s\n  mount:   %s\n  export:  %s\n  class:   %s\n", p.Device, p.Mountpoint, p.Export, p.StorageClass)
	fmt.Printf("  clients: %s\n  options: %s\n", strings.Join(p.Clients, ", "), p.Options)
	if p.Request.Server != "" {
		fmt.Printf("  server:  %s\n  mount options: %s\n", p.Request.Server, p.MountOptions)
	}
	for _, w := range p.Warnings {
		fmt.Printf("  WARN: %s\n", w)
	}
	for _, s := range p.Steps {
		fmt.Printf("  - %s\n", s)
	}
}

// storageAPI is the console/CLI client for the host agent.
type storageAPI struct {
	socket string
	http   *http.Client
}

func (c *storageAPI) available() bool {
	if c.socket == "" {
		return false
	}
	_, err := os.Stat(c.socket)
	return err == nil
}

func (c *storageAPI) client() *http.Client {
	if c.http != nil {
		return c.http
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

func (c *storageAPI) get(path string, out any) error {
	resp, err := c.client().Get("http://unix" + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, out)
}

func (c *storageAPI) post(path string, body, out any) error {
	raw, _ := json.Marshal(body)
	resp, err := c.client().Post("http://unix"+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeResponse(resp, out)
}

func decodeResponse(resp *http.Response, out any) error {
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("storage agent: %s", strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func unixHTTPClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

// storageAgentCmd runs the root host agent. The console reaches it through a
// hostPath Unix socket; the CLI can use the same socket.
func storageAgentCmd(args []string) error {
	fs := flag.NewFlagSet("storage-agent", flag.ContinueOnError)
	socket := fs.String("socket", envOr("UMKIM_THIN_STORAGE_SOCKET", "/run/umkim-thin/storage.sock"), "Unix socket")
	stateFile := fs.String("state", envOr("UMKIM_THIN_STORAGE_STATE", "/etc/umkim-thin/storage.yml"), "state file")
	nfsServer := fs.String("nfs-server", os.Getenv("UMKIM_THIN_NFS_SERVER"), "NFS server address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sm := storage.New(storage.Config{
		StateFile: *stateFile, NFSServer: *nfsServer,
		Helm:       envOr("UMKIM_THIN_HELM", "/usr/local/bin/helm"),
		Kubectl:    envOr("UMKIM_THIN_KUBECTL", "/usr/local/bin/kubectl"),
		Kubeconfig: envOr("UMKIM_THIN_KUBECONFIG", "/etc/rancher/k3s/k3s.yaml"),
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /disks", func(w http.ResponseWriter, r *http.Request) {
		d, err := sm.Discover()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, d)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, sm.List()) })
	mux.HandleFunc("POST /plan", func(w http.ResponseWriter, r *http.Request) {
		var req storage.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		p, err := sm.Plan(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, p)
	})
	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, r *http.Request) {
		var req storage.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		p, err := sm.Apply(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, p)
	})
	mux.HandleFunc("POST /remove", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		if err := sm.Remove(req.Name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	dir := filepath.Dir(*socket)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	_ = os.Chown(dir, 0, 65532)
	_ = os.Chmod(dir, 0o750)
	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	_ = os.Chmod(*socket, 0o660)
	_ = os.Chown(*socket, 0, 65532)
	fmt.Printf("storage agent listening on %s\n", *socket)
	return http.Serve(ln, mux)
}
