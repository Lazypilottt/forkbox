package agent

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RunInit is the agent's PID 1 mode, for images without an init system
// (most OCI images). It mounts the pseudo filesystems, brings up loopback,
// runs the agent server as a child (serveArgs) and reaps every orphan.
// The server is restarted if it dies. RunInit never returns.
func RunInit(logger *log.Logger, serveArgs []string) {
	for _, m := range []struct {
		src, dst, fstype, data string
		flags                  uintptr
	}{
		{"proc", "/proc", "proc", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"sysfs", "/sys", "sysfs", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755", unix.MS_NOSUID},
		{"devpts", "/dev/pts", "devpts", "mode=0620,ptmxmode=0666", unix.MS_NOSUID | unix.MS_NOEXEC},
		{"tmpfs", "/dev/shm", "tmpfs", "mode=1777", unix.MS_NOSUID | unix.MS_NODEV},
		{"tmpfs", "/run", "tmpfs", "mode=0755", unix.MS_NOSUID | unix.MS_NODEV},
	} {
		_ = os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.fstype, m.flags, m.data); err != nil && !errors.Is(err, unix.EBUSY) {
			logger.Printf("init: mount %s: %v", m.dst, err)
		}
	}
	for link, target := range map[string]string{
		"/dev/fd": "/proc/self/fd", "/dev/stdin": "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1", "/dev/stderr": "/proc/self/fd/2",
	} {
		_ = os.Symlink(target, link)
	}
	if name := readTrim("/etc/hostname"); name != "" {
		_ = unix.Sethostname([]byte(name))
	}
	if err := linkUp("lo"); err != nil {
		logger.Printf("init: loopback: %v", err)
	}
	if gc, err := LoadGuestConfig(GuestConfigPath); err == nil && gc.Net != nil {
		if err := ConfigureNet(gc.Net); err != nil {
			logger.Printf("init: network: %v", err)
		} else {
			logger.Printf("init: %s %s via %s", gc.Net.Iface, gc.Net.Addr, gc.Net.Gateway)
		}
	}

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	server := startServer(logger, serveArgs)
	for s := range sigs {
		if s != syscall.SIGCHLD {
			continue // PID 1 outlives everything; the host stops the VM
		}
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			if server != nil && pid == server.Process.Pid {
				logger.Printf("init: agent server exited (%v); restarting", describe(ws))
				time.Sleep(100 * time.Millisecond)
				server = startServer(logger, serveArgs)
			}
		}
	}
}

func startServer(logger *log.Logger, args []string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = []string{"PATH=" + strings.TrimPrefix(defaultEnv[0], "PATH=")}
	if err := cmd.Start(); err != nil {
		logger.Printf("init: starting agent server: %v", err)
		return nil
	}
	// PID 1 reaps it with Wait4 above; os/exec's Wait is never called.
	return cmd
}

func describe(ws unix.WaitStatus) string {
	if ws.Signaled() {
		return "signal " + ws.Signal().String()
	}
	return fmt.Sprintf("exit status %d", ws.ExitStatus())
}

// linkUp sets IFF_UP on an interface.
func linkUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
