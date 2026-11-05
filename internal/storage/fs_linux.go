//go:build linux

package storage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// FSType returns the type of the filesystem holding path, such as "xfs".
func FSType(path string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	switch int64(st.Type) {
	case unix.XFS_SUPER_MAGIC:
		return FSXFS, nil
	case unix.BTRFS_SUPER_MAGIC:
		return FSBtrfs, nil
	case unix.EXT4_SUPER_MAGIC: // also ext2 and ext3, which share the magic
		return FSExt4, nil
	case unix.TMPFS_MAGIC:
		return FSTmpfs, nil
	case unix.OVERLAYFS_SUPER_MAGIC:
		return FSOverlay, nil
	case unix.CGROUP2_SUPER_MAGIC:
		return FSCgroup2, nil
	case unix.CGROUP_SUPER_MAGIC:
		return FSCgroup1, nil
	}
	return fmt.Sprintf("unknown(0x%x)", int64(st.Type)), nil
}

// Reflink creates dst as a copy-on-write clone of src (FICLONE). dst must not
// exist. It never falls back to a byte copy.
func Reflink(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	cloneErr := unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	closeErr := out.Close()
	if cloneErr != nil {
		os.Remove(dst)
		if errors.Is(cloneErr, unix.EOPNOTSUPP) || errors.Is(cloneErr, unix.ENOTTY) ||
			errors.Is(cloneErr, unix.EINVAL) || errors.Is(cloneErr, unix.EXDEV) {
			return fmt.Errorf("reflink %s -> %s: %w (%v)", src, dst, ErrNoReflink, cloneErr)
		}
		return fmt.Errorf("reflink %s -> %s: %w", src, dst, cloneErr)
	}
	if closeErr != nil {
		os.Remove(dst)
		return closeErr
	}
	return nil
}
