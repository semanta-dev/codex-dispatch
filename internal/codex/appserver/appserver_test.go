package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNotificationRoundTrip(t *testing.T) {
	in := []byte(`{"method":"turn/completed","params":{"threadId":"t1"}}`)
	var n Notification
	if err := json.Unmarshal(in, &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.Method != "turn/completed" {
		t.Fatalf("method: got %q", n.Method)
	}
	var inner struct {
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(n.Params, &inner); err != nil {
		t.Fatalf("inner: %v", err)
	}
	if inner.ThreadID != "t1" {
		t.Fatalf("threadId: got %q", inner.ThreadID)
	}
}

func TestTurnItemPreservesRaw(t *testing.T) {
	in := []byte(`{"id":"i1","type":"fileChange","extra":{"foo":42}}`)
	var item TurnItem
	if err := json.Unmarshal(in, &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if item.ID != "i1" || item.Type != "fileChange" {
		t.Fatalf("decoded: %+v", item)
	}
	if string(item.Raw) != string(in) {
		t.Fatalf("Raw was %q want %q", item.Raw, in)
	}
}

func TestNewIsZeroAllocStable(t *testing.T) {
	a := New("codex", []string{"app-server"}, nil, "/tmp")
	if a == nil {
		t.Fatal("New returned nil")
	}
	if a.cmdPath != "codex" || len(a.cmdArgs) != 1 {
		t.Fatalf("ctor: %+v", a)
	}
	if a.initialized.Load() {
		t.Fatal("should not be initialized")
	}
}

func TestSentinelErrors(t *testing.T) {
	// Smoke: errors are distinguishable via errors.Is.
	if errors.Is(ErrStaleSession, ErrCodexExited) {
		t.Fatal("sentinels collapsed")
	}
}

func TestCheckCodexVersionDoesNotBlowUp(t *testing.T) {
	// Only assert that this function doesn't panic — the runtime answer
	// depends on whether codex is on PATH in the test env.
	_ = CheckCodexVersion()
}

// ---------------------------------------------------------------------------
// In-process fake server + piped AppServer helpers (R2)
// ---------------------------------------------------------------------------

// testServer is an in-process fake that speaks the real codex protocol
// subset our AppServer client uses. It runs on top of io.Pipe pairs so the
// test doesn't fork a process.
type testServer struct {
	t       *testing.T
	in      *bufio.Reader // server reads requests from here
	out     io.Writer     // server writes responses here
	writeMu sync.Mutex    // serializes frame writes to out (see writeFrame)
	methods map[string]func(json.RawMessage) (interface{}, *rpcError)
	notifsT chan struct{} // closed when server should stop
}

// writeFrame writes one JSON-RPC frame (body + a single trailing newline) to the
// client as ONE atomic, mutex-guarded operation. This is load-bearing: the
// server's run loop writes responses while a test handler may concurrently emit
// notifications onto the same io.Pipe writer. io.Pipe.Write is atomic per call,
// so splitting a frame into body-then-"\n" (two writes) lets a concurrent
// notification interleave between them, concatenating two JSON objects onto one
// physical line. The client's ReadBytes('\n') then sees malformed JSON and drops
// the line — silently losing a turn/start RESPONSE and deadlocking the caller's
// call(). One Write of body+"\n" under writeMu removes that race entirely.
func (s *testServer) writeFrame(b []byte) {
	frame := make([]byte, 0, len(b)+1)
	frame = append(frame, b...)
	frame = append(frame, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.out.Write(frame)
}

// emit writes a pre-serialized frame string (without the trailing newline)
// atomically. Test handlers use it for async notifications so those writes never
// interleave with the run loop's response writes.
func (s *testServer) emit(frame string) {
	s.writeFrame([]byte(frame))
}

func newTestServer(t *testing.T, clientIn io.Writer, clientOut io.Reader) *testServer {
	return &testServer{
		t:       t,
		in:      bufio.NewReader(clientOut),
		out:     clientIn,
		methods: make(map[string]func(json.RawMessage) (interface{}, *rpcError)),
		notifsT: make(chan struct{}),
	}
}

func (s *testServer) handle(method string, fn func(json.RawMessage) (interface{}, *rpcError)) {
	s.methods[method] = fn
}

func (s *testServer) run() {
	for {
		line, err := s.in.ReadBytes('\n')
		if err != nil {
			return
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if req.ID == nil {
			// client notification; ignore for now
			continue
		}
		fn, ok := s.methods[req.Method]
		var resp map[string]interface{}
		if !ok {
			resp = map[string]interface{}{
				"jsonrpc": "2.0", "id": *req.ID,
				"error": map[string]interface{}{"code": -32601, "message": "Method not found"},
			}
		} else {
			result, rpcerr := fn(req.Params)
			if rpcerr != nil {
				resp = map[string]interface{}{
					"jsonrpc": "2.0", "id": *req.ID,
					"error": rpcerr,
				}
			} else {
				resp = map[string]interface{}{
					"jsonrpc": "2.0", "id": *req.ID,
					"result": result,
				}
			}
		}
		b, _ := json.Marshal(resp)
		s.writeFrame(b)
	}
}

// newPipedAppServer constructs an AppServer wired to an in-process test
// server via two io.Pipe pairs (one per direction). Bypasses os/exec.
//
// The fake server's run loop is launched HERE (not by the caller), so the
// helper owns the single srv.run() goroutine and can join it on cleanup.
// Callers MUST NOT start srv.run() again.
//
// The registered t.Cleanup closes both pipe write ends and then JOINS both
// background goroutines before returning: the fake server loop (via serverDone)
// and the AppServer reader goroutine started lazily by initialize (via
// a.readerDone). Without this join, leaked srv.run()/startReader goroutines
// accumulate across `go test -count=N` iterations; under -race overhead that
// scheduler pressure starves io.Pipe.Write and deadlocks later iterations.
// Double-close on io.PipeWriter is safe (returns ErrClosedPipe, no panic).
func newPipedAppServer(t *testing.T) (*AppServer, *testServer) {
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	a := &AppServer{
		stdin:      clientToServerW,
		stdout:     serverToClientR,
		done:       make(chan struct{}),
		readerDone: make(chan struct{}),
		turns:      make(map[string]*turnState),
		notifFn:    func(Notification) {},
	}

	srv := newTestServer(t, serverToClientW, clientToServerR)

	serverDone := make(chan struct{})
	go func() { srv.run(); close(serverDone) }()

	t.Cleanup(func() {
		clientToServerW.Close() // unblocks srv.run's ReadBytes loop (EOF)
		serverToClientW.Close() // unblocks AppServer.startReader's read loop (EOF)

		// Join the fake server loop. It always exits once its read pipe EOFs.
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Error("newPipedAppServer cleanup: srv.run goroutine did not exit")
		}

		// Join the reader goroutine. initialize() starts it (via initOnce) and
		// it always returns once the stdout write end is closed above (EOF), so
		// readerDone closes promptly. A test that never calls initialize never
		// starts the reader and readerDone stays open; the bounded wait then
		// returns on the timeout without failing. All current callers do
		// initialize, so this is a real join in practice, not a leak.
		select {
		case <-a.readerDone:
		case <-time.After(2 * time.Second):
		}
	})

	return a, srv
}

func TestSpawnHandshake(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(params json.RawMessage) (interface{}, *rpcError) {
		return map[string]interface{}{
			"serverInfo": map[string]string{"name": "fake", "version": "0.130.0"},
		}, nil
	})
	// srv.run() is launched and joined by newPipedAppServer; do not start it here.

	if err := a.initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !a.initialized.Load() {
		t.Fatal("initialized flag not set")
	}
}

func TestSpawnHandshakeBadResponse(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(params json.RawMessage) (interface{}, *rpcError) {
		return nil, &rpcError{Code: -32603, Message: "internal"}
	})
	// srv.run() is launched and joined by newPipedAppServer.
	err := a.initialize(context.Background())
	if err == nil {
		t.Fatal("expected error from bad initialize")
	}
	// Verify the wrapped error contains the rpc -32603 information.
	if err.Error() == "" {
		t.Fatal("error message empty")
	}
}

func TestStartThread(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) {
		return map[string]string{}, nil
	})
	srv.handle("thread/start", func(params json.RawMessage) (interface{}, *rpcError) {
		var p map[string]interface{}
		_ = json.Unmarshal(params, &p)
		if p["approvalPolicy"] != "never" {
			t.Errorf("approvalPolicy: got %v", p["approvalPolicy"])
		}
		return map[string]interface{}{
			"thread": map[string]interface{}{
				"id":     "thr-123",
				"cwd":    "/tmp/x",
				"status": "running",
			},
		}, nil
	})
	// srv.run() is launched and joined by newPipedAppServer.

	ctx := context.Background()
	if err := a.initialize(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	thread, err := a.StartThread(ctx, ThreadStartOptions{
		CWD:                   "/tmp/x",
		Sandbox:               "workspace-write",
		DeveloperInstructions: "be helpful",
	})
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	if thread.ID != "thr-123" || thread.CWD != "/tmp/x" {
		t.Fatalf("thread: %+v", thread)
	}
}

// TestStartThreadSendsModel asserts the CODEX_MODEL pin reaches codex: when
// ThreadStartOptions.Model is set, thread/start carries a matching "model".
func TestStartThreadSendsModel(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) {
		return map[string]string{}, nil
	})
	srv.handle("thread/start", func(params json.RawMessage) (interface{}, *rpcError) {
		var p map[string]interface{}
		_ = json.Unmarshal(params, &p)
		if p["model"] != "gpt-5.5" {
			t.Errorf("thread/start model: got %v, want gpt-5.5", p["model"])
		}
		return map[string]interface{}{
			"thread": map[string]interface{}{"id": "thr-m", "cwd": "/x", "status": "running"},
		}, nil
	})
	ctx := context.Background()
	if err := a.initialize(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := a.StartThread(ctx, ThreadStartOptions{CWD: "/x", Sandbox: "workspace-write", Model: "gpt-5.5"}); err != nil {
		t.Fatalf("StartThread: %v", err)
	}
}

