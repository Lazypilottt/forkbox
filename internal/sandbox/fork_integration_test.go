//go:build integration

package sandbox

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

const counterScript = `#!/bin/sh
# Counts in a shell variable (memory) and mirrors it to a file.
i=0
while :; do
  i=$((i+1))
  echo "$i" > /tmp/counter.tmp && mv /tmp/counter.tmp /tmp/counter
  sleep 0.05
done
`

func sh(t *testing.T, m *Manager, id, cmd string) string {
	t.Helper()
	r, err := m.Agent(id).Run(context.Background(), agentclient.ExecRequest{Args: []string{"sh", "-c", cmd}, Timeout: 2 * time.Minute}, 0)
	if err != nil {
		t.Fatalf("%s: %s: %v", id, cmd, err)
	}
	if r.Code != 0 {
		t.Fatalf("%s: %s: exit %d: %s", id, cmd, r.Code, r.Stderr)
	}
	return strings.TrimSpace(string(r.Stdout))
}

func counter(t *testing.T, m *Manager, id string) int {
	t.Helper()
	n, err := strconv.Atoi(sh(t, m, id, "cat /tmp/counter 2>/dev/null || echo 0"))
	if err != nil {
		t.Fatalf("%s: counter: %v", id, err)
	}
	return n
}

func TestIntegrationForkTen(t *testing.T) {
	m := newTestManager(t)
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
	if _, err := m.Agent(parent.ID).WriteFile(ctx, "/usr/local/bin/fbx-counter", strings.NewReader(counterScript), agentclient.WriteOptions{Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	sh(t, m, parent.ID, "setsid fbx-counter >/dev/null 2>&1 < /dev/null &")
	deadline := time.Now().Add(20 * time.Second)
	for counter(t, m, parent.ID) < 20 {
		if time.Now().After(deadline) {
			t.Fatal("parent counter not counting")
		}
		time.Sleep(100 * time.Millisecond)
	}

	atFork := counter(t, m, parent.ID)
	forkStart := time.Now()
	res, err := m.Fork(ctx, parent.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Children {
		all = append(all, c.ID)
	}
	tm := res.Timings
	t.Logf("fork into 10: pause %s, diff snapshot %s (diff file %.1f MiB allocated), disk reflinks %s, resume %s => parent paused %s",
		tm.Pause, tm.Snapshot, mb(tm.DiffBytes), tm.Disks, tm.Resume, tm.ParentPaused)
	t.Logf("merge %s (%.1f MiB dirty, %.1f MiB shared by FICLONERANGE), total %s", tm.Merge, mb(tm.DirtyBytes), mb(tm.ClonedBytes), tm.Total)
	t.Logf("restore per child: %v", tm.Restore)

	// The parent carries on.
	if after := counter(t, m, parent.ID); after < atFork {
		t.Errorf("parent counter went back: %d -> %d", atFork, after)
	}
	for i, c := range res.Children {
		if c.Parent != parent.ID || c.BaseLayer != res.Layer {
			t.Errorf("child %s: parent %q base %q", c.ID, c.Parent, c.BaseLayer)
		}
		if got := sh(t, m, c.ID, "sha256sum /tmp/big.bin | cut -d' ' -f1"); got != sum {
			t.Errorf("child %s: /tmp/big.bin sha256 %s, parent's %s", c.ID, got, sum)
		}
		if pid := sh(t, m, c.ID, "pgrep -f [f]bx-counter | head -1"); pid == "" {
			t.Errorf("child %s: counter process missing", c.ID)
		}
		first := counter(t, m, c.ID)
		// The counter ticks every 50 ms from the parent's value at the
		// pause; it cannot be further ahead than the time since the fork.
		upper := atFork + int(time.Since(forkStart)/(50*time.Millisecond)) + 5
		time.Sleep(300 * time.Millisecond)
		second := counter(t, m, c.ID)
		if first < atFork || first > upper || second <= first {
			t.Errorf("child %s: counter %d then %d; parent was at %d when forking", c.ID, first, second, atFork)
		}
		if h := sh(t, m, c.ID, "hostname"); h != c.ID {
			t.Errorf("child %s: hostname %q", c.ID, h)
		}
		sh(t, m, c.ID, fmt.Sprintf("echo %d > /tmp/only-in-%s", i, c.ID))
	}
	// Each child's write is invisible to its siblings and the parent.
	for _, id := range append([]string{parent.ID}, all[1:]...) {
		files := sh(t, m, id, "ls /tmp | grep '^only-in-' || true")
		want := ""
		if id != parent.ID {
			want = "only-in-" + id
		}
		if files != want {
			t.Errorf("%s sees %q, want %q", id, files, want)
		}
	}
	machineIDs := map[string]bool{}
	for _, id := range all {
		machineIDs[sh(t, m, id, "cat /etc/machine-id")] = true
	}
	if len(machineIDs) != len(all) {
		t.Errorf("%d machine-ids for %d sandboxes", len(machineIDs), len(all))
	}

	// Lineage: a child forks onto its own base, the parent's layer.
	grand, err := m.Fork(ctx, res.Children[0].ID, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grand.Children {
		all = append(all, g.ID)
		if got := sh(t, m, g.ID, "sha256sum /tmp/big.bin | cut -d' ' -f1; cat /tmp/only-in-"+res.Children[0].ID); got != sum+"\n0" {
			t.Errorf("grandchild %s: %q", g.ID, got)
		}
	}
	lin, err := m.Store.Lineage(ctx, grand.Layer)
	if err != nil || len(lin) != 2 || lin[1].ID != res.Layer || lin[1].Parent != parent.Template {
		t.Errorf("lineage of %s: %+v %v", grand.Layer, lin, err)
	}
	t.Logf("second-level fork into 2: parent paused %s, %.1f MiB dirty, total %s",
		grand.Timings.ParentPaused, mb(grand.Timings.DirtyBytes), grand.Timings.Total)
	if fi, err := os.Stat(lin[0].Mem); err != nil || fi.Mode().Perm() != 0o440 {
		t.Errorf("layer memory file: %v %v", fi, err)
	}
}
