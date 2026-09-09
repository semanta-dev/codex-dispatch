//go:build !windows

package broker

import (
	"os"
	"syscall"
)

// processLive returns true when a process with the given PID exists.
// Uses signal 0 (no-op probe) per POSIX.
// EPERM means the process exists but we lack permission — still live.
// ESRCH means the process does not exist — dead.
func processLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM: process exists, we can't signal it (e.g. different user/namespace).
	if errno, ok := err.(syscall.Errno); ok && errno == syscall.EPERM {
		return true
	}
	return false
}
