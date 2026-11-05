//go:build integration

package doctor

import (
	"bytes"
	"context"
	"testing"

	"github.com/Lazypilottt/forkbox/internal/datadir"
	"github.com/Lazypilottt/forkbox/internal/testutil"
	"github.com/Lazypilottt/forkbox/internal/versions"
)

// TestDoctorPassesOnThisHost is Task 1's acceptance check on the KVM host. It
// expects `sudo forkbox setup` to have run, and the tests to run as a user
// that can write the data directory ($FORKBOX_DATA_DIR, default /var/lib/forkbox).
func TestDoctorPassesOnThisHost(t *testing.T) {
	testutil.RequireKVM(t)
	v, err := versions.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	results := Run(context.Background(), System{}, Config{DataDir: datadir.FromEnv(), Versions: v})
	var buf bytes.Buffer
	Print(&buf, results)
	t.Log("\n" + buf.String())
	if n := Failed(results); n > 0 {
		t.Fatalf("%d doctor checks failed on this host", n)
	}
}
