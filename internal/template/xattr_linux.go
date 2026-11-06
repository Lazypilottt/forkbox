package template

import (
	"archive/tar"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// setXattrs copies the extended attributes an image records (PAX
// SCHILY.xattr.*), such as security.capability on ping. mkfs.ext4 -d then
// carries them into the image.
func setXattrs(f *os.File, hdr *tar.Header) error {
	for k, v := range hdr.PAXRecords {
		attr, ok := strings.CutPrefix(k, "SCHILY.xattr.")
		if !ok {
			continue
		}
		if err := unix.Fsetxattr(int(f.Fd()), attr, []byte(v), 0); err != nil {
			return err
		}
	}
	return nil
}
