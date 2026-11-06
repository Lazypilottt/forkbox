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
