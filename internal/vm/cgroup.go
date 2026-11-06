package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cgroups manages the cgroup v2 groups of Forkbox VMs: <root>/<parent>/<id>.
// The jailer creates each VM's group and writes cpu.max and memory.max into
// it (--cgroup-version 2 --parent-cgroup <parent> --cgroup ...); Forkbox only
// prepares the parent and removes groups afterwards.
type cgroups struct {
	root   string // cgroup2 mount, normally /sys/fs/cgroup
	parent string // relative group holding every VM, e.g. "forkbox"
	// rmdir removes an empty group. Real cgroup directories are removed with
	// rmdir(2) even though they list control files; tests swap in RemoveAll.
	rmdir func(path string) error
}

// controllers the parent delegates to VM groups, so that cpu.max and
// memory.max exist there.
var controllers = []string{"cpu", "memory"}

func (c cgroups) parentPath() string { return filepath.Join(c.root, c.parent) }

func (c cgroups) path(id string) string { return filepath.Join(c.parentPath(), id) }

// ensureParent creates the parent group and enables the cpu and memory
// controllers for its children. The parent holds no processes, so the "no
// internal processes" rule does not get in the way.
func (c cgroups) ensureParent() error {
	avail, err := os.ReadFile(filepath.Join(c.root, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("cgroup v2 root %s: %w", c.root, err)
	}
	for _, ctl := range controllers {
		if !hasWord(string(avail), ctl) {
			return fmt.Errorf("cgroup v2: the %s controller is not enabled in %s/cgroup.subtree_control (run `forkbox doctor`)", ctl, c.root)
		}
	}
	p := c.parentPath()
	if err := os.MkdirAll(p, 0o755); err != nil {
		return fmt.Errorf("cgroup %s: %w", p, err)
	}
	stc := filepath.Join(p, "cgroup.subtree_control")
	cur, err := os.ReadFile(stc)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cgroup %s: %w", p, err)
	}
	var missing []string
	for _, ctl := range controllers {
		if !hasWord(string(cur), ctl) {
			missing = append(missing, "+"+ctl)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.WriteFile(stc, []byte(strings.Join(missing, " ")), 0o644); err != nil {
		return fmt.Errorf("cgroup %s: enabling %s: %w", p, strings.Join(missing, " "), err)
	}
	return nil
}

// remove kills every process left in the VM's group and removes the group.
// A missing group is not an error.
func (c cgroups) remove(id string, timeout time.Duration) error {
	p := c.path(id)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	c.killAll(p)
	deadline := time.Now().Add(timeout)
	for {
		if len(c.procs(p)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cgroup %s: processes %v still running after %s", p, c.procs(p), timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The kernel can report the group busy for a moment after its last
	// process exits, so retry until the deadline.
	for {
		err := c.rmdir(p)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			return fmt.Errorf("cgroup %s: %w", p, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killAll SIGKILLs everything in the group: through cgroup.kill (Linux 5.14+)
// when it exists, and pid by pid otherwise.
func (c cgroups) killAll(p string) {
	if err := os.WriteFile(filepath.Join(p, "cgroup.kill"), []byte("1"), 0o644); err == nil {
		return
	}
	for _, pid := range c.procs(p) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (c cgroups) procs(p string) []int {
	data, err := os.ReadFile(filepath.Join(p, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

func hasWord(s, w string) bool {
	for _, f := range strings.Fields(s) {
		if f == w {
			return true
		}
	}
	return false
}
