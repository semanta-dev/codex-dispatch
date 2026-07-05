package appserver

import "os/exec"

// childController wires the codex app-server child into a platform kill-domain
// and tears down that whole domain (the child and every process it spawned) as
// one unit. It is the cross-platform seam behind reapChild/waitChild:
//
//   - POSIX (posixController): the child is placed in its own process group at
//     spawn (setChildProcAttr), so there is no OS handle to hold — arm/close are
//     no-ops and signal targets the process group via signalGroup.
//   - Windows (jobController): the child is assigned to a Job Object created with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so terminating (or closing the handle
//     to) the job kills the whole subtree, and a broker crash that drops the
//     handle also kills the child — the analog of Linux Setpgid + Pdeathsig.
//
// Implementations are build-tagged; newChildController is defined per platform.
type childController interface {
	// arm enrolls the just-started child (cmd.Process must be valid) into the
	// platform kill-domain. Called once, immediately after cmd.Start().
	arm(cmd *exec.Cmd) error
	// signal terminates the child's whole kill-domain. sig selects graceful vs
	// forceful termination where the platform distinguishes them.
	signal(pid int, sig childSignal) error
	// close releases any OS handle held by the controller (the Job handle on
	// Windows; a no-op on POSIX). Safe to call more than once.
	close()
}
