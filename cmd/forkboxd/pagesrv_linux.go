package main

import "github.com/Lazypilottt/forkbox/internal/pagesrv"

func pagesrvRun(socket string, layers []string, uid, gid int, stats, cgroup string, prefetch bool) error {
	return pagesrv.Run(pagesrv.Options{Socket: socket, Layers: layers, UID: uid, GID: gid, Stats: stats, Cgroup: cgroup, Prefetch: prefetch})
}
