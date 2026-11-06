// Package sandbox creates and destroys sandboxes: a VM restored from a
// template's base snapshot, with its own reflinked disk, made distinct from
// its siblings by the guest agent, and recorded in the store.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/internal/storage"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

// DiskFile is a sandbox's root disk inside its directory.
const DiskFile = "rootfs.ext4"

// Manager owns sandboxes on this host.
type Manager struct {
	DataDir string
	VMs     *vm.Manager
	Store   *store.Store
	// AgentTimeout bounds the wait for the agent after resume (default 30 s).
	AgentTimeout time.Duration
	Log          *log.Logger

	mu  sync.Mutex
	vms map[string]*vm.VM // VMs started by this process
}

// New returns a manager.
func New(dataDir string, vms *vm.Manager, st *store.Store) *Manager {
	return &Manager{DataDir: dataDir, VMs: vms, Store: st, Log: log.New(io.Discard, "", 0), vms: map[string]*vm.VM{}}
}

// Dir is a sandbox's directory, <data>/sandboxes/<id>.
func Dir(dataDir, id string) string { return filepath.Join(dataDir, "sandboxes", id) }

// CreateOptions describe a new sandbox.
type CreateOptions struct {
	Template string        // name or id
	TTL      time.Duration // 0: lives until destroyed
	ID       string        // optional; generated when empty
}

// Timings of one Create, for benchmarks.
type Timings struct {
	Disk, Restore, Resume, Agent, Identity, Total time.Duration
}

// Created is the result of Create.
type Created struct {
	*store.Sandbox
	Timings  Timings
	Identity *Identity
}

// Identity is what the agent did to make the clone unique.
type Identity struct {
	MachineID   string
	ClockStep   time.Duration
	Generation  uint64
	Actions     []string
	AgentVer    string
	VMTimings   []vm.Timing
	TemplateID  string
	TemplateMem string
}

// ErrNoReflink means the data directory cannot make instant disk copies.
var ErrNoReflink = errors.New("the data directory's filesystem cannot reflink: put it on XFS (mkfs.xfs -m reflink=1) or btrfs; `forkbox doctor` checks this")

// Create restores a sandbox from a template's base snapshot.
//
//   - Disk: a reflink copy of the template's read-only rootfs.
//   - Memory: the File backend on the template's read-only memory file.
//     Firecracker maps it MAP_PRIVATE (vstate/memory.rs, snapshot_file), so
//     the kernel shares unchanged pages between all sandboxes of a template
//     and copies a page only when a sandbox writes it.
//   - Then resume, wait for the agent, and send it the sandbox's identity.
func (m *Manager) Create(ctx context.Context, o CreateOptions) (*Created, error) {
	start := time.Now()
	tpl, err := template.Get(m.DataDir, o.Template)
	if err != nil {
		return nil, err
	}
	id := o.ID
	if id == "" {
		b := make([]byte, 6)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		id = "sb-" + hex.EncodeToString(b)
	}
	if !vm.ValidID(id) {
		return nil, fmt.Errorf("sandbox id %q: use 1-32 lowercase letters, digits and hyphens", id)
	}
	dir := Dir(m.DataDir, id)
	rec := &store.Sandbox{
		ID: id, Template: tpl.ID, State: store.StateCreating, Dir: dir,
		Jail: vm.JailDir(m.DataDir, id), Cgroup: m.VMs.CgroupPath(id),
		Vsock: vm.VsockSocketPath(m.DataDir, id), TTL: o.TTL,
	}
	// Recorded before anything is created, so a reaper can always find it.
	if err := m.Store.InsertSandbox(ctx, rec); err != nil {
		return nil, err
	}
	c, err := m.create(ctx, tpl, rec)
	if err != nil {
		cerr := m.cleanup(context.WithoutCancel(ctx), id, dir)
		_ = m.Store.DeleteSandbox(context.WithoutCancel(ctx), id)
		return nil, errors.Join(fmt.Errorf("create %s from %s: %w", id, tpl.Name, err), cerr)
	}
	c.Timings.Total = time.Since(start)
	m.Log.Printf("sandbox %s from %s in %s (disk %s, restore %s, agent %s)", id, tpl.Name,
		c.Timings.Total.Round(time.Millisecond), c.Timings.Disk.Round(time.Microsecond),
		c.Timings.Restore.Round(time.Microsecond), c.Timings.Agent.Round(time.Millisecond))
	return c, nil
}

