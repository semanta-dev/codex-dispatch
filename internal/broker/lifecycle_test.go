package broker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquirePIDFileWritesOwnPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broker.pid")
	release, err := AcquirePIDFile(path)
	if err != nil {
		t.Fatalf("AcquirePIDFile: %v", err)
	}
	t.Cleanup(release)

	raw, _ := os.ReadFile(path)
	got, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("PID file content not an int: %q", raw)
	}
	if got != os.Getpid() {
		t.Fatalf("PID = %d, want %d", got, os.Getpid())
	}
}

func TestAcquirePIDFileRefusesWhenAnotherLiveBroker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broker.pid")
	// PID 1 (init/systemd) is always alive on Unix. Use it to simulate
	// "another live broker" (a PID different from ours but provably alive).
	if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := AcquirePIDFile(path)
	if err == nil {
		t.Fatalf("AcquirePIDFile should refuse when a live PID owns the file")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %q, want 'already running'", err.Error())
	}
}

func TestAcquirePIDFileReplacesStalePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broker.pid")
	stale := 999999 // definitely dead
	if err := os.WriteFile(path, []byte(strconv.Itoa(stale)), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	release, err := AcquirePIDFile(path)
	if err != nil {
		t.Fatalf("AcquirePIDFile (stale): %v", err)
	}
	t.Cleanup(release)

	raw, _ := os.ReadFile(path)
	got, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if got != os.Getpid() {
		t.Fatalf("PID = %d, want %d (stale should have been replaced)", got, os.Getpid())
	}
}

func TestAcquirePIDFileReleaseRemovesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broker.pid")
	release, err := AcquirePIDFile(path)
	if err != nil {
		t.Fatalf("AcquirePIDFile: %v", err)
	}
	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("PID file should be removed after release, stat err = %v", err)
	}
}

func TestIdleTimerFiresAfterTimeout(t *testing.T) {
	fired := make(chan struct{}, 1)
	timer := NewIdleTimer(50*time.Millisecond, func() { fired <- struct{}{} })
	timer.Start()
	defer timer.Stop()

	select {
	case <-fired:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("idle timer did not fire within 500ms")
	}
}

func TestIdleTimerResetCancelsFire(t *testing.T) {
	fired := make(chan struct{}, 1)
	timer := NewIdleTimer(80*time.Millisecond, func() { fired <- struct{}{} })
	timer.Start()
	defer timer.Stop()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		timer.Reset()
		time.Sleep(30 * time.Millisecond)
	}
	select {
	case <-fired:
		t.Fatalf("idle timer fired despite continuous resets")
	default:
	}
}

// TestIdleGuardRefusesShutdownWhileTaskNonTerminal models the cmd/broker.go
// idle wiring: the IdleTimer's onFire consults Table.HasNonTerminal() and
// re-arms (Reset) instead of shutting down while any task is non-terminal.
// This is the unit-level guarantee behind "idle-out never kills an active or
// long task". On the OLD wiring (onFire shut down unconditionally) shutdown
// would have fired even with a running task present.
func TestIdleGuardRefusesShutdownWhileTaskNonTerminal(t *testing.T) {
	table := NewTable(1, 8)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	if err := table.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	var shutdownCalls int32
	var idle *IdleTimer
	idle = NewIdleTimer(30*time.Millisecond, func() {
		if table.HasNonTerminal() {
			idle.Reset()
			return
		}
		atomic.AddInt32(&shutdownCalls, 1)
	})
	idle.Start()
	defer idle.Stop()

	// Let the timer fire several times while the task stays running. Each fire
	// must re-arm rather than shut down.
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&shutdownCalls); n != 0 {
		t.Fatalf("shutdown fired %d times while a running task was present, want 0", n)
	}

	// Once the task reaches a terminal state, idle-out is allowed to proceed.
	if err := table.MarkDone(id, 0, "thread-x", false); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&shutdownCalls) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("shutdown did not fire after task became terminal")
}

// TestTableOnActivityResetsIdle proves Table activity (a streamed event)
// invokes the onActivity callback the broker wires to idle.Reset, so a steady
// stream of notifications keeps the broker alive even with a tiny idle timeout.
func TestTableOnActivityResetsIdle(t *testing.T) {
	table := NewTable(1, 8)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})

	var resets int32
	table.SetOnActivity(func() { atomic.AddInt32(&resets, 1) })

	before := atomic.LoadInt32(&resets)
	if _, err := table.AppendEvent(id, "turn/started", []byte(`{}`)); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if got := atomic.LoadInt32(&resets); got <= before {
		t.Fatalf("AppendEvent did not fire onActivity: before=%d after=%d", before, got)
	}
}

// TestDetachedRunnerDrainsOnContextCancel verifies the broker-lifetime runner
// contract: launched goroutines derive their cancellation from the runner's
// Context, and Wait blocks until they finish. This is what lets the broker
// drain in-flight detached runs on shutdown instead of yanking the codex child.
func TestDetachedRunnerDrainsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := NewDetachedRunner(ctx)

	var started, finished sync.WaitGroup
	started.Add(1)
	finished.Add(1)
	runner.Go(func() {
		started.Done()
		<-runner.Context().Done() // mirrors a detached drain observing shutdown
		finished.Done()
	})

	started.Wait()

	waited := make(chan struct{})
	go func() {
		runner.Wait()
		close(waited)
	}()
	// Wait must block while the goroutine is still in flight.
	select {
	case <-waited:
		t.Fatalf("runner.Wait returned before its goroutine finished")
	case <-time.After(50 * time.Millisecond):
	}

	cancel() // broker shutdown: cancel the lifetime ctx
	finished.Wait()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatalf("runner.Wait did not return after goroutines drained")
	}
}

// TestNilDetachedRunnerSafe documents the nil-runner fallback used by
// bare-Table tests: Go still runs the func, Context is Background, Wait is a
// no-op.
func TestNilDetachedRunnerSafe(t *testing.T) {
	var r *DetachedRunner
	if r.Context() == nil {
		t.Fatalf("nil runner Context() must be non-nil")
	}
	done := make(chan struct{})
	r.Go(func() { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("nil runner Go did not run fn")
	}
	r.Wait() // must not panic
}
