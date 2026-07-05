package broker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTaskListAll(t *testing.T) {
	table := NewTable(8, 2048)
	_, _ = table.Start("sess-a", TaskParams{Mode: "fresh"})
	_, _ = table.Start("sess-b", TaskParams{Mode: "fresh"})

	h := HandleTaskList(&BrokerState{Table: table})
	out, err := h(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("HandleTaskList: %v", err)
	}
	m := out.(map[string]any)
	tasks := m["tasks"].([]map[string]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
}

func TestTaskListFiltersBySession(t *testing.T) {
	table := NewTable(8, 2048)
	a, _ := table.Start("sess-a", TaskParams{Mode: "fresh"})
	_, _ = table.Start("sess-b", TaskParams{Mode: "fresh"})

	h := HandleTaskList(&BrokerState{Table: table})
	out, err := h(context.Background(), []byte(`{"session_id":"sess-a"}`))
	if err != nil {
		t.Fatalf("HandleTaskList: %v", err)
	}
	tasks := out.(map[string]any)["tasks"].([]map[string]any)
	if len(tasks) != 1 || tasks[0]["task_id"] != a {
		t.Fatalf("tasks = %v, want one with id %s", tasks, a)
	}
}

func TestTaskStatusReturnsState(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})

	h := HandleTaskStatus(&BrokerState{Table: table})
	raw, _ := json.Marshal(map[string]any{"task_id": id})
	out, err := h(context.Background(), raw)
	if err != nil {
		t.Fatalf("HandleTaskStatus: %v", err)
	}
	m := out.(map[string]any)
	if m["state"] != "queued" {
		t.Fatalf("state = %v, want queued", m["state"])
	}
}

func TestTaskStatusReturnsTaskNotFound(t *testing.T) {
	h := HandleTaskStatus(&BrokerState{Table: NewTable(8, 2048)})
	_, err := h(context.Background(), []byte(`{"task_id":"nope"}`))
	if err == nil {
		t.Fatalf("expected error")
	}
	rpcErr := ToRPCError(err)
	if rpcErr == nil || rpcErr.Code != -32001 {
		t.Fatalf("expected -32001 TaskNotFound, got %v", err)
	}
}

func TestTaskCancelTransitionsState(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})

	h := HandleTaskCancel(&BrokerState{Table: table})
	raw, _ := json.Marshal(map[string]any{"task_id": id})
	out, err := h(context.Background(), raw)
	if err != nil {
		t.Fatalf("HandleTaskCancel: %v", err)
	}
	if !strings.Contains(string(mustJSON(t, out)), `"ok":true`) {
		t.Fatalf("response: %s", mustJSON(t, out))
	}
	st, _ := table.Status(id)
	if st.State != StateCancelled {
		t.Fatalf("state = %s, want cancelled", st.State)
	}
}

func TestTaskCancelTerminalErrorsWithRPCCode(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(id)
	_ = table.MarkDone(id, 0, "thread-x", false)

	h := HandleTaskCancel(&BrokerState{Table: table})
	raw, _ := json.Marshal(map[string]any{"task_id": id})
	_, err := h(context.Background(), raw)
	if err == nil {
		t.Fatalf("expected ErrTaskAlreadyTerminal")
	}
	rpcErr := ToRPCError(err)
	if rpcErr == nil || rpcErr.Code != -32007 {
		t.Fatalf("expected -32007 TaskAlreadyTerminal, got %v", err)
	}
}

