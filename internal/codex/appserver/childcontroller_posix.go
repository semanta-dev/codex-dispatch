//go:build !windows

package appserver

import "os/exec"

// posixController relies on the codex child being in its own process group (set
// at spawn via setChildProcAttr) and terminates the whole group through the
// signalGroup seam (kill(-pid) on unix; a best-effort single-process fallback on
// plan9/other). There is no OS handle to hold, so arm and close are no-ops.
type posixController struct{}

func newChildController() childController { return posixController{} }

func (posixController) arm(*exec.Cmd) error                   { return nil }
func (posixController) signal(pid int, sig childSignal) error { return signalGroup(pid, sig) }
func (posixController) close()                                {}
