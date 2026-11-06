package agent

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ConfigureNet gives the guest interface its address and default route over
// rtnetlink. Images like python:3.12-slim ship no `ip`, so the agent does it
// itself. The template builder runs this before the base snapshot, so every
// clone starts with the network already up.
func ConfigureNet(n *GuestNet) error {
	ifc, err := net.InterfaceByName(n.Iface)
	if err != nil {
		return err
	}
	pfx, err := netip.ParsePrefix(n.Addr)
	if err != nil || !pfx.Addr().Is4() {
		return fmt.Errorf("guest address %q: want an IPv4 CIDR", n.Addr)
	}
	gw, err := netip.ParseAddr(n.Gateway)
	if err != nil {
		return fmt.Errorf("gateway %q: %w", n.Gateway, err)
	}
	if err := linkUp(n.Iface); err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	a := pfx.Addr().As4()
	addr := struct {
		unix.IfAddrmsg
	}{unix.IfAddrmsg{Family: unix.AF_INET, Prefixlen: uint8(pfx.Bits()), Scope: unix.RT_SCOPE_UNIVERSE, Index: uint32(ifc.Index)}}
	body := append(structBytes(&addr.IfAddrmsg), rtattr(unix.IFA_LOCAL, a[:])...)
	body = append(body, rtattr(unix.IFA_ADDRESS, a[:])...)
	if err := nlRequest(fd, unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body); err != nil {
		return fmt.Errorf("address %s on %s: %w", n.Addr, n.Iface, err)
	}
	g := gw.As4()
	rt := unix.RtMsg{Family: unix.AF_INET, Table: unix.RT_TABLE_MAIN, Protocol: unix.RTPROT_STATIC,
		Scope: unix.RT_SCOPE_UNIVERSE, Type: unix.RTN_UNICAST}
	oif := make([]byte, 4)
	binary.NativeEndian.PutUint32(oif, uint32(ifc.Index))
	body = append(structBytes(&rt), rtattr(unix.RTA_GATEWAY, g[:])...)
	body = append(body, rtattr(unix.RTA_OIF, oif)...)
	if err := nlRequest(fd, unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body); err != nil {
		return fmt.Errorf("default route via %s: %w", n.Gateway, err)
	}
	return nil
}

func structBytes[T any](v *T) []byte {
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(v)), unsafe.Sizeof(*v))...)
}

func rtattr(typ uint16, data []byte) []byte {
	l := unix.SizeofRtAttr + len(data)
	b := make([]byte, (l+3)&^3)
	binary.NativeEndian.PutUint16(b[0:], uint16(l))
	binary.NativeEndian.PutUint16(b[2:], typ)
	copy(b[unix.SizeofRtAttr:], data)
	return b
}

// nlRequest sends one request and waits for its acknowledgement.
func nlRequest(fd int, typ uint16, flags uint16, body []byte) error {
	h := unix.NlMsghdr{Len: uint32(unix.SizeofNlMsghdr + len(body)), Type: typ,
		Flags: unix.NLM_F_REQUEST | unix.NLM_F_ACK | flags, Seq: 1}
	msg := append(structBytes(&h), body...)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return err
		}
		for b := buf[:n]; len(b) >= unix.SizeofNlMsghdr; {
			l := int(binary.NativeEndian.Uint32(b[0:4]))
			typ := binary.NativeEndian.Uint16(b[4:6])
			if l < unix.SizeofNlMsghdr || l > len(b) {
				return fmt.Errorf("netlink: malformed reply")
			}
			if typ == unix.NLMSG_ERROR && l >= unix.SizeofNlMsghdr+4 {
				if code := int32(binary.NativeEndian.Uint32(b[unix.SizeofNlMsghdr:])); code != 0 {
					return unix.Errno(-code)
				}
				return nil
			}
			next := (l + 3) &^ 3
			if next > len(b) {
				break
			}
			b = b[next:]
		}
	}
}
