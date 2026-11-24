package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// ExecCgroup holds every process started through Exec when the host set a
// process limit (pids.max): a fork bomb in a sandbox stops there.
const ExecCgroup = "/sys/fs/cgroup/forkbox-exec"

// setPidsMax puts later Execs in ExecCgroup with pids.max = n.
func (s *Server) setPidsMax(n int) error {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); errors.Is(err, os.ErrNotExist) {
		// Images run by the agent as init have no cgroup2 mount yet.
		if err := unix.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("mounting cgroup2: %w", err)
		}
	}
	_ = os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+pids"), 0o644)
	if err := os.MkdirAll(ExecCgroup, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ExecCgroup, "pids.max"), []byte(strconv.Itoa(n)), 0o644); err != nil {
		return fmt.Errorf("pids.max: %w", err)
	}
	fd, err := unix.Open(ExecCgroup, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.execCgroupFD > 0 {
		unix.Close(s.execCgroupFD)
	}
	s.execCgroupFD = fd
	s.mu.Unlock()
	return nil
}
