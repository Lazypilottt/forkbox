// Package doctor checks that a host can run Forkbox, and says exactly how to
// fix each problem it finds.
package doctor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Lazypilottt/forkbox/internal/versions"
)

// Status is the outcome of one check.
type Status string

const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
)

// Result is one line of doctor output.
type Result struct {
	Name   string
	Status Status
	Detail string
	// Fix holds the exact command(s) that fix a failure, one per line. Lines
	// starting with "#" are explanations.
	Fix string
}

// Config is what the checks verify the host against.
type Config struct {
	DataDir  string
	Versions *versions.File
}

// Check is one named host check.
type Check struct {
	Name string
	Run  func(ctx context.Context, env Env, cfg Config) Result
}

// Checks returns every check, in the order they are printed.
func Checks() []Check {
	return []Check{
		{"platform", checkPlatform},
		{"kvm", checkKVM},
		{"kernel", checkKernel},
		{"cgroup-v2", checkCgroupV2},
		{"userfaultfd", checkUserfaultfd},
		{"data-dir-fs", checkDataDirFS},
		{"data-dir-reflink", checkDataDirReflink},
		{"firecracker", binaryCheck("firecracker", "Firecracker")},
		{"jailer", binaryCheck("jailer", "Jailer")},
		{"nftables", toolCheck("nft", []string{"--version"}, "nftables", "nftables")},
		{"iproute2", toolCheck("ip", []string{"-V"}, "iproute2", "iproute2")},
	}
}

// checkTimeout bounds a single check, so a hung command cannot stall doctor.
const checkTimeout = 15 * time.Second

// Run runs every check and returns the results in order.
func Run(ctx context.Context, env Env, cfg Config) []Result {
	checks := Checks()
	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		r := c.Run(cctx, env, cfg)
		cancel()
		r.Name = c.Name
		if r.Status != Pass {
			r.Status = Fail
		}
		results = append(results, r)
	}
	return results
}

// Failed counts failing results.
func Failed(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Status != Pass {
			n++
		}
	}
	return n
}

// Print writes one PASS or FAIL line per result, each failure followed by its
// fix, then a summary line.
func Print(w io.Writer, results []Result) {
	width := 0
	for _, r := range results {
		width = max(width, len(r.Name))
	}
	for _, r := range results {
		fmt.Fprintf(w, "%s  %-*s  %s\n", r.Status, width, r.Name, r.Detail)
		if r.Status == Pass || r.Fix == "" {
			continue
		}
		indent := strings.Repeat(" ", len(r.Status)+2+width+2)
		for i, line := range strings.Split(strings.TrimRight(r.Fix, "\n"), "\n") {
			label := "     "
			if i == 0 {
				label = "fix: "
			}
			fmt.Fprintf(w, "%s%s%s\n", indent, label, line)
		}
	}
	failed := Failed(results)
	fmt.Fprintf(w, "\n%d passed, %d failed\n", len(results)-failed, failed)
}
