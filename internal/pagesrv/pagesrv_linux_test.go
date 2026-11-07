package pagesrv

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const pages = 16

// layerFile writes a sparse file of `pages` pages holding only the given
// pages, each filled with fill(p).
func layerFile(t *testing.T, dir, name string, present []int, fill func(p int) byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(pages * PageSize); err != nil {
		t.Fatal(err)
	}
	for _, pg := range present {
		if _, err := f.WriteAt(bytes.Repeat([]byte{fill(pg)}, PageSize), int64(pg)*PageSize); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// A chain: a template base with every page but 10 (a hole: a zero page),
// a first fork's diff with pages 3 and 4, and a second fork's diff with 4.
func testChain(t *testing.T) (*Chain, func(p int) []byte) {
	dir := t.TempDir()
	var all []int
	for p := 0; p < pages; p++ {
		if p != 10 {
			all = append(all, p)
		}
	}
	base := layerFile(t, dir, "base", all, func(p int) byte { return byte(0x10 + p) })
	d1 := layerFile(t, dir, "d1", []int{3, 4}, func(p int) byte { return byte(0xA0 + p) })
	d2 := layerFile(t, dir, "d2", []int{4}, func(p int) byte { return byte(0xB0 + p) })
	c, err := NewChain([]string{d2, d1, base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	want := func(p int) []byte {
		switch {
		case p == 4:
			return bytes.Repeat([]byte{0xB4}, PageSize)
		case p == 3:
			return bytes.Repeat([]byte{0xA3}, PageSize)
		case p == 10:
			return make([]byte, PageSize)
		default:
			return bytes.Repeat([]byte{byte(0x10 + p)}, PageSize)
		}
	}
	return c, want
}

func TestChainLookup(t *testing.T) {
	c, want := testChain(t)
	if c.Size() != pages*PageSize {
		t.Fatalf("size %d", c.Size())
	}
	for p := 0; p < pages; p++ {
		got := c.Page(int64(p))
		if p == 10 {
			if got != nil {
				t.Errorf("page 10 should be a zero page")
			}
			continue
		}
		if !bytes.Equal(got, want(p)) {
			t.Errorf("page %d comes from the wrong layer: %#x", p, got[0])
		}
	}
	if n := c.Layers[0].Pages(); n != 1 {
		t.Errorf("newest layer holds %d pages, want 1", n)
	}
	if !c.Layers[1].Has(3) || c.Layers[1].Has(5) {
		t.Errorf("diff 1 index wrong")
	}
	if _, err := NewChain(nil); err == nil {
		t.Error("empty chain accepted")
	}
}

// userfaultfd for this process, user-mode faults only (no privilege needed).
func testUffd(t *testing.T, length int) (int, []byte) {
	t.Helper()
	// The test faults in its own process: a thread stuck in a page fault
	// keeps its P, so the handler goroutine needs another one.
	if runtime.GOMAXPROCS(0) < 2 {
		runtime.GOMAXPROCS(2)
	}
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly), 0, 0)
	if errno != 0 {
		t.Skipf("userfaultfd not available to this process: %v", errno)
	}
	api := struct{ api, features, ioctls uint64 }{api: uffdAPI}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uffdioAPI, uintptr(unsafe.Pointer(&api))); e != 0 {
		t.Fatalf("UFFDIO_API: %v", e)
	}
	mem, err := unix.Mmap(-1, 0, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	reg := struct{ start, len, mode, ioctls uint64 }{start: uint64(uintptr(unsafe.Pointer(&mem[0]))), len: uint64(length), mode: uffdioRegisterModeMissing}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uffdioRegister, uintptr(unsafe.Pointer(&reg))); e != 0 {
		t.Fatalf("UFFDIO_REGISTER: %v", e)
	}
	t.Cleanup(func() { unix.Munmap(mem); unix.Close(int(fd)) })
	return int(fd), mem
}

func TestServeFaults(t *testing.T) {
	c, want := testChain(t)
	fd, mem := testUffd(t, pages*PageSize)
	base := uint64(uintptr(unsafe.Pointer(&mem[0])))
	h, err := NewHandler(fd, []Region{{BaseHostVirtAddr: base, Size: pages * PageSize, Offset: 0, PageSize: PageSize}}, c)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- h.Serve(stop) }()

	// Every read faults once and is served from the right layer.
	for p := 0; p < pages; p++ {
		if got := mem[p*PageSize : (p+1)*PageSize]; !bytes.Equal(got, want(p)) {
			t.Errorf("page %d: %#x", p, got[0])
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if h.Stats.Faults.Load() != pages || h.Stats.Zeroed.Load() != 1 || h.Stats.Copied.Load() != pages-1 {
		t.Errorf("faults %d, copied %d, zeroed %d", h.Stats.Faults.Load(), h.Stats.Copied.Load(), h.Stats.Zeroed.Load())
	}
	lat := h.Stats.FaultLatencies()
	if len(lat) != pages {
		t.Errorf("%d latencies", len(lat))
	}
	t.Logf("fault latency: min %s, median %s, max %s", lat[0], lat[len(lat)/2], lat[len(lat)-1])
}

func TestPrefetch(t *testing.T) {
	c, want := testChain(t)
	fd, mem := testUffd(t, pages*PageSize)
	base := uint64(uintptr(unsafe.Pointer(&mem[0])))
	h, _ := NewHandler(fd, []Region{{BaseHostVirtAddr: base, Size: pages * PageSize, PageSize: PageSize}}, c)
	if err := h.Prefetch(); err != nil {
		t.Fatal(err)
	}
	if h.Stats.Prefetched.Load() != 1 {
		t.Fatalf("prefetched %d pages, want the newest layer's 1", h.Stats.Prefetched.Load())
	}
	stop := make(chan struct{})
	go h.Serve(stop)
	defer close(stop)
	if !bytes.Equal(mem[4*PageSize:5*PageSize], want(4)) { // no fault: already there
		t.Error("prefetched page wrong")
	}
	if !bytes.Equal(mem[3*PageSize:4*PageSize], want(3)) {
		t.Error("faulted page wrong")
	}
	if f := h.Stats.Faults.Load(); f != 1 {
		t.Errorf("%d faults, want 1 (page 4 was prefetched)", f)
	}
}

func TestHandlerRejectsBadRegions(t *testing.T) {
	c, _ := testChain(t)
	if _, err := NewHandler(-1, []Region{{Size: PageSize, PageSize: 2 << 20}}, c); err == nil {
		t.Error("huge pages accepted")
	}
	if _, err := NewHandler(-1, []Region{{Offset: pages * PageSize, Size: PageSize, PageSize: PageSize}}, c); err == nil {
		t.Error("region past the file accepted")
	}
}
