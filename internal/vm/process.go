package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// process is a running jailer, which execs into Firecracker in place (no
// --new-pid-ns, no --daemonize), so its pid is Firecracker's pid.
type process interface {
	Pid() int
	Kill() error
	// Done is closed once the process has exited and been reaped.
	Done() <-chan struct{}
}

// launcher starts the jailer with argv[0] as the binary. Its stdout and
// stderr (the guest serial console and Firecracker's log) go to console.
type launcher func(ctx context.Context, argv []string, console *os.File) (process, error)

func execLauncher(_ context.Context, argv []string, console *os.File) (process, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = nil // /dev/null
	cmd.Stdout = console
	cmd.Stderr = console
	// Own process group: a Ctrl-C aimed at the daemon's terminal must not
	// reach the VMs. Cleanup kills through the cgroup anyway.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

type execProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *execProcess) Pid() int { return p.cmd.Process.Pid }

func (p *execProcess) Kill() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	return p.cmd.Process.Kill()
}

func (p *execProcess) Done() <-chan struct{} { return p.done }

// pidProcess is a Firecracker this process did not start (adopted after a
// daemon restart). It cannot be waited for, so Done polls liveness.
type pidProcess struct {
	pid  int
	done chan struct{}
}

func newPidProcess(pid int, alive func() bool) *pidProcess {
	p := &pidProcess{pid: pid, done: make(chan struct{})}
	go func() {
		for alive() {
			time.Sleep(20 * time.Millisecond)
		}
		close(p.done)
	}()
	return p
}

func (p *pidProcess) Pid() int { return p.pid }

func (p *pidProcess) Kill() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	err := syscall.Kill(p.pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (p *pidProcess) Done() <-chan struct{} { return p.done }
