package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost stands in for the jailer and Firecracker. Its launcher does what
// the jailer does that Forkbox relies on (cgroup with limits, pid file) and
// then serves a Firecracker-like API on the jail's API socket.
type fakeHost struct {
	t          *testing.T
	dataDir    string
	cgroupRoot string

	mu    sync.Mutex
	procs []*fakeFC
	chown []chownCall

	// Behaviour of the next launches.
	failStatus map[string]int // "METHOD /path" -> HTTP status with a fault
	hang       map[string]bool
	exitEarly  bool
}

type chownCall struct {
	path     string
	uid, gid int
}

type fakeReq struct {
	Method, Path string
	Body         map[string]any
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	// macOS limits Unix socket paths to 104 bytes and t.TempDir() is long.
	dir, err := os.MkdirTemp("/tmp", "fbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := &fakeHost{
		t:          t,
		dataDir:    filepath.Join(dir, "data"),
		cgroupRoot: filepath.Join(dir, "cgroup"),
		failStatus: map[string]int{},
		hang:       map[string]bool{},
	}
	must(t, os.MkdirAll(filepath.Join(h.dataDir, "kernels"), 0o755))
	must(t, os.MkdirAll(h.cgroupRoot, 0o755))
	must(t, os.WriteFile(filepath.Join(h.cgroupRoot, "cgroup.subtree_control"), []byte("cpuset cpu io memory pids\n"), 0o644))
	return h
}

func (h *fakeHost) manager(t *testing.T, mod func(*Options)) *Manager {
	t.Helper()
	o := Options{
		DataDir:            h.dataDir,
		FirecrackerVersion: "v1.17.0",
		CgroupRoot:         h.cgroupRoot,
		APITimeout:         time.Second,
		StartTimeout:       2 * time.Second,
		CleanupTimeout:     2 * time.Second,
		launch:             h.launch,
		chown: func(p string, uid, gid int) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.chown = append(h.chown, chownCall{p, uid, gid})
			return nil
		},
		rmdir: os.RemoveAll, // temp dirs hold the fake control files
	}
	if mod != nil {
		mod(&o)
	}
	m, err := NewManager(o)
	must(t, err)
	return m
}

// file creates a data-dir file (kernel, rootfs) for a test.
func (h *fakeHost) file(t *testing.T, rel, content string) string {
	t.Helper()
	p := filepath.Join(h.dataDir, rel)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func (h *fakeHost) config(t *testing.T) Config {
	return Config{
		VCPUs:  2,
		MemMiB: 512,
		Kernel: h.file(t, "kernels/vmlinux-test", "kernel"),
		Rootfs: Drive{Path: h.file(t, "images/rootfs.ext4", "rootfs"), ReadOnly: true},
	}
}

func (h *fakeHost) last() *fakeFC {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.procs) == 0 {
		h.t.Fatal("no fake firecracker was launched")
	}
	return h.procs[len(h.procs)-1]
}

func (h *fakeHost) chowns() []chownCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]chownCall(nil), h.chown...)
}

// fakeFC is one fake jailed Firecracker.
type fakeFC struct {
	argv    []string
	chroot  string
	console string
	srv     *http.Server
	done    chan struct{}
	once    sync.Once

	mu    sync.Mutex
	reqs  []fakeReq
	state string
}

func (f *fakeFC) Pid() int { return 4194000 } // never a live pid
func (f *fakeFC) Kill() error {
	f.once.Do(func() {
		if f.srv != nil {
			f.srv.Close()
		}
		close(f.done)
	})
	return nil
}
func (f *fakeFC) Done() <-chan struct{} { return f.done }

func (f *fakeFC) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

// configRequests are the requests other than readiness probes.
func (f *fakeFC) configRequests() []fakeReq {
	var out []fakeReq
	for _, r := range f.requests() {
		if !(r.Method == "GET" && r.Path == "/") {
			out = append(out, r)
		}
	}
	return out
}

func argValue(argv []string, flag string) string {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func argValues(argv []string, flag string) []string {
	var out []string
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag {
			out = append(out, argv[i+1])
		}
	}
	return out
}

