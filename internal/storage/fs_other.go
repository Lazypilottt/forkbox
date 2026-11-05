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
