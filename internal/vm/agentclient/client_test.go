package agentclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agent"
	"github.com/Lazypilottt/forkbox/internal/agentwire"
)

// fakeVsock is Firecracker's host side of vsock: a Unix socket that takes
// "CONNECT <port>\n", answers "OK <n>\n" and hands the connection to the
// guest's listener, here a real agent server. Like Firecracker, it closes
// the connection when nobody listens on the port.
type fakeVsock struct {
	path    string
	srv     *agent.Server
	reject  atomic.Int32 // close this many connections, as before the agent is up
	accepts atomic.Int32
}

func newFakeVsock(t *testing.T) (*fakeVsock, *Client) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fbxa") // short: macOS caps socket paths at 104 bytes
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeVsock{path: filepath.Join(dir, "v.sock"), srv: agent.New("test-1")}
	ln, err := net.Listen("unix", f.path)
	must(t, err)
	t.Cleanup(func() { ln.Close(); f.srv.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepts.Add(1)
			go f.handle(c)
		}
	}()
	return f, New(f.path, agentwire.Port)
}

func (f *fakeVsock) handle(c net.Conn) {
	var line []byte
	b := make([]byte, 1)
	for {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	port, err := strconv.Atoi(strings.TrimPrefix(string(line), "CONNECT "))
	if err != nil || port != agentwire.Port || f.reject.Add(-1) >= 0 {
		c.Close()
		return
	}
	fmt.Fprintf(c, "OK 1073741824\n")
	f.srv.ServeConn(c)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPingRetriesUntilListening(t *testing.T) {
	f, c := newFakeVsock(t)
	f.reject.Store(3)
	p, err := c.WaitReady(testCtx(t))
	must(t, err)
	if p.AgentVersion != "test-1" || p.ProtocolVersion != agentwire.ProtocolVersion {
		t.Errorf("pong = %v", p)
	}
	if n := f.accepts.Load(); n != 4 {
		t.Errorf("%d connection attempts, want 4", n)
	}
}

func TestConnectStopsAtDeadline(t *testing.T) {
	f, c := newFakeVsock(t)
	f.reject.Store(1 << 30)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Ping(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "not listening") {
		t.Errorf("got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("gave up after %s", d)
	}
}

func TestDialerReportsNotListening(t *testing.T) {
	f, _ := newFakeVsock(t)
	_, err := FirecrackerDialer(f.path, 1)(context.Background())
	if !errors.Is(err, ErrNotListening) {
		t.Errorf("got %v, want ErrNotListening", err)
	}
}

func TestRunEcho(t *testing.T) {
	_, c := newFakeVsock(t)
	r, err := c.Run(testCtx(t), ExecRequest{Args: []string{"echo", "hello"}}, 0)
	must(t, err)
	if string(r.Stdout) != "hello\n" || r.Code != 0 || r.Signal != "" || r.TimedOut {
		t.Errorf("result = %+v (stdout %q)", r.Exit, r.Stdout)
	}
}

func TestRunEnvCwdStderrExitCode(t *testing.T) {
	_, c := newFakeVsock(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	r, err := c.Run(testCtx(t), ExecRequest{
		Args: []string{"sh", "-c", `echo "$FOO"; pwd; echo oops >&2; exit 3`},
		Env:  []string{"FOO=bar"},
		Cwd:  dir,
	}, 0)
	must(t, err)
	if want := "bar\n" + dir + "\n"; string(r.Stdout) != want {
		t.Errorf("stdout = %q, want %q", r.Stdout, want)
	}
	if string(r.Stderr) != "oops\n" || r.Code != 3 {
		t.Errorf("stderr %q, code %d", r.Stderr, r.Code)
	}
}

func TestExecStreamsOutputAsItComes(t *testing.T) {
	_, c := newFakeVsock(t)
	start := time.Now()
	var first time.Duration
	var got bytes.Buffer
	exit, err := c.Exec(testCtx(t), ExecRequest{Args: []string{"sh", "-c", "for i in 1 2 3 4 5; do echo line$i; sleep 0.2; done"}},
		func(s Stream, d []byte) {
			if first == 0 {
				first = time.Since(start)
			}
			got.Write(d)
		})
	must(t, err)
	total := time.Since(start)
	if got.String() != "line1\nline2\nline3\nline4\nline5\n" || exit.Code != 0 {
		t.Errorf("output %q, exit %+v", got.String(), exit)
	}
	if first > 500*time.Millisecond || total < 800*time.Millisecond {
		t.Errorf("first output after %s of %s: not streamed", first, total)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	_, c := newFakeVsock(t)
	start := time.Now()
	r, err := c.Run(testCtx(t), ExecRequest{
		Args:    []string{"sh", "-c", "sleep 30 & echo $!; wait"},
		Timeout: 300 * time.Millisecond,
	}, 0)
	must(t, err)
	if !r.TimedOut || r.Code != -1 || r.Signal != "killed" {
		t.Errorf("exit = %+v", r.Exit)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("timeout took %s", d)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(r.Stdout)))
	must(t, err)
	waitGone(t, child) // the background child died with its group
}

func TestSignal(t *testing.T) {
	_, c := newFakeVsock(t)
	ctx := testCtx(t)
	p, err := c.Start(ctx, ExecRequest{Args: []string{"sleep", "30"}})
	must(t, err)
	if p.ID == "" || p.Pid <= 0 {
		t.Fatalf("process = %+v", p)
	}
	must(t, c.Signal(ctx, p.ID, syscall.SIGTERM))
	exit, err := p.Wait(nil)
	must(t, err)
	if exit.Code != -1 || exit.Signal != "terminated" || exit.TimedOut {
		t.Errorf("exit = %+v", exit)
	}
	if err := c.Signal(ctx, p.ID, syscall.SIGTERM); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("signal to a finished exec: got %v", err)
	}
	if err := c.Signal(ctx, "x", 0); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("signal 0: got %v", err)
	}
}

func TestExecErrors(t *testing.T) {
	_, c := newFakeVsock(t)
	if _, err := c.Run(testCtx(t), ExecRequest{Args: []string{"/nonexistent/binary"}}, 0); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing binary: got %v", err)
	}
	if _, err := c.Run(testCtx(t), ExecRequest{}, 0); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("empty argv: got %v", err)
	}
}

