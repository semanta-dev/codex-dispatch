package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/codex/appserver"
)

// LogWriter writes one codex notification per line to <run_dir>/stdout.log
// in the new {method, params} JSON-RPC notification format. Safe for
// concurrent use.
type LogWriter struct {
	mu   sync.Mutex
	file *os.File
}

// OpenLogWriter opens (or creates) logPath in append mode.
func OpenLogWriter(logPath string) (*LogWriter, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &LogWriter{file: f}, nil
}

// WriteNotification writes one codex notification as {method, params}\n.
func (lw *LogWriter) WriteNotification(n appserver.Notification) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	b, err := json.Marshal(struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{Method: n.Method, Params: n.Params})
	if err != nil {
		return err
	}
	if _, err := lw.file.Write(b); err != nil {
		return err
	}
	_, err = lw.file.Write([]byte("\n"))
	return err
}

// WriteSyntheticError emits a broker-namespaced error notification so
// stdout.log always carries an explanation of broker-side failures. Never
// confused with real codex notifications because of the "broker/" prefix.
func (lw *LogWriter) WriteSyntheticError(method, message string) error {
	quoted, _ := json.Marshal(message)
	params := json.RawMessage(`{"message":` + string(quoted) + `}`)
	return lw.WriteNotification(appserver.Notification{Method: method, Params: params})
}

// WriteMarker writes the literal fall-back-to-fresh marker verbatim. Phase 1
// byte-stable line; the leading newline gives it visual separation in the log.
func (lw *LogWriter) WriteMarker() error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	_, err := lw.file.Write([]byte("\n==== fell back to fresh dispatch ====\n"))
	return err
}

// Sync flushes stdout.log to stable storage.
func (lw *LogWriter) Sync() error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.file.Sync()
}

// Close shuts the underlying file.
func (lw *LogWriter) Close() error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.file.Close()
}

// DispatchRunParams is the params shape for dispatch.run and task.start.
type DispatchRunParams struct {
	SessionID     string `json:"session_id"`
	Mode          string `json:"mode"`
	Prompt        string `json:"prompt"`
	Sandbox       string `json:"sandbox"`
	PrevSessionID string `json:"prev_session_id,omitempty"`
	ResultDir     string `json:"result_dir"`
	LogPath       string `json:"log_path"`
	CWD           string `json:"cwd,omitempty"`
	Model         string `json:"model,omitempty"`
}

// DispatchRunResult is the final response for dispatch.run.
type DispatchRunResult struct {
	TaskID          string `json:"task_id"`
	State           string `json:"state"`
	ExitCode        int    `json:"exit_code"`
	SessionID       string `json:"session_id,omitempty"`
	FellBackToFresh bool   `json:"fell_back_to_fresh"`
	EventCount      int    `json:"event_count"`
	ErrorMessage    string `json:"error_message,omitempty"`
}

