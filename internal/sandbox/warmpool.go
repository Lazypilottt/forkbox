package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm"
)

// Warm pools keep K sandboxes of a template restored and paused, so a
// create is only resume, agent and identity. K follows the last ten
// minutes of demand, between a pool's Min and Max and within a memory
// budget shared by all pools, and pools refill in the background.

// PoolConfig marks a template warm.
type PoolConfig struct {
	Template string // name
	Min, Max int
}

// ParsePools parses "python=1:8,node=0:4" (name=min:max).
func ParsePools(s string) ([]PoolConfig, error) {
	var out []PoolConfig
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rng, ok := strings.Cut(part, "=")
		lo, hi, ok2 := strings.Cut(rng, ":")
		min, err1 := strconv.Atoi(lo)
		max, err2 := strconv.Atoi(hi)
		if !ok || !ok2 || err1 != nil || err2 != nil || name == "" || min < 0 || max < min || max > 256 {
			return nil, fmt.Errorf("warm pool %q: want name=min:max, e.g. python=1:8", part)
		}
		out = append(out, PoolConfig{Template: name, Min: min, Max: max})
	}
	return out, nil
}

// DemandWindow is how far back pools look at create demand.
const DemandWindow = 10 * time.Minute

type pool struct {
	cfg      PoolConfig
	memMiB   int
	ready    []string // warm sandbox ids, oldest first
	inflight int
	demand   []time.Time
	hits     uint64
	misses   uint64
	lastErr  string
}

type pools struct {
	mu        sync.Mutex
	byName    map[string]*pool
	budgetMiB int
}

// PoolStat is one pool's state.
type PoolStat struct {
	Template            string
	Ready, Target       int
	Hits, Misses        uint64
	Demand10m           int
	MemoryMiBPerSandbox int
	LastError           string
}

// StartPools starts the warm pools and their refill loop; it returns when
// ctx ends.
func (m *Manager) StartPools(ctx context.Context, cfgs []PoolConfig, budgetMiB int) {
	p := &pools{byName: map[string]*pool{}, budgetMiB: budgetMiB}
	for _, c := range cfgs {
		p.byName[c.Template] = &pool{cfg: c}
	}
	m.mu.Lock()
	m.pools = p
	m.mu.Unlock()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.refill(ctx)
		}
	}
}

// target is K for each pool: Min plus the creates per minute over the last
// window, at most Max, and all pools together within the memory budget
// (each warm sandbox counted at its template's full memory size).
func (p *pools) targets(now time.Time) map[string]int {
	names := make([]string, 0, len(p.byName))
	for n := range p.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string]int{}
	used := 0
	for _, n := range names {
		pl := p.byName[n]
		keep := pl.demand[:0]
		for _, t := range pl.demand {
			if now.Sub(t) < DemandWindow {
				keep = append(keep, t)
			}
		}
		pl.demand = keep
		perMin := (len(keep) + int(DemandWindow/time.Minute) - 1) / int(DemandWindow/time.Minute)
		k := min(max(pl.cfg.Min+perMin, pl.cfg.Min), pl.cfg.Max)
		if pl.memMiB > 0 && p.budgetMiB > 0 {
			k = min(k, max(0, (p.budgetMiB-used)/pl.memMiB))
			used += k * pl.memMiB
		}
		out[n] = k
	}
	return out
}

func (m *Manager) refill(ctx context.Context) {
	m.mu.Lock()
	p := m.pools
	m.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	targets := p.targets(time.Now())
	var start []string
	var trim []string
	for name, pl := range p.byName {
		k := targets[name]
		if have := len(pl.ready) + pl.inflight; have < k && pl.inflight == 0 {
			pl.inflight++
			start = append(start, name)
		}
		for len(pl.ready) > k {
			trim = append(trim, pl.ready[0])
			pl.ready = pl.ready[1:]
		}
	}
	p.mu.Unlock()
	for _, id := range trim {
		go m.Destroy(context.WithoutCancel(ctx), id)
	}
	for _, name := range start {
		go m.warmOne(context.WithoutCancel(ctx), p, name)
	}
}

// warmOne restores one sandbox of a template and leaves it paused.
func (m *Manager) warmOne(ctx context.Context, p *pools, name string) {
	id, err := m.prepareWarm(ctx, name, p)
	p.mu.Lock()
	defer p.mu.Unlock()
	pl := p.byName[name]
	pl.inflight--
	if err != nil {
		pl.lastErr = err.Error()
		m.Log.Printf("warm pool %s: %v", name, err)
		return
	}
	pl.lastErr = ""
	pl.ready = append(pl.ready, id)
}

