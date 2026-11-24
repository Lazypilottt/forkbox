package netns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	upstreamTimeout = 4 * time.Second
	maxInflight     = 64 // queries handled at once, per sandbox
)

// dnsProxy is one sandbox's DNS server: UDP and TCP sockets bound to
// TapIP:53 inside the sandbox's namespace, served by goroutines of this
// process. Queries go upstream from the host's namespace.
type dnsProxy struct {
	udp  net.PacketConn
	tcp  net.Listener
	sem  chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func startDNSProxy(nsPath string, addr netip.AddrPort, upstream netip.Addr, h *dnsHandler, st *EgressStats) (*dnsProxy, error) {
	p := &dnsProxy{sem: make(chan struct{}, maxInflight)}
	err := inNetns(nsPath, func() error {
		var err error
		if p.udp, err = net.ListenPacket("udp4", addr.String()); err != nil {
			return err
		}
		if p.tcp, err = net.Listen("tcp4", addr.String()); err != nil {
			p.udp.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	up := netip.AddrPortFrom(upstream, 53).String()
	udpH := *h
	udpH.upstream = func(q []byte) ([]byte, error) { return exchangeUDP(up, q) }
	tcpH := *h
	tcpH.upstream = func(q []byte) ([]byte, error) { return exchangeTCP(up, q) }
	count := func(hh *dnsHandler) *dnsHandler {
		inner := hh.allow
		hh.allow = func(a []netip.Addr, ttl time.Duration) error {
			st.DNSAllowed.Add(1)
			return inner(a, ttl)
		}
		return hh
	}
	p.wg.Add(2)
	go p.serveUDP(count(&udpH))
	go p.serveTCP(count(&tcpH))
	return p, nil
}

func (p *dnsProxy) close() {
	p.once.Do(func() {
		p.udp.Close()
		p.tcp.Close()
	})
	p.wg.Wait()
}

func (p *dnsProxy) serveUDP(h *dnsHandler) {
	defer p.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, from, err := p.udp.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		q := append([]byte(nil), buf[:n]...)
		select {
		case p.sem <- struct{}{}:
		default:
			continue // overloaded: the guest's resolver retries
		}
		go func() {
			defer func() { <-p.sem }()
			if r := h.handle(q); r != nil {
				_, _ = p.udp.WriteTo(r, from)
			}
		}()
	}
}

func (p *dnsProxy) serveTCP(h *dnsHandler) {
	defer p.wg.Done()
	for {
		c, err := p.tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		select {
		case p.sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-p.sem }()
			defer c.Close()
			for {
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				q, err := readTCPMsg(c)
				if err != nil {
					return
				}
				r := h.handle(q)
				if r == nil || writeTCPMsg(c, r) != nil {
					return
				}
			}
		}()
	}
}

func exchangeUDP(server string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout("udp", server, upstreamTimeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(upstreamTimeout))
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		// Only the answer to this query (same id).
		if n >= 2 && len(q) >= 2 && buf[0] == q[0] && buf[1] == q[1] {
			return append([]byte(nil), buf[:n]...), nil
		}
	}
}

func exchangeTCP(server string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout("tcp", server, upstreamTimeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(upstreamTimeout))
	if err := writeTCPMsg(c, q); err != nil {
		return nil, err
	}
	return readTCPMsg(c)
}

func readTCPMsg(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeTCPMsg(w io.Writer, b []byte) error {
	if len(b) > 65535 {
		return errors.New("dns message too large")
	}
	out := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(out, uint16(len(b)))
	copy(out[2:], b)
	_, err := w.Write(out)
	return err
}

// inNetns runs f on an OS thread switched into the network namespace at
// nsPath; sockets f creates stay in that namespace. The thread is thrown
// away if it cannot switch back.
func inNetns(nsPath string, f func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			errc <- err
			return
		}
		defer orig.Close()
		target, err := os.Open(nsPath)
		if err != nil {
			runtime.UnlockOSThread()
			errc <- err
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("entering %s: %w", nsPath, err)
			return
		}
		ferr := f()
		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err != nil {
			// Stay locked: the goroutine exits and the runtime ends the thread.
			errc <- errors.Join(ferr, fmt.Errorf("leaving %s: %w", nsPath, err))
			return
		}
		runtime.UnlockOSThread()
		errc <- ferr
	}()
	return <-errc
}