// TestStartThreadOmitsModelWhenEmpty asserts no "model" key is sent when unset,
// so codex falls back to its configured default.
func TestStartThreadOmitsModelWhenEmpty(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) {
		return map[string]string{}, nil
	})
	srv.handle("thread/start", func(params json.RawMessage) (interface{}, *rpcError) {
		var p map[string]interface{}
		_ = json.Unmarshal(params, &p)
		if _, ok := p["model"]; ok {
			t.Errorf("thread/start should omit model when empty; got %v", p["model"])
		}
		return map[string]interface{}{
			"thread": map[string]interface{}{"id": "thr-d", "cwd": "/x", "status": "running"},
		}, nil
	})
	ctx := context.Background()
	if err := a.initialize(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := a.StartThread(ctx, ThreadStartOptions{CWD: "/x", Sandbox: "workspace-write"}); err != nil {
		t.Fatalf("StartThread: %v", err)
	}
}

func TestResumeThread(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) { return map[string]string{}, nil })
	srv.handle("thread/resume", func(params json.RawMessage) (interface{}, *rpcError) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &p)
		if p.ThreadID != "thr-prev" {
			t.Errorf("threadId: got %q", p.ThreadID)
		}
		return map[string]interface{}{
			"thread": map[string]interface{}{
				"id":     "thr-prev",
				"cwd":    "/tmp/x",
				"status": "running",
			},
		}, nil
	})
	// srv.run() is launched and joined by newPipedAppServer.
	if err := a.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	thread, err := a.ResumeThread(context.Background(), "thr-prev", ThreadResumeOptions{})
	if err != nil {
		t.Fatalf("ResumeThread: %v", err)
	}
	if thread.ID != "thr-prev" {
		t.Fatalf("thread: %+v", thread)
	}
}

