// Package netns gives each sandbox its own network namespace.
//
// Restored clones keep the guest IP, MAC and tap name from the snapshot
// (docs/snapshotting/network-for-clones.md). So every namespace holds an
// identical tap (TapName, TapAddr, TapMAC) facing the identical guest
// (GuestAddr), plus a veth pair to the host with a unique /30 from the
// address pool. NAT in the namespace rewrites the guest's address to the
// namespace's unique one, so replies find the right clone, and NAT on the
// host takes it out through the uplink.
//
// Everything is done with the ip and nft commands (which forkbox doctor
// already requires): each namespace's ruleset is applied atomically with
// nft -f, and the code stays free of large netlink libraries.
package netns

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The guest side, identical in every namespace and baked into templates.
const (
	TapName    = "fbx-tap0"
	TapMAC     = "06:00:ac:1e:00:01" // fixed, so a clone's ARP cache stays valid
	GuestMAC   = "06:00:ac:1e:00:02"
	GuestIface = "eth0"
	TapIP      = "172.30.0.1" // the guest's gateway and DNS server
	GuestIP    = "172.30.0.2"
	PrefixLen  = 30
	NsVeth     = "fbx-veth" // the namespace end of the veth pair
	// DefaultPool supplies one /30 per sandbox: 16384 slots.
	DefaultPool = "10.200.0.0/16"
	// NetnsDir is where `ip netns` keeps namespace handles.
	NetnsDir = "/run/netns"
	// Prefix starts every network object Forkbox creates.
	Prefix = "fbx-"
)

// Runner runs a command; tests replace it.
type Runner func(ctx context.Context, stdin string, name string, args ...string) error

// Manager creates and removes sandbox namespaces.
type Manager struct {
	Pool netip.Prefix
	// DNS is the upstream resolver the guest's DNS (TapIP:53) is sent to.
	DNS netip.Addr
	// UID and GID own the tap, so the jailed Firecracker can open it.
	UID, GID int
	Run      Runner
}

// New returns a manager for pool (DefaultPool when empty).
func New(pool string, uid, gid int) (*Manager, error) {
	if pool == "" {
		pool = DefaultPool
	}
	p, err := netip.ParsePrefix(pool)
	if err != nil || !p.Addr().Is4() || p.Bits() > 29 {
		return nil, fmt.Errorf("address pool %q: need an IPv4 prefix of /29 or larger", pool)
	}
	return &Manager{Pool: p.Masked(), DNS: UpstreamDNS(), UID: uid, GID: gid, Run: run}, nil
}

// Slots is how many sandboxes the pool can hold.
func (m *Manager) Slots() int { return 1 << (32 - m.Pool.Bits() - 2) }

// Addrs are a slot's two veth addresses: host end and namespace end.
func (m *Manager) Addrs(slot int) (host, ns netip.Addr, err error) {
	if slot < 0 || slot >= m.Slots() {
		return host, ns, fmt.Errorf("slot %d outside the pool %s", slot, m.Pool)
	}
	b := m.Pool.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(slot) * 4
	at := func(off uint32) netip.Addr {
		x := v + off
		return netip.AddrFrom4([4]byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)})
	}
	return at(1), at(2), nil
}

// Name is a sandbox's namespace name.
func Name(id string) string { return Prefix + id }

// Path is the namespace handle the jailer joins (--netns).
func Path(id string) string { return filepath.Join(NetnsDir, Name(id)) }

// HostVeth is the host end of a slot's veth pair (at most 15 bytes).
func HostVeth(slot int) string { return Prefix + "h" + strconv.Itoa(slot) }

// Setup creates the namespace for id. With uplink (slot >= 0) it is
// connected to the host; without (slot < 0, used for template builds) it
// holds only the tap. Setup first removes anything left from an earlier
// attempt, so it is idempotent.
func (m *Manager) Setup(ctx context.Context, id string, slot int) error {
	if err := m.Teardown(ctx, id); err != nil {
		return err
	}
	ns := Name(id)
	ip := func(args ...string) error { return m.Run(ctx, "", "ip", args...) }
	nsip := func(args ...string) error { return ip(append([]string{"-n", ns}, args...)...) }
	steps := []func() error{
		func() error { return ip("netns", "add", ns) },
		func() error { return nsip("link", "set", "lo", "up") },
		func() error {
			return nsip("tuntap", "add", "dev", TapName, "mode", "tap", "user", strconv.Itoa(m.UID), "group", strconv.Itoa(m.GID))
		},
		func() error { return nsip("link", "set", TapName, "address", TapMAC) },
		func() error { return nsip("addr", "add", fmt.Sprintf("%s/%d", TapIP, PrefixLen), "dev", TapName) },
		func() error { return nsip("link", "set", TapName, "up") },
	}
	if slot >= 0 {
		host, nsAddr, err := m.Addrs(slot)
		if err != nil {
			return err
		}
		hv := HostVeth(slot)
		steps = append(steps,
			func() error { return m.deleteLink(ctx, hv) }, // a leftover from a crash
			func() error { return ip("link", "add", hv, "type", "veth", "peer", "name", NsVeth, "netns", ns) },
			func() error { return ip("addr", "add", fmt.Sprintf("%s/%d", host, PrefixLen), "dev", hv) },
			func() error { return ip("link", "set", hv, "up") },
			func() error { return nsip("addr", "add", fmt.Sprintf("%s/%d", nsAddr, PrefixLen), "dev", NsVeth) },
			func() error { return nsip("link", "set", NsVeth, "up") },
			func() error { return nsip("route", "add", "default", "via", host.String()) },
			func() error {
				return m.Run(ctx, "", "ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv4.ip_forward=1")
			},
			func() error { return m.Run(ctx, m.nsRuleset(nsAddr), "ip", "netns", "exec", ns, "nft", "-f", "-") },
		)
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return errors.Join(fmt.Errorf("network for %s: %w", id, err), m.Teardown(context.WithoutCancel(ctx), id))
		}
	}
	return nil
}