func TestBrokerStateRunSlotHonorsConcurrencyCap(t *testing.T) {
	state := &BrokerState{Table: NewTable(1, 2048)}
	if err := state.acquireRunSlot(context.Background()); err != nil {
		t.Fatalf("first acquireRunSlot: %v", err)
	}
	acquired := make(chan error, 1)
	go func() {
		acquired <- state.acquireRunSlot(context.Background())
	}()
	select {
	case err := <-acquired:
		t.Fatalf("second acquireRunSlot completed before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	state.releaseRunSlot()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("second acquireRunSlot after release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("second acquireRunSlot did not complete after release")
	}
	state.releaseRunSlot()
}

func TestBrokerStateRunSlotCanBeCancelledWhileQueued(t *testing.T) {
	state := &BrokerState{Table: NewTable(1, 2048)}
	if err := state.acquireRunSlot(context.Background()); err != nil {
		t.Fatalf("first acquireRunSlot: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	acquired := make(chan error, 1)
	go func() {
		acquired <- state.acquireRunSlot(ctx)
	}()
	select {
	case err := <-acquired:
		t.Fatalf("second acquireRunSlot completed before cancel: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-acquired:
		if err == nil {
			t.Fatalf("second acquireRunSlot succeeded after cancel")
		}
	case <-time.After(time.Second):
		t.Fatalf("second acquireRunSlot did not unblock after cancel")
	}
	state.releaseRunSlot()
}

// TestTaskStatusExposesErrorMessage verifies the detached observable contract:
// a broker-side failure reason recorded via MarkErrored is surfaced through
// task.status as error_message. On the OLD code MarkErrored dropped the reason
// and task.status had no error_message field, so a failed detached run (which
// writes no result.json) carried no machine-readable error. This backs the
// documented reduced --status shape in the README.
func TestTaskStatusExposesErrorMessage(t *testing.T) {
	table := NewTable(8, 2048)
	id, _ := table.Start("s", TaskParams{Mode: "fresh"})
	_ = table.MarkRunning(id)
	const reason = "ensure codex app-server: spawn failed"
	if err := table.MarkErrored(id, 64, reason); err != nil {
		t.Fatalf("MarkErrored: %v", err)
	}

	h := HandleTaskStatus(&BrokerState{Table: table})
	raw, _ := json.Marshal(map[string]any{"task_id": id})
	out, err := h(context.Background(), raw)
	if err != nil {
		t.Fatalf("HandleTaskStatus: %v", err)
	}
	m := out.(map[string]any)
	if m["state"] != string(StateErrored) {
		t.Fatalf("state = %v, want errored", m["state"])
	}
	if m["error_message"] != reason {
		t.Fatalf("error_message = %v, want %q", m["error_message"], reason)
	}
	if m["exit_code"] != 64 {
		t.Fatalf("exit_code = %v, want 64", m["exit_code"])
	}
}

// startGuardedBroker spins a real loopback broker wired exactly like
// cmd/codex-dispatch/broker.go: a detached runner bound to the serve ctx, an
// idle timer whose onFire consults HasNonTerminal and re-arms while work is
// outstanding, Table.SetOnActivity → idle.Reset, and a shutdown that cancels →
// drains detached → closes the codex child (child closed LAST). Returns the
// dial address and a shutdown func the test can invoke directly. idleTimeout
// drives the guard under test. The codex child is always reaped on cleanup.
func startGuardedBroker(t *testing.T, state *BrokerState, idleTimeout time.Duration) (string, func()) {
	t.Helper()
	srv := NewServer("127.0.0.1:0")
	addrPath := filepath.Join(t.TempDir(), "broker.addr")
	srv.SetAddrFile(addrPath)
	srv.HandleFunc("broker.ping", HandleBrokerPing(state))
	srv.HandleFunc("broker.shutdown", HandleBrokerShutdown(state))
	srv.HandleFunc("task.list", HandleTaskList(state))
	srv.HandleFunc("task.status", HandleTaskStatus(state))
	srv.HandleFunc("task.cancel", HandleTaskCancel(state))
	srv.HandleFunc("task.start", HandleTaskStart(state))
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))

	ctx, cancel := context.WithCancel(context.Background())
	detached := NewDetachedRunner(ctx)
	state.Table.SetDetachedRunner(detached)

	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			cancel()
			detached.Wait()
			state.CloseAppServer(context.Background())
		})
	}
	state.Shutdown = func(_ bool) { go shutdown() }

	var idle *IdleTimer
	idle = NewIdleTimer(idleTimeout, func() {
		if state.Table.HasNonTerminal() {
			idle.Reset()
			return
		}
		shutdown()
	})
	state.Table.SetOnActivity(idle.Reset)
	idle.Start()

	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		idle.Stop()
		shutdown()
		<-done
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(addrPath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b)), shutdown
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("broker addr %s did not appear", addrPath)
	return "", nil
}

