//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/testutil"
	pb "github.com/Lazypilottt/forkbox/proto/forkboxpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The daemon under test is the real binary, built by `make integration`.
const daemonBinary = "../../bin/forkboxd"

type daemon struct {
	t    *testing.T
	data string
	cmd  *exec.Cmd
	log  *os.File
	c    pb.ForkboxClient
	cc   *grpc.ClientConn
}

func startDaemon(t *testing.T) *daemon {
	t.Helper()
	testutil.RequireKVM(t)
	if os.Geteuid() != 0 {
		t.Fatal("run `make integration` (needs root)")
	}
	if _, err := os.Stat(daemonBinary); err != nil {
		t.Fatalf("%v: run `make integration`, which builds it", err)
	}
	d := &daemon{t: t, data: datadir.FromEnv()}
	logPath := filepath.Join(d.data, "test", "forkboxd-it.log")
	os.MkdirAll(filepath.Dir(logPath), 0o755)
	var err error
	if d.log, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		t.Fatal(err)
	}
	d.cmd = exec.Command(daemonBinary, "--data-dir", d.data, "--ttl-sweep", "1s")
	d.cmd.Stdout, d.cmd.Stderr = d.log, d.log
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(d.data, RunDir, GRPCSocket)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not come up; log:\n%s", readLog(logPath))
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.cc, err = grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	d.c = pb.NewForkboxClient(d.cc)
	return d
}

func (d *daemon) kill() {
	d.cc.Close()
	d.cmd.Process.Signal(syscall.SIGKILL)
	d.cmd.Wait()
	d.log.Close()
}

func (d *daemon) stop() {
	d.cc.Close()
	d.cmd.Process.Signal(syscall.SIGTERM)
	d.cmd.Wait()
	d.log.Close()
}

