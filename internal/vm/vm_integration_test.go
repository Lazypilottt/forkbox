//go:build integration

package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/setup"
	"github.com/Lazypilottt/forkbox/internal/testutil"
	"github.com/Lazypilottt/forkbox/internal/versions"
)

// The test guest is the pinned Ubuntu rootfs with one extra file, an init
// that prints a counter to the serial console ten times a second. It runs
// as pid 1, so the snapshot holds it mid-loop.
const testInit = `#!/bin/sh
echo "fbx-test-init: started"
i=0
while :; do
  echo "fbx-counter $i"
  i=$((i+1))
  sleep 0.1
done
`

const testInitMarker = "fbx-test-init: started"

var counterRE = regexp.MustCompile(`fbx-counter (\d+)`)

type kvmEnv struct {
	m       *Manager
	dataDir string
	kernel  string
	rootfs  string
}

func newKVMEnv(t *testing.T) *kvmEnv {
	t.Helper()
	testutil.RequireKVM(t)
	if os.Geteuid() != 0 {
		t.Fatal("the jailer needs root: run `make integration` (go test -exec 'sudo -n')")
	}
	v, err := versions.Embedded()
	must(t, err)
	arch, err := versions.Arch(runtime.GOARCH)
	must(t, err)
	dataDir := datadir.FromEnv()
	for _, b := range []string{"jailer", "firecracker"} {
		if _, err := os.Stat(datadir.Bin(dataDir, b)); err != nil {
			t.Fatalf("%v: run `sudo forkbox setup` first", err)
		}
	}
	kernel := datadir.Kernel(dataDir, v.GuestKernel.Version)
	if _, err := os.Stat(kernel); err != nil {
		t.Fatalf("%v: run `sudo forkbox setup` first", err)
	}
	m, err := NewManager(Options{DataDir: dataDir, FirecrackerVersion: v.Firecracker.Version})
	must(t, err)
	return &kvmEnv{m: m, dataDir: dataDir, kernel: kernel, rootfs: testRootfs(t, dataDir, v, arch)}
}

// testRootfs builds the test image once per pinned rootfs, the way
// Firecracker's getting-started guide does (unsquashfs, then mkfs.ext4 -d).
func testRootfs(t *testing.T, dataDir string, v *versions.File, arch string) string {
	t.Helper()
	art, ok := v.BaseRootfs.Artifacts[arch]
	if !ok {
		t.Fatalf("versions.yaml pins no base rootfs for %s", arch)
	}
	dir := filepath.Join(dataDir, "test")
	must(t, os.MkdirAll(dir, 0o755))
	img := filepath.Join(dir, fmt.Sprintf("counter-%s.ext4", art.SHA256[:12]))
	if _, err := os.Stat(img); err == nil {
		return img
	}
	for _, tool := range []string{"unsquashfs", "mkfs.ext4"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s not found: sudo apt install squashfs-tools e2fsprogs", tool)
		}
	}
	squash := filepath.Join(dir, "base-"+v.BaseRootfs.Version+".squashfs")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	must(t, setup.FetchVerified(ctx, nil, testLog{t}, art.URL, squash, art.SHA256))

	build, err := os.MkdirTemp(dir, "build-")
	must(t, err)
	defer os.RemoveAll(build)
	root := filepath.Join(build, "root")
	run(t, "unsquashfs", "-q", "-d", root, squash)
	must(t, os.WriteFile(filepath.Join(root, "fbx-test-init"), []byte(testInit), 0o755))
	part := filepath.Join(build, "rootfs.ext4")
	run(t, "truncate", "-s", "1G", part)
	run(t, "mkfs.ext4", "-q", "-d", root, "-F", part)
	must(t, os.Rename(part, img))
	return img
}

func (e *kvmEnv) config() Config {
	return Config{
		VCPUs:      2,
		MemMiB:     256,
		Kernel:     e.kernel,
		KernelArgs: DefaultKernelArgs + " init=/fbx-test-init",
		Rootfs:     Drive{Path: e.rootfs, ReadOnly: true},
		Vsock:      &Vsock{GuestCID: 3},
	}
}

func TestIntegrationBootReachesInit(t *testing.T) {
	e := newKVMEnv(t)
	v, err := e.m.Boot(context.Background(), e.config())
	must(t, err)
	id := v.ID()
	t.Cleanup(func() { v.Stop() })

	waitConsole(t, v.ConsolePath(), 15*time.Second, func(s string) bool { return strings.Contains(s, testInitMarker) })
	if !strings.Contains(readFile(t, v.ConsolePath()), "Linux version") {
		t.Errorf("console has no kernel banner")
	}

	// Firecracker runs jailed: unprivileged ids, seccomp filtering, own cgroup.
	status := readFile(t, fmt.Sprintf("/proc/%d/status", v.Pid()))
	for _, want := range []string{
		fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d", DefaultUID, DefaultUID, DefaultUID, DefaultUID),
		fmt.Sprintf("Gid:\t%d\t%d\t%d\t%d", DefaultGID, DefaultGID, DefaultGID, DefaultGID),
		"Seccomp:\t2",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("/proc/%d/status lacks %q", v.Pid(), want)
		}
	}
	cg := readFile(t, fmt.Sprintf("/proc/%d/cgroup", v.Pid()))
	if want := "0::/" + DefaultCgroupParent + "/" + id; !strings.Contains(cg, want) {
		t.Errorf("cgroup = %q, want %q", cg, want)
	}
	readEq(t, filepath.Join(v.CgroupPath(), "cpu.max"), "200000 100000\n")
	readEq(t, filepath.Join(v.CgroupPath(), "memory.max"), strconv.Itoa((256+MemoryOverheadMiB)<<20)+"\n")
	if _, err := os.Stat(v.VsockPath()); err != nil {
		t.Errorf("vsock socket: %v", err)
	}
	logTimings(t, v)

	must(t, v.Stop())
	assertHostClean(t, e, id)
}

