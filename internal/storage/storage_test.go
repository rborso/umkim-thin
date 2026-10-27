package storage

import (
	"os"
	"strings"
	"testing"
)

const lsblkFixture = `{
  "blockdevices": [
    {"name":"sda","path":"/dev/sda","type":"disk","size":100000000000,"model":"OS","rota":false,"mountpoints":[null],"fstype":null,
      "children":[{"name":"sda2","path":"/dev/sda2","type":"part","mountpoints":["/"],"fstype":"xfs"}]},
    {"name":"sdb","path":"/dev/sdb","type":"disk","size":500000000000,"model":"SSD","rota":false,"mountpoints":[null],"fstype":null},
    {"name":"sdc","path":"/dev/sdc","type":"disk","size":1000000000000,"model":"HDD","rota":true,"mountpoints":["/mnt/old"],"fstype":"ext4"},
    {"name":"nvme0n1","path":"/dev/nvme0n1","type":"disk","size":2000000000000,"model":"NVMe","rota":false,"mountpoints":[null],"fstype":null}
  ]
}`

func fakeConfig() *Config {
	c := New(Config{StateFile: "/tmp/umkim-storage-test.yml"})
	c.checkExternal = func(server, export string) ([]string, error) { return nil, nil }
	c.run = func(name string, args ...string) (string, error) {
		switch name {
		case "lsblk":
			return lsblkFixture, nil
		case "findmnt":
			return "/dev/sda2\n", nil
		default:
			return "", nil
		}
	}
	return c
}

func TestDiscoverEligibility(t *testing.T) {
	c := fakeConfig()
	disks, err := c.Discover()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Disk{}
	for _, d := range disks {
		byPath[d.Path] = d
	}
	if byPath["/dev/sda"].Eligible {
		t.Fatal("the root disk was marked eligible")
	}
	if !byPath["/dev/sdb"].Eligible {
		t.Fatalf("blank disk sdb not eligible: %+v", byPath["/dev/sdb"])
	}
	if !byPath["/dev/nvme0n1"].Eligible {
		t.Fatalf("blank nvme not eligible: %+v", byPath["/dev/nvme0n1"])
	}
	if byPath["/dev/sdc"].Eligible {
		t.Fatalf("mounted disk sdc was marked eligible: %+v", byPath["/dev/sdc"])
	}
}

