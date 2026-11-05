package versions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedIsValid(t *testing.T) {
	f, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if f.Firecracker.Version == "" || f.GuestKernel.Version == "" || f.BaseRootfs.Version == "" {
		t.Fatalf("incomplete pins: %+v", f)
	}
	fc, k, err := f.ForArch("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.Archive, f.Firecracker.Version) {
		t.Errorf("archive %q does not carry version %s", fc.Archive, f.Firecracker.Version)
	}
	if !strings.HasSuffix(k.URL, "vmlinux-"+f.GuestKernel.Version) {
		t.Errorf("kernel url %q does not match version %s", k.URL, f.GuestKernel.Version)
	}
}

func TestArchiveNaming(t *testing.T) {
	f, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	fc := f.Firecracker.Artifacts["x86_64"]
	want := "https://github.com/firecracker-microvm/firecracker/releases/download/" + f.Firecracker.Version + "/" + fc.Archive
	if got := f.ArchiveURL(fc); got != want {
		t.Errorf("ArchiveURL = %s, want %s", got, want)
	}
	v := f.Firecracker.Version
	if got, want := f.ArchiveMember("x86_64", "jailer"), "release-"+v+"-x86_64/jailer-"+v+"-x86_64"; got != want {
		t.Errorf("ArchiveMember = %s, want %s", got, want)
	}
}

func TestForArchUnpinned(t *testing.T) {
	f, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.ForArch("aarch64")
	if err == nil || !strings.Contains(err.Error(), "pinned: x86_64") {
		t.Errorf("ForArch(aarch64) error = %v, want one naming the pinned arches", err)
	}
}

func TestArch(t *testing.T) {
	for goarch, want := range map[string]string{"amd64": "x86_64", "arm64": "aarch64"} {
		if got, err := Arch(goarch); err != nil || got != want {
			t.Errorf("Arch(%s) = %s, %v", goarch, got, err)
		}
	}
	if _, err := Arch("riscv64"); err == nil {
		t.Error("want an error for riscv64")
	}
}

const valid = `
schema: 1
firecracker:
  version: v1.0.0
  release_url: https://example.com/r
  artifacts:
    x86_64:
      archive: fc.tgz
      sha256: 0000000000000000000000000000000000000000000000000000000000000000
      binaries:
        firecracker: 1111111111111111111111111111111111111111111111111111111111111111
        jailer: 2222222222222222222222222222222222222222222222222222222222222222
guest_kernel:
  version: 6.18.1
  artifacts:
    x86_64:
      url: https://example.com/vmlinux
      sha256: 3333333333333333333333333333333333333333333333333333333333333333
base_rootfs:
  version: ubuntu-24.04
  artifacts:
    x86_64:
      url: https://example.com/rootfs
      sha256: 4444444444444444444444444444444444444444444444444444444444444444
`

func TestParseRejects(t *testing.T) {
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	tests := map[string]struct{ old, new, want string }{
		"unknown key":     {"schema: 1", "schema: 1\nextra: true", "field extra not found"},
		"wrong schema":    {"schema: 1", "schema: 2", "schema: want 1"},
		"bad version":     {"version: v1.0.0", "version: 1.0.0", "release tag"},
		"http url":        {"https://example.com/vmlinux", "http://example.com/vmlinux", "must be https"},
		"short sha":       {"sha256: 3333333333333333333333333333333333333333333333333333333333333333", "sha256: 3333", "guest_kernel.artifacts.x86_64.sha256"},
		"uppercase sha":   {"jailer: 2222222222222222222222222222222222222222222222222222222222222222", "jailer: AAAA222222222222222222222222222222222222222222222222222222222222", "binaries.jailer: not a lowercase"},
		"missing jailer":  {"        jailer: 2222222222222222222222222222222222222222222222222222222222222222\n", "", "binaries.jailer: missing"},
		"missing rootfs":  {"  version: ubuntu-24.04\n", "", "base_rootfs.version: missing"},
		"not yaml at all": {"schema: 1", "schema: [", "parse versions.yaml"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(valid, tt.old, tt.new, 1)
			if doc == valid {
				t.Fatal("test did not change the document")
			}
			_, err := Parse([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versions.yaml")
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Firecracker.Version != "v1.0.0" {
		t.Errorf("loaded version %s", f.Firecracker.Version)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("want an error for a missing file")
	}
}
