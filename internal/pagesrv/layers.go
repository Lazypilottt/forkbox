// Package pagesrv is the userfaultfd page server: it serves a restored
// VM's memory, page by page as the guest touches it, from a chain of
// layers (the VM's own diffs, its parent's, down to the template's full
// memory file), so a fork never copies or merges memory.
//
// Handshake (docs/snapshotting/handling-page-faults-on-snapshot-resume.md
// and src/firecracker/examples/uffd in v1.17.0):
//  1. The handler listens on a Unix socket inside the jail.
//  2. PUT /snapshot/load names it: mem_backend {backend_type: "Uffd",
//     backend_path: <socket>}.
//  3. Firecracker creates the userfaultfd, maps guest memory anonymously,
//     registers it, connects, and sends one message: a JSON array of
//     regions {base_host_virt_addr, size, offset, page_size} with the
//     userfaultfd attached as SCM_RIGHTS. Nothing else is ever sent.
//  4. The handler reads uffd_msg events: for a page fault it UFFDIO_COPYs
//     the page from the newest layer that has it (EEXIST means already
//     there; EAGAIN means retry after pending remove events); for
//     UFFD_EVENT_REMOVE (the balloon) it remembers the range and serves
//     zero pages there from then on.
//  5. If the handler dies, Firecracker blocks on the next fault forever, so
//     the handler kills Firecracker on any fatal error, and the daemon kills
//     the VM if the handler process disappears.
package pagesrv

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// PageSize is the guest page size served (4 KiB; hugepages come in Task 14).
const PageSize = 4096

// Layer is one memory file, mapped read-only and shared, so every VM that
// uses it reads the same page cache.
type Layer struct {
	Path string
	data []byte
	// present lists the pages this layer holds: for a diff, its data
	// extents; for a full file, everything that is not a hole.
	present []uint64 // bitset by page index
	pages   int64
}

// OpenLayer maps path and indexes its pages from its data extents
// (SEEK_DATA/SEEK_HOLE): a diff snapshot's memory file holds exactly the
// dirty pages, at their guest offsets.
func OpenLayer(path string) (*Layer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	l := &Layer{Path: path, pages: (st.Size() + PageSize - 1) / PageSize}
	l.present = make([]uint64, (l.pages+63)/64)
	fd := int(f.Fd())
	var off int64
	for off < st.Size() {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: SEEK_DATA: %w", path, err)
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("%s: SEEK_HOLE: %w", path, err)
		}
		for p := start / PageSize; p < (end+PageSize-1)/PageSize; p++ {
			l.present[p/64] |= 1 << (p % 64)
		}
		off = end
	}
	if st.Size() > 0 {
		l.data, err = unix.Mmap(fd, 0, int(st.Size()), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return nil, fmt.Errorf("%s: mmap: %w", path, err)
		}
	}
	return l, nil
}

// Has reports whether the layer holds page p.
func (l *Layer) Has(p int64) bool {
	return p >= 0 && p < l.pages && l.present[p/64]&(1<<(p%64)) != 0
}

// Pages is how many pages the layer holds.
func (l *Layer) Pages() int64 {
	var n int64
	for _, w := range l.present {
		n += int64(popcount(w))
	}
	return n
}

// Close unmaps the layer.
func (l *Layer) Close() error {
	if l.data == nil {
		return nil
	}
	err := unix.Munmap(l.data)
	l.data = nil
	return err
}

func popcount(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}

// Chain is a VM's layers, newest first; the last is a full memory file.
// Its owner index says, for each page, which layer to serve it from, so a
// fault costs one lookup however long the chain is.
type Chain struct {
	Layers []*Layer
	owner  []int16 // layer index per page, -1 for a zero page
}

// NewChain opens the layers (newest first) and builds the owner index.
func NewChain(paths []string) (*Chain, error) {
	if len(paths) == 0 || len(paths) > 32000 {
		return nil, fmt.Errorf("pagesrv: chain of %d layers", len(paths))
	}
	c := &Chain{}
	for _, p := range paths {
		l, err := OpenLayer(p)
		if err != nil {
			c.Close()
			return nil, err
		}
		c.Layers = append(c.Layers, l)
	}
	var pages int64
	for _, l := range c.Layers {
		pages = max(pages, l.pages)
	}
	c.owner = make([]int16, pages)
	for i := range c.owner {
		c.owner[i] = -1
	}
	// Oldest first, so newer layers overwrite.
	for i := len(c.Layers) - 1; i >= 0; i-- {
		l := c.Layers[i]
		for w, bits := range l.present {
			for bits != 0 {
				b := bits & -bits
				p := int64(w)*64 + int64(trailingZeros(b))
				c.owner[p] = int16(i)
				bits &^= b
			}
		}
	}
	return c, nil
}

func trailingZeros(x uint64) int {
	n := 0
	for x&1 == 0 {
		x >>= 1
		n++
	}
	return n
}

// Page returns the bytes of page p, or nil for a zero page.
func (c *Chain) Page(p int64) []byte {
	if p < 0 || p >= int64(len(c.owner)) || c.owner[p] < 0 {
		return nil
	}
	l := c.Layers[c.owner[p]]
	off := p * PageSize
	return l.data[off : off+PageSize]
}

// Size is the memory size the chain covers, in bytes.
func (c *Chain) Size() int64 { return int64(len(c.owner)) * PageSize }

// Close unmaps every layer.
func (c *Chain) Close() {
	for _, l := range c.Layers {
		l.Close()
	}
}