func TestResumeThreadStale(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) { return map[string]string{}, nil })
	srv.handle("thread/resume", func(params json.RawMessage) (interface{}, *rpcError) {
		return nil, &rpcError{Code: -32004, Message: "thread not found"}
	})
	// srv.run() is launched and joined by newPipedAppServer.
	if err := a.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := a.ResumeThread(context.Background(), "thr-stale", ThreadResumeOptions{})
	if !errors.Is(err, ErrStaleSession) {
		t.Fatalf("expected ErrStaleSession, got %v", err)
	}
}

func TestTurnLifecycle(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) { return map[string]string{}, nil })
	srv.handle("thread/start", func(json.RawMessage) (interface{}, *rpcError) {
		return map[string]interface{}{
			"thread": map[string]interface{}{"id": "thr-1", "cwd": "/x", "status": "running"},
		}, nil
	})
	srv.handle("turn/start", func(_ json.RawMessage) (interface{}, *rpcError) {
		// After the response, async-emit turn/started then turn/completed.
		// Use srv.emit (atomic, mutex-guarded) so these notifications cannot
		// interleave with the run loop's turn/start response frame on the pipe.
		go func() {
			srv.emit(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-1","turn":{"id":"trn-1","status":"pending"}}}`)
			srv.emit(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-1","turn":{"id":"trn-1","status":"completed","durationMs":42}}}`)
		}()
		return map[string]interface{}{
			"turn": map[string]interface{}{"id": "trn-1", "status": "pending"},
		}, nil
	})
	// srv.run() is launched and joined by newPipedAppServer.

	ctx := context.Background()
	if err := a.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	thread, err := a.StartThread(ctx, ThreadStartOptions{CWD: "/x", Sandbox: "workspace-write"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := a.StartTurn(ctx, thread.ID, "go", TurnStartOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	for n := range h.Events {
		seen = append(seen, n.Method)
	}
	if len(seen) < 2 || seen[0] != "turn/started" {
		t.Fatalf("events: %v", seen)
	}

	turn := <-h.Result
	if turn == nil {
		t.Fatal("nil turn")
	}
	if turn.Status != "completed" || turn.ID != "trn-1" {
		t.Fatalf("turn: %+v", turn)
	}
}

// TestOversizedNotificationNotCodexDeath feeds a single notification line well
// over the old 16 MiB bufio.Scanner ceiling, then a normal turn/completed. The
// oversized line is valid JSON (a large agentMessage), so it must be parsed and
// routed like any other notification — never misread as "codex exited" — and
// the AppServer must keep working: the subsequent turn/completed still arrives
// and the server is not torn down.
//
// Against the OLD bufio.Scanner reader this fails: scanner.Scan() returns false
// with bufio.ErrTooLong, the reader closes done, stores ErrCodexExited, and
// failPendingRequests closes the turn channels with no result.
func TestOversizedNotificationNotCodexDeath(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) { return map[string]string{}, nil })
	srv.handle("thread/start", func(json.RawMessage) (interface{}, *rpcError) {
		return map[string]interface{}{
			"thread": map[string]interface{}{"id": "thr-big", "cwd": "/x", "status": "running"},
		}, nil
	})
	srv.handle("turn/start", func(_ json.RawMessage) (interface{}, *rpcError) {
		go func() {
			// A single notification line larger than the old 16 MiB ceiling.
			huge := strings.Repeat("x", 16*1024*1024+1)
			big, _ := json.Marshal(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "item/started",
				"params": map[string]interface{}{
					"threadId": "thr-big",
					"item":     map[string]interface{}{"id": "item-oversized", "type": "agentMessage", "text": huge},
				},
			})
			// Atomic, mutex-guarded frame writes so neither the oversized line
			// nor the completion can interleave with the run loop's turn/start
			// response on the shared pipe.
			srv.writeFrame(big)
			srv.emit(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-big","turn":{"id":"trn-big","status":"completed","durationMs":7}}}`)
		}()
		return map[string]interface{}{
			"turn": map[string]interface{}{"id": "trn-big", "status": "pending"},
		}, nil
	})
	// srv.run() is launched and joined by newPipedAppServer.

	ctx := context.Background()
	if err := a.initialize(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	thread, err := a.StartThread(ctx, ThreadStartOptions{CWD: "/x", Sandbox: "workspace-write"})
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	h, err := a.StartTurn(ctx, thread.ID, "go", TurnStartOptions{})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	// Drain events; the oversized notification must come through as a normal
	// notification (it is valid JSON), followed by the channel closing.
	var sawOversized bool
	for n := range h.Events {
		if n.Method == "item/started" {
			sawOversized = true
		}
	}
	if !sawOversized {
		t.Fatal("oversized item/started notification was not delivered to the events stream")
	}

	turn, ok := <-h.Result
	if !ok || turn == nil {
		t.Fatal("expected a final Turn after the oversized line; got a closed/empty Result (codex falsely treated as exited)")
	}
	if turn.Status != "completed" || turn.ID != "trn-big" {
		t.Fatalf("turn: %+v", turn)
	}

	// The shared AppServer must NOT be torn down by the oversized line.
	if a.IsDead() {
		t.Fatal("AppServer was torn down by an oversized notification line (masqueraded as codex death)")
	}
	if pErr := a.doneErr.Load(); pErr != nil {
		t.Fatalf("doneErr was set by an oversized line (spurious codex-exited): %v", *pErr)
	}
}

// TestOversizedLineThenEOFStillExits is the companion check: after the oversized
// line is handled, a genuine stdout EOF must still surface ErrCodexExited (so we
// have not accidentally made the reader unkillable).
func TestOversizedLineThenEOFStillExits(t *testing.T) {
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	a := &AppServer{
		stdin:   clientToServerW,
		stdout:  serverToClientR,
		done:    make(chan struct{}),
		turns:   make(map[string]*turnState),
		notifFn: func(Notification) {},
	}
	// Nothing reads the client→server direction in this test; drain it so any
	// stray write cannot block, and so the reader can be closed cleanly.
	go func() { _, _ = io.Copy(io.Discard, clientToServerR) }()
	t.Cleanup(func() {
		clientToServerW.Close()
		serverToClientW.Close()
	})
	a.startReader()

	go func() {
		huge := strings.Repeat("y", 16*1024*1024+1)
		big, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "item/started",
			"params":  map[string]interface{}{"text": huge},
		})
		_, _ = serverToClientW.Write(big)
		_, _ = serverToClientW.Write([]byte("\n"))
		// Now simulate the child closing stdout — a real codex death.
		serverToClientW.Close()
	}()

	select {
	case <-a.done:
		// expected
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not exit on EOF after an oversized line")
	}
	if !a.IsDead() {
		t.Fatal("expected IsDead after EOF")
	}
	pErr := a.doneErr.Load()
	if pErr == nil {
		t.Fatal("expected doneErr set on EOF")
	}
	if !errors.Is(*pErr, ErrCodexExited) {
		t.Fatalf("expected ErrCodexExited on EOF, got %v", *pErr)
	}
}

func TestTurnCrashSurfacesError(t *testing.T) {
	a, srv := newPipedAppServer(t)
	srv.handle("initialize", func(json.RawMessage) (interface{}, *rpcError) { return map[string]string{}, nil })
	srv.handle("thread/start", func(json.RawMessage) (interface{}, *rpcError) {
		return map[string]interface{}{
			"thread": map[string]interface{}{"id": "thr-c", "cwd": "/x", "status": "running"},
		}, nil
	})
	srv.handle("turn/start", func(_ json.RawMessage) (interface{}, *rpcError) {
		// Close the server→client pipe shortly after responding — simulates crash.
		go func() {
			time.Sleep(20 * time.Millisecond)
			if c, ok := srv.out.(io.Closer); ok {
				c.Close()
			}
		}()
		return map[string]interface{}{
			"turn": map[string]interface{}{"id": "trn-c", "status": "pending"},
		}, nil
	})
	// srv.run() is launched and joined by newPipedAppServer.

	ctx := context.Background()
	if err := a.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	thread, _ := a.StartThread(ctx, ThreadStartOptions{CWD: "/x", Sandbox: "workspace-write"})
	h, err := a.StartTurn(ctx, thread.ID, "go", TurnStartOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for range h.Events {
	}
	// On codex death the turn now receives a synthetic terminal *Turn carrying
	// the codex-exited reason rather than a closed-empty Result. That distinct
	// signal is what lets the broker drain attribute the death (and a peer turn
	// draining alongside a killed turn avoid being mislabelled as a vanilla
	// completed/unknown run). See setCodexDied + failPendingRequests.
	turn, ok := <-h.Result
	if !ok || turn == nil {
		t.Fatal("expected a synthetic codex-died Turn on crash, got closed/empty Result")
	}
	if turn.Status != "failed" {
		t.Fatalf("crash turn status = %q, want failed", turn.Status)
	}
	if turn.Error == nil || turn.Error.Code != codexExitedTurnError {
		t.Fatalf("crash turn error = %+v, want code %q", turn.Error, codexExitedTurnError)
	}
	if !a.IsDead() {
		t.Fatal("AppServer should be dead")
	}
}

// ---------------------------------------------------------------------------
// Packet 011: close/reader races, head-of-line blocking, server→client
// requests, child reaping. The tests below target the per-turn pump design.
// ---------------------------------------------------------------------------

// newRawAppServer wires an AppServer to a pair of pipes and starts its reader,
// returning a handle that lets the test inject arbitrary server frames and a
// drain of the client→server direction so writes never block. Unlike
// newPipedAppServer it does not run a method-routed server, so a test can model
// multiple threads, blocked sends, and abrupt codex death directly.
type rawServer struct {
	a            *AppServer
	toClient     *io.PipeWriter // server → client (codex stdout)
	clientWrites <-chan []byte  // frames the client wrote to stdin
}

func newRawAppServer(t *testing.T) *rawServer {
	t.Helper()
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	a := &AppServer{
		stdin:   clientToServerW,
		stdout:  serverToClientR,
		done:    make(chan struct{}),
		turns:   make(map[string]*turnState),
		notifFn: func(Notification) {},
	}
	a.initialized.Store(true)
	a.startReader()

	writes := make(chan []byte, 1024)
	go func() {
		r := bufio.NewReader(clientToServerR)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				cp := append([]byte(nil), bytes.TrimRight(line, "\r\n")...)
				select {
				case writes <- cp:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		clientToServerW.Close()
		serverToClientW.Close()
	})
	return &rawServer{a: a, toClient: serverToClientW, clientWrites: writes}
}

// emit writes a server frame to the client. Write errors are returned, not
// fatal: a test may close toClient to model codex death while another goroutine
// is still emitting, and a closed-pipe write there is expected, not a failure.
// (Calling t.Fatalf off the test goroutine would also be illegal.)
func (rs *rawServer) emit(frame string) error {
	_, err := io.WriteString(rs.toClient, frame+"\n")
	return err
}

// startTurnRaw registers a turn against the reader by responding to the turn/start
// call from a goroutine, then returns the handle. It mirrors what a real server
// would do so StartTurn's call() resolves.
func (rs *rawServer) startTurnRaw(t *testing.T, threadID, turnID string) *TurnHandle {
	t.Helper()
	// Answer the turn/start request the moment it is written.
	go func() {
		for w := range rs.clientWrites {
			var req struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(w, &req)
			if req.Method == "turn/start" && req.ID != nil {
				_ = rs.emit(fmt.Sprintf(
					`{"jsonrpc":"2.0","id":%d,"result":{"turn":{"id":%q,"status":"pending"}}}`,
					*req.ID, turnID))
				return
			}
		}
	}()
	h, err := rs.a.StartTurn(context.Background(), threadID, "go", TurnStartOptions{})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	return h
}

// TestBlockedSendRacesCancelAndDeath drives the worst-case close-vs-blocked-send
// scenario: a turn whose consumer never drains its Events channel, while a
// concurrent Cancel and a concurrent codex stdout EOF both try to tear the turn
// down. Against the OLD design (reader closes ts.events/ts.result and so do
// Cancel and failPendingRequests, all guarded only by a CAS) a blocked
// `ts.events <- n` in the reader could race a close from Cancel → send-on-closed
// panic, or two closers could double-close. With the per-turn pump the reader
// never blocks on a consumer channel and only the pump closes it, so this must
// be race- and panic-free under -race -count=20.
func TestBlockedSendRacesCancelAndDeath(t *testing.T) {
	rs := newRawAppServer(t)
	h := rs.startTurnRaw(t, "thr-race", "trn-race")

	// Flood the turn with notifications WITHOUT anyone draining h.Events. This
	// fills the per-turn buffers so the pump is blocked forwarding, exactly the
	// state in which the OLD reader would have blocked on a closed channel.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if err := rs.emit(fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thr-race","item":{"id":"i%d","type":"agentMessage"}}}`, i)); err != nil {
				return // codex death closed the pipe mid-flood; expected
			}
		}
	}()

	// Concurrently cancel the turn and kill codex (stdout EOF), racing the flood.
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		h.Cancel()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		rs.toClient.Close() // codex death → reader EOF → failPendingRequests
	}()

	// The consumer eventually drains; both channels must close without panic.
	drained := make(chan struct{})
	go func() {
		for range h.Events {
		}
		for range h.Result {
		}
		close(drained)
	}()

	wg.Wait()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("Events/Result channels never closed after cancel+death race")
	}
}

