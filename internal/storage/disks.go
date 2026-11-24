package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// Disk backends (forkboxd --storage).
const (
	BackendReflink = "reflink" // XFS or btrfs: disks are reflinked files
	BackendDMThin  = "dmthin"  // any filesystem: disks are device-mapper thin snapshots
)

// Disks makes and removes sandbox disks. A disk is a path in the data
// directory that Firecracker opens: a file (reflink) or a block device node
// (dmthin). Directories that may hold disks are removed with RemoveAll, so
// the backend can free what backs them.
type Disks interface {
	Name() string
	// Clone makes dst a copy-on-write copy of src, which is a disk this
	// backend made or a plain image file (a template's rootfs). The source
	// must not change while Clone runs (its VM is paused or stopped).
	Clone(ctx context.Context, src, dst string) error
	// Remove removes one disk; a missing one is fine.
	Remove(ctx context.Context, path string) error
	// RemoveAll removes dir and every disk in it.
	RemoveAll(ctx context.Context, dir string) error
	// Reap frees what no disk refers to any more (after a crash).
	Reap(ctx context.Context) ([]string, error)
}

// Reflinks is the reflink backend.
type Reflinks struct{}

func (Reflinks) Name() string { return BackendReflink }

func (Reflinks) Clone(_ context.Context, src, dst string) error { return Reflink(src, dst) }

func (Reflinks) Remove(_ context.Context, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (Reflinks) RemoveAll(_ context.Context, dir string) error { return os.RemoveAll(dir) }

func (Reflinks) Reap(context.Context) ([]string, error) { return nil, nil }
