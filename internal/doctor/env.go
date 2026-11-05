package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"time"

	"github.com/Lazypilottt/forkbox/internal/storage"
)

// Env is everything the checks read from the host. Tests substitute a fake.
type Env interface {
	OS() string
	Arch() string // GOARCH
	Username() string
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// AccessRW reports whether the current user may read and write path.
	AccessRW(path string) error
	KernelRelease() (string, error)
	FSType(path string) (string, error)
	ProbeReflink(dir string) error
	LookPath(file string) (string, error)
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
	SHA256File(path string) (string, error)
}

// System is the real host.
type System struct{}

var _ Env = System{}

func (System) OS() string   { return runtime.GOOS }
func (System) Arch() string { return runtime.GOARCH }

func (System) Username() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "$USER"
}

func (System) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
func (System) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (System) FSType(path string) (string, error)    { return storage.FSType(path) }
func (System) ProbeReflink(dir string) error         { return storage.ProbeReflink(dir) }
func (System) LookPath(file string) (string, error)  { return exec.LookPath(file) }
func (System) KernelRelease() (string, error)        { return kernelRelease() }
func (System) AccessRW(path string) error            { return accessRW(path) }

func (System) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (System) SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
