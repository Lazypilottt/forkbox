// Command forkbox is the Forkbox operator CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/doctor"
	"github.com/Lazypilottt/forkbox/internal/setup"
	"github.com/Lazypilottt/forkbox/internal/versions"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

const usage = `forkbox is the operator CLI for Forkbox, forkable Firecracker sandboxes.

Usage:
  forkbox doctor [--data-dir DIR] [--versions FILE]
      Check that this host can run Forkbox. Prints PASS or FAIL per check,
      with the command that fixes each failure. Exits 1 if any check fails.

  forkbox setup [--data-dir DIR] [--versions FILE] [--force]
      Download the pinned Firecracker release and guest kernel into the data
      directory and verify their checksums.

  forkbox templates build|list|inspect|rm ...
      Build and manage templates (forkbox templates for details).

  forkbox version
      Print the CLI version and the pinned Firecracker release.

The data directory defaults to $FORKBOX_DATA_DIR, else /var/lib/forkbox.
versions.yaml defaults to the copy built into this binary.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "doctor":
		return runDoctor(ctx, args[1:], stdout, stderr)
	case "setup":
		return runSetup(ctx, args[1:], stdout, stderr)
	case "templates":
		return runTemplates(ctx, args[1:], stdout, stderr)
	case "version", "--version":
		return runVersion(stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "forkbox: unknown command %q\n\n%s", args[0], usage)
	return 2
}

type commonFlags struct {
	dataDir  string
	versions string
}

func newFlagSet(name string, stderr io.Writer, c *commonFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("forkbox "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.dataDir, "data-dir", datadir.FromEnv(), "data directory")
	fs.StringVar(&c.versions, "versions", "", "versions.yaml to use instead of the built-in copy")
	return fs
}

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c commonFlags
	fs := newFlagSet("doctor", stderr, &c)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	v, err := versions.Load(c.versions)
	if err != nil {
		fmt.Fprintln(stderr, "forkbox doctor:", err)
		return 1
	}
	fmt.Fprintf(stdout, "forkbox doctor: Firecracker %s, guest kernel %s, data dir %s\n\n",
		v.Firecracker.Version, v.GuestKernel.Version, c.dataDir)
	results := doctor.Run(ctx, doctor.System{}, doctor.Config{DataDir: c.dataDir, Versions: v})
	doctor.Print(stdout, results)
	if doctor.Failed(results) > 0 {
		return 1
	}
	return 0
}

func runSetup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c commonFlags
	fs := newFlagSet("setup", stderr, &c)
	force := fs.Bool("force", false, "re-download and re-install even if the files already verify")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	v, err := versions.Load(c.versions)
	if err != nil {
		fmt.Fprintln(stderr, "forkbox setup:", err)
		return 1
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(stderr, "forkbox setup: installs Linux binaries for a KVM host; this host runs %s\n", runtime.GOOS)
		return 1
	}
	arch, err := versions.Arch(runtime.GOARCH)
	if err != nil {
		fmt.Fprintln(stderr, "forkbox setup:", err)
		return 1
	}
	inst, err := setup.Run(ctx, setup.Options{
		DataDir: c.dataDir, Versions: v, Arch: arch, Force: *force, Log: stdout,
	})
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintln(stderr, "forkbox setup:", err, "\n  fix: sudo forkbox setup")
		} else {
			fmt.Fprintln(stderr, "forkbox setup:", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "\nInstalled Firecracker %s (%d binaries) and guest kernel %s.\nNext: forkbox doctor\n",
		v.Firecracker.Version, len(inst.Binaries), v.GuestKernel.Version)
	return 0
}

func runVersion(stdout, stderr io.Writer) int {
	v, err := versions.Embedded()
	if err != nil {
		fmt.Fprintln(stderr, "forkbox version:", err)
		return 1
	}
	fmt.Fprintf(stdout, "forkbox %s (Firecracker %s, guest kernel %s)\n",
		version, v.Firecracker.Version, v.GuestKernel.Version)
	return 0
}
