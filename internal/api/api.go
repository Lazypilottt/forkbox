// Package api serves the forkboxd API: gRPC (proto/forkbox.proto) and a
// small HTTP gateway with server-sent events for Exec.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/internal/sandbox"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
	"github.com/Lazypilottt/forkbox/internal/vm"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
	pb "github.com/Lazypilottt/forkbox/proto/forkboxpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Backend is what the API drives; *sandbox.Manager implements it.
type Backend interface {
	Create(ctx context.Context, o sandbox.CreateOptions) (*sandbox.Created, error)
	Get(ctx context.Context, id string) (*store.Sandbox, error)
	List(ctx context.Context) ([]*store.Sandbox, error)
	Destroy(ctx context.Context, id string) error
	Agent(id string) *agentclient.Client
	Expose(ctx context.Context, id string, guestPort, hostPort int) (int, error)
}

// Server implements the Forkbox gRPC service.
type Server struct {
	pb.UnimplementedForkboxServer
	B Backend
	// AgentTimeout bounds unary agent calls (default 30 s). Exec and file
	// streams are bounded by the caller's context instead.
	AgentTimeout time.Duration
	// ExposeHost is reported with port forwards (where they listen).
	ExposeHost string
}

var _ pb.ForkboxServer = (*Server)(nil)

func (s *Server) CreateSandbox(ctx context.Context, r *pb.CreateSandboxRequest) (*pb.Sandbox, error) {
	if r.Template == "" {
		return nil, status.Error(codes.InvalidArgument, "template is required")
	}
	c, err := s.B.Create(ctx, sandbox.CreateOptions{Template: r.Template, TTL: time.Duration(r.TtlS) * time.Second})
	if err != nil {
		return nil, toStatus(err)
	}
	out := toPB(c.Sandbox)
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	out.Timings = &pb.CreateTimings{
		TotalMs: ms(c.Timings.Total), DiskMs: ms(c.Timings.Disk), RestoreMs: ms(c.Timings.Restore),
		ResumeMs: ms(c.Timings.Resume), AgentMs: ms(c.Timings.Agent), IdentityMs: ms(c.Timings.Identity),
	}
	return out, nil
}

func (s *Server) GetSandbox(ctx context.Context, r *pb.GetSandboxRequest) (*pb.Sandbox, error) {
	sb, err := s.B.Get(ctx, r.Id)
	if err != nil {
		return nil, toStatus(err)
	}
	return toPB(sb), nil
}

func (s *Server) ListSandboxes(ctx context.Context, _ *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	all, err := s.B.List(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &pb.ListSandboxesResponse{}
	for _, sb := range all {
		out.Sandboxes = append(out.Sandboxes, toPB(sb))
	}
	return out, nil
}

func (s *Server) DestroySandbox(ctx context.Context, r *pb.DestroySandboxRequest) (*pb.DestroySandboxResponse, error) {
	if err := s.B.Destroy(ctx, r.Id); err != nil {
		return nil, toStatus(err)
	}
	return &pb.DestroySandboxResponse{}, nil
}

func (s *Server) ExposePort(ctx context.Context, r *pb.ExposePortRequest) (*pb.ExposePortResponse, error) {
	if r.GuestPort == 0 || r.GuestPort > 65535 || r.HostPort > 65535 {
		return nil, status.Error(codes.InvalidArgument, "ports must be 1-65535 (host_port 0 picks one)")
	}
	port, err := s.B.Expose(ctx, r.SandboxId, int(r.GuestPort), int(r.HostPort))
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.ExposePortResponse{Host: s.ExposeHost, HostPort: uint32(port)}, nil
}

// running returns the agent of a running sandbox.
func (s *Server) running(ctx context.Context, id string) (*agentclient.Client, error) {
	sb, err := s.B.Get(ctx, id)
	if err != nil {
		return nil, toStatus(err)
	}
	if sb.State != store.StateRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is %s", id, sb.State)
	}
	return s.B.Agent(id), nil
}

func (s *Server) Exec(r *pb.ExecRequest, stream grpc.ServerStreamingServer[pb.ExecEvent]) error {
	ctx := stream.Context()
	if len(r.Args) == 0 {
		return status.Error(codes.InvalidArgument, "args is empty")
	}
	ac, err := s.running(ctx, r.SandboxId)
	if err != nil {
		return err
	}
	p, err := ac.Start(ctx, agentclient.ExecRequest{Args: r.Args, Env: r.Env, Cwd: r.Cwd, Timeout: time.Duration(r.TimeoutMs) * time.Millisecond})
	if err != nil {
		return toStatus(err)
	}
	if err := stream.Send(&pb.ExecEvent{Event: &pb.ExecEvent_Started{Started: &pb.ExecStarted{ExecId: p.ID, Pid: int32(p.Pid)}}}); err != nil {
		p.Close()
		return err
	}
	var sendErr error
	exit, err := p.Wait(func(st agentclient.Stream, d []byte) {
		if sendErr != nil {
			return
		}
		ev := &pb.ExecEvent{Event: &pb.ExecEvent_Stdout{Stdout: d}}
		if st == agentclient.Stderr {
			ev = &pb.ExecEvent{Event: &pb.ExecEvent_Stderr{Stderr: d}}
		}
		// Send blocks when the client reads slowly; that blocks the agent
		// stream and so the process, rather than buffering here.
		if sendErr = stream.Send(ev); sendErr != nil {
			p.Close()
		}
	})
	if sendErr != nil {
		return sendErr
	}
	if err != nil {
		return toStatus(err)
	}
	return stream.Send(&pb.ExecEvent{Event: &pb.ExecEvent_Exit{Exit: &pb.ExecExit{
		ExitCode: int32(exit.Code), Signal: exit.Signal, TimedOut: exit.TimedOut,
		DurationMs: float64(exit.Duration.Microseconds()) / 1000,
	}}})
}

