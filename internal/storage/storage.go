// Package storage plans and applies NFS-backed StorageClasses. Local host
// exports use blank disks or existing directories, optional mdadm RAID and XFS;
// external exports point the provisioner at an existing server and path.
// Planning is pure and safe. Local disk Apply is destructive and is guarded by
// an explicit FORMAT confirmation plus a re-check of the selected disks.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// Disk is one physical disk from lsblk, with eligibility for formatting.
type Disk struct {
	Path        string   `json:"path"`
	ByID        string   `json:"byID,omitempty"`
	Name        string   `json:"name"`
	Model       string   `json:"model,omitempty"`
	Serial      string   `json:"serial,omitempty"`
	Size        uint64   `json:"size"`
	Rotational  bool     `json:"rotational"`
	FSType      string   `json:"fstype,omitempty"`
	Mountpoints []string `json:"mountpoints,omitempty"`
	Children    int      `json:"children,omitempty"`
	Eligible    bool     `json:"eligible"`
	Reason      string   `json:"reason,omitempty"`
}

// Request is one array/export operation.
type Request struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"` // disk | directory | external
	Disks      []string `json:"disks,omitempty"`
	RAID       string   `json:"raid,omitempty"` // none | 0 | 1 | 5
	Filesystem string   `json:"filesystem,omitempty"`
	Mountpoint string   `json:"mountpoint,omitempty"`
	Directory  string   `json:"directory,omitempty"`
	// Server and Export describe an existing external NFS server. They are
	// ignored for the host-local disk/directory kinds.
	Server       string   `json:"server,omitempty"`
	Export       string   `json:"export,omitempty"`
	MountOptions string   `json:"mountOptions,omitempty"`
	Clients      []string `json:"clients,omitempty"`
	Options      string   `json:"options,omitempty"`
	StorageClass string   `json:"storageClass,omitempty"`
	Confirm      string   `json:"confirm,omitempty"`
}

// Plan is the validated, human-reviewable description of an operation.
type Plan struct {
	Request      Request  `json:"request"`
	Device       string   `json:"device,omitempty"`
	Mountpoint   string   `json:"mountpoint"`
	Export       string   `json:"export"`
	StorageClass string   `json:"storageClass"`
	MountOptions string   `json:"mountOptions,omitempty"`
	Clients      []string `json:"clients"`
	Options      string   `json:"options"`
	Steps        []string `json:"steps"`
	Warnings     []string `json:"warnings,omitempty"`
}

// Record is persisted in the configured state file.
type Record struct {
	Name         string   `yaml:"name" json:"name"`
	Kind         string   `yaml:"kind" json:"kind"`
	Device       string   `yaml:"device,omitempty" json:"device,omitempty"`
	RAID         string   `yaml:"raid,omitempty" json:"raid,omitempty"`
	Mountpoint   string   `yaml:"mountpoint" json:"mountpoint"`
	Server       string   `yaml:"server,omitempty" json:"server,omitempty"`
	Export       string   `yaml:"export" json:"export"`
	MountOptions string   `yaml:"mountOptions,omitempty" json:"mountOptions,omitempty"`
	Clients      []string `yaml:"clients" json:"clients"`
	Options      string   `yaml:"options" json:"options"`
	StorageClass string   `yaml:"storageClass" json:"storageClass"`
	Created      string   `yaml:"created" json:"created"`
}

type state struct {
	Arrays []Record `yaml:"arrays"`
}

// Config is the host-side context.
type Config struct {
	StateFile      string
	Helm           string
	Kubectl        string
	Kubeconfig     string
	NFSServer      string
	DefaultClients []string
	NFSChartRepo   string
	// command runner is a test seam.
	run func(name string, args ...string) (string, error)
	// external NFS probe is a test seam.
	checkExternal func(server, export string) ([]string, error)
}

