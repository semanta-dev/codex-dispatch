//go:build !unix

package appserver

import "os/exec"

// setChildProcAttr (non-unix, e.g. Windows/plan9): no portable process-group or
// death-signal controls, so leave SysProcAttr unset. reapChild handles explicit
// child teardown on Close/shutdown.
func setChildProcAttr(cmd *exec.Cmd) {}
