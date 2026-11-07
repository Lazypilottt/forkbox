package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// State is a VM's lifecycle state.
type State string

const (
	Starting State = "starting"
	Running  State = "running"
	Paused   State = "paused"
	Stopped  State = "stopped"
)

// ErrState means the call is not allowed in the VM's current state. The VM
// is left as it was.
var ErrState = errors.New("vm: wrong state")

// Step names for Timings.
const (
	StepJail         = "jail"      // build the jail, spawn the jailer, API ready
	StepConfigure    = "configure" // machine, boot source and devices
	StepStart        = "start"     // InstanceStart
	StepBoot         = "boot"      // whole Boot call
	StepPause        = "pause"
	StepResume       = "resume"
	StepSnapshotFull = "snapshot-full"
	StepSnapshotDiff = "snapshot-diff"
	StepRestoreJail  = "restore-jail" // as StepJail, for Restore
	StepRestoreLoad  = "restore-load" // PUT /snapshot/load
	StepRestore      = "restore"      // whole Restore call
	StepStop         = "stop"
)

// Timing is how long one step took, for benchmarks.
type Timing struct {
	Step     string
	Duration time.Duration
}

// VM is one jailed Firecracker microVM. Its methods are safe for concurrent
// use; they run one at a time.
type VM struct {
	m   *Manager
	cfg Config
	id  string
	l   layout

	mu      sync.Mutex
	state   State
	proc    process
	api     *client
	timings []Timing
}

// ID is the VM's id (jailer id and cgroup name).
func (v *VM) ID() string { return v.id }

// State is the VM's current state.
func (v *VM) State() State {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

// Pid is the Firecracker process id, or 0 before launch.
func (v *VM) Pid() int {
	if v.proc == nil {
		return 0
	}
	return v.proc.Pid()
}

// ConsolePath is the file receiving the guest serial console and
// Firecracker's own log. It is removed by Stop.
func (v *VM) ConsolePath() string { return v.l.consoleLog() }

// VsockPath is the host Unix socket of the VM's vsock device.
func (v *VM) VsockPath() string { return v.l.host(jailVsock) }

// CgroupPath is the VM's cgroup v2 directory.
func (v *VM) CgroupPath() string { return v.m.cg.path(v.id) }

// JailDir is the VM's jail directory (<data>/jail/firecracker/<id>).
func (v *VM) JailDir() string { return v.l.jailDir() }

// Timings returns the duration of every step so far, in order.
func (v *VM) Timings() []Timing {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]Timing(nil), v.timings...)
}

func (v *VM) record(step string, d time.Duration) {
	v.timings = append(v.timings, Timing{step, d})
}

// Pause pauses the vCPUs.
func (v *VM) Pause(ctx context.Context) error {
	return v.setState(ctx, Running, Paused, "Paused", StepPause)
}

// Resume resumes a paused VM.
func (v *VM) Resume(ctx context.Context) error {
	return v.setState(ctx, Paused, Running, "Resumed", StepResume)
}

func (v *VM) setState(ctx context.Context, from, to State, fc, step string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != from {
		return fmt.Errorf("%w: %s needs a %s VM, %s is %s", ErrState, step, from, v.id, v.state)
	}
	t := time.Now()
	if err := v.call(ctx, v.m.o.APITimeout, "PATCH", "/vm", vmState{State: fc}); err != nil {
		return err
	}
	v.record(step, time.Since(t))
	v.state = to
	return nil
}

// SnapshotType is Full or Diff, as in Firecracker's API.
type SnapshotType string

const (
	SnapshotFull SnapshotType = "Full"
	SnapshotDiff SnapshotType = "Diff"
)

// Snapshot describes a snapshot directory: vmstate, mem and snapshot.json.
type Snapshot struct {
	Dir                string       `json:"-"`
	Type               SnapshotType `json:"type"`
	VMID               string       `json:"vm_id"`
	FirecrackerVersion string       `json:"firecracker_version"`
	VCPUs              int          `json:"vcpus"`
	MemMiB             int          `json:"mem_mib"`
	CreatedAt          time.Time    `json:"created_at"`
}

// ReadSnapshot reads the metadata of a snapshot directory.
func ReadSnapshot(dir string) (*Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(dir, SnapshotMetaFile))
	if err != nil {
		return nil, fmt.Errorf("vm: snapshot %s: %w", dir, err)
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("vm: snapshot %s: %w", dir, err)
	}
	if s.Type != SnapshotFull && s.Type != SnapshotDiff {
		return nil, fmt.Errorf("vm: snapshot %s: unknown type %q", dir, s.Type)
	}
	s.Dir = dir
	return &s, nil
}

