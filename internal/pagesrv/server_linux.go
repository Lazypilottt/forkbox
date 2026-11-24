package pagesrv

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Options configure one page server process (one per VM).
type Options struct {
	Socket   string   // host path of the jail's uffd socket
	Layers   []string // memory files, newest first; the last is full
	UID, GID int      // Firecracker's ids, which must reach the socket
	Prefetch bool     // copy the newest layer's pages before the first fault
	Stats    string   // JSON stats file, rewritten every second and at exit
	Cgroup   string   // the VM's cgroup: join it once it exists
	// AcceptTimeout bounds the wait for Firecracker (default 60 s).
	AcceptTimeout time.Duration
	// Ready is called once the socket listens (the caller can then start
	// the snapshot load).
	Ready func()
}

// StatsFile is what the server writes to Options.Stats.
type StatsFile struct {
	Faults       uint64  `json:"faults"`
	Copied       uint64  `json:"copied"`
	Zeroed       uint64  `json:"zeroed"`
	Prefetched   uint64  `json:"prefetched"`
	Removed      uint64  `json:"removed"`
	Retries      uint64  `json:"retries"`
	FaultP50us   float64 `json:"fault_p50_us"`
	FaultP99us   float64 `json:"fault_p99_us"`
	Layers       int     `json:"layers"`
	PrefetchMs   float64 `json:"prefetch_ms"`
	FirecrackerP int     `json:"firecracker_pid"`
	Exit         string  `json:"exit,omitempty"`
}

// Run serves one VM: it listens, waits for Firecracker's handshake, and
// serves faults until Firecracker exits. On a fatal error it kills
// Firecracker, which would otherwise block on its next fault forever.
func Run(o Options) (err error) {
	chain, err := NewChain(o.Layers)
	if err != nil {
		return err
	}
	defer chain.Close()
	os.Remove(o.Socket)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: o.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer ln.Close()
	// Only Firecracker (the jail's uid) may connect.
	if err := os.Chown(o.Socket, o.UID, o.GID); err != nil {
		return err
	}
	if err := os.Chmod(o.Socket, 0o600); err != nil {
		return err
	}
	if o.Ready != nil {
		o.Ready()
	}
	at := o.AcceptTimeout
	if at <= 0 {
		at = 60 * time.Second
	}
	ln.SetDeadline(time.Now().Add(at))
	conn, err := ln.AcceptUnix()
	if err != nil {
		return fmt.Errorf("pagesrv: waiting for Firecracker: %w", err)
	}
	defer conn.Close()
	ln.Close()
	os.Remove(o.Socket)

	cred, uffd, regions, err := handshake(conn)
	if err != nil {
		return err
	}
	fcPid := int(cred.Pid)
	defer func() {
		if err != nil {
			// Fail the VM rather than leave it hanging on a page that never comes.
			unix.Kill(fcPid, unix.SIGKILL)
		}
	}()
	if o.Cgroup != "" {
		_ = os.WriteFile(filepath.Join(o.Cgroup, "cgroup.procs"), []byte(fmt.Sprint(os.Getpid())), 0o644)
	}
	h, err := NewHandler(uffd, regions, chain)
	if err != nil {
		return err
	}
	st := &StatsFile{Layers: len(chain.Layers), FirecrackerP: fcPid}
	write := func() {
		if o.Stats == "" {
			return
		}
		st.Faults, st.Copied, st.Zeroed = h.Stats.Faults.Load(), h.Stats.Copied.Load(), h.Stats.Zeroed.Load()
		st.Prefetched, st.Removed, st.Retries = h.Stats.Prefetched.Load(), h.Stats.Removed.Load(), h.Stats.Retries.Load()
		lat := h.Stats.FaultLatencies()
		if len(lat) > 0 {
			st.FaultP50us = float64(lat[len(lat)/2].Nanoseconds()) / 1000
			st.FaultP99us = float64(lat[min(len(lat)-1, len(lat)*99/100)].Nanoseconds()) / 1000
		}
		b, _ := json.Marshal(st)
		tmp := o.Stats + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, o.Stats)
		}
	}
	stop := make(chan struct{})
	served := make(chan error, 1)
	// Faults are served from here on; prefetch runs alongside (EEXIST makes
	// races with faults harmless).
	go func() { served <- h.Serve(stop) }()
	if o.Prefetch {
		t := time.Now()
		if err := h.Prefetch(); err != nil {
			close(stop)
			return err
		}
		st.PrefetchMs = float64(time.Since(t).Microseconds()) / 1000
	}
	exited, err := pidExited(fcPid)
	if err != nil {
		close(stop)
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-served:
			st.Exit = fmt.Sprint(err)
			write()
			return err
		case <-exited:
			close(stop)
			<-served
			st.Exit = "firecracker exited"
			write()
			return nil
		case <-tick.C:
			write()
		}
	}
}

// handshake receives Firecracker's regions and userfaultfd.
func handshake(conn *net.UnixConn) (*unix.Ucred, int, []Region, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, -1, nil, err
	}
	var cred *unix.Ucred
	var cerr error
	raw.Control(func(fd uintptr) { cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if cerr != nil {
		return nil, -1, nil, cerr
	}
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	buf := make([]byte, 64<<10)
	oob := make([]byte, unix.CmsgSpace(4*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, -1, nil, fmt.Errorf("pagesrv: handshake: %w", err)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, -1, nil, err
	}
	uffd := -1
	for _, m := range msgs {
		if fds, err := unix.ParseUnixRights(&m); err == nil && len(fds) > 0 {
			uffd = fds[0]
		}
	}
	if uffd < 0 {
		return nil, -1, nil, errors.New("pagesrv: handshake carried no userfaultfd")
	}
	var regions []Region
	if err := json.Unmarshal(buf[:n], &regions); err != nil {
		return nil, -1, nil, fmt.Errorf("pagesrv: handshake: %w (%q)", err, buf[:min(n, 200)])
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].BaseHostVirtAddr < regions[j].BaseHostVirtAddr })
	return cred, uffd, regions, nil
}

// pidExited closes the returned channel when pid exits (pidfd).
func pidExited(pid int) (<-chan struct{}, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("pagesrv: pidfd for %d: %w", pid, err)
	}
	ch := make(chan struct{})
	go func() {
		defer unix.Close(fd)
		defer close(ch)
		for {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(fds, -1); err != nil && errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
	}()
	return ch, nil
}
