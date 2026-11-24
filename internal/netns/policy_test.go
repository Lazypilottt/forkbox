package netns

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestParsePolicy(t *testing.T) {
	p, err := ParsePolicy([]string{"PyPI.org", "*.pythonhosted.org", "pypi.org.", "10.1.2.3", "192.0.2.0/24", "192.0.2.7/24"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pypi.org", "pythonhosted.org", "10.1.2.3/32", "192.0.2.0/24"}; !slices.Equal(p.Entries(), want) {
		t.Errorf("entries %v, want %v", p.Entries(), want)
	}
	for name, ok := range map[string]bool{
		"pypi.org": true, "files.pypi.org.": true, "FILES.PYTHONHOSTED.ORG": true, "pythonhosted.org": true,
		"notpypi.org": false, "pypi.org.evil.com": false, "example.com": false, "org": false,
	} {
		if p.AllowsName(name) != ok {
			t.Errorf("AllowsName(%q) = %v", name, !ok)
		}
	}
	if !p.AllowsAddr(netip.MustParseAddr("192.0.2.200")) || p.AllowsAddr(netip.MustParseAddr("192.0.3.1")) {
		t.Error("AllowsAddr")
	}
	for _, bad := range []string{"", "localhost", "no spaces.com", "-bad.com", "::1", "fd00::/8", "1.2.3.4/33", "a..b"} {
		if _, err := ParsePolicy([]string{bad}); !errors.Is(err, ErrPolicy) {
			t.Errorf("%q accepted (%v)", bad, err)
		}
	}
	open, _ := ParsePolicy([]string{"*"})
	if !open.Open || !open.AllowsName("anything.example") || open.Entries()[0] != "*" {
		t.Errorf("open: %+v", open)
	}
	if m := p.Merge(open); !m.Open || !m.AllowsName("pypi.org") {
		t.Errorf("merge: %+v", m)
	}
	if none, _ := ParsePolicy(nil); none.AllowsName("pypi.org") || len(none.Entries()) != 0 {
		t.Error("an empty policy allows something")
	}
}

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func answer(t *testing.T, q []byte, ttl uint32, addrs ...string) []byte {
	t.Helper()
	var p dnsmessage.Parser
	h, _ := p.Start(q)
	qq, _ := p.Question()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true})
	b.StartQuestions()
	b.Question(qq)
	b.StartAnswers()
	b.CNAMEResource(dnsmessage.ResourceHeader{Name: qq.Name, Class: dnsmessage.ClassINET, TTL: ttl},
		dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("edge.cdn.example.")})
	for _, a := range addrs {
		b.AResource(dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("edge.cdn.example."), Class: dnsmessage.ClassINET, TTL: ttl},
			dnsmessage.AResource{A: netip.MustParseAddr(a).As4()})
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func rcode(t *testing.T, resp []byte) (dnsmessage.RCode, int) {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != 0x1234 || !h.Response {
		t.Errorf("header %+v", h)
	}
	p.SkipAllQuestions()
	all, _ := p.AllAnswers()
	return h.RCode, len(all)
}

func TestDNSHandler(t *testing.T) {
	pol, _ := ParsePolicy([]string{"pypi.org"})
	var allowed []netip.Addr
	var allowedTTL time.Duration
	var refused []string
	upstreamCalls := 0
	h := &dnsHandler{
		policy: pol,
		upstream: func(q []byte) ([]byte, error) {
			upstreamCalls++
			return answer(t, q, 30, "151.101.0.223", "151.101.64.223"), nil
		},
		allow: func(a []netip.Addr, ttl time.Duration) error {
			allowed, allowedTTL = a, ttl
			return nil
		},
		refused: func(n string) { refused = append(refused, n) },
	}

	// An allowed name: forwarded, and its addresses allowed before the answer.
	code, n := rcode(t, h.handle(query(t, "files.pypi.org.", dnsmessage.TypeA)))
	if code != dnsmessage.RCodeSuccess || n != 3 {
		t.Errorf("allowed: rcode %v, %d answers", code, n)
	}
	if len(allowed) != 2 || allowed[1].String() != "151.101.64.223" || allowedTTL != MinAllowTTL {
		t.Errorf("allowed %v for %s", allowed, allowedTTL)
	}

	// Another name: NXDOMAIN, never sent upstream.
	upstreamCalls = 0
	code, _ = rcode(t, h.handle(query(t, "evil.example.", dnsmessage.TypeA)))
	if code != dnsmessage.RCodeNameError || upstreamCalls != 0 || !slices.Equal(refused, []string{"evil.example"}) {
		t.Errorf("refused: rcode %v, upstream calls %d, refused %v", code, upstreamCalls, refused)
	}

	// AAAA for an allowed name: empty (the guests have IPv4 only).
	code, n = rcode(t, h.handle(query(t, "pypi.org.", dnsmessage.TypeAAAA)))
	if code != dnsmessage.RCodeSuccess || n != 0 || upstreamCalls != 0 {
		t.Errorf("AAAA: rcode %v, %d answers, upstream %d", code, n, upstreamCalls)
	}

	// Failures: upstream down, allow fails, garbage.
	h.upstream = func([]byte) ([]byte, error) { return nil, errors.New("down") }
	if code, _ := rcode(t, h.handle(query(t, "pypi.org.", dnsmessage.TypeA))); code != dnsmessage.RCodeServerFailure {
		t.Errorf("upstream down: %v", code)
	}
	h.upstream = func(q []byte) ([]byte, error) { return answer(t, q, 7200, "1.2.3.4"), nil }
	h.allow = func([]netip.Addr, time.Duration) error { return errors.New("nft failed") }
	if code, _ := rcode(t, h.handle(query(t, "pypi.org.", dnsmessage.TypeA))); code != dnsmessage.RCodeServerFailure {
		t.Errorf("allow failed: %v (an answer the guest cannot use must not be sent)", code)
	}
	if h.handle([]byte{1, 2, 3}) != nil {
		t.Error("garbage answered")
	}
}

func TestBlockedParsing(t *testing.T) {
	m, _ := manager(t)
	m.Output = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return []byte(`{"nftables": [{"metainfo": {}}, {"set": {"name": "blocked", "elem": [
			{"elem": {"val": {"concat": ["93.184.215.14", "tcp", 443]}, "timeout": 600, "expires": 590}},
			{"concat": ["1.1.1.1", "udp", 53]}]}}]}`), nil
	}
	bl, err := m.Blocked(context.Background(), "sb-1")
	if err != nil || len(bl) != 2 || bl[0].String() != "tcp 93.184.215.14:443" || bl[1].String() != "udp 1.1.1.1:53" {
		t.Errorf("%v %v", bl, err)
	}
	m.Output = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"nftables": [{"set": {"name": "dyn4", "elem": [{"elem": {"val": "151.101.0.223", "timeout": 3600, "expires": 3000}}]}}]}`), nil
	}
	al, err := m.Allowed(context.Background(), "sb-1")
	if err != nil || al[netip.MustParseAddr("151.101.0.223")] != 3000*time.Second {
		t.Errorf("%v %v", al, err)
	}
}
