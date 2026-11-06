package storage

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// MergeStats says how much of a diff was applied.
type MergeStats struct {
	Extents      int
	Bytes        int64 // data bytes in the diff (its dirty pages)
	ClonedBytes  int64 // applied by FICLONERANGE: shared extents, no copy
	CopiedBytes  int64 // applied by copy_file_range
	BaseSize     int64
	DiffAllocate int64 // the diff's allocated size on disk
}

// MergeDiff writes out = base with diff's pages on top, the way Firecracker's
// `snapshot-editor edit-memory rebase` does: a diff snapshot's memory file
// is sparse, and its data extents are the dirty pages, at their offsets in
// guest memory. out starts as a reflink of base (instant, shared), and each
// data extent of diff is cloned in with FICLONERANGE (also shared), falling
// back to copy_file_range where a range cannot be cloned. Nothing outside
// the dirty pages is read or written.
func MergeDiff(base, diff, out string) (*MergeStats, error) {
	if err := Reflink(base, out); err != nil {
		return nil, err
	}
	st, err := applyDiff(out, diff)
	if err != nil {
		os.Remove(out)
		return nil, err
	}
	return st, nil
}

func applyDiff(outPath, diffPath string) (*MergeStats, error) {
	out, err := os.OpenFile(outPath, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	in, err := os.Open(diffPath)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	ost, err := out.Stat()
	if err != nil {
		return nil, err
	}
	ist, err := in.Stat()
	if err != nil {
		return nil, err
	}
	st := &MergeStats{BaseSize: ost.Size()}
	if sys, ok := ist.Sys().(*unix.Stat_t); ok {
		st.DiffAllocate = sys.Blocks * 512
	}
	if ist.Size() > ost.Size() {
		return nil, fmt.Errorf("merge: diff %s (%d bytes) is larger than the base (%d bytes)", diffPath, ist.Size(), ost.Size())
	}
	ifd, ofd := int(in.Fd()), int(out.Fd())
	cloneOK := true
	var off int64
	for off < ist.Size() {
		start, err := unix.Seek(ifd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // no data after off
		}
		if err != nil {
			return nil, fmt.Errorf("merge: SEEK_DATA: %w", err)
		}
		end, err := unix.Seek(ifd, start, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("merge: SEEK_HOLE: %w", err)
		}
		n := end - start
		st.Extents++
		st.Bytes += n
		if cloneOK {
			err := unix.IoctlFileCloneRange(ofd, &unix.FileCloneRange{
				Src_fd: int64(ifd), Src_offset: uint64(start), Src_length: uint64(n), Dest_offset: uint64(start),
			})
			if err == nil {
				st.ClonedBytes += n
				off = end
				continue
			}
			if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.ENOTTY) {
				return nil, fmt.Errorf("merge: FICLONERANGE at %d: %w", start, err)
			}
			cloneOK = false // e.g. a filesystem without reflink, or an unaligned tail
		}
		if err := copyRange(ifd, ofd, start, n); err != nil {
			return nil, err
		}
		st.CopiedBytes += n
		off = end
	}
	return st, out.Sync()
}

func copyRange(ifd, ofd int, off, n int64) error {
	for n > 0 {
		ioff, ooff := off, off
		w, err := unix.CopyFileRange(ifd, &ioff, ofd, &ooff, int(min(n, 1<<30)), 0)
		if err != nil {
			return fmt.Errorf("merge: copy_file_range at %d: %w", off, err)
		}
		if w == 0 {
			return io.ErrUnexpectedEOF
		}
		off += int64(w)
		n -= int64(w)
	}
	return nil
}