func TestIntegrationSnapshotRestoreKeepsCounting(t *testing.T) {
	e := newKVMEnv(t)
	ctx := context.Background()
	v, err := e.m.Boot(ctx, e.config())
	must(t, err)
	parentID := v.ID()
	t.Cleanup(func() { v.Stop() })
	waitConsole(t, v.ConsolePath(), 15*time.Second, func(s string) bool { return maxCounter(s) >= 10 })

	must(t, v.Pause(ctx))
	before := maxCounter(readFile(t, v.ConsolePath()))
	snapDir := filepath.Join(e.dataDir, "test", "snapshots", parentID)
	t.Cleanup(func() { os.RemoveAll(snapDir) })
	_, err = v.SnapshotFull(ctx, snapDir)
	must(t, err)
	logTimings(t, v)
	must(t, v.Stop())
	assertHostClean(t, e, parentID)

	r, err := e.m.Restore(ctx, snapDir, MemBackend{Kind: MemFile}, Config{Rootfs: Drive{Path: e.rootfs, ReadOnly: true}})
	must(t, err)
	t.Cleanup(func() { r.Stop() })
	must(t, r.Resume(ctx))
	waitConsole(t, r.ConsolePath(), 10*time.Second, func(s string) bool { return maxCounter(s) > before+5 })

	out := readFile(t, r.ConsolePath())
	if strings.Contains(out, testInitMarker) {
		t.Fatalf("restored guest rebooted instead of resuming:\n%s", out)
	}
	// The counter continues from where the snapshot froze it.
	first := firstCounter(out)
	if first < before || first > before+2 {
		t.Errorf("first counter after restore = %d, snapshot was at %d", first, before)
	}
	t.Logf("counter: %d at snapshot, %d first after restore, %d now", before, first, maxCounter(out))
	logTimings(t, r)
	must(t, r.Stop())
	assertHostClean(t, e, r.ID())
}

func TestIntegrationFailuresLeaveNothing(t *testing.T) {
	e := newKVMEnv(t)
	ctx := context.Background()

	t.Run("bad kernel", func(t *testing.T) {
		bad := filepath.Join(e.dataDir, "test", "bad-vmlinux")
		must(t, os.WriteFile(bad, make([]byte, 4096), 0o644))
		t.Cleanup(func() { os.Remove(bad) })
		cfg := e.config()
		cfg.ID = "fbx-it-badkernel"
		cfg.Kernel = bad
		if _, err := e.m.Boot(ctx, cfg); err == nil {
			t.Fatal("booting a zeroed kernel succeeded")
		} else {
			t.Logf("boot error (expected): %v", err)
		}
		assertHostClean(t, e, cfg.ID)
	})

	t.Run("firecracker killed", func(t *testing.T) {
		v, err := e.m.Boot(ctx, e.config())
		must(t, err)
		must(t, syscall.Kill(v.Pid(), syscall.SIGKILL))
		select {
		case <-v.proc.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("firecracker did not die from SIGKILL")
		}
		// The next call finds the API gone and cleans up.
		if err := v.Pause(ctx); err == nil || v.State() != Stopped {
			t.Errorf("after SIGKILL: pause err=%v state=%s", err, v.State())
		}
		must(t, v.Stop())
		assertHostClean(t, e, v.ID())
	})

	t.Run("owner lost", func(t *testing.T) {
		v, err := e.m.Boot(ctx, e.config())
		must(t, err)
		// A new owner only knows the id, as after a daemon restart.
		must(t, e.m.Reap(v.ID()))
		assertHostClean(t, e, v.ID())
	})
}

// assertHostClean checks for leftover processes, jails, run dirs and cgroups.
func assertHostClean(t *testing.T, e *kvmEnv, id string) {
	t.Helper()
	l := layout{e.dataDir, id}
	for _, p := range []string{l.jailDir(), l.runDir(), e.m.cg.path(id)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind (err=%v)", p, err)
		}
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, a := range splitNul(b) {
			if a == "--id="+id || a == id && strings.Contains(string(b), "jailer") {
				t.Errorf("process %s still running for %s: %q", filepath.Dir(p), id, splitNul(b))
				break
			}
		}
	}
}

func waitConsole(t *testing.T, path string, timeout time.Duration, ok func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		b, _ := os.ReadFile(path)
		if ok(string(b)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("console condition not met after %s; console:\n%s", timeout, tailFile(path, 4096))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func maxCounter(s string) int {
	best := -1
	for _, m := range counterRE.FindAllStringSubmatch(s, -1) {
		if n, _ := strconv.Atoi(m[1]); n > best {
			best = n
		}
	}
	return best
}

func firstCounter(s string) int {
	m := counterRE.FindStringSubmatch(s)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func logTimings(t *testing.T, v *VM) {
	t.Helper()
	for _, tm := range v.Timings() {
		t.Logf("timing %-14s %8.2f ms", tm.Step, float64(tm.Duration.Microseconds())/1000)
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
