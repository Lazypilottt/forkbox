package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// threadStates describes pid's threads as "name:state:wchan" (state R is
// running or runnable, S sleeping; wchan is the kernel function a sleeping
// thread waits in, e.g. handle_userfault for a fault nobody served). For
// diagnosing a VM that stopped answering.
func threadStates(pid int) string {
	if pid <= 0 {
		return ""
	}
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*", pid))
	var out []string
	for _, t := range tasks {
		name := strings.TrimSpace(readSmall(filepath.Join(t, "comm")))
		stat := readSmall(filepath.Join(t, "stat"))
		state := "?"
		if i := strings.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) {
			state = stat[i+2 : i+3]
		}
		out = append(out, fmt.Sprintf("%s:%s:%s", name, state, strings.TrimSpace(readSmall(filepath.Join(t, "wchan")))))
	}
	return strings.Join(out, " ")
}

func readSmall(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}
