//go:build integration && linux

package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A pool of its own (fbx-pool-test) in a temporary data directory.
func testThin(t *testing.T) *Thin {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root (make integration)")
	}
	if _, err := exec.LookPath("dmsetup"); err != nil {
		t.Skip("dmsetup is missing")
	}
	dir := t.TempDir()
	th := NewThin(dir, 1<<30)
	th.PoolName = "fbx-pool-test"
	ctx := context.Background()
	if err := th.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out, _ := exec.Command("dmsetup", "ls").Output()
		for _, line := range strings.Split(string(out), "\n") {
			if name, _, _ := strings.Cut(line, "\t"); strings.HasPrefix(name, th.PoolName+"-") {
				t.Errorf("volume %s left active", name)
				exec.Command("dmsetup", "remove", name).Run()
			}
		}
		exec.Command("dmsetup", "remove", th.PoolName).Run()
		for _, f := range []string{"data.img", "meta.img"} {
			if out, err := exec.Command("losetup", "-j", filepath.Join(th.Dir, f)).Output(); err == nil {
				if dev, _, ok := strings.Cut(string(out), ":"); ok {
					exec.Command("losetup", "-d", dev).Run()
				}
			}
		}
	})
	return th
}

func readAt(t *testing.T, path string, off int64, n int) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeAt(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationThin(t *testing.T) {
	th := testThin(t)
	ctx := context.Background()
	work := filepath.Join(th.DataDir, "work")
	os.MkdirAll(work, 0o755)

	// A sparse 64 MiB image with data at 0 and at 40 MiB.
	img := filepath.Join(work, "rootfs.ext4")
	head, mid := make([]byte, 8192), make([]byte, 4096)
	rand.Read(head)
	rand.Read(mid)
	f, _ := os.Create(img)
	f.Truncate(64 << 20)
	f.WriteAt(head, 0)
	f.WriteAt(mid, 40<<20)
	f.Close()

	a := filepath.Join(work, "a", "disk")
	b := filepath.Join(work, "b", "disk")
	os.MkdirAll(filepath.Dir(a), 0o755)
	os.MkdirAll(filepath.Dir(b), 0o755)
	if err := th.Clone(ctx, img, a); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(a); err != nil || fi.Mode()&os.ModeDevice == 0 {
		t.Fatalf("%s: %v %v", a, fi, err)
	}
	if !bytes.Equal(readAt(t, a, 0, len(head)), head) || !bytes.Equal(readAt(t, a, 40<<20, len(mid)), mid) {
		t.Fatal("clone of the image differs from it")
	}
	if !bytes.Equal(readAt(t, a, 20<<20, 4096), make([]byte, 4096)) {
		t.Fatal("a hole of the image is not zero in the clone")
	}

	// Writes to a stay in a; a clone of a sees them; the image is untouched.
	patch := bytes.Repeat([]byte{0xab}, 4096)
	writeAt(t, a, 4096, patch)
	if err := th.Clone(ctx, a, b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAt(t, b, 4096, 4096), patch) {
		t.Error("clone of a lacks a's write")
	}
	writeAt(t, b, 0, patch)
	if bytes.Equal(readAt(t, a, 0, 4096), patch) {
		t.Error("a write to b reached a")
	}
	if got, _ := os.ReadFile(img); !bytes.Equal(got[:len(head)], head) {
		t.Error("the image changed")
	}
	// A second clone of the image reuses its import.
	c := filepath.Join(work, "c")
	if err := th.Clone(ctx, img, c); err != nil {
		t.Fatal(err)
	}
	if imps, _ := filepath.Glob(filepath.Join(th.Dir, "imports", "*.json")); len(imps) != 1 {
		t.Errorf("imports: %v", imps)
	}
	if err := th.Clone(ctx, img, c); err == nil {
		t.Error("clone over an existing disk")
	}

	// Removing frees the volumes; Reap frees an id nothing records.
	if err := th.RemoveAll(ctx, filepath.Join(work, "a")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAt(t, b, 4096, 4096), patch) {
		t.Error("removing a broke its clone b")
	}
	if err := th.Remove(ctx, c); err != nil {
		t.Fatal(err)
	}
	th.mu.Lock()
	stray, err := th.allocID()
	th.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := th.message(ctx, "create_thin "+strconv.Itoa(stray)); err != nil {
		t.Fatal(err)
	}
	freed, err := th.Reap(ctx)
	if err != nil || len(freed) != 1 || freed[0] != th.volName(stray) {
		t.Errorf("reap: %v %v", freed, err)
	}
	if err := th.Remove(ctx, b); err != nil {
		t.Fatal(err)
	}
	os.Remove(img)
	if freed, err := th.Reap(ctx); err != nil || len(freed) != 1 || !strings.HasPrefix(freed[0], "import ") {
		t.Errorf("reap after the image went: %v %v", freed, err)
	}
	if ids, _ := os.ReadDir(filepath.Join(th.Dir, "ids")); len(ids) != 0 {
		t.Errorf("ids left: %v", ids)
	}
}
