package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"

	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/storage"
	"github.com/Lazypilottt/forkbox/internal/versions"
)

const (
	kvmDevice  = "/dev/kvm"
	uffdDevice = "/dev/userfaultfd"
	uffdSysctl = "/proc/sys/vm/unprivileged_userfaultfd"
	cgroupRoot = "/sys/fs/cgroup"
)

// validatedHostKernels are the host kernel series Firecracker v1.17 tests
// against (docs/kernel-policy.md in the pinned release). Others usually work.
var validatedHostKernels = []string{"5.10", "6.1", "6.18"}

const linuxFix = `# Firecracker needs Linux with KVM. Run Forkbox on a Linux x86_64 host:
# bare metal, or a cloud VM with nested virtualisation (README: Requirements).`

const grubCgroupV2Fix = `sudo sed -i 's/^GRUB_CMDLINE_LINUX="/&systemd.unified_cgroup_hierarchy=1 /' /etc/default/grub && sudo update-grub && sudo reboot`

const installUffdKernelFix = `# This kernel was built without CONFIG_USERFAULTFD. Install one that has it
# (Ubuntu's generic kernels do):
sudo apt-get install -y linux-generic && sudo reboot`

func pass(detail string) Result { return Result{Status: Pass, Detail: detail} }

func fail(detail, fix string) Result { return Result{Status: Fail, Detail: detail, Fix: fix} }

// requireLinux fails a check early on non-Linux hosts. Only the platform
// check carries the fix, so the output does not repeat it eleven times.
func requireLinux(env Env) (Result, bool) {
	if env.OS() == "linux" {
		return Result{}, true
	}
	return fail(fmt.Sprintf("skipped: needs Linux, this host runs %s (see platform)", env.OS()), ""), false
}

func checkPlatform(_ context.Context, env Env, cfg Config) Result {
	if env.OS() != "linux" {
		return fail(fmt.Sprintf("%s/%s: Firecracker runs only on Linux", env.OS(), env.Arch()), linuxFix)
	}
	arch, err := versions.Arch(env.Arch())
	if err != nil {
		return fail(err.Error(), "# Use an x86_64 Linux host.")
	}
	if _, _, err := cfg.Versions.ForArch(arch); err != nil {
		return fail(err.Error(), "# Use an x86_64 Linux host, or pin a Firecracker release and guest kernel\n# for "+arch+" in versions.yaml.")
	}
	return pass(fmt.Sprintf("linux/%s (%s is pinned in versions.yaml)", env.Arch(), arch))
}

func checkKVM(_ context.Context, env Env, _ Config) Result {
	if r, ok := requireLinux(env); !ok {
		return r
	}
	fi, err := env.Stat(kvmDevice)
	if errors.Is(err, fs.ErrNotExist) {
		if inContainer(env) {
			return fail(kvmDevice+" does not exist inside this container",
				"docker run --device /dev/kvm ...   # pass the host's KVM device into the container\n# or run forkbox doctor on the host itself")
		}
		vendor, hasVirt := cpuVirtualisation(env)
		if !hasVirt {
			return fail(kvmDevice+" does not exist and the CPU reports no vmx/svm flag",
				"# Enable Intel VT-x or AMD-V in the firmware settings, or enable nested\n# virtualisation for this cloud VM, then reboot.")
		}
		module := "kvm_intel"
		if vendor == "AuthenticAMD" {
			module = "kvm_amd"
		}
		return fail(kvmDevice+" does not exist (the KVM module is not loaded)", "sudo modprobe "+module)
	}
	if err != nil {
		return fail("cannot stat "+kvmDevice+": "+err.Error(), "")
	}
	if fi.Mode()&fs.ModeCharDevice == 0 {
		return fail(kvmDevice+" exists but is not a character device",
			"sudo rm "+kvmDevice+" && sudo mknod -m 0660 /dev/kvm c 10 232 && sudo chgrp kvm /dev/kvm")
	}
	u := env.Username()
	if err := env.AccessRW(kvmDevice); err != nil {
		return fail(fmt.Sprintf("%s is not readable and writable by %s", kvmDevice, u),
			fmt.Sprintf("sudo setfacl -m u:%s:rw /dev/kvm\n# or permanently: sudo usermod -aG kvm %s   (then log out and back in)", u, u))
	}
	return pass(fmt.Sprintf("%s is readable and writable by %s", kvmDevice, u))
}

