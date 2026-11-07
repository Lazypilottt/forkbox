//go:build integration

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/vm/agentclient"
)

// The clone-correctness suite: one parent, forked into 50 children.

// A long-lived process started in the parent before the fork. It keeps
// user-space random state that every child inherits.
const probeScript = `import json, os, random, signal, ssl, time, uuid
import numpy as np
import forkbox_guest

rng = np.random.default_rng()          # created before the fork
def remake():
    global rng
    rng = np.random.default_rng()
forkbox_guest.on_restore(remake)        # the documented call for own generators

value = 0
def bump(*_):
    global value
    value += 1000
signal.signal(signal.SIGUSR1, bump)

while True:
    out = {"restores": forkbox_guest.restores(), "random": random.random(), "uuid4": str(uuid.uuid4()),
           "ssl": ssl.RAND_bytes(16).hex(), "np_global": float(np.random.random()),
           "np_rng": float(rng.random()), "value": value}
    with open("/tmp/probe.json.tmp", "w") as f:
        json.dump(out, f)
    os.replace("/tmp/probe.json.tmp", "/tmp/probe.json")
    value += 1
    time.sleep(0.05)
`

// Holds one keep-alive HTTP connection open from before the fork, and uses
// it again when /tmp/tcp-go appears.
const tcpProbeScript = `import http.client, os, time
c = http.client.HTTPConnection("example.com", 80, timeout=10)
c.request("GET", "/", headers={"Connection": "keep-alive"})
r = c.getresponse(); r.read()
open("/tmp/tcp-ready", "w").write(str(r.status))
while not os.path.exists("/tmp/tcp-go"):
    time.sleep(0.05)
try:
    c.request("GET", "/", headers={"Connection": "keep-alive"})
    r = c.getresponse(); r.read()
    res = "ok %d" % r.status
except Exception as e:
    res = "error %s" % type(e).__name__
open("/tmp/tcp-result", "w").write(res)
`

type probe struct {
	Restores int     `json:"restores"`
	Random   float64 `json:"random"`
	UUID4    string  `json:"uuid4"`
	SSL      string  `json:"ssl"`
	NPGlobal float64 `json:"np_global"`
	NPRng    float64 `json:"np_rng"`
	Value    int     `json:"value"`
}

func readProbe(t *testing.T, m *Manager, id string) probe {
	t.Helper()
	var p probe
	if err := json.Unmarshal([]byte(sh(t, m, id, "cat /tmp/probe.json")), &p); err != nil {
		t.Fatalf("%s: probe: %v", id, err)
	}
	return p
}

// unique checks that f gives a different value for every sandbox.
func unique(t *testing.T, what string, ids []string, f func(id string) string) {
	t.Helper()
	seen := map[string]string{}
	var mu sync.Mutex
	parallel(ids, func(id string) {
		v := f(id)
		mu.Lock()
		defer mu.Unlock()
		if prev, dup := seen[v]; dup {
			t.Errorf("%s: %s and %s both have %q", what, prev, id, v)
		}
		seen[v] = id
	})
}

func parallel(ids []string, f func(id string)) {
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f(id)
		}()
	}
	wg.Wait()
}

