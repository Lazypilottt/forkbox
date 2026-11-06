package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agent"
	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/internal/sandbox"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
	pb "github.com/Lazypilottt/forkbox/proto/forkboxpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeBackend keeps records in memory; every sandbox's agent is a real
// agent server behind a fake Firecracker vsock socket.
type fakeBackend struct {
	t    *testing.T
	dir  string
	mu   sync.Mutex
	recs map[string]*store.Sandbox
	n    int
}

func newFakeBackend(t *testing.T) *fakeBackend {
	dir, err := os.MkdirTemp("/tmp", "fbxapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return &fakeBackend{t: t, dir: dir, recs: map[string]*store.Sandbox{}}
}

func (b *fakeBackend) Create(_ context.Context, o sandbox.CreateOptions) (*sandbox.Created, error) {
	if o.Template != "python" {
		return nil, fmt.Errorf("%w: %q", template.ErrNotFound, o.Template)
	}
	b.mu.Lock()
	b.n++
	id := fmt.Sprintf("sb-%d", b.n)
	b.mu.Unlock()
	sock := filepath.Join(b.dir, id+".sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	srv := agent.New("test")
	b.t.Cleanup(func() { ln.Close(); srv.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				// Firecracker's handshake: "CONNECT <port>\n" -> "OK <n>\n".
				buf := make([]byte, 1)
				for {
					if _, err := c.Read(buf); err != nil || buf[0] == '\n' {
						break
					}
				}
				fmt.Fprint(c, "OK 1\n")
				srv.ServeConn(c)
			}()
		}
	}()
	rec := &store.Sandbox{ID: id, Template: "tpl-x", TemplateName: o.Template, State: store.StateRunning,
		Vsock: sock, Pid: 42, CreatedAt: time.Now(), TTL: o.TTL}
	b.mu.Lock()
	b.recs[id] = rec
	b.mu.Unlock()
	return &sandbox.Created{Sandbox: rec, Timings: sandbox.Timings{Total: 12 * time.Millisecond, Restore: 5 * time.Millisecond}}, nil
}

func (b *fakeBackend) Get(_ context.Context, id string) (*store.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.recs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", store.ErrNotFound, id)
	}
	return r, nil
}