func TestNoisyOutputIsCapped(t *testing.T) {
	_, c := newFakeVsock(t)
	r, err := c.Run(testCtx(t), ExecRequest{Args: []string{"sh", "-c", "head -c 20000000 /dev/zero; echo done >&2"}}, 1<<20)
	must(t, err)
	if len(r.Stdout) != 1<<20 || !r.StdoutCut || r.Code != 0 {
		t.Errorf("stdout %d bytes, cut=%v, exit %+v", len(r.Stdout), r.StdoutCut, r.Exit)
	}
	if string(r.Stderr) != "done\n" || r.StderrCut {
		t.Errorf("stderr %q cut=%v", r.Stderr, r.StderrCut)
	}
}

func TestConcurrentExecs(t *testing.T) {
	_, c := newFakeVsock(t)
	ctx := testCtx(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Run(ctx, ExecRequest{Args: []string{"sh", "-c", fmt.Sprintf("sleep 0.1; echo %d", i)}}, 0)
			if err != nil {
				errs <- err
			} else if string(r.Stdout) != fmt.Sprintf("%d\n", i) {
				errs <- fmt.Errorf("exec %d got %q", i, r.Stdout)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestClosingTheStreamKillsTheProcess(t *testing.T) {
	_, c := newFakeVsock(t)
	p, err := c.Start(testCtx(t), ExecRequest{Args: []string{"sleep", "30"}})
	must(t, err)
	p.Close()
	waitGone(t, p.Pid)
}

func TestFileRoundTrip(t *testing.T) {
	_, c := newFakeVsock(t)
	ctx := testCtx(t)
	data := make([]byte, 20<<20+123) // not a multiple of the chunk size
	_, err := rand.Read(data)
	must(t, err)
	want := sha256.Sum256(data)
	path := filepath.Join(t.TempDir(), "a", "b", "blob")

	wi, err := c.WriteFile(ctx, path, bytes.NewReader(data), WriteOptions{Mode: 0o600, MakeParents: true})
	must(t, err)
	if wi.Size != uint64(len(data)) || !bytes.Equal(wi.SHA256, want[:]) {
		t.Errorf("write info = %d bytes %x", wi.Size, wi.SHA256)
	}
	st, err := c.Stat(ctx, path)
	must(t, err)
	if !st.Exists || st.Size != uint64(len(data)) || st.Mode != 0o600 || st.IsDir {
		t.Errorf("stat = %v", st)
	}
	var back bytes.Buffer
	ri, err := c.ReadFile(ctx, path, &back)
	must(t, err)
	if !bytes.Equal(back.Bytes(), data) || !bytes.Equal(ri.SHA256, want[:]) || ri.Mode != 0o600 {
		t.Errorf("read back %d bytes, sha %x, mode %v", back.Len(), ri.SHA256, ri.Mode)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Errorf("upload left temporary files: %v", ents)
	}
}

func TestFileErrors(t *testing.T) {
	_, c := newFakeVsock(t)
	ctx := testCtx(t)
	dir := t.TempDir()
	if _, err := c.ReadFile(ctx, filepath.Join(dir, "missing"), io.Discard); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read missing: got %v", err)
	}
	if _, err := c.ReadFile(ctx, "relative/path", io.Discard); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("read relative: got %v", err)
	}
	if _, err := c.ReadFile(ctx, dir, io.Discard); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("read a directory: got %v", err)
	}
	big := bytes.NewReader(make([]byte, 4<<20))
	if _, err := c.WriteFile(ctx, filepath.Join(dir, "no", "such", "dir", "f"), big, WriteOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("write without parents: got %v", err)
	}
	st, err := c.Stat(ctx, filepath.Join(dir, "missing"))
	must(t, err)
	if st.Exists {
		t.Errorf("stat of a missing file: %v", st)
	}
	st, err = c.Stat(ctx, dir)
	must(t, err)
	if !st.Exists || !st.IsDir {
		t.Errorf("stat of a dir: %v", st)
	}
	// A failing source aborts the upload; nothing appears in the guest.
	dst := filepath.Join(dir, "partial")
	if _, err := c.WriteFile(ctx, dst, io.MultiReader(bytes.NewReader(make([]byte, 1<<20)), errReader{}), WriteOptions{}); err == nil {
		t.Errorf("failing source: no error")
	}
	time.Sleep(100 * time.Millisecond)
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("aborted upload left %v", ents)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("source broke") }

