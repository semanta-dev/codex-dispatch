package broker

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/codex/appserver"
)

// startTestBroker spins a full broker.Server over loopback TCP with all the
// handlers the lifecycle tests exercise (dispatch.run, task.start, task.status,
// task.cancel, task.list, broker.ping). Returns the dial address. The codex
// app-server child (the fake-appserver on PATH) is torn down on cleanup so a
// wedged/sleeping turn cannot leak past the test.
func startTestBroker(t *testing.T, state *BrokerState) string {
	t.Helper()
	srv := NewServer("127.0.0.1:0")
	addrPath := filepath.Join(t.TempDir(), "broker.addr")
	srv.SetAddrFile(addrPath)
	srv.HandleFunc("broker.ping", HandleBrokerPing(state))
	srv.HandleFunc("task.list", HandleTaskList(state))
	srv.HandleFunc("task.status", HandleTaskStatus(state))
	srv.HandleFunc("task.cancel", HandleTaskCancel(state))
	srv.HandleFunc("task.start", HandleTaskStart(state))
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		// Kill the shared codex child so any in-flight/sleeping turn returns.
		state.CloseAppServer(context.Background())
		<-done
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(addrPath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("broker addr %s did not appear", addrPath)
	return ""
}

// waitForState polls task.status until the task reaches want or the deadline
// elapses, failing the test on timeout.
func waitForState(t *testing.T, c *Client, taskID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		st, err := c.TaskStatusCall(context.Background(), taskID)
		if err == nil {
			last = st.State
			if st.State == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s state = %q after %s, want %q", taskID, last, timeout, want)
}

// waitForLogContains polls a file until it contains substr or timeout elapses.
func waitForLogContains(t *testing.T, path, substr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), substr) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func setupFakeAppserver(t *testing.T, env map[string]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-appserver unavailable on Windows")
	}
	wd, _ := os.Getwd()
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(root, "tests/fixtures/fake-appserver")); err == nil {
				break
			}
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("repo root not found from %s", wd)
		}
		root = parent
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join(root, "tests/fixtures/fake-appserver")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build fake-appserver: %v: %s", err, out)
	}
	_ = os.Chmod(bin, fs.FileMode(0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestDispatchRunHappyPath(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "thread-x",
		"FAKE_APPSERVER_EDIT":    filepath.Join(repoDir, "hello.txt") + ":Hello",
	})

	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	result, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s",
		Mode:      "fresh",
		Prompt:    "do thing",
		Sandbox:   "workspace-write",
		LogPath:   logPath,
	}, func(ev DispatchEvent) {})
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", result.ExitCode)
	}
	if result.SessionID != "thread-x" {
		t.Fatalf("session = %q, want thread-x", result.SessionID)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"id":"thread-x"`) {
		t.Fatalf("log missing thread id: %s", s)
	}
	if !strings.Contains(s, `"method":"turn/completed"`) {
		t.Fatalf("log missing turn/completed: %s", s)
	}
}

func TestDispatchRunUsesRequestCWDForThreadStart(t *testing.T) {
	brokerDir := t.TempDir()
	worktreeDir := t.TempDir()
	logPath := filepath.Join(worktreeDir, "stdout.log")
	rpcLog := filepath.Join(brokerDir, "rpc.log")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "thread-cwd",
		"FAKE_APPSERVER_RPC_LOG": rpcLog,
	})

	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: brokerDir}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	_, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s",
		Mode:      "fresh",
		Prompt:    "do thing",
		Sandbox:   "workspace-write",
		LogPath:   logPath,
		CWD:       worktreeDir,
	}, nil)
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	raw, err := os.ReadFile(rpcLog)
	if err != nil {
		t.Fatalf("read rpc log: %v", err)
	}
	if !strings.Contains(string(raw), `"cwd":"`+worktreeDir+`"`) {
		t.Fatalf("thread/start did not use request cwd %q:\n%s", worktreeDir, raw)
	}
}

func TestDispatchRunStreamsEventsToCallback(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "tx",
	})

	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	var got []string
	_, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s",
		Mode:      "fresh",
		Prompt:    "t",
		Sandbox:   "workspace-write",
		LogPath:   logPath,
	}, func(ev DispatchEvent) {
		got = append(got, ev.Type)
	})
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	var sawThreadStarted, sawTaskFinished bool
	for _, tt := range got {
		if tt == "thread/started" {
			sawThreadStarted = true
		}
		if tt == "task.finished" {
			sawTaskFinished = true
		}
	}
	if !sawThreadStarted || !sawTaskFinished {
		t.Fatalf("events = %v, want thread/started + task.finished", got)
	}
}

func TestDispatchRunMapsFailedStatus(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_EXIT":    "failed",
		"FAKE_APPSERVER_SESSION": "tx",
	})
	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	result, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s", Mode: "fresh", Prompt: "t", Sandbox: "workspace-write", LogPath: logPath,
	}, nil)
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	if result.ExitCode != 2 {
		t.Fatalf("exit = %d, want 2 (failed)", result.ExitCode)
	}
}

func TestDispatchRunTaskTableReflectsState(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	setupFakeAppserver(t, map[string]string{"FAKE_CODEX_VERSION": "0.130.0", "FAKE_APPSERVER_SESSION": "tx"})
	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	result, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s", Mode: "fresh", Prompt: "t", Sandbox: "workspace-write", LogPath: logPath,
	}, nil)
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	tasks := table.List("s")
	if len(tasks) != 1 || tasks[0].ID != result.TaskID {
		t.Fatalf("table missing task: %+v", tasks)
	}
	if tasks[0].State != StateDone {
		t.Fatalf("state = %s, want done", tasks[0].State)
	}
}

func TestDispatchRunStaleResumeRetriesFresh(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":          "0.130.0",
		"FAKE_APPSERVER_STALE_RESUME": "stale-thread-1",
		"FAKE_APPSERVER_SESSION":      "fresh-thread-2",
		"FAKE_APPSERVER_EDIT":         filepath.Join(repoDir, "from-fresh.txt") + ":from fresh retry",
	})

	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	result, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID:     "s",
		Mode:          "resume",
		Prompt:        "x",
		Sandbox:       "workspace-write",
		PrevSessionID: "stale-thread-1",
		LogPath:       logPath,
	}, nil)
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	if !result.FellBackToFresh {
		t.Fatalf("FellBackToFresh = false, want true")
	}
	if result.SessionID != "fresh-thread-2" {
		t.Fatalf("session = %q, want fresh-thread-2 (from fresh retry)", result.SessionID)
	}

	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), "==== fell back to fresh dispatch ====") {
		t.Fatalf("stdout.log missing marker: %s", raw)
	}
}

func TestLogWriterByteStability(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stdout.log")
	lw, err := OpenLogWriter(logPath)
	if err != nil {
		t.Fatalf("OpenLogWriter: %v", err)
	}
	defer lw.Close()

	// One notification: rendered as {"method":..., "params":...}\n
	if err := lw.WriteNotification(appserver.Notification{
		Method: "thread/started",
		Params: []byte(`{"thread":{"id":"a"}}`),
	}); err != nil {
		t.Fatalf("WriteNotification: %v", err)
	}
	// A second notification
	if err := lw.WriteNotification(appserver.Notification{
		Method: "turn/completed",
		Params: []byte(`{"turn":{"id":"t","status":"completed"}}`),
	}); err != nil {
		t.Fatalf("WriteNotification: %v", err)
	}
	// Fall-back marker
	if err := lw.WriteMarker(); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	// Synthetic broker error
	if err := lw.WriteSyntheticError("broker/dispatch/error", "something broke"); err != nil {
		t.Fatalf("WriteSyntheticError: %v", err)
	}

	got, _ := os.ReadFile(logPath)
	want := `{"method":"thread/started","params":{"thread":{"id":"a"}}}` + "\n" +
		`{"method":"turn/completed","params":{"turn":{"id":"t","status":"completed"}}}` + "\n" +
		"\n==== fell back to fresh dispatch ====\n" +
		`{"method":"broker/dispatch/error","params":{"message":"something broke"}}` + "\n"
	if string(got) != want {
		t.Fatalf("log mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// TestCancelRunningTaskInterruptsTurnAndFreesSlot exercises the cancel-aware
// drain: a RUNNING task (held in-flight by FAKE_APPSERVER_TURN_DELAY_MS) is
// cancelled via task.cancel; the drain must send turn/interrupt, return
// promptly, free the single run slot, and let a queued task start. Against the
// OLD drain (`for n := range handle.Events`) the cancel only cancelled a ctx
// the drain ignored, so dispatch.run blocked until the full TURN_DELAY elapsed
// and the queued task never got the slot in time — this test would time out.
func TestCancelRunningTaskInterruptsTurnAndFreesSlot(t *testing.T) {
	repoDir := t.TempDir()
	rpcLog := filepath.Join(repoDir, "rpc.log")
	// Disable the per-turn deadline so the cancel path (not a timeout) is what
	// terminates the turn, and so this test is hermetic regardless of process
	// env or sibling tests that exercise the deadline.
	t.Setenv("CODEX_BROKER_TURN_TIMEOUT_MS", "")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":           "0.130.0",
		"FAKE_APPSERVER_SESSION":       "thread-cancel",
		"FAKE_APPSERVER_TURN_DELAY_MS": "1500",
		"FAKE_APPSERVER_RPC_LOG":       rpcLog,
	})

	// cap=1 so the second task can only run after the first frees its slot.
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

	// Wait until task 1's TURN is genuinely in-flight: the fake logs turn/start
	// only after thread/start + EnsureAppServer succeed, and then sleeps in the
	// turn delay. Waiting on this (rather than merely the table's "running"
	// state, which is set before the app-server is even spawned) guarantees we
	// cancel an established turn — exercising handle.Cancel()/turn/interrupt.
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

	// Cancel the running task 1.
	if err := dialer.TaskCancel(context.Background(), task1ID); err != nil {
		t.Fatalf("TaskCancel: %v", err)
	}

	// The drain must return promptly (well under the 4s turn delay), not hang.
	select {
	case rr := <-runCh:
		if rr.err != nil {
			t.Fatalf("dispatch.run returned error: %v", rr.err)
		}
		if rr.res.State != string(StateCancelled) {
			t.Fatalf("task1 final state = %q (exit=%d msg=%q), want cancelled", rr.res.State, rr.res.ExitCode, rr.res.ErrorMessage)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("dispatch.run did not return promptly after cancel (drain ignored cancellation)")
	}

	// A cancel must NOT emit task.finished for the cancelled task. On the
	// buggy drain, handle.Cancel()'s channel close could race ctx.Done() and be
	// picked by the `case n,ok := <-events; if !ok` arm, returning
	// (nil, drainCompleted) → turnToExit(nil) → a spurious task.finished(exit=64)
	// before the MarkDone-failure fallback recovered to cancelled. The ctx.Err()
	// guard in drainTurn's !ok branch prevents that mislabelled event. Inspect
	// the task's event ring directly (no notifier was attached to task 1).
	evs, eerr := state.Table.Events(task1ID, 1)
	if eerr != nil {
		t.Fatalf("Events(task1): %v", eerr)
	}
	var sawFinished, sawCancelled bool
	for _, ev := range evs {
		switch ev.Type {
		case "task.finished":
			sawFinished = true
		case "task.cancelled":
			sawCancelled = true
		}
	}
	if sawFinished {
		t.Fatalf("cancelled task emitted a spurious task.finished event: %+v", evs)
	}
	if !sawCancelled {
		t.Fatalf("cancelled task did not emit task.cancelled: %+v", evs)
	}

	// The freed slot must let the queued task start (slot frees the instant the
	// drain returns; task2 transitions queued -> running on slot acquisition).
	waitForState(t, dialer, task2ID, string(StateRunning), 3*time.Second)

	// turn/interrupt must reach codex. The shared fake child is mid-sleep when
	// the interrupt is written to its stdin, so it logs the method only after
	// waking; poll the rpc log rather than read it once.
	if !waitForLogContains(t, rpcLog, "turn/interrupt", 4*time.Second) {
		raw, _ := os.ReadFile(rpcLog)
		t.Fatalf("turn/interrupt not sent on cancel; rpc log:\n%s", raw)
	}
}

// TestConcurrentTaskStartSharedAppServerNoCrossTurnCorruption is the detached
// (task.start) sibling of the synchronous concurrent shared-app-server test. It
// submits several DETACHED task.start dispatches CONCURRENTLY through one broker
// wired exactly like cmd (DetachedRunner + idle guard, via startGuardedBroker).
// All detached runs share one EnsureAppServer and serialise on the single run
// slot; each must reach a terminal done with its own correct exit_code/session
// and distinct task id, and the broker must keep their event rings from crossing.
//
// The detached path (runDetached) is the one P08 rebound to the broker-lifetime
// context and tracked on the DetachedRunner. This drives that path under
// concurrency on a SHARED app-server: it exercises the same reader-multiplexing /
// per-task bookkeeping the synchronous test does, but through the background
// goroutines the detached runner manages. Race-clean and corruption-free required.
func TestConcurrentTaskStartSharedAppServerNoCrossTurnCorruption(t *testing.T) {
	repoDir := t.TempDir()
	t.Setenv("CODEX_BROKER_TURN_TIMEOUT_MS", "") // no per-turn deadline
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "thread-detached-concurrent",
	})

	// cap=1: detached turns serialise on the run slot over the single shared
	// thread (matching the per-thread single-turn constraint). Long idle so
	// idle-out is not what ends the test.
	state := &BrokerState{Table: NewTable(1, 2048), CWD: repoDir}
	addr, _ := startGuardedBroker(t, state, time.Hour)

	const n = 4
	submitter, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// Submit all detached starts concurrently to maximise contention on the slot
	// and the shared app-server.
	type startResult struct {
		idx    int
		taskID string
		err    error
	}
	starts := make(chan startResult, n)
	begin := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(idx int) {
			client, derr := Dial(addr)
			if derr != nil {
				starts <- startResult{idx: idx, err: derr}
				return
			}
			<-begin
			id, _, serr := client.TaskStart(context.Background(), DispatchRunParams{
				SessionID: fmt.Sprintf("s%d", idx),
				Mode:      "fresh",
				Prompt:    fmt.Sprintf("prompt-%d", idx),
				Sandbox:   "workspace-write",
				LogPath:   filepath.Join(repoDir, fmt.Sprintf("d%d.log", idx)),
			})
			starts <- startResult{idx: idx, taskID: id, err: serr}
		}(i)
	}
	close(begin)

	taskIDs := make(map[string]int, n)
	for i := 0; i < n; i++ {
		sr := <-starts
		if sr.err != nil {
			t.Fatalf("task.start %d: %v", sr.idx, sr.err)
		}
		if sr.taskID == "" {
			t.Fatalf("task.start %d returned empty task id", sr.idx)
		}
		taskIDs[sr.taskID] = sr.idx
	}
	if len(taskIDs) != n {
		t.Fatalf("concurrent task.start produced %d distinct task ids, want %d", len(taskIDs), n)
	}

	// Each detached task must reach a terminal done with exit_code 0 and its own
	// session id (the shared fake's configured thread).
	for taskID, idx := range taskIDs {
		waitForState(t, submitter, taskID, string(StateDone), 15*time.Second)
		st, serr := submitter.TaskStatusCall(context.Background(), taskID)
		if serr != nil {
			t.Fatalf("task.status %d (%s): %v", idx, taskID, serr)
		}
		if st.ExitCode == nil || *st.ExitCode != 0 {
			t.Fatalf("detached task %d (%s) exit_code = %v, want 0", idx, taskID, st.ExitCode)
		}
		if st.SessionID != "thread-detached-concurrent" {
			t.Fatalf("detached task %d (%s) session_id = %q, want thread-detached-concurrent", idx, taskID, st.SessionID)
		}
	}

	// No cross-turn corruption: each detached task's ring carries only its own
	// task_id-stamped events and a clean completion (task.finished, no
	// cancelled/errored).
	for taskID, idx := range taskIDs {
		evs, eerr := state.Table.Events(taskID, 1)
		if eerr != nil {
			t.Fatalf("Events(detached task %d=%s): %v", idx, taskID, eerr)
		}
		var sawFinished bool
		for _, ev := range evs {
			if pid := taskIDFromPayload(ev.Payload); pid != "" && pid != taskID {
				t.Fatalf("detached task %d (%s) ring contains peer task_id %q: %s", idx, taskID, pid, ev.Payload)
			}
			switch ev.Type {
			case "task.finished":
				sawFinished = true
			case "task.cancelled", "task.errored":
				t.Fatalf("detached task %d (%s) ring has a %s event for a clean run: %+v", idx, taskID, ev.Type, evs)
			}
		}
		if !sawFinished {
			t.Fatalf("detached task %d (%s) ring missing task.finished: %+v", idx, taskID, evs)
		}
	}
}

// TestWedgedTurnHitsPerTurnDeadline verifies that a turn which never reaches
// turn/completed within CODEX_BROKER_TURN_TIMEOUT_MS is interrupted (slot
// freed, errored result) instead of pinning the slot forever. The fake sleeps
// far longer than the deadline; with no deadline (old behavior) dispatch.run
// would block for the full delay.
func TestWedgedTurnHitsPerTurnDeadline(t *testing.T) {
	repoDir := t.TempDir()
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":           "0.130.0",
		"FAKE_APPSERVER_SESSION":       "thread-wedged",
		"FAKE_APPSERVER_TURN_DELAY_MS": "10000",
	})
	t.Setenv("CODEX_BROKER_TURN_TIMEOUT_MS", "200")

	state := &BrokerState{Table: NewTable(1, 2048), CWD: repoDir}
	addr := startTestBroker(t, state)
	client, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	done := make(chan *DispatchRunResult, 1)
	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		res, rerr := client.DispatchRun(context.Background(), DispatchRunParams{
			SessionID: "s", Mode: "fresh", Prompt: "p",
			Sandbox: "workspace-write", LogPath: filepath.Join(repoDir, "wedged.log"),
		}, nil)
		if rerr != nil {
			errCh <- rerr
			return
		}
		done <- res
	}()

	select {
	case res := <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("deadline did not fire promptly: elapsed=%s", elapsed)
		}
		if res.State != string(StateErrored) {
			t.Fatalf("wedged turn final state = %q, want errored", res.State)
		}
		if res.ExitCode != 64 {
			t.Fatalf("wedged turn exit_code = %d, want 64", res.ExitCode)
		}
		if !strings.Contains(res.ErrorMessage, "deadline") {
			t.Fatalf("wedged turn error = %q, want a deadline message", res.ErrorMessage)
		}
	case err := <-errCh:
		t.Fatalf("dispatch.run returned error: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("wedged turn never hit the per-turn deadline (slot pinned)")
	}

	// The slot must be free: ping should report zero running tasks.
	ping, err := client.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if ping.RunningCount != 0 {
		t.Fatalf("running_count = %d after deadline, want 0 (slot not freed)", ping.RunningCount)
	}
}

func TestDispatchRunSandboxPreflightFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	rpcLog := filepath.Join(repoDir, "rpc.log")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":          "0.130.0",
		"FAKE_APPSERVER_SESSION":      "thread-x",
		"FAKE_APPSERVER_RPC_LOG":      rpcLog,
		"FAKE_APPSERVER_BWRAP_BROKEN": "1",
	})

	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	result, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s",
		Mode:      "fresh",
		Prompt:    "do thing",
		Sandbox:   "workspace-write",
		LogPath:   logPath,
	}, func(ev DispatchEvent) {})
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	if result.ExitCode != 64 || result.State != string(StateErrored) {
		t.Fatalf("result = {exit:%d state:%q}, want {64 errored}", result.ExitCode, result.State)
	}
	// The message must steer the operator to the working fix and quote codex's
	// own diagnostic.
	for _, want := range []string{"danger-full-access", "bwrap", "workspace-write"} {
		if !strings.Contains(result.ErrorMessage, want) {
			t.Fatalf("error message %q missing %q", result.ErrorMessage, want)
		}
	}
	// stdout.log carries the broker-side explanation.
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), "broker/dispatch/error") {
		t.Fatalf("stdout.log missing broker error: %s", raw)
	}
	// Fail-fast: the probe ran, but NO thread/turn was ever started.
	rpc, _ := os.ReadFile(rpcLog)
	if !strings.Contains(string(rpc), "command/exec") {
		t.Fatalf("expected a command/exec probe in rpc log:\n%s", rpc)
	}
	if strings.Contains(string(rpc), "thread/start") || strings.Contains(string(rpc), "turn/start") {
		t.Fatalf("dispatch should not start a thread/turn after a failed preflight:\n%s", rpc)
	}
}

func TestDispatchRunSandboxPreflightSkippedForDangerFullAccess(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	rpcLog := filepath.Join(repoDir, "rpc.log")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":          "0.130.0",
		"FAKE_APPSERVER_SESSION":      "thread-dfa",
		"FAKE_APPSERVER_RPC_LOG":      rpcLog,
		"FAKE_APPSERVER_BWRAP_BROKEN": "1",
	})

	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	result, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s",
		Mode:      "fresh",
		Prompt:    "do thing",
		Sandbox:   "danger-full-access",
		LogPath:   logPath,
	}, func(ev DispatchEvent) {})
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	if result.ExitCode != 0 || result.SessionID != "thread-dfa" {
		t.Fatalf("result = {exit:%d session:%q}, want {0 thread-dfa}", result.ExitCode, result.SessionID)
	}
	if rpc, _ := os.ReadFile(rpcLog); strings.Contains(string(rpc), "command/exec") {
		t.Fatalf("danger-full-access must not trigger a sandbox probe:\n%s", rpc)
	}
}

func TestDispatchRunDropsRealThreadStartedAndSynthesizesOne(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "thread-synth",
	})

	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	var callbackThreadStarted []DispatchEvent
	result, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s",
		Mode:      "fresh",
		Prompt:    "t",
		Sandbox:   "workspace-write",
		LogPath:   logPath,
	}, func(ev DispatchEvent) {
		if ev.Type == "thread/started" {
			callbackThreadStarted = append(callbackThreadStarted, ev)
		}
	})
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	if len(callbackThreadStarted) != 1 {
		t.Fatalf("callback thread/started count = %d, want exactly synthesized one: %+v", len(callbackThreadStarted), callbackThreadStarted)
	}
	if !strings.Contains(string(callbackThreadStarted[0].Payload), `"id":"thread-synth"`) {
		t.Fatalf("synthesized callback payload missing thread id: %s", callbackThreadStarted[0].Payload)
	}

	evs, err := table.Events(result.TaskID, 1)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var tableThreadStarted int
	for _, ev := range evs {
		if ev.Type == "thread/started" {
			tableThreadStarted++
			if !strings.Contains(string(ev.Payload), `"id":"thread-synth"`) {
				t.Fatalf("synthesized table payload missing thread id: %s", ev.Payload)
			}
		}
	}
	if tableThreadStarted != 1 {
		t.Fatalf("table thread/started count = %d, want exactly synthesized one: %+v", tableThreadStarted, evs)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if got := strings.Count(string(raw), `"method":"thread/started"`); got != 1 {
		t.Fatalf("log thread/started count = %d, want exactly synthesized one:\n%s", got, raw)
	}
	if !strings.Contains(string(raw), `"id":"thread-synth"`) {
		t.Fatalf("log synthesized thread/started missing thread id:\n%s", raw)
	}
}
