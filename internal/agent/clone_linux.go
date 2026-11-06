package agent

import (
	"bytes"
	"encoding/binary"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type linuxSystem struct{}

func defaultSystem() system { return linuxSystem{} }

func (linuxSystem) Now() time.Time { return time.Now() }

func (linuxSystem) SetClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

func (linuxSystem) SetHostname(name string) error { return unix.Sethostname([]byte(name)) }

// AddEntropy does what random-for-clones.md recommends before user code
// runs in a clone: RNDADDENTROPY (credited), then RNDRESEEDCRNG.
func (linuxSystem) AddEntropy(b []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	words := (len(b) + 3) / 4
	info := make([]byte, 8+words*4)
	binary.NativeEndian.PutUint32(info[0:], uint32(len(b)*8)) // bits
	binary.NativeEndian.PutUint32(info[4:], uint32(len(b)))   // bytes
	copy(info[8:], b)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&info[0]))); e != 0 {
		return e
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDRESEEDCRNG, 0); e != 0 {
		return e
	}
	return nil
}

// WatchVMGenID calls OnNewGeneration for every NEW_VMGENID uevent, which
// the kernel's vmgenid driver sends when Firecracker restores the VM. It
// returns only if the netlink socket cannot be opened.
func (s *Server) WatchVMGenID() error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return err
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1}); err != nil {
		unix.Close(fd)
		return err
	}
	go func() {
		defer unix.Close(fd)
		buf := make([]byte, 64<<10)
		for {
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				if err == unix.EINTR || err == unix.ENOBUFS {
					continue
				}
				s.Log.Printf("uevent: %v", err)
				return
			}
			if isNewVMGenID(buf[:n]) {
				s.OnNewGeneration()
			}
		}
	}()
	return nil
}

func isNewVMGenID(msg []byte) bool {
	for _, f := range bytes.Split(msg, []byte{0}) {
		if bytes.Equal(f, []byte("NEW_VMGENID=1")) {
			return true
		}
	}
	return false
}