func (h *fakeHost) launch(_ context.Context, argv []string, console *os.File) (process, error) {
	id := argValue(argv, "--id")
	base := argValue(argv, "--chroot-base-dir")
	chroot := filepath.Join(base, filepath.Base(argValue(argv, "--exec-file")), id, "root")
	f := &fakeFC{argv: argv, chroot: chroot, console: console.Name(), done: make(chan struct{}), state: "Not started"}
	h.mu.Lock()
	h.procs = append(h.procs, f)
	failStatus, hang, exitEarly := h.failStatus, h.hang, h.exitEarly
	h.mu.Unlock()

	fmt.Fprintf(console, "fake jailer: %s\n", id)
	// The jailer creates <root>/<parent>/<id> and writes the --cgroup values.
	cg := filepath.Join(h.cgroupRoot, argValue(argv, "--parent-cgroup"), id)
	if err := os.MkdirAll(cg, 0o755); err != nil {
		return nil, err
	}
	for _, kv := range argValues(argv, "--cgroup") {
		k, v, _ := strings.Cut(kv, "=")
		if err := os.WriteFile(filepath.Join(cg, k), []byte(v), 0o644); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(cg, "cgroup.procs"), nil, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(chroot, "firecracker.pid"), []byte("4194000\n"), 0o644); err != nil {
		return nil, err
	}
	if exitEarly {
		fmt.Fprintln(console, "fake firecracker: bad config, exiting")
		f.Kill()
		return f, nil
	}

	sock := filepath.Join(chroot, strings.TrimPrefix(argValue(argv, "--api-sock"), "/"))
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	f.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serve(w, r, failStatus, hang)
	})}
	go f.srv.Serve(ln)
	return f, nil
}

func (f *fakeFC) fault(w http.ResponseWriter, status int, format string, a ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"fault_message": fmt.Sprintf(format, a...)})
}

func (f *fakeFC) inJail(p any) string {
	s, _ := p.(string)
	return filepath.Join(f.chroot, strings.TrimPrefix(s, "/"))
}

func (f *fakeFC) serve(w http.ResponseWriter, r *http.Request, failStatus map[string]int, hang map[string]bool) {
	key := r.Method + " " + r.URL.Path
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		if err := json.Unmarshal(b, &body); err != nil {
			f.fault(w, 400, "bad json: %v", err)
			return
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, fakeReq{r.Method, r.URL.Path, body})
	f.mu.Unlock()

	if hang[key] {
		select {
		case <-r.Context().Done():
		case <-f.done:
		}
		return
	}
	if st := failStatus[key]; st != 0 {
		f.fault(w, st, "injected failure for %s", key)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case key == "GET /":
		json.NewEncoder(w).Encode(map[string]string{"id": argValue(f.argv, "--id"), "state": f.state, "vmm_version": "1.17.0", "app_name": "Firecracker"})
		return
	case key == "PUT /boot-source":
		if _, err := os.Stat(f.inJail(body["kernel_image_path"])); err != nil {
			f.fault(w, 400, "kernel: %v", err)
			return
		}
	case strings.HasPrefix(key, "PUT /drives/"):
		if _, err := os.Stat(f.inJail(body["path_on_host"])); err != nil {
			f.fault(w, 400, "drive: %v", err)
			return
		}
	case key == "PUT /actions":
		if body["action_type"] == "InstanceStart" {
			f.state = "Running"
			appendFile(f.console, "fake guest: init started\n")
		}
	case key == "PATCH /vm":
		if f.state == "Not started" {
			f.fault(w, 400, "not started")
			return
		}
		if body["state"] == "Paused" {
			f.state = "Paused"
		} else {
			f.state = "Running"
		}
	case key == "PUT /snapshot/create":
		if f.state != "Paused" {
			f.fault(w, 400, "the microVM must be paused")
			return
		}
		for k, content := range map[string]string{"snapshot_path": "state:", "mem_file_path": "mem:"} {
			if err := os.WriteFile(f.inJail(body[k]), []byte(content+fmt.Sprint(body["snapshot_type"])), 0o600); err != nil {
				f.fault(w, 400, "%s: %v", k, err)
				return
			}
		}
	case key == "PUT /snapshot/load":
		mb, _ := body["mem_backend"].(map[string]any)
		for _, p := range []any{body["snapshot_path"], mb["backend_path"]} {
			if _, err := os.Stat(f.inJail(p)); err != nil {
				f.fault(w, 400, "load: %v", err)
				return
			}
		}
		f.state = "Paused"
	}
	w.WriteHeader(http.StatusNoContent)
}

func appendFile(path, s string) {
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return
	}
	fh.WriteString(s)
	fh.Close()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// assertGone checks that nothing of the VM is left on the host.
func assertGone(t *testing.T, h *fakeHost, m *Manager, id string) {
	t.Helper()
	l := layout{h.dataDir, id}
	for _, p := range []string{l.jailDir(), l.runDir(), m.cg.path(id)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind (err=%v)", p, err)
		}
	}
	for _, f := range h.procs {
		if argValue(f.argv, "--id") != id {
			continue
		}
		select {
		case <-f.Done():
		default:
			t.Errorf("fake firecracker for %s still running", id)
		}
	}
}
