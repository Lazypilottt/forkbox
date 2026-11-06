package vm

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MemStats is a process's memory from /proc/<pid>/smaps_rollup, in bytes.
// For a restored VM, Pss is the honest per-VM cost: pages of the template's
// memory file that several VMs map are split between them.
type MemStats struct {
	Rss, Pss                   int64
	SharedClean, SharedDirty   int64
	PrivateClean, PrivateDirty int64
	Anonymous                  int64
}

// ReadMemStats reads /proc/<pid>/smaps_rollup (Linux 4.14+).
func ReadMemStats(pid int) (MemStats, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return MemStats{}, err
	}
	return parseSmapsRollup(string(b))
}

func parseSmapsRollup(s string) (MemStats, error) {
	var m MemStats
	fields := map[string]*int64{
		"Rss:": &m.Rss, "Pss:": &m.Pss, "Shared_Clean:": &m.SharedClean, "Shared_Dirty:": &m.SharedDirty,
		"Private_Clean:": &m.PrivateClean, "Private_Dirty:": &m.PrivateDirty, "Anonymous:": &m.Anonymous,
	}
	found := 0
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "kB" {
			continue
		}
		if p, ok := fields[f[0]]; ok {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return m, fmt.Errorf("smaps_rollup %s: %w", f[0], err)
			}
			*p = kb << 10
			found++
		}
	}
	if found == 0 {
		return m, fmt.Errorf("smaps_rollup: no fields found")
	}
	return m, nil
}
