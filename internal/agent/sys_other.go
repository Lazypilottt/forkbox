//go:build !linux

package agent

import (
	"errors"
	"fmt"
	"log"
	"net"
	"time"
)

// The agent only runs in Linux guests. These stand-ins let the server build
// and be unit-tested on other systems.

func portListening(port uint16) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
	}
	return false
}

func uptimeMs() uint64 { return 0 }

// ListenVsock is Linux-only.
func ListenVsock(port uint32) (net.Listener, error) {
	return nil, errors.New("vsock: the guest agent only runs on Linux")
}

// RunInit is Linux-only.
func RunInit(logger *log.Logger, serveArgs []string) {
	logger.Fatal("init mode only runs in a Linux guest")
}

type otherSystem struct{}

func defaultSystem() system { return otherSystem{} }

func (otherSystem) Now() time.Time           { return time.Now() }
func (otherSystem) SetClock(time.Time) error { return errors.New("not supported off Linux") }
func (otherSystem) AddEntropy([]byte) error  { return errors.New("not supported off Linux") }
func (otherSystem) SetHostname(string) error { return errors.New("not supported off Linux") }
func (s *Server) WatchVMGenID() error        { return errors.New("uevents are Linux-only") }
func isNewVMGenID([]byte) bool               { return false }
