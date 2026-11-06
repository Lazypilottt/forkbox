//go:build integration

package main

import (
	"context"
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
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "--id=sb-") {
			left = append(left, "process "+filepath.Dir(p)+": "+strings.ReplaceAll(string(b), "\x00", " "))
		}
	}
	if len(left) > 0 {
		t.Errorf("left behind:\n  %s", strings.Join(left, "\n  "))
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

// Kill the daemon at several points of a create, restart it, and check the
// reaper leaves nothing behind.
func TestIntegrationDaemonKilledMidCreate(t *testing.T) {
	for _, delay := range []time.Duration{0, 5 * time.Millisecond, 15 * time.Millisecond, 30 * time.Millisecond, 50 * time.Millisecond, 80 * time.Millisecond} {
		d := startDaemon(t)
		go d.c.CreateSandbox(context.Background(), &pb.CreateSandboxRequest{Template: "python"})
		time.Sleep(delay)
		d.kill()

		d = startDaemon(t)
		ctx := testCtx(t)
		list, err := d.c.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		// A create that finished before the kill is adopted and must work;
		// clean it up through the API.
		for _, sb := range list.Sandboxes {
			if sb.State != "running" {
				t.Errorf("delay %s: %s left in state %s", delay, sb.Id, sb.State)
			}
			if _, err := d.c.DestroySandbox(ctx, &pb.DestroySandboxRequest{Id: sb.Id}); err != nil {
				t.Errorf("destroy adopted %s: %v", sb.Id, err)
			}
		}
		t.Logf("delay %s: %d sandboxes survived and were adopted", delay, len(list.Sandboxes))
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

	d = startDaemon(t)
	defer func() { d.stop(); assertNoSandboxLeftovers(t, d) }()
	got, err := d.c.GetSandbox(ctx, &pb.GetSandboxRequest{Id: sb.Id})
	if err != nil || got.State != "running" {
		t.Fatalf("sandbox after restart: %v %v", got, err)
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
