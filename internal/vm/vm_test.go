package vm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func TestJailerArgs(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	cfg := h.config(t)
	cfg.NetNS = "/run/netns/fbx-test"
	got := m.jailerArgs("vm-1", &cfg)
	want := []string{
		filepath.Join(h.dataDir, "bin", "jailer"),
		"--id", "vm-1",
		"--exec-file", filepath.Join(h.dataDir, "bin", "firecracker"),
		"--uid", "70000",
		"--gid", "70000",
		"--chroot-base-dir", filepath.Join(h.dataDir, "jail"),
		"--cgroup-version", "2",
		"--parent-cgroup", "forkbox",
		"--cgroup", "cpu.max=200000 100000",
		"--cgroup", "memory.max=603979776", // (512+64) MiB
		"--netns", "/run/netns/fbx-test",
		"--", "--api-sock", "/api.sock",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("jailer args\n got %q\nwant %q", got, want)
	}
	for _, bad := range []string{"--no-seccomp", "--seccomp-filter", "--daemonize", "--new-pid-ns"} {
		if slices.Contains(got, bad) {
			t.Errorf("jailer args contain %s", bad)
		}
	}

	cfg.CPUMax, cfg.MemoryMax, cfg.NetNS = "50000 100000", 1<<30, ""
	got = m.jailerArgs("vm-1", &cfg)
	for _, w := range []string{"cpu.max=50000 100000", "memory.max=1073741824"} {
		if !slices.Contains(got, w) {
			t.Errorf("overrides: %q missing from %q", w, got)
		}
	}
	if slices.Contains(got, "--netns") {
		t.Errorf("--netns passed without a namespace")
	}
}

func TestConfigValidation(t *testing.T) {
	h := newFakeHost(t)
	base := h.config(t)
	cases := map[string]func(*Config){
		"bad id":         func(c *Config) { c.ID = "Bad_ID" },
		"long id":        func(c *Config) { c.ID = strings.Repeat("a", 33) },
		"zero vcpus":     func(c *Config) { c.VCPUs = 0 },
		"odd vcpus":      func(c *Config) { c.VCPUs = 3 },
		"too many vcpus": func(c *Config) { c.VCPUs = 34 },
		"no memory":      func(c *Config) { c.MemMiB = 0 },
		"no kernel":      func(c *Config) { c.Kernel = "" },
		"missing kernel": func(c *Config) { c.Kernel = "/nonexistent/vmlinux" },
		"no rootfs":      func(c *Config) { c.Rootfs.Path = "" },
		"low vsock cid":  func(c *Config) { c.Vsock = &Vsock{GuestCID: 2} },
		"net no tap":     func(c *Config) { c.Net = &NetIface{IfaceID: "eth0"} },
		"missing netns":  func(c *Config) { c.NetNS = "/nonexistent/netns" },
	}
	for name, mod := range cases {
		c := base
		mod(&c)
		if err := c.validateBoot(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: got %v, want ErrConfig", name, err)
		}
	}
	if err := base.validateBoot(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	for _, id := range []string{"a", "vm-0123456789abcdef", "sbx-1-2"} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false", id)
		}
	}
}

