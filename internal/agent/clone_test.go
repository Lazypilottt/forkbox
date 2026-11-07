package agent

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/proto/agentpb"
)

type fakeSystem struct {
	now      time.Time
	clock    time.Time
	entropy  [][]byte
	hostname string
}

func (f *fakeSystem) Now() time.Time             { return f.now }
func (f *fakeSystem) SetClock(t time.Time) error { f.clock = t; return nil }
func (f *fakeSystem) AddEntropy(b []byte) error {
	f.entropy = append(f.entropy, append([]byte(nil), b...))
	return nil
}
func (f *fakeSystem) SetHostname(n string) error { f.hostname = n; return nil }

func call(t *testing.T, s *Server, req *agentpb.Request) *agentpb.Response {
	t.Helper()
	a, b := net.Pipe()
	go s.ServeConn(b)
	defer a.Close()
	if err := agentwire.Write(a, req); err != nil {
		t.Fatal(err)
	}
	resp := &agentpb.Response{}
	if err := agentwire.Read(a, resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRestored(t *testing.T) {
	etc := t.TempDir()
	os.WriteFile(filepath.Join(etc, "machine-id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o444)
	fs := &fakeSystem{now: time.Unix(1000, 0)}
	s := New("test")
	s.sys, s.EtcDir = fs, etc
	s.GenerationFile = filepath.Join(etc, "run", "generation")

	host := time.Unix(5000, 250)
	resp := call(t, s, &agentpb.Request{Op: &agentpb.Request_Restored{Restored: &agentpb.RestoredRequest{
		SandboxId: "sb-1", Hostname: "sb-1", RealtimeUnixNano: host.UnixNano(), Entropy: []byte("host-entropy"),
	}}}).GetRestored()
	if resp == nil {
		t.Fatal("no restored response")
	}
	if !fs.clock.Equal(host) || resp.ClockStepNs != host.Sub(fs.now).Nanoseconds() {
		t.Errorf("clock set to %v, step %d", fs.clock, resp.ClockStepNs)
	}
	if len(fs.entropy) != 1 || !strings.HasPrefix(string(fs.entropy[0]), "host-entropy") || len(fs.entropy[0]) != 12+64 {
		t.Errorf("entropy = %q", fs.entropy)
	}
	if fs.hostname != "sb-1" {
		t.Errorf("hostname = %q", fs.hostname)
	}
	mid, _ := os.ReadFile(filepath.Join(etc, "machine-id"))
	if !regexp.MustCompile(`^[0-9a-f]{32}\n$`).Match(mid) || strings.TrimSpace(string(mid)) != resp.MachineId || resp.MachineId == "0123456789abcdef0123456789abcdef" {
		t.Errorf("machine-id %q (resp %q)", mid, resp.MachineId)
	}
	if hn, _ := os.ReadFile(filepath.Join(etc, "hostname")); string(hn) != "sb-1\n" {
		t.Errorf("/etc/hostname = %q", hn)
	}
	in := call(t, s, &agentpb.Request{Op: &agentpb.Request_Info{Info: &agentpb.InfoRequest{}}}).GetInfo()
	if in.SandboxId != "sb-1" {
		t.Errorf("info sandbox id = %q", in.SandboxId)
	}

	gen1, err := os.ReadFile(s.GenerationFile)
	if err != nil || !strings.HasPrefix(string(gen1), "1 ") {
		t.Errorf("generation file %q %v", gen1, err)
	}
	// A second clone gets a different machine-id.
	resp2 := call(t, s, &agentpb.Request{Op: &agentpb.Request_Restored{Restored: &agentpb.RestoredRequest{SandboxId: "sb-2", Hostname: "sb-2"}}}).GetRestored()
	if resp2.MachineId == resp.MachineId {
		t.Errorf("machine-id repeated")
	}
	if gen2, _ := os.ReadFile(s.GenerationFile); !strings.HasPrefix(string(gen2), "2 ") || string(gen2) == string(gen1) {
		t.Errorf("generation file did not change: %q", gen2)
	}
	bad := call(t, s, &agentpb.Request{Op: &agentpb.Request_Restored{Restored: &agentpb.RestoredRequest{SandboxId: "x", Hostname: "a b"}}})
	if bad.GetError().GetCode() != agentpb.Error_INVALID_ARGUMENT {
		t.Errorf("bad hostname: %v", bad)
	}
}

func TestOnNewGeneration(t *testing.T) {
	etc := t.TempDir()
	fs := &fakeSystem{}
	s := New("test")
	s.sys, s.EtcDir = fs, etc
	s.GenerationFile = filepath.Join(etc, "generation")
	s.OnNewGeneration()
	s.OnNewGeneration()
	if s.generation.Load() != 2 || len(fs.entropy) != 2 {
		t.Errorf("generation %d, reseeds %d", s.generation.Load(), len(fs.entropy))
	}
	if _, err := os.Stat(filepath.Join(etc, "machine-id")); err != nil {
		t.Errorf("machine-id not written: %v", err)
	}
	in := call(t, s, &agentpb.Request{Op: &agentpb.Request_Info{Info: &agentpb.InfoRequest{}}}).GetInfo()
	if in.Generation != 2 {
		t.Errorf("info generation = %d", in.Generation)
	}
}