// nsRuleset is the namespace's nftables ruleset:
//   - the guest's DNS (to TapIP) goes to the upstream resolver;
//   - TCP from the host to the namespace address goes to the guest (port
//     forwards);
//   - the guest's traffic leaves with the namespace's unique address;
//   - egress allows DNS, HTTP and HTTPS and ping, and never reaches link-local
//     addresses (cloud metadata) or other sandboxes. Deny-by-default
//     allow-lists come in Task 14.
func (m *Manager) nsRuleset(nsAddr netip.Addr) string {
	return fmt.Sprintf(`table ip fbx {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "%[1]s" ip daddr %[2]s udp dport 53 dnat to %[3]s
		iifname "%[1]s" ip daddr %[2]s tcp dport 53 dnat to %[3]s
		iifname "%[4]s" ip daddr %[5]s meta l4proto tcp dnat to %[6]s
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "%[4]s" ip saddr %[2]s/%[7]d masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy drop;
		ct state established,related accept
		iifname "%[4]s" oifname "%[1]s" ct status dnat accept
		iifname "%[1]s" ip daddr { 169.254.0.0/16, %[8]s } drop
		iifname "%[1]s" udp dport 53 accept
		iifname "%[1]s" tcp dport { 53, 80, 443 } accept
		iifname "%[1]s" icmp type echo-request accept
	}
}
`, TapName, TapIP, m.DNS, NsVeth, nsAddr, GuestIP, PrefixLen, m.Pool)
}

// HostRuleset is the host's own table: it masquerades the pool out of the
// uplink and stops sandboxes from reaching each other. It touches nothing
// but the "ip fbx" table.
func (m *Manager) HostRuleset() string {
	return fmt.Sprintf(`table ip fbx
delete table ip fbx
table ip fbx {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[1]s oifname != "%[2]sh*" masquerade
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "%[2]sh*" oifname "%[2]sh*" drop
	}
}
`, m.Pool, Prefix)
}

// EnsureHost installs the host table and checks the host can forward. It
// never changes host settings itself: if forwarding is off it says how to
// turn it on.
func (m *Manager) EnsureHost(ctx context.Context) error {
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(b)) != "1" {
		return errors.New("IPv4 forwarding is off, so sandboxes cannot reach the network: sudo sysctl -w net.ipv4.ip_forward=1 (and set it in /etc/sysctl.d to keep it)")
	}
	return m.Run(ctx, m.HostRuleset(), "nft", "-f", "-")
}

// Teardown removes a sandbox's namespace; its veth pair goes with it. A
// missing namespace is fine.
func (m *Manager) Teardown(ctx context.Context, id string) error {
	if _, err := os.Stat(Path(id)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := m.Run(ctx, "", "ip", "netns", "del", Name(id)); err != nil {
		if _, serr := os.Stat(Path(id)); errors.Is(serr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("removing network namespace %s: %w", Name(id), err)
	}
	return nil
}

func (m *Manager) deleteLink(ctx context.Context, name string) error {
	if _, err := os.Stat(filepath.Join("/sys/class/net", name)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return m.Run(ctx, "", "ip", "link", "del", name)
}

// Namespaces lists the Forkbox namespaces that exist (names without the
// prefix are not ours and are never listed).
func Namespaces() []string {
	ents, _ := os.ReadDir(NetnsDir)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), Prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// UpstreamDNS picks the resolver the guests' DNS goes to: the first
// non-loopback nameserver of systemd-resolved's upstream list, else of
// /etc/resolv.conf, else 1.1.1.1.
func UpstreamDNS() netip.Addr {
	for _, f := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		if a, ok := firstNameserver(f); ok {
			return a
		}
	}
	return netip.MustParseAddr("1.1.1.1")
}

func firstNameserver(path string) (netip.Addr, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return netip.Addr{}, false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "nameserver" {
			if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() && !a.IsLoopback() {
				return a, true
			}
		}
	}
	return netip.Addr{}, false
}

func run(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
