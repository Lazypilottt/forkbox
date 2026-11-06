// Package agentclient is the host side of the guest agent protocol
// (proto/agent.proto). Every operation opens its own connection through the
// VM's vsock Unix socket, retrying with backoff while the agent is not yet
// listening. Snapshots reset vsock connections, so this is also how a client
// carries on after pause, snapshot, restore and resume: nothing to re-open.
package agentclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/proto/agentpb"
)

// DialFunc opens a raw connection to the agent, handshake included.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Client talks to one VM's agent. It is safe for concurrent use; every call
// is independent.
type Client struct {
	dial DialFunc
	// Backoff between connection attempts: starts at MinBackoff, doubles up
	// to MaxBackoff. Attempts continue until the call's context ends.
	MinBackoff, MaxBackoff time.Duration
}

// New returns a client for the agent behind a Firecracker vsock socket
// (vm.VM.VsockPath) on the given guest port (agentwire.Port).
func New(vsockPath string, port uint32) *Client {
	return NewWithDialer(FirecrackerDialer(vsockPath, port))
}

// NewWithDialer returns a client using dial for every connection.
func NewWithDialer(dial DialFunc) *Client {
	return &Client{dial: dial, MinBackoff: 10 * time.Millisecond, MaxBackoff: 500 * time.Millisecond}
}

// handshakeTimeout bounds one CONNECT exchange.
const handshakeTimeout = 2 * time.Second

// ErrNotListening means Firecracker closed the connection after CONNECT:
// nothing in the guest listens on the port (yet).
var ErrNotListening = errors.New("agentclient: guest port not listening")

// FirecrackerDialer connects to a guest vsock port through Firecracker's
// host-initiated connection protocol (docs/vsock.md): connect to the Unix
// socket, send "CONNECT <port>\n", read "OK <host port>\n".
func FirecrackerDialer(vsockPath string, port uint32) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", vsockPath)
		if err != nil {
			return nil, err
		}
		_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
		if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
			c.Close()
			return nil, err
		}
		// Read byte by byte: anything after the newline is agent data.
		var line []byte
		b := make([]byte, 1)
		for len(line) < 64 {
			if _, err := c.Read(b); err != nil {
				c.Close()
				if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
					return nil, ErrNotListening
				}
				return nil, err
			}
			if b[0] == '\n' {
				break
			}
			line = append(line, b[0])
		}
		if !strings.HasPrefix(string(line), "OK ") {
			c.Close()
			return nil, fmt.Errorf("agentclient: vsock handshake: unexpected answer %q", line)
		}
		_ = c.SetDeadline(time.Time{})
		return c, nil
	}
}

// connect dials with backoff until it succeeds or ctx ends. Once connected,
// the context also bounds the whole operation: cancelling it closes the
// connection.
func (c *Client) connect(ctx context.Context) (net.Conn, func(), error) {
	wait := c.MinBackoff
	var last error
	for attempt := 1; ; attempt++ {
		conn, err := c.dial(ctx)
		if err == nil {
			if dl, ok := ctx.Deadline(); ok {
				_ = conn.SetDeadline(dl)
			}
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			return conn, func() { stop(); conn.Close() }, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("agentclient: connecting (%d attempts): %w (last error: %v)", attempt, ctx.Err(), last)
		case <-time.After(wait):
		}
		wait = min(wait*2, c.MaxBackoff)
	}
}

// Error is an error reported by the agent.
type Error struct {
	Code    agentpb.Error_Code
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("agent: %s: %s", e.Code, e.Message) }

// Is lets callers test agent errors with errors.Is(err, fs.ErrNotExist) and
// fs.ErrPermission.
func (e *Error) Is(target error) bool {
	switch e.Code {
	case agentpb.Error_NOT_FOUND:
		return target == fs.ErrNotExist
	case agentpb.Error_PERMISSION_DENIED:
		return target == fs.ErrPermission
	case agentpb.Error_INVALID_ARGUMENT:
		return target == fs.ErrInvalid
	}
	return false
}

// ErrChecksum means a file transfer's SHA-256 differs between the sides.
var ErrChecksum = errors.New("agentclient: checksum mismatch")

// ErrProtocol means the agent sent something the protocol does not allow.
var ErrProtocol = errors.New("agentclient: protocol error")

func protoErr(want string, got *agentpb.Response) error {
	return fmt.Errorf("%w: expected %s, got %T", ErrProtocol, want, got.Msg)
}

