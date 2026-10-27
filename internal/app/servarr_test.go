package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStorageClasses(t *testing.T) {
	out := `{"items":[
		{"metadata":{"name":"nfs","annotations":{}},"provisioner":"nfs.csi.k8s.io"},
		{"metadata":{"name":"longhorn-rwo","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"driver.longhorn.io"},
		{"metadata":{"name":"old","annotations":{"storageclass.beta.kubernetes.io/is-default-class":"true"}},"provisioner":"kubernetes.io/no-provisioner"}
	]}`
	classes, err := parseStorageClasses(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(classes) != 3 {
		t.Fatalf("got %d classes", len(classes))
	}
	if classes[0].Name != "longhorn-rwo" || !classes[0].Default {
		t.Fatalf("default class not parsed: %+v", classes[0])
	}
	if classes[1].Name != "nfs" || classes[1].Default {
		t.Fatalf("unexpected class: %+v", classes[1])
	}
	if !classes[2].Default {
		t.Fatalf("beta default annotation ignored: %+v", classes[2])
	}
}

func TestAccessModeHints(t *testing.T) {
	cases := []struct {
		name, provisioner, want string
	}{
		{"nfs", "nfs.csi.k8s.io", "RWX"},
		{"nfs-client", "cluster.local/nfs-subdir-external-provisioner", "RWX"},
		{"longhorn-rwx", "driver.longhorn.io", "RWX"},
		{"longhorn-rwo", "driver.longhorn.io", "RWO"},
		{"local-path", "rancher.io/local-path", "RWO"},
		{"weird", "example.com/thing", ""},
	}
	for _, c := range cases {
		got := strings.Join(knownAccessModes(c.name, c.provisioner), ",")
		if got != c.want {
			t.Errorf("%s/%s: got %q, want %q", c.name, c.provisioner, got, c.want)
		}
	}
}

func TestAccessModeMap(t *testing.T) {
	got := accessModeMap(" media-nfs = RWX+RWO , other=ReadWriteOnce , broken")
	if strings.Join(got["media-nfs"], ",") != "RWO,RWX" {
		t.Errorf("media-nfs: %v", got["media-nfs"])
	}
	if strings.Join(got["other"], ",") != "ReadWriteOnce" {
		t.Errorf("other: %v", got["other"])
	}
	if _, ok := got["broken"]; ok {
		t.Errorf("a valueless entry should be ignored: %v", got)
	}
}

func TestParsePVAccessModes(t *testing.T) {
	out := `{"items":[
		{"spec":{"storageClassName":"nfs","accessModes":["ReadWriteMany"]}},
		{"spec":{"storageClassName":"nfs","accessModes":["ReadWriteMany"]}},
		{"spec":{"storageClassName":"longhorn-rwo","accessModes":["ReadWriteOnce"]}},
		{"spec":{"storageClassName":"","accessModes":["ReadWriteOnce"]}}
	]}`
	got := parsePVAccessModes(out)
	if strings.Join(got["nfs"], ",") != "ReadWriteMany" {
		t.Errorf("nfs: %v", got["nfs"])
	}
	if strings.Join(got["longhorn-rwo"], ",") != "ReadWriteOnce" {
		t.Errorf("longhorn-rwo: %v", got["longhorn-rwo"])
	}
	if len(got) != 2 {
		t.Errorf("unexpected classes: %v", got)
	}
}

func TestHashDirChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := hashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("hashDir is not deterministic")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := hashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("hashDir ignored a content change")
	}
}

func TestNormalizeServarrSettings(t *testing.T) {
	cases := []struct {
		name    string
		in      ServarrSettings
		want    ServarrSettings
		wantErr bool
	}{
		{
			name: "defaults",
			in:   ServarrSettings{},
			want: ServarrSettings{MediaLanguage: "en", QualityCutoff: "720p"},
		},
		{
			name: "language and numeric quality",
			in:   ServarrSettings{MediaLanguage: "EN", QualityCutoff: "1080"},
			want: ServarrSettings{MediaLanguage: "en", QualityCutoff: "1080p"},
		},
		{
			name: "regional language code",
			in:   ServarrSettings{MediaLanguage: "pt-BR", QualityCutoff: "2160p"},
			want: ServarrSettings{MediaLanguage: "pt", QualityCutoff: "2160p"},
		},
		{
			name:    "unknown language",
			in:      ServarrSettings{MediaLanguage: "xx", QualityCutoff: "720p"},
			wantErr: true,
		},
		{
			name:    "unknown quality",
			in:      ServarrSettings{MediaLanguage: "hu", QualityCutoff: "8k"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeServarrSettings(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBootstrapImageTag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("0.2.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := New(Config{BootstrapContext: dir, BootstrapImage: "localhost/umkim-thin/servarr-bootstrap", GitSHA: "abc123def456"})
	tag, err := in.BootstrapImageTag()
	if err != nil {
		t.Fatal(err)
	}
	want := "localhost/umkim-thin/servarr-bootstrap:0.2.23-abc123def456"
	if tag != want {
		t.Fatalf("got %q, want %q", tag, want)
	}

	// Without a baked commit the tag falls back to the content hash.
	in.cfg.GitSHA = ""
	tag, err = in.BootstrapImageTag()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tag, "localhost/umkim-thin/servarr-bootstrap:0.2.23-") || len(tag) <= len(want)-len("abc123def456") {
		t.Fatalf("unexpected fallback tag %q", tag)
	}
}
