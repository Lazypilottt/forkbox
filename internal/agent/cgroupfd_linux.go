package agent

import "syscall"

// setCgroup starts the process directly in the process-limit cgroup.
func setCgroup(a *syscall.SysProcAttr, fd int) {
	if fd > 0 {
		a.UseCgroupFD = true
		a.CgroupFD = fd
	}
}