// SnapshotFull writes a full snapshot of a paused VM into dir, which must be
// new or empty and on the data directory's filesystem.
func (v *VM) SnapshotFull(ctx context.Context, dir string) (*Snapshot, error) {
	return v.snapshot(ctx, dir, SnapshotFull, StepSnapshotFull)
}

// SnapshotDiff writes the memory pages dirtied since the last snapshot (or
// since boot or restore) as a sparse mem file, plus the VM state.
func (v *VM) SnapshotDiff(ctx context.Context, dir string) (*Snapshot, error) {
	return v.snapshot(ctx, dir, SnapshotDiff, StepSnapshotDiff)
}

func (v *VM) snapshot(ctx context.Context, dir string, typ SnapshotType, step string) (*Snapshot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state != Paused {
		return nil, fmt.Errorf("%w: a snapshot needs a paused VM, %s is %s", ErrState, v.id, v.state)
	}
	if err := ensureEmptyDir(dir); err != nil {
		return nil, fmt.Errorf("vm: snapshot dir: %w", err)
	}
	// Firecracker truncates existing files, so the in-jail targets must never
	// be links to anything else. They are always moved out right away.
	for _, p := range []string{jailSnapVM, jailSnapMem} {
		_ = os.Remove(v.l.host(p))
	}
	t := time.Now()
	body := snapshotCreate{SnapshotType: string(typ), SnapshotPath: jailSnapVM, MemFilePath: jailSnapMem}
	if typ == SnapshotDiff {
		// A diff is merged into a layer and deleted right away, so it need
		// not reach the disk: skipping fsync keeps the pause short
		// (snapshot-support.md, "Syncing snapshot files"). Full snapshots,
		// which templates keep, stay synced.
		no := false
		body.SyncSnapshotFiles = &no
	}
	if err := v.call(ctx, v.m.o.SnapshotTimeout, "PUT", "/snapshot/create", body); err != nil {
		return nil, err
	}
	moves := []struct{ from, to string }{
		{v.l.host(jailSnapVM), filepath.Join(dir, SnapshotStateFile)},
		{v.l.host(jailSnapMem), filepath.Join(dir, SnapshotMemFile)},
	}
	for _, mv := range moves {
		if err := os.Rename(mv.from, mv.to); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				err = fmt.Errorf("%w (the snapshot dir must be on the data directory's filesystem)", err)
			}
			return nil, fmt.Errorf("vm: snapshot: %w", err)
		}
		// Owned by root and read-only to the jail group: restored VMs map
		// the mem file, and it must never change underneath them.
		if err := v.m.o.chown(mv.to, 0, v.m.o.GID); err != nil {
			return nil, fmt.Errorf("vm: snapshot: %w", err)
		}
		if err := os.Chmod(mv.to, 0o640); err != nil {
			return nil, fmt.Errorf("vm: snapshot: %w", err)
		}
	}
	s := &Snapshot{
		Dir:                dir,
		Type:               typ,
		VMID:               v.id,
		FirecrackerVersion: v.m.o.FirecrackerVersion,
		VCPUs:              v.cfg.VCPUs,
		MemMiB:             v.cfg.MemMiB,
		CreatedAt:          time.Now().UTC(),
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, SnapshotMetaFile), append(b, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("vm: snapshot: %w", err)
	}
	v.record(step, time.Since(t))
	return s, nil
}

// Stop kills Firecracker and removes the VM's jail, cgroup and run
// directory (including the console log). It is idempotent.
func (v *VM) Stop() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == Stopped {
		return nil
	}
	t := time.Now()
	err := v.cleanup()
	v.record(StepStop, time.Since(t))
	return err
}

// call is an API call on a live VM. A Firecracker error answer has no side
// effects (snapshot-support.md), so the VM keeps running. A timeout or a
// broken connection means the VMM is unhealthy: the VM is killed and
// cleaned up, so nothing leaks.
func (v *VM) call(ctx context.Context, timeout time.Duration, method, path string, body any) error {
	err := v.api.do(ctx, timeout, method, path, body, nil)
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return err
	}
	cerr := v.cleanup()
	return errors.Join(fmt.Errorf("%w (VM %s was stopped and cleaned up)", err, v.id), cerr)
}