func TestBoot(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	cfg := h.config(t)
	cfg.ID = "vm-boot"
	cfg.Vsock = &Vsock{GuestCID: 3}
	cfg.Net = &NetIface{IfaceID: "eth0", HostDev: "fbx-tap0", GuestMAC: "06:00:AC:10:00:02"}
	v, err := m.Boot(ctx, cfg)
	must(t, err)
	defer v.Stop()

	if v.State() != Running {
		t.Errorf("state = %s, want running", v.State())
	}
	type r struct{ method, path string }
	var got []r
	for _, q := range h.last().configRequests() {
		got = append(got, r{q.Method, q.Path})
	}
	want := []r{
		{"PUT", "/machine-config"}, {"PUT", "/boot-source"}, {"PUT", "/drives/rootfs"},
		{"PUT", "/vsock"}, {"PUT", "/network-interfaces/eth0"}, {"PUT", "/actions"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests\n got %v\nwant %v", got, want)
	}
	reqs := h.last().configRequests()
	checkBody(t, reqs[0], map[string]any{"vcpu_count": 2.0, "mem_size_mib": 512.0, "smt": false, "track_dirty_pages": true})
	checkBody(t, reqs[1], map[string]any{"kernel_image_path": "/boot/vmlinux", "boot_args": DefaultKernelArgs})
	checkBody(t, reqs[2], map[string]any{"drive_id": "rootfs", "path_on_host": "/drives/rootfs", "is_root_device": true, "is_read_only": true})
	checkBody(t, reqs[3], map[string]any{"guest_cid": 3.0, "uds_path": "/v.sock"})
	checkBody(t, reqs[4], map[string]any{"iface_id": "eth0", "host_dev_name": "fbx-tap0", "guest_mac": "06:00:AC:10:00:02"})
	checkBody(t, reqs[5], map[string]any{"action_type": "InstanceStart"})

	// The kernel and rootfs are hard links: same file, nothing copied.
	l := layout{h.dataDir, v.ID()}
	sameFile(t, cfg.Kernel, l.host(jailKernel))
	sameFile(t, cfg.Rootfs.Path, l.host(jailRootfs))
	// A read-only rootfs is not chowned; the snapshot dir is.
	if c := h.chowns(); !reflect.DeepEqual(c, []chownCall{{l.host(jailSnapDir), DefaultUID, DefaultGID}}) {
		t.Errorf("chown calls = %v", c)
	}
	// The (fake) jailer wrote the limits into the VM's own cgroup.
	readEq(t, filepath.Join(v.CgroupPath(), "cpu.max"), "200000 100000")
	readEq(t, filepath.Join(v.CgroupPath(), "memory.max"), "603979776")
	if want := filepath.Join(h.cgroupRoot, "forkbox", "vm-boot"); v.CgroupPath() != want {
		t.Errorf("cgroup = %s, want %s", v.CgroupPath(), want)
	}
	if !strings.Contains(readFile(t, filepath.Join(h.cgroupRoot, "forkbox", "cgroup.subtree_control")), "+cpu +memory") {
		t.Errorf("parent cgroup does not delegate cpu and memory")
	}
	if !strings.Contains(readFile(t, v.ConsolePath()), "fake guest: init started") {
		t.Errorf("console log missing guest output")
	}
	if v.VsockPath() != l.host("/v.sock") {
		t.Errorf("vsock path = %s", v.VsockPath())
	}
	assertSteps(t, v, StepJail, StepConfigure, StepStart, StepBoot)
}

func TestBootWritableRootfsIsChowned(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	cfg := h.config(t)
	cfg.Rootfs.ReadOnly = false
	must(t, os.Chmod(cfg.Rootfs.Path, 0o444)) // as a reflink of a template is
	v, err := m.Boot(ctx, cfg)
	must(t, err)
	defer v.Stop()
	want := chownCall{layout{h.dataDir, v.ID()}.host(jailRootfs), DefaultUID, DefaultGID}
	if !slices.Contains(h.chowns(), want) {
		t.Errorf("writable rootfs not chowned to the jail user: %v", h.chowns())
	}
	if fi, _ := os.Stat(want.path); fi.Mode().Perm() != 0o600 {
		t.Errorf("writable rootfs mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestSnapshotAndRestore(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	cfg := h.config(t)
	v, err := m.Boot(ctx, cfg)
	must(t, err)

	snapDir := filepath.Join(h.dataDir, "snapshots", "s1")
	if _, err := v.SnapshotFull(ctx, snapDir); !errors.Is(err, ErrState) {
		t.Errorf("snapshot of a running VM: got %v, want ErrState", err)
	}
	if v.State() != Running {
		t.Fatalf("a refused snapshot changed the state to %s", v.State())
	}
	must(t, v.Pause(ctx))
	if err := v.Pause(ctx); !errors.Is(err, ErrState) {
		t.Errorf("second pause: got %v, want ErrState", err)
	}
	s, err := v.SnapshotFull(ctx, snapDir)
	must(t, err)
	if s.Type != SnapshotFull || s.VCPUs != 2 || s.MemMiB != 512 || s.FirecrackerVersion != "v1.17.0" || s.VMID != v.ID() {
		t.Errorf("snapshot metadata = %+v", s)
	}
	checkBody(t, lastReq(h.last()), map[string]any{"snapshot_type": "Full", "snapshot_path": "/snapshot/vmstate", "mem_file_path": "/snapshot/mem"})
	readEq(t, filepath.Join(snapDir, SnapshotStateFile), "state:Full")
	readEq(t, filepath.Join(snapDir, SnapshotMemFile), "mem:Full")
	for _, f := range []string{SnapshotStateFile, SnapshotMemFile} {
		p := filepath.Join(snapDir, f)
		if !slices.Contains(h.chowns(), chownCall{p, 0, DefaultGID}) {
			t.Errorf("%s not chowned to root:jail-group", f)
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
			t.Errorf("%s mode = %v, want 0640", f, st.Mode().Perm())
		}
	}
	// Moved out of the jail, not copied.
	if ents, _ := os.ReadDir(layout{h.dataDir, v.ID()}.host(jailSnapDir)); len(ents) != 0 {
		t.Errorf("snapshot files left in the jail: %v", ents)
	}
	if _, err := v.SnapshotFull(ctx, snapDir); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("snapshot into a used dir: got %v", err)
	}
	if rs, err := ReadSnapshot(snapDir); err != nil || rs.Type != SnapshotFull || rs.Dir != snapDir {
		t.Errorf("ReadSnapshot = %+v, %v", rs, err)
	}
	must(t, v.Resume(ctx))
	if v.State() != Running {
		t.Errorf("state after resume = %s", v.State())
	}
	must(t, v.Stop())
	assertGone(t, h, m, v.ID())

	// Restore in a fresh jail, with only the rootfs from the config.
	r, err := m.Restore(ctx, snapDir, MemBackend{Kind: MemFile}, Config{Rootfs: cfg.Rootfs})
	must(t, err)
	if r.State() != Paused {
		t.Errorf("restored state = %s, want paused", r.State())
	}
	reqs := h.last().configRequests()
	if len(reqs) != 1 || reqs[0].Path != "/snapshot/load" {
		t.Fatalf("restore requests = %v, want only /snapshot/load", reqs)
	}
	checkBody(t, reqs[0], map[string]any{
		"snapshot_path":     "/restore/vmstate",
		"mem_backend":       map[string]any{"backend_type": "File", "backend_path": "/restore/mem"},
		"track_dirty_pages": true,
		"resume_vm":         false,
	})
	rl := layout{h.dataDir, r.ID()}
	sameFile(t, filepath.Join(snapDir, SnapshotMemFile), rl.host(jailRestMem))
	sameFile(t, filepath.Join(snapDir, SnapshotStateFile), rl.host(jailRestVM))
	sameFile(t, cfg.Rootfs.Path, rl.host(jailRootfs)) // same in-jail path as at snapshot time
	readEq(t, filepath.Join(r.CgroupPath(), "memory.max"), "603979776")
	assertSteps(t, r, StepRestoreJail, StepRestoreLoad, StepRestore)

	// A snapshot of the restored VM must not touch the snapshot it came from.
	must(t, r.Resume(ctx))
	must(t, r.Pause(ctx))
	_, err = r.SnapshotDiff(ctx, filepath.Join(h.dataDir, "snapshots", "s2"))
	must(t, err)
	readEq(t, filepath.Join(snapDir, SnapshotMemFile), "mem:Full")
	must(t, r.Stop())
	assertGone(t, h, m, r.ID())
	readEq(t, filepath.Join(snapDir, SnapshotMemFile), "mem:Full") // Stop only unlinks
}

func TestSnapshotDiff(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	v, err := m.Boot(ctx, h.config(t))
	must(t, err)
	defer v.Stop()
	must(t, v.Pause(ctx))
	dir := filepath.Join(h.dataDir, "snapshots", "d1")
	s, err := v.SnapshotDiff(ctx, dir)
	must(t, err)
	if s.Type != SnapshotDiff {
		t.Errorf("type = %s", s.Type)
	}
	checkBody(t, lastReq(h.last()), map[string]any{"snapshot_type": "Diff"})
	assertSteps(t, v, StepJail, StepConfigure, StepStart, StepBoot, StepPause, StepSnapshotDiff)
	if _, err := m.Restore(ctx, dir, MemBackend{}, Config{Rootfs: h.config(t).Rootfs}); err == nil || !strings.Contains(err.Error(), "not resumable") {
		t.Errorf("restore of a diff snapshot: got %v", err)
	}
}

func TestRestoreRefusals(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	cfg := h.config(t)
	v, err := m.Boot(ctx, cfg)
	must(t, err)
	must(t, v.Pause(ctx))
	dir := filepath.Join(h.dataDir, "snapshots", "s")
	_, err = v.SnapshotFull(ctx, dir)
	must(t, err)
	must(t, v.Stop())

	rc := Config{Rootfs: cfg.Rootfs}
	if _, err := m.Restore(ctx, dir, MemBackend{Kind: MemUffd, Path: "/x"}, rc); err == nil || !strings.Contains(err.Error(), "Task 12") {
		t.Errorf("uffd backend: got %v", err)
	}
	bad := rc
	bad.VCPUs = 4
	if _, err := m.Restore(ctx, dir, MemBackend{}, bad); !errors.Is(err, ErrConfig) {
		t.Errorf("vcpu mismatch: got %v", err)
	}
	other := h.manager(t, func(o *Options) { o.FirecrackerVersion = "v1.18.0" })
	if _, err := other.Restore(ctx, dir, MemBackend{}, rc); err == nil || !strings.Contains(err.Error(), "pins v1.18.0") {
		t.Errorf("version mismatch: got %v", err)
	}
	if _, err := m.Restore(ctx, filepath.Join(h.dataDir, "nope"), MemBackend{}, rc); err == nil {
		t.Errorf("missing snapshot accepted")
	}
	// Nothing was launched for any refusal.
	if n := len(h.procs); n != 1 {
		t.Errorf("%d launches, want 1", n)
	}
}

func TestBootFailureCleansUp(t *testing.T) {
	h := newFakeHost(t)
	h.failStatus["PUT /actions"] = 400
	m := h.manager(t, nil)
	cfg := h.config(t)
	cfg.ID = "vm-fail"
	_, err := m.Boot(ctx, cfg)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || !strings.Contains(ae.Fault, "injected failure") {
		t.Fatalf("got %v, want the API error", err)
	}
	if !strings.Contains(err.Error(), "fake jailer: vm-fail") {
		t.Errorf("error does not carry the console tail: %v", err)
	}
	assertGone(t, h, m, "vm-fail")
	// Inputs are untouched.
	readEq(t, cfg.Kernel, "kernel")
	readEq(t, cfg.Rootfs.Path, "rootfs")
}

func TestBootTimeoutCleansUp(t *testing.T) {
	h := newFakeHost(t)
	h.hang["PUT /machine-config"] = true
	m := h.manager(t, func(o *Options) { o.APITimeout = 100 * time.Millisecond })
	cfg := h.config(t)
	cfg.ID = "vm-hang"
	start := time.Now()
	_, err := m.Boot(ctx, cfg)
	if !errors.Is(err, ErrAPITimeout) {
		t.Fatalf("got %v, want ErrAPITimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timeout took %s", d)
	}
	assertGone(t, h, m, "vm-hang")
}

func TestEarlyExitCleansUp(t *testing.T) {
	h := newFakeHost(t)
	h.exitEarly = true
	m := h.manager(t, nil)
	cfg := h.config(t)
	cfg.ID = "vm-exit"
	_, err := m.Boot(ctx, cfg)
	if err == nil || !strings.Contains(err.Error(), "exited before its API came up") || !strings.Contains(err.Error(), "bad config, exiting") {
		t.Fatalf("got %v", err)
	}
	assertGone(t, h, m, "vm-exit")
}

func TestLiveCallFailures(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, func(o *Options) { o.APITimeout = 100 * time.Millisecond })

	// An error answer has no side effects in Firecracker: the VM keeps running.
	h.failStatus["PATCH /vm"] = 400
	v, err := m.Boot(ctx, h.config(t))
	must(t, err)
	var ae *APIError
	if err := v.Pause(ctx); !errors.As(err, &ae) {
		t.Errorf("pause: got %v, want APIError", err)
	}
	if v.State() != Running {
		t.Errorf("state after a refused pause = %s", v.State())
	}
	must(t, v.Stop())

	// A hung VMM is killed and cleaned up.
	delete(h.failStatus, "PATCH /vm")
	h.hang["PATCH /vm"] = true
	v, err = m.Boot(ctx, h.config(t))
	must(t, err)
	if err := v.Pause(ctx); !errors.Is(err, ErrAPITimeout) {
		t.Errorf("pause: got %v, want ErrAPITimeout", err)
	}
	if v.State() != Stopped {
		t.Errorf("state after a hung pause = %s, want stopped", v.State())
	}
	assertGone(t, h, m, v.ID())
	must(t, v.Stop()) // idempotent
}

func TestStopIsIdempotent(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	v, err := m.Boot(ctx, h.config(t))
	must(t, err)
	must(t, v.Stop())
	must(t, v.Stop())
	if v.State() != Stopped {
		t.Errorf("state = %s", v.State())
	}
	assertGone(t, h, m, v.ID())
	if err := v.Pause(ctx); !errors.Is(err, ErrState) {
		t.Errorf("pause after stop: got %v", err)
	}
}

func TestDuplicateIDRefused(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	cfg := h.config(t)
	cfg.ID = "vm-dup"
	v, err := m.Boot(ctx, cfg)
	must(t, err)
	defer v.Stop()
	if _, err := m.Boot(ctx, cfg); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second boot with the same id: got %v", err)
	}
	if v.State() != Running {
		t.Errorf("the refused duplicate disturbed the first VM: %s", v.State())
	}
}

