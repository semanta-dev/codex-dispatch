package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/codex/appserver"
)

// BrokerState bundles the dependencies handlers need. Passed by reference to
// the handler factories so we can wire them once in the broker subcommand.
type BrokerState struct {
	Table     *Table
	StartedAt string           // RFC3339 UTC
	Shutdown  func(force bool) // signals the server to exit; force=true cancels running tasks first
	CWD       string           // working directory passed to spawned codex children

	schedulerOnce sync.Once
	scheduler     chan struct{}
	taskMu        sync.Mutex
	taskCancels   map[string]*taskCancelRegistration

	// AppServer singleton, populated lazily by EnsureAppServer on first
	// dispatch and replaced if it dies. CloseAppServer tears it down on
	// broker idle-out / shutdown. appserverFactory is overrideable for tests.
	appserverMu      sync.Mutex
	appserver        *appserver.AppServer
	appserverFactory AppServerFactory
}

type taskCancelRegistration struct {
	cancel context.CancelFunc

	// handle is the in-flight codex TurnHandle for the task, registered by
	// the dispatch drain once turn/start succeeds. cancelTask calls
	// handle.Cancel() (which sends turn/interrupt and closes the per-turn
	// channels) so a task.cancel interrupts the SPECIFIC running turn rather
	// than only cancelling a ctx the drain might be blocked outside of.
	handle interface{ Cancel() }
}

func (b *BrokerState) acquireRunSlot(ctx context.Context) error {
	b.schedulerOnce.Do(func() {
		capacity := 1
		if b.Table != nil {
			capacity = b.Table.ConcurrencyCap()
		}
		b.scheduler = make(chan struct{}, capacity)
	})
	select {
	case b.scheduler <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *BrokerState) releaseRunSlot() {
	if b.scheduler == nil {
		return
	}
	select {
	case <-b.scheduler:
	default:
	}
}

func (b *BrokerState) registerTaskContext(parent context.Context, taskID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	b.taskMu.Lock()
	if b.taskCancels == nil {
		b.taskCancels = map[string]*taskCancelRegistration{}
	}
	reg := &taskCancelRegistration{cancel: cancel}
	b.taskCancels[taskID] = reg
	b.taskMu.Unlock()

	cleanup := func() {
		b.taskMu.Lock()
		if b.taskCancels[taskID] == reg {
			delete(b.taskCancels, taskID)
		}
		b.taskMu.Unlock()
		cancel()
	}
	return ctx, cleanup
}

func (b *BrokerState) cancelTask(taskID string) {
	// Capture BOTH cancel and handle while still holding taskMu. reg.handle is
	// written under taskMu by registerTurnHandle on a concurrent goroutine (the
	// dispatch drain, once turn/start succeeds), so reading reg.handle after
	// dropping the lock would be a data race against that write. Snapshot it
	// here into a local and use the local below. handle may be nil if the turn
	// has not started yet — that is fine: cancelling the ctx still unblocks the
	// task before/during thread/turn start.
	b.taskMu.Lock()
	reg := b.taskCancels[taskID]
	if reg == nil {
		b.taskMu.Unlock()
		return
	}
	cancel := reg.cancel
	handle := reg.handle
	b.taskMu.Unlock()
	// Cancel the ctx FIRST, then interrupt the turn. Ordering is load-bearing:
	// handle.Cancel() closes the per-turn channels, which races the drain's
	// select arms — the `case n,ok := <-events; if !ok` arm can win over
	// `case <-ctx.Done()`. drainTurn disambiguates a cancel-driven close from
	// natural completion via ctx.Err(); for that guard to be reliable the ctx
	// must already be cancelled before the channel-closing goroutine is even
	// spawned. cancel() is sequenced-before the `go` below, so by the time
	// events can close, ctx.Err() is guaranteed non-nil. (cancel also unblocks
	// callers waiting outside the drain — e.g. queued for a run slot or mid
	// thread/start.) handle.Cancel runs in a goroutine so task.cancel returns
	// promptly even when the turn/interrupt RPC is wedged; Cancel is idempotent,
	// so the drain's own deadline path calling it too is harmless.
	cancel()
	if handle != nil {
		go handle.Cancel()
	}
}

// registerTurnHandle attaches the in-flight TurnHandle to a task's cancel
// registration so cancelTask can interrupt the specific turn. Safe to call at
// most once per registration; ignored if the registration has been cleaned up.
func (b *BrokerState) registerTurnHandle(taskID string, handle interface{ Cancel() }) {
	b.taskMu.Lock()
	if reg := b.taskCancels[taskID]; reg != nil {
		reg.handle = handle
	}
	b.taskMu.Unlock()
}

// turnTimeout returns the per-turn deadline from CODEX_BROKER_TURN_TIMEOUT_MS.
// A value <= 0 (or unset/unparseable) disables the deadline (default: off), so
// existing behavior is unchanged unless an operator opts in. A wedged turn that
// never reaches turn/completed is interrupted once this deadline elapses,
// freeing the run slot rather than pinning it forever.
func turnTimeout() time.Duration {
	if s := os.Getenv("CODEX_BROKER_TURN_TIMEOUT_MS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 0
}

// HandleBrokerPing returns a Handler that reports broker version and counts.
func HandleBrokerPing(state *BrokerState) Handler {
	return func(_ context.Context, _ json.RawMessage) (any, error) {
		all := state.Table.List("")
		return map[string]any{
			"version":       "1.0.0",
			"started_at":    state.StartedAt,
			"task_count":    len(all),
			"running_count": state.Table.RunningCount(),
		}, nil
	}
}

// HandleBrokerShutdown returns a Handler that triggers a graceful exit. With
// force=false, refuses if running_count > 0.
func HandleBrokerShutdown(state *BrokerState) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var params struct {
			Force bool `json:"force"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, fmt.Errorf("invalid params: %w", err)
			}
		}
		if !params.Force && state.Table.RunningCount() > 0 {
			return nil, &RPCError{Code: -32001, Message: "running_count > 0 (use force=true to cancel)"}
		}
		// Signal shutdown AFTER the response has been written. Using AfterFunc
		// gives the response time to flush through the socket before the
		// listener tears down and the OS drops in-flight bytes.
		time.AfterFunc(10*time.Millisecond, func() {
			state.Shutdown(params.Force)
		})
		return map[string]any{"ok": true}, nil
	}
}

// ToRPCError walks an error chain looking for an *RPCError. Returns nil when none.
func ToRPCError(err error) *RPCError {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	return nil
}
