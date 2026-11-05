package doctor

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/Lazypilottt/forkbox/internal/storage"
)

func TestHealthyHostPassesEveryCheck(t *testing.T) {
	results := Run(context.Background(), healthy(t), testConfig(t))
	if len(results) != len(Checks()) {
		t.Fatalf("got %d results for %d checks", len(results), len(Checks()))
	}
	for _, r := range results {
		if r.Status != Pass {
			t.Errorf("%s: %s %s (fix: %s)", r.Name, r.Status, r.Detail, r.Fix)
		}
	}
}

// TestChecks breaks one thing at a time on a healthy host and checks that the
// right check fails, with a useful detail and the right fix.
func TestChecks(t *testing.T) {
	fc := testDataDir + "/bin/firecracker"
	jailer := testDataDir + "/bin/jailer"
	tests := []struct {
		name       string
		check      string
		break_     func(e *fakeEnv)
		wantStatus Status
		wantDetail string
		wantFix    string
	}{
		// platform
		{"linux amd64", "platform", nil, Pass, "x86_64 is pinned", ""},
		{"macOS", "platform", func(e *fakeEnv) { e.os, e.arch = "darwin", "arm64" }, Fail, "runs only on Linux", "Linux x86_64 host"},
		{"arm64 not pinned", "platform", func(e *fakeEnv) { e.arch = "arm64" }, Fail, "aarch64", "pin a Firecracker release"},
		{"unsupported arch", "platform", func(e *fakeEnv) { e.arch = "riscv64" }, Fail, "riscv64", "x86_64"},

		// kvm
		{"kvm ok", "kvm", nil, Pass, "readable and writable by dev", ""},
		{"kvm missing intel", "kvm", func(e *fakeEnv) { delete(e.nodes, kvmDevice) }, Fail, "module is not loaded", "sudo modprobe kvm_intel"},
		{"kvm missing amd", "kvm", func(e *fakeEnv) {
			delete(e.nodes, kvmDevice)
			e.files["/proc/cpuinfo"] = "vendor_id : AuthenticAMD\nflags : fpu svm\n"
		}, Fail, "module is not loaded", "sudo modprobe kvm_amd"},
		{"kvm missing no virt", "kvm", func(e *fakeEnv) {
			delete(e.nodes, kvmDevice)
			e.files["/proc/cpuinfo"] = "vendor_id : GenuineIntel\nflags : fpu sse\n"
		}, Fail, "no vmx/svm", "nested"},
		{"kvm missing in container", "kvm", func(e *fakeEnv) {
			delete(e.nodes, kvmDevice)
			e.nodes["/.dockerenv"] = 0o644
		}, Fail, "inside this container", "--device /dev/kvm"},
		{"kvm missing in podman", "kvm", func(e *fakeEnv) {
			delete(e.nodes, kvmDevice)
			e.nodes["/run/.containerenv"] = 0o644
		}, Fail, "inside this container", "--device /dev/kvm"},
		{"kvm not a device", "kvm", func(e *fakeEnv) { e.nodes[kvmDevice] = 0o644 }, Fail, "not a character device", "mknod -m 0660 /dev/kvm c 10 232"},
		{"kvm no access", "kvm", func(e *fakeEnv) { e.noAccess = map[string]bool{kvmDevice: true} }, Fail, "not readable and writable by dev", "sudo setfacl -m u:dev:rw /dev/kvm"},
		{"kvm on mac", "kvm", func(e *fakeEnv) { e.os = "darwin" }, Fail, "needs Linux", ""},

		// kernel
		{"kernel 6.8", "kernel", nil, Pass, "6.8.0-45-generic (5.10 or newer", ""},
		{"kernel validated", "kernel", func(e *fakeEnv) { e.kernel = "6.1.102-111.182.amzn2023.x86_64" }, Pass, "a host series Firecracker validates", ""},
		{"kernel 5.10", "kernel", func(e *fakeEnv) { e.kernel = "5.10.0-28-amd64" }, Pass, "validates", ""},
		{"kernel too old", "kernel", func(e *fakeEnv) { e.kernel = "5.4.0-150-generic" }, Fail, "older than 5.10", "linux-generic-hwe-24.04"},
		{"kernel 4.19", "kernel", func(e *fakeEnv) { e.kernel = "4.19.0" }, Fail, "older than 5.10", "sudo reboot"},
		{"kernel garbage", "kernel", func(e *fakeEnv) { e.kernel = "weird" }, Fail, "cannot parse", ""},
		{"kernel unreadable", "kernel", func(e *fakeEnv) { e.kernel = "" }, Fail, "cannot read the kernel release", ""},

		// cgroup v2
		{"cgroup ok", "cgroup-v2", nil, Pass, "cpu and memory", ""},
		{"cgroup v1", "cgroup-v2", func(e *fakeEnv) { e.fsTypes[cgroupRoot] = storage.FSTmpfs }, Fail, "is tmpfs, not the unified", "systemd.unified_cgroup_hierarchy=1"},
		{"cgroup missing memory", "cgroup-v2", func(e *fakeEnv) {
			e.files["/sys/fs/cgroup/cgroup.controllers"] = "cpuset cpu io pids\n"
		}, Fail, "memory controller", "unified_cgroup_hierarchy"},
		{"cgroup missing both", "cgroup-v2", func(e *fakeEnv) {
			e.files["/sys/fs/cgroup/cgroup.controllers"] = "pids\n"
		}, Fail, "cpu and memory controller", ""},
		{"cgroup not mounted", "cgroup-v2", func(e *fakeEnv) { delete(e.fsTypes, cgroupRoot) }, Fail, "cannot statfs", "update-grub"},

		// userfaultfd
		{"uffd device", "userfaultfd", nil, Pass, "/dev/userfaultfd is present", ""},
		{"uffd device missing but in /proc/misc", "userfaultfd", func(e *fakeEnv) { delete(e.nodes, uffdDevice) }, Fail, "misc minor 126", "sudo mknod -m 0600 /dev/userfaultfd c 10 126"},
		{"uffd no device in kernel", "userfaultfd", func(e *fakeEnv) {
			delete(e.nodes, uffdDevice)
			e.files["/proc/misc"] = "232 kvm\n"
		}, Fail, "no userfaultfd device", "CONFIG_USERFAULTFD"},
		{"uffd 5.10 sysctl on", "userfaultfd", func(e *fakeEnv) {
			e.kernel = "5.10.0-28-amd64"
			delete(e.nodes, uffdDevice)
			e.files[uffdSysctl] = "1\n"
		}, Pass, "vm.unprivileged_userfaultfd=1", ""},
		{"uffd 5.10 sysctl off", "userfaultfd", func(e *fakeEnv) {
			e.kernel = "5.10.0-28-amd64"
			delete(e.nodes, uffdDevice)
		}, Fail, "vm.unprivileged_userfaultfd=0", "sudo sysctl -w vm.unprivileged_userfaultfd=1"},
		{"uffd 5.10 ignores device", "userfaultfd", func(e *fakeEnv) { e.kernel = "5.10.0-28-amd64" }, Fail, "=0", "sysctl"},
		{"uffd 5.10 no sysctl", "userfaultfd", func(e *fakeEnv) {
			e.kernel = "5.10.0"
			delete(e.files, uffdSysctl)
		}, Fail, "no vm.unprivileged_userfaultfd sysctl", "CONFIG_USERFAULTFD"},

		// data dir filesystem
		{"datadir xfs", "data-dir-fs", nil, Pass, "is on xfs", ""},
		{"datadir btrfs", "data-dir-fs", func(e *fakeEnv) { e.fsTypes[testDataDir] = storage.FSBtrfs }, Pass, "is on btrfs", ""},
		{"datadir ext4", "data-dir-fs", func(e *fakeEnv) { e.fsTypes[testDataDir] = storage.FSExt4 }, Fail, "is on ext4; Forkbox needs XFS or btrfs", "sudo mkfs.xfs -m reflink=1 /dev/<device>"},
		{"datadir missing", "data-dir-fs", func(e *fakeEnv) { delete(e.nodes, testDataDir) }, Fail, "does not exist", "sudo mount /dev/<device> /var/lib/forkbox"},
		{"datadir is file", "data-dir-fs", func(e *fakeEnv) { e.nodes[testDataDir] = 0o644 }, Fail, "not a directory", ""},

		// data dir reflink
		{"reflink ok", "data-dir-reflink", nil, Pass, "real reflink copy", ""},
		{"reflink xfs without reflink", "data-dir-reflink", func(e *fakeEnv) { e.reflinkErr = storage.ErrNoReflink }, Fail, "made without reflink support", "mkfs.xfs -f -m reflink=1"},
		{"reflink refused on ext4", "data-dir-reflink", func(e *fakeEnv) {
			e.fsTypes[testDataDir] = storage.FSExt4
			e.reflinkErr = storage.ErrNoReflink
		}, Fail, "refused a reflink copy", "mkfs.xfs -m reflink=1"},
		{"reflink permission", "data-dir-reflink", func(e *fakeEnv) {
			e.reflinkErr = &fs.PathError{Op: "open", Path: testDataDir, Err: fs.ErrPermission}
		}, Fail, "permission denied", "sudo forkbox doctor"},
		{"reflink other error", "data-dir-reflink", func(e *fakeEnv) { e.reflinkErr = errors.New("disk on fire") }, Fail, "disk on fire", ""},
		{"reflink no datadir", "data-dir-reflink", func(e *fakeEnv) { delete(e.nodes, testDataDir) }, Fail, "see data-dir-fs", ""},

		// firecracker and jailer
		{"firecracker ok", "firecracker", nil, Pass, "Firecracker v1.17.0, checksum matches", ""},
		{"jailer ok", "jailer", nil, Pass, "Jailer v1.17.0", ""},
		{"firecracker missing", "firecracker", func(e *fakeEnv) { delete(e.nodes, fc) }, Fail, "is not installed", "sudo forkbox setup"},
		{"jailer missing", "jailer", func(e *fakeEnv) { delete(e.nodes, jailer) }, Fail, "is not installed", "sudo forkbox setup"},
		{"firecracker wrong checksum", "firecracker", func(e *fakeEnv) { e.sums[fc] = strings.Repeat("ab", 32) }, Fail, "does not match the pinned Firecracker v1.17.0", "sudo forkbox setup --force"},
		{"jailer wrong version", "jailer", func(e *fakeEnv) { e.outputs[jailer+" --version"] = "Jailer v1.16.2\n" }, Fail, `reports "Jailer v1.16.2", want Jailer v1.17.0`, "--force"},
		{"version prefix is not a match", "firecracker", func(e *fakeEnv) { e.outputs[fc+" --version"] = "Firecracker v1.17.0-dev\n" }, Fail, "want Firecracker v1.17.0", ""},
		{"firecracker will not run", "firecracker", func(e *fakeEnv) { e.outputErr = map[string]error{fc + " --version": errors.New("exec format error")} }, Fail, "exec format error", "--force"},

		// nftables and iproute2
		{"nft ok", "nftables", nil, Pass, "nftables v1.0.9", ""},
		{"ip ok", "iproute2", nil, Pass, "iproute2-6.1.0", ""},
		{"nft missing", "nftables", func(e *fakeEnv) { delete(e.paths, "nft") }, Fail, "nft not found", "sudo apt-get install -y nftables"},
		{"ip missing", "iproute2", func(e *fakeEnv) { delete(e.paths, "ip") }, Fail, "ip not found", "sudo apt-get install -y iproute2"},
		{"nft only in sbin", "nftables", func(e *fakeEnv) {
			delete(e.paths, "nft")
			e.nodes["/usr/sbin/nft"] = 0o755
		}, Pass, "/usr/sbin/nft", ""},
		{"busybox ip", "iproute2", func(e *fakeEnv) { e.outputs["/usr/sbin/ip -V"] = "BusyBox v1.36.1 multi-call binary.\n" }, Fail, "not from iproute2", "sudo apt-get install -y iproute2"},
		{"nft broken", "nftables", func(e *fakeEnv) {
			e.outputErr = map[string]error{"/usr/sbin/nft --version": errors.New("exit status 1")}
		}, Fail, "failed", "--reinstall nftables"},
	}

	byName := map[string]Check{}
	for _, c := range Checks() {
		byName[c.Name] = c
	}
	for _, tt := range tests {
		t.Run(tt.check+"/"+tt.name, func(t *testing.T) {
			c, ok := byName[tt.check]
			if !ok {
				t.Fatalf("no check named %q", tt.check)
			}
			env := healthy(t)
			if tt.break_ != nil {
				tt.break_(env)
			}
			r := c.Run(context.Background(), env, testConfig(t))
			if r.Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s (detail %q)", r.Status, tt.wantStatus, r.Detail)
			}
			if !strings.Contains(r.Detail, tt.wantDetail) {
				t.Errorf("detail %q does not contain %q", r.Detail, tt.wantDetail)
			}
			if !strings.Contains(r.Fix, tt.wantFix) {
				t.Errorf("fix %q does not contain %q", r.Fix, tt.wantFix)
			}
			if r.Status == Fail && r.Detail == "" {
				t.Error("a failure must explain itself")
			}
		})
	}
}

