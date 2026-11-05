//go:build linux

package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFSTypeOfTempDir(t *testing.T) {
	got, err := FSType(t.TempDir())
	if err != nil || got == "" {
		t.Fatalf("FSType = %q, %v", got, err)
	}
	if _, err := FSType(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("FSType(missing) error = %v, want ErrNotExist", err)
	}
}

func TestFSTypeCgroup(t *testing.T) {
	got, err := FSType("/sys/fs/cgroup")
	if err != nil {
		t.Skip("no /sys/fs/cgroup:", err)
	}
	if got != FSCgroup2 && got != FSCgroup1 && got != FSTmpfs {
		t.Errorf("FSType(/sys/fs/cgroup) = %q", got)
	}
}

// The temp dir may or may not support reflinks. Either the copy is exact, or
// the error says reflinks are unsupported and nothing is left behind.
func TestReflinkOrCleanRefusal(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	want := bytes.Repeat([]byte("forkbox"), 10000)
	if err := os.WriteFile(src, want, 0o640); err != nil {
		t.Fatal(err)
	}
	fsType, _ := FSType(dir)
	err := Reflink(src, dst)
	if err != nil {
		if !errors.Is(err, ErrNoReflink) {
			t.Fatalf("Reflink on %s: %v, want ErrNoReflink", fsType, err)
		}
		if ReflinkCapable(fsType) {
			t.Logf("note: %s refused a reflink (XFS without reflink=1?)", fsType)
		}
		if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("failed reflink left dst behind")
		}
		if perr := ProbeReflink(dir); !errors.Is(perr, ErrNoReflink) {
			t.Errorf("ProbeReflink = %v, want ErrNoReflink", perr)
		}
		return
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("clone differs: %v", err)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o640 {
		t.Errorf("clone mode %v, want 0640", fi.Mode().Perm())
	}
	if err := ProbeReflink(dir); err != nil {
		t.Errorf("ProbeReflink = %v", err)
	}
}

func TestReflinkRefusesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.WriteFile(src, []byte("a"), 0o644)
	os.WriteFile(dst, []byte("keep me"), 0o644)
	if err := Reflink(src, dst); !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "keep me" {
		t.Error("existing dst was modified")
	}
}

func TestProbeLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	_ = ProbeReflink(dir)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("probe left %d files behind", len(entries))
	}
}
