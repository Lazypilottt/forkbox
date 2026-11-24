package storage

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// copySparse copies src's data extents into dst at the same offsets: a new
// file of src's size (holes stay holes), or an existing block device at
// least as large. Nothing is written for src's holes, so dst must read
// zeros there (a new file, a new thin volume).
func copySparse(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	ist, err := in.Stat()
	if err != nil {
		return err
	}
	var out *os.File
	if st, err := os.Stat(dst); err == nil && st.Mode()&os.ModeDevice != 0 {
		out, err = os.OpenFile(dst, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
	} else {
		out, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, ist.Mode().Perm())
		if err != nil {
			return err
		}
		if err := out.Truncate(ist.Size()); err != nil {
			out.Close()
			return err
		}
	}
	defer out.Close()
	ifd, ofd := int(in.Fd()), int(out.Fd())
	useCFR := true
	buf := make([]byte, 1<<20)
	var off int64
	for off < ist.Size() {
		start, err := unix.Seek(ifd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return fmt.Errorf("copy %s: SEEK_DATA: %w", src, err)
		}
		end, err := unix.Seek(ifd, start, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("copy %s: SEEK_HOLE: %w", src, err)
		}
		if useCFR {
			err := copyRange(ifd, ofd, start, end-start)
			if err == nil {
				off = end
				continue
			}
			if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.EOPNOTSUPP) {
				return err
			}
			useCFR = false // e.g. into a block device
		}
		for p := start; p < end; {
			n, err := in.ReadAt(buf[:min(int64(len(buf)), end-p)], p)
			if n > 0 {
				if _, werr := out.WriteAt(buf[:n], p); werr != nil {
					return werr
				}
				p += int64(n)
			}
			if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
				return fmt.Errorf("copy %s at %d: %w", src, p, err)
			}
		}
		off = end
	}
	return out.Sync()
}

// ReflinkOrCopy reflinks src to dst, or makes a sparse copy where the
// filesystem cannot reflink.
func ReflinkOrCopy(src, dst string) error {
	if err := Reflink(src, dst); !errors.Is(err, ErrNoReflink) {
		return err
	}
	return copySparse(src, dst)
}
