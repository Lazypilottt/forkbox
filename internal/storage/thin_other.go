//go:build !linux

package storage

import "context"

// Thin is the dmthin backend (Linux only).
type Thin struct{ DataDir, PoolName string }

// DefaultThinSize is the default pool size.
const DefaultThinSize = 100 << 30

// NewThin returns the dmthin backend for dataDir.
func NewThin(dataDir string, _ int64) *Thin { return &Thin{DataDir: dataDir} }

func (*Thin) Name() string                                { return BackendDMThin }
func (*Thin) Ensure(context.Context) error                { return ErrUnsupportedOS }
func (*Thin) Clone(context.Context, string, string) error { return ErrUnsupportedOS }
func (*Thin) Remove(context.Context, string) error        { return ErrUnsupportedOS }
func (*Thin) RemoveAll(context.Context, string) error     { return ErrUnsupportedOS }
func (*Thin) Reap(context.Context) ([]string, error)      { return nil, ErrUnsupportedOS }
