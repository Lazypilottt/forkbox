//go:build integration

package sandbox

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/vm"
)

// TestIntegrationHostCanary checks that this host keeps restored microVMs
// running: a busy sandbox forked into eight, exec in each for 30 s. Some nested
// virtualization hosts (seen on Intel Xeon 6975P CI runners) stop
// restored VMs with KVM_EXIT_FAIL_ENTRY within seconds; CI runs this first
// (FORKBOX_CANARY=1) so such a runner fails in a minute, not after the
// suite's timeout.
func TestIntegrationHostCanary(t *testing.T) {
	if os.Getenv("FORKBOX_CANARY") == "" {
		t.Skip("set FORKBOX_CANARY=1 (CI runs it before the suite)")
	}
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			m.Destroy(context.Background(), id)
		}
	})
	hostFault := func(what string, err error) {
		t.Helper()
		if err != nil && strings.Contains(err.Error(), "KVM_EXIT_FAIL_ENTRY") {
			t.Fatalf("HOST FAULT: KVM could not enter a restored VM (%s: KVM_EXIT_FAIL_ENTRY). This host cannot run restored microVMs reliably; use another one.", what)
		}
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// What the fault showed up under: a busy parent forked into children
	// that all keep working.
	parent, err := m.Create(ctx, CreateOptions{Template: "python"})
	hostFault("create", err)
	ids = append(ids, parent.ID)
	sh(t, m, parent.ID, `setsid python -c "
import time, numpy
while True:
    numpy.random.random(10000).sum(); time.sleep(0.02)
" >/dev/null 2>&1 < /dev/null &`)
	time.Sleep(2 * time.Second)
	fr, err := m.Fork(ctx, parent.ID, 8, 0)
	hostFault("fork", err)
	for _, c := range fr.Children {
		ids = append(ids, c.ID)
	}
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		for _, id := range ids {
			rec, err := m.Store.GetSandbox(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !m.VMs.Alive(rec.Pid, id) {
				console := vm.ConsoleTail(m.DataDir, id, 4096)
				if strings.Contains(console, "KVM_EXIT_FAIL_ENTRY") {
					hostFault("sandbox "+id, errors.New("KVM_EXIT_FAIL_ENTRY"))
				}
				t.Fatalf("restored VM %s exited:\n%s", id, console)
			}
			sh(t, m, id, "python -c 'print(sum(range(100000)))' >/dev/null")
		}
	}
}
