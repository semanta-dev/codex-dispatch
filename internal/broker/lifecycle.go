package broker

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// AcquirePIDFile writes the current PID to path. If the file already exists
// and contains a live PID different from os.Getpid(), returns an error. If
// the contained PID is dead, the file is replaced.
//
// Returns a release function that removes the PID file. Safe to call
// multiple times; subsequent calls are no-ops.
func AcquirePIDFile(path string) (func(), error) {
	if existing, ok := readPIDFile(path); ok {
		if existing != os.Getpid() && processLive(existing) {
			return nil, fmt.Errorf("broker already running (PID %d) — PID file at %s", existing, path)
		}
	}

	pidBytes := []byte(strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(path, pidBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write PID file %s: %w", path, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = os.Remove(path)
		})
	}, nil
}

func readPIDFile(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return n, true
}

// processLive returns true when a process with the given PID exists.
// Uses signal 0 (no-op probe) per POSIX.
// EPERM means the process exists but we lack permission — still live.
// ESRCH means the process does not exist — dead.
func processLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM: process exists, we can't signal it (e.g. different user/namespace).
	if errno, ok := err.(syscall.Errno); ok && errno == syscall.EPERM {
		return true
	}
	return false
}

// IdleTimer fires onFire after `timeout` of inactivity. Reset extends the
// deadline; Stop cancels.
type IdleTimer struct {
	timeout time.Duration
	onFire  func()

	mu     sync.Mutex
	timer  *time.Timer
	stopCh chan struct{}
}

// NewIdleTimer constructs an unstarted IdleTimer.
func NewIdleTimer(timeout time.Duration, onFire func()) *IdleTimer {
	return &IdleTimer{timeout: timeout, onFire: onFire, stopCh: make(chan struct{})}
}

// Start arms the timer. Idempotent.
func (t *IdleTimer) Start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		return
	}
	t.timer = time.AfterFunc(t.timeout, t.onFire)
}

// Reset extends the deadline to (now + timeout). No-op if not started.
func (t *IdleTimer) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer == nil {
		return
	}
	t.timer.Reset(t.timeout)
}

// Stop cancels the timer; onFire will not be called.
func (t *IdleTimer) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
	}
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
}
