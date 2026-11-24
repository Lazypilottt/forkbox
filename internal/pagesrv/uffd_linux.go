package pagesrv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The userfaultfd ABI (include/uapi/linux/userfaultfd.h, x86_64).
const (
	uffdioCopy     = 0xc028aa03 // _IOWR(0xAA, 0x03, struct uffdio_copy)
	uffdioZeropage = 0xc020aa04 // _IOWR(0xAA, 0x04, struct uffdio_zeropage)
	uffdioAPI      = 0xc018aa3f // _IOWR(0xAA, 0x3F, struct uffdio_api)
	uffdioRegister = 0xc020aa00 // _IOWR(0xAA, 0x00, struct uffdio_register)
	uffdioWake     = 0x8010aa02 // _IOR(0xAA, 0x02, struct uffdio_range)

	uffdAPI                   = 0xAA
	uffdUserModeOnly          = 1
	uffdioRegisterModeMissing = 1

	eventPagefault = 0x12
	eventFork      = 0x13
	eventRemap     = 0x14
	eventRemove    = 0x15
	eventUnmap     = 0x16

	msgSize = 32 // sizeof(struct uffd_msg)
)

type uffdioCopyArg struct {
	dst, src, len, mode uint64
	copy                int64
}

type uffdioZeropageArg struct {
	start, len, mode uint64
	zeropage         int64
}

// Region is one guest memory region as Firecracker describes it.
type Region struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
}

// Stats are a handler's counters.
type Stats struct {
	Faults     atomic.Uint64 // page fault events served
	LastFault  atomic.Int64  // unix nanoseconds of the last fault served
	Deferred   atomic.Int64  // faults waiting for a retry (EAGAIN)
	Copied     atomic.Uint64 // pages copied from a layer
	Zeroed     atomic.Uint64 // zero pages
	Prefetched atomic.Uint64 // pages copied ahead of any fault
	Removed    atomic.Uint64 // pages released by the balloon
	Retries    atomic.Uint64 // EAGAIN: retried after pending events
	latMu      sync.Mutex
	latencies  []time.Duration // per fault, capped
}