func TestIntegrationCloneCorrectness(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	parent, err := m.Create(ctx, CreateOptions{Template: "python"})
	if err != nil {
		t.Fatal(err)
	}
	all := []string{parent.ID}
	t.Cleanup(func() { parallel(all, func(id string) { m.Destroy(context.Background(), id) }) })

	ac := m.Agent(parent.ID)
	for path, body := range map[string]string{"/root/probe.py": probeScript, "/root/tcp_probe.py": tcpProbeScript} {
		if _, err := ac.WriteFile(ctx, path, strings.NewReader(body), agentclient.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	sh(t, m, parent.ID, "setsid python /root/probe.py >/tmp/probe.log 2>&1 < /dev/null &")
	sh(t, m, parent.ID, "setsid python /root/tcp_probe.py >/tmp/tcp_probe.log 2>&1 < /dev/null &")
	waitFile := func(id, path string) {
		deadline := time.Now().Add(30 * time.Second)
		for sh(t, m, id, "test -s "+path+" && echo yes || true") != "yes" {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %s never appeared; logs: %s", id, path, sh(t, m, id, "cat /tmp/*.log"))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitFile(parent.ID, "/tmp/probe.json")
	waitFile(parent.ID, "/tmp/tcp-ready")
	t.Logf("parent: vmgenid symbols in the guest kernel: %s", sh(t, m, parent.ID, "grep -ci vmgenid /proc/kallsyms || true"))

	res, err := m.Fork(ctx, parent.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, c := range res.Children {
		kids = append(kids, c.ID)
		all = append(all, c.ID)
	}
	t.Logf("fork into 50: parent paused %s, %.1f MiB dirty, merge %s, total %s", res.Timings.ParentPaused,
		mb(res.Timings.DirtyBytes), res.Timings.Merge, res.Timings.Total)

	t.Run("kernel randomness", func(t *testing.T) {
		unique(t, "/dev/urandom", all, func(id string) string { return sh(t, m, id, "head -c 1024 /dev/urandom | sha256sum") })
		unique(t, "getrandom()", all, func(id string) string {
			return sh(t, m, id, `python -c 'import os, hashlib; print(hashlib.sha256(os.getrandom(1024)).hexdigest())'`)
		})
	})

	t.Run("user-space generators", func(t *testing.T) {
		probes := map[string]probe{}
		var mu sync.Mutex
		parallel(kids, func(id string) {
			deadline := time.Now().Add(10 * time.Second)
			p := readProbe(t, m, id)
			for p.Restores < 1 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
				p = readProbe(t, m, id)
			}
			if p.Restores < 1 {
				t.Errorf("%s: the probe never saw the restore", id)
			}
			mu.Lock()
			probes[id] = p
			mu.Unlock()
		})
		for name, f := range map[string]func(p probe) string{
			"random.random":            func(p probe) string { return fmt.Sprint(p.Random) },
			"uuid.uuid4":               func(p probe) string { return p.UUID4 },
			"ssl.RAND_bytes":           func(p probe) string { return p.SSL },
			"numpy.random (global)":    func(p probe) string { return fmt.Sprint(p.NPGlobal) },
			"numpy default_rng (hook)": func(p probe) string { return fmt.Sprint(p.NPRng) },
		} {
			seen := map[string]string{}
			for id, p := range probes {
				v := f(p)
				if prev, dup := seen[v]; dup {
					t.Errorf("%s: %s and %s both produced %s", name, prev, id, v)
				}
				seen[v] = id
			}
		}
	})

	t.Run("identity", func(t *testing.T) {
		unique(t, "machine-id", all, func(id string) string { return sh(t, m, id, "cat /etc/machine-id") })
		unique(t, "hostname", all[1:], func(id string) string {
			h := sh(t, m, id, "hostname")
			if h != id {
				t.Errorf("%s: hostname %q", id, h)
			}
			return h
		})
		unique(t, "agent sandbox id", kids, func(id string) string {
			in, err := m.Agent(id).Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if in.SandboxId != id {
				t.Errorf("%s: agent reports %q", id, in.SandboxId)
			}
			return in.SandboxId
		})
		// boot_id is set once per kernel boot and is read-only; clones resume
		// the same boot, so it is the same everywhere (random-for-clones.md).
		boot := map[string]bool{}
		for _, id := range all {
			boot[sh(t, m, id, "cat /proc/sys/kernel/random/boot_id")] = true
		}
		if len(boot) != 1 {
			t.Errorf("boot_id differs across clones (%d values); the docs say it stays", len(boot))
		}
		in, _ := m.Agent(kids[0]).Info(ctx)
		t.Logf("VM generation ID changes seen by a child's agent: %d", in.Generation)
	})

	checkClocks := func(t *testing.T) {
		parallel(all, func(id string) {
			before := time.Now()
			in, err := m.Agent(id).Info(ctx)
			after := time.Now()
			if err != nil {
				t.Errorf("%s: %v", id, err)
				return
			}
			g := time.Unix(0, in.RealtimeUnixNano)
			if g.Before(before.Add(-time.Second)) || g.After(after.Add(time.Second)) {
				t.Errorf("%s: guest clock %s, host %s", id, g.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano))
			}
		})
	}
	t.Run("clock after resume", checkClocks)

	t.Run("network", func(t *testing.T) {
		parallel(all, func(id string) { sh(t, m, id, "touch /tmp/tcp-go") })
		parallel(all, func(id string) {
			deadline := time.Now().Add(30 * time.Second)
			var r string
			for r == "" && time.Now().Before(deadline) {
				r = sh(t, m, id, "cat /tmp/tcp-result 2>/dev/null || true")
				time.Sleep(100 * time.Millisecond)
			}
			wantOK := id == parent.ID
			if gotOK := strings.HasPrefix(r, "ok"); gotOK != wantOK || r == "" {
				t.Errorf("%s: stale connection gave %q (parent should keep it, children should not)", id, r)
			}
		})
		parallel(kids[:10], func(id string) {
			if code := sh(t, m, id, "curl -sS -o /dev/null -w '%{http_code}' --max-time 20 https://example.com/"); code != "200" {
				t.Errorf("%s: new outbound connection: %s", id, code)
			}
		})
	})

	t.Run("isolation", func(t *testing.T) {
		target := kids[0]
		before := map[string]int{}
		for _, id := range all {
			before[id] = readProbe(t, m, id).Value
		}
		sh(t, m, target, "pkill -USR1 -f [p]robe.py")
		sh(t, m, target, "echo mine > /tmp/written-by-"+target)
		time.Sleep(500 * time.Millisecond)
		for _, id := range all {
			jumped := readProbe(t, m, id).Value-before[id] >= 1000
			if jumped != (id == target) {
				t.Errorf("%s: in-memory variable jumped=%v after a signal to %s only", id, jumped, target)
			}
			has := sh(t, m, id, "test -e /tmp/written-by-"+target+" && echo yes || echo no") == "yes"
			if has != (id == target) {
				t.Errorf("%s: sees %s's file: %v", id, target, has)
			}
		}
	})

	if os.Getenv("FORKBOX_SHORT") == "" {
		time.Sleep(60 * time.Second)
		t.Run("clock after 60s", checkClocks)
	}
}
