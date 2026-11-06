package agent

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func portListening(port uint16) bool {
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(p); err == nil && listeningIn(string(b), port) {
			return true
		}
	}
	return false
}

func uptimeMs() uint64 {
	f := strings.Fields(readTrim("/proc/uptime"))
	if len(f) == 0 {
		return 0
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return uint64(sec * 1000)
}

// ListenVsock listens on an AF_VSOCK port for any CID. The net package does
// not know AF_VSOCK, so the sockets are non-blocking os.Files, which the Go
// runtime poller drives (accept, read, write and deadlines all work).
func ListenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w (is /dev/vsock present?)", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port))
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &vsockListener{f: f, rc: rc, addr: vsockAddr{cid: unix.VMADDR_CID_ANY, port: port}}, nil
}

type vsockListener struct {
	f    *os.File
	rc   syscall.RawConn
	addr vsockAddr
}

func (l *vsockListener) Accept() (net.Conn, error) {
	var nfd int
	var sa unix.Sockaddr
	var aerr error
	err := l.rc.Read(func(fd uintptr) bool {
		nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		return aerr != unix.EAGAIN
	})
	if err != nil {
		return nil, err // listener closed
	}
	if aerr != nil {
		return nil, aerr
	}
	remote := vsockAddr{}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		remote = vsockAddr{cid: vm.CID, port: vm.Port}
	}
	return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock-conn"), local: l.addr, remote: remote}, nil
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }

type vsockConn struct {
	*os.File
	local, remote vsockAddr
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

type vsockAddr struct{ cid, port uint32 }

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }
