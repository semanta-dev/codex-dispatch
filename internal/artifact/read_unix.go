//go:build !windows

package artifact

import "syscall"

const nonblockingRead = syscall.O_NONBLOCK
