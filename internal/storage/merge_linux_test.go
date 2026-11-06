package storage

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// The extent logic, on any Linux filesystem with SEEK_DATA (ext4 in CI's
// unit job: no reflink there, so this exercises the copy fallback).
func TestApplyDiff(t *testing.T) {
	dir := t.TempDir()
	const page = 4096
	const pages = 64
	base := make([]byte, pages*page)
	rand.Read(base)
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(out, base, 0o644); err != nil {
		t.Fatal(err)
	}
	// A sparse diff with pages 3, 4, 10 and 63 dirty.
	diff := filepath.Join(dir, "diff")
	f, _ := os.Create(diff)
	f.Truncate(pages * page)
	want := append([]byte(nil), base...)
	for _, p := range []int{3, 4, 10, 63} {
		b := bytes.Repeat([]byte{byte(p)}, page)
		f.WriteAt(b, int64(p*page))
		copy(want[p*page:], b)
	}
	f.Close()

	st, err := applyDiff(out, diff)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, want) {
		t.Fatal("merged file differs from base plus dirty pages")
	}
	if st.Bytes > 8*page || st.Extents < 1 || st.ClonedBytes+st.CopiedBytes != st.Bytes {
		t.Errorf("stats %+v", st)
	}
	t.Logf("%+v", st)

	big := filepath.Join(dir, "big")
	os.WriteFile(big, make([]byte, (pages+1)*page), 0o644)
	if _, err := applyDiff(out, big); err == nil {
		t.Error("a diff larger than the base was accepted")
	}
}