func checkKernel(_ context.Context, env Env, _ Config) Result {
	if r, ok := requireLinux(env); !ok {
		return r
	}
	rel, err := env.KernelRelease()
	if err != nil {
		return fail("cannot read the kernel release: "+err.Error(), "")
	}
	major, minor, err := parseKernelRelease(rel)
	if err != nil {
		return fail(err.Error(), "")
	}
	if !atLeast(major, minor, 5, 10) {
		return fail(fmt.Sprintf("kernel %s is older than 5.10, the oldest host kernel Firecracker supports", rel),
			"sudo apt-get install -y linux-generic-hwe-24.04 && sudo reboot\n# (Ubuntu; on other distributions install any 5.10 or newer kernel)")
	}
	series := fmt.Sprintf("%d.%d", major, minor)
	for _, v := range validatedHostKernels {
		if v == series {
			return pass(fmt.Sprintf("kernel %s (a host series Firecracker validates)", rel))
		}
	}
	return pass(fmt.Sprintf("kernel %s (5.10 or newer; Firecracker validates %s)", rel, strings.Join(validatedHostKernels, ", ")))
}

func checkCgroupV2(_ context.Context, env Env, _ Config) Result {
	if r, ok := requireLinux(env); !ok {
		return r
	}
	t, err := env.FSType(cgroupRoot)
	if err != nil {
		return fail("cannot statfs "+cgroupRoot+": "+err.Error(), grubCgroupV2Fix)
	}
	if t != storage.FSCgroup2 {
		return fail(fmt.Sprintf("%s is %s, not the unified cgroup v2 hierarchy", cgroupRoot, t), grubCgroupV2Fix)
	}
	data, err := env.ReadFile(cgroupRoot + "/cgroup.controllers")
	if err != nil {
		return fail("cannot read "+cgroupRoot+"/cgroup.controllers: "+err.Error(), "")
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(data)) {
		have[c] = true
	}
	var missing []string
	for _, c := range []string{"cpu", "memory"} {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fail(fmt.Sprintf("cgroup v2 is mounted, but the %s controller is not available", strings.Join(missing, " and ")),
			"# A controller still bound to cgroup v1 is unavailable to v2; boot with v1 off:\n"+grubCgroupV2Fix)
	}
	return pass("unified hierarchy at " + cgroupRoot + " with the cpu and memory controllers")
}

func checkUserfaultfd(_ context.Context, env Env, _ Config) Result {
	if r, ok := requireLinux(env); !ok {
		return r
	}
	rel, err := env.KernelRelease()
	if err != nil {
		return fail("cannot read the kernel release: "+err.Error(), "")
	}
	major, minor, err := parseKernelRelease(rel)
	if err != nil {
		return fail(err.Error(), "")
	}
	// Firecracker's "handling page faults on snapshot resume" doc: from kernel
	// 6.1 the UFFD comes from /dev/userfaultfd (the jailer exposes it in the
	// jail); on 5.10 it comes from the userfaultfd syscall, which a jailed,
	// unprivileged Firecracker may only call with vm.unprivileged_userfaultfd=1.
	if atLeast(major, minor, 6, 1) {
		fi, err := env.Stat(uffdDevice)
		if err == nil && fi.Mode()&fs.ModeCharDevice != 0 {
			return pass(uffdDevice + " is present; the jailer exposes it inside each jail")
		}
		if m := miscMinor(env, "userfaultfd"); m != "" {
			return fail(fmt.Sprintf("%s is missing, but kernel %s provides it (misc minor %s)", uffdDevice, rel, m),
				fmt.Sprintf("sudo mknod -m 0600 %s c 10 %s", uffdDevice, m))
		}
		return fail(fmt.Sprintf("%s is missing and kernel %s has no userfaultfd device", uffdDevice, rel), installUffdKernelFix)
	}
	data, err := env.ReadFile(uffdSysctl)
	if err != nil {
		return fail(fmt.Sprintf("kernel %s has no vm.unprivileged_userfaultfd sysctl", rel), installUffdKernelFix)
	}
	if strings.TrimSpace(string(data)) == "1" {
		return pass(fmt.Sprintf("kernel %s uses the userfaultfd syscall, and vm.unprivileged_userfaultfd=1", rel))
	}
	return fail(fmt.Sprintf("vm.unprivileged_userfaultfd=%s: the jailed, unprivileged Firecracker cannot create a userfaultfd on kernel %s",
		strings.TrimSpace(string(data)), rel),
		"sudo sysctl -w vm.unprivileged_userfaultfd=1\necho 'vm.unprivileged_userfaultfd = 1' | sudo tee /etc/sysctl.d/99-forkbox.conf")
}

