//go:build integration

package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/store"
)

func TestIntegrationForkUffd(t *testing.T) {
	m := newTestManager(t)
	m.MemoryBackend = BackendUffd
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var all []string
	t.Cleanup(func() {
		for _, id := range all {
			m.Destroy(context.Background(), id)
		}
	})
	parent, err := m.Create(ctx, CreateOptions{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, parent.ID)
	sum := sh(t, m, parent.ID, "head -c 104857600 /dev/urandom > /tmp/big.bin && sha256sum /tmp/big.bin | cut -d' ' -f1")

	for _, prefetch := range []bool{false, true} {
		pf := prefetch
		res, err := m.ForkWith(ctx, parent.ID, ForkOptions{N: 3, Prefetch: &pf})
		if err != nil {
			t.Fatal(err)
		}
		if res.Backend != BackendUffd {
			t.Fatalf("backend %q", res.Backend)
		}
		l, err := m.Store.GetLayer(ctx, res.Layer)
		if err != nil || l.Kind != store.LayerDiff {
			t.Fatalf("layer %+v %v", l, err)
		}
		t.Logf("uffd fork (prefetch %v): parent paused %s (diff %s), %.1f MiB dirty kept as a layer, no merge; restore per child %v, total %s",
			prefetch, res.Timings.ParentPaused, res.Timings.Snapshot, mb(res.Timings.DirtyBytes), res.Timings.Restore, res.Timings.Total)
		for _, c := range res.Children {
			all = append(all, c.ID)
			if got := sh(t, m, c.ID, "sha256sum /tmp/big.bin | cut -d' ' -f1"); got != sum {
				t.Errorf("%s: file differs", c.ID)
			}
			rec := mustRecord(t, m, c.ID)
			if !pagerAlive(rec.PagerPid, c.ID) {
				t.Errorf("%s: no page server", c.ID)
			}
		}
		time.Sleep(1500 * time.Millisecond) // the page server writes stats every second
		for _, c := range res.Children {
			st, err := m.PagerStats(c.ID)
			if err != nil || st == nil {
				t.Fatalf("%s: pager stats %v %v", c.ID, st, err)
			}
			t.Logf("%s: %v faults (%v copied, %v zero), %v prefetched, fault p50 %.1f µs p99 %.1f µs, %v layers",
				c.ID, st["faults"], st["copied"], st["zeroed"], st["prefetched"], st["fault_p50_us"], st["fault_p99_us"], st["layers"])
			if st["faults"].(float64) == 0 && st["prefetched"].(float64) == 0 {
				t.Errorf("%s: the page server served nothing", c.ID)
			}
			if prefetch && st["prefetched"].(float64) == 0 {
				t.Errorf("%s: prefetch copied nothing", c.ID)
			}
		}
	}

	// Second level: a child forks again; its children's chain is two
	// diffs over the template.
	kid := all[1]
	sh(t, m, kid, "echo second > /tmp/level")
	grand, err := m.ForkWith(ctx, kid, ForkOptions{N: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grand.Children {
		all = append(all, g.ID)
		if got := sh(t, m, g.ID, "cat /tmp/level; sha256sum /tmp/big.bin | cut -d' ' -f1"); got != "second\n"+sum {
			t.Errorf("grandchild %s: %q", g.ID, got)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	if st, _ := m.PagerStats(grand.Children[0].ID); st == nil || st["layers"].(float64) != 3 {
		t.Errorf("grandchild chain: %v, want 3 layers", st)
	}

	// A merge-mode fork of a chain-backed sandbox fails clearly.
	if _, err := m.ForkWith(ctx, kid, ForkOptions{N: 1, Backend: BackendMerge}); err == nil {
		t.Error("merge fork of a diff chain accepted")
	}

	// Destroying everything frees every layer.
	var layers []string
	for _, id := range all {
		if r, err := m.Store.GetSandbox(ctx, id); err == nil {
			if l, err := m.Store.GetLayer(ctx, r.BaseLayer); err == nil {
				layers = append(layers, l.Dir)
			}
		}
	}
	for _, id := range all {
		if err := m.Destroy(ctx, id); err != nil {
			t.Error(err)
		}
	}
	all = nil
	for _, d := range layers {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("layer %s left after destroying every user", d)
		}
	}
}
