// Package agent is the guest side of Forkbox: the server behind
// forkbox-agent, which runs inside every VM and answers the host over vsock
// (proto/agent.proto, one operation per connection).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/proto/agentpb"
)

// InstallPath is where templates put the agent binary in the guest.
const InstallPath = "/usr/local/bin/forkbox-agent"

// SystemdUnit starts the agent early in boot and restarts it if it dies.
// KillMode=process keeps processes started through Exec alive across an
// agent restart.
const SystemdUnit = `[Unit]
Description=Forkbox guest agent
DefaultDependencies=no
After=local-fs.target
Before=sysinit.target

[Service]
ExecStart=` + InstallPath + `
Restart=always
RestartSec=100ms
KillMode=process
OOMScoreAdjust=-900

[Install]
WantedBy=sysinit.target
`

// Defaults for Server.
const (
	DefaultMaxConns = 512
	// requestTimeout bounds the wait for the first frame of a connection.
	requestTimeout = 10 * time.Second
)

// GuestConfigPath holds the image's and template's defaults for Exec,
// written by the template builder.
const GuestConfigPath = "/etc/forkbox/agent.json"

// GuestConfig is the content of GuestConfigPath.
type GuestConfig struct {
	Env []string  `json:"env"` // KEY=value, over the agent's defaults
	Cwd string    `json:"cwd"` // default working directory
	Net *GuestNet `json:"net,omitempty"`
}

// GuestNet is the guest's network, set up by the agent in init mode.
type GuestNet struct {
	Iface   string `json:"iface"`   // e.g. eth0
	Addr    string `json:"addr"`    // CIDR, e.g. 172.30.0.2/30
	Gateway string `json:"gateway"` // also the DNS server
}

// LoadGuestConfig reads path. A missing file is an empty config.
func LoadGuestConfig(path string) (GuestConfig, error) {
	var c GuestConfig
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Server answers agent requests. The zero value is not usable; use New.
type Server struct {
	Version  string
	MaxConns int
	Log      *log.Logger
	// Env and Cwd are Exec defaults (see GuestConfig).
	Env []string
	Cwd string
	// EtcDir is where machine-id and hostname are written (default /etc).
	EtcDir string

	// GenerationFile overrides agent.GenerationFile (tests).
	GenerationFile string

	sys        system
	generation atomic.Uint64 // NEW_VMGENID uevents seen
	restores   atomic.Uint64 // restores handled (uevents and Restored calls)
	sandboxID  string        // guarded by mu

	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}

	mu    sync.Mutex
	execs map[string]int // exec id -> process group id
	wg    sync.WaitGroup
}

// New returns a server reporting the given version.
func New(version string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		Version:  version,
		MaxConns: DefaultMaxConns,
		Log:      log.New(io.Discard, "", 0),
		EtcDir:   "/etc",
		ctx:      ctx,
		cancel:   cancel,
		execs:    map[string]int{},
		sys:      defaultSystem(),
	}
}

// Serve accepts connections until ln is closed.
func (s *Server) Serve(ln net.Listener) error {
	if s.sem == nil {
		s.sem = make(chan struct{}, max(s.MaxConns, 1))
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case s.sem <- struct{}{}:
		default:
			_ = agentwire.Write(c, errResp(agentpb.Error_INTERNAL, "agent: too many connections"))
			c.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer func() { <-s.sem; s.wg.Done() }()
			s.ServeConn(c)
		}()
	}
}

// Close kills every running Exec and waits for connections to finish.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