func mountXFSFix(dir string) string {
	return fmt.Sprintf(`# Mount a dedicated XFS volume there. WARNING: mkfs erases /dev/<device>.
sudo mkfs.xfs -m reflink=1 /dev/<device>
sudo mkdir -p %[1]s && sudo mount /dev/<device> %[1]s
echo '/dev/<device> %[1]s xfs defaults 0 0' | sudo tee -a /etc/fstab`, dir)
}

func checkDataDirFS(_ context.Context, env Env, cfg Config) Result {
	if r, ok := requireLinux(env); !ok {
		return r
	}
	d := cfg.DataDir
	fi, err := env.Stat(d)
	if errors.Is(err, fs.ErrNotExist) {
		return fail(d+" does not exist", mountXFSFix(d))
	}
	if err != nil {
		return fail("cannot stat "+d+": "+err.Error(), "sudo forkbox doctor")
	}
	if !fi.IsDir() {
		return fail(d+" is not a directory", "")
	}
	t, err := env.FSType(d)
	if err != nil {
		return fail("cannot detect the filesystem of "+d+": "+err.Error(), "")
	}
	if !storage.ReflinkCapable(t) {
		return fail(fmt.Sprintf("%s is on %s; Forkbox needs XFS or btrfs for reflink copies", d, t), mountXFSFix(d))
	}
	return pass(d + " is on " + t)
}

func checkDataDirReflink(_ context.Context, env Env, cfg Config) Result {
	if r, ok := requireLinux(env); !ok {
		return r
	}
	d := cfg.DataDir
	if fi, err := env.Stat(d); err != nil || !fi.IsDir() {
		return fail("skipped: "+d+" is not available (see data-dir-fs)", "")
	}
	err := env.ProbeReflink(d)
	switch {
	case err == nil:
		return pass("a real reflink copy in " + d + " succeeded")
	case errors.Is(err, fs.ErrPermission):
		return fail("cannot write a probe file in "+d+": permission denied",
			"sudo forkbox doctor   # forkboxd runs as root and owns the data directory")
	case errors.Is(err, storage.ErrNoReflink):
		if t, _ := env.FSType(d); t == storage.FSXFS {
			return fail("the XFS filesystem at "+d+" was made without reflink support",
				fmt.Sprintf("# Recreate it with reflink on. WARNING: this erases it.\nsudo umount %[1]s && sudo mkfs.xfs -f -m reflink=1 /dev/<device> && sudo mount /dev/<device> %[1]s", d))
		}
		return fail(d+" refused a reflink copy: "+err.Error(), mountXFSFix(d))
	}
	return fail("reflink probe in "+d+" failed: "+err.Error(), "")
}

