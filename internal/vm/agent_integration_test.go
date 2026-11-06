//go:build integration

package vm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agent"
	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/internal/storage"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

// agentBinary is built by `make integration` (target bin/forkbox-agent):
// the tests run as root through sudo, whose PATH usually has no Go.
const agentBinary = "../../bin/forkbox-agent"

// agentImage is the pinned rootfs with the agent and its systemd unit,
// booted with the rootfs's own systemd as init.
func (e *kvmEnv) agentImage(t *testing.T) string {
	t.Helper()
	bin, err := os.ReadFile(agentBinary)
	if err != nil {
		t.Fatalf("%v: run `make integration`, which builds the static agent first", err)
	}
	unit := "/etc/systemd/system/forkbox-agent.service"
	return e.image(t, "agent", []imageFile{
		{path: agent.InstallPath, data: bin, mode: 0o755},
		{path: unit, data: []byte(agent.SystemdUnit), mode: 0o644},
		{path: "/etc/systemd/system/sysinit.target.wants/forkbox-agent.service", link: unit},
	})
}

// disk gives a VM its own writable copy of an image: a reflink, so instant.
func (e *kvmEnv) disk(t *testing.T, img, name string) string {
	t.Helper()
	dir := filepath.Join(e.dataDir, "test", "disks")
	must(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, name+".ext4")
	os.Remove(p)
	must(t, storage.Reflink(img, p))
	t.Cleanup(func() { os.Remove(p) })
	return p
}

