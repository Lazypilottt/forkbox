package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "forkbox.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func TestSandboxLifecycle(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	sb := &Sandbox{ID: "sb-1", Template: "tpl-a", State: StateCreating, Dir: "/d/sb-1", Jail: "/j/sb-1",
		Cgroup: "/sys/fs/cgroup/forkbox/sb-1", Vsock: "/j/sb-1/root/v.sock", TTL: 90 * time.Second}
	if err := s.InsertSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSandbox(ctx, sb); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate insert: %v", err)
	}
	if err := s.SetState(ctx, "sb-1", StateRunning, 4242, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSandbox(ctx, "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateRunning || got.Pid != 4242 || got.TTL != 90*time.Second || got.Vsock != sb.Vsock ||
		!got.CreatedAt.Equal(sb.CreatedAt) || got.UpdatedAt.Before(got.CreatedAt) {
		t.Errorf("got %+v", got)
	}
	if got.Expired(got.CreatedAt.Add(time.Minute)) || !got.Expired(got.CreatedAt.Add(2*time.Minute)) {
		t.Errorf("expiry wrong")
	}
	if err := s.SetState(ctx, "nope", StateFailed, 0, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of a missing id: %v", err)
	}

	// Survives reopening (and migrating again is a no-op).
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	all, err := s2.ListSandboxes(ctx)
	if err != nil || len(all) != 1 || all[0].ID != "sb-1" {
		t.Fatalf("after reopen: %v %v", all, err)
	}
	if err := s2.DeleteSandbox(ctx, "sb-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GetSandbox(ctx, "sb-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	var mode string
	s2.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal mode %q", mode)
	}
}

func TestConcurrentWriters(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "sb-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			if err := s.InsertSandbox(ctx, &Sandbox{ID: id, Template: "t", State: StateCreating}); err != nil {
				errs <- err
				return
			}
			if err := s.SetState(ctx, id, StateRunning, i, ""); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	all, _ := s.ListSandboxes(ctx)
	if len(all) != 50 {
		t.Errorf("%d records", len(all))
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	s, path := open(t)
	s.db.Exec(`PRAGMA user_version = 99`)
	s.Close()
	if _, err := Open(path); err == nil {
		t.Error("opened a database from a newer version")
	}
}

func TestNetSlots(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c", "d"} {
		s.InsertSandbox(ctx, &Sandbox{ID: id, Template: "t", State: StateRunning})
	}
	got := map[string]int{}
	for _, id := range []string{"a", "b", "c"} {
		slot, err := s.AllocSlot(ctx, id, 3)
		if err != nil {
			t.Fatal(err)
		}
		got[id] = slot
	}
	if got["a"] != 0 || got["b"] != 1 || got["c"] != 2 {
		t.Errorf("slots %v", got)
	}
	if again, _ := s.AllocSlot(ctx, "b", 3); again != 1 {
		t.Errorf("re-alloc gave %d", again)
	}
	if _, err := s.AllocSlot(ctx, "d", 3); !errors.Is(err, ErrNoSlot) {
		t.Errorf("exhausted pool: %v", err)
	}
	// Freed slots are reused, lowest first; deleting a sandbox frees its slot.
	s.FreeSlot(ctx, "b")
	if slot, _ := s.AllocSlot(ctx, "d", 3); slot != 1 {
		t.Errorf("reuse gave %d", slot)
	}
	s.DeleteSandbox(ctx, "a")
	if _, err := s.Slot(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("slot survived its sandbox: %v", err)
	}
	if slot, err := s.Slot(ctx, "d"); err != nil || slot != 1 {
		t.Errorf("slot of d: %d %v", slot, err)
	}
}

