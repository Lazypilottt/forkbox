package netns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EgressStats counts egress decisions since the daemon started.
type EgressStats struct {
	DNSAllowed, DNSRefused atomic.Uint64
}

// Stats returns the egress counters.
func (m *Manager) Stats() (dnsAllowed, dnsRefused uint64) {
	return m.stats.DNSAllowed.Load(), m.stats.DNSRefused.Load()
}

func (m *Manager) logf(format string, a ...any) {
	if m.Logf != nil {
		m.Logf(format, a...)
	}
}

// allowAddrs lets the guest of id reach addrs for ttl.
func (m *Manager) allowAddrs(ctx context.Context, id string, addrs []netip.Addr, ttl time.Duration) error {
	var elems []string
	for _, a := range addrs {
		if a.Is4() {
			elems = append(elems, fmt.Sprintf("%s timeout %ds", a, int(ttl.Seconds())))
		}
	}
	if len(elems) == 0 {
		return nil
	}
	return m.Run(ctx, "", "ip", "netns", "exec", Name(id), "nft", "add", "element", "ip", "fbx", "dyn4",
		"{ "+strings.Join(elems, ", ")+" }")
}

// nftSet is the part of `nft -j list set` output read here.
type nftSet struct {
	Nftables []struct {
		Set *struct {
			Elem []json.RawMessage `json:"elem"`
		} `json:"set"`
	} `json:"nftables"`
}

func (m *Manager) listSet(ctx context.Context, id, set string) ([]json.RawMessage, error) {
	out, err := m.Output(ctx, "ip", "netns", "exec", Name(id), "nft", "-j", "list", "set", "ip", "fbx", set)
	if err != nil {
		return nil, err
	}
	var s nftSet
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("nft list set %s: %w", set, err)
	}
	for _, o := range s.Nftables {
		if o.Set != nil {
			return o.Set.Elem, nil
		}
	}
	return nil, nil
}

// Allowed lists the addresses allowed names resolved to in id's namespace,
// with the time each stays allowed.
func (m *Manager) Allowed(ctx context.Context, id string) (map[netip.Addr]time.Duration, error) {
	elems, err := m.listSet(ctx, id, "dyn4")
	if err != nil {
		return nil, err
	}
	out := map[netip.Addr]time.Duration{}
	for _, raw := range elems {
		var e struct {
			Elem struct {
				Val     string `json:"val"`
				Expires int    `json:"expires"`
			} `json:"elem"`
		}
		var val string
		if json.Unmarshal(raw, &e) == nil && e.Elem.Val != "" {
			val = e.Elem.Val
		} else if json.Unmarshal(raw, &val) != nil {
			continue
		}
		if a, err := netip.ParseAddr(val); err == nil {
			out[a] = time.Duration(e.Elem.Expires) * time.Second
		}
	}
	return out, nil
}

// CopyAllowed gives the namespace of to the addresses allowed in from: a
// fork's children keep reaching what the parent's processes resolved
// before the fork.
func (m *Manager) CopyAllowed(ctx context.Context, from, to string) error {
	al, err := m.Allowed(ctx, from)
	if err != nil || len(al) == 0 {
		return err
	}
	byTTL := map[time.Duration][]netip.Addr{}
	for a, ttl := range al {
		ttl = max(ttl, time.Minute)
		byTTL[ttl] = append(byTTL[ttl], a)
	}
	for ttl, addrs := range byTTL {
		if err := m.allowAddrs(ctx, to, addrs, ttl); err != nil {
			return err
		}
	}
	return nil
}

// Blocked is a destination the guest tried to reach and was refused.
type Blocked struct {
	Addr  netip.Addr
	Proto string // tcp or udp
	Port  int
}

func (b Blocked) String() string { return fmt.Sprintf("%s %s:%d", b.Proto, b.Addr, b.Port) }

// Blocked lists the destinations refused in id's namespace in the last ten
// minutes.
func (m *Manager) Blocked(ctx context.Context, id string) ([]Blocked, error) {
	elems, err := m.listSet(ctx, id, "blocked")
	if err != nil {
		return nil, err
	}
	var out []Blocked
	for _, raw := range elems {
		// {"elem": {"val": {"concat": ["1.2.3.4", "tcp", 443]}, ...}} or
		// {"concat": [...]} without a timeout.
		var e struct {
			Elem struct {
				Val struct {
					Concat []any `json:"concat"`
				} `json:"val"`
			} `json:"elem"`
			Concat []any `json:"concat"`
		}
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		c := e.Elem.Val.Concat
		if c == nil {
			c = e.Concat
		}
		if len(c) != 3 {
			continue
		}
		s, _ := c[0].(string)
		a, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		proto, _ := c[1].(string)
		port, _ := c[2].(float64)
		out = append(out, Blocked{a, proto, int(port)})
	}
	return out, nil
}

// StopDNS stops id's DNS proxy, if it runs.
func (m *Manager) StopDNS(id string) {
	m.mu.Lock()
	p := m.proxies[id]
	delete(m.proxies, id)
	m.mu.Unlock()
	if p != nil {
		p.close()
	}
}

// StartDNS serves DNS on TapIP:53 in id's namespace under policy, replacing
// a proxy already running for id. The proxy lives in this process: a
// restarted daemon starts it again for the sandboxes it adopts.
func (m *Manager) StartDNS(id string, policy Policy) error {
	m.StopDNS(id)
	h := &dnsHandler{
		policy: policy,
		allow: func(addrs []netip.Addr, ttl time.Duration) error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return m.allowAddrs(ctx, id, addrs, ttl)
		},
	}
	rl := &refusedLog{seen: map[string]time.Time{}}
	h.refused = func(name string) {
		m.stats.DNSRefused.Add(1)
		if rl.first(name) {
			m.logf("egress: %s: refused DNS name %s (not in the allow-list)", id, name)
		}
	}
	p, err := startDNSProxy(Path(id), netip.AddrPortFrom(netip.MustParseAddr(TapIP), 53), m.DNS, h, &m.stats)
	if err != nil {
		return fmt.Errorf("DNS proxy for %s: %w", id, err)
	}
	m.mu.Lock()
	if m.proxies == nil {
		m.proxies = map[string]*dnsProxy{}
	}
	m.proxies[id] = p
	m.mu.Unlock()
	return nil
}

// refusedLog logs each refused name at most once a minute.
type refusedLog struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (r *refusedLog) first(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if t, ok := r.seen[name]; ok && now.Sub(t) < time.Minute {
		return false
	}
	if len(r.seen) > 1024 {
		clear(r.seen)
	}
	r.seen[name] = now
	return true
}
