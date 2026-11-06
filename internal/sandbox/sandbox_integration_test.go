//go:build integration

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/testutil"
	"github.com/Lazypilottt/forkbox/internal/versions"
	"github.com/Lazypilottt/forkbox/internal/vm"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	testutil.RequireKVM(t)
	if os.Geteuid() != 0 {
		t.Fatal("run `make integration` (needs root)")
	}
	v, err := versions.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	data := datadir.FromEnv()
	vms, err := vm.NewManager(vm.Options{DataDir: data, FirecrackerVersion: v.Firecracker.Version})
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(data, "test", fmt.Sprintf("sandbox-%d.db", time.Now().UnixNano()))
	os.MkdirAll(filepath.Dir(dbPath), 0o755)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(); os.Remove(dbPath); os.Remove(dbPath + "-wal"); os.Remove(dbPath + "-shm") })
	if _, err := template.Get(data, "python"); err != nil {
		t.Fatalf("%v: build it first with `sudo forkbox templates build templates/python.yaml`", err)
	}
	return New(data, vms, st)
}

func TestIntegrationCreateTwenty(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const n = 20
	var sbs []*Created
	t.Cleanup(func() {
		for _, sb := range sbs {
			m.Destroy(context.Background(), sb.ID)
		}
	})
	var creates []time.Duration
	for i := 0; i < n; i++ {
		c, err := m.Create(ctx, CreateOptions{Template: "python"})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		sbs = append(sbs, c)
		creates = append(creates, c.Timings.Total)
		if i == 0 {
			t.Logf("first create: %+v", c.Timings)
			t.Logf("identity: %v (generation %d, clock step %s)", c.Identity.Actions, c.Identity.Generation, c.Identity.ClockStep)
		}
	}
	sort.Slice(creates, func(i, j int) bool { return creates[i] < creates[j] })
	t.Logf("create time over %d: min %s p50 %s max %s", n, creates[0], creates[n/2], creates[n-1])

	seen := map[string]map[string]string{"random": {}, "machine-id": {}, "hostname": {}}
	for _, sb := range sbs {
		c := m.Agent(sb.ID)
		r, err := c.Run(ctx, agentclient.ExecRequest{Args: []string{"sh", "-c",
			`head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; echo; cat /etc/machine-id; hostname; python -c 'print(1+1)'`}}, 0)
		if err != nil {
			t.Fatalf("%s exec: %v", sb.ID, err)
		}
		lines := strings.Split(strings.TrimSpace(string(r.Stdout)), "\n")
		if r.Code != 0 || len(lines) != 4 || lines[3] != "2" {
			t.Fatalf("%s: exit %d stdout %q stderr %q", sb.ID, r.Code, r.Stdout, r.Stderr)
		}
		if lines[2] != sb.ID {
			t.Errorf("%s: hostname %q", sb.ID, lines[2])
		}
		for i, k := range []string{"random", "machine-id", "hostname"} {
			if prev, dup := seen[k][lines[i]]; dup {
				t.Errorf("%s %q repeats in %s and %s", k, lines[i], prev, sb.ID)
			}
			seen[k][lines[i]] = sb.ID
		}

		before := time.Now()
		info, err := c.Info(ctx)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		guest := time.Unix(0, info.RealtimeUnixNano)
		if guest.Before(before.Add(-time.Second)) || guest.After(after.Add(time.Second)) {
			t.Errorf("%s: guest clock %s, host %s..%s", sb.ID, guest.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
		}
		if info.SandboxId != sb.ID {
			t.Errorf("%s: agent reports sandbox id %q", sb.ID, info.SandboxId)
		}
	}

	// Memory: the template's pages are shared, so PSS is well below RSS.
	var sumPss, sumRss int64
	t.Logf("%-18s %10s %10s %12s %13s", "sandbox", "RSS MiB", "PSS MiB", "shared MiB", "private MiB")
	for _, sb := range sbs {
		ms, err := vm.ReadMemStats(sb.Pid)
		if err != nil {
			t.Fatal(err)
		}
		sumPss += ms.Pss
		sumRss += ms.Rss
		t.Logf("%-18s %10.1f %10.1f %12.1f %13.1f", sb.ID, mb(ms.Rss), mb(ms.Pss), mb(ms.SharedClean+ms.SharedDirty), mb(ms.PrivateClean+ms.PrivateDirty))
	}
	t.Logf("total: RSS %.1f MiB, PSS %.1f MiB (%.1f MiB PSS per sandbox; template memory %d MiB)", mb(sumRss), mb(sumPss), mb(sumPss)/n, 512)
	if sumPss >= sumRss*9/10 {
		t.Errorf("no page sharing: PSS %d vs RSS %d", sumPss, sumRss)
	}

	for _, sb := range sbs {
		if err := m.Destroy(ctx, sb.ID); err != nil {
			t.Errorf("destroy %s: %v", sb.ID, err)
		}
		for _, p := range []string{sb.Dir, sb.Jail, sb.Cgroup} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s left behind", p)
			}
		}
	}
	if left, _ := m.Store.ListSandboxes(ctx); len(left) != 0 {
		t.Errorf("%d records left", len(left))
	}
	sbs = nil
}

func mb(b int64) float64 { return float64(b) / (1 << 20) }
