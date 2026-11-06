package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Lazypilottt/forkbox/internal/netns"
	"github.com/Lazypilottt/forkbox/internal/storage"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm"
)

// Checkpoints are fork layers kept under a name: the memory layer (the
// sandbox's base plus a diff, as for a fork) and a reflink of the disk in
// <data>/checkpoints/<id>/. They outlive the sandbox until deleted.

var checkpointName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CheckpointDir is a checkpoint's directory.
func CheckpointDir(dataDir, id string) string { return filepath.Join(dataDir, "checkpoints", id) }

// Checkpoint pauses the sandbox, takes a diff snapshot and a disk reflink,
// and resumes it; the parent is paused about as long as for a fork.
func (m *Manager) Checkpoint(ctx context.Context, id, name string) (*store.Checkpoint, *ForkTimings, error) {
	if !checkpointName.MatchString(name) {
		return nil, nil, fmt.Errorf("%w: checkpoint name %q: 1-64 letters, digits, '.', '_' or '-'", vm.ErrConfig, name)
	}
	unlock := m.lockFork(id)
	defer unlock()
	sb, err := m.Store.GetSandbox(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if sb.State != store.StateRunning {
		return nil, nil, fmt.Errorf("%w: sandbox %s is %s", vm.ErrState, id, sb.State)
	}
	if _, err := m.Store.GetCheckpoint(ctx, id, name); err == nil {
		return nil, nil, fmt.Errorf("%w: checkpoint %q of %s", store.ErrExists, name, id)
	}
	tpl, err := template.Get(m.DataDir, sb.Template)
	if err != nil {
		return nil, nil, err
	}
	baseMem, err := m.layerMem(ctx, sb, tpl)
	if err != nil {
		return nil, nil, err
	}
	v, err := m.vmFor(ctx, sb, tpl)
	if err != nil {
		return nil, nil, err
	}
	cp := &store.Checkpoint{ID: "cp-" + randHex(8), SandboxID: id, Name: name, Layer: "layer-" + randHex(8),
		Template: sb.Template, TemplateName: sb.TemplateName}
	cp.Dir = CheckpointDir(m.DataDir, cp.ID)
	if err := os.MkdirAll(cp.Dir, 0o700); err != nil {
		return nil, nil, err
	}
	tm := &ForkTimings{}
	start := time.Now()
	if err := m.snapshotLayer(ctx, sb, v, baseMem, cp.Layer, []string{filepath.Join(cp.Dir, DiskFile)}, tm); err != nil {
		os.RemoveAll(cp.Dir)
		return nil, nil, fmt.Errorf("checkpoint %s: %w", id, err)
	}
	tm.Total = time.Since(start)
	if err := m.Store.InsertCheckpoint(ctx, cp); err != nil {
		return nil, nil, err
	}
	m.Log.Printf("checkpoint %s of %s (%s): paused %s, %.1f MiB dirty", name, id, cp.ID, tm.ParentPaused.Round(time.Microsecond), float64(tm.DirtyBytes)/(1<<20))
	m.gc(ctx)
	return cp, tm, nil
}

// Checkpoints lists a sandbox's checkpoints, oldest first.
func (m *Manager) Checkpoints(ctx context.Context, id string) ([]*store.Checkpoint, error) {
	return m.Store.ListCheckpoints(ctx, id)
}

// Rewind restores a checkpoint into a fresh VM that keeps the sandbox's
// id, record, network namespace, slot and port forwards. The old VM is
// stopped first: the jail, cgroup and sockets are named after the id, so
// two VMs of one sandbox cannot run at once.
func (m *Manager) Rewind(ctx context.Context, id, name string) (*Created, error) {
	unlock := m.lockFork(id)
	defer unlock()
	sb, err := m.Store.GetSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	if sb.State != store.StateRunning {
		return nil, fmt.Errorf("%w: sandbox %s is %s", vm.ErrState, id, sb.State)
	}
	cp, err := m.Store.GetCheckpoint(ctx, id, name)
	if err != nil {
		return nil, err
	}
	l, err := m.Store.GetLayer(ctx, cp.Layer)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	_ = m.Store.SetState(ctx, id, store.StateCreating, sb.Pid, "")
	if err := m.stopVM(ctx, id); err != nil {
		return nil, m.failSandbox(ctx, id, err)
	}
	disk := filepath.Join(sb.Dir, DiskFile)
	if err := os.Remove(disk); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, m.failSandbox(ctx, id, err)
	}
	if err := storage.Reflink(filepath.Join(cp.Dir, DiskFile), disk); err != nil {
		return nil, m.failSandbox(ctx, id, err)
	}
	c := &Created{Sandbox: sb}
	// The namespace and slot stay: restoreInto's network step is idempotent
	// for the same slot, but recreating it would cut port forwards, so the
	// VM is restored straight into the existing namespace.
	if err := m.restoreVM(ctx, c, l.Dir, l.Mem, disk); err != nil {
		return nil, m.failSandbox(ctx, id, err)
	}
	if err := m.Store.SetBaseLayer(ctx, id, cp.Layer); err != nil {
		return nil, err
	}
	sb.BaseLayer = cp.Layer
	c.Timings.Total = time.Since(start)
	m.Log.Printf("rewound %s to %s in %s", id, name, c.Timings.Total.Round(time.Millisecond))
	m.gc(ctx)
	return c, nil
}

