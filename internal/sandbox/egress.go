package sandbox

import (
	"context"
	"strings"

	"github.com/Lazypilottt/forkbox/internal/netns"
	"github.com/Lazypilottt/forkbox/internal/store"
	"github.com/Lazypilottt/forkbox/internal/template"
)

// egressFor is a new sandbox's allow-list: the template's plus extra, as
// recorded in store.Sandbox.Egress.
func egressFor(tpl *template.Template, extra []string) (string, error) {
	p, err := netns.ParsePolicy(append(append([]string(nil), tpl.Spec.Egress...), extra...))
	if err != nil {
		return "", err
	}
	return strings.Join(p.Entries(), ","), nil
}

func splitEgress(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// LogBlocked logs destinations sandboxes were refused since the last call
// (refused DNS names are logged as they happen).
func (m *Manager) LogBlocked(ctx context.Context) {
	recs, err := m.Store.ListSandboxes(ctx)
	if err != nil {
		return
	}
	m.mu.Lock()
	if m.blockedSeen == nil {
		m.blockedSeen = map[string]map[string]bool{}
	}
	m.mu.Unlock()
	live := map[string]bool{}
	for _, r := range recs {
		if r.State != store.StateRunning {
			continue
		}
		live[r.ID] = true
		bl, err := m.Net.Blocked(ctx, r.ID)
		if err != nil {
			continue
		}
		var fresh []string
		m.mu.Lock()
		seen := m.blockedSeen[r.ID]
		if seen == nil {
			seen = map[string]bool{}
			m.blockedSeen[r.ID] = seen
		}
		for _, b := range bl {
			if k := b.String(); !seen[k] {
				seen[k] = true
				fresh = append(fresh, k)
			}
		}
		m.mu.Unlock()
		if len(fresh) > 0 {
			m.blockedConns.Add(uint64(len(fresh)))
			m.Log.Printf("egress: %s: blocked %s", r.ID, strings.Join(fresh, ", "))
		}
	}
	m.mu.Lock()
	for id := range m.blockedSeen {
		if !live[id] {
			delete(m.blockedSeen, id)
		}
	}
	m.mu.Unlock()
}

// EgressStats reports egress decisions since the daemon started.
type EgressStats struct {
	DNSAllowed, DNSRefused, BlockedDestinations uint64
}

// EgressStats returns the egress counters.
func (m *Manager) EgressStats() EgressStats {
	a, r := m.Net.Stats()
	return EgressStats{DNSAllowed: a, DNSRefused: r, BlockedDestinations: m.blockedConns.Load()}
}
