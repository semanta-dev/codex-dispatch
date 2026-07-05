package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/broker"
	"github.com/semanta-dev/codex-dispatch/internal/codex"
)

func runBroker(_ []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
	repoRoot, err := codex.RepoRoot("")
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: broker: %v\n", err)
		return 1
	}
	addrPath := codex.BrokerAddrPath(repoRoot)
	dir := filepath.Dir(addrPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: broker: %v\n", err)
		return 1
	}
	pidPath := filepath.Join(dir, "broker.pid")
	release, err := broker.AcquirePIDFile(pidPath)
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: broker: %v\n", err)
		return 1
	}
	defer release()

	srv := broker.NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)

	ctx, cancel := context.WithCancel(context.Background())

	table := broker.NewTable(brokerCap(), brokerRingSize())
	// Bind detached (task.start) goroutines to the broker lifetime: their
	// contexts derive from the serve ctx (cancelled below on shutdown/idle-out)
	// and the runner's WaitGroup lets us drain in-flight detached runs before
	// closing the shared codex child, so a detached turn is never yanked out
	// from under itself.
	detached := broker.NewDetachedRunner(ctx)
	table.SetDetachedRunner(detached)
	state := &broker.BrokerState{
		Table:     table,
		CWD:       repoRoot,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Version:   version,
	}

	// shutdown sequences teardown so a streaming handler is never severed from
	// the codex child it is mid-drain on:
	//
	//  1. cancel() the serve context FIRST. Every running sync drain and detached
	//     drain observes ctx.Done(), fires handle.Cancel() (turn/interrupt +
	//     per-turn channel close), releases its run slot, and unregisters its turn
	//     from the shared app-server's turns map.
	//  2. detached.Wait() drains in-flight DETACHED (task.start) goroutines, which
	//     run outside the HTTP serve loop and are not bounded by its 2s
	//     httpSrv.Shutdown grace.
	//  3. CloseAppServer() last. It does NOT yank the child the instant Wait()
	//     returns: it waits for the app-server's in-flight turns to drain to zero
	//     before reaping, which reconciles the 2s httpSrv.Shutdown grace with
	//     synchronous streaming dispatch.run handlers that can legitimately run
	//     for minutes — a sync handler still inside its drain keeps its turn
	//     registered, so the child stays up until that drain unregisters it. This
	//     closes the window where the long-lived codex child could be closed out
	//     from under a peer turn's reader during shutdown.
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			cancel()
			detached.Wait()
			state.CloseAppServer(context.Background())
		})
	}
	// Shutdown runs the teardown asynchronously so the broker.shutdown RPC
	// response can flush before the listener tears down (HandleBrokerShutdown
	// already defers the call via time.AfterFunc).
	state.Shutdown = func(_ bool) { go shutdown() }

	// Idle-out fires only when the broker has been quiet AND has no outstanding
	// work. A non-terminal task (queued waiting for a slot, or a long/silent
	// running turn) re-arms the timer instead of shutting down, so idle-out can
	// never kill an active or long task — or the whole broker mid-dispatch.
	// AppendEvent also resets the timer on every streamed notification, so a
	// busy turn keeps the broker alive even with a tiny CODEX_BROKER_IDLE_MS.
	// idle is declared before its callback so the guard can re-arm it (Reset).
	idleTimeout := brokerIdleTimeout()
	var idle *broker.IdleTimer
	idle = broker.NewIdleTimer(idleTimeout, func() {
		if table.HasNonTerminal() {
			idle.Reset()
			return
		}
		fmt.Fprintf(stderr, "codex-dispatch: broker: idle for %s, shutting down\n", idleTimeout)
		shutdown()
	})
	table.SetOnActivity(idle.Reset)
	idle.Start()
	defer idle.Stop()
	wrap := func(_ string, h broker.Handler) broker.Handler {
		return func(ctx context.Context, raw json.RawMessage) (any, error) {
			idle.Reset()
			return h(ctx, raw)
		}
	}
	srv.HandleFunc("broker.ping", wrap("broker.ping", broker.HandleBrokerPing(state)))
	srv.HandleFunc("broker.shutdown", wrap("broker.shutdown", broker.HandleBrokerShutdown(state)))
	srv.HandleFunc("session.register", wrap("session.register", broker.HandleSessionRegister(state)))
	srv.HandleFunc("session.deregister", wrap("session.deregister", broker.HandleSessionDeregister(state)))
	srv.HandleFunc("task.list", wrap("task.list", broker.HandleTaskList(state)))
	srv.HandleFunc("task.status", wrap("task.status", broker.HandleTaskStatus(state)))
	srv.HandleFunc("task.cancel", wrap("task.cancel", broker.HandleTaskCancel(state)))
	srv.HandleFunc("task.start", wrap("task.start", broker.HandleTaskStart(state)))
	srv.HandleFunc("dispatch.run", wrap("dispatch.run", broker.HandleDispatchRun(state)))
	// shutdown is idempotent (sync.Once); deferring it guarantees the detached
	// drain runs and the codex child is closed on every exit path.
	defer shutdown()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			// On signal, run the full shutdown (cancel → drain detached → close
			// child) rather than a bare cancel, so a SIGTERM mid-detached-run
			// does not leave an orphaned codex child or yank it mid-drain.
			shutdown()
		case <-ctx.Done():
		}
	}()

	if err := srv.Serve(ctx); err != nil && err != context.Canceled {
		fmt.Fprintf(stderr, "codex-dispatch: broker: %v\n", err)
		return 1
	}
	return 0
}

// brokerCap returns the concurrency cap from CODEX_BROKER_MAX_CONCURRENT (default 8).
func brokerCap() int {
	if s := os.Getenv("CODEX_BROKER_MAX_CONCURRENT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 8
}

// brokerRingSize returns the per-task event ring size from
// CODEX_BROKER_RING_SIZE (default 2048).
func brokerRingSize() int {
	if s := os.Getenv("CODEX_BROKER_RING_SIZE"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 2048
}

// brokerIdleTimeout returns the broker idle timeout from CODEX_BROKER_IDLE_MS
// (default 300_000 ms = 5 min).
func brokerIdleTimeout() time.Duration {
	if s := os.Getenv("CODEX_BROKER_IDLE_MS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 5 * time.Minute
}
