//go:build linux

package appserver

import (
	"os/exec"
	"syscall"
)

// setChildProcAttr (Linux): die-with-broker via Pdeathsig=SIGKILL — when the
// broker dies (crash/SIGKILL included) the kernel kills the codex child too —
// plus its own process group (Setpgid) so signals target the child subtree, not
// the broker's group. Closes the orphaned-danger-full-access-child hole.
func setChildProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}
