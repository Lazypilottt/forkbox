package sandbox

import (
	"testing"
	"time"
)

func TestParsePools(t *testing.T) {
	got, err := ParsePools("python=1:8, node=0:4")
	if err != nil || len(got) != 2 || got[0] != (PoolConfig{"python", 1, 8}) || got[1] != (PoolConfig{"node", 0, 4}) {
		t.Fatalf("got %v %v", got, err)
	}
	if got, err := ParsePools(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"python", "python=1", "python=a:b", "python=5:2", "=1:2", "python=-1:2", "python=0:999"} {
		if _, err := ParsePools(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPoolTargets(t *testing.T) {
	now := time.Now()
	p := &pools{budgetMiB: 4096, byName: map[string]*pool{
		"python": {cfg: PoolConfig{"python", 1, 8}, memMiB: 512},
		"node":   {cfg: PoolConfig{"node", 0, 4}, memMiB: 512},
	}}
	if k := p.targets(now); k["python"] != 1 || k["node"] != 0 {
		t.Errorf("idle: %v", k)
	}
	// 45 creates in the last ten minutes: 5 a minute, plus the minimum.
	for i := 0; i < 45; i++ {
		p.byName["python"].demand = append(p.byName["python"].demand, now.Add(-time.Duration(i)*10*time.Second))
	}
	// Old demand is forgotten.
	p.byName["node"].demand = []time.Time{now.Add(-11 * time.Minute)}
	if k := p.targets(now); k["python"] != 6 || k["node"] != 0 || len(p.byName["node"].demand) != 0 {
		t.Errorf("busy: %v", k)
	}
	// Capped by Max, then by the memory budget across pools.
	for i := 0; i < 200; i++ {
		p.byName["python"].demand = append(p.byName["python"].demand, now)
		p.byName["node"].demand = append(p.byName["node"].demand, now)
	}
	if k := p.targets(now); k["node"] != 4 || k["python"] != 4 {
		t.Errorf("budget 4096 MiB at 512 each: %v, want node 4 then python the remaining 4", k)
	}
}
