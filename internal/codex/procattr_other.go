//go:build !unix && !windows

package codex

import "os/exec"

// setBrokerProcAttr is a no-op on platforms without session/process-group or
// detached-creation controls (e.g. plan9).
func setBrokerProcAttr(cmd *exec.Cmd) {}
