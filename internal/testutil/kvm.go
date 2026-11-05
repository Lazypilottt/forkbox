// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"runtime"
	"testing"
)

// RequireKVM skips the test unless this is Linux and /dev/kvm can be opened
// for reading and writing. Integration tests (build tag "integration") call it
// first, so they skip cleanly on laptops and in containers.
func RequireKVM(t testing.TB) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("needs a Linux KVM host (running on %s)", runtime.GOOS)
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("needs read-write /dev/kvm: %v", err)
	}
	f.Close()
}