func (m *Manager) create(ctx context.Context, tpl *template.Template, rec *store.Sandbox) (*Created, error) {
	c := &Created{Sandbox: rec}
	t := time.Now()
	if err := os.MkdirAll(rec.Dir, 0o700); err != nil {
		return nil, err
	}
	disk := filepath.Join(rec.Dir, DiskFile)
	if err := storage.Reflink(tpl.Rootfs(), disk); err != nil {
		if errors.Is(err, storage.ErrNoReflink) {
			return nil, fmt.Errorf("%w (%v)", ErrNoReflink, err)
		}
		return nil, err
	}
	c.Timings.Disk = time.Since(t)

	t = time.Now()
	mem := filepath.Join(tpl.SnapshotDir(), vm.SnapshotMemFile)
	v, err := m.VMs.Restore(ctx, tpl.SnapshotDir(), vm.MemBackend{Kind: vm.MemFile, Path: mem},
		vm.Config{ID: rec.ID, Rootfs: vm.Drive{Path: disk}})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.vms[rec.ID] = v
	m.mu.Unlock()
	c.Timings.Restore = time.Since(t)

	t = time.Now()
	if err := v.Resume(ctx); err != nil {
		return nil, err
	}
	c.Timings.Resume = time.Since(t)

	t = time.Now()
	ac := m.Agent(rec.ID)
	at := m.AgentTimeout
	if at <= 0 {
		at = 30 * time.Second
	}
	actx, cancel := context.WithTimeout(ctx, at)
	pong, err := ac.WaitReady(actx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("agent did not answer within %s: %w", at, err)
	}
	c.Timings.Agent = time.Since(t)

	t = time.Now()
	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return nil, err
	}
	r, err := ac.Restored(ctx, rec.ID, rec.ID, entropy)
	if err != nil {
		return nil, fmt.Errorf("agent restored: %w", err)
	}
	c.Timings.Identity = time.Since(t)
	c.Identity = &Identity{
		MachineID: r.MachineId, ClockStep: time.Duration(r.ClockStepNs), Generation: r.Generation,
		Actions: r.Actions, AgentVer: pong.AgentVersion, VMTimings: v.Timings(), TemplateID: tpl.ID, TemplateMem: mem,
	}
	m.Log.Printf("sandbox %s identity: %v", rec.ID, r.Actions)
	rec.Pid = v.Pid()
	rec.State = store.StateRunning
	if err := m.Store.SetState(ctx, rec.ID, store.StateRunning, rec.Pid, ""); err != nil {
		return nil, err
	}
	return c, nil
}

// Agent is a client for a sandbox's guest agent.
func (m *Manager) Agent(id string) *agentclient.Client {
	return agentclient.New(vm.VsockSocketPath(m.DataDir, id), agentwire.Port)
}

// VM returns the VM of a sandbox this process started, or nil.
func (m *Manager) VM(id string) *vm.VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vms[id]
}

// Destroy stops a sandbox's VM and removes its jail, cgroup, disk and
// record. It works whether or not this process started the VM.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	rec, err := m.Store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	_ = m.Store.SetState(ctx, id, store.StateDestroying, rec.Pid, "")
	if err := m.cleanup(ctx, id, rec.Dir); err != nil {
		_ = m.Store.SetState(ctx, id, store.StateFailed, 0, err.Error())
		return err
	}
	return m.Store.DeleteSandbox(ctx, id)
}

func (m *Manager) cleanup(_ context.Context, id, dir string) error {
	m.mu.Lock()
	v := m.vms[id]
	delete(m.vms, id)
	m.mu.Unlock()
	var errs []error
	if v != nil {
		errs = append(errs, v.Stop())
	} else {
		errs = append(errs, m.VMs.Reap(id))
	}
	if err := os.RemoveAll(dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