// RestoreAsNew creates a separate sandbox from a checkpoint (given by id,
// or by name with the sandbox it was taken from).
func (m *Manager) RestoreAsNew(ctx context.Context, sandboxID, nameOrID string, ttl time.Duration) (*Created, error) {
	cp, err := m.Store.GetCheckpoint(ctx, sandboxID, nameOrID)
	if err != nil {
		return nil, err
	}
	l, err := m.Store.GetLayer(ctx, cp.Layer)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	nid := IDPrefix + randHex(6)
	rec := &store.Sandbox{
		ID: nid, Template: cp.Template, TemplateName: cp.TemplateName, Parent: cp.SandboxID,
		State: store.StateCreating, Dir: Dir(m.DataDir, nid), Jail: vm.JailDir(m.DataDir, nid),
		Cgroup: m.VMs.CgroupPath(nid), Vsock: vm.VsockSocketPath(m.DataDir, nid), TTL: ttl, BaseLayer: cp.Layer,
	}
	if err := m.Store.InsertSandbox(ctx, rec); err != nil {
		return nil, err
	}
	c := &Created{Sandbox: rec}
	err = func() error {
		if err := os.MkdirAll(rec.Dir, 0o700); err != nil {
			return err
		}
		disk := filepath.Join(rec.Dir, DiskFile)
		if err := storage.Reflink(filepath.Join(cp.Dir, DiskFile), disk); err != nil {
			return err
		}
		return m.restoreInto(ctx, c, l.Dir, l.Mem, disk)
	}()
	if err != nil {
		cerr := m.cleanup(context.WithoutCancel(ctx), nid, rec.Dir)
		_ = m.Store.DeleteSandbox(context.WithoutCancel(ctx), nid)
		return nil, errors.Join(fmt.Errorf("restore %s as a new sandbox: %w", cp.Name, err), cerr)
	}
	c.Timings.Total = time.Since(start)
	return c, nil
}

// DeleteCheckpoint removes a checkpoint and frees whatever only it used.
func (m *Manager) DeleteCheckpoint(ctx context.Context, sandboxID, nameOrID string) error {
	cp, err := m.Store.GetCheckpoint(ctx, sandboxID, nameOrID)
	if err != nil {
		return err
	}
	if err := m.Store.DeleteCheckpoint(ctx, cp.ID); err != nil {
		return err
	}
	if err := os.RemoveAll(cp.Dir); err != nil {
		return err
	}
	m.gc(ctx)
	return nil
}

// GC deletes memory layers that no sandbox has as its base and no
// checkpoint uses, and returns their ids. A running VM may still map a
// deleted layer's memory file; the kernel keeps the data until it exits.
func (m *Manager) GC(ctx context.Context) ([]string, error) {
	m.gcMu.Lock()
	defer m.gcMu.Unlock()
	layers, err := m.Store.UnreferencedLayers(ctx)
	if err != nil {
		return nil, err
	}
	var done []string
	var errs []error
	for _, l := range layers {
		if err := os.RemoveAll(l.Dir); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := m.Store.DeleteLayer(ctx, l.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, l.ID)
	}
	return done, errors.Join(errs...)
}

func (m *Manager) gc(ctx context.Context) {
	done, err := m.GC(context.WithoutCancel(ctx))
	if len(done) > 0 {
		m.Log.Printf("gc: removed layers %v", done)
	}
	if err != nil {
		m.Log.Printf("gc: %v", err)
	}
}

// stopVM stops a sandbox's VM and removes its jail and cgroup, but keeps
// its namespace, slot, port forwards and record.
func (m *Manager) stopVM(_ context.Context, id string) error {
	m.mu.Lock()
	v := m.vms[id]
	delete(m.vms, id)
	m.mu.Unlock()
	if v != nil {
		return v.Stop()
	}
	return m.VMs.Reap(id)
}

// restoreVM restores rec's VM into its existing namespace: restore,
// resume, agent, identity.
func (m *Manager) restoreVM(ctx context.Context, c *Created, snapDir, mem, disk string) error {
	rec := c.Sandbox
	t := time.Now()
	v, err := m.VMs.Restore(ctx, snapDir, vm.MemBackend{Kind: vm.MemFile, Path: mem},
		vm.Config{ID: rec.ID, Rootfs: vm.Drive{Path: disk}, NetNS: netns.Path(rec.ID)})
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.vms[rec.ID] = v
	m.mu.Unlock()
	c.Timings.Restore = time.Since(t)
	return m.finishRestore(ctx, c, v)
}

func (m *Manager) failSandbox(ctx context.Context, id string, err error) error {
	_ = m.Store.SetState(context.WithoutCancel(ctx), id, store.StateFailed, 0, err.Error())
	return err
}
