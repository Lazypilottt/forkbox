package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Lazypilottt/forkbox/internal/datadir"
)

// Defaults for Options.
const (
	// DefaultUID and DefaultGID are the unprivileged ids Firecracker runs as
	// inside every jail. 70000 sits above the Debian static ranges and below
	// the usual /etc/subuid ranges (100000+), so it maps to no real user.
	DefaultUID = 70000
	DefaultGID = 70000

	DefaultCgroupRoot   = "/sys/fs/cgroup"
	DefaultCgroupParent = "forkbox"

	DefaultStartTimeout    = 5 * time.Second
	DefaultAPITimeout      = 5 * time.Second
	DefaultSnapshotTimeout = 2 * time.Minute
	DefaultCleanupTimeout  = 5 * time.Second
)

// Options configures a Manager. Zero values take the defaults above.
type Options struct {
	DataDir string // required, absolute

	// FirecrackerVersion is the pinned release (versions.yaml). It is
	// recorded in every snapshot, and Restore refuses snapshots taken by a
	// different version.
	FirecrackerVersion string

	Jailer      string // default <data>/bin/jailer
	Firecracker string // default <data>/bin/firecracker

	UID, GID int

	CgroupRoot   string
	CgroupParent string

	StartTimeout    time.Duration // until the API socket answers
	APITimeout      time.Duration // every other API call
	SnapshotTimeout time.Duration // snapshot create and load
	CleanupTimeout  time.Duration // waiting for processes to die

	// Test hooks.
	launch launcher
	chown  func(path string, uid, gid int) error
	rmdir  func(path string) error
}

// Manager boots and restores VMs. It holds no per-VM state, so any number of
// VMs can run at once; the daemon (Task 6) tracks them.
type Manager struct {
	o  Options
	cg cgroups
}

// NewManager fills in defaults and checks the options.
func NewManager(o Options) (*Manager, error) {
	if !filepath.IsAbs(o.DataDir) {
		return nil, fmt.Errorf("vm: data dir %q must be an absolute path", o.DataDir)
	}
	if o.Jailer == "" {
		o.Jailer = datadir.Bin(o.DataDir, "jailer")
	}
	if o.Firecracker == "" {
		o.Firecracker = datadir.Bin(o.DataDir, "firecracker")
	}
	if o.UID == 0 {
		o.UID = DefaultUID
	}
	if o.GID == 0 {
		o.GID = DefaultGID
	}
	if o.CgroupRoot == "" {
		o.CgroupRoot = DefaultCgroupRoot
	}
	if o.CgroupParent == "" {
		o.CgroupParent = DefaultCgroupParent
	}
	setDur := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	setDur(&o.StartTimeout, DefaultStartTimeout)
	setDur(&o.APITimeout, DefaultAPITimeout)
	setDur(&o.SnapshotTimeout, DefaultSnapshotTimeout)
	setDur(&o.CleanupTimeout, DefaultCleanupTimeout)
	if o.launch == nil {
		o.launch = execLauncher
	}
	if o.chown == nil {
		o.chown = os.Lchown
	}
	if o.rmdir == nil {
		o.rmdir = os.Remove
	}
	return &Manager{o: o, cg: cgroups{root: o.CgroupRoot, parent: o.CgroupParent, rmdir: o.rmdir}}, nil
}

