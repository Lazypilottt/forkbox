package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm"
)

// Memory backends for forks and checkpoints.
const (
	// BackendMerge (v0.2): a fork's diff is merged onto the base into a
	// full layer, which the kernel maps for every child.
	BackendMerge = "merge"
	// BackendUffd (v1.0): a fork keeps its diff as a layer, and a page
	// server serves each child's memory from the layer chain.
	BackendUffd = "uffd"
)

// memSource is where a restored VM's memory comes from: a full memory file
// (mapped by the kernel), or a layer chain served by a page server.
type memSource struct {
	File     string
	Chain    []string // newest first; the last is a full file
	Prefetch bool
}

// PagerStatsFile is the page server's stats in a sandbox's directory.
const PagerStatsFile = "pagesrv.json"

// layerSource returns the snapshot directory and memory source to restore
// from layer id (or a template id).
func (m *Manager) layerSource(ctx context.Context, id string, tpl *template.Template) (string, memSource, error) {
	if id == "" || id == tpl.ID {
		return tpl.SnapshotDir(), fileSource(tpl, filepath.Join(tpl.SnapshotDir(), vm.SnapshotMemFile), m.Prefetch), nil
	}
	l, err := m.Store.GetLayer(ctx, id)
	if err != nil {
		return "", memSource{}, err
	}
	if l.Kind != store.LayerDiff {
		return l.Dir, fileSource(tpl, l.Mem, m.Prefetch), nil
	}
	lin, err := m.Store.Lineage(ctx, id)
	if err != nil {
		return "", memSource{}, err
	}
	var chain []string
	full := false
	for _, x := range lin {
		chain = append(chain, x.Mem)
		if x.Kind != store.LayerDiff {
			full = true
			break
		}
	}
	if !full {
		chain = append(chain, filepath.Join(tpl.SnapshotDir(), vm.SnapshotMemFile))
	}
	return l.Dir, memSource{Chain: chain, Prefetch: m.Prefetch}, nil
}

// fileSource restores from one full memory file: mapped by Firecracker, or
// served by a page server for hugepage templates (Firecracker restores
// hugetlbfs memory only through userfaultfd).
func fileSource(tpl *template.Template, mem string, prefetch bool) memSource {
	if hugePages(tpl) {
		return memSource{Chain: []string{mem}, Prefetch: prefetch}
	}
	return memSource{File: mem}
}

func hugePages(tpl *template.Template) bool { return tpl.Spec.HugePages != "" }

// backend returns the vm memory backend for src, starting a page server
// for a chain. The returned function reports the page server's pid once
// the VM has connected (0 for a file).
func (m *Manager) backend(ctx context.Context, rec *store.Sandbox, src memSource) (vm.MemBackend, func() int) {
	if src.Chain == nil {
		return vm.MemBackend{Kind: vm.MemFile, Path: src.File}, func() int { return 0 }
	}
	var cmd *exec.Cmd
	start := func(socket string) error {
		bin := m.PagerBinary
		if bin == "" {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			bin = exe
		}
		args := []string{"pagesrv", "--socket", socket, "--layers", strings.Join(src.Chain, ","),
			"--uid", strconv.Itoa(vm.DefaultUID), "--gid", strconv.Itoa(vm.DefaultGID),
			"--stats", filepath.Join(rec.Dir, PagerStatsFile), "--cgroup", m.VMs.CgroupPath(rec.ID), "--id", rec.ID}
		if src.Prefetch {
			args = append(args, "--prefetch")
		}
		cmd = exec.Command(bin, args...)
		log, err := os.OpenFile(filepath.Join(rec.Dir, "pagesrv.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err = cmd.Start()
		log.Close()
		if err != nil {
			return fmt.Errorf("starting the page server: %w", err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		// Ready once the socket exists and belongs to the jail user.
		deadline := time.Now().Add(10 * time.Second)
		for {
			if fi, err := os.Stat(socket); err == nil {
				if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == vm.DefaultUID {
					break
				}
			}
			select {
			case err := <-exited:
				return fmt.Errorf("page server exited before listening (%v; see %s)", err, filepath.Join(rec.Dir, "pagesrv.log"))
			default:
			}
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				return fmt.Errorf("page server did not come up (see %s)", filepath.Join(rec.Dir, "pagesrv.log"))
			}
			time.Sleep(2 * time.Millisecond)
		}
		m.watchPager(rec.ID, cmd.Process.Pid, exited)
		return nil
	}
	return vm.MemBackend{Kind: vm.MemUffd, Start: start}, func() int {
		if cmd == nil || cmd.Process == nil {
			return 0
		}
		return cmd.Process.Pid
	}
}

// watchPager fails the sandbox if its page server exits while the VM
// still runs: Firecracker would otherwise block on the next fault forever.
func (m *Manager) watchPager(id string, pid int, exited <-chan error) {
	go func() {
		err := <-exited
		ctx := context.Background()
		rec, gerr := m.Store.GetSandbox(ctx, id)
		if gerr != nil || rec.State == store.StateDestroying || rec.PagerPid != pid {
			return // destroyed, or a newer page server took over (rewind)
		}
		if !m.VMs.Alive(rec.Pid, id) {
			return
		}
		m.Log.Printf("page server of %s exited (%v) while its VM ran: stopping the VM", id, err)
		_ = m.stopVM(ctx, id)
		_ = m.Store.SetState(ctx, id, store.StateFailed, 0, fmt.Sprintf("page server exited: %v", err))
	}()
}

// pagerAlive reports whether pid is sandbox id's page server.
func pagerAlive(pid int, id string) bool {
	if pid <= 0 {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(string(b), "\x00")
	for i, a := range args {
		if a == "--id" && i+1 < len(args) && args[i+1] == id {
			return true
		}
	}
	return false
}

func killPager(pid int, id string) {
	if pagerAlive(pid, id) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// PagerStats reads a sandbox's page server stats (uffd backend).
func (m *Manager) PagerStats(id string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(Dir(m.DataDir, id), PagerStatsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

// watchAdoptedPager watches a page server this process did not start (it
// cannot wait for it), by polling.
func (m *Manager) watchAdoptedPager(id string, pid int) {
	exited := make(chan error, 1)
	go func() {
		for pagerAlive(pid, id) {
			time.Sleep(time.Second)
		}
		exited <- errors.New("exited")
	}()
	m.watchPager(id, pid, exited)
}