// call sends one request and returns the connection for reading responses.
func (c *Client) call(ctx context.Context, req *agentpb.Request) (net.Conn, func(), error) {
	conn, done, err := c.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := agentwire.Write(conn, req); err != nil {
		done()
		return nil, nil, ctxErr(ctx, err)
	}
	return conn, done, nil
}

// recv reads one response, turning an agent error into *Error.
func recv(ctx context.Context, conn net.Conn) (*agentpb.Response, error) {
	resp := &agentpb.Response{}
	if err := agentwire.Read(conn, resp); err != nil {
		return nil, ctxErr(ctx, err)
	}
	if e := resp.GetError(); e != nil {
		return nil, &Error{Code: e.Code, Message: e.Message}
	}
	return resp, nil
}

// ctxErr reports the context's error when it caused a connection failure.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w (%v)", ctx.Err(), err)
	}
	return err
}

// unary runs a one-request, one-response operation.
func (c *Client) unary(ctx context.Context, req *agentpb.Request) (*agentpb.Response, error) {
	conn, done, err := c.call(ctx, req)
	if err != nil {
		return nil, err
	}
	defer done()
	return recv(ctx, conn)
}

// Ping returns the agent's version and protocol version.
func (c *Client) Ping(ctx context.Context) (*agentpb.PingResponse, error) {
	resp, err := c.unary(ctx, &agentpb.Request{Op: &agentpb.Request_Ping{Ping: &agentpb.PingRequest{}}})
	if err != nil {
		return nil, err
	}
	p := resp.GetPong()
	if p == nil {
		return nil, protoErr("pong", resp)
	}
	return p, nil
}

// WaitReady pings until the agent answers or ctx ends, also retrying
// connections that drop mid-request (as they do around a restore).
func (c *Client) WaitReady(ctx context.Context) (*agentpb.PingResponse, error) {
	wait := c.MinBackoff
	for {
		p, err := c.Ping(ctx)
		if err == nil {
			if p.ProtocolVersion != agentwire.ProtocolVersion {
				return p, fmt.Errorf("agentclient: agent %s speaks protocol %d, this host speaks %d", p.AgentVersion, p.ProtocolVersion, agentwire.ProtocolVersion)
			}
			return p, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
		wait = min(wait*2, c.MaxBackoff)
	}
}

// Signal sends a signal to the process group of a running Exec.
func (c *Client) Signal(ctx context.Context, execID string, sig syscall.Signal) error {
	resp, err := c.unary(ctx, &agentpb.Request{Op: &agentpb.Request_Signal{Signal: &agentpb.SignalRequest{ExecId: execID, Signal: int32(sig)}}})
	if err != nil {
		return err
	}
	if resp.GetSignal() == nil {
		return protoErr("signal", resp)
	}
	return nil
}

// Stat describes a guest path. A missing path is Exists=false, not an error.
func (c *Client) Stat(ctx context.Context, path string) (*agentpb.StatResponse, error) {
	resp, err := c.unary(ctx, &agentpb.Request{Op: &agentpb.Request_Stat{Stat: &agentpb.StatRequest{Path: path}}})
	if err != nil {
		return nil, err
	}
	s := resp.GetStat()
	if s == nil {
		return nil, protoErr("stat", resp)
	}
	return s, nil
}

// WaitPort waits up to timeout for a TCP port in the guest to be listening.
func (c *Client) WaitPort(ctx context.Context, port uint16, timeout time.Duration) (bool, error) {
	resp, err := c.unary(ctx, &agentpb.Request{Op: &agentpb.Request_WaitPort{WaitPort: &agentpb.WaitPortRequest{
		Port: uint32(port), TimeoutMs: uint64(timeout.Milliseconds()),
	}}})
	if err != nil {
		return false, err
	}
	w := resp.GetWaitPort()
	if w == nil {
		return false, protoErr("wait_port", resp)
	}
	return w.Listening, nil
}

// Info returns the guest's identity and clock.
func (c *Client) Info(ctx context.Context) (*agentpb.InfoResponse, error) {
	resp, err := c.unary(ctx, &agentpb.Request{Op: &agentpb.Request_Info{Info: &agentpb.InfoRequest{}}})
	if err != nil {
		return nil, err
	}
	i := resp.GetInfo()
	if i == nil {
		return nil, protoErr("info", resp)
	}
	return i, nil
}

// FileInfo is the result of a file transfer.
type FileInfo struct {
	Size   uint64
	SHA256 []byte
	Mode   fs.FileMode // ReadFile only
}

// WriteOptions control WriteFile.
type WriteOptions struct {
	Mode        fs.FileMode // 0 means 0644
	MakeParents bool
}

// WriteFile streams r to an absolute guest path. The file appears there only
// when complete; both sides hash the data and must agree.
func (c *Client) WriteFile(ctx context.Context, path string, r io.Reader, o WriteOptions) (*FileInfo, error) {
	conn, done, err := c.call(ctx, &agentpb.Request{Op: &agentpb.Request_WriteFile{WriteFile: &agentpb.WriteFileRequest{
		Path: path, Mode: uint32(o.Mode.Perm()), MakeParents: o.MakeParents,
	}}})
	if err != nil {
		return nil, err
	}
	defer done()
	// The agent may refuse the path at once; read its answer concurrently
	// so an early error is not hidden behind a failing upload.
	type result struct {
		resp *agentpb.Response
		err  error
	}
	answer := make(chan result, 1)
	go func() {
		resp, err := recv(ctx, conn)
		answer <- result{resp, err}
	}()

	h := sha256.New()
	var size uint64
	buf := make([]byte, agentwire.ChunkSize)
	var upErr error
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			h.Write(buf[:n])
			size += uint64(n)
		}
		last := rerr == io.EOF || rerr == io.ErrUnexpectedEOF
		if rerr != nil && !last {
			upErr = fmt.Errorf("agentclient: reading upload source: %w", rerr)
			break
		}
		if err := agentwire.Write(conn, &agentpb.Request{Op: &agentpb.Request_Chunk{Chunk: &agentpb.Chunk{Data: buf[:n], Eof: last}}}); err != nil {
			upErr = ctxErr(ctx, err)
			break
		}
		if last {
			break
		}
	}
	if upErr != nil {
		done() // the agent discards the partial file
	}
	res := <-answer
	// An agent error (say, a bad path) explains a broken upload better than
	// the broken pipe it caused, so it wins.
	var ae *Error
	switch {
	case errors.As(res.err, &ae):
		return nil, res.err
	case upErr != nil:
		return nil, upErr
	case res.err != nil:
		return nil, res.err
	}
	fd := res.resp.GetFileDone()
	if fd == nil {
		return nil, protoErr("file_done", res.resp)
	}
	sum := h.Sum(nil)
	if fd.Size != size || !bytes.Equal(fd.Sha256, sum) {
		return nil, fmt.Errorf("%w: sent %d bytes sha256 %x, agent wrote %d bytes sha256 %x", ErrChecksum, size, sum, fd.Size, fd.Sha256)
	}
	return &FileInfo{Size: size, SHA256: sum}, nil
}

