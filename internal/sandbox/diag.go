package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lazypilottt/forkbox/internal/netns"
	"github.com/Lazypilottt/forkbox/internal/vm"
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

// deviceMetrics flushes Firecracker's metrics and keeps the non-zero
// counters of the devices a stalled guest could be waiting on.
func deviceMetrics(ctx context.Context, v *vm.VM) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := v.Metrics(ctx)
	if err != nil {
		return "metrics: " + err.Error()
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return "metrics: " + err.Error()
	}
	var out []string
	for _, dev := range []string{"vsock", "net", "vcpu", "block"} {
		var fields map[string]any
		if json.Unmarshal(all[dev], &fields) != nil {
			continue
		}
		var nz []string
		for k, val := range fields {
			if f, ok := val.(float64); ok && f != 0 {
				nz = append(nz, fmt.Sprintf("%s=%g", k, f))
			}
		}
		sort.Strings(nz)
		out = append(out, dev+"{"+strings.Join(nz, " ")+"}")
	}
	return strings.Join(out, " ")
}

// guestNetProbe pings the guest from its namespace and reports whether
// packets came back on the tap (the namespace's rules drop the replies, but
// the tap counts them): an answer means the guest kernel still takes
// interrupts and runs its network stack.
func guestNetProbe(ctx context.Context, id string) string {
	ns := netns.Name(id)
	rx := func() string {
		out, _ := exec.CommandContext(ctx, "ip", "netns", "exec", ns, "cat", "/sys/class/net/"+netns.TapName+"/statistics/rx_packets").Output()
		return strings.TrimSpace(string(out))
	}
	before := rx()
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	_ = exec.CommandContext(pctx, "ip", "netns", "exec", ns, "ping", "-c", "3", "-i", "0.3", "-W", "1", netns.GuestIP).Run()
	return fmt.Sprintf("tap rx packets %s -> %s across 3 pings", before, rx())
}
