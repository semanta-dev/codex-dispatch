package broker

import (
	"context"
	"os"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/codex/appserver"
)

// AppServerFactory builds a fresh AppServer instance. Defaults to spawning
// real `codex app-server`; tests override it to inject in-process pipes.
type AppServerFactory func(cwd string) *appserver.AppServer

var defaultAppServerFactory AppServerFactory = func(cwd string) *appserver.AppServer {
	cmdPath := "codex"
	if v := os.Getenv("CODEX_BROKER_CODEX_BIN"); v != "" {
		cmdPath = v
	}
	return appserver.New(cmdPath, []string{"app-server"}, os.Environ(), cwd)
}

// SetAppServerFactory replaces the constructor used by EnsureAppServer.
// Returns the previous factory so callers can restore it on test teardown.
// Safe to call from one goroutine at a time (only used in tests).
func (b *BrokerState) SetAppServerFactory(f AppServerFactory) AppServerFactory {
	b.appserverMu.Lock()
	defer b.appserverMu.Unlock()
	prev := b.appserverFactory
	b.appserverFactory = f
	return prev
}

// EnsureAppServer returns a live AppServer, spawning one if needed or
// replacing a dead one. Idempotent and safe for concurrent callers.
//
// Recycle safety: when the current instance is dead it is DETACHED from the
// singleton slot and reaped in the BACKGROUND rather than Close-d synchronously
// under appserverMu. A synchronous Close would (a) hold appserverMu while
// reapChild waits up to ~4s for the child, stalling every peer dispatch, and
// (b) tear the shared codex child down while a peer turn that was in-flight on
// the same instance is still draining its per-turn pump — exactly the race this
// packet closes. reapInstance honours the instance's own teardown discipline
// (Close is idempotent via closeOnce; the reader has already exited on a dead
// instance) and only reaps once ActiveTurns() drains to zero, so a peer turn's
// reader is never raced by the recycle path.
func (b *BrokerState) EnsureAppServer(ctx context.Context) (*appserver.AppServer, error) {
	b.appserverMu.Lock()
	defer b.appserverMu.Unlock()
	if b.appserver != nil && !b.appserver.IsDead() {
		return b.appserver, nil
	}
	if b.appserver != nil {
		// Detach the dead instance and reap it off the lock once its peer turns
		// finish draining; never Close it inline under appserverMu.
		dead := b.appserver
		b.appserver = nil
		go reapInstance(dead)
	}
	factory := b.appserverFactory
	if factory == nil {
		factory = defaultAppServerFactory
	}
	a := factory(b.CWD)
	if err := a.Spawn(ctx); err != nil {
		return nil, err
	}
	b.appserver = a
	return a, nil
}

// reapInstance drains any still-referenced turns on a detached dead instance,
// then Closes it (reaps the child + joins the reader). Run in a background
// goroutine from EnsureAppServer (so the caller never blocks under appserverMu)
// and synchronously from CloseAppServer (so explicit teardown is complete on
// return). Waiting for ActiveTurns() to drain to zero before Close is what
// prevents the recycle/shutdown path from racing a peer turn's reader: on codex
// death the reader's failPendingRequests has already removed every turn, so this
// usually returns immediately; the bounded poll only guards the rare
// turn-mid-teardown interleaving. Close is idempotent (closeOnce), so a
// background reap and a later CloseAppServer touching the same instance is safe.
func reapInstance(a *appserver.AppServer) {
	if a == nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.ActiveTurns() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_ = a.Close(context.Background())
}

// CloseAppServer tears down the singleton if present. Called by the broker
// idle-out and shutdown paths. The instance is detached under the lock and
// reaped off it (drain peer turns, then Close) synchronously, so callers that
// expect the child gone on return — broker shutdown, test cleanup — get that
// guarantee, while the lock is never held across the multi-second child wait.
func (b *BrokerState) CloseAppServer(_ context.Context) {
	b.appserverMu.Lock()
	dead := b.appserver
	b.appserver = nil
	b.appserverMu.Unlock()
	reapInstance(dead)
}
