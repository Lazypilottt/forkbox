//go:build integration

package template

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	forkbox "github.com/Lazypilottt/forkbox"
	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/storage"
	"github.com/Lazypilottt/forkbox/internal/testutil"
	"github.com/Lazypilottt/forkbox/internal/versions"
	"github.com/Lazypilottt/forkbox/internal/vm"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

func TestIntegrationBuildTiny(t *testing.T) {
	testutil.RequireKVM(t)
	if os.Geteuid() != 0 {
		t.Fatal("run `make integration` (needs root)")
	}
	v, err := versions.Embedded()
	must(t, err)
	data := datadir.FromEnv()
	m, err := vm.NewManager(vm.Options{DataDir: data, FirecrackerVersion: v.Firecracker.Version})
	must(t, err)
	b := &Builder{
		DataDir: data, Manager: m,
		Kernel: datadir.Kernel(data, v.GuestKernel.Version), KernelVer: v.GuestKernel.Version,
		FCVersion: v.Firecracker.Version, VersionsYAML: forkbox.VersionsYAML,
		AgentBinary: "../../bin/forkbox-agent", GOARCH: runtime.GOARCH, Log: testWriter{t},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tpl, rebuilt, err := b.Build(ctx, "testdata/tiny.yaml", true)
	must(t, err)
	t.Cleanup(func() { Remove(data, tpl.ID) })
	if !rebuilt || tpl.Dir != Dir(data, tpl.ID) {
		t.Fatalf("build: rebuilt=%v dir=%s", rebuilt, tpl.Dir)
	}
	for _, k := range []string{"rootfs", "mem", "vmstate"} {
		if tpl.Sizes[k].Apparent == 0 {
			t.Errorf("size %s missing: %+v", k, tpl.Sizes)
		}
	}
	for _, k := range []string{StepPull, StepExtract, StepMkfs, StepBoot, "warm_up", StepTotal} {
		if _, ok := tpl.Durations[k]; !ok {
			t.Errorf("duration %s missing", k)
		}
	}
	t.Logf("built in %.1fs: %v", tpl.Durations[StepTotal], tpl.Durations)

	start := time.Now()
	again, rebuilt, err := b.Build(ctx, "testdata/tiny.yaml", false)
	must(t, err)
	if rebuilt || again.ID != tpl.ID || time.Since(start) > 5*time.Second {
		t.Errorf("unchanged rebuild: rebuilt=%v id=%s took %s", rebuilt, again.ID, time.Since(start))
	}
	if fi, err := os.Stat(tpl.Rootfs()); err != nil || fi.Mode().Perm() != 0o444 {
		t.Errorf("rootfs not read-only: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(Root(data), ".build-"+tpl.ID)); !os.IsNotExist(err) {
		t.Errorf("staging dir left behind")
	}

	// The base snapshot restores and the agent answers, with the template's
	// files and environment.
	disk := filepath.Join(data, "test", "tiny-restore.ext4")
	os.MkdirAll(filepath.Dir(disk), 0o755)
	os.Remove(disk)
	must(t, storage.Reflink(tpl.Rootfs(), disk))
	t.Cleanup(func() { os.Remove(disk) })
	sb, err := m.Restore(ctx, tpl.SnapshotDir(), vm.MemBackend{Kind: vm.MemFile}, vm.Config{Rootfs: vm.Drive{Path: disk}})
	must(t, err)
	t.Cleanup(func() { sb.Stop() })
	must(t, sb.Resume(ctx))
	c := agentclient.New(sb.VsockPath(), agentwire.Port)
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	defer rcancel()
	if _, err := c.WaitReady(rctx); err != nil {
		t.Fatalf("agent after restore: %v", err)
	}
	r, err := c.Run(ctx, agentclient.ExecRequest{Args: []string{"sh", "-c", `cat /etc/motd; echo "$FORKBOX_TEST"; hostname; cat /proc/1/comm`}}, 0)
	must(t, err)
	if want := "built by forkbox\n1\ntiny\nforkbox-agent\n"; string(r.Stdout) != want {
		t.Errorf("restored guest says %q, want %q (stderr %q)", r.Stdout, want, r.Stderr)
	}
	must(t, sb.Stop())
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
