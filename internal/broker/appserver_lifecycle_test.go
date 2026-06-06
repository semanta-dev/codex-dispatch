package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/codex/appserver"
)

// TestSyntheticCodexDeathMapsToNon64 ties the app-server fix to the broker's
// exit-code attribution: when the shared codex child dies under an in-flight
// turn, that turn now receives a synthetic *Turn (status "failed", codex-exited
// error) rather than a closed-empty Result. turnToExit must map it to a non-64
// codex-error exit. Against the OLD behavior the turn's Result was closed with no
// value, the drain read nil, and turnToExit(nil) returned exit 64 "codex exited
// without completing turn" — silently mislabelling a peer turn that was draining
// alongside a killed sibling. This asserts that mislabel is gone at the layer
// that decides the exit code.
func TestSyntheticCodexDeathMapsToNon64(t *testing.T) {
	// A turn that was draining when the shared child died (synthesized by the
	// app-server's failPendingRequests path).
	died := &appserver.Turn{
		Status: "failed",
		Error:  &appserver.TurnError{Code: "codex_exited", Message: "codex app-server exited unexpectedly"},
	}
	code, msg := turnToExit(died)
	if code == 64 {
		t.Fatalf("codex-died turn mapped to exit 64 (the silent-mislabel bug); msg=%q", msg)
	}
	if code != 2 {
		t.Fatalf("codex-died turn exit = %d, want 2 (turn failed); msg=%q", code, msg)
	}
	if msg == "" {
		t.Fatal("codex-died turn produced an empty error message")
	}

	// Sanity: a genuinely nil turn (no signal at all) still maps to 64, so we
	// have not flattened the distinction the other way.
	if code, _ := turnToExit(nil); code != 64 {
		t.Fatalf("nil turn exit = %d, want 64", code)
	}
}

// TestEnsureAppServerRecyclesDeadInstance verifies the recycle path: once the
// singleton is dead, EnsureAppServer detaches it (reaping it off the lock) and
// spawns a FRESH live instance, returning a different *AppServer. Run under
// -race this also exercises the background reap of the dead instance racing the
// fresh spawn without touching the shared lock for the multi-second child wait.
func TestEnsureAppServerRecyclesDeadInstance(t *testing.T) {
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "thr-recycle",
	})
	repoDir := t.TempDir()
	state := &BrokerState{Table: NewTable(8, 2048), CWD: repoDir}
	t.Cleanup(func() { state.CloseAppServer(context.Background()) })

	ctx := context.Background()
	first, err := state.EnsureAppServer(ctx)
	if err != nil {
		t.Fatalf("EnsureAppServer #1: %v", err)
	}
	if first == nil {
		t.Fatal("EnsureAppServer #1 returned nil")
	}

	// Kill the child so the next EnsureAppServer takes the dead-instance branch.
	_ = first.Close(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for !first.IsDead() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !first.IsDead() {
		t.Fatal("first instance never became dead after Close")
	}

	second, err := state.EnsureAppServer(ctx)
	if err != nil {
		t.Fatalf("EnsureAppServer #2 (recycle): %v", err)
	}
	if second == nil {
		t.Fatal("EnsureAppServer #2 returned nil")
	}
	if second == first {
		t.Fatal("EnsureAppServer reused the dead instance instead of recycling")
	}
	if second.IsDead() {
		t.Fatal("recycled instance is already dead")
	}
}

// TestConcurrentEnsureAppServerSingleInstance asserts that many concurrent
// callers of EnsureAppServer converge on ONE live instance (the appserverMu +
// IsDead fast path), never double-spawning. Race-clean under -race.
func TestConcurrentEnsureAppServerSingleInstance(t *testing.T) {
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "thr-shared",
	})
	repoDir := t.TempDir()
	state := &BrokerState{Table: NewTable(8, 2048), CWD: repoDir}
	t.Cleanup(func() { state.CloseAppServer(context.Background()) })

	const n = 16
	got := make(chan *appserver.AppServer, n)
	errc := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			a, err := state.EnsureAppServer(context.Background())
			if err != nil {
				errc <- err
				return
			}
			got <- a
		}()
	}
	close(start)

	var first *appserver.AppServer
	for i := 0; i < n; i++ {
		select {
		case err := <-errc:
			t.Fatalf("EnsureAppServer: %v", err)
		case a := <-got:
			if first == nil {
				first = a
			} else if a != first {
				t.Fatal("concurrent EnsureAppServer returned distinct instances (double spawn)")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("EnsureAppServer goroutines did not all return")
		}
	}
}