// FaultLatencies returns the recorded fault-handling times, sorted.
func (s *Stats) FaultLatencies() []time.Duration {
	s.latMu.Lock()
	defer s.latMu.Unlock()
	out := append([]time.Duration(nil), s.latencies...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Stats) record(d time.Duration) {
	s.latMu.Lock()
	if len(s.latencies) < 1<<20 {
		s.latencies = append(s.latencies, d)
	}
	s.latMu.Unlock()
}

// Handler serves one VM's page faults from a chain.
type Handler struct {
	uffd    int
	regions []Region
	chain   *Chain
	removed map[uint64]bool // guest pages (by offset) released by the balloon
	huge    []byte          // scratch for assembling a 2 MiB page
	zero    []byte          // a zero 2 MiB page (hugetlbfs has no UFFDIO_ZEROPAGE)
	Stats   Stats
}

// HugePageSize is the hugetlbfs page size served (Firecracker's "2M").
const HugePageSize = 2 << 20

// NewHandler serves regions over uffd from chain.
func NewHandler(uffd int, regions []Region, chain *Chain) (*Handler, error) {
	huge := false
	for _, r := range regions {
		if r.PageSize != PageSize && r.PageSize != HugePageSize {
			return nil, fmt.Errorf("pagesrv: region page size %d; only %d and %d are supported", r.PageSize, PageSize, HugePageSize)
		}
		if r.Offset%r.PageSize != 0 || r.Size%r.PageSize != 0 || r.BaseHostVirtAddr%r.PageSize != 0 {
			return nil, fmt.Errorf("pagesrv: region [%#x, +%#x) is not aligned to its page size %d", r.Offset, r.Size, r.PageSize)
		}
		huge = huge || r.PageSize == HugePageSize
		if int64(r.Offset+r.Size) > chain.Size() {
			return nil, fmt.Errorf("pagesrv: region [%#x, +%#x) is past the memory file (%d bytes)", r.Offset, r.Size, chain.Size())
		}
	}
	h := &Handler{uffd: uffd, regions: regions, chain: chain, removed: map[uint64]bool{}}
	if huge {
		h.huge = make([]byte, HugePageSize)
		h.zero = make([]byte, HugePageSize)
	}
	return h, nil
}

// hugePage assembles the 2 MiB page at file offset off from the chain's
// 4 KiB pages (diff layers are 4 KiB-granular even for hugepage memory).
// nil means every part is zero.
func (h *Handler) hugePage(off uint64) []byte {
	first := int64(off / PageSize)
	any := false
	for i := range int64(HugePageSize / PageSize) {
		dst := h.huge[i*PageSize : (i+1)*PageSize]
		if src := h.chain.Page(first + i); src != nil {
			copy(dst, src)
			any = true
		} else {
			clear(dst)
		}
	}
	if !any {
		return nil
	}
	return h.huge
}

// Prefetch copies the pages of the newest layer (what the parent touched
// last before the fork) before the guest asks for them.
func (h *Handler) Prefetch() error {
	l := h.chain.Layers[0]
	for _, r := range h.regions {
		first := int64(r.Offset / PageSize)
		if r.PageSize == HugePageSize {
			per := int64(HugePageSize / PageSize)
			for hp := first; hp < first+int64(r.Size/PageSize); hp += per {
				touched := false
				for p := hp; p < hp+per && !touched; p++ {
					touched = l.Has(p)
				}
				if !touched {
					continue
				}
				addr := r.BaseHostVirtAddr + uint64(hp-first)*PageSize
				ok, err := h.copyPageSize(addr, h.hugePage(uint64(hp)*PageSize), HugePageSize)
				if err != nil {
					return err
				}
				if ok {
					h.Stats.Prefetched.Add(1)
				}
			}
			continue
		}
		for p := first; p < first+int64(r.Size/PageSize); p++ {
			if !l.Has(p) {
				continue
			}
			addr := r.BaseHostVirtAddr + uint64(p-first)*PageSize
			ok, err := h.copyPage(addr, h.chain.Page(p))
			if err != nil {
				return err
			}
			if ok {
				h.Stats.Prefetched.Add(1)
			}
		}
	}
	return nil
}

// Serve handles events until stop is closed or the userfaultfd fails
// (Firecracker exited).
func (h *Handler) Serve(stop <-chan struct{}) error {
	buf := make([]byte, msgSize*64)
	var deferred []uint64
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		fds := []unix.PollFd{{Fd: int32(h.uffd), Events: unix.POLLIN}}
		timeout := 100
		if len(deferred) > 0 {
			timeout = 1
		}
		if _, err := unix.Poll(fds, timeout); err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("pagesrv: poll: %w", err)
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return nil // the VM's memory is gone
		}
		var faults []uint64
		faults, deferred = deferred, nil
		// Read every pending event first: while a remove is queued, every
		// ioctl returns EAGAIN.
		for {
			n, err := unix.Read(h.uffd, buf)
			if errors.Is(err, unix.EAGAIN) || n == 0 {
				break
			}
			if err != nil {
				if errors.Is(err, unix.EINTR) {
					continue
				}
				return fmt.Errorf("pagesrv: read: %w", err)
			}
			for off := 0; off+msgSize <= n; off += msgSize {
				m := buf[off : off+msgSize]
				switch m[0] {
				case eventPagefault:
					faults = append(faults, binary.NativeEndian.Uint64(m[16:24]))
				case eventRemove:
					start, end := binary.NativeEndian.Uint64(m[8:16]), binary.NativeEndian.Uint64(m[16:24])
					ps := h.pageSize(start)
					for a := start &^ (ps - 1); a < end; a += ps {
						if off, ok := h.fileOffset(a); ok {
							h.removed[off] = true
							h.Stats.Removed.Add(1)
						}
					}
				case eventFork, eventRemap, eventUnmap:
					// Not requested by Firecracker; nothing to serve.
				}
			}
		}
		for _, addr := range faults {
			t := time.Now()
			ok, err := h.serve(addr)
			if err != nil {
				return err
			}
			if !ok {
				h.Stats.Retries.Add(1)
				deferred = append(deferred, addr)
				continue
			}
			h.Stats.Faults.Add(1)
			h.Stats.LastFault.Store(time.Now().UnixNano())
			h.Stats.record(time.Since(t))
		}
		h.Stats.Deferred.Store(int64(len(deferred)))
	}
}