// cleanup kills the process and reaps the VM's resources. Callers hold mu.
func (v *VM) cleanup() error {
	var errs []error
	if v.proc != nil {
		_ = v.proc.Kill()
		select {
		case <-v.proc.Done():
		case <-time.After(v.m.o.CleanupTimeout):
			errs = append(errs, fmt.Errorf("firecracker pid %d did not exit within %s", v.proc.Pid(), v.m.o.CleanupTimeout))
		}
	}
	if v.api != nil {
		v.api.close()
	}
	if err := v.m.Reap(v.id); err != nil {
		errs = append(errs, err)
	}
	v.state = Stopped
	return errors.Join(errs...)
}

// abort cleans up after a failed Boot or Restore. The error carries the end
// of the console log, which usually says why Firecracker or the guest failed.
func (v *VM) abort(err error) error {
	err = fmt.Errorf("vm %s: %w", v.id, err)
	if tail := tailFile(v.l.consoleLog(), 2048); tail != "" {
		err = fmt.Errorf("%w\nconsole:\n%s", err, tail)
	}
	return errors.Join(err, v.cleanup())
}

// prepareJail creates the run directory and the jail root with the drive
// and the snapshot directory. extra adds the boot or restore inputs.
//
// Everything the jail uid must not change is root-owned: /boot, /drives and
// /restore and their files (except a writable rootfs). The jailer then
// chowns the jail root itself to the jail uid so Firecracker can create its
// sockets there.
func (v *VM) prepareJail(extra func() error) error {
	if err := os.MkdirAll(v.l.runDir(), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(v.l.chroot(), 0o755); err != nil {
		return err
	}
	for _, d := range []string{"/boot", "/drives"} {
		if err := os.Mkdir(v.l.host(d), 0o755); err != nil {
			return err
		}
	}
	if err := v.linkInto(v.cfg.Rootfs.Path, jailRootfs); err != nil {
		return err
	}
	if !v.cfg.Rootfs.ReadOnly {
		if err := v.m.o.chown(v.l.host(jailRootfs), v.m.o.UID, v.m.o.GID); err != nil {
			return fmt.Errorf("rootfs: %w", err)
		}
		// A reflink of a read-only template keeps its 0444 mode.
		if err := os.Chmod(v.l.host(jailRootfs), 0o600); err != nil {
			return fmt.Errorf("rootfs: %w", err)
		}
	}
	snap := v.l.host(jailSnapDir)
	if err := os.Mkdir(snap, 0o700); err != nil {
		return err
	}
	if err := v.m.o.chown(snap, v.m.o.UID, v.m.o.GID); err != nil {
		return err
	}
	return extra()
}

// linkInto hard-links a host file into the jail. Links are instant and
// share the page cache; they need the file on the data dir's filesystem.
func (v *VM) linkInto(src, inJail string) error {
	if err := os.Link(src, v.l.host(inJail)); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("%s must be on the same filesystem as %s to be linked into the jail: %w", src, v.m.o.DataDir, err)
		}
		return err
	}
	return nil
}

// launch spawns the jailer and waits until Firecracker's API answers.
func (v *VM) launch(ctx context.Context) error {
	if err := v.m.cg.ensureParent(); err != nil {
		return err
	}
	console, err := os.OpenFile(v.l.consoleLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	p, err := v.m.o.launch(ctx, v.m.jailerArgs(v.id, &v.cfg), console)
	console.Close() // the child has its own copy
	if err != nil {
		return fmt.Errorf("starting jailer: %w", err)
	}
	v.proc = p
	v.api = newClient(v.l.host(jailAPISock))

	deadline := time.Now().Add(v.m.o.StartTimeout)
	probe := min(v.m.o.APITimeout, 250*time.Millisecond)
	for {
		select {
		case <-p.Done():
			return errors.New("firecracker exited before its API came up")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var info instanceInfo
		if err := v.api.do(ctx, probe, "GET", "/", nil, &info); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: API socket %s not answering after %s", ErrAPITimeout, v.l.host(jailAPISock), v.m.o.StartTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ensureEmptyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s is not empty", dir)
		}
		return err
	}
	return nil
}

func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(st.Size()-n, 0)
	b := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		return ""
	}
	return string(b)
}

// WriteSnapshotMeta writes s as dir's snapshot.json, so Restore accepts the
// directory (for example a merged fork layer, which is a full snapshot).
func WriteSnapshotMeta(dir string, s *Snapshot) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, SnapshotMetaFile), append(b, '\n'), 0o644)
}
