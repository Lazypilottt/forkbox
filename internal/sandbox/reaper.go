package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/Lazypilottt/forkbox/internal/store"
)

// ReapStats counts what the reaper removed since the daemon started.
type ReapStats struct {
	Sweeps     uint64 // periodic sweeps run
	DeadVMs    uint64 // running sandboxes whose Firecracker process was gone
	DeadPagers uint64 // uffd sandboxes whose page server was gone
	Warm       uint64 // warm-pool members found dead
}

type reapCounters struct {
	sweeps, deadVMs, deadPagers, warm atomic.Uint64
}

// ReapStats returns the reaper's counters.
func (m *Manager) ReapStats() ReapStats {
	c := &m.reaped
	return ReapStats{Sweeps: c.sweeps.Load(), DeadVMs: c.deadVMs.Load(), DeadPagers: c.deadPagers.Load(), Warm: c.warm.Load()}
}

// Sweep finds sandboxes whose VM or page server died while the daemon was
// running and removes their resources (jail, cgroup, namespace, files,
// port forwards). A running sandbox's record stays, marked failed, so its
// owner sees why it went away; a dead warm-pool member is dropped. Sandboxes
// busy with a fork, checkpoint or rewind are left for the next sweep.
func (m *Manager) Sweep(ctx context.Context) ([]string, error) {
	m.reaped.sweeps.Add(1)
	recs, err := m.Store.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	var reaped []string
	var errs []error
	for _, r := range recs {
		if r.State != store.StateRunning && r.State != store.StateWarm {
			continue
		}
		l := m.forkLock(r.ID)
		if !l.TryLock() {
			continue
		}
		why, err := m.sweepOne(ctx, r.ID)
		l.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
		if why != "" {
			reaped = append(reaped, r.ID+" ("+why+")")
		}
	}
	return reaped, errors.Join(errs...)
}

// sweepOne checks one sandbox under its fork lock.
func (m *Manager) sweepOne(ctx context.Context, id string) (string, error) {
	r, err := m.Store.GetSandbox(ctx, id) // fresh, now that nothing else changes it
	if err != nil || (r.State != store.StateRunning && r.State != store.StateWarm) {
		return "", nil
	}
	var why string
	switch {
	case !m.VMs.Alive(r.Pid, r.ID):
		why = "firecracker exited"
		if r.State == store.StateRunning {
			m.reaped.deadVMs.Add(1)
		}
	case r.PagerPid != 0 && !pagerAlive(r.PagerPid, r.ID):
		why = "page server exited"
		if r.State == store.StateRunning {
			m.reaped.deadPagers.Add(1)
		}
	default:
		return "", nil
	}
	m.Log.Printf("reaper: %s: %s", id, why)
	if r.State == store.StateWarm {
		m.reaped.warm.Add(1)
		m.dropWarm(id)
		if err := m.cleanup(ctx, id, r.Dir); err != nil {
			return why, fmt.Errorf("reap %s: %w", id, err)
		}
		return why, m.Store.DeleteSandbox(ctx, id)
	}
	err = m.cleanup(ctx, id, r.Dir)
	_ = m.Store.SetState(context.WithoutCancel(ctx), id, store.StateFailed, 0, "reaped: "+why)
	if err != nil {
		return why, fmt.Errorf("reap %s: %w", id, err)
	}
	return why, nil
}

// forkLock is the per-sandbox lock lockFork takes.
func (m *Manager) forkLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.forkLocks == nil {
		m.forkLocks = map[string]*sync.Mutex{}
	}
	l := m.forkLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.forkLocks[id] = l
	}
	return l
}

// dropWarm removes id from its warm pool's ready list.
func (m *Manager) dropWarm(id string) {
	m.mu.Lock()
	p := m.pools
	m.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pl := range p.byName {
		for i, r := range pl.ready {
			if r == id {
				pl.ready = append(pl.ready[:i:i], pl.ready[i+1:]...)
				return
			}
		}
	}
}
