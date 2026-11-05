// Package storage manages sandbox disks and memory files on the data directory.
//
// Task 1 adds the filesystem primitives that `forkbox doctor` needs: detecting
// the filesystem type and making a real reflink (copy-on-write) copy.
package storage

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Filesystem type names returned by FSType.
const (
	FSXFS     = "xfs"
	FSBtrfs   = "btrfs"
	FSExt4    = "ext4"
	FSTmpfs   = "tmpfs"
	FSOverlay = "overlay"
	FSCgroup2 = "cgroup2"
	FSCgroup1 = "cgroup"
)

// ErrNoReflink means the filesystem refused a reflink copy.
var ErrNoReflink = errors.New("filesystem does not support reflink copies")

// ErrUnsupportedOS is returned by the Linux-only primitives on other systems.
var ErrUnsupportedOS = errors.New("needs Linux")

// ReflinkCapable reports whether Forkbox can rely on reflinks on this filesystem type.
func ReflinkCapable(fsType string) bool { return fsType == FSXFS || fsType == FSBtrfs }

// ProbeReflink makes a real reflink copy inside dir and checks the copy's
// contents, then removes both files.
func ProbeReflink(dir string) error {
	src, err := os.CreateTemp(dir, ".forkbox-reflink-probe-*")
	if err != nil {
		return err
	}
	srcPath := src.Name()
	dstPath := srcPath + ".clone"
	defer os.Remove(srcPath)
	defer os.Remove(dstPath)

	// 64 KiB spans several filesystem blocks, so the clone is a real extent share.
	want := make([]byte, 64<<10)
	if _, err := rand.Read(want); err != nil {
		src.Close()
		return err
	}
	if _, err := src.Write(want); err != nil {
		src.Close()
		return err
	}
	if err := src.Sync(); err != nil {
		src.Close()
		return err
	}
	if err := src.Close(); err != nil {
		return err
	}
	if err := Reflink(srcPath, dstPath); err != nil {
		return err
	}
	got, err := os.ReadFile(dstPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("reflink copy in %s has different contents from its source", filepath.Clean(dir))
	}
	return nil
}
