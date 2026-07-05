//go:build unix

package appserver

import "syscall"

type childSignal syscall.Signal

const (
	childSIGTERM childSignal = childSignal(syscall.SIGTERM)
	childSIGKILL childSignal = childSignal(syscall.SIGKILL)
)

func signalGroup(pid int, sig childSignal) error {
	return syscall.Kill(-pid, syscall.Signal(sig))
}
