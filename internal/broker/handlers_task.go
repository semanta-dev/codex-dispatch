package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// formatTaskTimestamp renders a task timestamp as RFC3339 in UTC. Callers omit
// the field entirely for a zero time (e.g. a queued task has no started_at yet)
// rather than emitting a misleading zero-value timestamp.
func formatTaskTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// HandleTaskList returns a Handler for task.list.
func HandleTaskList(state *BrokerState) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var params struct {
			SessionID string `json:"session_id"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, fmt.Errorf("invalid params: %w", err)
			}
		}
		tasks := state.Table.List(params.SessionID)
		out := make([]map[string]any, 0, len(tasks))
		for _, task := range tasks {
			entry := map[string]any{
				"task_id": task.ID,
				"state":   string(task.State),
			}
			if !task.StartedAt.IsZero() {
				entry["started_at"] = formatTaskTimestamp(task.StartedAt)
			}
			if !task.FinishedAt.IsZero() {
				entry["finished_at"] = formatTaskTimestamp(task.FinishedAt)
			}
			out = append(out, entry)
		}
		return map[string]any{"tasks": out}, nil
	}
}

// HandleTaskStatus returns a Handler for task.status.
func HandleTaskStatus(state *BrokerState) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var params struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		task, err := state.Table.Status(params.TaskID)
		if errors.Is(err, ErrTaskNotFound) {
			return nil, &RPCError{Code: -32001, Message: err.Error()}
		}
		if err != nil {
			return nil, err
		}
		// task.status is the canonical observable contract for a DETACHED run:
		// unlike a synchronous dispatch.run it writes no result.json and computes
		// no diff, so files_changed / lines_* are unavailable here. exit_code
		// (including the no-edits exit_code=4 a future packet may set),
		// session_id, fell_back_to_fresh, event_count and error_message are the
		// machine-readable fields a detached observer relies on. README documents
		// exactly this shape.
		result := map[string]any{
			"task_id":            task.ID,
			"state":              string(task.State),
			"event_count":        task.EventCount,
			"fell_back_to_fresh": task.FellBackToFresh,
		}
		if !task.StartedAt.IsZero() {
			result["started_at"] = formatTaskTimestamp(task.StartedAt)
		}
		if !task.FinishedAt.IsZero() {
			result["finished_at"] = formatTaskTimestamp(task.FinishedAt)
			result["exit_code"] = task.ExitCode
		}
		if task.CodexSession != "" {
			result["session_id"] = task.CodexSession
		}
		if task.ErrorMessage != "" {
			result["error_message"] = task.ErrorMessage
		}
		return result, nil
	}
}

// HandleTaskStart returns a Handler for task.start. Returns
// {task_id, queued} immediately and runs the dispatch in the background
// (no notifier; events are only persisted to the table + log file).
func HandleTaskStart(state *BrokerState) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var p DispatchRunParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		taskID, queued := state.Table.Start(p.SessionID, TaskParams{
			Mode:        p.Mode,
			Prompt:      p.Prompt,
			Sandbox:     p.Sandbox,
			PrevSession: p.PrevSessionID,
			ResultDir:   p.ResultDir,
			LogPath:     p.LogPath,
			CWD:         p.CWD,
		})
		// Track the background goroutine on the broker-lifetime DetachedRunner
		// (if wired) so broker shutdown/idle-out can drain it instead of yanking
		// the codex child mid-run. DetachedRunner.Go falls back to a plain
		// goroutine when no runner is installed (bare-Table tests).
		state.Table.DetachedRunner().Go(func() { runDetached(state, taskID, p) })
		return map[string]any{"task_id": taskID, "queued": queued}, nil
	}
}

// runDetached drives the dispatch in the background for task.start. No
// notifier; events are persisted to the table + log only. Its context derives
// from the broker-lifetime DetachedRunner context (NOT context.Background()),
// so broker shutdown/idle-out cancellation reaches the detached drain and frees
// the run slot rather than leaving an orphaned turn pinned to a dead broker.
func runDetached(state *BrokerState, taskID string, p DispatchRunParams) {
	parent := state.Table.DetachedRunner().Context()
	ctx, cleanupTask := state.registerTaskContext(parent, taskID)
	defer cleanupTask()
	if st, serr := state.Table.Status(taskID); serr == nil && st.State == StateCancelled {
		return
	}
	if err := state.acquireRunSlot(ctx); err != nil {
		if st, serr := state.Table.Status(taskID); serr == nil && st.State == StateCancelled {
			return
		}
		_ = state.Table.MarkErrored(taskID, 64, err.Error())
		return
	}
	defer state.releaseRunSlot()
	if err := state.Table.MarkRunning(taskID); err != nil {
		return
	}
	logW, err := OpenLogWriter(p.LogPath)
	if err != nil {
		_ = state.Table.MarkErrored(taskID, -1, err.Error())
		return
	}
	defer logW.Close()
	_ = runDispatchOn(ctx, state, taskID, p, logW, nil)
}

// HandleTaskCancel returns a Handler for task.cancel.
func HandleTaskCancel(state *BrokerState) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var params struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		err := state.Table.Cancel(params.TaskID)
		if errors.Is(err, ErrTaskNotFound) {
			return nil, &RPCError{Code: -32001, Message: err.Error()}
		}
		if errors.Is(err, ErrTaskAlreadyTerminal) {
			return nil, &RPCError{Code: -32007, Message: err.Error()}
		}
		if err != nil {
			return nil, err
		}
		state.cancelTask(params.TaskID)
		return map[string]any{"ok": true}, nil
	}
}
