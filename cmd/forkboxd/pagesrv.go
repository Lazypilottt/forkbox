package main

import (
	"errors"
	"flag"
	"strings"
)

// runPagesrv is `forkboxd pagesrv`, the page server process the daemon
// starts for each uffd-backed VM (internal/sandbox). It is not meant to be
// run by hand.
func runPagesrv(args []string) error {
	fs := flag.NewFlagSet("pagesrv", flag.ContinueOnError)
	socket := fs.String("socket", "", "host path of the jail's uffd socket")
	layers := fs.String("layers", "", "memory files, newest first, comma-separated")
	uid := fs.Int("uid", 0, "Firecracker's uid")
	gid := fs.Int("gid", 0, "Firecracker's gid")
	stats := fs.String("stats", "", "stats JSON file")
	cgroup := fs.String("cgroup", "", "the VM's cgroup, joined after the handshake")
	prefetch := fs.Bool("prefetch", false, "copy the newest layer's pages ahead of faults")
	prefetched := fs.String("prefetched-file", "", "with --prefetch: create this file once the prefetch is done")
	fs.String("id", "", "the sandbox id (lets the daemon find this process)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" || *layers == "" {
		return errors.New("--socket and --layers are required")
	}
	return pagesrvRun(*socket, strings.Split(*layers, ","), *uid, *gid, *stats, *cgroup, *prefetch, *prefetched)
}