func (b *fakeBackend) List(context.Context) ([]*store.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*store.Sandbox
	for i := 1; i <= b.n; i++ {
		if r, ok := b.recs[fmt.Sprintf("sb-%d", i)]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (b *fakeBackend) Destroy(ctx context.Context, id string) error {
	if _, err := b.Get(ctx, id); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.recs, id)
	b.mu.Unlock()
	return nil
}

func (b *fakeBackend) Expose(ctx context.Context, id string, guest, host int) (int, error) {
	if _, err := b.Get(ctx, id); err != nil {
		return 0, err
	}
	if host == 0 {
		host = 40000 + guest
	}
	return host, nil
}

func (b *fakeBackend) Fork(ctx context.Context, id string, n, _ int) (*sandbox.ForkResult, error) {
	if _, err := b.Get(ctx, id); err != nil {
		return nil, err
	}
	res := &sandbox.ForkResult{Layer: "layer-x", Timings: sandbox.ForkTimings{ParentPaused: 3 * time.Millisecond, DirtyBytes: 1 << 20}}
	for i := 0; i < n; i++ {
		c, err := b.Create(ctx, sandbox.CreateOptions{Template: "python"})
		if err != nil {
			return nil, err
		}
		c.Parent = id
		res.Children = append(res.Children, c)
		res.Timings.Restore = append(res.Timings.Restore, 20*time.Millisecond)
	}
	return res, nil
}

func (b *fakeBackend) Agent(id string) *agentclient.Client {
	b.mu.Lock()
	sock := b.recs[id].Vsock
	b.mu.Unlock()
	return agentclient.New(sock, agentwire.Port)
}

func dial(t *testing.T, s *Server, tok *Token) pb.ForkboxClient {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	g := NewGRPCServer(s, tok)
	go g.Serve(ln)
	t.Cleanup(g.Stop)
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return pb.NewForkboxClient(cc)
}

func code(err error) codes.Code { return status.Code(err) }

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSandboxCRUD(t *testing.T) {
	b := newFakeBackend(t)
	c := dial(t, &Server{B: b}, nil)
	ctx := testCtx(t)

	sb, err := c.CreateSandbox(ctx, &pb.CreateSandboxRequest{Template: "python", TtlS: 60})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Id != "sb-1" || sb.Template != "python" || sb.TtlS != 60 || sb.State != "running" || sb.Timings.GetTotalMs() != 12 {
		t.Errorf("created %v", sb)
	}
	if _, err := c.CreateSandbox(ctx, &pb.CreateSandboxRequest{Template: "ruby"}); code(err) != codes.NotFound {
		t.Errorf("unknown template: %v", err)
	}
	if _, err := c.CreateSandbox(ctx, &pb.CreateSandboxRequest{}); code(err) != codes.InvalidArgument {
		t.Errorf("no template: %v", err)
	}
	got, err := c.GetSandbox(ctx, &pb.GetSandboxRequest{Id: "sb-1"})
	if err != nil || got.Id != "sb-1" || got.Timings != nil {
		t.Errorf("get %v %v", got, err)
	}
	list, err := c.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
	if err != nil || len(list.Sandboxes) != 1 {
		t.Errorf("list %v %v", list, err)
	}
	if ep, err := c.ExposePort(ctx, &pb.ExposePortRequest{SandboxId: "sb-1", GuestPort: 8000}); err != nil || ep.HostPort != 48000 {
		t.Errorf("expose: %v %v", ep, err)
	}
	if _, err := c.ExposePort(ctx, &pb.ExposePortRequest{SandboxId: "sb-1", GuestPort: 70000}); code(err) != codes.InvalidArgument {
		t.Errorf("bad port: %v", err)
	}
	if _, err := c.ExposePort(ctx, &pb.ExposePortRequest{SandboxId: "sb-9", GuestPort: 80}); code(err) != codes.NotFound {
		t.Errorf("expose on a missing sandbox: %v", err)
	}
	fr, err := c.ForkSandbox(ctx, &pb.ForkSandboxRequest{Id: "sb-1", N: 3})
	if err != nil || len(fr.Children) != 3 || fr.Children[0].Parent != "sb-1" || fr.Layer != "layer-x" ||
		fr.Timings.ParentPausedMs != 3 || len(fr.Timings.RestoreMs) != 3 || fr.Timings.DirtyBytes != 1<<20 {
		t.Errorf("fork: %v %v", fr, err)
	}
	if _, err := c.ForkSandbox(ctx, &pb.ForkSandboxRequest{Id: "sb-1", N: 0}); code(err) != codes.InvalidArgument {
		t.Errorf("fork 0: %v", err)
	}
	if _, err := c.ForkSandbox(ctx, &pb.ForkSandboxRequest{Id: "sb-9", N: 1}); code(err) != codes.NotFound {
		t.Errorf("fork of a missing sandbox: %v", err)
	}
	if _, err := c.DestroySandbox(ctx, &pb.DestroySandboxRequest{Id: "sb-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSandbox(ctx, &pb.GetSandboxRequest{Id: "sb-1"}); code(err) != codes.NotFound {
		t.Errorf("after destroy: %v", err)
	}
	if _, err := c.DestroySandbox(ctx, &pb.DestroySandboxRequest{Id: "sb-1"}); code(err) != codes.NotFound {
		t.Errorf("destroy twice: %v", err)
	}
}

type execResult struct {
	started        *pb.ExecStarted
	stdout, stderr bytes.Buffer
	exit           *pb.ExecExit
}

func runExec(t *testing.T, ctx context.Context, c pb.ForkboxClient, r *pb.ExecRequest) (*execResult, error) {
	t.Helper()
	st, err := c.Exec(ctx, r)
	if err != nil {
		return nil, err
	}
	res := &execResult{}
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			return res, nil
		}
		if err != nil {
			return res, err
		}
		switch e := ev.Event.(type) {
		case *pb.ExecEvent_Started:
			res.started = e.Started
		case *pb.ExecEvent_Stdout:
			res.stdout.Write(e.Stdout)
		case *pb.ExecEvent_Stderr:
			res.stderr.Write(e.Stderr)
		case *pb.ExecEvent_Exit:
			res.exit = e.Exit
		}
	}
}