// TestIdleOutDoesNotKillLongRunningDispatch is the headline acceptance test for
// the idle guard: a synchronous dispatch held in-flight by a TURN_DELAY far
// longer than CODEX_BROKER_IDLE_MS must NOT be killed, and the broker must not
// exit, while it runs. The guard re-arms idle while the task is non-terminal
// and AppendEvent resets it on streamed notifications. On the OLD wiring (idle
// onFire closed the app-server + cancelled unconditionally) the idle timer
// would fire mid-turn, CloseAppServer would yank the codex child, and the
// dispatch would come back errored — this test would fail.
func TestIdleOutDoesNotKillLongRunningDispatch(t *testing.T) {
	repoDir := t.TempDir()
	t.Setenv("CODEX_BROKER_TURN_TIMEOUT_MS", "") // no per-turn deadline
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":           "0.130.0",
		"FAKE_APPSERVER_SESSION":       "thread-long",
		"FAKE_APPSERVER_TURN_DELAY_MS": "700",
	})

	state := &BrokerState{Table: NewTable(1, 2048), CWD: repoDir}
	// Idle timeout an order of magnitude shorter than the turn delay: it would
	// fire many times during the single in-flight turn without the guard.
	addr, _ := startGuardedBroker(t, state, 40*time.Millisecond)

	client, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	res, rerr := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s", Mode: "fresh", Prompt: "p",
		Sandbox: "workspace-write", LogPath: filepath.Join(repoDir, "long.log"),
	}, nil)
	if rerr != nil {
		t.Fatalf("dispatch.run returned error (idle-out likely killed it): %v", rerr)
	}
	if res.State != string(StateDone) {
		t.Fatalf("long dispatch final state = %q (exit=%d msg=%q), want done — idle-out killed the turn",
			res.State, res.ExitCode, res.ErrorMessage)
	}
	if res.ExitCode != 0 {
		t.Fatalf("long dispatch exit_code = %d, want 0", res.ExitCode)
	}

	// The broker must still be alive after a turn that outlived several idle
	// windows: ping must succeed.
	if _, err := client.Ping(context.Background()); err != nil {
		t.Fatalf("broker not alive after long dispatch (it exited on idle-out): %v", err)
	}
}