func TestSocketPathLimit(t *testing.T) {
	h := newFakeHost(t)
	long := filepath.Join(h.dataDir, strings.Repeat("d", 80))
	m := h.manager(t, func(o *Options) { o.DataDir = long })
	cfg := h.config(t)
	if _, err := m.Boot(ctx, cfg); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("got %v", err)
	}
}

func TestReapLeftovers(t *testing.T) {
	h := newFakeHost(t)
	m := h.manager(t, nil)
	id := "vm-orphan"
	l := layout{h.dataDir, id}
	must(t, os.MkdirAll(filepath.Join(l.chroot(), "dev"), 0o755))
	must(t, os.WriteFile(filepath.Join(l.chroot(), "firecracker.pid"), []byte("4194000\n"), 0o644))
	must(t, os.MkdirAll(l.runDir(), 0o700))
	cg := m.cg.path(id)
	must(t, os.MkdirAll(cg, 0o755))
	must(t, os.WriteFile(filepath.Join(cg, "cgroup.procs"), nil, 0o644))
	// Record the kill request before remove deletes the directory.
	var killed string
	m.cg.rmdir = func(p string) error {
		b, _ := os.ReadFile(filepath.Join(p, "cgroup.kill"))
		killed = string(b)
		return os.RemoveAll(p)
	}

	must(t, m.Reap(id))
	if killed != "1" {
		t.Errorf("cgroup.kill = %q, want 1", killed)
	}
	assertGone(t, h, m, id)
	must(t, m.Reap(id)) // nothing left: still fine
	if err := m.Reap("../etc"); err == nil {
		t.Errorf("reap accepted a path as id")
	}
}

