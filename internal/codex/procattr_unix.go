//go:build unix

package codex

import (
	"os/exec"
	"syscall"
)

// setBrokerProcAttr detaches the spawned broker into its own session (Setsid) so
// it survives the launching dispatch process' teardown and is not killed by a
// signal delivered to the launcher's process group.
func setBrokerProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