// TestShutdownDrainsDetachedRunBeforeClosingChild verifies that broker shutdown
// drains an in-flight DETACHED task instead of yanking the codex child out from
// under it. A detached task.start is held in-flight by a TURN_DELAY; once its
// turn is genuinely running we trigger shutdown. The detached drain observes
// ctx cancellation, interrupts the turn, and reaches a terminal state; shutdown
// blocks on detached.Wait until that happens, then closes the child. We assert
// the detached task reached a terminal state (it was drained, not orphaned),
// and that no panic/race occurred under -race. On the OLD code runDetached used
// context.Background() (shutdown could not cancel it) and shutdown closed the
// child immediately with no drain — the detached run would be yanked.
func TestShutdownDrainsDetachedRunBeforeClosingChild(t *testing.T) {
	repoDir := t.TempDir()
	rpcLog := filepath.Join(repoDir, "rpc.log")
	t.Setenv("CODEX_BROKER_TURN_TIMEOUT_MS", "") // no per-turn deadline
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":           "0.130.0",
		"FAKE_APPSERVER_SESSION":       "thread-detached",
		"FAKE_APPSERVER_TURN_DELAY_MS": "800",
		"FAKE_APPSERVER_RPC_LOG":       rpcLog,
	})

	state := &BrokerState{Table: NewTable(1, 2048), CWD: repoDir}
	// Long idle so idle-out is not what ends this test; we trigger shutdown.
	addr, shutdown := startGuardedBroker(t, state, time.Hour)

	client, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	taskID, _, err := client.TaskStart(context.Background(), DispatchRunParams{
		SessionID: "s", Mode: "fresh", Prompt: "p",
		Sandbox: "workspace-write", LogPath: filepath.Join(repoDir, "detached.log"),
	})
	if err != nil {
		t.Fatalf("TaskStart: %v", err)
	}

	// Wait until the detached turn is genuinely in-flight (turn/start logged by
	// the fake, then it sleeps in the delay), so shutdown races a live turn.
	if !waitForLogContains(t, rpcLog, "turn/start", 3*time.Second) {
		raw, _ := os.ReadFile(rpcLog)
		t.Fatalf("detached turn never started; rpc log:\n%s", raw)
	}

	// Trigger shutdown and run it to completion. With the lifecycle binding,
	// shutdown cancels the serve ctx (which the detached drain derives from),
	// the drain interrupts the turn and marks the task cancelled, and
	// detached.Wait() blocks until that drain returns — only THEN is the codex
	// child closed. So the instant shutdown() returns, the detached task is
	// already terminal (cancelled).
	//
	// On the OLD code the detached run used context.Background() (shutdown's
	// cancel could not reach it) and the goroutine was untracked (Wait returned
	// immediately), so shutdown closed the child while the fake was still
	// mid-sleep: shutdown() would return with the detached task STILL running,
	// its child yanked out from under it. Asserting the task is terminal the
	// moment shutdown() returns therefore fails on the old behavior.
	shutDone := make(chan struct{})
	go func() {
		shutdown()
		close(shutDone)
	}()
	select {
	case <-shutDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("shutdown did not return — detached drain not bound to broker lifetime (Wait hung)")
	}

	// Read the state immediately (no polling): shutdown drained the detached run
	// before returning, so it must already be terminal — and specifically
	// cancelled (drained via ctx cancellation), not yanked.
	st, serr := state.Table.Status(taskID)
	if serr != nil {
		t.Fatalf("Status(detached): %v", serr)
	}
	if !st.State.IsTerminal() {
		t.Fatalf("detached task state = %q the instant shutdown returned, want terminal — shutdown did not drain it (child yanked from a still-running turn)", st.State)
	}
	if st.State != StateCancelled {
		t.Fatalf("detached task state = %q after shutdown, want cancelled (drained via ctx cancellation, not completed against a dead child)", st.State)
	}
}