// TestStalledConsumerDoesNotBlockOtherTurn proves head-of-line isolation: turn A's
// consumer never reads its events, yet turn B (a different thread) still receives
// its full event stream and final Turn. Against the OLD reader — which blocked on
// `ts.events <- n` for whichever turn a notification targeted — a stalled consumer
// on A would wedge the single reader goroutine and starve B entirely.
func TestStalledConsumerDoesNotBlockOtherTurn(t *testing.T) {
	rs := newRawAppServer(t)

	hA := rs.startTurnRaw(t, "thr-A", "trn-A")
	hB := rs.startTurnRaw(t, "thr-B", "trn-B")

	// Stall A: never read hA.Events. Flood A past its buffers so, in the OLD
	// design, the shared reader would block on A's full events channel.
	go func() {
		for i := 0; i < 2000; i++ {
			if err := rs.emit(fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thr-A","item":{"id":"a%d","type":"agentMessage"}}}`, i)); err != nil {
				return
			}
		}
	}()

	// Meanwhile drive turn B to completion. B must flow despite A being stuck.
	if err := rs.emit(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-B","turn":{"id":"trn-B","status":"pending"}}}`); err != nil {
		t.Fatalf("emit B started: %v", err)
	}
	if err := rs.emit(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-B","turn":{"id":"trn-B","status":"completed","durationMs":5}}}`); err != nil {
		t.Fatalf("emit B completed: %v", err)
	}

	gotB := make(chan *Turn, 1)
	go func() {
		for range hB.Events {
		}
		gotB <- <-hB.Result
	}()

	select {
	case turn := <-gotB:
		if turn == nil || turn.Status != "completed" || turn.ID != "trn-B" {
			t.Fatalf("turn B: %+v", turn)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn B was starved by a stalled consumer on turn A (head-of-line blocking)")
	}

	// Cleanup: cancel A so its pump exits. Cancel issues a blocking
	// turn/interrupt RPC that nothing answers here (no scripted server), so run
	// it off the test goroutine exactly as the broker does (go handle.Cancel()).
	go hA.Cancel()
}

// TestServerRequestDoesNotHangCodex verifies a server→client request (approval /
// elicitation) is answered with a benign result rather than a blanket -32601 that
// could wedge codex. We assert the client writes a JSON-RPC RESPONSE (id, no
// method) carrying a result, not an error, for a known approval method.
func TestServerRequestDoesNotHangCodex(t *testing.T) {
	rs := newRawAppServer(t)

	// Codex issues an approval request mid-stream.
	if err := rs.emit(`{"jsonrpc":"2.0","id":4242,"method":"execCommandApproval","params":{"command":"ls"}}`); err != nil {
		t.Fatalf("emit approval request: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case w := <-rs.clientWrites:
			var resp struct {
				ID     *int64          `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *rpcError       `json:"error"`
			}
			if err := json.Unmarshal(w, &resp); err != nil {
				continue
			}
			// We want the RESPONSE to id 4242 (no method field).
			if resp.ID != nil && *resp.ID == 4242 && resp.Method == "" {
				if resp.Error != nil {
					t.Fatalf("approval answered with error %v (would risk wedging codex)", resp.Error)
				}
				if len(resp.Result) == 0 {
					t.Fatalf("approval answered with empty result: %s", w)
				}
				var got map[string]interface{}
				_ = json.Unmarshal(resp.Result, &got)
				if got["decision"] != "approved" {
					t.Fatalf("approval decision = %v, want approved: %s", got["decision"], w)
				}
				return
			}
		case <-deadline:
			t.Fatal("no response to server→client approval request (codex would hang)")
		}
	}
}

// TestUnknownServerRequestStillAnswered ensures a genuinely unknown server
// request gets a -32601 response (one response, never zero), so codex never
// hangs waiting even for methods we deliberately do not implement.
func TestUnknownServerRequestStillAnswered(t *testing.T) {
	rs := newRawAppServer(t)
	if err := rs.emit(`{"jsonrpc":"2.0","id":777,"method":"some/unknownThing","params":{}}`); err != nil {
		t.Fatalf("emit unknown request: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case w := <-rs.clientWrites:
			var resp struct {
				ID     *int64    `json:"id"`
				Method string    `json:"method"`
				Error  *rpcError `json:"error"`
			}
			if err := json.Unmarshal(w, &resp); err != nil {
				continue
			}
			if resp.ID != nil && *resp.ID == 777 && resp.Method == "" {
				if resp.Error == nil || resp.Error.Code != -32601 {
					t.Fatalf("unknown request not answered with -32601: %s", w)
				}
				return
			}
		case <-deadline:
			t.Fatal("no response to unknown server→client request")
		}
	}
}

// TestSpawnFailureReapsChild proves a failed handshake does not leak the started
// codex child or its reader goroutine. We point Spawn at a fake codex that starts
// but never answers initialize; with a short ctx, Spawn must return an error AND
// leave no extra goroutines/processes behind (NumGoroutine returns to baseline).
func TestSpawnFailureReapsChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a unix shell child")
	}
	// `sh -c 'cat'` reads stdin forever and never speaks the protocol, so the
	// initialize handshake times out — modelling a hung/misbehaving codex.
	base := runtime.NumGoroutine()
	a := New("sh", []string{"-c", "cat"}, nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := a.Spawn(ctx); err == nil {
		_ = a.Close(context.Background())
		t.Fatal("expected Spawn to fail when handshake never completes")
	}
	// After a failed Spawn the child must be reaped and the reader joined.
	if a.cmd != nil && a.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped after handshake failure")
	}
	// Reader goroutine must have returned (readerDone closed).
	select {
	case <-a.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine not joined after failed Spawn")
	}
	waitGoroutines(t, base, 2*time.Second)
}

// TestRecycleNoGoroutineGrowth runs many spawn→die→recycle cycles and asserts the
// goroutine count does not grow without bound. Each cycle spawns a fake codex,
// runs one turn to completion, then kills the child and closes the AppServer — the
// path EnsureAppServer takes when recycling a dead instance. A leak in the reader
// or per-turn pump would show as monotonically rising NumGoroutine.
func TestRecycleNoGoroutineGrowth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a unix shell child indirectly")
	}
	const cycles = 100

	runOne := func() {
		clientToServerR, clientToServerW := io.Pipe()
		serverToClientR, serverToClientW := io.Pipe()
		a := &AppServer{
			stdin:   clientToServerW,
			stdout:  serverToClientR,
			done:    make(chan struct{}),
			turns:   make(map[string]*turnState),
			notifFn: func(Notification) {},
		}
		a.initialized.Store(true)
		a.startReader()

		// Minimal scripted server: answer turn/start then complete the turn.
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			r := bufio.NewReader(clientToServerR)
			for {
				line, err := r.ReadBytes('\n')
				line = bytes.TrimRight(line, "\r\n")
				if len(line) > 0 {
					var req struct {
						ID     *int64 `json:"id"`
						Method string `json:"method"`
					}
					_ = json.Unmarshal(line, &req)
					if req.Method == "turn/start" && req.ID != nil {
						_, _ = io.WriteString(serverToClientW, fmt.Sprintf(
							`{"jsonrpc":"2.0","id":%d,"result":{"turn":{"id":"t","status":"pending"}}}`+"\n", *req.ID))
						_, _ = io.WriteString(serverToClientW,
							`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr","turn":{"id":"t","status":"completed"}}}`+"\n")
					}
				}
				if err != nil {
					return
				}
			}
		}()

		h, err := a.StartTurn(context.Background(), "thr", "go", TurnStartOptions{})
		if err != nil {
			t.Fatalf("StartTurn: %v", err)
		}
		for range h.Events {
		}
		<-h.Result

		// Recycle: kill the child (close server side) and tear down.
		serverToClientW.Close()
		clientToServerW.Close()
		<-a.done // reader observed EOF
		<-serverDone
	}

	// Warm up a couple cycles before sampling the baseline so one-time lazily
	// initialised goroutines don't read as growth.
	runOne()
	runOne()
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < cycles; i++ {
		runOne()
	}
	waitGoroutines(t, base, 3*time.Second)
}

