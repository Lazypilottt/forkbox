// Package datadir defines where Forkbox keeps files inside its data directory.
package datadir

import (
	"os"
	"path/filepath"
)

// Default is the data directory when neither --data-dir nor FORKBOX_DATA_DIR is set.
const Default = "/var/lib/forkbox"

// EnvVar overrides Default.
const EnvVar = "FORKBOX_DATA_DIR"

// FromEnv returns $FORKBOX_DATA_DIR, or Default.
func FromEnv() string {
	if d := os.Getenv(EnvVar); d != "" {
		return d
	}
	return Default
}

// Bin is the path of an installed host binary (firecracker, jailer, snapshot-editor).
func Bin(dataDir, name string) string { return filepath.Join(dataDir, "bin", name) }

// Kernel is the path of the guest kernel with the given version.
func Kernel(dataDir, version string) string {
	return filepath.Join(dataDir, "kernels", "vmlinux-"+version)
}

// Downloads holds verified release archives, so setup can re-install offline.
func Downloads(dataDir string) string { return filepath.Join(dataDir, "downloads") }
