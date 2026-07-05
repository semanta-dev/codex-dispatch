//go:build !unix && !windows

package appserver

import "os"

type childSignal int

const (
	childSIGTERM childSignal = iota
	childSIGKILL
)

func signalGroup(pid int, sig childSignal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if sig == childSIGKILL {
		return proc.Kill()
	}
	return proc.Signal(os.Interrupt)
}