func TestEnsureParent(t *testing.T) {
	h := newFakeHost(t)
	cg := cgroups{root: h.cgroupRoot, parent: "forkbox", rmdir: os.RemoveAll}
	must(t, cg.ensureParent())
	readEq(t, filepath.Join(h.cgroupRoot, "forkbox", "cgroup.subtree_control"), "+cpu +memory")
	// Already delegated: nothing is written.
	must(t, os.WriteFile(filepath.Join(h.cgroupRoot, "forkbox", "cgroup.subtree_control"), []byte("cpu memory\n"), 0o644))
	must(t, cg.ensureParent())
	readEq(t, filepath.Join(h.cgroupRoot, "forkbox", "cgroup.subtree_control"), "cpu memory\n")

	must(t, os.WriteFile(filepath.Join(h.cgroupRoot, "cgroup.subtree_control"), []byte("cpu io\n"), 0o644))
	if err := cg.ensureParent(); err == nil || !strings.Contains(err.Error(), "memory controller") {
		t.Errorf("root without memory: got %v", err)
	}
}

func TestNewManagerNeedsAbsoluteDataDir(t *testing.T) {
	if _, err := NewManager(Options{DataDir: "relative"}); err == nil {
		t.Error("relative data dir accepted")
	}
}

func checkBody(t *testing.T, r fakeReq, want map[string]any) {
	t.Helper()
	for k, w := range want {
		if g, ok := r.Body[k]; !ok || !reflect.DeepEqual(g, w) {
			b, _ := json.Marshal(r.Body)
			t.Errorf("%s %s: %s = %v, want %v (body %s)", r.Method, r.Path, k, g, w, b)
		}
	}
}

func lastReq(f *fakeFC) fakeReq {
	r := f.configRequests()
	return r[len(r)-1]
}

func sameFile(t *testing.T, a, b string) {
	t.Helper()
	sa, err := os.Stat(a)
	must(t, err)
	sb, err := os.Stat(b)
	must(t, err)
	if !os.SameFile(sa, sb) {
		t.Errorf("%s is not a hard link of %s", b, a)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	must(t, err)
	return string(b)
}

func readEq(t *testing.T, p, want string) {
	t.Helper()
	if got := readFile(t, p); got != want {
		t.Errorf("%s = %q, want %q", p, got, want)
	}
}

func assertSteps(t *testing.T, v *VM, want ...string) {
	t.Helper()
	var got []string
	for _, tm := range v.Timings() {
		got = append(got, tm.Step)
		if tm.Duration <= 0 {
			t.Errorf("step %s has duration %s", tm.Step, tm.Duration)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("timing steps = %v, want %v", got, want)
	}
}
