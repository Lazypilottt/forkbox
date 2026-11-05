package main

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
)

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsage(t *testing.T) {
	if code, _, stderr := runCLI(); code != 2 || !strings.Contains(stderr, "forkbox doctor") {
		t.Errorf("no args: code %d, stderr %q", code, stderr)
	}
	if code, stdout, _ := runCLI("help"); code != 0 || !strings.Contains(stdout, "forkbox setup") {
		t.Errorf("help: code %d", code)
	}
	if code, _, stderr := runCLI("frobnicate"); code != 2 || !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("unknown: code %d, stderr %q", code, stderr)
	}
}

func TestVersion(t *testing.T) {
	code, stdout, _ := runCLI("version")
	if code != 0 || !strings.Contains(stdout, "Firecracker v") {
		t.Errorf("version: code %d, stdout %q", code, stdout)
	}
}

func TestDoctorBadVersionsFile(t *testing.T) {
	code, _, stderr := runCLI("doctor", "--versions", "/nonexistent/versions.yaml")
	if code != 1 || !strings.Contains(stderr, "/nonexistent/versions.yaml") {
		t.Errorf("code %d, stderr %q", code, stderr)
	}
}

func TestDoctorFailsOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("covered by the integration test on Linux")
	}
	code, stdout, _ := runCLI("doctor", "--data-dir", t.TempDir())
	if code != 1 || !strings.Contains(stdout, "FAIL  platform") || !strings.Contains(stdout, "fix: # Firecracker needs Linux") {
		t.Errorf("code %d, stdout:\n%s", code, stdout)
	}
}

func TestSetupRefusesOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("off-Linux behaviour")
	}
	code, _, stderr := runCLI("setup", "--data-dir", t.TempDir())
	if code != 1 || !strings.Contains(stderr, "this host runs "+runtime.GOOS) {
		t.Errorf("code %d, stderr %q", code, stderr)
	}
}

func TestBadFlag(t *testing.T) {
	if code, _, _ := runCLI("doctor", "--nope"); code != 2 {
		t.Errorf("bad flag: code %d, want 2", code)
	}
}
