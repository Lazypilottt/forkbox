package vm

import (
	"path/filepath"
	"strings"
)

// Paths inside every jail, as the jailed Firecracker sees them. Snapshots
// record device paths, so these must be identical in every jail: a VM
// restored in a new jail finds its drive and vsock socket at the same place.
const (
	jailKernel   = "/boot/vmlinux"
	jailRootfs   = "/drives/rootfs"
	jailAPISock  = "/api.sock"
	jailVsock    = "/v.sock"
	jailSnapDir  = "/snapshot" // written by Firecracker, then moved out
	jailSnapVM   = jailSnapDir + "/vmstate"
	jailSnapMem  = jailSnapDir + "/mem"
	jailRestDir  = "/restore" // read-only inputs of a restore
	jailRestVM   = jailRestDir + "/vmstate"
	jailRestMem  = jailRestDir + "/mem"
	execFileName = "firecracker" // the jailer names the jail after the exec file
)

// Files of a snapshot directory written by SnapshotFull and SnapshotDiff.
const (
	SnapshotStateFile = "vmstate"
	SnapshotMemFile   = "mem"
	SnapshotMetaFile  = "snapshot.json"
)

// maxSunPath is the usable length of a Unix socket path on Linux.
const maxSunPath = 107

// layout holds the host paths for one VM.
type layout struct {
	dataDir string
	id      string
}

// JailBase is the jailer's --chroot-base-dir.
func JailBase(dataDir string) string { return filepath.Join(dataDir, "jail") }

// jailDir is <chroot-base>/firecracker/<id>, removed as a whole on cleanup.
func (l layout) jailDir() string { return filepath.Join(JailBase(l.dataDir), execFileName, l.id) }

// chroot is the jail root the jailed Firecracker sees as "/".
func (l layout) chroot() string { return filepath.Join(l.jailDir(), "root") }

// host maps an in-jail absolute path to the host path.
func (l layout) host(inJail string) string {
	return filepath.Join(l.chroot(), strings.TrimPrefix(inJail, "/"))
}

// runDir holds host-only files for the VM, outside the jail (console log).
func (l layout) runDir() string { return filepath.Join(l.dataDir, "vms", l.id) }

func (l layout) consoleLog() string { return filepath.Join(l.runDir(), "console.log") }

// VsockSocketPath is the host path of a VM's vsock Unix socket. Host-initiated
// connections dial it and send "CONNECT <port>\n".
func VsockSocketPath(dataDir, id string) string {
	return layout{dataDir, id}.host(jailVsock)
}
