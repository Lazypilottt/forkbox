package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/proto/agentpb"
)

// Clone correctness. Clones restored from one snapshot start with the same
// kernel RNG state, machine-id, hostname and a wall clock frozen at snapshot
// time. Firecracker changes the VM generation ID on restore, and Linux 5.18+
// reseeds its CRNG when the vmgenid driver sees that, but there is a window
// before that happens (docs/snapshotting/random-for-clones.md). So:
//
//   - On the NEW_VMGENID uevent the agent reseeds from local randomness
//     (RNDADDENTROPY, then RNDRESEEDCRNG) and writes a new machine-id.
//   - The host calls Restored right after resuming, with its own entropy,
//     the sandbox id and its clock. The agent reseeds again, writes a new
//     machine-id, sets the hostname and steps the clock.

// system is what clone handling changes in the guest; tests swap it.
type system interface {
	Now() time.Time
	SetClock(time.Time) error
	// AddEntropy mixes b into the input pool, credited, and forces a CRNG
	// reseed.
	AddEntropy(b []byte) error
	SetHostname(string) error
}

const maxHostEntropy = 512

func (s *Server) restored(c net.Conn, r *agentpb.RestoredRequest) error {
	if r.SandboxId == "" || r.Hostname == "" || strings.ContainsAny(r.Hostname, "/ \x00") || len(r.Hostname) > 64 {
		return agentwire.Write(c, errResp(agentpb.Error_INVALID_ARGUMENT, "restored: need a sandbox id and a hostname of up to 64 characters"))
	}
	if len(r.Entropy) > maxHostEntropy {
		r.Entropy = r.Entropy[:maxHostEntropy]
	}
	resp := &agentpb.RestoredResponse{}
	act := func(format string, a ...any) { resp.Actions = append(resp.Actions, fmt.Sprintf(format, a...)) }

	// Clock first, so files written below get the right time.
	if r.RealtimeUnixNano > 0 {
		before := s.sys.Now()
		host := time.Unix(0, r.RealtimeUnixNano)
		if err := s.sys.SetClock(host); err != nil {
			act("clock: %v", err)
		} else {
			resp.ClockStepNs = host.Sub(before).Nanoseconds()
			act("clock stepped by %s", time.Duration(resp.ClockStepNs).Round(time.Millisecond))
		}
	}
	if err := s.reseed(r.Entropy); err != nil {
		act("rng: %v", err)
	} else {
		act("rng reseeded with %d host bytes", len(r.Entropy))
	}
	id, err := s.newMachineID()
	if err != nil {
		act("machine-id: %v", err)
	} else {
		resp.MachineId = id
		act("machine-id %s", id)
	}
	if err := s.sys.SetHostname(r.Hostname); err != nil {
		act("hostname: %v", err)
	} else {
		act("hostname %s", r.Hostname)
	}
	if err := writeAtomic(filepath.Join(s.EtcDir, "hostname"), r.Hostname+"\n"); err != nil {
		act("/etc/hostname: %v", err)
	}
	s.mu.Lock()
	s.sandboxID = r.SandboxId
	s.mu.Unlock()
	resp.Generation = s.generation.Load()
	s.Log.Printf("restored as %s: %s", r.SandboxId, strings.Join(resp.Actions, "; "))
	return agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_Restored{Restored: resp}})
}

// OnNewGeneration handles a VM generation ID change seen inside the guest.
func (s *Server) OnNewGeneration() {
	g := s.generation.Add(1)
	var done []string
	if err := s.reseed(nil); err != nil {
		done = append(done, "rng: "+err.Error())
	} else {
		done = append(done, "rng reseeded")
	}
	if id, err := s.newMachineID(); err != nil {
		done = append(done, "machine-id: "+err.Error())
	} else {
		done = append(done, "machine-id "+id)
	}
	s.Log.Printf("VM generation changed (%d): %s", g, strings.Join(done, "; "))
}

// reseed mixes extra plus 64 fresh local bytes into the kernel pool.
func (s *Server) reseed(extra []byte) error {
	local := make([]byte, 64)
	if _, err := rand.Read(local); err != nil {
		return err
	}
	return s.sys.AddEntropy(append(append([]byte(nil), extra...), local...))
}

// newMachineID writes a new random /etc/machine-id (32 hex digits).
func (s *Server) newMachineID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	return id, writeAtomic(filepath.Join(s.EtcDir, "machine-id"), id+"\n")
}

func writeAtomic(path, content string) error {
	tmp := path + ".fbx-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o444); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
