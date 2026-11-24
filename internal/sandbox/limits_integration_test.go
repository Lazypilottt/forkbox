//go:build integration

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/netns"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

func readCgroup(t *testing.T, m *Manager, id, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.VMs.CgroupPath(id), file))
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return strings.TrimSpace(string(b))
}

// Limits reach the host cgroup (CPU, memory) and the guest (processes), and
// forks inherit them.
func TestIntegrationLimits(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	tpl, err := template.Get(m.DataDir, "python")
	if err != nil {
		t.Fatal(err)
	}
	lim := Limits{CPUs: 0.5, MemoryMiB: tpl.MemMiB + 128, PidsMax: 32}
	if _, err := m.Create(ctx, CreateOptions{Template: "python", Limits: Limits{MemoryMiB: tpl.MemMiB}}); !errors.Is(err, vm.ErrConfig) {
		t.Fatalf("memory below the guest's: got %v, want a config error", err)
	}
	c, err := m.Create(ctx, CreateOptions{Template: "python", Limits: lim})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{c.ID}
	t.Cleanup(func() {
		for _, id := range ids {
			m.Destroy(context.Background(), id)
		}
	})
	if c.FromPool {
		t.Fatal("a sandbox with limits must not come from a warm pool")
	}
	check := func(id string) {
		t.Helper()
		if got := readCgroup(t, m, id, "cpu.max"); got != "50000 100000" {
			t.Errorf("%s: cpu.max %q", id, got)
		}
		if got, want := readCgroup(t, m, id, "memory.max"), fmt.Sprint(int64(lim.MemoryMiB)<<20); got != want {
			t.Errorf("%s: memory.max %s, want %s", id, got, want)
		}
		if got := sh(t, m, id, "cat /sys/fs/cgroup/forkbox-exec/pids.max; grep -c forkbox-exec /proc/self/cgroup"); got != "32\n1" {
			t.Errorf("%s: guest pids.max and membership: %q", id, got)
		}
	}
	check(c.ID)

	// A process storm stops at the limit, and the sandbox stays usable.
	r, err := m.Agent(c.ID).Run(ctx, agentclient.ExecRequest{Args: []string{"sh", "-c",
		`i=0; while [ $i -lt 64 ]; do sleep 3 & i=$((i+1)); done; echo started $i`}, Timeout: time.Minute}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("storm: exit %d, stdout %q, stderr %q", r.Code, r.Stdout, firstLine(string(r.Stderr)))
	if r.Code == 0 && strings.Contains(string(r.Stdout), "started 64") {
		t.Fatal("64 processes started under pids.max 32")
	}
	time.Sleep(4 * time.Second) // the sleeps that did start exit
	if got := sh(t, m, c.ID, "echo ok"); got != "ok" {
		t.Fatalf("after the storm: %q", got)
	}

	fr, err := m.Fork(ctx, c.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range fr.Children {
		ids = append(ids, ch.ID)
	}
	rec, err := m.Store.GetSandbox(ctx, fr.Children[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if ParseLimits(rec.Limits) != lim {
		t.Errorf("child limits %q, want %+v", rec.Limits, lim)
	}
	check(fr.Children[0].ID)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// Past the density limit, creates and forks fail at once with ErrDensity.
func TestIntegrationDensityLimit(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m.MaxSandboxes = 1
	c, err := m.Create(ctx, CreateOptions{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Destroy(context.Background(), c.ID) })

	start := time.Now()
	_, err = m.Create(ctx, CreateOptions{Template: "python"})
	if !errors.Is(err, ErrDensity) {
		t.Fatalf("second create: got %v, want ErrDensity", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("refusal took %s", d)
	}
	t.Logf("refused in %s: %v", time.Since(start), err)
	if _, err := m.Fork(ctx, c.ID, 2, 0); !errors.Is(err, ErrDensity) {
		t.Fatalf("fork: got %v, want ErrDensity", err)
	}

	m.MaxSandboxes = 0
	m.MinFreeMiB = 1 << 30 // more than any host has
	if _, err := m.Create(ctx, CreateOptions{Template: "python"}); !errors.Is(err, ErrDensity) || !strings.Contains(err.Error(), "MiB available") {
		t.Fatalf("memory floor: got %v", err)
	}
	all, _ := m.Store.ListSandboxes(ctx)
	if len(all) != 1 {
		t.Errorf("refused creates left %d records", len(all))
	}
}

// A sandbox whose Firecracker process dies is found by the periodic sweep,
// marked failed, and leaves nothing behind.
func TestIntegrationReaperSweep(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := m.Create(ctx, CreateOptions{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Destroy(context.Background(), c.ID) })
	live, err := m.Create(ctx, CreateOptions{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Destroy(context.Background(), live.ID) })

	if reaped, err := m.Sweep(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("sweep of healthy sandboxes: %v, %v", reaped, err)
	}
	if err := syscall.Kill(c.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for m.VMs.Alive(c.Pid, c.ID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	reaped, err := m.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || !strings.HasPrefix(reaped[0], c.ID) {
		t.Fatalf("reaped %v, want %s", reaped, c.ID)
	}
	rec, err := m.Store.GetSandbox(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != store.StateFailed || !strings.Contains(rec.Error, "firecracker exited") {
		t.Errorf("record: %s %q", rec.State, rec.Error)
	}
	for _, p := range []string{vm.JailDir(m.DataDir, c.ID), m.VMs.CgroupPath(c.ID), Dir(m.DataDir, c.ID)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind (%v)", p, err)
		}
	}
	if slices.Contains(netns.Namespaces(), netns.Prefix+c.ID) {
		t.Errorf("namespace of %s left behind", c.ID)
	}
	if st := m.ReapStats(); st.DeadVMs != 1 || st.Sweeps != 2 {
		t.Errorf("stats %+v", st)
	}
	if got := sh(t, m, live.ID, "echo ok"); got != "ok" {
		t.Errorf("the other sandbox: %q", got)
	}
	// Destroying the failed record is fine.
	if err := m.Destroy(ctx, c.ID); err != nil {
		t.Errorf("destroy after reap: %v", err)
	}
}