// Boot starts a fresh VM through the jailer: it builds the jail, configures
// the machine (with dirty-page tracking on), and issues InstanceStart. On any
// failure the VM's process, jail and cgroup are removed before returning.
func (m *Manager) Boot(ctx context.Context, cfg Config) (*VM, error) {
	if err := cfg.validateBoot(); err != nil {
		return nil, err
	}
	v, err := m.newVM(&cfg)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := v.prepareJail(func() error {
		return v.linkInto(cfg.Kernel, jailKernel)
	}); err != nil {
		return nil, v.abort(err)
	}
	if err := v.launch(ctx); err != nil {
		return nil, v.abort(err)
	}
	v.record(StepJail, time.Since(start))

	t := time.Now()
	type put struct {
		path string
		body any
	}
	reqs := []put{
		{"/machine-config", machineConfig{VCPUCount: cfg.VCPUs, MemSizeMiB: cfg.MemMiB, SMT: false, TrackDirtyPages: true}},
		{"/boot-source", bootSource{KernelImagePath: jailKernel, BootArgs: cfg.kernelArgs()}},
		{"/drives/rootfs", drive{DriveID: "rootfs", PathOnHost: jailRootfs, IsRootDevice: true, IsReadOnly: cfg.Rootfs.ReadOnly}},
	}
	if cfg.Vsock != nil {
		reqs = append(reqs, put{"/vsock", vsockDev{GuestCID: cfg.Vsock.GuestCID, UDSPath: jailVsock}})
	}
	if cfg.Net != nil {
		reqs = append(reqs, put{"/network-interfaces/" + cfg.Net.IfaceID,
			netIface{IfaceID: cfg.Net.IfaceID, HostDevName: cfg.Net.HostDev, GuestMAC: cfg.Net.GuestMAC}})
	}
	for _, r := range reqs {
		if err := v.api.do(ctx, m.o.APITimeout, "PUT", r.path, r.body, nil); err != nil {
			return nil, v.abort(err)
		}
	}
	v.record(StepConfigure, time.Since(t))

	t = time.Now()
	if err := v.api.do(ctx, m.o.APITimeout, "PUT", "/actions", action{ActionType: "InstanceStart"}, nil); err != nil {
		return nil, v.abort(err)
	}
	v.record(StepStart, time.Since(t))
	v.record(StepBoot, time.Since(start))
	v.state = Running
	return v, nil
}

// MemBackendKind selects how a restored VM's memory is served.
type MemBackendKind string

const (
	// MemFile maps the snapshot memory file privately (copy-on-write).
	MemFile MemBackendKind = "File"
	// MemUffd serves page faults from a userfaultfd page server (Task 12).
	MemUffd MemBackendKind = "Uffd"
)

// MemBackend is the memory source for Restore.
type MemBackend struct {
	Kind MemBackendKind
	// Path is the memory file for MemFile; empty means the snapshot's own
	// mem file. The file must never change while a VM uses it.
	Path string
}

// Restore starts a VM from a full snapshot in a new jail. The snapshot's
// drive and vsock paths are the same in every jail, so cfg.Rootfs is linked
// to the path the snapshot expects. VCPUs and MemMiB default to the
// snapshot's; they only set the cgroup limits. The VM is left Paused, with
// dirty-page tracking on; call Resume to run it.
func (m *Manager) Restore(ctx context.Context, snapshotDir string, mem MemBackend, cfg Config) (*VM, error) {
	snap, err := ReadSnapshot(snapshotDir)
	if err != nil {
		return nil, err
	}
	if snap.Type != SnapshotFull {
		return nil, fmt.Errorf("vm: restore %s: a %s snapshot is not resumable on its own; merge it into a full snapshot first", snapshotDir, snap.Type)
	}
	if m.o.FirecrackerVersion != "" && snap.FirecrackerVersion != "" && snap.FirecrackerVersion != m.o.FirecrackerVersion {
		return nil, fmt.Errorf("vm: restore %s: snapshot was taken by Firecracker %s, this host pins %s", snapshotDir, snap.FirecrackerVersion, m.o.FirecrackerVersion)
	}
	if cfg.VCPUs == 0 {
		cfg.VCPUs = snap.VCPUs
	}
	if cfg.MemMiB == 0 {
		cfg.MemMiB = snap.MemMiB
	}
	if cfg.VCPUs != snap.VCPUs || cfg.MemMiB != snap.MemMiB {
		return nil, configErr("restore: config asks for %d vCPUs and %d MiB, the snapshot has %d and %d", cfg.VCPUs, cfg.MemMiB, snap.VCPUs, snap.MemMiB)
	}
	if err := cfg.validateLimits(); err != nil {
		return nil, err
	}
	switch mem.Kind {
	case MemFile, "":
		if mem.Path == "" {
			mem.Path = filepath.Join(snapshotDir, SnapshotMemFile)
		}
	case MemUffd:
		return nil, errors.New("vm: the uffd memory backend is not built yet (Task 12)")
	default:
		return nil, configErr("unknown memory backend %q", mem.Kind)
	}

	v, err := m.newVM(&cfg)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := v.prepareJail(func() error {
		if err := os.Mkdir(v.l.host(jailRestDir), 0o755); err != nil {
			return err
		}
		if err := v.linkInto(filepath.Join(snapshotDir, SnapshotStateFile), jailRestVM); err != nil {
			return err
		}
		return v.linkInto(mem.Path, jailRestMem)
	}); err != nil {
		return nil, v.abort(err)
	}
	if err := v.launch(ctx); err != nil {
		return nil, v.abort(err)
	}
	v.record(StepRestoreJail, time.Since(start))

	t := time.Now()
	load := snapshotLoad{
		SnapshotPath:    jailRestVM,
		MemBackend:      memBackend{BackendType: string(MemFile), BackendPath: jailRestMem},
		TrackDirtyPages: true, // not saved in snapshots; must be set again
		ResumeVM:        false,
	}
	if err := v.api.do(ctx, m.o.SnapshotTimeout, "PUT", "/snapshot/load", load, nil); err != nil {
		// Firecracker exits after a failed load.
		return nil, v.abort(err)
	}
	v.record(StepRestoreLoad, time.Since(t))
	v.record(StepRestore, time.Since(start))
	v.state = Paused
	return v, nil
}

