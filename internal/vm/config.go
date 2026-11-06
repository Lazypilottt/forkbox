package vm

import (
	"errors"
	"fmt"
	"os"
	"regexp"
)

// Config describes one microVM. Boot uses all of it; Restore uses the drive,
// network namespace and resource limits (vCPUs, memory, kernel and devices
// come from the snapshot).
type Config struct {
	// ID names the VM in the jailer, the cgroup and the data directory. It is
	// generated when empty. See ValidID.
	ID string

	VCPUs  int // 1 or an even number, at most 32 (Firecracker's limit)
	MemMiB int

	Kernel     string // host path of an uncompressed vmlinux
	KernelArgs string // DefaultKernelArgs when empty

	Rootfs Drive
	Vsock  *Vsock    // nil: no vsock device
	Net    *NetIface // nil: no network interface

	// NetNS is a network namespace path (e.g. /run/netns/fbx-...) that the
	// jailer joins. Empty keeps the host namespace; Task 7 always sets it.
	NetNS string

	// CPUMax is written to cpu.max. Empty means "<VCPUs*100000> 100000",
	// i.e. at most VCPUs host CPUs.
	CPUMax string
	// MemoryMax is written to memory.max in bytes. Zero means guest memory
	// plus MemoryOverheadMiB for the VMM.
	MemoryMax int64
}

// Drive is the root block device.
type Drive struct {
	// Path is the host file. It must be on the same filesystem as the data
	// directory, because it is hard-linked into the jail. A writable drive
	// must be a per-VM file: it is chowned to the jail user.
	Path     string
	ReadOnly bool
}

// Vsock is the virtio-vsock device. Its host-side Unix socket always lives at
// the same path inside every jail (see VsockSocketPath).
type Vsock struct {
	GuestCID uint32 // at least 3
}

// NetIface is one virtio-net device backed by a TAP in the VM's namespace.
type NetIface struct {
	IfaceID  string // e.g. eth0
	HostDev  string // TAP name; identical in every namespace (Task 7)
	GuestMAC string // optional
}

const (
	// DefaultKernelArgs are the boot arguments from Firecracker's getting
	// started guide. Firecracker appends root= for the root drive itself.
	DefaultKernelArgs = "console=ttyS0 reboot=k panic=1"
	// MemoryOverheadMiB is added to guest memory for the default memory.max.
	// Firecracker's own footprint is a few MiB; the rest covers page tables
	// and page cache charged to the VM's cgroup.
	MemoryOverheadMiB = 64
	// cpuPeriod is the cpu.max period in microseconds (the kernel default).
	cpuPeriod = 100000
)

// idPattern keeps IDs valid for the jailer (alphanumerics and hyphens) and
// short enough that the API socket path inside the jail stays under the
// 108-byte sun_path limit.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidID reports whether id can name a VM: 1 to 32 characters of lowercase
// letters, digits and hyphens, starting with a letter or digit.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ErrConfig wraps every validation error.
var ErrConfig = errors.New("invalid VM config")

func configErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConfig, fmt.Sprintf(format, a...))
}

// validateLimits checks the fields shared by Boot and Restore.
func (c *Config) validateLimits() error {
	if c.ID != "" && !ValidID(c.ID) {
		return configErr("id %q: use 1-32 lowercase letters, digits and hyphens", c.ID)
	}
	if c.VCPUs < 1 || c.VCPUs > 32 || (c.VCPUs > 1 && c.VCPUs%2 != 0) {
		return configErr("vcpus %d: Firecracker accepts 1 or an even number up to 32", c.VCPUs)
	}
	if c.MemMiB < 1 {
		return configErr("memory %d MiB: must be positive", c.MemMiB)
	}
	if c.MemoryMax < 0 {
		return configErr("memory.max %d: must not be negative", c.MemoryMax)
	}
	if c.Rootfs.Path == "" {
		return configErr("rootfs path is empty")
	}
	if _, err := os.Stat(c.Rootfs.Path); err != nil {
		return configErr("rootfs: %v", err)
	}
	if c.Vsock != nil && c.Vsock.GuestCID < 3 {
		return configErr("vsock guest CID %d: must be at least 3", c.Vsock.GuestCID)
	}
	if c.Net != nil && (c.Net.IfaceID == "" || c.Net.HostDev == "") {
		return configErr("network interface needs an iface id and a host device")
	}
	if c.NetNS != "" {
		if _, err := os.Stat(c.NetNS); err != nil {
			return configErr("network namespace: %v", err)
		}
	}
	return nil
}

// validateBoot checks everything Boot needs.
func (c *Config) validateBoot() error {
	if err := c.validateLimits(); err != nil {
		return err
	}
	if c.Kernel == "" {
		return configErr("kernel path is empty")
	}
	if _, err := os.Stat(c.Kernel); err != nil {
		return configErr("kernel: %v", err)
	}
	return nil
}

func (c *Config) kernelArgs() string {
	if c.KernelArgs == "" {
		return DefaultKernelArgs
	}
	return c.KernelArgs
}

func (c *Config) cpuMax() string {
	if c.CPUMax != "" {
		return c.CPUMax
	}
	return fmt.Sprintf("%d %d", c.VCPUs*cpuPeriod, cpuPeriod)
}

func (c *Config) memoryMax() int64 {
	if c.MemoryMax > 0 {
		return c.MemoryMax
	}
	return int64(c.MemMiB+MemoryOverheadMiB) << 20
}
