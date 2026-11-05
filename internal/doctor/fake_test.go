package doctor

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/storage"
	"github.com/Lazypilottt/forkbox/internal/versions"
)

// fakeEnv is an in-memory host. The zero value is an empty Linux box; healthy()
// returns one where every check passes.
type fakeEnv struct {
	os, arch, user string
	kernel         string
	files          map[string]string      // ReadFile contents
	nodes          map[string]fs.FileMode // Stat results
	noAccess       map[string]bool
	fsTypes        map[string]string
	reflinkErr     error
	paths          map[string]string // LookPath results
	outputs        map[string]string // "path arg..." -> output
	outputErr      map[string]error
	sums           map[string]string
}

var _ Env = (*fakeEnv)(nil)

func (f *fakeEnv) OS() string       { return f.os }
func (f *fakeEnv) Arch() string     { return f.arch }
func (f *fakeEnv) Username() string { return f.user }

func (f *fakeEnv) Stat(p string) (fs.FileInfo, error) {
	mode, ok := f.nodes[p]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return fakeInfo{name: p, mode: mode}, nil
}

func (f *fakeEnv) ReadFile(p string) ([]byte, error) {
	s, ok := f.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return []byte(s), nil
}

func (f *fakeEnv) AccessRW(p string) error {
	if f.noAccess[p] {
		return &fs.PathError{Op: "access", Path: p, Err: fs.ErrPermission}
	}
	return nil
}

func (f *fakeEnv) KernelRelease() (string, error) {
	if f.kernel == "" {
		return "", errors.New("no kernel")
	}
	return f.kernel, nil
}

func (f *fakeEnv) FSType(p string) (string, error) {
	t, ok := f.fsTypes[p]
	if !ok {
		return "", &fs.PathError{Op: "statfs", Path: p, Err: fs.ErrNotExist}
	}
	return t, nil
}

func (f *fakeEnv) ProbeReflink(string) error { return f.reflinkErr }

func (f *fakeEnv) LookPath(file string) (string, error) {
	if p, ok := f.paths[file]; ok {
		return p, nil
	}
	return "", errors.New("executable file not found in $PATH")
}

func (f *fakeEnv) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	if err := f.outputErr[key]; err != nil {
		return nil, err
	}
	out, ok := f.outputs[key]
	if !ok {
		return nil, errors.New("exec: no such file")
	}
	return []byte(out), nil
}

func (f *fakeEnv) SHA256File(p string) (string, error) {
	s, ok := f.sums[p]
	if !ok {
		return "", &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return s, nil
}

type fakeInfo struct {
	name string
	mode fs.FileMode
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

const (
	testDataDir = "/var/lib/forkbox"
	charDev     = fs.ModeDevice | fs.ModeCharDevice | 0o660
)

func testVersions(t *testing.T) *versions.File {
	t.Helper()
	v, err := versions.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func testConfig(t *testing.T) Config {
	return Config{DataDir: testDataDir, Versions: testVersions(t)}
}

// healthy returns an Ubuntu 24.04 KVM host on which every check passes.
func healthy(t *testing.T) *fakeEnv {
	v := testVersions(t)
	bins := v.Firecracker.Artifacts["x86_64"].Binaries
	fcPath := testDataDir + "/bin/firecracker"
	jPath := testDataDir + "/bin/jailer"
	return &fakeEnv{
		os: "linux", arch: "amd64", user: "dev",
		kernel: "6.8.0-45-generic",
		files: map[string]string{
			"/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory hugetlb pids rdma misc\n",
			"/proc/cpuinfo":                     "vendor_id\t: GenuineIntel\nflags\t\t: fpu vme vmx sse\n",
			"/proc/misc":                        "126 userfaultfd\n232 kvm\n",
			uffdSysctl:                          "0\n",
		},
		nodes: map[string]fs.FileMode{
			kvmDevice:   charDev,
			uffdDevice:  charDev,
			testDataDir: fs.ModeDir | 0o755,
			fcPath:      0o755,
			jPath:       0o755,
		},
		fsTypes: map[string]string{cgroupRoot: storage.FSCgroup2, testDataDir: storage.FSXFS},
		paths:   map[string]string{"nft": "/usr/sbin/nft", "ip": "/usr/sbin/ip"},
		outputs: map[string]string{
			fcPath + " --version":     "Firecracker v1.17.0\n\nExiting successfully\n",
			jPath + " --version":      "Jailer v1.17.0\n",
			"/usr/sbin/nft --version": "nftables v1.0.9 (Old Doc Yak #3)\n",
			"/usr/sbin/ip -V":         "ip utility, iproute2-6.1.0, libbpf 1.3.0\n",
		},
		sums: map[string]string{fcPath: bins["firecracker"], jPath: bins["jailer"]},
	}
}
