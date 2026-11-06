// Command forkbox-agent runs inside each guest and serves the host over
// vsock: exec with streamed output, files, stat, port waits and guest info.
// Build it static (CGO_ENABLED=0); templates install it at
// /usr/local/bin/forkbox-agent with agent.SystemdUnit.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Lazypilottt/forkbox/internal/agent"
	"github.com/Lazypilottt/forkbox/internal/agentwire"
)

// version is set at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	port := flag.Uint("port", agentwire.Port, "vsock port to listen on")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	logger := log.New(os.Stderr, "forkbox-agent: ", 0) // journald adds timestamps

	ln, err := agent.ListenVsock(uint32(*port))
	if err != nil {
		logger.Fatal(err)
	}
	srv := agent.New(version)
	srv.Log = logger

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		logger.Printf("%s: shutting down", s)
		ln.Close()
	}()

	logger.Printf("version %s listening on vsock port %d", version, *port)
	if err := srv.Serve(ln); err != nil {
		logger.Fatal(err)
	}
	srv.Close()
}
