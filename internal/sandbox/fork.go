package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Lazypilottt/forkbox/internal/storage"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm"
)

// Fork constraints, from docs/snapshotting/snapshot-support.md:
//   - A diff snapshot holds only the pages dirtied since the VM's last
//     snapshot or snapshot load (track_dirty_pages is on for every VM), in a
//     sparse memory file at their guest offsets, plus the full VM state.
//   - It is not resumable alone: it must be merged onto the memory file the
//     VM's state was last based on. For a sandbox restored from a template
//     that is the template's memory; after a fork it is the fork's layer.
//   - The VM must be paused; block devices are flushed during the snapshot,
//     so the disk is consistent with memory and can be reflinked right away.
//   - Snapshotting resets the vsock device: open agent connections to the
//     parent end, and new ones work (agent clients reconnect per call).
//
// The v0.2 method: the merged memory is one new layer, a reflink of the
// base with the dirty pages cloned in (storage.MergeDiff, the equivalent of
// `snapshot-editor edit-memory rebase`). All children map that one file
// privately, so they share its pages, and it becomes the base of the
// parent's and every child's next diff.

// ForkTimings are the measured steps of one fork.
type ForkTimings struct {
	Pause        time.Duration
	Snapshot     time.Duration // diff snapshot
	Disks        time.Duration // reflink copies of the parent's disk
	Resume       time.Duration
	ParentPaused time.Duration // pause through resume
	Merge        time.Duration
	Restore      []time.Duration // per child, until its agent has its identity
	Total        time.Duration
	DiffBytes    int64 // the diff memory file's allocated size
	DirtyBytes   int64 // dirty pages merged into the layer
	ClonedBytes  int64 // of which shared by FICLONERANGE, not copied
}

// ForkResult is the outcome of Fork.
type ForkResult struct {
	Layer    string
	Children []*Created
	Timings  ForkTimings
}

// MaxFork bounds one fork.
const MaxFork = 256