// TestRecycleNoGoroutineGrowth runs many spawn → die → recycle cycles through
// EnsureAppServer and asserts the broker-layer goroutine count does not grow
// without bound. Each cycle spawns a fake codex, kills it (Close → child EOF →
// reader exits), then EnsureAppServer detaches the dead instance, reaps it in
// the background, and spawns a fresh one. A leak in the reaper, the per-instance
// reader, or the spawn path would show as monotonically rising NumGoroutine.
func TestRecycleNoGoroutineGrowth(t *testing.T) {
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "thr-stress",
	})
	repoDir := t.TempDir()
	state := &BrokerState{Table: NewTable(8, 2048), CWD: repoDir}
	t.Cleanup(func() { state.CloseAppServer(context.Background()) })

	cycle := func() {
		a, err := state.EnsureAppServer(context.Background())
		if err != nil {
			t.Fatalf("EnsureAppServer: %v", err)
		}
		// Kill this instance so the NEXT EnsureAppServer recycles it.
		_ = a.Close(context.Background())
		deadline := time.Now().Add(2 * time.Second)
		for !a.IsDead() && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Warm up so one-time lazily-initialised goroutines (and an in-flight reap)
	// don't read as growth, then sample the baseline.
	cycle()
	cycle()
	// Drain any pending background reap from the warm-up before sampling.
	state.CloseAppServer(context.Background())
	runtime.GC()
	base := runtime.NumGoroutine()

	const cycles = 40
	for i := 0; i < cycles; i++ {
		cycle()
	}
	// Settle any outstanding background reaps from the loop's recycles.
	state.CloseAppServer(context.Background())
	waitGoroutinesSettle(t, base, 5*time.Second)
}