// Every failure on a real host (not "skipped" ones that point at another
// check) must come with a fix.
func TestEveryFailureOnLinuxHasAFix(t *testing.T) {
	env := &fakeEnv{os: "linux", arch: "amd64", user: "dev", kernel: "6.8.0"}
	results := Run(context.Background(), env, testConfig(t))
	// Only platform and kernel can pass on a Linux host with nothing else on it.
	if got := len(results) - Failed(results); got != 2 {
		t.Errorf("%d checks passed on an empty host, want 2", got)
	}
	for _, r := range results {
		if r.Status == Pass {
			continue
		}
		if r.Fix == "" && !strings.Contains(r.Detail, "skipped") {
			t.Errorf("%s fails without a fix: %s", r.Name, r.Detail)
		}
	}
}

func TestNonLinuxFailsEverythingWithOneFix(t *testing.T) {
	env := &fakeEnv{os: "darwin", arch: "arm64", user: "dev"}
	results := Run(context.Background(), env, testConfig(t))
	if Failed(results) != len(results) {
		t.Fatalf("want every check to fail on darwin, got %d of %d", Failed(results), len(results))
	}
	if results[0].Name != "platform" || !strings.Contains(results[0].Fix, "Linux") {
		t.Errorf("platform should carry the fix, got %+v", results[0])
	}
	for _, r := range results[1:] {
		if r.Fix != "" {
			t.Errorf("%s repeats a fix: %q", r.Name, r.Fix)
		}
	}
}

