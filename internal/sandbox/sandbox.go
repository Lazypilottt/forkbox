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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/internal/netns"
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
	Net     *netns.Manager
	// ExposeAddr is the host address port forwards listen on (default
	// 127.0.0.1; set 0.0.0.0 to publish previews).
	ExposeAddr string
	// AgentTimeout bounds the wait for the agent after resume (default 30 s).
	AgentTimeout time.Duration
	Log          *log.Logger

	mu        sync.Mutex
	vms       map[string]*vm.VM         // VMs started by this process
	listeners map[string][]net.Listener // port forwards, by sandbox
	forkLocks map[string]*sync.Mutex    // one fork at a time per parent
	gcMu      sync.Mutex
}

// New returns a manager.
func New(dataDir string, vms *vm.Manager, st *store.Store, nm *netns.Manager) *Manager {
	return &Manager{DataDir: dataDir, VMs: vms, Store: st, Net: nm, ExposeAddr: "127.0.0.1",
		Log: log.New(io.Discard, "", 0), vms: map[string]*vm.VM{}, listeners: map[string][]net.Listener{}}
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
	Disk, Network, Restore, Resume, Agent, Identity, Total time.Duration
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
		id = IDPrefix + hex.EncodeToString(b)
	}
	if !vm.ValidID(id) {
		return nil, fmt.Errorf("sandbox id %q: use 1-32 lowercase letters, digits and hyphens", id)
	}
	dir := Dir(m.DataDir, id)
	rec := &store.Sandbox{
		ID: id, Template: tpl.ID, TemplateName: tpl.Name, State: store.StateCreating, Dir: dir, BaseLayer: tpl.ID,
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
	mem := filepath.Join(tpl.SnapshotDir(), vm.SnapshotMemFile)
	if err := m.restoreInto(ctx, c, tpl.SnapshotDir(), mem, disk); err != nil {
		return nil, err
	}
	c.Identity.TemplateID, c.Identity.TemplateMem = tpl.ID, mem
	return c, nil
}

// restoreInto brings up rec's VM from a snapshot (vmstate in snapDir, the
// memory file mem, the root disk disk): network, restore, resume, agent,
// identity. Used by Create and by Fork.
func (m *Manager) restoreInto(ctx context.Context, c *Created, snapDir, mem, disk string) error {
	rec := c.Sandbox
	// The namespace's tap has the name, address and MAC the snapshot expects.
	t := time.Now()
	slot, err := m.Store.AllocSlot(ctx, rec.ID, m.Net.Slots())
	if err != nil {
		return err
	}
	if err := m.Net.Setup(ctx, rec.ID, slot); err != nil {
		return err
	}
	c.Timings.Network = time.Since(t)

	return m.restoreVM(ctx, c, snapDir, mem, disk)
}

// finishRestore resumes a restored VM, waits for its agent and gives the
// guest its identity, then marks the sandbox running.
func (m *Manager) finishRestore(ctx context.Context, c *Created, v *vm.VM) error {
	rec := c.Sandbox
	t := time.Now()
	if err := v.Resume(ctx); err != nil {
		return err
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
		return fmt.Errorf("agent did not answer within %s: %w", at, err)
	}
	c.Timings.Agent = time.Since(t)

	t = time.Now()
	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return err
	}
	r, err := ac.Restored(ctx, rec.ID, rec.ID, entropy)
	if err != nil {
		return fmt.Errorf("agent restored: %w", err)
	}
	c.Timings.Identity = time.Since(t)
	c.Identity = &Identity{
		MachineID: r.MachineId, ClockStep: time.Duration(r.ClockStepNs), Generation: r.Generation,
		Actions: r.Actions, AgentVer: pong.AgentVersion, VMTimings: v.Timings(),
	}
	m.Log.Printf("sandbox %s identity: %v", rec.ID, r.Actions)
	rec.Pid = v.Pid()
	rec.State = store.StateRunning
	return m.Store.SetState(ctx, rec.ID, store.StateRunning, rec.Pid, "")
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
	if err := m.Store.DeleteSandbox(ctx, id); err != nil {
		return err
	}
	m.gc(ctx)
	return nil
}