func (m *Manager) prepareWarm(ctx context.Context, name string, p *pools) (string, error) {
	if err := m.checkDensity(1); err != nil {
		return "", err
	}
	tpl, err := template.Get(m.DataDir, name)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.byName[name].memMiB = tpl.MemMiB
	p.mu.Unlock()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := IDPrefix + hex.EncodeToString(b)
	rec := &store.Sandbox{
		ID: id, Template: tpl.ID, TemplateName: tpl.Name, State: store.StateCreating, Dir: Dir(m.DataDir, id), BaseLayer: tpl.ID,
		Jail: vm.JailDir(m.DataDir, id), Cgroup: m.VMs.CgroupPath(id), Vsock: vm.VsockSocketPath(m.DataDir, id),
	}
	if rec.Egress, err = egressFor(tpl, nil); err != nil {
		return "", err
	}
	if err := m.Store.InsertSandbox(ctx, rec); err != nil {
		return "", err
	}
	c := &Created{Sandbox: rec, warm: true}
	err = func() error {
		if err := os.MkdirAll(rec.Dir, 0o700); err != nil {
			return err
		}
		disk := filepath.Join(rec.Dir, DiskFile)
		if err := m.cloneDisk(ctx, tpl.Rootfs(), disk); err != nil {
			return err
		}
		return m.restoreInto(ctx, c, tpl.SnapshotDir(), memSource{File: filepath.Join(tpl.SnapshotDir(), vm.SnapshotMemFile)}, disk)
	}()
	if err != nil {
		_ = m.cleanup(ctx, id, rec.Dir)
		_ = m.Store.DeleteSandbox(ctx, id)
		return "", err
	}
	return id, nil
}

// fromPool hands out a warm sandbox of tpl, if there is one: resume, wait
// for the agent, give it its identity. Every call counts as demand.
func (m *Manager) fromPool(ctx context.Context, tpl *template.Template, ttl time.Duration) (*Created, bool) {
	m.mu.Lock()
	p := m.pools
	m.mu.Unlock()
	if p == nil {
		return nil, false
	}
	p.mu.Lock()
	pl := p.byName[tpl.Name]
	if pl == nil {
		p.mu.Unlock()
		return nil, false
	}
	pl.demand = append(pl.demand, time.Now())
	var id string
	if len(pl.ready) > 0 {
		id, pl.ready = pl.ready[0], pl.ready[1:]
		pl.hits++
	} else {
		pl.misses++
	}
	p.mu.Unlock()
	if id == "" {
		return nil, false
	}
	rec, err := m.Store.GetSandbox(ctx, id)
	if err != nil || rec.Template != tpl.ID { // a rebuilt template: drop it
		go m.Destroy(context.WithoutCancel(ctx), id)
		return nil, false
	}
	m.mu.Lock()
	v := m.vms[id]
	m.mu.Unlock()
	if v == nil {
		go m.Destroy(context.WithoutCancel(ctx), id)
		return nil, false
	}
	rec.TTL = ttl
	rec.CreatedAt = time.Now()
	if err := m.Store.SetTTL(ctx, id, ttl); err != nil {
		go m.Destroy(context.WithoutCancel(ctx), id)
		return nil, false
	}
	c := &Created{Sandbox: rec, FromPool: true}
	if err := m.finishRestore(ctx, c, v); err != nil {
		m.Log.Printf("warm pool %s: handing out %s: %v", tpl.Name, id, err)
		go m.Destroy(context.WithoutCancel(ctx), id)
		return nil, false
	}
	return c, true
}

// PoolStats reports every warm pool.
func (m *Manager) PoolStats() []PoolStat {
	m.mu.Lock()
	p := m.pools
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	targets := p.targets(time.Now())
	var out []PoolStat
	for name, pl := range p.byName {
		out = append(out, PoolStat{Template: name, Ready: len(pl.ready), Target: targets[name], Hits: pl.hits,
			Misses: pl.misses, Demand10m: len(pl.demand), MemoryMiBPerSandbox: pl.memMiB, LastError: pl.lastErr})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Template < out[j].Template })
	return out
}