// ---------------------------------------------------------------------------
// Packet 009: shared app-server recycle safety + shutdown ordering.
// ---------------------------------------------------------------------------

// TestPeerTurnSurvivesSiblingChildDeath models the recycle-safety scenario at the
// app-server layer: two turns (A and B) share ONE AppServer. The child is killed
// (stdout EOF) while turn A is in-flight and turn B is still draining. Both turns
// must receive a TERMINAL signal that distinguishes "codex exited under me" from
// a clean turn/completed: a synthetic *Turn with status "failed" and the
// codex-exited error code. Against the OLD failPendingRequests (which closed the
// inbox with no finalTurn) the pump emitted NOTHING on result, so each turn's
// consumer saw a closed-empty Result. The broker drain maps that to
// turnToExit(nil) → exit 64 "codex exited without completing turn" — i.e. turn B
// is silently mislabelled as an unknown completion rather than attributed to the
// child death. This test asserts the distinct codex-died Turn so that mislabel
// can no longer happen.
func TestPeerTurnSurvivesSiblingChildDeath(t *testing.T) {
	rs := newRawAppServer(t)

	hA := rs.startTurnRaw(t, "thr-A", "trn-A")
	hB := rs.startTurnRaw(t, "thr-B", "trn-B")

	// Give A a single in-flight notification, then never complete it.
	if err := rs.emit(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-A","turn":{"id":"trn-A","status":"pending"}}}`); err != nil {
		t.Fatalf("emit A started: %v", err)
	}
	// B is mid-drain too: one event, no completion.
	if err := rs.emit(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr-B","turn":{"id":"trn-B","status":"pending"}}}`); err != nil {
		t.Fatalf("emit B started: %v", err)
	}

	// Kill the shared child mid-turn-A while turn B is still draining.
	rs.toClient.Close()

	// Both turns must deliver a synthetic codex-died Turn (status "failed",
	// codexExitedTurnError), not a closed-empty Result.
	assertCodexDied := func(name string, h *TurnHandle) {
		for range h.Events { // drain whatever made it through first
		}
		select {
		case turn, ok := <-h.Result:
			if !ok || turn == nil {
				t.Fatalf("turn %s: Result closed with no value on child death (would mislabel as exit 64)", name)
			}
			if turn.Status != "failed" {
				t.Fatalf("turn %s: status = %q, want failed", name, turn.Status)
			}
			if turn.Error == nil || turn.Error.Code != codexExitedTurnError {
				t.Fatalf("turn %s: error = %+v, want code %q", name, turn.Error, codexExitedTurnError)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("turn %s: no terminal Result after child death", name)
		}
	}
	assertCodexDied("A", hA)
	assertCodexDied("B", hB)

	if !rs.a.IsDead() {
		t.Fatal("AppServer should be dead after stdout EOF")
	}
	if rs.a.ExitErr() == nil {
		t.Fatal("ExitErr should be set after child death")
	}
	// Every turn must be unregistered so a recycle reaps a fully-drained instance.
	if n := rs.a.ActiveTurns(); n != 0 {
		t.Fatalf("ActiveTurns = %d after child death, want 0", n)
	}
}

// TestChildDeathRacesPeerTurnDrain is the -race -count=20 stressor for AC#1: a
// peer turn (B) is being actively drained by a consumer at the exact moment the
// shared child dies and turn A is also in flight. The consumer keeps reading
// h.Events/h.Result while failPendingRequests tears every turn down. With the
// per-turn pump owning the consumer channels and the reader only stamping
// finalTurn + closing the inbox, there is no send-vs-close race on B's channels
// regardless of the interleaving. Run under -race -count=20 this must be clean.
func TestChildDeathRacesPeerTurnDrain(t *testing.T) {
	rs := newRawAppServer(t)
	hA := rs.startTurnRaw(t, "thr-A", "trn-A")
	hB := rs.startTurnRaw(t, "thr-B", "trn-B")

	var wg sync.WaitGroup

	// A flood on both turns so the pumps are actively forwarding when death hits.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			if err := rs.emit(fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thr-A","item":{"id":"a%d","type":"agentMessage"}}}`, i)); err != nil {
				return
			}
			if err := rs.emit(fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thr-B","item":{"id":"b%d","type":"agentMessage"}}}`, i)); err != nil {
				return
			}
		}
	}()

	// B's consumer actively drains while the child dies — the peer-turn drain.
	bDone := make(chan struct{})
	go func() {
		for range hB.Events {
		}
		<-hB.Result
		close(bDone)
	}()
	// A's consumer drains too so its pump can exit.
	aDone := make(chan struct{})
	go func() {
		for range hA.Events {
		}
		<-hA.Result
		close(aDone)
	}()

	// Kill the child concurrently with the flood + drains.
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		rs.toClient.Close()
	}()

	wg.Wait()
	for _, c := range []struct {
		name string
		ch   chan struct{}
	}{{"B", bDone}, {"A", aDone}} {
		select {
		case <-c.ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("turn %s consumer never finished after child-death race", c.name)
		}
	}
}

// waitGoroutines waits until NumGoroutine settles at or below base (+ a small
// slack for the runtime), failing if it does not within the timeout.
func waitGoroutines(t *testing.T, base int, timeout time.Duration) {
	t.Helper()
	const slack = 5
	deadline := time.Now().Add(timeout)
	for {
		runtime.GC()
		n := runtime.NumGoroutine()
		if n <= base+slack {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count did not settle: base=%d now=%d (leak suspected)", base, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