// ReadFile streams an absolute guest path into w and checks the agent's
// SHA-256 of what it sent against the bytes received.
func (c *Client) ReadFile(ctx context.Context, path string, w io.Writer) (*FileInfo, error) {
	conn, done, err := c.call(ctx, &agentpb.Request{Op: &agentpb.Request_ReadFile{ReadFile: &agentpb.ReadFileRequest{Path: path}}})
	if err != nil {
		return nil, err
	}
	defer done()
	resp, err := recv(ctx, conn)
	if err != nil {
		return nil, err
	}
	hdr := resp.GetFileHeader()
	if hdr == nil {
		return nil, protoErr("file_header", resp)
	}
	h := sha256.New()
	var size uint64
	for {
		resp, err := recv(ctx, conn)
		if err != nil {
			return nil, err
		}
		switch m := resp.Msg.(type) {
		case *agentpb.Response_Chunk:
			h.Write(m.Chunk.Data)
			size += uint64(len(m.Chunk.Data))
			if _, err := w.Write(m.Chunk.Data); err != nil {
				return nil, err
			}
		case *agentpb.Response_FileDone:
			sum := h.Sum(nil)
			if m.FileDone.Size != size || !bytes.Equal(m.FileDone.Sha256, sum) {
				return nil, fmt.Errorf("%w: received %d bytes sha256 %x, agent sent %d bytes sha256 %x", ErrChecksum, size, sum, m.FileDone.Size, m.FileDone.Sha256)
			}
			return &FileInfo{Size: size, SHA256: sum, Mode: fs.FileMode(hdr.Mode)}, nil
		default:
			return nil, protoErr("chunk or file_done", resp)
		}
	}
}
