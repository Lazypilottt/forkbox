//go:build !linux

package template

import (
	"archive/tar"
	"os"
)

// setXattrs is a no-op off Linux, where templates are not built.
func setXattrs(*os.File, *tar.Header) error { return nil }
