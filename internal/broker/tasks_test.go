package broker

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTaskStartReturnsID(t *testing.T) {
	table := NewTable(4 /* concurrency cap */, 2048 /* ring */)
	id, queued := table.Start("sess-a", TaskParams{Mode: "fresh", Prompt: "do thing"})
	if id == "" {
		t.Fatalf("Start returned empty task_id")
	}
	if queued {
		t.Fatalf("first task on an empty table should not be queued")
	}
}

func TestTaskStartQueuesBeyondCap(t *testing.T) {
	table := NewTable(2, 2048)
	a, _ := table.Start("s", TaskParams{Mode: "fresh"})
	b, _ := table.Start("s", TaskParams{Mode: "fresh"})
	// Mark both as running so the third hits the cap.
	if err := table.MarkRunning(a); err != nil {
		t.Fatalf("MarkRunning a: %v", err)
	}
	if err := table.MarkRunning(b); err != nil {
		t.Fatalf("MarkRunning b: %v", err)
	}

	c, queued := table.Start("s", TaskParams{Mode: "fresh"})
	if c == "" || !queued {
		t.Fatalf("third task should be queued (id=%q queued=%v)", c, queued)
	}
}

func TestTaskStartQueuesWhenEarlierTasksAreStillQueued(t *testing.T) {
	table := NewTable(1, 2048)
	first, queued := table.Start("s", TaskParams{Mode: "fresh"})
	if first == "" || queued {
		t.Fatalf("first task should not be queued (id=%q queued=%v)", first, queued)
	}
	second, queued := table.Start("s", TaskParams{Mode: "fresh"})
	if second == "" || !queued {
		t.Fatalf("second task should be queued behind first pending task (id=%q queued=%v)", second, queued)
	}
}

func TestStateMachineLegalTransitions(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})

	if got, _ := table.Status(id); got.State != StateQueued {
		t.Fatalf("state = %s, want queued", got.State)
	}
	if err := table.MarkRunning(id); err != nil {
		t.Fatalf("queued → running: %v", err)
	}
	if got, _ := table.Status(id); got.State != StateRunning {
		t.Fatalf("state = %s, want running", got.State)
	}
	if err := table.MarkDone(id, 0, "thread-1", false); err != nil {
		t.Fatalf("running → done: %v", err)
	}
	if got, _ := table.Status(id); got.State != StateDone {
		t.Fatalf("state = %s, want done", got.State)
	}
}

func TestStateMachineRejectsIllegalTransitions(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	if err := table.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := table.MarkDone(id, 0, "t", false); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	// Now in terminal state. Cancelling should return ErrTaskAlreadyTerminal.
	err := table.Cancel(id)
	if err == nil {
		t.Fatalf("Cancel on terminal task should fail")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %q, want terminal-state language", err.Error())
	}
}

func TestCancelQueuedGoesStraightToCancelled(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	if err := table.Cancel(id); err != nil {
		t.Fatalf("Cancel queued: %v", err)
	}
	got, _ := table.Status(id)
	if got.State != StateCancelled {
		t.Fatalf("state = %s, want cancelled", got.State)
	}
}

func TestListFiltersBySession(t *testing.T) {
	table := NewTable(8, 2048)
	a, _ := table.Start("alpha", TaskParams{Mode: "fresh"})
	b, _ := table.Start("beta", TaskParams{Mode: "fresh"})
	_, _ = table.Start("alpha", TaskParams{Mode: "fresh"})

	alphaList := table.List("alpha")
	if len(alphaList) != 2 {
		t.Fatalf("alpha tasks = %d, want 2", len(alphaList))
	}
	betaList := table.List("beta")
	if len(betaList) != 1 || betaList[0].ID != b {
		t.Fatalf("beta tasks = %v, want one with id %s", betaList, b)
	}
	// Empty filter returns everything.
	all := table.List("")
	if len(all) != 3 {
		t.Fatalf("all tasks = %d, want 3", len(all))
	}
	// Stable order: keep alpha[0] = a.
	if alphaList[0].ID != a {
		t.Fatalf("alpha[0] = %s, want %s (insertion order)", alphaList[0].ID, a)
	}
}