func TestPlanDisk(t *testing.T) {
	c := fakeConfig()
	p, err := c.Plan(Request{
		Name: "media", Kind: "disk", Disks: []string{"/dev/sdb"},
		RAID: "none", Filesystem: "xfs", Confirm: "FORMAT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Device != "/dev/sdb" || p.Mountpoint != "/srv/nfs/media" || p.StorageClass != "nfs-media" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestPlanPreviewDoesNotNeedConfirm(t *testing.T) {
	c := fakeConfig()
	p, err := c.Plan(Request{Name: "media", Kind: "disk", Disks: []string{"/dev/sdb"}, RAID: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Device != "/dev/sdb" {
		t.Fatalf("plan = %+v", p)
	}
	if _, err := c.Apply(Request{Name: "media", Kind: "disk", Disks: []string{"/dev/sdb"}, RAID: "none"}); err == nil || !strings.Contains(err.Error(), "confirm=FORMAT") {
		t.Fatalf("Apply() without confirmation = %v, want confirm=FORMAT", err)
	}
}

func TestPlanRejectsUnsafe(t *testing.T) {
	c := fakeConfig()
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{"root disk", Request{Name: "aa", Kind: "disk", Disks: []string{"/dev/sda"}, RAID: "none", Confirm: "FORMAT"}, "not blank"},
		{"raid5 two disks", Request{Name: "aa", Kind: "disk", Disks: []string{"/dev/sdb", "/dev/nvme0n1"}, RAID: "5", Confirm: "FORMAT"}, "at least three"},
		{"whole internet", Request{Name: "aa", Kind: "directory", Directory: t.TempDir(), Clients: []string{"0.0.0.0/0"}}, "whole internet"},
		{"bad option", Request{Name: "aa", Kind: "directory", Directory: t.TempDir(), Options: "rw,insecure"}, "unsupported"},
		{"bad mount", Request{Name: "aa", Kind: "disk", Disks: []string{"/dev/sdb"}, RAID: "none", Mountpoint: "/tmp/x", Confirm: "FORMAT"}, "under /srv/nfs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Plan(tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Plan() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPlanDirectory(t *testing.T) {
	dir := t.TempDir()
	c := fakeConfig()
	p, err := c.Plan(Request{Name: "archive", Kind: "directory", Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	if p.Export != dir || p.Mountpoint != dir || len(p.Steps) == 0 {
		t.Fatalf("directory plan = %+v", p)
	}
}

func TestRootDiskFor(t *testing.T) {
	for in, want := range map[string]string{
		"/dev/sda2": "sda", "/dev/nvme0n1p2": "nvme0n1", "/dev/vda1": "vda",
	} {
		if got := rootDiskFor(in); got != want {
			t.Errorf("rootDiskFor(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	f, err := os.CreateTemp("", "storage-*.yml")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())
	c := New(Config{StateFile: f.Name()})
	s := state{Arrays: []Record{{Name: "media", Kind: "disk", Mountpoint: "/srv/nfs/media", Export: "/srv/nfs/media"}}}
	if err := c.writeState(s); err != nil {
		t.Fatal(err)
	}
	if got := c.List(); len(got) != 1 || got[0].Name != "media" {
		t.Fatalf("list = %+v", got)
	}
}

func TestPlanExternal(t *testing.T) {
	c := fakeConfig()
	p, err := c.Plan(Request{
		Name: "nas", Kind: "external", Server: "192.168.1.20",
		Export: "/volume1/media", MountOptions: "nfsvers=4.2,rw,nolock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.StorageClass != "nfs-nas" || p.Mountpoint != "192.168.1.20:/volume1/media" || p.Export != "/volume1/media" {
		t.Fatalf("external plan = %+v", p)
	}
	if len(p.Steps) != 2 || p.Request.Server != "192.168.1.20" {
		t.Fatalf("external plan steps = %+v", p)
	}
}

func TestPlanExternalDefaults(t *testing.T) {
	c := fakeConfig()
	p, err := c.Plan(Request{Name: "nas", Kind: "external", Server: "nas.lan", Export: "/media/"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Export != "/media" || p.MountOptions != "nfsvers=4.2,rw,nolock" {
		t.Fatalf("defaults = %+v", p)
	}
}

func TestPlanExternalChecksAccess(t *testing.T) {
	c := fakeConfig()
	c.checkExternal = func(server, export string) ([]string, error) {
		if server != "nas.lan" || export != "/media" {
			t.Fatalf("checked %s:%s", server, export)
		}
		return []string{"export not enumerated"}, nil
	}
	p, err := c.Plan(Request{Name: "nas", Kind: "external", Server: "nas.lan", Export: "/media"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 1 || p.Warnings[0] != "export not enumerated" {
		t.Fatalf("warnings = %+v", p.Warnings)
	}

	c.checkExternal = func(string, string) ([]string, error) {
		return nil, os.ErrPermission
	}
	if _, err := c.Plan(Request{Name: "nas", Kind: "external", Server: "nas.lan", Export: "/media"}); err == nil {
		t.Fatal("unreachable export was accepted")
	}
}

func TestPlanRejectsUnsafeExternal(t *testing.T) {
	c := fakeConfig()
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{"relative export", Request{Name: "aa", Kind: "external", Server: "nas.lan", Export: "media"}, "absolute"},
		{"root export", Request{Name: "aa", Kind: "external", Server: "nas.lan", Export: "/"}, "other than /"},
		{"dotdot", Request{Name: "aa", Kind: "external", Server: "nas.lan", Export: "/srv/../etc"}, "without '..'"},
		{"space in export", Request{Name: "aa", Kind: "external", Server: "nas.lan", Export: "/srv/my media"}, "spaces"},
		{"bad server", Request{Name: "aa", Kind: "external", Server: "nas lan", Export: "/media"}, "hostname or IP"},
		{"server path", Request{Name: "aa", Kind: "external", Server: "nas.lan/share", Export: "/media"}, "hostname or IP"},
		{"bad mount option", Request{Name: "aa", Kind: "external", Server: "nas.lan", Export: "/media", MountOptions: "rw,exec"}, "unsupported"},
		{"brace in mount option", Request{Name: "aa", Kind: "external", Server: "nas.lan", Export: "/media", MountOptions: "rw,port=2049}"}, "unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Plan(tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Plan() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestApplyExternalDoesNotTouchHost(t *testing.T) {
	f, err := os.CreateTemp("", "storage-external-*.yml")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())
	c := New(Config{StateFile: f.Name()})
	c.checkExternal = func(server, export string) ([]string, error) { return nil, nil }
	var calls []string
	c.run = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}
	p, err := c.Apply(Request{Name: "nas", Kind: "external", Server: "nas.lan", Export: "/media"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Request.Name != "nas" {
		t.Fatalf("plan = %+v", p)
	}
	joined := strings.Join(calls, "\n")
	for _, call := range calls {
		if strings.HasPrefix(call, "mkfs") || strings.HasPrefix(call, "exportfs") || strings.HasPrefix(call, "systemctl") || strings.HasPrefix(call, "hostname") {
			t.Fatalf("external apply touched the host: %v", calls)
		}
	}
	for _, want := range []string{"nfs.server=nas.lan", "nfs.path=/media", "nfs.mountOptions={nfsvers=4.2,rw,nolock}", "storageClass.name=nfs-nas"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("external apply args missing %q: %v", want, calls)
		}
	}
	records := c.List()
	if len(records) != 1 || records[0].Kind != "external" || records[0].Server != "nas.lan" {
		t.Fatalf("records = %+v", records)
	}
}