func TestIntegrationAgent(t *testing.T) {
	e := newKVMEnv(t)
	img := e.agentImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg := Config{
		VCPUs:  2,
		MemMiB: 512,
		Kernel: e.kernel,
		Rootfs: Drive{Path: e.disk(t, img, "agent-parent"), ReadOnly: false},
		Vsock:  &Vsock{GuestCID: 3},
	}
	v, err := e.m.Boot(ctx, cfg)
	must(t, err)
	t.Cleanup(func() { v.Stop() })
	c := agentclient.New(v.VsockPath(), agentwire.Port)
	start := time.Now()
	ready := func(c *agentclient.Client) {
		t.Helper()
		rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
		defer rcancel()
		if _, err := c.WaitReady(rctx); err != nil {
			t.Fatalf("agent not ready: %v\nconsole:\n%s", err, tailFile(v.ConsolePath(), 4096))
		}
	}
	ready(c)
	t.Logf("agent answered %s after InstanceStart", time.Since(start).Round(time.Millisecond))

	t.Run("echo", func(t *testing.T) {
		r, err := c.Run(ctx, agentclient.ExecRequest{Args: []string{"echo", "hello from the guest"}}, 0)
		must(t, err)
		if string(r.Stdout) != "hello from the guest\n" || r.Code != 0 {
			t.Errorf("stdout %q exit %+v", r.Stdout, r.Exit)
		}
	})

	t.Run("streaming", func(t *testing.T) {
		begin := time.Now()
		var first time.Duration
		var out bytes.Buffer
		exit, err := c.Exec(ctx, agentclient.ExecRequest{Args: []string{"sh", "-c", "for i in $(seq 1 10); do echo tick $i; sleep 0.2; done"}},
			func(_ agentclient.Stream, d []byte) {
				if first == 0 {
					first = time.Since(begin)
				}
				out.Write(d)
			})
		must(t, err)
		total := time.Since(begin)
		if strings.Count(out.String(), "tick ") != 10 || exit.Code != 0 {
			t.Errorf("output %q exit %+v", out.String(), exit)
		}
		if first > time.Second || total < 1800*time.Millisecond {
			t.Errorf("first output after %s of %s: not streamed", first, total)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		r, err := c.Run(ctx, agentclient.ExecRequest{Args: []string{"sleep", "30"}, Timeout: 500 * time.Millisecond}, 0)
		must(t, err)
		if !r.TimedOut || r.Signal != "killed" || r.Duration > 3*time.Second {
			t.Errorf("exit = %+v", r.Exit)
		}
	})

	// 50 MB round trip, kept in the guest for the restore check below.
	data := make([]byte, 50<<20)
	_, err = rand.Read(data)
	must(t, err)
	sum := sha256.Sum256(data)
	const guestFile = "/root/forkbox-roundtrip.bin"
	t.Run("file 50MB", func(t *testing.T) {
		begin := time.Now()
		wi, err := c.WriteFile(ctx, guestFile, bytes.NewReader(data), agentclient.WriteOptions{Mode: 0o600})
		must(t, err)
		up := time.Since(begin)
		begin = time.Now()
		var back bytes.Buffer
		ri, err := c.ReadFile(ctx, guestFile, &back)
		must(t, err)
		down := time.Since(begin)
		if !bytes.Equal(wi.SHA256, sum[:]) || !bytes.Equal(ri.SHA256, sum[:]) || !bytes.Equal(back.Bytes(), data) {
			t.Fatalf("round trip differs")
		}
		t.Logf("50 MiB up in %s, down in %s", up.Round(time.Millisecond), down.Round(time.Millisecond))
	})

	before, err := c.Info(ctx)
	must(t, err)
	if before.BootId == "" || before.Hostname == "" {
		t.Errorf("info = %v", before)
	}

	// Pause, snapshot, resume: the same client carries on over new connections.
	must(t, v.Pause(ctx))
	snapDir := filepath.Join(e.dataDir, "test", "snapshots", v.ID())
	t.Cleanup(func() { os.RemoveAll(snapDir) })
	_, err = v.SnapshotFull(ctx, snapDir)
	must(t, err)
	// The disk as of the snapshot (Firecracker flushed it), for the restore.
	snapDisk := e.disk(t, cfg.Rootfs.Path, "agent-restored")
	must(t, v.Resume(ctx))
	t.Run("reconnect after resume", func(t *testing.T) {
		begin := time.Now()
		ready(c)
		r, err := c.Run(ctx, agentclient.ExecRequest{Args: []string{"echo", "still here"}}, 0)
		must(t, err)
		if string(r.Stdout) != "still here\n" {
			t.Errorf("stdout %q", r.Stdout)
		}
		t.Logf("agent back %s after resume", time.Since(begin).Round(time.Millisecond))
	})
	must(t, v.Stop())

	// Restore in a new jail: a client for the new socket works at once, and
	// the guest is the same machine (same boot, same files).
	r, err := e.m.Restore(ctx, snapDir, MemBackend{Kind: MemFile}, Config{Rootfs: Drive{Path: snapDisk}})
	must(t, err)
	t.Cleanup(func() { r.Stop() })
	must(t, r.Resume(ctx))
	rc := agentclient.New(r.VsockPath(), agentwire.Port)
	t.Run("reconnect after restore", func(t *testing.T) {
		begin := time.Now()
		ready(rc)
		t.Logf("agent answered %s after restore+resume", time.Since(begin).Round(time.Millisecond))
		after, err := rc.Info(ctx)
		must(t, err)
		if after.BootId != before.BootId || after.MachineId != before.MachineId {
			t.Errorf("restored guest is a different boot: before %v, after %v", before, after)
		}
		ri, err := rc.ReadFile(ctx, guestFile, io.Discard)
		must(t, err)
		if !bytes.Equal(ri.SHA256, sum[:]) {
			t.Errorf("file changed across restore")
		}
		res, err := rc.Run(ctx, agentclient.ExecRequest{Args: []string{"echo", "restored"}}, 0)
		must(t, err)
		if string(res.Stdout) != "restored\n" {
			t.Errorf("stdout %q", res.Stdout)
		}
	})
	logTimings(t, r)
	must(t, r.Stop())
	assertHostClean(t, e, r.ID())
}