type taskEventParams struct {
	TaskID  string          `json:"task_id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// HandleDispatchRun returns a streaming Handler for dispatch.run. The handler
// drives the codex app-server protocol: ensure singleton AppServer → start or
// resume thread (with stale-fallback) → start turn → drain notifications until
// turn/completed → build result.
func HandleDispatchRun(state *BrokerState) Handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p DispatchRunParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		notifier := NotifierFrom(ctx)

		taskID, _ := state.Table.Start(p.SessionID, TaskParams{
			Mode:        p.Mode,
			Prompt:      p.Prompt,
			Sandbox:     p.Sandbox,
			PrevSession: p.PrevSessionID,
			ResultDir:   p.ResultDir,
			LogPath:     p.LogPath,
			CWD:         p.CWD,
		})
		taskCtx, cleanupTask := state.registerTaskContext(ctx, taskID)
		defer cleanupTask()
		if st, serr := state.Table.Status(taskID); serr == nil && st.State == StateCancelled {
			return DispatchRunResult{
				TaskID:       taskID,
				State:        string(StateCancelled),
				ExitCode:     st.ExitCode,
				EventCount:   st.EventCount,
				ErrorMessage: "task cancelled",
			}, nil
		}
		if err := state.acquireRunSlot(taskCtx); err != nil {
			_ = state.Table.MarkErrored(taskID, 64, err.Error())
			return nil, fmt.Errorf("acquire run slot: %w", err)
		}
		defer state.releaseRunSlot()
		if err := state.Table.MarkRunning(taskID); err != nil {
			return nil, fmt.Errorf("mark running: %w", err)
		}

		logW, err := OpenLogWriter(p.LogPath)
		if err != nil {
			_ = state.Table.MarkErrored(taskID, -1, err.Error())
			return nil, fmt.Errorf("open log: %w", err)
		}
		defer logW.Close()

		return runDispatchOn(taskCtx, state, taskID, p, logW, notifier), nil
	}
}

// runDispatchOn drives the codex app-server protocol for a single dispatch.
// Reused by HandleDispatchRun (sync, with notifier) and runDetached
// (background, no notifier). Always returns a DispatchRunResult; never an
// error — codex-side failures are encoded into ExitCode + ErrorMessage so
// detached callers don't have to disambiguate transport errors from turn
// failures.
func runDispatchOn(ctx context.Context, state *BrokerState, taskID string, p DispatchRunParams, logW *LogWriter, notifier Notifier) DispatchRunResult {
	emit := func(eventType string, payload any) {
		rawPayload, _ := json.Marshal(payload)
		seq, _ := state.Table.appendEventOwned(taskID, eventType, rawPayload)
		if notifier != nil {
			_ = notifier.Notify("task.event", taskEventParams{
				TaskID:  taskID,
				Seq:     seq,
				Type:    eventType,
				Payload: json.RawMessage(rawPayload),
			})
		}
	}
	emit("task.started", map[string]any{"task_id": taskID})

	srv, err := state.EnsureAppServer(ctx)
	if err != nil {
		return dispatchFailure(state, logW, taskID, fmt.Sprintf("ensure codex app-server: %v", err), emit)
	}

	// Sandbox preflight: on a host whose bubblewrap Linux sandbox can't start,
	// a sandboxed mode (read-only/workspace-write) would let codex run the turn
	// but every shell command fails with a cryptic `bwrap: ...` line buried in
	// per-command output. Fail fast with an actionable message instead.
	if msg := preflightSandbox(ctx, srv, p.Sandbox); msg != "" {
		return dispatchFailure(state, logW, taskID, msg, emit)
	}

	threadCWD := p.CWD
	if threadCWD == "" {
		threadCWD = state.CWD
	}
	threadOpts := appserver.ThreadStartOptions{
		CWD:     threadCWD,
		Sandbox: p.Sandbox,
		Model:   p.Model,
	}

	var (
		thread          *appserver.Thread
		fellBackToFresh bool
	)
	if p.Mode == "resume" && p.PrevSessionID != "" {
		t, rerr := srv.ResumeThread(ctx, p.PrevSessionID, appserver.ThreadResumeOptions{})
		if errors.Is(rerr, appserver.ErrStaleSession) {
			_ = logW.WriteMarker()
			emit("task.fell_back_to_fresh", map[string]any{"stale_session_id": p.PrevSessionID})
			fellBackToFresh = true
			t, rerr = srv.StartThread(ctx, threadOpts)
		}
		if rerr != nil {
			return dispatchFailure(state, logW, taskID, fmt.Sprintf("acquire thread: %v", rerr), emit)
		}
		thread = t
	} else {
		t, terr := srv.StartThread(ctx, threadOpts)
		if terr != nil {
			return dispatchFailure(state, logW, taskID, fmt.Sprintf("start thread: %v", terr), emit)
		}
		thread = t
	}

	// Synthesize a thread/started log line + task.event so consumers see the
	// thread even though codex's real thread/started notification arrives
	// before StartTurn registers a per-turn channel and is therefore dropped
	// by the multiplexer.
	threadParams, _ := json.Marshal(struct {
		Thread *appserver.Thread `json:"thread"`
	}{Thread: thread})
	_ = logW.WriteNotification(appserver.Notification{
		Method: "thread/started",
		Params: threadParams,
	})
	tsSeq, _ := state.Table.appendEventOwned(taskID, "thread/started", threadParams)
	if notifier != nil {
		_ = notifier.Notify("task.event", taskEventParams{
			TaskID:  taskID,
			Seq:     tsSeq,
			Type:    "thread/started",
			Payload: json.RawMessage(threadParams),
		})
	}

	handle, err := srv.StartTurn(ctx, thread.ID, p.Prompt, appserver.TurnStartOptions{})
	if err != nil {
		return dispatchFailure(state, logW, taskID, fmt.Sprintf("start turn: %v", err), emit)
	}
	// Register the handle so task.cancel (cancelTask) can interrupt THIS turn
	// (turn/interrupt + channel close) rather than only cancelling a ctx the
	// drain below would otherwise ignore.
	state.registerTurnHandle(taskID, handle)

	turn, drainState := drainTurn(ctx, state, taskID, handle, logW, notifier)

	switch drainState {
	case drainCancelled:
		// Operator cancel (task.cancel) or ctx cancellation interrupted an
		// in-flight turn. The turn was interrupted (handle.Cancel) so the run
		// slot frees as soon as we return. Record a terminal cancelled state
		// (no-op if a concurrent task.cancel already moved it there).
		_ = state.Table.MarkCancelled(taskID, 64)
		emit("task.cancelled", map[string]any{"task_id": taskID})
		st, _ := state.Table.Status(taskID)
		return DispatchRunResult{
			TaskID:          taskID,
			State:           string(StateCancelled),
			ExitCode:        st.ExitCode,
			SessionID:       thread.ID,
			FellBackToFresh: fellBackToFresh,
			EventCount:      countEvents(state.Table, taskID),
			ErrorMessage:    "task cancelled",
		}
	case drainTimedOut:
		// A wedged turn that never reached turn/completed within the per-turn
		// deadline. handle.Cancel already sent turn/interrupt; surface a
		// timeout error and free the slot rather than pin it forever.
		const msg = "turn exceeded per-turn deadline"
		_ = logW.WriteSyntheticError("broker/dispatch/error", msg)
		_ = state.Table.MarkErrored(taskID, 64, msg)
		emit("task.errored", map[string]any{"error": msg})
		return DispatchRunResult{
			TaskID:          taskID,
			State:           string(StateErrored),
			ExitCode:        64,
			SessionID:       thread.ID,
			FellBackToFresh: fellBackToFresh,
			EventCount:      countEvents(state.Table, taskID),
			ErrorMessage:    msg,
		}
	}

	exitCode, errMsg := turnToExit(turn)

	if err := state.Table.MarkDone(taskID, exitCode, thread.ID, fellBackToFresh); err != nil {
		// MarkDone failed: the task is no longer RUNNING. Reflect its ACTUAL
		// terminal state rather than falling through and mislabelling it Done.
		// The most common cause is a concurrent terminal transition between the
		// drain returning (turn/completed) and this MarkDone:
		//   - task.cancel moved it to cancelled (the drain saw completion, the
		//     cancel landed a hair later) — report cancelled, not done;
		//   - the per-turn deadline path moved it to errored — report errored,
		//     preserving that error_message.
		// Falling through to StateDone here was the bug (a "low x2"): it emitted
		// task.finished and returned exit_code/done for a task the table records
		// as cancelled or errored, so the wire result contradicted task.status.
		if st, serr := state.Table.Status(taskID); serr == nil {
			switch st.State {
			case StateCancelled:
				emit("task.cancelled", map[string]any{"task_id": taskID})
				return DispatchRunResult{
					TaskID:          taskID,
					State:           string(StateCancelled),
					ExitCode:        st.ExitCode,
					SessionID:       thread.ID,
					FellBackToFresh: fellBackToFresh,
					EventCount:      countEvents(state.Table, taskID),
					ErrorMessage:    "task cancelled",
				}
			case StateErrored:
				emit("task.errored", map[string]any{"error": st.ErrorMessage})
				return DispatchRunResult{
					TaskID:          taskID,
					State:           string(StateErrored),
					ExitCode:        st.ExitCode,
					SessionID:       thread.ID,
					FellBackToFresh: fellBackToFresh,
					EventCount:      countEvents(state.Table, taskID),
					ErrorMessage:    st.ErrorMessage,
				}
			case StateDone:
				// Already done (e.g. a duplicate completion). Report the
				// recorded terminal result rather than re-marking.
				emit("task.finished", map[string]any{
					"exit_code":  st.ExitCode,
					"session_id": thread.ID,
					"error":      st.ErrorMessage,
				})
				return DispatchRunResult{
					TaskID:          taskID,
					State:           string(StateDone),
					ExitCode:        st.ExitCode,
					SessionID:       thread.ID,
					FellBackToFresh: fellBackToFresh,
					EventCount:      countEvents(state.Table, taskID),
					ErrorMessage:    st.ErrorMessage,
				}
			}
		}
		// Unexpected: MarkDone failed but the task is not in a known terminal
		// state (or vanished). Surface it as an error result rather than a
		// spurious success.
		failMsg := fmt.Sprintf("mark done failed: %v", err)
		emit("task.errored", map[string]any{"error": failMsg})
		return DispatchRunResult{
			TaskID:          taskID,
			State:           string(StateErrored),
			ExitCode:        64,
			SessionID:       thread.ID,
			FellBackToFresh: fellBackToFresh,
			EventCount:      countEvents(state.Table, taskID),
			ErrorMessage:    failMsg,
		}
	}
	emit("task.finished", map[string]any{
		"exit_code":  exitCode,
		"session_id": thread.ID,
		"error":      errMsg,
	})
	_ = logW.Sync()
	return DispatchRunResult{
		TaskID:          taskID,
		State:           string(StateDone),
		ExitCode:        exitCode,
		SessionID:       thread.ID,
		FellBackToFresh: fellBackToFresh,
		EventCount:      countEvents(state.Table, taskID),
		ErrorMessage:    errMsg,
	}
}

// drainOutcome describes why drainTurn returned.
type drainOutcome int

const (
	drainCompleted drainOutcome = iota // turn reached turn/completed (or codex died); turn holds the result (may be nil)
	drainCancelled                     // ctx cancelled / task.cancel interrupted the turn
	drainTimedOut                      // per-turn deadline elapsed before completion
)

// drainTurn consumes the turn's event stream, logging each notification and
// relaying it as a task.event, until one of:
//   - the events channel closes and the final Turn is available (drainCompleted),
//   - ctx is cancelled (drainCancelled) — e.g. task.cancel or broker shutdown,
//   - the optional per-turn deadline elapses (drainTimedOut).
//
// On cancel or timeout it calls handle.Cancel() to send turn/interrupt and
// close the per-turn channels, so the shared app-server reader is not left
// blocked and the run slot is released as soon as the caller returns. The
// returned *Turn is non-nil only on drainCompleted (when codex actually
// reported turn/completed).
func drainTurn(ctx context.Context, state *BrokerState, taskID string, handle *appserver.TurnHandle, logW *LogWriter, notifier Notifier) (*appserver.Turn, drainOutcome) {
	relay := func(n appserver.Notification) {
		_ = logW.WriteNotification(n)
		seq, _ := state.Table.appendEventOwned(taskID, n.Method, n.Params)
		if notifier != nil {
			_ = notifier.Notify("task.event", taskEventParams{
				TaskID:  taskID,
				Seq:     seq,
				Type:    n.Method,
				Payload: n.Params,
			})
		}
	}

	var deadlineCh <-chan time.Time
	if d := turnTimeout(); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		deadlineCh = timer.C
	}

	events := handle.Events
	for {
		select {
		case n, ok := <-events:
			if !ok {
				// The events channel closed. Two distinct callers can close it:
				//   1. the reader goroutine, when codex reported turn/completed
				//      (the natural path) — handle.Result then holds the *Turn;
				//   2. handle.Cancel(), invoked by a concurrent cancelTask or the
				//      deadline branch below, which closes BOTH channels WITHOUT
				//      sending a *Turn first.
				// In case (2) the close races our ctx.Done()/deadline arms, and
				// select may non-deterministically pick this !ok arm instead. If
				// we blindly read handle.Result we'd get nil → turnToExit(nil) →
				// a spurious task.finished(exit=64) before recovering to
				// cancelled. Disambiguate: a cancelled ctx means the close was
				// cancel-driven, so treat it as cancellation, not completion.
				if ctx.Err() != nil {
					return nil, drainCancelled
				}
				// Natural completion: Result is buffered (size 1) and populated
				// before the close, so this receive does not block.
				turn := <-handle.Result
				return turn, drainCompleted
			}
			relay(n)
		case <-ctx.Done():
			interruptTurn(handle)
			return nil, drainCancelled
		case <-deadlineCh:
			interruptTurn(handle)
			return nil, drainTimedOut
		}
	}
}

// interruptTurnTimeout bounds how long interruptTurn waits on the best-effort
// turn/interrupt before returning. Tuned to a few seconds: long enough that a
// responsive codex acknowledges the interrupt, short enough that the interrupt
// goroutine is guaranteed to terminate promptly rather than living for the
// broker's whole lifetime.
const interruptTurnTimeout = 3 * time.Second

// interruptTurn fires handle.Cancel() in the background. Cancel closes the
// per-turn channels synchronously (so the shared app-server reader unblocks and
// any peer turn keeps flowing), which is why the drain can return — and the run
// slot free — the instant interruptTurn is called. Cancel then issues a
// best-effort turn/interrupt RPC whose response a wedged codex can delay
// indefinitely. Cancel is idempotent, so a concurrent task.cancel invoking it
// too is harmless.
//
// The launched goroutine is bounded by interruptTurnTimeout via a
// context.WithTimeout deadline: it always returns within that window even when
// codex never answers, so a stream of cancelled/timed-out turns cannot
// accumulate goroutines that hang for the broker's entire lifetime. Cancel's
// turn/interrupt RPC is also bounded in the appserver layer.
func interruptTurn(handle *appserver.TurnHandle) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), interruptTurnTimeout)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			handle.Cancel()
		}()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}()
}

// turnToExit maps the Turn.Status enum to a Phase 1-compatible exit code.
// Unknown/nil turn → 64 ("unexpected codex failure").
func turnToExit(turn *appserver.Turn) (int, string) {
	if turn == nil {
		return 64, "codex exited without completing turn"
	}
	switch turn.Status {
	case "completed":
		return 0, ""
	case "failed":
		msg := "turn failed"
		if turn.Error != nil && turn.Error.Message != "" {
			msg = turn.Error.Message
		}
		return 2, msg
	case "cancelled":
		return 64, "turn cancelled"
	default:
		return 64, "unknown turn status: " + turn.Status
	}
}

// dispatchFailure marks the task errored, emits a synthetic broker error
// notification into stdout.log, and returns a DispatchRunResult with exit
// 64. Callers should return this directly.
func dispatchFailure(state *BrokerState, logW *LogWriter, taskID, msg string, emit func(string, any)) DispatchRunResult {
	_ = logW.WriteSyntheticError("broker/dispatch/error", msg)
	_ = state.Table.MarkErrored(taskID, 64, msg)
	emit("task.errored", map[string]any{"error": msg})
	return DispatchRunResult{
		TaskID:       taskID,
		State:        string(StateErrored),
		ExitCode:     64,
		EventCount:   countEvents(state.Table, taskID),
		ErrorMessage: msg,
	}
}

// preflightSandbox returns a non-empty, actionable failure message when the
// requested sandbox MODE cannot initialize its bubblewrap-based Linux sandbox on
// this host, so a dispatch fails fast instead of letting codex bury the cryptic
// `bwrap: ...` diagnostic in a command's output while the turn limps on with
// every shell command failing.
//
// It is a no-op (returns "") for the danger-full-access and empty modes, which
// never invoke bubblewrap. For a sandboxed mode it runs codex's command/exec
// probe fresh on each dispatch: the verdict is deliberately NOT cached so that
// the moment an operator follows the message's own advice — lifting the host's
// userns restriction — the very next dispatch succeeds without needing to
// restart the long-lived per-cwd broker (and a host hardened mid-broker-life is
// likewise caught rather than slipping past a stale "healthy" verdict). The
// probe is a threadless `true` exec that only runs for the uncommon sandboxed
// modes (the danger-full-access default skips it), so the cost is negligible.
//
// The probe FAILS OPEN: any error performing it (an older codex without
// command/exec, a transport failure) returns "" so the guard can only ever turn
// an already-doomed run into a clearer error — never block a run that would
// otherwise have worked.
func preflightSandbox(ctx context.Context, srv *appserver.AppServer, mode string) string {
	if mode == "" || mode == "danger-full-access" {
		return ""
	}
	res, err := srv.ProbeSandbox(ctx, mode)
	if err != nil {
		return "" // fail open; never introduce a new failure mode
	}
	if broken, detail := sandboxProbeBroken(res); broken {
		return sandboxBrokenMessage(mode, detail)
	}
	return ""
}

// sandboxProbeBroken interprets a ProbeSandbox result. bubblewrap prints a
// "bwrap: <reason>" line to stderr and the probe command never runs (nonzero
// exit) when it cannot set up the sandbox — most commonly because the host
// restricts the unprivileged user namespaces bwrap needs. A working sandbox runs
// `true` → exit 0, empty stderr. A nonzero exit WITHOUT a bwrap signature (e.g.
// the probe binary was not found on PATH inside a working sandbox) is treated as
// healthy so the guard stays conservative and only fires on a genuine bwrap
// failure.
func sandboxProbeBroken(res appserver.CommandExecResult) (bool, string) {
	if res.ExitCode == 0 {
		return false, ""
	}
	lower := strings.ToLower(res.Stderr)
	if strings.Contains(lower, "bwrap") ||
		strings.Contains(lower, "user namespace") ||
		strings.Contains(lower, "uid map") {
		return true, sandboxFirstLine(res.Stderr)
	}
	return false, ""
}

// sandboxBrokenMessage builds the operator-facing fail-fast message, folding in
// codex's own bwrap diagnostic when available.
func sandboxBrokenMessage(mode, detail string) string {
	msg := fmt.Sprintf("codex sandbox %q cannot initialize on this host", mode)
	if detail != "" {
		msg += ": " + detail
	}
	msg += ". The Linux sandbox uses bubblewrap (bwrap), which needs unprivileged " +
		"user namespaces this host restricts. Set CODEX_SANDBOX=danger-full-access " +
		"(the dispatch default) to run without the sandbox, or lift the restriction " +
		"(e.g. sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0)."
	return msg
}

// sandboxFirstLine returns the first non-empty, trimmed line of s so a compact
// error message can quote codex's multi-line bwrap stderr.
func sandboxFirstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return strings.TrimSpace(s)
}

func countEvents(table *Table, taskID string) int {
	st, err := table.Status(taskID)
	if err != nil {
		return 0
	}
	return st.EventCount
}

// Note: legacy dispatchAndRelay / relayEvent / drainEvents / logContainsStaleMarker
// were deleted with the appserver.dispatch invention. Stale detection now lives
// in the appserver layer (ErrStaleSession returned by ResumeThread).