// fileOffset maps a guest host-virtual address to its offset in the
// memory file.
func (h *Handler) fileOffset(addr uint64) (uint64, bool) {
	for _, r := range h.regions {
		if addr >= r.BaseHostVirtAddr && addr < r.BaseHostVirtAddr+r.Size {
			return r.Offset + (addr - r.BaseHostVirtAddr), true
		}
	}
	return 0, false
}

// pageSize is the page size of the region holding addr.
func (h *Handler) pageSize(addr uint64) uint64 {
	for _, r := range h.regions {
		if addr >= r.BaseHostVirtAddr && addr < r.BaseHostVirtAddr+r.Size {
			return r.PageSize
		}
	}
	return PageSize
}

// serve resolves one fault; false means try again later (EAGAIN).
func (h *Handler) serve(addr uint64) (bool, error) {
	ps := h.pageSize(addr)
	page := addr &^ (ps - 1)
	off, ok := h.fileOffset(page)
	if !ok {
		return false, fmt.Errorf("pagesrv: fault at %#x is outside every region", addr)
	}
	if ps == HugePageSize {
		var data []byte
		if !h.removed[off] {
			data = h.hugePage(off)
		}
		return h.copyPageSize(page, data, HugePageSize)
	}
	var data []byte
	if !h.removed[off] {
		data = h.chain.Page(int64(off / PageSize))
	}
	return h.copyPage(page, data)
}

// copyPageSize fills a page of size ps; hugetlbfs pages are always copied
// (UFFDIO_ZEROPAGE does not support them).
func (h *Handler) copyPageSize(addr uint64, data []byte, ps uint64) (bool, error) {
	if ps == PageSize {
		return h.copyPage(addr, data)
	}
	zero := data == nil
	if zero {
		data = h.zero
	}
	a := uffdioCopyArg{dst: addr, src: uint64(uintptr(unsafe.Pointer(&data[0]))), len: ps}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uffdioCopy, uintptr(unsafe.Pointer(&a)))
	switch errno {
	case 0:
		if zero {
			h.Stats.Zeroed.Add(1)
		} else {
			h.Stats.Copied.Add(1)
		}
		return true, nil
	case unix.EEXIST:
		return true, h.wake(addr, ps)
	case unix.EAGAIN:
		return false, nil
	default:
		return false, fmt.Errorf("pagesrv: filling huge page %#x: %w", addr, errno)
	}
}

// copyPage copies data (or a zero page when nil) to addr and wakes the
// faulting thread. EEXIST (already present) counts as done.
func (h *Handler) copyPage(addr uint64, data []byte) (bool, error) {
	var errno unix.Errno
	if data == nil {
		a := uffdioZeropageArg{start: addr, len: PageSize}
		_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uffdioZeropage, uintptr(unsafe.Pointer(&a)))
		if errno == 0 {
			h.Stats.Zeroed.Add(1)
		}
	} else {
		a := uffdioCopyArg{dst: addr, src: uint64(uintptr(unsafe.Pointer(&data[0]))), len: PageSize}
		_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uffdioCopy, uintptr(unsafe.Pointer(&a)))
		if errno == 0 {
			h.Stats.Copied.Add(1)
		}
	}
	switch errno {
	case 0:
		return true, nil
	case unix.EEXIST:
		return true, h.wake(addr, PageSize)
	case unix.EAGAIN:
		return false, nil
	default:
		return false, fmt.Errorf("pagesrv: filling page %#x: %w", addr, errno)
	}
}

// wake wakes threads waiting on a page that is already present: a failed
// UFFDIO_COPY (EEXIST) does not wake them, and a thread whose fault raced
// with another fill of the same page would otherwise sleep forever.
func (h *Handler) wake(addr, size uint64) error {
	r := struct{ start, len uint64 }{addr, size}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uffdioWake, uintptr(unsafe.Pointer(&r))); errno != 0 {
		return fmt.Errorf("pagesrv: waking %#x: %w", addr, errno)
	}
	return nil
}
