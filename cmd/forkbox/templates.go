package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	forkbox "github.com/Lazypilottt/forkbox"
	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/versions"
	"github.com/Lazypilottt/forkbox/internal/vm"
)

const templatesUsage = `Usage:
  forkbox templates build SPEC.yaml [--force] [--agent PATH]
      Build a template: root filesystem from the spec's OCI image, packages,
      files and the guest agent; boot it, run warm_up, take the base
      snapshot. Rebuilding an unchanged spec does nothing (--force rebuilds).
  forkbox templates list
  forkbox templates inspect NAME|ID [--json]
  forkbox templates rm NAME|ID

All take --data-dir DIR and --versions FILE.
`

func runTemplates(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, templatesUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	var c commonFlags
	fs := newFlagSet("templates "+sub, stderr, &c)
	force := fs.Bool("force", false, "rebuild even if the template is up to date")
	agentPath := fs.String("agent", "", "guest agent binary (default: forkbox-agent next to this binary, else <data>/bin/forkbox-agent)")
	asJSON := fs.Bool("json", false, "print template.json")
	// Flags may come after the positional argument.
	var pos []string
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "forkbox templates %s: %v\n", sub, err)
		return 1
	}
	need := func(n int) bool {
		if len(pos) != n {
			fmt.Fprint(stderr, templatesUsage)
			return false
		}
		return true
	}
	switch sub {
	case "build":
		if !need(1) {
			return 2
		}
		b, err := newBuilder(c, *agentPath, stdout)
		if err != nil {
			return fail(err)
		}
		t, rebuilt, err := b.Build(ctx, pos[0], *force)
		if err != nil {
			return fail(err)
		}
		if rebuilt {
			printTemplate(stdout, t)
		}
		return 0
	case "list":
		if !need(0) {
			return 2
		}
		all, err := template.List(c.dataDir)
		if err != nil {
			return fail(err)
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tID\tBASE\tDISK\tMEMORY\tBUILT\tBUILD TIME")
		for _, t := range all {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d MiB\t%s\t%.0fs\n", t.Name, t.ID, t.Spec.Base,
				mib(t.Sizes["rootfs"].Allocated), t.MemMiB, t.CreatedAt.Local().Format("2006-01-02 15:04"), t.Durations[template.StepTotal])
		}
		tw.Flush()
		return 0
	case "inspect":
		if !need(1) {
			return 2
		}
		t, err := template.Get(c.dataDir, pos[0])
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			b, _ := json.MarshalIndent(t, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		printTemplate(stdout, t)
		return 0
	case "rm":
		if !need(1) {
			return 2
		}
		t, err := template.Remove(c.dataDir, pos[0])
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "removed %s (%s)\n", t.Name, t.ID)
		return 0
	}
	fmt.Fprintf(stderr, "forkbox templates: unknown command %q\n\n%s", sub, templatesUsage)
	return 2
}

func newBuilder(c commonFlags, agentPath string, log io.Writer) (*template.Builder, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("templates are built on a Linux KVM host; this host runs %s", runtime.GOOS)
	}
	v, err := versions.Load(c.versions)
	if err != nil {
		return nil, err
	}
	raw := forkbox.VersionsYAML
	if c.versions != "" {
		if raw, err = os.ReadFile(c.versions); err != nil {
			return nil, err
		}
	}
	if agentPath == "" {
		agentPath = findAgent(c.dataDir)
	}
	m, err := vm.NewManager(vm.Options{DataDir: c.dataDir, FirecrackerVersion: v.Firecracker.Version})
	if err != nil {
		return nil, err
	}
	return &template.Builder{
		DataDir:      c.dataDir,
		Manager:      m,
		Kernel:       datadir.Kernel(c.dataDir, v.GuestKernel.Version),
		KernelVer:    v.GuestKernel.Version,
		FCVersion:    v.Firecracker.Version,
		VersionsYAML: raw,
		AgentBinary:  agentPath,
		GOARCH:       runtime.GOARCH,
		Log:          log,
	}, nil
}

// findAgent prefers forkbox-agent next to this binary (bin/ after `make
// agent build`), then the data directory.
func findAgent(dataDir string) string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "forkbox-agent")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return datadir.Bin(dataDir, "forkbox-agent")
}

func printTemplate(w io.Writer, t *template.Template) {
	fmt.Fprintf(w, "Template %s (%s)\n", t.Name, t.ID)
	fmt.Fprintf(w, "  base         %s\n               %s\n", t.Spec.Base, t.Image.Digest)
	fmt.Fprintf(w, "  machine      %d vCPUs, %d MiB, disk %d MiB\n", t.VCPUs, t.MemMiB, t.Spec.DiskSizeMiB)
	fmt.Fprintf(w, "  built        %s with Firecracker %s, kernel %s\n", t.CreatedAt.Local().Format(time.RFC3339), t.FirecrackerVersion, t.KernelVersion)
	fmt.Fprintf(w, "  dir          %s\n", t.Dir)
	fmt.Fprintln(w, "  sizes        (allocated / apparent)")
	for _, k := range []string{"rootfs", "mem", "vmstate"} {
		s := t.Sizes[k]
		fmt.Fprintf(w, "    %-10s %s / %s\n", k, mib(s.Allocated), mib(s.Apparent))
	}
	fmt.Fprintln(w, "  build times")
	steps := make([]string, 0, len(t.Durations))
	for k := range t.Durations {
		steps = append(steps, k)
	}
	order := map[string]int{template.StepPull: 0, template.StepExtract: 1, template.StepPackages: 2, template.StepFiles: 3,
		template.StepAgent: 4, template.StepMkfs: 5, template.StepBoot: 6, "vm-boot": 7, "warm_up": 8, "vm-snapshot-full": 9, template.StepTotal: 10}
	sort.Slice(steps, func(i, j int) bool { return order[steps[i]] < order[steps[j]] })
	for _, k := range steps {
		fmt.Fprintf(w, "    %-22s %8.2fs\n", k, t.Durations[k])
	}
	if len(t.Spec.WarmUp) > 0 {
		fmt.Fprintf(w, "  warm_up      %s\n", strings.Join(t.Spec.WarmUp, "; "))
	}
}

func mib(b int64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
