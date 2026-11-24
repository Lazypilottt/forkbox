//go:build integration

package sandbox

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/netns"
	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

// run runs a shell command and returns its exit code and output, without
// failing the test on a non-zero exit.
func run(t *testing.T, m *Manager, id, cmd string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, err := m.Agent(id).Run(ctx, agentclient.ExecRequest{Args: []string{"sh", "-c", cmd}, Timeout: 2 * time.Minute}, 0)
	if err != nil {
		t.Fatalf("%s: %s: %v", id, cmd, err)
	}
	return r.Code, strings.TrimSpace(string(r.Stdout) + string(r.Stderr))
}

// Egress is denied by default: the python template allows PyPI only.
func TestIntegrationEgress(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			m.Destroy(context.Background(), id)
		}
	})
	create := func(egress ...string) *Created {
		c, err := m.Create(ctx, CreateOptions{Template: "python", Egress: egress})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
		return c
	}

	c := create()
	if c.Egress != "pypi.org,pythonhosted.org" {
		t.Errorf("egress %q", c.Egress)
	}
	start := time.Now()
	if code, out := run(t, m, c.ID, "pip install --no-cache-dir --target /tmp/pkgs six 2>&1 | tail -2"); code != 0 || !strings.Contains(out, "Successfully installed six") {
		t.Fatalf("pip install from PyPI: exit %d: %s", code, out)
	}
	t.Logf("pip install six through the allow-list: %s", time.Since(start).Round(time.Millisecond))
	code, out := run(t, m, c.ID, "curl -sS --max-time 5 https://example.com/")
	if code == 0 || !strings.Contains(out, "resolve") {
		t.Errorf("curl to a name off the list: exit %d: %s", code, out)
	}
	code, out = run(t, m, c.ID, "curl -sS --max-time 3 http://1.1.1.1/")
	if code == 0 {
		t.Errorf("curl to an address off the list succeeded: %s", out)
	}
	_, out = run(t, m, c.ID, "getent hosts example.com || echo unresolved")
	if !strings.Contains(out, "unresolved") {
		t.Errorf("example.com resolved: %s", out)
	}
	bl, err := m.Net.Blocked(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(bl, func(b netns.Blocked) bool { return b.String() == "tcp 1.1.1.1:80" }) {
		t.Errorf("blocked set %v lacks tcp 1.1.1.1:80", bl)
	}
	m.LogBlocked(ctx)
	if st := m.EgressStats(); st.DNSRefused == 0 || st.BlockedDestinations == 0 || st.DNSAllowed == 0 {
		t.Errorf("stats %+v", st)
	}

	// Forks keep the policy, and the addresses the parent resolved.
	fr, err := m.Fork(ctx, c.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	child := fr.Children[0]
	ids = append(ids, child.ID)
	if child.Egress != c.Egress {
		t.Errorf("child egress %q", child.Egress)
	}
	if al, err := m.Net.Allowed(ctx, child.ID); err != nil || len(al) == 0 {
		t.Errorf("child's allowed addresses: %v %v", al, err)
	}
	if code, out := run(t, m, child.ID, "curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://pypi.org/simple/six/"); code != 0 || out != "200" {
		t.Errorf("child to PyPI: exit %d: %s", code, out)
	}
	if code, _ := run(t, m, child.ID, "curl -sS --max-time 5 https://example.com/"); code == 0 {
		t.Error("child reached a name off the list")
	}

	// A sandbox's own entries add to the template's; "*" opens everything
	// but the metadata address.
	if code, out := run(t, m, create("example.com").ID, "curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://example.com/"); code != 0 || out != "200" {
		t.Errorf("allowed by the sandbox: exit %d: %s", code, out)
	}
	open := create("*")
	if code, out := run(t, m, open.ID, "curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://www.iana.org/"); code != 0 || out == "000" {
		t.Errorf("open egress: exit %d: %s", code, out)
	}
	if code, _ := run(t, m, open.ID, "curl -sS --max-time 3 http://169.254.169.254/"); code == 0 {
		t.Error("open egress reached the cloud metadata address")
	}
	if _, err := m.Create(ctx, CreateOptions{Template: "python", Egress: []string{"not a name"}}); err == nil {
		t.Error("a bad egress entry was accepted")
	}
}