// New returns a storage manager.
func New(cfg Config) *Config {
	if cfg.StateFile == "" {
		cfg.StateFile = "/etc/umkim-thin/storage.yml"
	}
	if cfg.Helm == "" {
		cfg.Helm = "/usr/local/bin/helm"
	}
	if cfg.Kubectl == "" {
		cfg.Kubectl = "/usr/local/bin/kubectl"
	}
	if cfg.Kubeconfig == "" {
		cfg.Kubeconfig = "/etc/rancher/k3s/k3s.yaml"
	}
	if len(cfg.DefaultClients) == 0 {
		cfg.DefaultClients = []string{"192.168.1.0/24", "10.42.0.0/16", "192.168.168.0/24"}
	}
	if cfg.NFSChartRepo == "" {
		cfg.NFSChartRepo = "https://kubernetes-sigs.github.io/nfs-subdir-external-provisioner/"
	}
	return &cfg
}

func (c *Config) command(name string, args ...string) (string, error) {
	if c.run != nil {
		return c.run(name, args...)
	}
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// Discover lists disks and marks only blank, unused, non-root disks eligible.
func (c *Config) Discover() ([]Disk, error) {
	out, err := c.command("lsblk", "-J", "-b", "-o",
		"NAME,PATH,TYPE,SIZE,MODEL,SERIAL,ROTA,MOUNTPOINTS,FSTYPE,PARTTYPE,PKNAME")
	if err != nil {
		return nil, err
	}
	var ls struct {
		Blockdevices []struct {
			Name        string   `json:"name"`
			Path        string   `json:"path"`
			Type        string   `json:"type"`
			Size        uint64   `json:"size"`
			Model       string   `json:"model"`
			Serial      string   `json:"serial"`
			ROTA        bool     `json:"rota"`
			Mountpoints []string `json:"mountpoints"`
			FSType      string   `json:"fstype"`
			PARTType    string   `json:"parttype"`
			Children    []struct {
				Name        string   `json:"name"`
				Path        string   `json:"path"`
				Type        string   `json:"type"`
				Mountpoints []string `json:"mountpoints"`
				FSType      string   `json:"fstype"`
			} `json:"children"`
		} `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &ls); err != nil {
		return nil, fmt.Errorf("parsing lsblk: %w", err)
	}
	rootSource, _ := c.command("findmnt", "-n", "-o", "SOURCE", "/")
	rootSource = strings.TrimSpace(rootSource)
	rootDisk := rootDiskFor(rootSource)
	byID := byIDMap()

	var disks []Disk
	for _, d := range ls.Blockdevices {
		if d.Type != "disk" {
			continue
		}
		mounts := cleanStrings(d.Mountpoints)
		children := 0
		for _, ch := range d.Children {
			if ch.Type != "" {
				children++
			}
			mounts = append(mounts, cleanStrings(ch.Mountpoints)...)
			if ch.FSType != "" && d.FSType == "" {
				d.FSType = ch.FSType
			}
		}
		mounts = cleanStrings(mounts)
		disk := Disk{
			Path: d.Path, Name: d.Name, Model: d.Model, Serial: d.Serial,
			Size: d.Size, Rotational: d.ROTA, FSType: d.FSType, Mountpoints: mounts,
			Children: children, ByID: byID[d.Name],
		}
		if out, err := c.command("mdadm", "--examine", d.Path); err == nil && strings.Contains(out, "Array UUID") {
			disk.Reason = "contains mdadm RAID metadata"
		}
		switch {
		case disk.Reason != "":
		case d.Name == rootDisk:
			disk.Reason = "contains the running root filesystem"
		case len(mounts) > 0:
			disk.Reason = "has a mounted filesystem: " + strings.Join(mounts, ", ")
		case d.FSType != "":
			disk.Reason = "has an existing filesystem (" + d.FSType + ")"
		case children > 0:
			disk.Reason = "has partitions or child devices"
		default:
			disk.Eligible = true
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

func rootDiskFor(source string) string {
	if source == "" {
		return ""
	}
	p, err := filepath.EvalSymlinks(source)
	if err != nil {
		p = source
	}
	base := filepath.Base(p)
	// /dev/sda2 -> sda, /dev/nvme0n1p2 -> nvme0n1
	switch {
	case strings.HasPrefix(base, "nvme"):
		if i := strings.Index(base, "p"); i > 0 {
			return base[:i]
		}
	case strings.HasPrefix(base, "mmcblk"):
		if i := strings.Index(base, "p"); i > 0 {
			return base[:i]
		}
	case strings.HasPrefix(base, "vd"), strings.HasPrefix(base, "sd"):
		return strings.TrimRight(base, "0123456789")
	}
	return base
}

func byIDMap() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir("/dev/disk/by-id")
	if err != nil {
		return out
	}
	for _, e := range entries {
		target, err := filepath.EvalSymlinks(filepath.Join("/dev/disk/by-id", e.Name()))
		if err != nil {
			continue
		}
		name := filepath.Base(target)
		if _, ok := out[name]; !ok {
			out[name] = "/dev/disk/by-id/" + e.Name()
		}
	}
	return out
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && v != "null" {
			out = append(out, v)
		}
	}
	return out
}

// CheckExternal verifies that the NFS server/export is reachable from the
// console. The export listing is best-effort: showmount is not available in
// every image and NFSv4 pseudo-roots do not always enumerate.
func (c *Config) CheckExternal(server, export string) ([]string, error) {
	if c.checkExternal != nil {
		return c.checkExternal(server, export)
	}
	addr := net.JoinHostPort(server, "2049")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("NFS server %s is not reachable on TCP/2049: %w", server, err)
	}
	_ = conn.Close()

	showmount, err := exec.LookPath("showmount")
	if err != nil {
		return []string{"NFS TCP/2049 is reachable; showmount is unavailable, so the export path was not verified"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, showmount, "-e", server).CombinedOutput()
	if err != nil {
		return []string{"NFS TCP/2049 is reachable; showmount failed, so the export path was not verified: " + strings.TrimSpace(string(out))}, nil
	}
	if !strings.Contains(string(out), export) {
		return nil, fmt.Errorf("NFS server %s does not advertise export %s", server, export)
	}
	return nil, nil
}

// Plan validates a request and returns the exact steps.
func (c *Config) Plan(req Request) (Plan, error) {
	if !nameRE.MatchString(req.Name) {
		return Plan{}, fmt.Errorf("name must be 2-31 lowercase letters, digits or hyphens")
	}
	if req.Kind != "disk" && req.Kind != "directory" && req.Kind != "external" {
		return Plan{}, fmt.Errorf("kind must be disk, directory or external")
	}
	if req.StorageClass == "" {
		req.StorageClass = "nfs-" + req.Name
	}
	if !nameRE.MatchString(req.StorageClass) {
		return Plan{}, fmt.Errorf("invalid StorageClass name")
	}

	if req.Kind == "external" {
		req.Server = strings.TrimSpace(req.Server)
		if !validNFSServer(req.Server) {
			return Plan{}, fmt.Errorf("server must be an NFS hostname or IP address")
		}
		rawExport := strings.TrimSpace(req.Export)
		if rawExport == "" || strings.Contains(rawExport, "..") {
			return Plan{}, fmt.Errorf("export must be an absolute path without '..'")
		}
		export := filepath.Clean(rawExport)
		if !filepath.IsAbs(export) || export == "/" {
			return Plan{}, fmt.Errorf("export must be an absolute path other than /")
		}
		if strings.ContainsAny(export, " \t\r\n,") {
			return Plan{}, fmt.Errorf("export must not contain spaces or commas")
		}
		req.Export = export
		if req.MountOptions == "" {
			req.MountOptions = "nfsvers=4.2,rw,nolock"
		}
		if err := validMountOptions(req.MountOptions); err != nil {
			return Plan{}, err
		}
		warnings, err := c.CheckExternal(req.Server, export)
		if err != nil {
			return Plan{}, err
		}
		return Plan{
			Request:      req,
			Mountpoint:   req.Server + ":" + export,
			Export:       export,
			StorageClass: req.StorageClass,
			MountOptions: req.MountOptions,
			Warnings:     warnings,
			Steps: []string{
				"verify external NFS " + req.Server + ":" + export,
				"install StorageClass " + req.StorageClass,
			},
		}, nil
	}

	if len(req.Clients) == 0 {
		req.Clients = append([]string(nil), c.DefaultClients...)
	}
	if len(req.Clients) == 0 {
		return Plan{}, fmt.Errorf("at least one client CIDR is required")
	}
	for _, cl := range req.Clients {
		if cl == "*" || cl == "0.0.0.0/0" {
			return Plan{}, fmt.Errorf("refusing to export to the whole internet")
		}
	}
	if req.Options == "" {
		req.Options = "rw,sync,no_subtree_check,no_root_squash"
	}
	if err := validOptions(req.Options); err != nil {
		return Plan{}, err
	}
	req.Filesystem = strings.ToLower(req.Filesystem)
	if req.Filesystem == "" {
		req.Filesystem = "xfs"
	}
	if req.Filesystem != "xfs" {
		return Plan{}, fmt.Errorf("only xfs is supported")
	}
	p := Plan{Request: req, Clients: req.Clients, Options: req.Options, StorageClass: req.StorageClass}
	switch req.Kind {
	case "directory":
		abs, err := filepath.Abs(req.Directory)
		if err != nil || abs == "/" {
			return Plan{}, fmt.Errorf("directory must be an absolute non-root path")
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return Plan{}, fmt.Errorf("directory %s does not exist or is not a directory", abs)
		}
		p.Mountpoint = abs
		p.Export = abs
		p.Steps = []string{
			"verify directory " + abs,
			"write /etc/exports.d/umkim-" + req.Name + ".exports",
			"exportfs -rav",
			"install StorageClass " + req.StorageClass,
		}
	case "disk":
		// Plan is a read-only preview and deliberately does not require the
		// destructive confirmation; Apply checks it before touching a disk.
		if len(req.Disks) == 0 {
			return Plan{}, fmt.Errorf("at least one disk is required")
		}
		if err := validRAID(req.RAID, len(req.Disks)); err != nil {
			return Plan{}, err
		}
		disks, err := c.Discover()
		if err != nil {
			return Plan{}, err
		}
		idx := map[string]Disk{}
		for _, d := range disks {
			idx[d.Path] = d
			if d.ByID != "" {
				idx[d.ByID] = d
			}
		}
		var chosen []string
		var warnings []string
		for _, path := range req.Disks {
			d, ok := idx[path]
			if !ok {
				return Plan{}, fmt.Errorf("disk %s not found", path)
			}
			if !d.Eligible {
				return Plan{}, fmt.Errorf("disk %s is not blank: %s", path, d.Reason)
			}
			if d.Rotational {
				warnings = append(warnings, path+" is a rotational disk")
			}
			chosen = append(chosen, path)
		}
		dev := chosen[0]
		if req.RAID != "none" {
			dev = "/dev/md/" + req.Name
		}
		if req.RAID == "0" {
			warnings = append(warnings, "RAID0 has no redundancy")
		}
		mount := req.Mountpoint
		if mount == "" {
			mount = "/srv/nfs/" + req.Name
		}
		if !strings.HasPrefix(filepath.Clean(mount), "/srv/nfs/") {
			return Plan{}, fmt.Errorf("mountpoint must be under /srv/nfs/")
		}
		p.Device = dev
		p.Mountpoint = mount
		p.Export = mount
		p.Warnings = warnings
		if req.RAID == "none" {
			p.Steps = append(p.Steps, "mkfs.xfs -f "+dev)
		} else {
			p.Steps = append(p.Steps, "mdadm --create "+dev+" --level="+req.RAID+" --raid-devices="+fmt.Sprint(len(chosen))+" "+strings.Join(chosen, " "))
			p.Steps = append(p.Steps, "wait for mdadm resync")
			p.Steps = append(p.Steps, "mkfs.xfs -f "+dev)
		}
		p.Steps = append(p.Steps,
			"mount "+dev+" "+mount,
			"persist UUID in /etc/fstab",
			"write /etc/mdadm.conf",
			"write /etc/exports.d/umkim-"+req.Name+".exports",
			"exportfs -rav",
			"install StorageClass "+req.StorageClass,
		)
	}
	return p, nil
}

func validNFSServer(server string) bool {
	if server == "" || len(server) > 253 {
		return false
	}
	if net.ParseIP(server) != nil {
		return true
	}
	if strings.ContainsAny(server, " \t\r\n/\\,:") || strings.Contains(server, "..") {
		return false
	}
	for _, label := range strings.Split(server, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func validMountOptions(opts string) error {
	allowed := map[string]bool{
		"rw": true, "ro": true, "sync": true, "async": true,
		"hard": true, "soft": true, "lock": true, "nolock": true,
		"noatime": true, "relatime": true,
	}
	prefixes := []string{"nfsvers=", "vers=", "port=", "mountport=", "timeo=", "retrans=", "rsize=", "wsize=", "actimeo="}
	valueRE := regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	for _, o := range strings.Split(opts, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if allowed[o] {
			continue
		}
		ok := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(o, prefix) && valueRE.MatchString(strings.TrimPrefix(o, prefix)) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("unsupported NFS mount option %q", o)
		}
	}
	return nil
}

func validRAID(level string, n int) error {
	switch level {
	case "none":
		if n != 1 {
			return fmt.Errorf("RAID none needs exactly one disk")
		}
	case "0", "1":
		if n < 2 {
			return fmt.Errorf("RAID %s needs at least two disks", level)
		}
	case "5":
		if n < 3 {
			return fmt.Errorf("RAID5 needs at least three disks")
		}
	default:
		return fmt.Errorf("RAID must be none, 0, 1 or 5")
	}
	return nil
}

func validOptions(opts string) error {
	allowed := map[string]bool{
		"rw": true, "ro": true, "sync": true, "async": true, "no_subtree_check": true,
		"subtree_check": true, "root_squash": true, "no_root_squash": true, "secure": true,
	}
	for _, o := range strings.Split(opts, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if !allowed[o] {
			return fmt.Errorf("unsupported NFS export option %q", o)
		}
	}
	return nil
}

// Apply re-plans and performs the operation. It must run as root on the host.
func (c *Config) Apply(req Request) (Plan, error) {
	if req.Kind == "disk" && req.Confirm != "FORMAT" {
		return Plan{}, fmt.Errorf("set confirm=FORMAT to allow disk formatting")
	}
	plan, err := c.Plan(req)
	if err != nil {
		return Plan{}, err
	}
	if req.Kind == "external" {
		releaseExists := c.releaseExists(req.Name)
		if err := c.checkExternalClass(plan, releaseExists); err != nil {
			return Plan{}, err
		}
		if err := c.installProvisioner(plan); err != nil {
			if !releaseExists {
				_, _ = c.command(c.Helm, "uninstall", "nfs-"+req.Name,
					"--namespace", "nfs-provisioner", "--kubeconfig", c.Kubeconfig)
			}
			return Plan{}, err
		}
		if err := c.saveRecord(plan); err != nil {
			return Plan{}, err
		}
		return plan, nil
	}
	if req.Kind == "disk" {
		// Re-check immediately before formatting: the plan could be stale.
		disks, err := c.Discover()
		if err != nil {
			return Plan{}, err
		}
		idx := map[string]Disk{}
		for _, d := range disks {
			idx[d.Path], idx[d.ByID] = d, d
		}
		for _, path := range req.Disks {
			d, ok := idx[path]
			if !ok || !d.Eligible {
				return Plan{}, fmt.Errorf("disk %s changed since the plan; refusing", path)
			}
		}
	}
	if req.Kind == "disk" {
		dev := plan.Device
		if req.RAID != "none" {
			if _, err := os.Stat(dev); err != nil {
				args := []string{"--create", dev, "--level=" + req.RAID,
					"--raid-devices=" + fmt.Sprint(len(req.Disks)), "--run"}
				args = append(args, req.Disks...)
				if _, err := c.command("mdadm", args...); err != nil {
					return Plan{}, err
				}
			}
			if _, err := c.command("mdadm", "--wait", dev); err != nil {
				return Plan{}, err
			}
		}
		if _, err := c.command("mkfs.xfs", "-f", dev); err != nil {
			return Plan{}, err
		}
		if err := os.MkdirAll(plan.Mountpoint, 0o755); err != nil {
			return Plan{}, err
		}
		if _, err := c.command("mount", dev, plan.Mountpoint); err != nil {
			return Plan{}, err
		}
		uuid, err := c.command("blkid", "-s", "UUID", "-o", "value", dev)
		if err != nil {
			return Plan{}, err
		}
		if err := appendFstab(strings.TrimSpace(uuid), plan.Mountpoint); err != nil {
			return Plan{}, err
		}
		if req.RAID != "none" {
			if err := saveMdadmConf(c); err != nil {
				return Plan{}, err
			}
		}
	}
	if err := writeExport(plan); err != nil {
		return Plan{}, err
	}
	if _, err := c.command("systemctl", "enable", "--now", "nfs-server"); err != nil {
		return Plan{}, err
	}
	if _, err := c.command("exportfs", "-rav"); err != nil {
		return Plan{}, err
	}
	if err := c.installProvisioner(plan); err != nil {
		return Plan{}, err
	}
	if err := c.saveRecord(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (c *Config) releaseExists(name string) bool {
	_, err := c.command(c.Helm, "status", "nfs-"+name, "--namespace", "nfs-provisioner",
		"--kubeconfig", c.Kubeconfig)
	return err == nil
}

func (c *Config) checkExternalClass(p Plan, releaseExists bool) error {
	if releaseExists {
		return nil
	}
	_, err := c.command(c.Kubectl, "--kubeconfig", c.Kubeconfig,
		"get", "storageclass", p.StorageClass, "-o", "name")
	if err != nil {
		// Not found (or the API is unavailable); let Helm report the real error.
		return nil
	}
	return fmt.Errorf("StorageClass %s already exists; remove the old NFS release/class or choose another name", p.StorageClass)
}

func (c *Config) installProvisioner(p Plan) error {
	// A local repository name is enough; Helm caches it after the first add.
	if out, err := c.command(c.Helm, "repo", "add", "nfs-subdir-external-provisioner", c.NFSChartRepo, "--force-update"); err != nil {
		return fmt.Errorf("helm repo add nfs-subdir-external-provisioner: %w: %s", err, strings.TrimSpace(out))
	}
	if out, err := c.command(c.Helm, "repo", "update", "nfs-subdir-external-provisioner"); err != nil {
		return fmt.Errorf("helm repo update nfs-subdir-external-provisioner: %w: %s", err, strings.TrimSpace(out))
	}
	server := p.Request.Server
	if server == "" {
		server = c.NFSServer
	}
	if server == "" {
		if out, err := c.command("hostname", "-I"); err == nil {
			server = strings.Fields(out)[0]
		}
	}
	if server == "" {
		return fmt.Errorf("cannot determine the NFS server address; set NFSServer")
	}
	mountOptions := p.MountOptions
	if mountOptions == "" {
		mountOptions = "nfsvers=4.2,rw,nolock"
	}
	args := []string{"upgrade", "--install", "nfs-" + p.Request.Name,
		"nfs-subdir-external-provisioner/nfs-subdir-external-provisioner",
		"--namespace", "nfs-provisioner", "--create-namespace",
		"--kubeconfig", c.Kubeconfig,
		"--set", "nfs.server=" + server,
		"--set", "nfs.path=" + p.Export,
		"--set", "nfs.mountOptions={" + mountOptions + "}",
		"--set", "storageClass.name=" + p.StorageClass,
		"--set", "storageClass.defaultClass=false",
		"--set", "storageClass.accessModes=ReadWriteMany",
		"--set", "storageClass.reclaimPolicy=Retain",
		"--wait", "--timeout", "5m"}
	out, err := c.command(c.Helm, args...)
	if err != nil {
		return fmt.Errorf("helm upgrade --install nfs-%s: %w: %s", p.Request.Name, err, strings.TrimSpace(out))
	}
	return nil
}

func writeExport(p Plan) error {
	path := "/etc/exports.d/umkim-" + p.Request.Name + ".exports"
	if err := os.MkdirAll("/etc/exports.d", 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, cl := range p.Clients {
		fmt.Fprintf(&b, "%s %s(%s)\n", p.Export, cl, p.Options)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func appendFstab(uuid, mount string) error {
	if uuid == "" {
		return fmt.Errorf("empty filesystem UUID")
	}
	raw, _ := os.ReadFile("/etc/fstab")
	if strings.Contains(string(raw), " "+mount+" ") {
		return nil
	}
	line := fmt.Sprintf("UUID=%s %s xfs defaults,noatime 0 2\n", uuid, mount)
	f, err := os.OpenFile("/etc/fstab", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("# umkim-thin storage\n" + line)
	return err
}

func saveMdadmConf(c *Config) error {
	out, err := c.command("mdadm", "--detail", "--scan")
	if err != nil {
		return err
	}
	current, _ := os.ReadFile("/etc/mdadm.conf")
	if strings.Contains(string(current), out) {
		return nil
	}
	f, err := os.OpenFile("/etc/mdadm.conf", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n# umkim-thin storage\n" + out)
	return err
}

func (c *Config) saveRecord(p Plan) error {
	s := c.loadState()
	for i := range s.Arrays {
		if s.Arrays[i].Name == p.Request.Name {
			s.Arrays = append(s.Arrays[:i], s.Arrays[i+1:]...)
			break
		}
	}
	s.Arrays = append(s.Arrays, Record{
		Name: p.Request.Name, Kind: p.Request.Kind, Device: p.Device,
		RAID: p.Request.RAID, Mountpoint: p.Mountpoint, Server: p.Request.Server,
		Export: p.Export, MountOptions: p.MountOptions, Clients: p.Clients,
		Options: p.Options, StorageClass: p.StorageClass,
		Created: time.Now().UTC().Format(time.RFC3339),
	})
	return c.writeState(s)
}

func (c *Config) loadState() state {
	var s state
	raw, err := os.ReadFile(c.StateFile)
	if err == nil {
		_ = yaml.Unmarshal(raw, &s)
	}
	return s
}

func (c *Config) writeState(s state) error {
	if err := os.MkdirAll(filepath.Dir(c.StateFile), 0o700); err != nil {
		return err
	}
	raw, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(c.StateFile, raw, 0o600)
}

// List returns the recorded exports.
func (c *Config) List() []Record { return c.loadState().Arrays }

// Remove unexports and removes the StorageClass/release; it never deletes data.
func (c *Config) Remove(name string) error {
	s := c.loadState()
	var keep []Record
	var found *Record
	for i := range s.Arrays {
		if s.Arrays[i].Name == name {
			found = &s.Arrays[i]
			continue
		}
		keep = append(keep, s.Arrays[i])
	}
	if found == nil {
		return fmt.Errorf("no storage export named %s", name)
	}
	if found.Kind != "external" {
		_ = os.Remove("/etc/exports.d/umkim-" + name + ".exports")
		_, _ = c.command("exportfs", "-ra")
	}
	_, _ = c.command(c.Helm, "uninstall", "nfs-"+name, "--namespace", "nfs-provisioner",
		"--kubeconfig", c.Kubeconfig)
	s.Arrays = keep
	return c.writeState(s)
}

// Fingerprint is a short, non-secret identifier used in the console plan.
func Fingerprint(p Plan) string {
	h := sha256.Sum256([]byte(p.Request.Name + p.Device + p.Mountpoint))
	return hex.EncodeToString(h[:4])
}