// Fork turns a running sandbox into n more, each continuing from the
// parent's state at the moment of the fork. concurrency bounds parallel
// restores (0: the number of CPUs).
func (m *Manager) Fork(ctx context.Context, parentID string, n, concurrency int) (*ForkResult, error) {
	start := time.Now()
	if n < 1 || n > MaxFork {
		return nil, fmt.Errorf("%w: fork into 1 to %d children, not %d", vm.ErrConfig, MaxFork, n)
	}
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}
	unlock := m.lockFork(parentID)
	defer unlock()
	parent, err := m.Store.GetSandbox(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if parent.State != store.StateRunning {
		return nil, fmt.Errorf("%w: sandbox %s is %s", vm.ErrState, parentID, parent.State)
	}
	tpl, err := template.Get(m.DataDir, parent.Template)
	if err != nil {
		return nil, err
	}
	baseMem, err := m.layerMem(ctx, parent, tpl)
	if err != nil {
		return nil, err
	}
	v, err := m.vmFor(ctx, parent, tpl)
	if err != nil {
		return nil, err
	}

	res := &ForkResult{Layer: "layer-" + randHex(8)}
	layerDir := filepath.Join(m.DataDir, "layers", res.Layer)
	diffDir := filepath.Join(parent.Dir, "diff-"+res.Layer)
	// Children are recorded before the pause, so the pause stays short and
	// a reaper can always find them.
	var children []*Created
	fail := func(err error) (*ForkResult, error) {
		var errs []error
		for _, c := range children {
			errs = append(errs, m.cleanup(context.WithoutCancel(ctx), c.ID, c.Dir))
			_ = m.Store.DeleteSandbox(context.WithoutCancel(ctx), c.ID)
		}
		os.RemoveAll(diffDir)
		return nil, errors.Join(append([]error{fmt.Errorf("fork %s: %w", parentID, err)}, errs...)...)
	}
	for i := 0; i < n; i++ {
		id := IDPrefix + randHex(6)
		rec := &store.Sandbox{
			ID: id, Template: parent.Template, TemplateName: parent.TemplateName, Parent: parentID,
			State: store.StateCreating, Dir: Dir(m.DataDir, id), Jail: vm.JailDir(m.DataDir, id),
			Cgroup: m.VMs.CgroupPath(id), Vsock: vm.VsockSocketPath(m.DataDir, id), TTL: parent.TTL, BaseLayer: res.Layer,
		}
		if err := m.Store.InsertSandbox(ctx, rec); err != nil {
			return fail(err)
		}
		children = append(children, &Created{Sandbox: rec})
		if err := os.MkdirAll(rec.Dir, 0o700); err != nil {
			return fail(err)
		}
	}

	// The parent is paused from here to Resume.
	tPause := time.Now()
	if err := v.Pause(ctx); err != nil {
		return fail(err)
	}
	res.Timings.Pause = time.Since(tPause)
	resumed := false
	defer func() {
		if !resumed {
			_ = v.Resume(context.WithoutCancel(ctx))
		}
	}()
	t := time.Now()
	if _, err := v.SnapshotDiff(ctx, diffDir); err != nil {
		return fail(err)
	}
	res.Timings.Snapshot = time.Since(t)
	t = time.Now()
	parentDisk := filepath.Join(parent.Dir, DiskFile)
	for _, c := range children {
		if err := storage.Reflink(parentDisk, filepath.Join(c.Dir, DiskFile)); err != nil {
			return fail(err)
		}
	}
	res.Timings.Disks = time.Since(t)
	t = time.Now()
	if err := v.Resume(ctx); err != nil {
		return fail(err)
	}
	resumed = true
	res.Timings.Resume = time.Since(t)
	res.Timings.ParentPaused = time.Since(tPause)

	// Merge the diff onto the parent's base: the new layer.
	t = time.Now()
	if err := os.MkdirAll(layerDir, 0o755); err != nil {
		return fail(err)
	}
	st, err := storage.MergeDiff(baseMem, filepath.Join(diffDir, vm.SnapshotMemFile), filepath.Join(layerDir, vm.SnapshotMemFile))
	if err != nil {
		os.RemoveAll(layerDir)
		return fail(err)
	}
	res.Timings.DiffBytes, res.Timings.DirtyBytes, res.Timings.ClonedBytes = st.DiffAllocate, st.Bytes, st.ClonedBytes
	if err := os.Rename(filepath.Join(diffDir, vm.SnapshotStateFile), filepath.Join(layerDir, vm.SnapshotStateFile)); err != nil {
		os.RemoveAll(layerDir)
		return fail(err)
	}
	diffMeta, err := vm.ReadSnapshot(diffDir)
	if err != nil {
		os.RemoveAll(layerDir)
		return fail(err)
	}
	full := *diffMeta
	full.Type, full.CreatedAt = vm.SnapshotFull, time.Now().UTC()
	if err := vm.WriteSnapshotMeta(layerDir, &full); err != nil {
		os.RemoveAll(layerDir)
		return fail(err)
	}
	for _, f := range []string{vm.SnapshotMemFile, vm.SnapshotStateFile} {
		p := filepath.Join(layerDir, f)
		// Read-only, readable by the jail group: children map it.
		if err := os.Lchown(p, 0, vm.DefaultGID); err != nil && !errors.Is(err, os.ErrPermission) {
			return fail(err)
		}
		if err := os.Chmod(p, 0o440); err != nil {
			return fail(err)
		}
	}
	os.RemoveAll(diffDir)
	if err := m.Store.InsertLayer(ctx, &store.Layer{
		ID: res.Layer, Parent: parent.BaseLayer, Kind: store.LayerFork, Dir: layerDir,
		Mem: filepath.Join(layerDir, vm.SnapshotMemFile), DirtyBytes: st.Bytes, SourceSandbox: parentID,
	}); err != nil {
		return fail(err)
	}
	// The parent's dirty tracking was reset by the snapshot: its next diff
	// is relative to this layer.
	if err := m.Store.SetBaseLayer(ctx, parentID, res.Layer); err != nil {
		return fail(err)
	}
	res.Timings.Merge = time.Since(t)

	// Restore the children, at most concurrency at a time.
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, c := range children {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t := time.Now()
			errs[i] = m.restoreInto(ctx, c, layerDir, filepath.Join(layerDir, vm.SnapshotMemFile), filepath.Join(c.Dir, DiskFile))
			c.Timings.Total = time.Since(t)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fail(err)
	}
	for _, c := range children {
		res.Timings.Restore = append(res.Timings.Restore, c.Timings.Total)
	}
	res.Children = children
	res.Timings.Total = time.Since(start)
	m.Log.Printf("fork %s into %d (layer %s): paused %s, diff %.1f MiB, merge %s, total %s", parentID, n, res.Layer,
		res.Timings.ParentPaused.Round(time.Microsecond), float64(st.Bytes)/(1<<20), res.Timings.Merge.Round(time.Millisecond),
		res.Timings.Total.Round(time.Millisecond))
	return res, nil
}

// layerMem is the memory file a sandbox's next diff applies to.
func (m *Manager) layerMem(ctx context.Context, sb *store.Sandbox, tpl *template.Template) (string, error) {
	if sb.BaseLayer == "" || sb.BaseLayer == tpl.ID || strings.HasPrefix(sb.BaseLayer, "tpl-") {
		return filepath.Join(tpl.SnapshotDir(), vm.SnapshotMemFile), nil
	}
	l, err := m.Store.GetLayer(ctx, sb.BaseLayer)
	if err != nil {
		return "", err
	}
	return l.Mem, nil
}

// vmFor returns a handle on a sandbox's VM: the one this process started,
// or one attached to an adopted VM.
func (m *Manager) vmFor(ctx context.Context, sb *store.Sandbox, tpl *template.Template) (*vm.VM, error) {
	m.mu.Lock()
	v := m.vms[sb.ID]
	m.mu.Unlock()
	if v != nil {
		return v, nil
	}
	v, err := m.VMs.Attach(ctx, sb.ID, sb.Pid, vm.Config{VCPUs: tpl.VCPUs, MemMiB: tpl.MemMiB})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.vms[sb.ID] = v
	m.mu.Unlock()
	return v, nil
}

func (m *Manager) lockFork(id string) func() {
	m.mu.Lock()
	if m.forkLocks == nil {
		m.forkLocks = map[string]*sync.Mutex{}
	}
	l := m.forkLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.forkLocks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