func TestWaitPort(t *testing.T) {
	_, c := newFakeVsock(t)
	ctx := testCtx(t)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := uint16(probe.Addr().(*net.TCPAddr).Port)
	probe.Close()

	if ok, err := c.WaitPort(ctx, port, 100*time.Millisecond); err != nil || ok {
		t.Errorf("closed port: ok=%v err=%v", ok, err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			t.Cleanup(func() { ln.Close() })
		}
	}()
	start := time.Now()
	ok, err := c.WaitPort(ctx, port, 5*time.Second)
	if err != nil || !ok {
		t.Errorf("opened port: ok=%v err=%v", ok, err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
		t.Errorf("wait took %s", d)
	}
}

func TestInfo(t *testing.T) {
	_, c := newFakeVsock(t)
	i, err := c.Info(testCtx(t))
	must(t, err)
	host, _ := os.Hostname()
	if i.Hostname != host || i.AgentVersion != "test-1" {
		t.Errorf("info = %v", i)
	}
	if d := time.Since(time.Unix(0, i.RealtimeUnixNano)); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("guest clock off by %s", d)
	}
}

func TestAgentDropsOversizedFrames(t *testing.T) {
	f, _ := newFakeVsock(t)
	conn, err := FirecrackerDialer(f.path, agentwire.Port)(context.Background())
	must(t, err)
	defer conn.Close()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 1<<30)
	conn.Write(hdr[:])
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("agent answered a 1 GiB frame: n=%d err=%v", n, err)
	}
}

// waitGone waits for a pid to disappear (killed and reaped).
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
