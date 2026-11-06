package vm

import (
	"context"
	"os"
	"os/exec"
	"syscall"
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
