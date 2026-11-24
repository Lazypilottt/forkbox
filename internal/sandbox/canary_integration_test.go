//go:build integration

package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/vm"
)

// TestIntegrationHostCanary checks that this host keeps restored microVMs
// running: a few sandboxes, exec in each for 20 s. Some nested
// virtualization hosts (seen on Intel Xeon 6975P CI runners) stop
// restored VMs with KVM_EXIT_FAIL_ENTRY within seconds; CI runs this first
// (FORKBOX_CANARY=1) so such a runner fails in a minute, not after the
// suite's timeout.
func TestIntegrationHostCanary(t *testing.T) {
	if os.Getenv("FORKBOX_CANARY") == "" {
		t.Skip("set FORKBOX_CANARY=1 (CI runs it before the suite)")
	}
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			m.Destroy(context.Background(), id)
		}
	})
	for range 6 {
		c, err := m.Create(ctx, CreateOptions{Template: "python"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		for _, id := range ids {
			rec, err := m.Store.GetSandbox(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !m.VMs.Alive(rec.Pid, id) {
				console := vm.ConsoleTail(m.DataDir, id, 4096)
				if strings.Contains(console, "KVM_EXIT_FAIL_ENTRY") {
					t.Fatalf("HOST FAULT: KVM could not enter the restored VM %s (KVM_EXIT_FAIL_ENTRY). This host cannot run restored microVMs reliably; use another one.", id)
				}
				t.Fatalf("restored VM %s exited:\n%s", id, console)
			}
			sh(t, m, id, "python -c 'print(sum(range(100000)))' >/dev/null")
		}
	}
}
