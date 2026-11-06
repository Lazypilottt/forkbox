package agentclient

import (
	"bytes"
	"context"
	"net"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/proto/agentpb"
)

// ExecRequest is a command to run in the guest.
type ExecRequest struct {
	Args    []string // argv; Args[0] is looked up in PATH
	Env     []string // KEY=value, added to the agent's defaults
	Cwd     string   // default /
	Timeout time.Duration
}

// Stream says which output a chunk came from.
type Stream = agentpb.Output_Stream

const (
	Stdout = agentpb.Output_STDOUT
	Stderr = agentpb.Output_STDERR
)

// Exit is how an Exec ended.
type Exit struct {
	Code     int    // -1 if killed by a signal
	Signal   string // e.g. "killed"; empty for a normal exit
	TimedOut bool
	Duration time.Duration
}

// Process is a running Exec.
type Process struct {
	ID  string // for Signal
	Pid int    // guest pid, also its process group

	ctx  context.Context
	conn net.Conn
	done func()
}

// Start runs a command and returns once the agent has started it.
func (c *Client) Start(ctx context.Context, r ExecRequest) (*Process, error) {
	conn, done, err := c.call(ctx, &agentpb.Request{Op: &agentpb.Request_Exec{Exec: &agentpb.ExecRequest{
		Args: r.Args, Env: r.Env, Cwd: r.Cwd, TimeoutMs: uint64(r.Timeout.Milliseconds()),
	}}})
	if err != nil {
		return nil, err
	}
	resp, err := recv(ctx, conn)
	if err != nil {
		done()
		return nil, err
	}
	st := resp.GetExecStarted()
	if st == nil {
		done()
		return nil, protoErr("exec_started", resp)
	}
	return &Process{ID: st.ExecId, Pid: int(st.Pid), ctx: ctx, conn: conn, done: done}, nil
}

// Wait streams the output to onOutput (which may be nil) until the process
// exits. data is only valid during the call. Nothing is buffered on the
// host: while onOutput runs, the agent stops reading the process's pipes,
// so a slow consumer slows the process down instead of using memory.
func (p *Process) Wait(onOutput func(s Stream, data []byte)) (*Exit, error) {
	defer p.done()
	for {
		resp, err := recv(p.ctx, p.conn)
		if err != nil {
			return nil, err
		}
		switch m := resp.Msg.(type) {
		case *agentpb.Response_Output:
			if onOutput != nil {
				onOutput(m.Output.Stream, m.Output.Data)
			}
		case *agentpb.Response_ExecExit:
			e := m.ExecExit
			return &Exit{Code: int(e.ExitCode), Signal: e.Signal, TimedOut: e.TimedOut, Duration: time.Duration(e.DurationMs) * time.Millisecond}, nil
		default:
			return nil, protoErr("output or exec_exit", resp)
		}
	}
}

// Close abandons the process: the agent sees the connection close and kills
// the process group.
func (p *Process) Close() { p.done() }

// Exec runs a command to completion, streaming its output.
func (c *Client) Exec(ctx context.Context, r ExecRequest, onOutput func(s Stream, data []byte)) (*Exit, error) {
	p, err := c.Start(ctx, r)
	if err != nil {
		return nil, err
	}
	return p.Wait(onOutput)
}

// DefaultOutputLimit is Run's cap per stream when limit is 0.
const DefaultOutputLimit = 16 << 20

// Result is the collected outcome of Run.
type Result struct {
	Exit
	Stdout, Stderr       []byte
	StdoutCut, StderrCut bool // output beyond the limit was dropped
}

// Run runs a command to completion and collects up to limit bytes of each
// stream (DefaultOutputLimit when 0). Output past the limit is read and
// dropped, so the process still finishes, and host memory stays bounded.
func (c *Client) Run(ctx context.Context, r ExecRequest, limit int) (*Result, error) {
	if limit <= 0 {
		limit = DefaultOutputLimit
	}
	var out, errb cappedBuffer
	out.limit, errb.limit = limit, limit
	exit, err := c.Exec(ctx, r, func(s Stream, d []byte) {
		if s == Stderr {
			errb.write(d)
		} else {
			out.write(d)
		}
	})
	if err != nil {
		return nil, err
	}
	return &Result{Exit: *exit, Stdout: out.b.Bytes(), Stderr: errb.b.Bytes(), StdoutCut: out.cut, StderrCut: errb.cut}, nil
}

type cappedBuffer struct {
	b     bytes.Buffer
	limit int
	cut   bool
}

func (c *cappedBuffer) write(d []byte) {
	room := c.limit - c.b.Len()
	if len(d) > room {
		d = d[:max(room, 0)]
		c.cut = true
	}
	c.b.Write(d)
}

// Port re-exports the agent's default vsock port for callers of New.
const Port = agentwire.Port
