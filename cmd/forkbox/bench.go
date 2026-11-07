package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agent"
	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/netns"
	"github.com/Lazypilottt/forkbox/internal/storage"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/versions"
	"github.com/Lazypilottt/forkbox/internal/vm"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

const benchUsage = `Usage (baselines for bench/fork_curve.py; root, on the KVM host):
  forkbox bench coldboot TEMPLATE [--runs N]
      Boot the template's root filesystem from the kernel (no snapshot) until
      the guest agent answers.
  forkbox bench copyrestore TEMPLATE [--runs N]
      Restore the template's full snapshot from a byte copy of its memory
      file and disk (no reflink, no shared pages) until the agent answers.

Each prints one JSON object with the per-run times in milliseconds.
`

func runBench(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprint(stderr, benchUsage)
		return 2
	}
	mode, tplName := args[0], args[1]
	var c commonFlags
	fs := newFlagSet("bench "+mode, stderr, &c)
	runs := fs.Int("runs", 10, "how many times")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "forkbox bench %s: %v\n", mode, err)
		return 1
	}
	if runtime.GOOS != "linux" {
		return fail(fmt.Errorf("needs a Linux KVM host"))
	}
	v, err := versions.Load(c.versions)
	if err != nil {
		return fail(err)
	}
	tpl, err := template.Get(c.dataDir, tplName)
	if err != nil {
		return fail(err)
	}
	m, err := vm.NewManager(vm.Options{DataDir: c.dataDir, FirecrackerVersion: v.Firecracker.Version})
	if err != nil {
		return fail(err)
	}
	nm, err := netns.New("", vm.DefaultUID, vm.DefaultGID)
	if err != nil {
		return fail(err)
	}
	work := filepath.Join(c.dataDir, "bench")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return fail(err)
	}
	var times []float64
	for i := 0; i < *runs; i++ {
		id := fmt.Sprintf("bench-%s-%d", mode, i)
		d, err := benchOnce(ctx, mode, m, nm, tpl, datadir.Kernel(c.dataDir, v.GuestKernel.Version), work, id)
		if err != nil {
			return fail(err)
		}
		times = append(times, float64(d.Microseconds())/1000)
	}
	json.NewEncoder(stdout).Encode(map[string]any{"mode": mode, "template": tpl.Name, "template_id": tpl.ID, "ms": times})
	return 0
}

func benchOnce(ctx context.Context, mode string, m *vm.Manager, nm *netns.Manager, tpl *template.Template, kernel, work, id string) (time.Duration, error) {
	_ = m.Reap(id)
	disk := filepath.Join(work, id+".ext4")
	os.Remove(disk)
	defer os.Remove(disk)
	if err := nm.Setup(ctx, id, -1); err != nil {
		return 0, err
	}
	defer nm.Teardown(context.WithoutCancel(ctx), id)

	start := time.Now()
	var v *vm.VM
	var err error
	switch mode {
	case "coldboot":
		if err = storage.Reflink(tpl.Rootfs(), disk); err != nil {
			return 0, err
		}
		v, err = m.Boot(ctx, vm.Config{
			ID: id, VCPUs: tpl.VCPUs, MemMiB: tpl.MemMiB, Kernel: kernel,
			KernelArgs: vm.DefaultKernelArgs + " init=" + agent.InstallPath,
			Rootfs:     vm.Drive{Path: disk}, Vsock: &vm.Vsock{GuestCID: 3},
			Net:   &vm.NetIface{IfaceID: netns.GuestIface, HostDev: netns.TapName, GuestMAC: netns.GuestMAC},
			NetNS: netns.Path(id),
		})
	case "copyrestore":
		// A full byte copy of memory and disk: what restoring costs without
		// reflinks and without sharing the template's pages.
		mem := filepath.Join(work, id+".mem")
		defer os.Remove(mem)
		if err = copyFile(filepath.Join(tpl.SnapshotDir(), vm.SnapshotMemFile), mem, 0o440); err != nil {
			return 0, err
		}
		if err = os.Chown(mem, 0, vm.DefaultGID); err != nil {
			return 0, err
		}
		if err = copyFile(tpl.Rootfs(), disk, 0o600); err != nil {
			return 0, err
		}
		if v, err = m.Restore(ctx, tpl.SnapshotDir(), vm.MemBackend{Kind: vm.MemFile, Path: mem},
			vm.Config{ID: id, Rootfs: vm.Drive{Path: disk}, NetNS: netns.Path(id)}); err == nil {
			err = v.Resume(ctx)
		}
	default:
		return 0, fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		return 0, err
	}
	defer v.Stop()
	actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := agentclient.New(v.VsockPath(), agentwire.Port).WaitReady(actx); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	// A plain read/write copy (io.Copy would use copy_file_range, which
	// reflinks on XFS and so would not be a real copy).
	buf := make([]byte, 4<<20)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}
	return out.Close()
}
