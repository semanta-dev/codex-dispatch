//go:build unix && !linux

package appserver

import (
	"os/exec"
	"syscall"
)

// setChildProcAttr (darwin/BSD): place the codex child in its own process group
// so our signals target the child subtree. Pdeathsig is Linux-only and not
// available here; reapChild's explicit kill on Close/shutdown handles teardown.
func setChildProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