// binaryCheck verifies an installed Firecracker release binary against
// versions.yaml: its checksum, then the version it reports.
func binaryCheck(name, label string) func(context.Context, Env, Config) Result {
	return func(ctx context.Context, env Env, cfg Config) Result {
		if r, ok := requireLinux(env); !ok {
			return r
		}
		want := cfg.Versions.Firecracker.Version
		arch, err := versions.Arch(env.Arch())
		if err != nil {
			return fail("skipped: no pinned "+name+" for this architecture (see platform)", "")
		}
		fc, _, err := cfg.Versions.ForArch(arch)
		if err != nil {
			return fail("skipped: no pinned "+name+" for "+arch+" (see platform)", "")
		}
		path := datadir.Bin(cfg.DataDir, name)
		if _, err := env.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return fail(path+" is not installed", "sudo forkbox setup")
		} else if err != nil {
			return fail("cannot stat "+path+": "+err.Error(), "")
		}
		sum, err := env.SHA256File(path)
		if err != nil {
			return fail("cannot read "+path+": "+err.Error(), "")
		}
		if pinned := fc.Binaries[name]; sum != pinned {
			return fail(fmt.Sprintf("%s does not match the pinned %s %s (sha256 %s, want %s)",
				path, label, want, short(sum), short(pinned)), "sudo forkbox setup --force")
		}
		out, err := env.Output(ctx, path, "--version")
		if err != nil {
			return fail(fmt.Sprintf("%s --version failed: %v", path, err), "sudo forkbox setup --force")
		}
		line := firstLine(out)
		if !hasField(line, want) {
			return fail(fmt.Sprintf("%s reports %q, want %s %s", path, line, label, want), "sudo forkbox setup --force")
		}
		return pass(fmt.Sprintf("%s is %s, checksum matches versions.yaml", path, line))
	}
}

// toolCheck finds a host tool and checks that --version names the expected package.
func toolCheck(bin string, versionArgs []string, wantInVersion, pkg string) func(context.Context, Env, Config) Result {
	return func(ctx context.Context, env Env, _ Config) Result {
		if r, ok := requireLinux(env); !ok {
			return r
		}
		path, ok := lookTool(env, bin)
		if !ok {
			return fail(bin+" not found ("+pkg+" is not installed)", "sudo apt-get install -y "+pkg)
		}
		out, err := env.Output(ctx, path, versionArgs...)
		if err != nil {
			return fail(fmt.Sprintf("%s %s failed: %v", path, strings.Join(versionArgs, " "), err),
				"sudo apt-get install -y --reinstall "+pkg)
		}
		line := firstLine(out)
		if !strings.Contains(line, wantInVersion) {
			return fail(fmt.Sprintf("%s is not from %s: %q", path, pkg, line), "sudo apt-get install -y "+pkg)
		}
		return pass(path + ": " + line)
	}
}

// lookTool searches PATH, then the sbin directories that are often missing
// from a normal user's PATH.
func lookTool(env Env, bin string) (string, bool) {
	if p, err := env.LookPath(bin); err == nil {
		return p, true
	}
	for _, dir := range []string{"/usr/sbin", "/sbin"} {
		p := dir + "/" + bin
		if fi, err := env.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

func inContainer(env Env) bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := env.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// cpuVirtualisation returns the CPU vendor and whether it advertises VT-x or AMD-V.
func cpuVirtualisation(env Env) (vendor string, ok bool) {
	data, err := env.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "vendor_id":
			vendor = strings.TrimSpace(val)
		case "flags":
			for _, f := range strings.Fields(val) {
				if f == "vmx" || f == "svm" {
					ok = true
				}
			}
		}
	}
	return vendor, ok
}

// miscMinor returns the minor number of a misc device from /proc/misc.
func miscMinor(env Env, name string) string {
	data, err := env.ReadFile("/proc/misc")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return f[0]
		}
	}
	return ""
}

var kernelRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

func parseKernelRelease(rel string) (major, minor int, err error) {
	m := kernelRe.FindStringSubmatch(rel)
	if m == nil {
		return 0, 0, fmt.Errorf("cannot parse kernel release %q", rel)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, nil
}

func atLeast(major, minor, wantMajor, wantMinor int) bool {
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

func hasField(line, want string) bool {
	for _, f := range strings.Fields(line) {
		if f == want {
			return true
		}
	}
	return false
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
