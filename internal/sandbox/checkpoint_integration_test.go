//go:build integration

package sandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

func TestIntegrationCheckpointRewind(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sb, err := m.Create(ctx, CreateOptions{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{sb.ID}
	t.Cleanup(func() {
		for _, id := range ids {
			m.Destroy(context.Background(), id)
		}
	})
	id := sb.ID
	ac := m.Agent(id)
	if _, err := ac.WriteFile(ctx, "/usr/local/bin/fbx-counter", strings.NewReader(counterScript), agentclient.WriteOptions{Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	project := sh(t, m, id, `mkdir -p /root/project/src && cd /root/project &&
		echo 'print("hello")' > src/main.py && seq 1 50000 > data.txt && git init -q . &&
		git -c user.email=t@t -c user.name=t add . && git -c user.email=t@t -c user.name=t commit -qm init &&
		find /root/project -type f -not -path '*/.git/*' | sort | xargs sha256sum | sha256sum | cut -d' ' -f1`)
	sh(t, m, id, "setsid fbx-counter >/dev/null 2>&1 < /dev/null &")
	sh(t, m, id, "echo served > /root/project/index.txt; cd /root/project && setsid python -m http.server 8000 >/dev/null 2>&1 < /dev/null &")
	if ok, err := ac.WaitPort(ctx, 8000, 20*time.Second); err != nil || !ok {
		t.Fatalf("server: %v %v", ok, err)
	}
	port, err := m.Expose(ctx, id, 8000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for counter(t, m, id) < 10 {
		time.Sleep(100 * time.Millisecond)
	}

	atCheckpoint := counter(t, m, id)
	cp, tm, err := m.Checkpoint(ctx, id, "before")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checkpoint %s: paused %s (pause %s, diff %s, disk %s, resume %s), merge %s, %.1f MiB dirty",
		cp.ID, tm.ParentPaused, tm.Pause, tm.Snapshot, tm.Disks, tm.Resume, tm.Merge, mb(tm.DirtyBytes))
	if _, _, err := m.Checkpoint(ctx, id, "before"); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate name: %v", err)
	}

	// Destroy the work, kill the processes.
	// Brackets keep the patterns from matching the shell running them.
	sh(t, m, id, "rm -rf /root/project; pkill -f [f]bx-counter; pkill -f [h]ttp.server; sleep 0.3")
	if out := sh(t, m, id, "ls /root/project 2>&1 || true"); !strings.Contains(out, "No such file") {
		t.Fatalf("project not removed: %q", out)
	}
	if out := sh(t, m, id, "pgrep -f [f]bx-counter || true; pgrep -f [h]ttp.server || true"); out != "" {
		t.Fatalf("processes not killed: %q", out)
	}

	pidBefore := mustRecord(t, m, id).Pid
	c, err := m.Rewind(ctx, id, "before")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rewind in %s", c.Timings.Total)
	rec := mustRecord(t, m, id)
	if rec.State != store.StateRunning || rec.Pid == pidBefore || rec.BaseLayer != cp.Layer {
		t.Errorf("after rewind: %+v", rec)
	}
	got := sh(t, m, id, `find /root/project -type f -not -path '*/.git/*' -not -name index.txt | sort | xargs sha256sum | sha256sum | cut -d' ' -f1`)
	if got != project {
		t.Errorf("project after rewind hashes to %s, before %s", got, project)
	}
	if log := sh(t, m, id, "cd /root/project && git log --oneline | wc -l"); log != "1" {
		t.Errorf("git history: %q", log)
	}
	if sh(t, m, id, "pgrep -f [f]bx-counter | head -1") == "" {
		t.Error("counter process not back")
	}
	first := counter(t, m, id)
	time.Sleep(300 * time.Millisecond)
	if second := counter(t, m, id); first < atCheckpoint || second <= first {
		t.Errorf("counter after rewind %d then %d; it was %d at the checkpoint", first, second, atCheckpoint)
	}
	if h := sh(t, m, id, "hostname"); h != id {
		t.Errorf("hostname %q", h)
	}
	// Same network slot: the port forward made before still serves.
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/index.txt")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) == "served\n" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("port forward after rewind: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	twin, err := m.RestoreAsNew(ctx, id, "before", 0)
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, twin.ID)
	if got := sh(t, m, twin.ID, `find /root/project -type f -not -path '*/.git/*' -not -name index.txt | sort | xargs sha256sum | sha256sum | cut -d' ' -f1`); got != project {
		t.Errorf("restored-as-new project %s", got)
	}
	if list, _ := m.Checkpoints(ctx, id); len(list) != 1 || list[0].Name != "before" {
		t.Errorf("checkpoints %v", list)
	}
}

func freeBytes(t *testing.T, dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	return int64(st.Bavail) * st.Bsize
}

func TestIntegrationCheckpointGC(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sb, err := m.Create(ctx, CreateOptions{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	// 128 MiB that exists only in this sandbox's memory and disk.
	sh(t, m, sb.ID, "head -c 134217728 /dev/urandom > /root/data.bin && sync")
	cp, tm, err := m.Checkpoint(ctx, sb.ID, "a")
	if err != nil {
		m.Destroy(ctx, sb.ID)
		t.Fatal(err)
	}
	l, err := m.Store.GetLayer(ctx, cp.Layer)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checkpoint a: %.1f MiB dirty", mb(tm.DirtyBytes))
	if err := m.Destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	// The checkpoint still holds the layer.
	if _, err := os.Stat(l.Mem); err != nil {
		t.Fatalf("layer gone while a checkpoint uses it: %v", err)
	}
	syscall.Sync()
	before := freeBytes(t, m.DataDir)
	if err := m.DeleteCheckpoint(ctx, sb.ID, "a"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{l.Dir, cp.Dir} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists", p)
		}
	}
	if _, err := m.Store.GetLayer(ctx, cp.Layer); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("layer record survived: %v", err)
	}
	// XFS frees blocks in the background; give it a moment.
	var freed int64
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		syscall.Sync()
		if freed = freeBytes(t, m.DataDir) - before; freed >= 100<<20 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("deleting the checkpoint freed %.1f MiB on %s", mb(freed), filepath.Base(m.DataDir))
	if freed < 100<<20 {
		t.Errorf("only %.1f MiB freed, want at least 100 MiB of the 128 MiB written", mb(freed))
	}
}

func mustRecord(t *testing.T, m *Manager, id string) *store.Sandbox {
	t.Helper()
	r, err := m.Store.GetSandbox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
