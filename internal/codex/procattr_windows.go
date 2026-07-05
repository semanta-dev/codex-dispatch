//go:build windows

package codex

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

// setBrokerProcAttr is the Windows analog of Setsid: DETACHED_PROCESS gives the
// spawned broker daemon no controlling console (so it survives the parent CLI
// exiting and never pops a console window), and CREATE_NEW_PROCESS_GROUP isolates
// it from Ctrl-C/Ctrl-Break delivered to the parent's group. HideWindow is
// belt-and-suspenders for any child that would otherwise create a window.
func setBrokerProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