// TestCancelDoesNotCorruptQueuedPeerOnSharedAppServer is the cancel+concurrency
// INTEGRATION test: a RUNNING task (held in-flight by TURN_DELAY) is cancelled
// while a PEER task is queued behind the single run slot on the SAME shared
// app-server. After the cancel interrupts task 1 and frees the slot, task 2 must
// run its OWN turn on the reused shared child and complete cleanly (done, exit 0)
// — the cancelled sibling must neither steal task 2's slot/result nor leave the
// shared app-server poisoned. It then asserts the two tasks' event rings did not
// cross.
//
// This goes past P07's single-task cancel unit test (which only asserts the
// cancelled task interrupts + frees its slot): here a concurrent peer must SURVIVE
// the cancel uncorrupted on the shared instance. On the OLD drain a cancel that
// raced the turn could mislabel events or tear the shared child down under the
// queued peer; this asserts the peer's clean completion after a sibling cancel.
func TestCancelDoesNotCorruptQueuedPeerOnSharedAppServer(t *testing.T) {
	repoDir := t.TempDir()
	rpcLog := filepath.Join(repoDir, "rpc.log")
	// Cancel (not a deadline) must be what ends task 1.
	t.Setenv("CODEX_BROKER_TURN_TIMEOUT_MS", "")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":           "0.130.0",
		"FAKE_APPSERVER_SESSION":       "thread-cancel-peer",
		"FAKE_APPSERVER_TURN_DELAY_MS": "1500",
		"FAKE_APPSERVER_RPC_LOG":       rpcLog,
	})

	// cap=1 so task 2 is genuinely queued behind task 1's slot.
	state := &BrokerState{Table: NewTable(1, 2048), CWD: repoDir}
	addr := startTestBroker(t, state)

	dialer, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// Task 1: synchronous dispatch.run held in-flight by the turn delay.
	type runResult struct {
		res *DispatchRunResult
		err error
	}
	runCh := make(chan runResult, 1)
	go func() {
		runner, derr := Dial(addr)
		if derr != nil {
			runCh <- runResult{err: derr}
			return
		}
		res, rerr := runner.DispatchRun(context.Background(), DispatchRunParams{
			SessionID: "s1", Mode: "fresh", Prompt: "p1",
			Sandbox: "workspace-write", LogPath: filepath.Join(repoDir, "t1.log"),
		}, nil)
		runCh <- runResult{res: res, err: rerr}
	}()

	// Wait until task 1's turn is genuinely in flight (the fake logs turn/start
	// then sleeps), so the cancel hits an established turn.
	if !waitForLogContains(t, rpcLog, "turn/start", 3*time.Second) {
		raw, _ := os.ReadFile(rpcLog)
		t.Fatalf("task 1 turn never started; rpc log:\n%s", raw)
	}
	var task1ID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && task1ID == "" {
		tasks, lerr := dialer.TaskList(context.Background(), "")
		if lerr == nil {
			for _, tk := range tasks {
				if tk.State == string(StateRunning) {
					task1ID = tk.TaskID
				}
			}
		}
		if task1ID == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if task1ID == "" {
		t.Fatalf("task 1 never reached running")
	}

	// Task 2: queued behind the single slot.
	task2ID, queued, err := dialer.TaskStart(context.Background(), DispatchRunParams{
		SessionID: "s2", Mode: "fresh", Prompt: "p2",
		Sandbox: "workspace-write", LogPath: filepath.Join(repoDir, "t2.log"),
	})
	if err != nil {
		t.Fatalf("TaskStart task2: %v", err)
	}
	if !queued {
		t.Fatalf("task2 should be queued behind the single run slot")
	}
	if task2ID == task1ID {
		t.Fatalf("task2 id collided with task1 id %q", task1ID)
	}

	// Cancel the running task 1.
	if err := dialer.TaskCancel(context.Background(), task1ID); err != nil {
		t.Fatalf("TaskCancel: %v", err)
	}

	// Task 1's dispatch.run must return promptly as cancelled.
	select {
	case rr := <-runCh:
		if rr.err != nil {
			t.Fatalf("task1 dispatch.run error: %v", rr.err)
		}
		if rr.res.State != string(StateCancelled) {
			t.Fatalf("task1 final state = %q, want cancelled", rr.res.State)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("task1 dispatch.run did not return promptly after cancel")
	}

	// The peer (task 2) must now acquire the freed slot, run its OWN turn on the
	// reused shared app-server, and complete cleanly — not be starved, not inherit
	// the cancelled sibling's state, not error on a poisoned child.
	waitForState(t, dialer, task2ID, string(StateDone), 10*time.Second)
	st2, serr := dialer.TaskStatusCall(context.Background(), task2ID)
	if serr != nil {
		t.Fatalf("task.status task2: %v", serr)
	}
	if st2.ExitCode == nil || *st2.ExitCode != 0 {
		t.Fatalf("peer task2 exit_code = %v, want 0 (cancel of sibling poisoned the shared app-server?)", st2.ExitCode)
	}

	// No cross-turn corruption: task 1's ring carries its cancel (and never
	// task.finished); task 2's ring carries a clean finish (and never task 1's
	// cancellation), and neither ring carries the other's task_id.
	evs1, e1 := state.Table.Events(task1ID, 1)
	if e1 != nil {
		t.Fatalf("Events(task1): %v", e1)
	}
	for _, ev := range evs1 {
		if pid := taskIDFromPayload(ev.Payload); pid != "" && pid != task1ID {
			t.Fatalf("task1 ring contains peer task_id %q: %s", pid, ev.Payload)
		}
		if ev.Type == "task.finished" {
			t.Fatalf("cancelled task1 emitted a spurious task.finished: %+v", evs1)
		}
	}
	evs2, e2 := state.Table.Events(task2ID, 1)
	if e2 != nil {
		t.Fatalf("Events(task2): %v", e2)
	}
	var task2Finished bool
	for _, ev := range evs2 {
		if pid := taskIDFromPayload(ev.Payload); pid != "" && pid != task2ID {
			t.Fatalf("task2 ring contains peer task_id %q: %s", pid, ev.Payload)
		}
		switch ev.Type {
		case "task.finished":
			task2Finished = true
		case "task.cancelled":
			t.Fatalf("peer task2 was wrongly marked cancelled by the sibling cancel: %+v", evs2)
		}
	}
	if !task2Finished {
		t.Fatalf("peer task2 ring missing task.finished after a clean run: %+v", evs2)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestTaskHandlersOmitQueuedStartedAtAndFormatRunningStartedAt(t *testing.T) {
	table := NewTable(8, 2048)
	queuedID, _ := table.Start("s", TaskParams{Mode: "fresh"})
	runningID, _ := table.Start("s", TaskParams{Mode: "fresh"})
	if err := table.MarkRunning(runningID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	runningTask, err := table.Status(runningID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	listOut, err := HandleTaskList(&BrokerState{Table: table})(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("HandleTaskList: %v", err)
	}
	for _, task := range listOut.(map[string]any)["tasks"].([]map[string]any) {
		switch task["task_id"] {
		case queuedID:
			if _, ok := task["started_at"]; ok {
				t.Fatalf("queued task.list started_at = %v, want omitted", task["started_at"])
			}
		case runningID:
			startedAt, ok := task["started_at"].(string)
			if !ok {
				t.Fatalf("running task.list started_at = %T, want string", task["started_at"])
			}
			if startedAt != formatTaskTimestamp(runningTask.StartedAt) {
				t.Fatalf("running task.list started_at = %q, want %q", startedAt, formatTaskTimestamp(runningTask.StartedAt))
			}
			if _, err := time.Parse(time.RFC3339, startedAt); err != nil {
				t.Fatalf("running task.list started_at is not RFC3339: %v", err)
			}
		}
	}

	status := HandleTaskStatus(&BrokerState{Table: table})
	raw, _ := json.Marshal(map[string]any{"task_id": queuedID})
	statusOut, err := status(context.Background(), raw)
	if err != nil {
		t.Fatalf("HandleTaskStatus queued: %v", err)
	}
	if _, ok := statusOut.(map[string]any)["started_at"]; ok {
		t.Fatalf("queued task.status started_at = %v, want omitted", statusOut.(map[string]any)["started_at"])
	}

	raw, _ = json.Marshal(map[string]any{"task_id": runningID})
	statusOut, err = status(context.Background(), raw)
	if err != nil {
		t.Fatalf("HandleTaskStatus running: %v", err)
	}
	startedAt, ok := statusOut.(map[string]any)["started_at"].(string)
	if !ok {
		t.Fatalf("running task.status started_at = %T, want string", statusOut.(map[string]any)["started_at"])
	}
	if startedAt != formatTaskTimestamp(runningTask.StartedAt) {
		t.Fatalf("running task.status started_at = %q, want %q", startedAt, formatTaskTimestamp(runningTask.StartedAt))
	}
	if _, err := time.Parse(time.RFC3339, startedAt); err != nil {
		t.Fatalf("running task.status started_at is not RFC3339: %v", err)
	}
}