// chunkWriter turns ReadFile output into FileChunk messages.
type chunkWriter struct {
	stream grpc.ServerStreamingServer[pb.FileChunk]
}

func (w chunkWriter) Write(p []byte) (int, error) {
	if err := w.stream.Send(&pb.FileChunk{Data: p}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *Server) ReadFile(r *pb.ReadFileRequest, stream grpc.ServerStreamingServer[pb.FileChunk]) error {
	ac, err := s.running(stream.Context(), r.SandboxId)
	if err != nil {
		return err
	}
	fi, err := ac.ReadFile(stream.Context(), r.Path, chunkWriter{stream})
	if err != nil {
		return toStatus(err)
	}
	return stream.Send(&pb.FileChunk{Last: true, Size: fi.Size, Sha256: fi.SHA256})
}

// writeStream adapts WriteFile's client stream to an io.Reader.
type writeStream struct {
	stream grpc.ClientStreamingServer[pb.WriteFileRequest, pb.WriteFileResponse]
	buf    []byte
	err    error
}

func (w *writeStream) Read(p []byte) (int, error) {
	for len(w.buf) == 0 {
		if w.err != nil {
			return 0, w.err
		}
		m, err := w.stream.Recv()
		if err != nil {
			w.err = err // io.EOF when the client is done
			continue
		}
		if m.GetHeader() != nil {
			w.err = status.Error(codes.InvalidArgument, "write_file: header sent twice")
			continue
		}
		w.buf = m.GetData()
	}
	n := copy(p, w.buf)
	w.buf = w.buf[n:]
	return n, nil
}

func (s *Server) WriteFile(stream grpc.ClientStreamingServer[pb.WriteFileRequest, pb.WriteFileResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	h := first.GetHeader()
	if h == nil {
		return status.Error(codes.InvalidArgument, "write_file: the first message must be the header")
	}
	ac, err := s.running(stream.Context(), h.SandboxId)
	if err != nil {
		return err
	}
	fi, err := ac.WriteFile(stream.Context(), h.Path, &writeStream{stream: stream},
		agentclient.WriteOptions{Mode: fs.FileMode(h.Mode), MakeParents: h.MakeParents})
	if err != nil {
		return toStatus(err)
	}
	return stream.SendAndClose(&pb.WriteFileResponse{Size: fi.Size, Sha256: fi.SHA256})
}

func toPB(sb *store.Sandbox) *pb.Sandbox {
	return &pb.Sandbox{
		Id: sb.ID, Template: sb.TemplateName, TemplateId: sb.Template, Parent: sb.Parent, State: sb.State,
		CreatedAtUnixMs: sb.CreatedAt.UnixMilli(), TtlS: uint64(sb.TTL / time.Second), Pid: int32(sb.Pid),
	}
}

// toStatus maps errors to the codes documented in forkbox.proto.
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ae *agentclient.Error
	code := codes.Internal
	switch {
	case errors.Is(err, context.Canceled):
		code = codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	case errors.Is(err, store.ErrNotFound), errors.Is(err, template.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		code = codes.NotFound
	case errors.Is(err, store.ErrExists):
		code = codes.AlreadyExists
	case errors.Is(err, store.ErrNoSlot):
		code = codes.ResourceExhausted
	case errors.Is(err, fs.ErrInvalid), errors.Is(err, vm.ErrConfig), errors.Is(err, template.ErrSpec):
		code = codes.InvalidArgument
	case errors.Is(err, fs.ErrPermission):
		code = codes.PermissionDenied
	case errors.Is(err, sandbox.ErrNoReflink), errors.Is(err, vm.ErrState):
		code = codes.FailedPrecondition
	case errors.Is(err, agentclient.ErrNotListening), errors.Is(err, agentclient.ErrChecksum):
		code = codes.Unavailable
	case errors.As(err, &ae):
		code = codes.Internal
	case strings.Contains(err.Error(), "sandbox id") && strings.Contains(err.Error(), "use 1-32"):
		code = codes.InvalidArgument
	}
	return status.Error(code, err.Error())
}

// Token checks bearer tokens on TCP listeners. The Unix sockets are
// owner-only and need no token.
type Token struct{ sum [32]byte }

// NewToken returns a checker for tok (at least 16 bytes).
func NewToken(tok string) (*Token, error) {
	tok = strings.TrimSpace(tok)
	if len(tok) < 16 {
		return nil, errors.New("api token must be at least 16 characters")
	}
	return &Token{sum: sha256.Sum256([]byte(tok))}, nil
}

// Valid compares in constant time.
func (t *Token) Valid(header string) bool {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	return subtle.ConstantTimeCompare(got[:], t.sum[:]) == 1
}

func (t *Token) check(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if t.Valid(v) {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "missing or wrong bearer token")
}

// ServerOptions returns gRPC options enforcing the token.
func (t *Token) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if err := t.check(ctx); err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			if err := t.check(ss.Context()); err != nil {
				return err
			}
			return h(srv, ss)
		}),
	}
}

// NewGRPCServer returns a gRPC server for s; tok may be nil (Unix socket).
func NewGRPCServer(s *Server, tok *Token) *grpc.Server {
	var opts []grpc.ServerOption
	if tok != nil {
		opts = tok.ServerOptions()
	}
	g := grpc.NewServer(append(opts, grpc.MaxRecvMsgSize(agentwire.MaxFrame*2))...)
	pb.RegisterForkboxServer(g, s)
	return g
}

var _ io.Reader = (*writeStream)(nil)
