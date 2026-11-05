//go:build !linux

package doctor

import (
	"os"

	"github.com/Lazypilottt/forkbox/internal/storage"
)

func kernelRelease() (string, error) { return "", storage.ErrUnsupportedOS }

func accessRW(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}
