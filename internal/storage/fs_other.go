//go:build !linux

package storage

import "fmt"

// FSType needs Linux statfs magic numbers.
func FSType(path string) (string, error) {
	return "", fmt.Errorf("statfs %s: %w", path, ErrUnsupportedOS)
}

// Reflink needs the Linux FICLONE ioctl.
func Reflink(src, dst string) error {
	return fmt.Errorf("reflink %s -> %s: %w", src, dst, ErrUnsupportedOS)
}

// MergeStats says how much of a diff was applied.
type MergeStats struct {
	Extents      int
	Bytes        int64
	ClonedBytes  int64
	CopiedBytes  int64
	BaseSize     int64
	DiffAllocate int64
}

// MergeDiff needs Linux.
func MergeDiff(base, diff, out string) (*MergeStats, error) { return nil, ErrUnsupportedOS }
