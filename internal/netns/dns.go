package netns

import (
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsHandler answers one sandbox's DNS queries (sent to TapIP:53). Names
// the policy allows are forwarded to the upstream resolver, and the IPv4
// addresses in the answer are allowed in the namespace (allow) before the
// guest sees them. Other names get NXDOMAIN. Guests have IPv4 only, so
// AAAA queries for allowed names get an empty answer.
type dnsHandler struct {
	policy   Policy
	upstream func(q []byte) ([]byte, error)
	allow    func(addrs []netip.Addr, ttl time.Duration) error
	refused  func(name string) // a name the policy does not allow
}

// MinAllowTTL is the shortest time an address an allowed name resolved to
// stays reachable; connections already made are kept by conntrack anyway.
const MinAllowTTL = time.Hour

func (h *dnsHandler) handle(q []byte) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(q)
	if err != nil {
		return nil // not DNS: drop
	}
	question, err := p.Question()
	if err != nil {
		return reply(hdr, nil, dnsmessage.RCodeFormatError)
	}
	name := strings.TrimSuffix(strings.ToLower(question.Name.String()), ".")
	if !h.policy.AllowsName(name) {
		if h.refused != nil {
			h.refused(name)
		}
		return reply(hdr, &question, dnsmessage.RCodeNameError)
	}
	if question.Type == dnsmessage.TypeAAAA {
		return reply(hdr, &question, dnsmessage.RCodeSuccess)
	}
	resp, err := h.upstream(q)
	if err != nil {
		return reply(hdr, &question, dnsmessage.RCodeServerFailure)
	}
	if h.policy.Open {
		return resp
	}
	addrs, ttl, err := answerAddrs(resp)
	if err != nil {
		return reply(hdr, &question, dnsmessage.RCodeServerFailure)
	}
	if len(addrs) > 0 {
		if err := h.allow(addrs, max(ttl, MinAllowTTL)); err != nil {
			return reply(hdr, &question, dnsmessage.RCodeServerFailure)
		}
	}
	return resp
}

// answerAddrs returns the A records of a response and their longest TTL.
func answerAddrs(resp []byte) ([]netip.Addr, time.Duration, error) {
	var p dnsmessage.Parser
	if _, err := p.Start(resp); err != nil {
		return nil, 0, err
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, 0, err
	}
	var out []netip.Addr
	var ttl uint32
	for {
		h, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if h.Type != dnsmessage.TypeA {
			if err := p.SkipAnswer(); err != nil {
				return nil, 0, err
			}
			continue
		}
		a, err := p.AResource()
		if err != nil {
			return nil, 0, err
		}
		out = append(out, netip.AddrFrom4(a.A))
		ttl = max(ttl, h.TTL)
	}
	return out, time.Duration(ttl) * time.Second, nil
}

// reply builds an answer-less response to a query.
func reply(q dnsmessage.Header, question *dnsmessage.Question, rcode dnsmessage.RCode) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: q.ID, Response: true, OpCode: q.OpCode, RecursionDesired: q.RecursionDesired,
		RecursionAvailable: true, RCode: rcode,
	})
	b.EnableCompression()
	if question != nil {
		if err := b.StartQuestions(); err != nil {
			return nil
		}
		if err := b.Question(*question); err != nil {
			return nil
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}
