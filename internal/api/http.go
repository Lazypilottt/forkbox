package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
	pb "github.com/Lazypilottt/forkbox/proto/forkboxpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// HTTP gateway:
//
//	GET    /v1/sandboxes                       list
//	POST   /v1/sandboxes                       create  {"template": "python", "ttl_s": 600}
//	GET    /v1/sandboxes/{id}                  get
//	DELETE /v1/sandboxes/{id}                  destroy
//	POST   /v1/sandboxes/{id}/exec             exec    {"args": [...], "env": [...], "cwd": "", "timeout_ms": 0}
//	       answers text/event-stream: events "started", "stdout", "stderr" (data {"b64": ...}), "exit"
//	GET    /v1/sandboxes/{id}/files?path=/abs  read a file (raw bytes)
//	PUT    /v1/sandboxes/{id}/files?path=/abs&mode=0644&parents=1   write a file (raw body)
//	POST   /v1/sandboxes/{id}/ports            expose  {"guest_port": 8000, "host_port": 0}
//	POST   /v1/sandboxes/{id}/fork             fork    {"n": 10}
//
// Errors are JSON {"code": "NOT_FOUND", "message": "..."} with a matching
// HTTP status.

// NewHTTPHandler returns the gateway; tok may be nil (Unix socket).
func NewHTTPHandler(s *Server, tok *Token) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.ListSandboxes(r.Context(), &pb.ListSandboxesRequest{})
		writeProto(w, out, err)
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req pb.CreateSandboxRequest
		if err := readProto(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		out, err := s.CreateSandbox(r.Context(), &req)
		writeProto(w, out, err)
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetSandbox(r.Context(), &pb.GetSandboxRequest{Id: r.PathValue("id")})
		writeProto(w, out, err)
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.DestroySandbox(r.Context(), &pb.DestroySandboxRequest{Id: r.PathValue("id")})
		writeProto(w, out, err)
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.httpExec)
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", s.httpReadFile)
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", s.httpWriteFile)
	mux.HandleFunc("POST /v1/sandboxes/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		var req pb.ForkSandboxRequest
		if err := readProto(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		req.Id = r.PathValue("id")
		out, err := s.ForkSandbox(r.Context(), &req)
		writeProto(w, out, err)
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/ports", func(w http.ResponseWriter, r *http.Request) {
		var req pb.ExposePortRequest
		if err := readProto(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		req.SandboxId = r.PathValue("id")
		out, err := s.ExposePort(r.Context(), &req)
		writeProto(w, out, err)
	})
	if tok == nil {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tok.Valid(r.Header.Get("Authorization")) {
			writeErr(w, status.Error(codes.Unauthenticated, "missing or wrong bearer token"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) httpExec(w http.ResponseWriter, r *http.Request) {
	var req pb.ExecRequest
	if err := readProto(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	req.SandboxId = r.PathValue("id")
	if len(req.Args) == 0 {
		writeErr(w, status.Error(codes.InvalidArgument, "args is empty"))
		return
	}
	ac, err := s.running(r.Context(), req.SandboxId)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := ac.Start(r.Context(), agentclient.ExecRequest{Args: req.Args, Env: req.Env, Cwd: req.Cwd, Timeout: time.Duration(req.TimeoutMs) * time.Millisecond})
	if err != nil {
		writeErr(w, toStatus(err))
		return
	}
	defer p.Close()
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	event := func(name string, v any) error {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	if event("started", map[string]any{"exec_id": p.ID, "pid": p.Pid}) != nil {
		return
	}
	var werr error
	exit, err := p.Wait(func(st agentclient.Stream, d []byte) {
		if werr != nil {
			return
		}
		name := "stdout"
		if st == agentclient.Stderr {
			name = "stderr"
		}
		if werr = event(name, map[string]string{"b64": base64.StdEncoding.EncodeToString(d)}); werr != nil {
			p.Close()
		}
	})
	if werr != nil {
		return
	}
	if err != nil {
		st := status.Convert(toStatus(err))
		event("error", map[string]string{"code": st.Code().String(), "message": st.Message()})
		return
	}
	event("exit", map[string]any{"exit_code": exit.Code, "signal": exit.Signal, "timed_out": exit.TimedOut,
		"duration_ms": float64(exit.Duration.Microseconds()) / 1000})
}

func (s *Server) httpReadFile(w http.ResponseWriter, r *http.Request) {
	ac, err := s.running(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// Buffer the first chunk's error: once bytes are sent the status is 200.
	lw := &lazyWriter{w: w}
	fi, err := ac.ReadFile(r.Context(), r.URL.Query().Get("path"), lw)
	if err != nil {
		if !lw.started {
			writeErr(w, toStatus(err))
		}
		return
	}
	if !lw.started { // empty file
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	}
	_ = fi // the agent client already checked size and SHA-256
}

type lazyWriter struct {
	w       http.ResponseWriter
	started bool
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.w.Header().Set("Content-Type", "application/octet-stream")
		l.w.WriteHeader(http.StatusOK)
		l.started = true
	}
	return l.w.Write(p)
}

func (s *Server) httpWriteFile(w http.ResponseWriter, r *http.Request) {
	ac, err := s.running(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	var mode uint64
	if m := q.Get("mode"); m != "" {
		if mode, err = strconv.ParseUint(m, 8, 32); err != nil {
			writeErr(w, status.Error(codes.InvalidArgument, "mode: want octal like 0644"))
			return
		}
	}
	fi, err := ac.WriteFile(r.Context(), q.Get("path"), r.Body,
		agentclient.WriteOptions{Mode: fs.FileMode(mode), MakeParents: q.Get("parents") == "1"})
	if err != nil {
		writeErr(w, toStatus(err))
		return
	}
	writeProto(w, &pb.WriteFileResponse{Size: fi.Size, Sha256: fi.SHA256}, nil)
}

var (
	protoOut = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
	protoIn  = protojson.UnmarshalOptions{DiscardUnknown: false}
)

// maxBody bounds JSON request bodies.
const maxBody = 1 << 20

func readProto(r *http.Request, m proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if len(b) > maxBody {
		return status.Error(codes.InvalidArgument, "request body too large")
	}
	if len(b) == 0 {
		return nil
	}
	if err := protoIn.Unmarshal(b, m); err != nil {
		return status.Errorf(codes.InvalidArgument, "request body: %v", err)
	}
	return nil
}

func writeProto(w http.ResponseWriter, m proto.Message, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := protoOut.Marshal(m)
	if err != nil {
		writeErr(w, status.Error(codes.Internal, err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(b, '\n'))
}

// httpStatus maps gRPC codes to HTTP statuses.
var httpStatus = map[codes.Code]int{
	codes.NotFound: http.StatusNotFound, codes.InvalidArgument: http.StatusBadRequest,
	codes.FailedPrecondition: http.StatusConflict, codes.AlreadyExists: http.StatusConflict,
	codes.ResourceExhausted: http.StatusServiceUnavailable, codes.DeadlineExceeded: http.StatusGatewayTimeout,
	codes.Unavailable: http.StatusBadGateway, codes.Unauthenticated: http.StatusUnauthorized,
	codes.PermissionDenied: http.StatusForbidden, codes.Canceled: 499,
}

func writeErr(w http.ResponseWriter, err error) {
	st := status.Convert(toStatus(err))
	code, ok := httpStatus[st.Code()]
	if !ok {
		code = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"code": codeName(st.Code()), "message": st.Message()})
	w.Write(append(b, '\n'))
}

// codeName is the canonical upper-case name, e.g. NOT_FOUND.
func codeName(c codes.Code) string {
	names := map[codes.Code]string{
		codes.OK: "OK", codes.Canceled: "CANCELLED", codes.Unknown: "UNKNOWN", codes.InvalidArgument: "INVALID_ARGUMENT",
		codes.DeadlineExceeded: "DEADLINE_EXCEEDED", codes.NotFound: "NOT_FOUND", codes.AlreadyExists: "ALREADY_EXISTS",
		codes.PermissionDenied: "PERMISSION_DENIED", codes.ResourceExhausted: "RESOURCE_EXHAUSTED",
		codes.FailedPrecondition: "FAILED_PRECONDITION", codes.Aborted: "ABORTED", codes.OutOfRange: "OUT_OF_RANGE",
		codes.Unimplemented: "UNIMPLEMENTED", codes.Internal: "INTERNAL", codes.Unavailable: "UNAVAILABLE",
		codes.DataLoss: "DATA_LOSS", codes.Unauthenticated: "UNAUTHENTICATED",
	}
	return names[c]
}
