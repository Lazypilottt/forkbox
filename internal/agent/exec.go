package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/proto/agentpb"
)

// defaultEnv is the environment of every Exec before the request's own.
var defaultEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"LANG=C.UTF-8",
	"TERM=dumb",
}

const (
	// pendingFrames bounds output waiting for the connection. When it is
	// full, the readers stop reading, the pipes fill and the process blocks
	// on write: a noisy process slows down instead of growing memory.
	pendingFrames = 8
	// pipeGrace is how long output may still arrive after the process
	// exits, from children that inherited its stdout or stderr.
	pipeGrace = 250 * time.Millisecond
)

func (s *Server) exec(c net.Conn, r *agentpb.ExecRequest) error {
	if len(r.Args) == 0 {
		return agentwire.Write(c, errResp(agentpb.Error_INVALID_ARGUMENT, "exec: no command"))
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	if r.TimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(r.TimeoutMs)*time.Millisecond)
		defer cancel()
	}

	cmd := exec.Command(r.Args[0], r.Args[1:]...)
	cmd.Env = append(append([]string(nil), defaultEnv...), r.Env...)
	cmd.Dir = r.Cwd
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	// Its own process group, so timeouts and signals reach its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outR, outW, err := os.Pipe()
	if err != nil {
		return writeErr(c, err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return writeErr(c, err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	start := time.Now()
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		if errors.Is(err, exec.ErrNotFound) {
			return agentwire.Write(c, errResp(agentpb.Error_NOT_FOUND, err.Error()))
		}
		return writeErr(c, err)
	}
	pgid := cmd.Process.Pid
	id := newID()
	s.mu.Lock()
	s.execs[id] = pgid
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.execs, id)
		s.mu.Unlock()
	}()

	// Kill the group on timeout, on server shutdown, or when the host goes
	// away (nobody is left to read the output).
	exited := make(chan struct{})
	var killedByUs bool
	var killMu sync.Mutex
	go func() {
		select {
		case <-ctx.Done():
			killMu.Lock()
			killedByUs = true
			killMu.Unlock()
			_ = killGroup(pgid, int(syscall.SIGKILL))
		case <-exited:
		}
	}()
	go func() {
		// The host sends nothing after the request; a read returning means
		// it closed the connection.
		_, _ = io.Copy(io.Discard, c)
		cancel()
	}()

	frames := make(chan *agentpb.Response, pendingFrames)
	writerDone := make(chan error, 1)
	go func() {
		var werr error
		for f := range frames {
			if werr != nil {
				continue // keep draining so the readers never block forever
			}
			if werr = agentwire.Write(c, f); werr != nil {
				cancel()
			}
		}
		writerDone <- werr
	}()
	frames <- &agentpb.Response{Msg: &agentpb.Response_ExecStarted{ExecStarted: &agentpb.ExecStarted{ExecId: id, Pid: int32(pgid)}}}

	var readers sync.WaitGroup
	for _, p := range []struct {
		f      *os.File
		stream agentpb.Output_Stream
	}{{outR, agentpb.Output_STDOUT}, {errR, agentpb.Output_STDERR}} {
		readers.Add(1)
		go func() {
			defer readers.Done()
			buf := make([]byte, agentwire.ChunkSize)
			for {
				n, err := p.f.Read(buf)
				if n > 0 {
					data := append([]byte(nil), buf[:n]...)
					frames <- &agentpb.Response{Msg: &agentpb.Response_Output{Output: &agentpb.Output{Stream: p.stream, Data: data}}}
				}
				if err != nil {
					return
				}
			}
		}()
	}

	waitErr := cmd.Wait()
	close(exited)
	readersDone := make(chan struct{})
	go func() { readers.Wait(); close(readersDone) }()
	select {
	case <-readersDone:
	case <-time.After(pipeGrace):
		outR.Close() // a background child still holds the pipe
		errR.Close()
		<-readersDone
	}
	outR.Close()
	errR.Close()

	ex := &agentpb.ExecExit{DurationMs: uint64(time.Since(start).Milliseconds())}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		ex.ExitCode = -1
		ex.Signal = ws.Signal().String()
	} else {
		ex.ExitCode = int32(cmd.ProcessState.ExitCode())
	}
	var ee *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &ee) {
		s.Log.Printf("exec %s: wait: %v", id, waitErr)
	}
	killMu.Lock()
	ex.TimedOut = killedByUs && errors.Is(ctx.Err(), context.DeadlineExceeded)
	killMu.Unlock()
	frames <- &agentpb.Response{Msg: &agentpb.Response_ExecExit{ExecExit: ex}}
	close(frames)
	return <-writerDone
}

func killGroup(pgid, sig int) error {
	err := syscall.Kill(-pgid, syscall.Signal(sig))
	if errors.Is(err, syscall.ESRCH) {
		return nil // already gone
	}
	return err
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
