//go:build !linux

package agent

import "syscall"

func setCgroup(*syscall.SysProcAttr, int) {}
