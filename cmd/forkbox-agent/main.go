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
var version = "0.1.0"

func main() {
	port := flag.Uint("port", agentwire.Port, "vsock port to listen on")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	logger := log.New(os.Stderr, "forkbox-agent: ", 0) // journald adds timestamps

	// As PID 1 (kernel init= in images without an init system), become a
	// minimal init and run the server as a child.
	if os.Getpid() == 1 {
		agent.RunInit(logger, []string{"serve", fmt.Sprintf("-port=%d", *port)})
	}
	if flag.Arg(0) != "" && flag.Arg(0) != "serve" {
		logger.Fatalf("unknown command %q (usage: forkbox-agent [serve] [-port N])", flag.Arg(0))
	}
	// Flags may also follow "serve".
	if flag.Arg(0) == "serve" {
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		p := fs.Uint("port", *port, "vsock port to listen on")
		_ = fs.Parse(flag.Args()[1:])
		*port = *p
	}
	// Marks a Forkbox guest for the clone hooks (templates/python).
	_ = os.MkdirAll("/run/forkbox", 0o755)
	gc, err := agent.LoadGuestConfig(agent.GuestConfigPath)
	if err != nil {
		logger.Printf("guest config: %v (using defaults)", err)
	}

	ln, err := agent.ListenVsock(uint32(*port))
	if err != nil {
		logger.Fatal(err)
	}
	srv := agent.New(version)
	srv.Log = logger
	srv.Env, srv.Cwd = gc.Env, gc.Cwd
	if err := srv.WatchVMGenID(); err != nil {
		logger.Printf("VM generation ID changes will not be seen: %v", err)
	}

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
