package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/vm"
)

// Limits are one sandbox's resource limits. The disk quota is the
// template's disk size (a sandbox's disk is a fixed-size reflink) and the
// lifetime is the TTL.
type Limits struct {
	CPUs      float64 `json:"cpus,omitempty"`       // host CPU time, in cores (cpu.max)
	MemoryMiB int     `json:"memory_mib,omitempty"` // host memory for the VM (memory.max)
	PidsMax   int     `json:"pids_max,omitempty"`   // processes started through exec, in the guest
}

func (l Limits) validate(guestMemMiB int) error {
	if l.CPUs < 0 || l.CPUs > 32 || (l.CPUs > 0 && l.CPUs < 0.05) {
		return fmt.Errorf("%w: cpus %g: 0.05 to 32 (0: the template's vCPUs)", vm.ErrConfig, l.CPUs)
	}
	if l.MemoryMiB != 0 && l.MemoryMiB < guestMemMiB+vm.MemoryOverheadMiB {
		return fmt.Errorf("%w: memory %d MiB: at least the guest's %d MiB plus %d MiB for the VMM", vm.ErrConfig,
			l.MemoryMiB, guestMemMiB, vm.MemoryOverheadMiB)
	}
	if l.PidsMax < 0 || (l.PidsMax > 0 && l.PidsMax < 8) || l.PidsMax > 1<<22 {
		return fmt.Errorf("%w: pids_max %d: 8 to %d (0: no limit)", vm.ErrConfig, l.PidsMax, 1<<22)
	}
	return nil
}

func (l Limits) encode() string {
	if l == (Limits{}) {
		return ""
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// ParseLimits decodes a record's Limits (empty: none).
func ParseLimits(s string) Limits {
	var l Limits
	if s != "" {
		_ = json.Unmarshal([]byte(s), &l)
	}
	return l
}

// apply sets the host-side limits on a VM config.
func (l Limits) apply(cfg *vm.Config) {
	if l.CPUs > 0 {
		cfg.CPUMax = fmt.Sprintf("%d 100000", int(l.CPUs*100000))
	}
	if l.MemoryMiB > 0 {
		cfg.MemoryMax = int64(l.MemoryMiB) << 20
	}
}

// ErrDensity means the host has no room for another sandbox.
var ErrDensity = errors.New("the host is at its sandbox density limit")

// checkDensity refuses n more sandboxes when the host would pass
// MaxSandboxes, or MemAvailable minus n times PerSandboxMiB would fall
// under MinFreeMiB: better to fail fast than to let the host swap.
func (m *Manager) checkDensity(n int) error {
	if m.MaxSandboxes > 0 {
		all, err := m.Store.ListSandboxes(context.Background())
		if err != nil {
			return err
		}
		live := 0
		for _, sb := range all {
			if sb.State != store.StateFailed {
				live++
			}
		}
		if live+n > m.MaxSandboxes {
			return fmt.Errorf("%w: %d sandboxes exist and %d more were asked for; the limit is %d", ErrDensity, live, n, m.MaxSandboxes)
		}
	}
	if m.MinFreeMiB > 0 {
		avail, ok := memAvailableMiB()
		per := m.PerSandboxMiB
		if per <= 0 {
			per = 64
		}
		if ok && avail-n*per < m.MinFreeMiB {
			return fmt.Errorf("%w: %d MiB available, %d more sandboxes need about %d MiB, and %d MiB must stay free",
				ErrDensity, avail, n, n*per, m.MinFreeMiB)
		}
	}
	return nil
}

func memAvailableMiB() (int, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			kb, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")))
			return kb / 1024, err == nil
		}
	}
	return 0, false
}