func TestPrint(t *testing.T) {
	results := []Result{
		{Name: "kvm", Status: Pass, Detail: "/dev/kvm is fine"},
		{Name: "data-dir-fs", Status: Fail, Detail: "on ext4", Fix: "# erase it\nsudo mkfs.xfs /dev/x"},
	}
	var buf bytes.Buffer
	Print(&buf, results)
	want := "" +
		"PASS  kvm          /dev/kvm is fine\n" +
		"FAIL  data-dir-fs  on ext4\n" +
		"                   fix: # erase it\n" +
		"                        sudo mkfs.xfs /dev/x\n" +
		"\n1 passed, 1 failed\n"
	if got := buf.String(); got != want {
		t.Errorf("Print output:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunNamesResultsAndNormalisesStatus(t *testing.T) {
	results := Run(context.Background(), &fakeEnv{os: "darwin"}, testConfig(t))
	for i, c := range Checks() {
		if results[i].Name != c.Name {
			t.Errorf("result %d named %q, want %q", i, results[i].Name, c.Name)
		}
		if results[i].Status != Pass && results[i].Status != Fail {
			t.Errorf("%s has status %q", c.Name, results[i].Status)
		}
	}
}

func TestParseKernelRelease(t *testing.T) {
	for rel, want := range map[string][2]int{
		"6.8.0-45-generic":                {6, 8},
		"6.1.102-111.182.amzn2023.x86_64": {6, 1},
		"5.10.0":                          {5, 10},
		"6.18":                            {6, 18},
	} {
		maj, min, err := parseKernelRelease(rel)
		if err != nil || maj != want[0] || min != want[1] {
			t.Errorf("parseKernelRelease(%q) = %d, %d, %v; want %v", rel, maj, min, err, want)
		}
	}
	if _, _, err := parseKernelRelease("v6"); err == nil {
		t.Error("want an error for v6")
	}
}