func (m *Manager) cleanup(ctx context.Context, id, dir string) error {
	m.mu.Lock()
	v := m.vms[id]
	delete(m.vms, id)
	ls := m.listeners[id]
	delete(m.listeners, id)
	m.mu.Unlock()
	for _, l := range ls {
		l.Close()
	}
	var errs []error
	if v != nil {
		errs = append(errs, v.Stop())
	} else {
		errs = append(errs, m.VMs.Reap(id))
	}
	// After the VM: the namespace (and its veth) goes once nothing uses it.
	errs = append(errs, m.Net.Teardown(context.WithoutCancel(ctx), id))
	if err := os.RemoveAll(dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Get returns a sandbox's record.
func (m *Manager) Get(ctx context.Context, id string) (*store.Sandbox, error) {
	return m.Store.GetSandbox(ctx, id)
}

// List returns every sandbox record.
func (m *Manager) List(ctx context.Context) ([]*store.Sandbox, error) {
	return m.Store.ListSandboxes(ctx)
}

// RecoverReport says what Recover did.
type RecoverReport struct {
	Adopted []string // running VMs kept, now driven by id
	Resumed []string // adopted VMs that were left paused (a fork cut short)
	Reaped  []string // records whose VM was gone or half-made, removed
	Orphans []string // jails, cgroups or dirs with no record, removed
}

// Recover runs when the daemon starts. A sandbox whose Firecracker process
// is still alive is adopted: everything else needs only its id (the agent
// socket and the jail paths are derived from it). A sandbox whose process
// is gone, or that was stuck creating or destroying, is reaped. Then any
// sandbox jail, cgroup or directory without a record is removed.
func (m *Manager) Recover(ctx context.Context) (*RecoverReport, error) {
	rep := &RecoverReport{}
	recs, err := m.Store.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var errs []error
	for _, r := range recs {
		known[r.ID] = true
		if r.State == store.StateRunning && m.VMs.Alive(r.Pid, r.ID) {
			rep.Adopted = append(rep.Adopted, r.ID)
			// A daemon killed mid-fork or mid-checkpoint can leave the
			// parent paused, and its diff half-written.
			if tpl, err := template.Get(m.DataDir, r.Template); err == nil {
				if v, err := m.vmFor(ctx, r, tpl); err != nil {
					errs = append(errs, fmt.Errorf("attach %s: %w", r.ID, err))
				} else if v.State() == vm.Paused {
					if err := v.Resume(ctx); err != nil {
						errs = append(errs, fmt.Errorf("resume %s: %w", r.ID, err))
					} else {
						rep.Resumed = append(rep.Resumed, r.ID)
					}
				}
			}
			diffs, _ := filepath.Glob(filepath.Join(r.Dir, "diff-*"))
			for _, d := range diffs {
				os.RemoveAll(d)
			}
			if err := m.restoreForwards(ctx, r.ID); err != nil {
				errs = append(errs, fmt.Errorf("port forwards of %s: %w", r.ID, err))
			}
			continue
		}
		if err := m.cleanup(ctx, r.ID, r.Dir); err != nil {
			errs = append(errs, fmt.Errorf("reap %s: %w", r.ID, err))
			continue
		}
		if err := m.Store.DeleteSandbox(ctx, r.ID); err != nil {
			errs = append(errs, err)
		}
		rep.Reaped = append(rep.Reaped, r.ID)
	}
	candidates := map[string]bool{}
	for _, glob := range []string{
		filepath.Join(m.DataDir, "sandboxes", IDPrefix+"*"),
		filepath.Join(vm.JailBase(m.DataDir), "firecracker", IDPrefix+"*"),
		filepath.Join(filepath.Dir(m.VMs.CgroupPath("x")), IDPrefix+"*"),
		filepath.Join(m.DataDir, "vms", IDPrefix+"*"),
	} {
		paths, _ := filepath.Glob(glob)
		for _, p := range paths {
			candidates[filepath.Base(p)] = true
		}
	}
	for _, ns := range netns.Namespaces() {
		if id := strings.TrimPrefix(ns, netns.Prefix); strings.HasPrefix(id, IDPrefix) {
			candidates[id] = true
		}
	}
	for id := range candidates {
		if known[id] || !vm.ValidID(id) {
			continue
		}
		if err := m.cleanup(ctx, id, Dir(m.DataDir, id)); err != nil {
			errs = append(errs, fmt.Errorf("orphan %s: %w", id, err))
			continue
		}
		rep.Orphans = append(rep.Orphans, id)
	}
	// Checkpoint and layer directories nobody records (a crash mid-way).
	cps, _ := m.Store.ListCheckpoints(ctx, "")
	knownCP := map[string]bool{}
	for _, c := range cps {
		knownCP[filepath.Base(c.Dir)] = true
	}
	dirs, _ := filepath.Glob(filepath.Join(m.DataDir, "checkpoints", "cp-*"))
	for _, d := range dirs {
		if !knownCP[filepath.Base(d)] {
			os.RemoveAll(d)
			rep.Orphans = append(rep.Orphans, filepath.Base(d))
		}
	}
	dirs, _ = filepath.Glob(filepath.Join(m.DataDir, "layers", "layer-*"))
	for _, d := range dirs {
		if _, err := m.Store.GetLayer(ctx, filepath.Base(d)); errors.Is(err, store.ErrNotFound) {
			os.RemoveAll(d)
			rep.Orphans = append(rep.Orphans, filepath.Base(d))
		}
	}
	m.gc(ctx)
	return rep, errors.Join(errs...)
}

// IDPrefix starts every generated sandbox id; Recover only touches
// resources with this prefix (template builds use "build-").
const IDPrefix = "sb-"

// ExpireOnce destroys every sandbox whose TTL has run out at now.
func (m *Manager) ExpireOnce(ctx context.Context, now time.Time) ([]string, error) {
	recs, err := m.Store.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	var done []string
	var errs []error
	for _, r := range recs {
		if r.Expired(now) && r.State == store.StateRunning {
			if err := m.Destroy(ctx, r.ID); err != nil {
				errs = append(errs, err)
				continue
			}
			done = append(done, r.ID)
		}
	}
	return done, errors.Join(errs...)
}

// Expose forwards a host TCP port to a guest port, for previews. hostPort
// 0 picks a free one. The daemon proxies each connection to the sandbox's
// namespace address, where the namespace's DNAT hands it to the guest.
func (m *Manager) Expose(ctx context.Context, id string, guestPort, hostPort int) (int, error) {
	if guestPort < 1 || guestPort > 65535 || hostPort < 0 || hostPort > 65535 {
		return 0, fmt.Errorf("port out of range: guest %d, host %d", guestPort, hostPort)
	}
	rec, err := m.Store.GetSandbox(ctx, id)
	if err != nil {
		return 0, err
	}
	if rec.State != store.StateRunning {
		return 0, fmt.Errorf("%w: sandbox %s is %s", vm.ErrState, id, rec.State)
	}
	ln, err := m.listen(ctx, id, guestPort, hostPort)
	if err != nil {
		return 0, err
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	if err := m.Store.AddPortForward(ctx, store.PortForward{SandboxID: id, GuestPort: guestPort, HostPort: actual}); err != nil {
		ln.Close()
		return 0, err
	}
	return actual, nil
}

// PortForwards lists a sandbox's port forwards.
func (m *Manager) PortForwards(ctx context.Context, id string) ([]store.PortForward, error) {
	return m.Store.PortForwards(ctx, id)
}

func (m *Manager) restoreForwards(ctx context.Context, id string) error {
	pfs, err := m.Store.PortForwards(ctx, id)
	if err != nil {
		return err
	}
	var errs []error
	for _, pf := range pfs {
		if _, err := m.listen(ctx, id, pf.GuestPort, pf.HostPort); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) listen(ctx context.Context, id string, guestPort, hostPort int) (net.Listener, error) {
	slot, err := m.Store.Slot(ctx, id)
	if err != nil {
		return nil, err
	}
	_, nsAddr, err := m.Net.Addrs(slot)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(m.ExposeAddr, strconv.Itoa(hostPort)))
	if err != nil {
		return nil, err
	}
	target := net.JoinHostPort(nsAddr.String(), strconv.Itoa(guestPort))
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go proxy(c, target)
		}
	}()
	m.mu.Lock()
	m.listeners[id] = append(m.listeners[id], ln)
	m.mu.Unlock()
	return ln, nil
}

func proxy(c net.Conn, target string) {
	defer c.Close()
	u, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer u.Close()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(u, c)
	go cp(c, u)
	<-done
	<-done
}