func readLog(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// assertNoSandboxLeftovers finds any sandbox process, jail, cgroup,
// directory or record left on the host.
func assertNoSandboxLeftovers(t *testing.T, d *daemon) {
	t.Helper()
	var left []string
	for _, g := range []string{
		filepath.Join(d.data, "sandboxes", "sb-*"),
		filepath.Join(d.data, "jail", "firecracker", "sb-*"),
		filepath.Join(d.data, "vms", "sb-*"),
		"/sys/fs/cgroup/forkbox/sb-*",
	} {
		m, _ := filepath.Glob(g)
		left = append(left, m...)
	}
	for _, p := range sandboxProcs() {
		left = append(left, "process "+p)
	}
	if len(left) > 0 {
		t.Errorf("left behind:\n  %s", strings.Join(left, "\n  "))
	}
}

// sandboxProcs lists jailer and Firecracker processes of sandboxes, however
// the jailer spells the id on their command lines.
func sandboxProcs() []string {
	var out []string
	procs, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, p := range procs {
		comm, _ := os.ReadFile(p)
		if c := strings.TrimSpace(string(comm)); c != "firecracker" && c != "jailer" {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "cmdline"))
		if strings.Contains(string(cmd), "sb-") {
			out = append(out, filepath.Dir(p)+": "+strings.ReplaceAll(string(cmd), "\x00", " "))
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestIntegrationDaemonSocketsAreOwnerOnly(t *testing.T) {
	d := startDaemon(t)
	defer d.stop()
	for p, want := range map[string]os.FileMode{
		filepath.Join(d.data, RunDir):             0o700 | os.ModeDir,
		filepath.Join(d.data, RunDir, GRPCSocket): 0o600 | os.ModeSocket,
		filepath.Join(d.data, RunDir, HTTPSocket): 0o600 | os.ModeSocket,
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if fi.Mode() != want || st.Uid != 0 {
			t.Errorf("%s: mode %v uid %d, want %v owned by root", p, fi.Mode(), st.Uid, want)
		}
	}
	// A second daemon on the same data directory refuses to start.
	out, err := exec.Command(daemonBinary, "--data-dir", d.data).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "another forkboxd") {
		t.Errorf("second daemon: err=%v out=%s", err, out)
	}
}

// Kill the daemon at each phase of a create (record written, VM process
// running, agent being reached, done), restart it, and check the reaper
// leaves nothing behind.
func TestIntegrationDaemonKilledMidCreate(t *testing.T) {
	data := datadir.FromEnv()
	exists := func(glob string) func() bool {
		return func() bool { m, _ := filepath.Glob(glob); return len(m) > 0 }
	}
	phases := []struct {
		name  string
		ready func() bool
		extra time.Duration
	}{
		{"before anything", func() bool { return true }, 0},
		{"record and disk", exists(filepath.Join(data, "sandboxes", "sb-*", "rootfs.ext4")), 0},
		{"jail", exists(filepath.Join(data, "jail", "firecracker", "sb-*")), 0},
		{"vm running", func() bool { return len(sandboxProcs()) > 0 }, 0},
		{"agent wait", func() bool { return len(sandboxProcs()) > 0 }, 15 * time.Millisecond},
		{"finished", nil, 0},
	}
	for _, ph := range phases {
		d := startDaemon(t)
		done := make(chan error, 1)
		go func() {
			_, err := d.c.CreateSandbox(context.Background(), &pb.CreateSandboxRequest{Template: "python"})
			done <- err
		}()
		if ph.ready == nil {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		} else {
			waitFor(t, ph.name, ph.ready)
			time.Sleep(ph.extra)
		}
		procsBefore := len(sandboxProcs())
		d.kill()

		d = startDaemon(t)
		ctx := testCtx(t)
		list, err := d.c.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		// A create that finished before the kill is adopted and must work.
		for _, sb := range list.Sandboxes {
			if sb.State != "running" {
				t.Errorf("%s: %s left in state %s", ph.name, sb.Id, sb.State)
			}
			if out := execOut(t, d, sb.Id, "echo adopted"); out != "adopted\n" {
				t.Errorf("%s: adopted %s answered %q", ph.name, sb.Id, out)
			}
			if _, err := d.c.DestroySandbox(ctx, &pb.DestroySandboxRequest{Id: sb.Id}); err != nil {
				t.Errorf("destroy adopted %s: %v", sb.Id, err)
			}
		}
		t.Logf("killed at %-15s: %d VM processes at kill, %d sandboxes adopted after restart", ph.name, procsBefore, len(list.Sandboxes))
		if ph.name == "finished" && len(list.Sandboxes) != 1 {
			t.Errorf("a finished sandbox was not adopted; daemon log:\n%s", readLog(d.log.Name()))
		}
		d.stop()
		assertNoSandboxLeftovers(t, d)
	}
}

// Kill the daemon during an exec. The sandbox keeps running and is adopted;
// the orphaned command dies with its stream; nothing leaks.
func TestIntegrationDaemonKilledMidExec(t *testing.T) {
	d := startDaemon(t)
	ctx := testCtx(t)
	sb, err := d.c.CreateSandbox(ctx, &pb.CreateSandboxRequest{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := d.c.Exec(ctx, &pb.ExecRequest{SandboxId: sb.Id, Args: []string{"sh", "-c", "echo started; exec sleep 300"}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		ev, err := st.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if len(ev.GetStdout()) > 0 {
			break
		}
	}
	d.kill()
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", sb.Pid))
	cg, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", sb.Pid))
	t.Logf("after the kill, pid %d: cmdline %q cgroup %q", sb.Pid, cmdline, cg)

	d = startDaemon(t)
	defer func() { d.stop(); assertNoSandboxLeftovers(t, d) }()
	got, err := d.c.GetSandbox(ctx, &pb.GetSandboxRequest{Id: sb.Id})
	if err != nil || got.State != "running" {
		t.Fatalf("sandbox after restart: %v %v; daemon log:\n%s", got, err, readLog(d.log.Name()))
	}
	// The guest killed the orphaned command when its stream went away.
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := execOut(t, d, sb.Id, "pgrep -x sleep || true")
		if strings.TrimSpace(out) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphaned command still running: pids %q", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if out := execOut(t, d, sb.Id, "echo alive"); out != "alive\n" {
		t.Errorf("adopted sandbox: %q", out)
	}
	if _, err := d.c.DestroySandbox(ctx, &pb.DestroySandboxRequest{Id: sb.Id}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationDaemonTTL(t *testing.T) {
	d := startDaemon(t)
	defer func() { d.stop(); assertNoSandboxLeftovers(t, d) }()
	ctx := testCtx(t)
	sb, err := d.c.CreateSandbox(ctx, &pb.CreateSandboxRequest{Template: "python", TtlS: 2})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := d.c.GetSandbox(ctx, &pb.GetSandboxRequest{Id: sb.Id}); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sandbox outlived its TTL")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func execOut(t *testing.T, d *daemon, id, cmd string) string {
	t.Helper()
	st, err := d.c.Exec(context.Background(), &pb.ExecRequest{SandboxId: id, Args: []string{"sh", "-c", cmd}})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			return out.String()
		}
		if err != nil {
			t.Fatal(err)
		}
		out.Write(ev.GetStdout())
	}
}

// Kill the daemon during a fork of 50: while the parent is paused for the
// diff snapshot, and while children are being restored. After a restart the
// parent must be running (resumed if it was left paused) and the half-made
// children, their namespaces and any layer nobody uses must be gone.
func TestIntegrationDaemonKilledMidFork(t *testing.T) {
	data := datadir.FromEnv()
	exists := func(glob string, except string) func() bool {
		return func() bool {
			m, _ := filepath.Glob(glob)
			for _, p := range m {
				if filepath.Base(p) != except {
					return true
				}
			}
			return false
		}
	}
	for _, phase := range []string{"diff snapshot", "child restores"} {
		d := startDaemon(t)
		ctx := testCtx(t)
		parent, err := d.c.CreateSandbox(ctx, &pb.CreateSandboxRequest{Template: "python"})
		if err != nil {
			t.Fatal(err)
		}
		// Dirty memory, so the snapshot takes long enough to be interrupted.
		execOut(t, d, parent.Id, `setsid python -c "b = bytearray(b'\x01') * (200 << 20); open('/tmp/ready', 'w').write('1'); import time; time.sleep(3600)" >/dev/null 2>&1 </dev/null &`)
		waitFor(t, "allocation", func() bool { return execOut(t, d, parent.Id, "cat /tmp/ready 2>/dev/null || true") == "1" })

		go d.c.ForkSandbox(context.Background(), &pb.ForkSandboxRequest{Id: parent.Id, N: 50})
		if phase == "diff snapshot" {
			waitFor(t, phase, exists(filepath.Join(data, "sandboxes", parent.Id, "diff-*"), ""))
		} else {
			waitFor(t, phase, exists(filepath.Join(data, "jail", "firecracker", "sb-*"), parent.Id))
		}
		d.kill()

		d = startDaemon(t)
		got, err := d.c.GetSandbox(ctx, &pb.GetSandboxRequest{Id: parent.Id})
		if err != nil || got.State != "running" {
			t.Fatalf("%s: parent after restart: %v %v\n%s", phase, got, err, readLog(d.log.Name()))
		}
		// An answer means the parent's vCPUs run again.
		if out := execOut(t, d, parent.Id, "echo alive"); out != "alive\n" {
			t.Errorf("%s: parent answered %q", phase, out)
		}
		list, _ := d.c.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
		if len(list.Sandboxes) != 1 {
			t.Errorf("%s: %d sandboxes after restart, want only the parent", phase, len(list.Sandboxes))
		}
		for _, g := range []string{filepath.Join(data, "jail", "firecracker", "sb-*"), filepath.Join(data, "sandboxes", "sb-*"), "/run/netns/fbx-sb-*", "/sys/fs/cgroup/forkbox/sb-*"} {
			m, _ := filepath.Glob(g)
			for _, p := range m {
				if !strings.HasSuffix(p, parent.Id) {
					t.Errorf("%s: left behind: %s", phase, p)
				}
			}
		}
		if m, _ := filepath.Glob(filepath.Join(data, "sandboxes", parent.Id, "diff-*")); len(m) != 0 {
			t.Errorf("%s: half-written diff left: %v", phase, m)
		}
		t.Logf("killed during %s: parent running, %s", phase, strings.TrimSpace(lastLine(readLog(d.log.Name()), "recover:")))
		if _, err := d.c.DestroySandbox(ctx, &pb.DestroySandboxRequest{Id: parent.Id}); err != nil {
			t.Fatal(err)
		}
		if m, _ := filepath.Glob(filepath.Join(data, "layers", "layer-*")); len(m) != 0 {
			t.Errorf("%s: layers left after destroying everything: %v", phase, m)
		}
		d.stop()
		assertNoSandboxLeftovers(t, d)
	}
}

func lastLine(log, prefix string) string {
	var last string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, prefix) {
			last = l
		}
	}
	return last
}

// Stress: fork 10 at a time for FORKBOX_STRESS (e.g. 30m) and check that
// forkboxd's memory and open file descriptors stay flat.
func TestIntegrationDaemonStress(t *testing.T) {
	dur, err := time.ParseDuration(os.Getenv("FORKBOX_STRESS"))
	if err != nil || dur <= 0 {
		t.Skip("set FORKBOX_STRESS=30m to run")
	}
	d := startDaemon(t)
	defer func() { d.stop(); assertNoSandboxLeftovers(t, d) }()
	ctx, cancel := context.WithTimeout(context.Background(), dur+10*time.Minute)
	defer cancel()
	parent, err := d.c.CreateSandbox(ctx, &pb.CreateSandboxRequest{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.c.DestroySandbox(context.Background(), &pb.DestroySandboxRequest{Id: parent.Id})
	type sample struct {
		at          time.Duration
		rssKB, fds  int
		forks, kids int
	}
	pid := d.cmd.Process.Pid
	take := func(start time.Time, forks, kids int) sample {
		s := sample{at: time.Since(start).Round(time.Second), forks: forks, kids: kids}
		status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		for _, l := range strings.Split(string(status), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmRSS:" {
				fmt.Sscan(f[1], &s.rssKB)
			}
		}
		fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		s.fds = len(fds)
		return s
	}
	start := time.Now()
	var samples []sample
	next := time.Minute
	forks, kids := 0, 0
	for time.Since(start) < dur {
		res, err := d.c.ForkSandbox(ctx, &pb.ForkSandboxRequest{Id: parent.Id, N: 10})
		if err != nil {
			t.Fatalf("fork %d: %v", forks+1, err)
		}
		forks++
		kids += len(res.Children)
		// Touch the parent's memory a little, as real work would.
		execOut(t, d, parent.Id, fmt.Sprintf("echo %d > /tmp/round", forks))
		for _, c := range res.Children {
			if _, err := d.c.DestroySandbox(ctx, &pb.DestroySandboxRequest{Id: c.Id}); err != nil {
				t.Fatal(err)
			}
		}
		if time.Since(start) >= next {
			samples = append(samples, take(start, forks, kids))
			s := samples[len(samples)-1]
			t.Logf("%8s: %4d forks, %5d children, forkboxd RSS %6d KiB, %3d fds", s.at, s.forks, s.kids, s.rssKB, s.fds)
			next += time.Minute
		}
	}
	if len(samples) < 4 {
		t.Fatalf("only %d samples; run for at least 4 minutes", len(samples))
	}
	base, last := samples[1], samples[len(samples)-1] // after a minute of warm-up
	if last.fds > base.fds+5 {
		t.Errorf("file descriptors grew from %d to %d", base.fds, last.fds)
	}
	if last.rssKB > base.rssKB*13/10+8192 {
		t.Errorf("RSS grew from %d KiB to %d KiB", base.rssKB, last.rssKB)
	}
	t.Logf("%d forks into %d children in %s; RSS %d -> %d KiB, fds %d -> %d", forks, kids, dur, base.rssKB, last.rssKB, base.fds, last.fds)
}
