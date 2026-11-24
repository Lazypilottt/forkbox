package netns

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// Policy is a sandbox's egress allow-list. Egress is denied by default: a
// guest reaches only the networks listed and the addresses that allowed
// names resolve to (through the namespace's DNS proxy). Link-local
// addresses (cloud metadata) and other sandboxes are never reachable.
type Policy struct {
	Open    bool           // "*": any destination
	Domains []string       // a name allows itself and its subdomains
	Nets    []netip.Prefix // addresses and CIDRs
}

// ErrPolicy marks a bad allow-list entry.
var ErrPolicy = errors.New("invalid egress entry")

var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ParsePolicy reads allow-list entries: "*" (anything), an IPv4 address or
// CIDR, or a domain name ("pypi.org" also allows "files.pypi.org"; a
// leading "*." is accepted and means the same).
func ParsePolicy(entries []string) (Policy, error) {
	var p Policy
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case e == "*":
			p.Open = true
		case strings.ContainsAny(e, ":/") || isIPv4(e):
			pr, err := netip.ParsePrefix(e)
			if err != nil {
				a, aerr := netip.ParseAddr(e)
				if aerr != nil {
					return Policy{}, fmt.Errorf("%w: %q is neither an address nor a CIDR", ErrPolicy, e)
				}
				pr = netip.PrefixFrom(a, a.BitLen())
			}
			if !pr.Addr().Is4() {
				return Policy{}, fmt.Errorf("%w: %q: guests have IPv4 only", ErrPolicy, e)
			}
			p.Nets = append(p.Nets, pr.Masked())
		default:
			d := strings.TrimSuffix(strings.TrimPrefix(e, "*."), ".")
			if len(d) > 253 || !domainPattern.MatchString(d) || !strings.Contains(d, ".") {
				return Policy{}, fmt.Errorf("%w: %q: give a domain such as pypi.org, an address, a CIDR or *", ErrPolicy, e)
			}
			p.Domains = append(p.Domains, d)
		}
	}
	slices.Sort(p.Domains)
	p.Domains = slices.Compact(p.Domains)
	slices.SortFunc(p.Nets, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	p.Nets = slices.Compact(p.Nets)
	return p, nil
}

func isIPv4(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4()
}

// Entries is the policy as allow-list entries (ParsePolicy's input).
func (p Policy) Entries() []string {
	var out []string
	if p.Open {
		out = append(out, "*")
	}
	out = append(out, p.Domains...)
	for _, n := range p.Nets {
		out = append(out, n.String())
	}
	return out
}

// Merge is the union of two policies.
func (p Policy) Merge(q Policy) Policy {
	out, _ := ParsePolicy(append(p.Entries(), q.Entries()...))
	return out
}

// AllowsName reports whether DNS may resolve name.
func (p Policy) AllowsName(name string) bool {
	if p.Open {
		return true
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, d := range p.Domains {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// AllowsAddr reports whether a guest may reach a.
func (p Policy) AllowsAddr(a netip.Addr) bool {
	if p.Open {
		return true
	}
	for _, n := range p.Nets {
		if n.Contains(a) {
			return true
		}
	}
	return false
}