// Reap removes everything a VM may have left behind, whether or not its
// owner is still alive: processes in its cgroup, the cgroup, the jail and
// the run directory. It is safe to call on an id that left nothing behind.
func (m *Manager) Reap(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("vm: reap: invalid id %q", id)
	}
	l := layout{m.o.DataDir, id}
	var errs []error
	// Kill by cgroup first. If the jailer died before creating the cgroup,
	// fall back to the pid file it writes in the jail root.
	if err := m.cg.remove(id, m.o.CleanupTimeout); err != nil {
		errs = append(errs, err)
	}
	if pid := readPidFile(filepath.Join(l.chroot(), execFileName+".pid")); pid > 0 && isJailedFirecracker(pid, id) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if err := os.RemoveAll(l.jailDir()); err != nil {
		errs = append(errs, fmt.Errorf("removing jail: %w", err))
	}
	if err := os.RemoveAll(l.runDir()); err != nil {
		errs = append(errs, fmt.Errorf("removing run dir: %w", err))
	}
	return errors.Join(errs...)
}

func (m *Manager) newVM(cfg *Config) (*VM, error) {
	if cfg.ID == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		cfg.ID = "vm-" + hex.EncodeToString(b)
	}
	l := layout{m.o.DataDir, cfg.ID}
	if n := len(l.host(jailAPISock)); n > maxSunPath {
		return nil, fmt.Errorf("vm: API socket path %s is %d bytes, over the %d-byte limit; use a shorter data dir", l.host(jailAPISock), n, maxSunPath)
	}
	if _, err := os.Lstat(l.jailDir()); err == nil {
		return nil, fmt.Errorf("vm: %s already exists (%s)", cfg.ID, l.jailDir())
	}
	return &VM{m: m, cfg: *cfg, id: cfg.ID, l: l, state: Starting}, nil
}

// jailerArgs builds the jailer command line. Seccomp stays at Firecracker's
// default (on): no --no-seccomp or --seccomp-filter is ever passed.
func (m *Manager) jailerArgs(id string, cfg *Config) []string {
	args := []string{
		m.o.Jailer,
		"--id", id,
		"--exec-file", m.o.Firecracker,
		"--uid", strconv.Itoa(m.o.UID),
		"--gid", strconv.Itoa(m.o.GID),
		"--chroot-base-dir", JailBase(m.o.DataDir),
		// The jailer defaults to cgroup v1.
		"--cgroup-version", "2",
		"--parent-cgroup", m.o.CgroupParent,
		"--cgroup", "cpu.max=" + cfg.cpuMax(),
		"--cgroup", "memory.max=" + strconv.FormatInt(cfg.memoryMax(), 10),
	}
	if cfg.NetNS != "" {
		args = append(args, "--netns", cfg.NetNS)
	}
	return append(args, "--", "--api-sock", jailAPISock)
}

func readPidFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// isJailedFirecracker guards against pid reuse: the jailer passes --id=<id>
// to Firecracker, so its command line names the VM.
func isJailedFirecracker(pid int, id string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	for _, a := range splitNul(b) {
		if a == "--id="+id {
			return true
		}
	}
	return false
}

func splitNul(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}
