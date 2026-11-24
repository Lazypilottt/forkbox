//go:build !linux

package netns

import (
	"errors"
	"net/netip"
)

type dnsProxy struct{}

func (*dnsProxy) close() {}

func startDNSProxy(string, netip.AddrPort, netip.Addr, *dnsHandler, *EgressStats) (*dnsProxy, error) {
	return nil, errors.New("sandbox networking is Linux-only")
}
