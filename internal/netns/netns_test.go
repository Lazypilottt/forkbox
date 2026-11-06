package netns

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorder struct {
	cmds   []string
	stdins []string
}

func (r *recorder) run(_ context.Context, stdin, name string, args ...string) error {
	r.cmds = append(r.cmds, name+" "+strings.Join(args, " "))
	r.stdins = append(r.stdins, stdin)
	return nil
}

func manager(t *testing.T) (*Manager, *recorder) {
	t.Helper()
	m, err := New("", 70000, 70000)
	if err != nil {
		t.Fatal(err)
	}
	m.DNS = netip.MustParseAddr("168.63.129.16")
	r := &recorder{}
	m.Run = r.run
	return m, r
}

func TestAddrs(t *testing.T) {
	m, _ := manager(t)
	if m.Slots() != 16384 {
		t.Errorf("slots %d", m.Slots())
	}
	for slot, want := range map[int][2]string{0: {"10.200.0.1", "10.200.0.2"}, 1: {"10.200.0.5", "10.200.0.6"}, 64: {"10.200.1.1", "10.200.1.2"}, 16383: {"10.200.255.253", "10.200.255.254"}} {
		h, n, err := m.Addrs(slot)
		if err != nil || h.String() != want[0] || n.String() != want[1] {
			t.Errorf("slot %d: %s %s %v", slot, h, n, err)
		}
	}
	if _, _, err := m.Addrs(16384); err == nil {
		t.Error("slot past the pool accepted")
	}
	if len(HostVeth(16383)) > 15 {
		t.Errorf("%s is longer than IFNAMSIZ-1", HostVeth(16383))
	}
	for _, bad := range []string{"10.200.0.0/30", "fd00::/64", "nonsense"} {
		if _, err := New(bad, 1, 1); err == nil {
			t.Errorf("pool %q accepted", bad)
		}
	}
}

func TestSetupCommands(t *testing.T) {
	m, r := manager(t)
	if err := m.Setup(context.Background(), "sb-1", 3); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ip netns add fbx-sb-1",
		"ip -n fbx-sb-1 link set lo up",
		"ip -n fbx-sb-1 tuntap add dev fbx-tap0 mode tap user 70000 group 70000",
		"ip -n fbx-sb-1 link set fbx-tap0 address 06:00:ac:1e:00:01",
		"ip -n fbx-sb-1 addr add 172.30.0.1/30 dev fbx-tap0",
		"ip -n fbx-sb-1 link set fbx-tap0 up",
		"ip link add fbx-h3 type veth peer name fbx-veth netns fbx-sb-1",
		"ip addr add 10.200.0.13/30 dev fbx-h3",
		"ip link set fbx-h3 up",
		"ip -n fbx-sb-1 addr add 10.200.0.14/30 dev fbx-veth",
		"ip -n fbx-sb-1 link set fbx-veth up",
		"ip -n fbx-sb-1 route add default via 10.200.0.13",
		"ip netns exec fbx-sb-1 sysctl -qw net.ipv4.ip_forward=1",
		"ip netns exec fbx-sb-1 nft -f -",
	}
	if strings.Join(r.cmds, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(r.cmds, "\n"), strings.Join(want, "\n"))
	}
	rules := r.stdins[len(r.stdins)-1]
	for _, frag := range []string{
		`iifname "fbx-tap0" ip daddr 172.30.0.1 udp dport 53 dnat to 168.63.129.16`,
		`iifname "fbx-veth" ip daddr 10.200.0.14 meta l4proto tcp dnat to 172.30.0.2`,
		`oifname "fbx-veth" ip saddr 172.30.0.1/30 masquerade`,
		`policy drop;`,
		`ip daddr { 169.254.0.0/16, 10.200.0.0/16 } drop`,
		`tcp dport { 53, 80, 443 } accept`,
	} {
		if !strings.Contains(rules, frag) {
			t.Errorf("namespace rules lack %q:\n%s", frag, rules)
		}
	}

	// A build namespace holds only the tap.
	r.cmds = nil
	m.Setup(context.Background(), "build-x", -1)
	if len(r.cmds) != 6 || strings.Contains(strings.Join(r.cmds, " "), "veth") {
		t.Errorf("tap-only setup: %v", r.cmds)
	}
}

func TestHostRuleset(t *testing.T) {
	m, _ := manager(t)
	rs := m.HostRuleset()
	for _, frag := range []string{"table ip fbx\ndelete table ip fbx\n", `ip saddr 10.200.0.0/16 oifname != "fbx-h*" masquerade`, `iifname "fbx-h*" oifname "fbx-h*" drop`} {
		if !strings.Contains(rs, frag) {
			t.Errorf("host rules lack %q:\n%s", frag, rs)
		}
	}
	if strings.Contains(rs, "table ip filter") || strings.Contains(rs, "table inet filter") {
		t.Error("host rules touch tables that are not Forkbox's")
	}
}

func TestFirstNameserver(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "resolv.conf")
	os.WriteFile(p, []byte("# stub\nnameserver 127.0.0.53\nsearch x\nnameserver ::1\nnameserver 168.63.129.16\nnameserver 8.8.8.8\n"), 0o644)
	if a, ok := firstNameserver(p); !ok || a.String() != "168.63.129.16" {
		t.Errorf("got %v %v", a, ok)
	}
	os.WriteFile(p, []byte("nameserver 127.0.0.53\n"), 0o644)
	if _, ok := firstNameserver(p); ok {
		t.Error("loopback resolver picked")
	}
}