// ServeConn handles one connection: one request and its responses.
func (s *Server) ServeConn(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(requestTimeout))
	req := &agentpb.Request{}
	if err := agentwire.Read(c, req); err != nil {
		if !errors.Is(err, io.EOF) {
			s.Log.Printf("reading request: %v", err)
		}
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var err error
	switch op := req.Op.(type) {
	case *agentpb.Request_Ping:
		err = agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_Pong{Pong: &agentpb.PingResponse{
			AgentVersion: s.Version, ProtocolVersion: agentwire.ProtocolVersion,
		}}})
	case *agentpb.Request_Exec:
		err = s.exec(c, op.Exec)
	case *agentpb.Request_Signal:
		err = s.signal(c, op.Signal)
	case *agentpb.Request_ReadFile:
		err = readFile(c, op.ReadFile)
	case *agentpb.Request_WriteFile:
		err = writeFile(c, op.WriteFile)
	case *agentpb.Request_Stat:
		err = stat(c, op.Stat)
	case *agentpb.Request_WaitPort:
		err = s.waitPort(c, op.WaitPort)
	case *agentpb.Request_Info:
		in := info(s.Version)
		s.mu.Lock()
		in.SandboxId = s.sandboxID
		s.mu.Unlock()
		in.Generation = s.generation.Load()
		err = agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_Info{Info: in}})
	case *agentpb.Request_Restored:
		err = s.restored(c, op.Restored)
	default:
		err = agentwire.Write(c, errResp(agentpb.Error_INVALID_ARGUMENT, fmt.Sprintf("unknown or unexpected operation %T", req.Op)))
	}
	if err != nil && !isDisconnect(err) {
		s.Log.Printf("%T: %v", req.Op, err)
	}
}

func (s *Server) signal(c net.Conn, r *agentpb.SignalRequest) error {
	if r.Signal < 1 || r.Signal > 64 {
		return agentwire.Write(c, errResp(agentpb.Error_INVALID_ARGUMENT, fmt.Sprintf("signal %d out of range", r.Signal)))
	}
	s.mu.Lock()
	pgid, ok := s.execs[r.ExecId]
	s.mu.Unlock()
	if !ok {
		return agentwire.Write(c, errResp(agentpb.Error_NOT_FOUND, fmt.Sprintf("no running exec %q", r.ExecId)))
	}
	if err := killGroup(pgid, int(r.Signal)); err != nil {
		return writeErr(c, err)
	}
	return agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_Signal{Signal: &agentpb.SignalResponse{}}})
}

func (s *Server) waitPort(c net.Conn, r *agentpb.WaitPortRequest) error {
	if r.Port == 0 || r.Port > 65535 {
		return agentwire.Write(c, errResp(agentpb.Error_INVALID_ARGUMENT, fmt.Sprintf("port %d out of range", r.Port)))
	}
	deadline := time.Now().Add(time.Duration(min(r.TimeoutMs, uint64(10*time.Minute/time.Millisecond))) * time.Millisecond)
	for {
		ok := portListening(uint16(r.Port))
		if ok || !time.Now().Before(deadline) {
			return agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_WaitPort{WaitPort: &agentpb.WaitPortResponse{Listening: ok}}})
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func info(version string) *agentpb.InfoResponse {
	host, _ := os.Hostname()
	return &agentpb.InfoResponse{
		Hostname:         host,
		MachineId:        readTrim("/etc/machine-id"),
		BootId:           readTrim("/proc/sys/kernel/random/boot_id"),
		UptimeMs:         uptimeMs(),
		RealtimeUnixNano: time.Now().UnixNano(),
		AgentVersion:     version,
	}
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func errResp(code agentpb.Error_Code, msg string) *agentpb.Response {
	return &agentpb.Response{Msg: &agentpb.Response_Error{Error: &agentpb.Error{Code: code, Message: msg}}}
}

// writeErr sends err with the code that matches it.
func writeErr(c io.Writer, err error) error {
	code := agentpb.Error_INTERNAL
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = agentpb.Error_NOT_FOUND
	case errors.Is(err, fs.ErrPermission):
		code = agentpb.Error_PERMISSION_DENIED
	case errors.Is(err, fs.ErrInvalid):
		code = agentpb.Error_INVALID_ARGUMENT
	}
	return agentwire.Write(c, errResp(code, err.Error()))
}

func isDisconnect(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) ||
		strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "connection reset")
}