// TestConcurrentDispatchSharedAppServerNoCrossTurnCorruption is the headline
// INTEGRATION test for the shared app-server: several synchronous dispatch.run
// calls run CONCURRENTLY through ONE broker (cap=2) and converge on a single
// shared EnsureAppServer instance. It asserts that the broker's per-task
// bookkeeping — task records, event rings, and terminal results — never crosses
// streams even though every turn flows through the SAME shared reader, and that
// the whole thing is race-clean.
//
// Concurrency model / fixture constraint: the app-server allows only one
// in-flight turn per thread id ("turn already in flight"), and the fake
// app-server (a separate test module, not editable from this packet) tracks the
// active thread in a single process-global field — so two genuinely overlapping
// turns on DISTINCT threads cannot be modelled faithfully here. We instead submit
// many dispatches CONCURRENTLY against one shared app-server and let the broker's
// run slot serialise their turns (cap=1): every dispatch races to acquire the
// single slot, runs its turn on the reused shared child, and frees the slot for
// the next. The shared reader routes each turn's notifications by thread id, and
// the broker must keep each concurrent dispatch's task/ring/result independent
// while reusing one app-server across all of them. That is exactly the
// cross-turn-corruption surface this test guards.
//
// Against pre-P09 behavior (synchronous Close of a dead/shared instance under
// appserverMu, reader-vs-Close races) this concurrent shared-instance traffic is
// what flushed out the data races; it must now be race-clean and corruption-free.
func TestConcurrentDispatchSharedAppServerNoCrossTurnCorruption(t *testing.T) {
	repoDir := t.TempDir()
	// No per-turn deadline: each turn completes on its own. No turn delay either,
	// so the fake handles each turn atomically and the shared single thread id
	// stays consistent across its serialised turns.
	t.Setenv("CODEX_BROKER_TURN_TIMEOUT_MS", "")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "thread-shared-concurrent",
	})

	// cap=1 so the run slot serialises turns on the single shared thread: all
	// dispatches are SUBMITTED concurrently and contend for the slot + the shared
	// app-server, but only one turn is ever in flight (matching the per-thread
	// single-turn constraint). The broker reuses one app-server across all of them.
	state := &BrokerState{Table: NewTable(1, 2048), CWD: repoDir}
	addr := startTestBroker(t, state)

	const n = 4
	type runResult struct {
		idx int
		res *DispatchRunResult
		err error
	}
	results := make(chan runResult, n)
	begin := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(idx int) {
			client, derr := Dial(addr)
			if derr != nil {
				results <- runResult{idx: idx, err: derr}
				return
			}
			<-begin // release all dispatchers at once to maximise contention
			res, rerr := client.DispatchRun(context.Background(), DispatchRunParams{
				SessionID: fmt.Sprintf("s%d", idx),
				Mode:      "fresh",
				Prompt:    fmt.Sprintf("prompt-%d", idx),
				Sandbox:   "workspace-write",
				LogPath:   filepath.Join(repoDir, fmt.Sprintf("t%d.log", idx)),
			}, nil)
			results <- runResult{idx: idx, res: res, err: rerr}
		}(i)
	}
	close(begin)

	got := make(map[int]*DispatchRunResult, n)
	taskIDs := make(map[string]int, n)
	for i := 0; i < n; i++ {
		select {
		case rr := <-results:
			if rr.err != nil {
				t.Fatalf("dispatch %d returned error: %v", rr.idx, rr.err)
			}
			if rr.res.State != string(StateDone) {
				t.Fatalf("dispatch %d final state = %q (exit=%d msg=%q), want done",
					rr.idx, rr.res.State, rr.res.ExitCode, rr.res.ErrorMessage)
			}
			if rr.res.ExitCode != 0 {
				t.Fatalf("dispatch %d exit_code = %d, want 0", rr.idx, rr.res.ExitCode)
			}
			got[rr.idx] = rr.res
			taskIDs[rr.res.TaskID] = rr.idx
		case <-time.After(15 * time.Second):
			t.Fatalf("concurrent dispatches did not all return (deadlock on the shared reader?)")
		}
	}

	// Distinct task ids: the concurrent dispatches must not collapse onto a single
	// task record despite sharing one app-server and one thread.
	if len(taskIDs) != n {
		t.Fatalf("concurrent dispatches produced %d distinct task ids, want %d: %v", len(taskIDs), n, got)
	}

	// Exactly ONE live app-server backed every turn (shared EnsureAppServer), and
	// it must still be the single live instance — not double-spawned per dispatch.
	live, err := state.EnsureAppServer(context.Background())
	if err != nil {
		t.Fatalf("EnsureAppServer (post): %v", err)
	}
	if live.IsDead() {
		t.Fatalf("shared app-server is dead after %d clean concurrent turns", n)
	}

	// No cross-turn corruption in the event rings: each task's ring carries its
	// OWN thread/started (stamped with its own task_id by the dispatch handler)
	// and its OWN turn lifecycle, and never an event belonging to a peer task.
	for taskID, idx := range taskIDs {
		evs, eerr := state.Table.Events(taskID, 1)
		if eerr != nil {
			t.Fatalf("Events(task %d=%s): %v", idx, taskID, eerr)
		}
		var sawThreadStarted, sawFinished, sawCancelled, sawErrored bool
		for _, ev := range evs {
			// Any payload that carries a task_id must carry THIS task's id; a peer
			// task's id appearing here is direct evidence of cross-turn corruption.
			if pid := taskIDFromPayload(ev.Payload); pid != "" && pid != taskID {
				t.Fatalf("task %d (%s) ring contains an event stamped with peer task_id %q: %s",
					idx, taskID, pid, ev.Payload)
			}
			switch ev.Type {
			case "thread/started":
				sawThreadStarted = true
			case "task.finished":
				sawFinished = true
			case "task.cancelled":
				sawCancelled = true
			case "task.errored":
				sawErrored = true
			}
		}
		if !sawThreadStarted {
			t.Fatalf("task %d (%s) ring missing its own thread/started: %+v", idx, taskID, evs)
		}
		if !sawFinished {
			t.Fatalf("task %d (%s) ring missing task.finished (clean completion): %+v", idx, taskID, evs)
		}
		if sawCancelled || sawErrored {
			t.Fatalf("task %d (%s) ring has a cancelled/errored event for a clean run: %+v", idx, taskID, evs)
		}
	}
}

// taskIDFromPayload extracts the "task_id" field from an event payload if
// present, returning "" otherwise. Used to detect cross-turn event-ring
// corruption: an event in task A's ring must never carry task B's id.
func taskIDFromPayload(payload []byte) string {
	var p struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.TaskID
}

// waitGoroutinesSettle waits until NumGoroutine settles at or below base (+ a
// small slack), failing if it does not within the timeout. Mirrors the appserver
// package helper; duplicated here because it is unexported there.
func waitGoroutinesSettle(t *testing.T, base int, timeout time.Duration) {
	t.Helper()
	const slack = 6
	deadline := time.Now().Add(timeout)
	for {
		runtime.GC()
		n := runtime.NumGoroutine()
		if n <= base+slack {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count did not settle: base=%d now=%d (recycle leak suspected)", base, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