func TestAppendEventMonotonicSeq(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(id)

	for i := 0; i < 5; i++ {
		seq, err := table.AppendEvent(id, "codex.turn.completed", []byte(`{"n":1}`))
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		if seq != int64(i+1) {
			t.Fatalf("seq = %d, want %d", seq, i+1)
		}
	}
}

func TestRingBufferEvictsOldEvents(t *testing.T) {
	table := NewTable(8, 3 /* ring size = 3 */)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(id)

	for i := 0; i < 10; i++ {
		_, err := table.AppendEvent(id, "codex.x", []byte(`{}`))
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// Replay from seq=8 should return events 8, 9, 10.
	events, err := table.Events(id, 8)
	if err != nil {
		t.Fatalf("Events(8): %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	if events[0].Seq != 8 {
		t.Fatalf("first seq = %d, want 8", events[0].Seq)
	}

	// Replay from seq=1 should return ErrEventsLost (eviction).
	_, err = table.Events(id, 1)
	if err == nil {
		t.Fatalf("Events(1) should fail with ErrEventsLost (events 1-7 evicted)")
	}
}

func TestTaskNotFound(t *testing.T) {
	table := NewTable(8, 2048)
	_, err := table.Status("does-not-exist")
	if err == nil {
		t.Fatalf("Status on unknown id should fail")
	}
	if _, err := table.AppendEvent("does-not-exist", "x", nil); err == nil {
		t.Fatalf("AppendEvent on unknown id should fail")
	}
}

func TestRingBufferBoundaryExact(t *testing.T) {
	table := NewTable(8, 3 /* ring size = 3 */)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(id)

	// Append 10 events. oldest = 10 - 3 + 1 = 8.
	for i := 0; i < 10; i++ {
		_, err := table.AppendEvent(id, "codex.x", []byte(`{}`))
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// sinceSeq = oldest (8) must succeed and return events 8, 9, 10.
	events, err := table.Events(id, 8)
	if err != nil {
		t.Fatalf("Events(8) should succeed: %v", err)
	}
	if len(events) != 3 || events[0].Seq != 8 {
		t.Fatalf("Events(8) = %d items first=%d, want 3 items first=8", len(events), events[0].Seq)
	}

	// sinceSeq = oldest - 1 (7) must return ErrEventsLost.
	_, err = table.Events(id, 7)
	if err == nil {
		t.Fatalf("Events(7) should return ErrEventsLost (7 is below oldest=8)")
	}
	if !errors.Is(err, ErrEventsLost) {
		t.Fatalf("err = %v, want ErrEventsLost", err)
	}
}

func TestEventsSinceLatestSeqIsInclusive(t *testing.T) {
	table := NewTable(8, 3)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(id)

	for i := 0; i < 3; i++ {
		if _, err := table.AppendEvent(id, "codex.x", []byte(`{}`)); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	events, err := table.Events(id, 3)
	if err != nil {
		t.Fatalf("Events(3): %v", err)
	}
	if len(events) != 1 || events[0].Seq != 3 {
		t.Fatalf("Events(3) = %+v, want exactly seq 3", events)
	}

	events, err = table.Events(id, 4)
	if err != nil {
		t.Fatalf("Events(4): %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Events(4) = %+v, want no events past latest", events)
	}
}

// drainToTerminal is a helper that starts, runs, and marks a task done so it is
// terminal — used by the eviction tests.
func runOneDone(t *testing.T, table *Table, session string) string {
	t.Helper()
	id, _ := table.Start(session, TaskParams{Mode: "fresh"})
	if err := table.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := table.MarkDone(id, 0, "thread-"+id, false); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	return id
}

// TestTerminalTasksEvictedUnderCap verifies the table does not grow unbounded:
// with a small terminal cap, the oldest terminal tasks are dropped (table +
// their event rings) as new tasks complete. Against the OLD behavior (no
// eviction) the table retained every task forever and len(List("")) would grow
// without bound — this test fails there.
func TestTerminalTasksEvictedUnderCap(t *testing.T) {
	table := NewTable(8, 2048)
	table.SetEvictionPolicy(3 /* keep at most 3 terminal */, 0)

	var ids []string
	for i := 0; i < 10; i++ {
		ids = append(ids, runOneDone(t, table, "s"))
	}

	all := table.List("")
	if len(all) != 3 {
		t.Fatalf("table size = %d, want 3 (eviction cap)", len(all))
	}
	// The three most-recent tasks must survive; the oldest must be evicted.
	for _, id := range ids[:7] {
		if _, err := table.Status(id); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("old task %s should be evicted, got err=%v", id, err)
		}
	}
	for _, id := range ids[7:] {
		if _, err := table.Status(id); err != nil {
			t.Fatalf("recent task %s should survive, got err=%v", id, err)
		}
	}
	// order must be compacted in lockstep with tasks (no dangling ids).
	table.mu.Lock()
	if len(table.order) != len(table.tasks) {
		table.mu.Unlock()
		t.Fatalf("order=%d tasks=%d, want equal after eviction", len(table.order), len(table.tasks))
	}
	table.mu.Unlock()
}

// TestEvictionNeverDropsNonTerminal verifies a running/queued task is never
// reaped out from under its drain even when terminal tasks pile up over the cap.
func TestEvictionNeverDropsNonTerminal(t *testing.T) {
	table := NewTable(8, 2048)
	table.SetEvictionPolicy(1, 0)

	// One running task that must never be evicted.
	running, _ := table.Start("s", TaskParams{Mode: "fresh"})
	if err := table.MarkRunning(running); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	// One queued task that must never be evicted.
	queued, _ := table.Start("s", TaskParams{Mode: "fresh"})

	// Pile up terminal tasks well over the cap of 1.
	for i := 0; i < 5; i++ {
		runOneDone(t, table, "s")
	}

	if st, err := table.Status(running); err != nil || st.State != StateRunning {
		t.Fatalf("running task evicted or wrong state: st=%+v err=%v", st, err)
	}
	if st, err := table.Status(queued); err != nil || st.State != StateQueued {
		t.Fatalf("queued task evicted or wrong state: st=%+v err=%v", st, err)
	}
}

// TestTerminalTasksEvictedUnderTTL verifies TTL eviction: a terminal task whose
// FinishedAt is older than the TTL is dropped at the next mutation. Uses an
// injected clock so the test is deterministic and fast.
func TestTerminalTasksEvictedUnderTTL(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	table := NewTable(8, 2048)
	table.setClock(func() time.Time { return now })
	table.SetEvictionPolicy(0 /* no cap */, 50*time.Millisecond)

	old := runOneDone(t, table, "s")

	// Advance the clock past the TTL, then trigger a mutation (a new Start runs
	// eviction). The aged-out terminal task must be gone.
	now = now.Add(time.Second)
	fresh := runOneDone(t, table, "s")

	if _, err := table.Status(old); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("TTL-expired task %s should be evicted, got err=%v", old, err)
	}
	if _, err := table.Status(fresh); err != nil {
		t.Fatalf("fresh task %s should survive TTL eviction, got err=%v", fresh, err)
	}
}

// TestEvictionDisabledByDefaultEnvZero verifies a cap of 0 disables eviction so
// the table retains everything (the unbounded opt-out path).
func TestEvictionDisabledWhenCapZero(t *testing.T) {
	table := NewTable(8, 2048)
	table.SetEvictionPolicy(0, 0)
	for i := 0; i < 50; i++ {
		runOneDone(t, table, "s")
	}
	if got := len(table.List("")); got != 50 {
		t.Fatalf("table size = %d, want 50 (eviction disabled)", got)
	}
}

// TestMarkErroredRetainsReasonInStatus verifies a failed task's broker-side
// error reason is retained on the task and surfaced via Status (the snapshot the
// task.status handler serializes). Against a MarkErrored that dropped the reason
// the ErrorMessage would be empty.
func TestMarkErroredRetainsReasonInStatus(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	if err := table.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	const reason = "ensure codex app-server: spawn failed"
	if err := table.MarkErrored(id, 64, reason); err != nil {
		t.Fatalf("MarkErrored: %v", err)
	}
	st, err := table.Status(id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StateErrored {
		t.Fatalf("state = %s, want errored", st.State)
	}
	if st.ErrorMessage != reason {
		t.Fatalf("ErrorMessage = %q, want %q", st.ErrorMessage, reason)
	}
	if st.ExitCode != 64 {
		t.Fatalf("ExitCode = %d, want 64", st.ExitCode)
	}
}

// TestEventReplayBeyondRingSize exercises the documented event-loss/replay
// contract across a ring larger than 2048 events: events within the most-recent
// ringSize window replay, while a sinceSeq older than the window returns
// ErrEventsLost (ring overflow), directing callers to stdout.log.
func TestEventReplayBeyondRingSize(t *testing.T) {
	const ring = 2048
	table := NewTable(8, ring)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	if err := table.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	const total = ring + 100 // > 2048: forces overflow
	for i := 0; i < total; i++ {
		if _, err := table.AppendEvent(id, "codex.x", []byte(`{}`)); err != nil {
			t.Fatalf("AppendEvent #%d: %v", i, err)
		}
	}

	// oldest still in the ring = total - ring + 1.
	oldest := int64(total - ring + 1)

	// Replaying from oldest succeeds and returns exactly the window.
	evs, err := table.Events(id, oldest)
	if err != nil {
		t.Fatalf("Events(oldest=%d): %v", oldest, err)
	}
	if len(evs) != ring {
		t.Fatalf("replay window = %d events, want %d", len(evs), ring)
	}
	if evs[0].Seq != oldest {
		t.Fatalf("first replayed seq = %d, want %d", evs[0].Seq, oldest)
	}
	if evs[len(evs)-1].Seq != int64(total) {
		t.Fatalf("last replayed seq = %d, want %d", evs[len(evs)-1].Seq, total)
	}

	// Replaying from before the window (seq=1, evicted) returns ErrEventsLost.
	if _, err := table.Events(id, 1); !errors.Is(err, ErrEventsLost) {
		t.Fatalf("Events(1) err = %v, want ErrEventsLost after %d>%d events", err, total, ring)
	}
}

// TestStartQueuedFlagReconcilesWithSemaphore verifies the queued flag returned
// by Start predicts the run-slot semaphore's decision. Every non-terminal task
// either holds a slot (running) or is waiting for one (queued), so a new task is
// queued exactly when cap contenders are already outstanding. Draining tasks to
// a TERMINAL state frees the slots they accounted for, and the next Start then
// reports not-queued — matching what acquireRunSlot would observe.
func TestStartQueuedFlagReconcilesWithSemaphore(t *testing.T) {
	table := NewTable(2, 2048)

	a, qa := table.Start("s", TaskParams{Mode: "fresh"})
	if qa {
		t.Fatalf("task a should not be queued (0/2 contenders)")
	}
	b, qb := table.Start("s", TaskParams{Mode: "fresh"})
	if qb {
		t.Fatalf("task b should not be queued (1/2 contenders)")
	}
	_, qc := table.Start("s", TaskParams{Mode: "fresh"})
	if !qc {
		t.Fatalf("task c should be queued (2/2 contenders outstanding)")
	}

	// Drain BOTH outstanding contenders (a, b) to terminal so the cap's worth of
	// slots is genuinely free. Only then should a new Start report not-queued —
	// a freed slot still occupied by an earlier queued contender does not.
	for _, id := range []string{a, b} {
		if err := table.MarkRunning(id); err != nil {
			t.Fatalf("MarkRunning %s: %v", id, err)
		}
		if err := table.MarkDone(id, 0, "t", false); err != nil {
			t.Fatalf("MarkDone %s: %v", id, err)
		}
	}

	// c is still queued (1 contender). A new d: contenders = c only = 1 < 2.
	_, qd := table.Start("s", TaskParams{Mode: "fresh"})
	if qd {
		t.Fatalf("task d should not be queued: only 1 contender (c) outstanding < cap 2")
	}
}

// TestEvictionConcurrentSafe stresses the eviction path under concurrent
// Start/MarkDone/Status/List to catch races (run under -race).
func TestEvictionConcurrentSafe(t *testing.T) {
	table := NewTable(16, 64)
	table.SetEvictionPolicy(8, 0)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id, _ := table.Start(fmt.Sprintf("s%d", w), TaskParams{Mode: "fresh"})
				_ = table.MarkRunning(id)
				_, _ = table.AppendEvent(id, "codex.x", []byte(`{}`))
				_ = table.MarkDone(id, 0, "t", false)
				_, _ = table.Status(id)
				_ = table.List("")
			}
		}(w)
	}
	wg.Wait()

	// Cap holds after the storm.
	if got := len(table.List("")); got > 8 {
		t.Fatalf("terminal tasks = %d, want <= 8 (cap)", got)
	}
}
