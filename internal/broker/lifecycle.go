package broker

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AcquirePIDFile writes the current PID to path. If the file already exists
// and contains a live PID different from os.Getpid(), returns an error. If
// the contained PID is dead, the file is replaced.
//
// Returns a release function that removes the PID file. Safe to call
// multiple times; subsequent calls are no-ops.
func AcquirePIDFile(path string) (func(), error) {
	pidBytes := []byte(strconv.Itoa(os.Getpid()))
	for {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, err := file.Write(pidBytes); err == nil {
				err = file.Close()
			} else {
				_ = file.Close()
			}
			if err != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("write PID file %s: %w", path, err)
			}
			break
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create PID file %s: %w", path, err)
		}
		existing, ok := readPIDFile(path)
		if ok && existing != os.Getpid() && processLive(existing) {
			return nil, fmt.Errorf("broker already running (PID %d) — PID file at %s", existing, path)
		}
		// A malformed or stale file cannot represent a live broker. Remove it
		// and retry exclusive creation; a concurrent broker will win the race.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale PID file %s: %w", path, err)
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if current, ok := readPIDFile(path); ok && current == os.Getpid() {
				_ = os.Remove(path)
			}
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