func TestPortForwards(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	s.InsertSandbox(ctx, &Sandbox{ID: "a", Template: "t", State: StateRunning})
	s.InsertSandbox(ctx, &Sandbox{ID: "b", Template: "t", State: StateRunning})
	if err := s.AddPortForward(ctx, PortForward{"a", 8000, 18000}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPortForward(ctx, PortForward{"b", 9000, 18000}); !errors.Is(err, ErrExists) {
		t.Errorf("host port reused: %v", err)
	}
	s.AddPortForward(ctx, PortForward{"b", 9000, 19000})
	all, _ := s.PortForwards(ctx, "")
	mine, _ := s.PortForwards(ctx, "b")
	if len(all) != 2 || len(mine) != 1 || mine[0].HostPort != 19000 {
		t.Errorf("all %v mine %v", all, mine)
	}
	s.DeleteSandbox(ctx, "a")
	if all, _ = s.PortForwards(ctx, ""); len(all) != 1 {
		t.Errorf("forward survived its sandbox: %v", all)
	}
}

func TestLayers(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	s.InsertSandbox(ctx, &Sandbox{ID: "p", Template: "tpl-x", State: StateRunning, BaseLayer: "tpl-x"})
	for i, l := range []*Layer{
		{ID: "L1", Parent: "tpl-x", Kind: LayerFork, Dir: "/l/L1", Mem: "/l/L1/mem", DirtyBytes: 100, SourceSandbox: "p"},
		{ID: "L2", Parent: "L1", Kind: LayerFork, Dir: "/l/L2", Mem: "/l/L2/mem", DirtyBytes: 7, SourceSandbox: "p"},
	} {
		if err := s.InsertLayer(ctx, l); err != nil {
			t.Fatal(i, err)
		}
	}
	if err := s.InsertLayer(ctx, &Layer{ID: "L1", Kind: LayerFork}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate layer: %v", err)
	}
	lin, err := s.Lineage(ctx, "L2")
	if err != nil || len(lin) != 2 || lin[0].ID != "L2" || lin[1].ID != "L1" || lin[1].Parent != "tpl-x" {
		t.Fatalf("lineage %v %v", lin, err)
	}
	if err := s.SetBaseLayer(ctx, "p", "L2"); err != nil {
		t.Fatal(err)
	}
	if sb, _ := s.GetSandbox(ctx, "p"); sb.BaseLayer != "L2" {
		t.Errorf("base layer %q", sb.BaseLayer)
	}
	if _, err := s.GetLayer(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing layer: %v", err)
	}
}

func TestCheckpointsAndLayerRefs(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	s.InsertSandbox(ctx, &Sandbox{ID: "a", Template: "tpl", State: StateRunning, BaseLayer: "L2"})
	for _, l := range []string{"L1", "L2", "L3"} {
		s.InsertLayer(ctx, &Layer{ID: l, Kind: LayerFork, Dir: "/l/" + l, Mem: "/l/" + l + "/mem"})
	}
	cp := &Checkpoint{ID: "cp-1", SandboxID: "a", Name: "before", Layer: "L1", Dir: "/c/1", Template: "tpl", TemplateName: "python"}
	if err := s.InsertCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCheckpoint(ctx, &Checkpoint{ID: "cp-2", SandboxID: "a", Name: "before", Layer: "L1"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate name: %v", err)
	}
	for _, key := range []string{"before", "cp-1"} {
		if got, err := s.GetCheckpoint(ctx, "a", key); err != nil || got.ID != "cp-1" || got.Layer != "L1" {
			t.Errorf("get %q: %v %v", key, got, err)
		}
	}
	un, _ := s.UnreferencedLayers(ctx)
	if len(un) != 1 || un[0].ID != "L3" {
		t.Errorf("unreferenced %v", un)
	}
	if n, _ := s.LayerRefs(ctx, "L1"); n != 1 {
		t.Errorf("refs of L1: %d", n)
	}
	s.DeleteCheckpoint(ctx, "cp-1")
	s.DeleteSandbox(ctx, "a")
	un, _ = s.UnreferencedLayers(ctx)
	if len(un) != 3 {
		t.Errorf("after deleting everything, %d unreferenced", len(un))
	}
	if list, _ := s.ListCheckpoints(ctx, ""); len(list) != 0 {
		t.Errorf("checkpoints left: %v", list)
	}
}
