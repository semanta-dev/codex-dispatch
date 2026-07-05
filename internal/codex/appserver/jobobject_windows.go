//go:build windows

package appserver

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows there is no process-group signal, so childSignal is a coarse
// graceful/forceful selector. Both values currently route to a forceful Job
// Object terminate — graceful shutdown is already attempted earlier by closing
// the child's stdin in reapChild before signal is ever called.
type childSignal int

const (
	childSIGTERM childSignal = iota
	childSIGKILL
)

// jobController assigns the codex child to a Windows Job Object created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. TerminateJobObject kills the whole subtree
// (the child and every descendant, since descendants inherit job membership)
// atomically. Because the broker process holds the only handle to the job, the
// OS closing that handle when the broker dies — for any reason, including an
// external TerminateProcess — also fires the kill-on-close and reaps the child.
// This is the Windows analog of Linux Setpgid + Pdeathsig=SIGKILL: no orphaned
// danger-full-access codex child can outlive the broker.
type jobController struct {
	job windows.Handle
}

func newChildController() childController { return &jobController{} }

func (c *jobController) arm(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_SET_QUOTA, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	c.job = job
	return nil
}

func (c *jobController) signal(_ int, _ childSignal) error {
	if c.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(c.job, 1)
}

func (c *jobController) close() {
	if c.job != 0 {
		_ = windows.CloseHandle(c.job)
		c.job = 0
	}
}
