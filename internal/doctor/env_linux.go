//go:build linux

package doctor

import (
	"os"

	"golang.org/x/sys/unix"
)

func kernelRelease() (string, error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(u.Release[:]), nil
}

func accessRW(path string) error {
	if err := unix.Access(path, unix.R_OK|unix.W_OK); err != nil {
		return &os.PathError{Op: "access", Path: path, Err: err}
	}
	return nil
}
