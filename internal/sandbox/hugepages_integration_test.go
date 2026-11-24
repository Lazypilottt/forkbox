//go:build integration

package sandbox

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

// Huge pages against 4 KiB pages, on the same host: create, the first
// exec after create (which faults the working set in), and fork. Built by
// CI from bench/hugepages-template.yaml; skipped where it is missing or the
// hugepage pool is too small.
func TestIntegrationHugePages(t *testing.T) {
	m := newTestManager(t)
	huge, err := template.Get(m.DataDir, "python-huge")
	if err != nil {
		t.Skipf("%v: build bench/hugepages-template.yaml (needs vm.nr_hugepages)", err)
	}
	if free, err := freeHugePages(); err != nil || free < 3*huge.MemMiB/2 {
		t.Skipf("need %d free 2 MiB pages, have %d (%v)", 3*huge.MemMiB/2, free, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			m.Destroy(context.Background(), id)
		}
	})
	const work = `python -c "import numpy; a = numpy.ones(4_000_000); print(int(a.sum()))"`
	execMs := func(id string) float64 {
		t0 := time.Now()
		r, err := m.Agent(id).Run(ctx, agentclient.ExecRequest{Args: []string{"sh", "-c", work}, Timeout: time.Minute}, 0)
		if err != nil || r.Code != 0 || strings.TrimSpace(string(r.Stdout)) != "4000000" {
			t.Fatalf("%s: %v %+v", id, err, r)
		}
		return float64(time.Since(t0).Microseconds()) / 1000
	}
	p50 := func(v []float64) float64 {
		s := slices.Clone(v)
		slices.Sort(s)
		return s[len(s)/2]
	}
	type row struct {
		name                         string
		create, firstExec, warmExec  float64
		faults                       float64
		forkTotal, forkChild, kidRun float64
	}
	measure := func(name string) row {
		r := row{name: name}
		var creates, first, warm []float64
		var last string
		for range 3 {
			c, err := m.Create(ctx, CreateOptions{Template: name, Limits: Limits{PidsMax: 4096}}) // limits: never from a pool
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			ids = append(ids, c.ID)
			creates = append(creates, float64(c.Timings.Total.Microseconds())/1000)
			first = append(first, execMs(c.ID))
			warm = append(warm, execMs(c.ID))
			if last != "" {
				m.Destroy(ctx, last) // keep at most two at a time (the pool is small)
			}
			last = c.ID
		}
		r.create, r.firstExec, r.warmExec = p50(creates), p50(first), p50(warm)
		// The page server rewrites its stats file every second.
		for deadline := time.Now().Add(3 * time.Second); r.faults == 0 && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if st, _ := m.PagerStats(last); st != nil {
				r.faults, _ = st["faults"].(float64)
			}
		}
		fr, err := m.ForkWith(ctx, last, ForkOptions{N: 2, Backend: BackendUffd})
		if err != nil {
			t.Fatalf("%s: fork: %v", name, err)
		}
		r.forkTotal = float64(fr.Timings.Total.Microseconds()) / 1000
		var per []float64
		for _, c := range fr.Children {
			ids = append(ids, c.ID)
			per = append(per, float64(c.Timings.Total.Microseconds())/1000)
		}
		r.forkChild = p50(per)
		r.kidRun = execMs(fr.Children[0].ID)
		return r
	}
	rows := []row{measure("python"), measure("python-huge")}
	var b strings.Builder
	fmt.Fprintf(&b, "\n| template | create p50 ms | first exec ms | warm exec ms | page faults (create) | fork into 2 ms | restore per child ms | child exec ms |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %.1f | %.1f | %.1f | %.0f | %.1f | %.1f | %.1f |\n", r.name, r.create, r.firstExec, r.warmExec, r.faults, r.forkTotal, r.forkChild, r.kidRun)
	}
	t.Log(b.String())
	if rows[1].faults == 0 {
		t.Error("the hugepage sandbox reported no page faults: was it restored through the page server?")
	}
}