func TestExec(t *testing.T) {
	b := newFakeBackend(t)
	c := dial(t, &Server{B: b}, nil)
	ctx := testCtx(t)
	sb, err := c.CreateSandbox(ctx, &pb.CreateSandboxRequest{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := runExec(t, ctx, c, &pb.ExecRequest{SandboxId: sb.Id, Args: []string{"sh", "-c", "echo out; echo err >&2; exit 4"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.started == nil || res.stdout.String() != "out\n" || res.stderr.String() != "err\n" || res.exit.GetExitCode() != 4 {
		t.Errorf("exec: started %v stdout %q stderr %q exit %v", res.started, res.stdout.String(), res.stderr.String(), res.exit)
	}
	if _, err := runExec(t, ctx, c, &pb.ExecRequest{SandboxId: "sb-404", Args: []string{"true"}}); code(err) != codes.NotFound {
		t.Errorf("unknown sandbox: %v", err)
	}
	if _, err := runExec(t, ctx, c, &pb.ExecRequest{SandboxId: sb.Id}); code(err) != codes.InvalidArgument {
		t.Errorf("no args: %v", err)
	}
	if _, err := runExec(t, ctx, c, &pb.ExecRequest{SandboxId: sb.Id, Args: []string{"/no/such/bin"}}); code(err) != codes.NotFound {
		t.Errorf("missing binary: %v", err)
	}
	b.recs[sb.Id].State = store.StateCreating
	if _, err := runExec(t, ctx, c, &pb.ExecRequest{SandboxId: sb.Id, Args: []string{"true"}}); code(err) != codes.FailedPrecondition {
		t.Errorf("not running: %v", err)
	}
	b.recs[sb.Id].State = store.StateRunning

	// Cancelling the call kills the command.
	cctx, cancel := context.WithCancel(ctx)
	st, err := c.Exec(cctx, &pb.ExecRequest{SandboxId: sb.Id, Args: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := st.Recv()
	if err != nil {
		t.Fatal(err)
	}
	pid := int(ev.GetStarted().GetPid())
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d survived a cancelled Exec", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFiles(t *testing.T) {
	b := newFakeBackend(t)
	c := dial(t, &Server{B: b}, nil)
	ctx := testCtx(t)
	sb, _ := c.CreateSandbox(ctx, &pb.CreateSandboxRequest{Template: "python"})
	data := make([]byte, 3<<20+7)
	rand.Read(data)
	sum := sha256.Sum256(data)
	path := filepath.Join(b.dir, "x", "data.bin")

	ws, err := c.WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ws.Send(&pb.WriteFileRequest{Msg: &pb.WriteFileRequest_Header{Header: &pb.WriteFileHeader{SandboxId: sb.Id, Path: path, Mode: 0o600, MakeParents: true}}})
	for off := 0; off < len(data); off += 100 << 10 {
		ws.Send(&pb.WriteFileRequest{Msg: &pb.WriteFileRequest_Data{Data: data[off:min(off+100<<10, len(data))]}})
	}
	wr, err := ws.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	if wr.Size != uint64(len(data)) || !bytes.Equal(wr.Sha256, sum[:]) {
		t.Errorf("write: %d %x", wr.Size, wr.Sha256)
	}
	rs, err := c.ReadFile(ctx, &pb.ReadFileRequest{SandboxId: sb.Id, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	var back bytes.Buffer
	var last *pb.FileChunk
	for {
		ch, err := rs.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		back.Write(ch.Data)
		if ch.Last {
			last = ch
		}
	}
	if !bytes.Equal(back.Bytes(), data) || last == nil || !bytes.Equal(last.Sha256, sum[:]) {
		t.Errorf("read back %d bytes, last %v", back.Len(), last)
	}
	rs, _ = c.ReadFile(ctx, &pb.ReadFileRequest{SandboxId: sb.Id, Path: filepath.Join(b.dir, "missing")})
	if _, err := rs.Recv(); code(err) != codes.NotFound {
		t.Errorf("missing file: %v", err)
	}
	ws, _ = c.WriteFile(ctx)
	ws.Send(&pb.WriteFileRequest{Msg: &pb.WriteFileRequest_Data{Data: []byte("x")}})
	if _, err := ws.CloseAndRecv(); code(err) != codes.InvalidArgument {
		t.Errorf("no header: %v", err)
	}
}

func TestTokenOnTCP(t *testing.T) {
	b := newFakeBackend(t)
	tok, err := NewToken("0123456789abcdef-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewToken("short"); err == nil {
		t.Error("short token accepted")
	}
	c := dial(t, &Server{B: b}, tok)
	ctx := testCtx(t)
	if _, err := c.ListSandboxes(ctx, &pb.ListSandboxesRequest{}); code(err) != codes.Unauthenticated {
		t.Errorf("no token: %v", err)
	}
	bad := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer wrong-token-wrong-token")
	if _, err := c.ListSandboxes(bad, &pb.ListSandboxesRequest{}); code(err) != codes.Unauthenticated {
		t.Errorf("wrong token: %v", err)
	}
	good := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer 0123456789abcdef-secret")
	if _, err := c.ListSandboxes(good, &pb.ListSandboxesRequest{}); err != nil {
		t.Errorf("right token: %v", err)
	}
	if _, err := runExec(t, ctx, c, &pb.ExecRequest{SandboxId: "x", Args: []string{"true"}}); code(err) != codes.Unauthenticated {
		t.Errorf("stream without token: %v", err)
	}
}

func TestHTTPGateway(t *testing.T) {
	b := newFakeBackend(t)
	tok, _ := NewToken("0123456789abcdef-secret")
	ts := httptest.NewServer(NewHTTPHandler(&Server{B: b}, tok))
	defer ts.Close()
	do := func(method, path, body string, auth bool) (*http.Response, []byte) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer 0123456789abcdef-secret")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp, out
	}
	if resp, _ := do("GET", "/v1/sandboxes", "", false); resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	resp, body := do("POST", "/v1/sandboxes", `{"template":"python","ttl_s":"30"}`, true)
	var sb map[string]any
	json.Unmarshal(body, &sb)
	if resp.StatusCode != 200 || sb["id"] != "sb-1" || sb["state"] != "running" {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	resp, body = do("GET", "/v1/sandboxes/sb-9", "", true)
	if resp.StatusCode != 404 || !strings.Contains(string(body), `"NOT_FOUND"`) {
		t.Errorf("missing: %d %s", resp.StatusCode, body)
	}
	if resp, body = do("POST", "/v1/sandboxes", `{"tempate":"x"}`, true); resp.StatusCode != 400 {
		t.Errorf("unknown field: %d %s", resp.StatusCode, body)
	}

	// Exec over server-sent events.
	resp, body = do("POST", "/v1/sandboxes/sb-1/exec", `{"args":["sh","-c","echo hi; echo oops >&2; exit 2"]}`, true)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q: %s", ct, body)
	}
	events := map[string][]string{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	var name string
	for sc.Scan() {
		line := sc.Text()
		if n, ok := strings.CutPrefix(line, "event: "); ok {
			name = n
		} else if d, ok := strings.CutPrefix(line, "data: "); ok {
			events[name] = append(events[name], d)
		}
	}
	dec := func(ev string) string {
		var out string
		for _, d := range events[ev] {
			var m map[string]string
			json.Unmarshal([]byte(d), &m)
			b, _ := base64.StdEncoding.DecodeString(m["b64"])
			out += string(b)
		}
		return out
	}
	if len(events["started"]) != 1 || dec("stdout") != "hi\n" || dec("stderr") != "oops\n" || len(events["exit"]) != 1 || !strings.Contains(events["exit"][0], `"exit_code":2`) {
		t.Errorf("events %v", events)
	}

	// Files.
	path := filepath.Join(b.dir, "h", "f.txt")
	resp, body = do("PUT", "/v1/sandboxes/sb-1/files?path="+path+"&parents=1&mode=0640", "hello gateway", true)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"size":"13"`) {
		t.Errorf("put: %d %s", resp.StatusCode, body)
	}
	if fi, _ := os.Stat(path); fi == nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("mode: %v", fi)
	}
	resp, body = do("GET", "/v1/sandboxes/sb-1/files?path="+path, "", true)
	if resp.StatusCode != 200 || string(body) != "hello gateway" {
		t.Errorf("get: %d %q", resp.StatusCode, body)
	}
	resp, _ = do("GET", "/v1/sandboxes/sb-1/files?path="+path+".missing", "", true)
	if resp.StatusCode != 404 {
		t.Errorf("missing file: %d", resp.StatusCode)
	}
	if resp, _ = do("DELETE", "/v1/sandboxes/sb-1", "", true); resp.StatusCode != 200 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
}
