// Package appserver wraps `codex app-server` subprocess interactions. It is
// the only place in the binary that knows codex's app-server JSON-RPC shape.
//
// Lives in a sub-package so internal/broker (the dispatch handler that uses
// AppServer) and internal/codex (the high-level Fresh/Resume wrappers that
// route through internal/broker) can both depend on it without an import
// cycle.
package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MinCodexVersion is the smallest codex-cli release we accept. Pinned to the
// spec's footnote value; update both together if codex changes their
// versioning.
const MinCodexVersion = "0.130.0"

// Sentinel errors.
var (
	ErrCodexNotFound      = errors.New("codex binary not found")
	ErrCodexVersionTooOld = errors.New("codex version too old")
)

// CheckCodexVersion runs `codex --version`, returns ErrCodexNotFound if the
// binary isn't on PATH or ErrCodexVersionTooOld if the parsed version is
// below MinCodexVersion.
func CheckCodexVersion() error {
	path, err := exec.LookPath("codex")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCodexNotFound, err)
	}
	cmd := exec.Command(path, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %v", ErrCodexNotFound, err)
	}
	ver := parseVersion(out.String())
	if ver == "" {
		return fmt.Errorf("%w: could not parse %q", ErrCodexNotFound, out.String())
	}
	if compareVersions(ver, MinCodexVersion) < 0 {
		return fmt.Errorf("%w: %s (need >= %s)", ErrCodexVersionTooOld, ver, MinCodexVersion)
	}
	return nil
}

// parseVersion extracts the first dotted-decimal token from a `codex --version`
// line like "codex-cli 0.130.0\n".
func parseVersion(s string) string {
	for _, field := range strings.Fields(s) {
		if strings.Count(field, ".") >= 2 {
			return field
		}
	}
	return ""
}

// compareVersions returns -1, 0, or +1 for a < b, a == b, a > b.
// Handles plain dotted-decimal; pre-release / build suffixes are ignored.
func compareVersions(a, b string) int {
	parse := func(s string) []int {
		if i := strings.IndexAny(s, "-+"); i >= 0 {
			s = s[:i]
		}
		parts := strings.Split(s, ".")
		nums := make([]int, len(parts))
		for i, p := range parts {
			n, _ := strconv.Atoi(p)
			nums[i] = n
		}
		return nums
	}
	x := parse(a)
	y := parse(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var xi, yi int
		if i < len(x) {
			xi = x[i]
		}
		if i < len(y) {
			yi = y[i]
		}
		if xi != yi {
			if xi < yi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// --------------------------------------------------------------------------
// New public surface (real protocol).
// --------------------------------------------------------------------------

// Notification is one server-to-client notification reduced to {method, params}.
// AppServer's reader goroutine puts these on per-turn channels.
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Thread is what `thread/start` and `thread/resume` return. Trimmed to the
// fields we actively read. Status is a json.RawMessage because real codex
// returns it as either a string OR a discriminated object (e.g. {"type":"idle"})
// depending on version; we don't currently inspect it, so preserve raw bytes.
type Thread struct {
	ID     string          `json:"id"`
	CWD    string          `json:"cwd"`
	Status json.RawMessage `json:"status,omitempty"`
	Source string          `json:"source,omitempty"`
}

// Turn is the final state reported by `turn/completed`. Trimmed.
type Turn struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"` // "completed" | "failed" | "cancelled"
	Error      *TurnError `json:"error,omitempty"`
	DurationMs int64      `json:"durationMs,omitempty"`
	Items      []TurnItem `json:"items,omitempty"`
}

// TurnError is populated when Turn.Status == "failed".
type TurnError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// TurnItem is an item in Turn.Items. Codex's item taxonomy is large; we
// capture the union and treat unknown types defensively.
type TurnItem struct {
	ID   string          `json:"id,omitempty"`
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"` // populated by custom UnmarshalJSON below
}

// UnmarshalJSON preserves the full item JSON in Raw so callers that need a
// specific subtype can re-decode without losing fidelity.
func (t *TurnItem) UnmarshalJSON(b []byte) error {
	t.Raw = append([]byte(nil), b...)
	var aux struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	t.ID = aux.ID
	t.Type = aux.Type
	return nil
}

// ThreadStartOptions controls thread/start.
type ThreadStartOptions struct {
	CWD                   string // absolute path; passed through unchanged
	Sandbox               string // "read-only" | "workspace-write" | "danger-full-access"
	DeveloperInstructions string // optional; usually our assembled prompt's system block
	Model                 string // optional; pins the codex model for the thread (from CODEX_MODEL). Empty = codex's configured default.
}

// ThreadResumeOptions controls thread/resume. Currently empty; reserved for future overrides.
type ThreadResumeOptions struct{}

// TurnStartOptions controls turn/start. Currently empty; the prompt is passed
// separately and approvalPolicy is pinned to "never" inside StartTurn.
type TurnStartOptions struct{}

// TurnHandle is returned by StartTurn. The caller drains Events until it's
// closed, then receives the final Turn on Result.
type TurnHandle struct {
	TurnID string
	Events <-chan Notification
	Result <-chan *Turn
	cancel func() // closes channels and best-effort sends turn/interrupt
}

// Cancel closes the handle's channels and attempts to interrupt the turn at
// codex. Idempotent.
func (h *TurnHandle) Cancel() { h.cancel() }

// AppServer is a long-lived JSON-RPC client for `codex app-server`. Zero
// value is unusable; construct with New().
type AppServer struct {
	// populated by New
	cmdPath  string
	cmdArgs  []string
	env      []string
	cwd      string
	childCtl childController // platform teardown seam (process group on POSIX, Job Object on Windows)

	// populated by Spawn (lazily, behind spawnMu + initOnce)
	spawnMu     sync.Mutex
	initOnce    sync.Once
	closeOnce   sync.Once
	initErr     error
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	initialized atomic.Bool

	// populated by reader goroutine
	nextID        atomic.Int64
	writeMu       sync.Mutex
	pending       sync.Map           // map[int64]chan *rpcResponse
	notifFn       func(Notification) // catch-all for notifications without a thread routing
	done          chan struct{}      // closed by the reader goroutine when stdout EOFs
	readerDone    chan struct{}      // closed when the reader goroutine has fully returned
	readerStarted atomic.Bool
	doneErr       atomic.Pointer[error]

	// populated by StartTurn
	turnsMu sync.Mutex
	turns   map[string]*turnState // keyed by Thread.id; one in-flight turn per thread
}

// Sentinel errors for the new surface.
var (
	ErrStaleSession   = errors.New("thread no longer exists")
	ErrCodexExited    = errors.New("codex app-server exited unexpectedly")
	ErrNotInitialized = errors.New("codex app-server not initialized")
)

// codexExitedTurnError is the TurnError code stamped on the synthetic *Turn that
// failPendingRequests hands to every in-flight turn when the shared codex child
// dies. It lets a peer turn's consumer (the broker drain) distinguish "codex
// exited under me" from a turn that genuinely reached turn/completed, so a peer
// turn draining alongside the killed turn is not silently mislabelled as a
// completed-with-unknown-status (exit 64) run.
const codexExitedTurnError = "codex_exited"

const eventsLostMethod = "codex/eventsLost"

// --------------------------------------------------------------------------
// Internal RPC plumbing types (unexported).
// --------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"` // nil when this is a server-side request
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	// present when the inbound message is a server→client request
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc %d: %s", e.Code, e.Message) }

// turnState carries one in-flight turn. A dedicated per-turn pump goroutine
// (startPump) is the SOLE owner and closer of events/result; the shared reader
// goroutine only ever sends (non-blocking) onto the unexported inbox and
// signals cancellation by closing canceled. This split makes the
// close-vs-blocked-send race impossible and decouples a stalled consumer on one
// turn from every other turn's delivery: the reader never blocks on any single
// turn, so head-of-line blocking across turns cannot happen.
type turnState struct {
	threadID string
	events   chan Notification // pump -> consumer; closed only by the pump
	result   chan *Turn        // pump -> consumer; closed only by the pump
	inbox    chan Notification // reader -> pump; closed only by closeInbox
	canceled chan struct{}     // closed (once) to tell the pump to stop early

	// finalTurn holds the decoded *Turn from turn/completed. Set by the reader
	// before closeInbox so the pump can emit it on natural end-of-turn even if
	// the inbox buffer overflowed. Guarded by the inbox-close happens-before
	// edge (reader stores it, then closeInbox; pump reads it only after the
	// inbox channel reports closed).
	finalTurn atomic.Pointer[Turn]

	// done marks the turn as terminal so the reader stops routing to it and a
	// second cancel/complete is a no-op. Set before the inbox is closed.
	done atomic.Bool
	// eventsLost is set if the reader had to drop a non-terminal frame because
	// the consumer stalled and the inbox buffer was full (overflow policy).
	eventsLost atomic.Bool
	// eventsLostSignaled ensures the consumer sees at most one loss marker per
	// turn, even if many frames are dropped.
	eventsLostSignaled atomic.Bool
	// inboxOnce guards closeInbox so the reader and a canceller never double
	// close the reader->pump channel.
	inboxOnce sync.Once
	// cancelOnce guards closing canceled.
	cancelOnce sync.Once
}

// signalCancel marks the turn terminal and wakes the pump so it closes the
// consumer-facing channels promptly without a turn/completed ever arriving.
// Idempotent and safe to call from any goroutine.
func (ts *turnState) signalCancel() {
	ts.done.Store(true)
	ts.cancelOnce.Do(func() { close(ts.canceled) })
}

// closeInbox closes the reader->pump channel exactly once. Only the reader
// goroutine (after routing turn/completed or on codex death) calls this; the
// pump treats a closed inbox as natural end-of-turn.
func (ts *turnState) closeInbox() {
	ts.inboxOnce.Do(func() { close(ts.inbox) })
}

// setCodexDied stamps a synthetic "failed" *Turn carrying the codex-exited
// reason into finalTurn, but only if a real turn/completed has not already set
// it (CompareAndSwap on the nil pointer). The reader calls this immediately
// before closeInbox on codex death, so the store happens-before the pump's
// closed-inbox observation and the pump emits it on the natural end-of-turn
// path. A consumer that drains this turn then sees a terminal Turn with status
// "failed" rather than a closed-empty Result it would mistake for an exit-64
// completion.
func (ts *turnState) setCodexDied(err error) {
	msg := ErrCodexExited.Error()
	if err != nil {
		msg = err.Error()
	}
	ts.finalTurn.CompareAndSwap(nil, &Turn{
		Status: "failed",
		Error:  &TurnError{Code: codexExitedTurnError, Message: msg},
	})
}

func (ts *turnState) markEventsLost() {
	ts.eventsLost.Store(true)
}

func (ts *turnState) forwardEventsLostIfNeeded() {
	if !ts.eventsLost.Load() || !ts.eventsLostSignaled.CompareAndSwap(false, true) {
		return
	}
	ts.forward(Notification{
		Method: eventsLostMethod,
		Params: json.RawMessage(`{"reason":"consumer stalled; notification frames were dropped"}`),
	})
}

// startPump is the per-turn delivery goroutine. It is the only goroutine that
// writes to and closes ts.events/ts.result, eliminating any send-vs-close race
// on those consumer channels. It forwards notifications from inbox to events,
// emits the final Turn on result at natural end-of-turn, and stops on either
// inbox close (turn/completed routed, or codex death) or canceled (operator
// cancel). A consumer that stalls only ever back-pressures this single pump,
// never the shared reader, so other turns keep flowing (no head-of-line block).
func (ts *turnState) startPump() {
	go func() {
		defer close(ts.result)
		defer close(ts.events)
		for {
			select {
			case n, ok := <-ts.inbox:
				if !ok {
					ts.forwardEventsLostIfNeeded()
					// Inbox closed by the reader: natural end-of-turn. If a
					// turn/completed was seen, finalTurn is set (the store
					// happens-before this closed-channel observation), so emit
					// it. result is buffered (size 1) so this never blocks.
					if turn := ts.finalTurn.Load(); turn != nil {
						select {
						case ts.result <- turn:
						default:
						}
					}
					return
				}
				ts.forwardEventsLostIfNeeded()
				ts.forward(n)
			case <-ts.canceled:
				// Operator cancel: drop pending events, close channels with no
				// *Turn so the drain classifies this as a cancellation.
				return
			}
		}
	}()
}

// forward delivers one notification to the consumer, abandoning the send if the
// turn is canceled so a stalled consumer cannot wedge the pump during teardown.
func (ts *turnState) forward(n Notification) {
	select {
	case ts.events <- n:
	case <-ts.canceled:
	}
}

// --------------------------------------------------------------------------
// Constructor.
// --------------------------------------------------------------------------

// New constructs an AppServer. cmdPath is typically "codex"; args is
// ["app-server"] for real codex but adjustable for tests. env is passed to
// exec; cwd is the child's working directory.
func New(cmdPath string, args []string, env []string, cwd string) *AppServer {
	return &AppServer{
		cmdPath:    cmdPath,
		cmdArgs:    args,
		env:        env,
		cwd:        cwd,
		childCtl:   newChildController(),
		done:       make(chan struct{}),
		readerDone: make(chan struct{}),
		turns:      make(map[string]*turnState),
		notifFn:    func(Notification) {}, // no-op default; R2's reader can rely on it being non-nil
	}
}

// --------------------------------------------------------------------------
// Reader goroutine (R2).
// --------------------------------------------------------------------------

// startReader spawns the goroutine that owns stdout. It demultiplexes:
//   - responses to our requests (by id → pending)
//   - notifications (routed to per-turn channels via turns map; fallback to notifFn)
//   - server→client requests (always responded with -32601)
//
// On stdout EOF, it closes done and stores doneErr.
//
// Lines are read with a bufio.Reader and ReadBytes('\n') rather than a
// bufio.Scanner so there is no per-line token ceiling: codex can legitimately
// emit a single notification (e.g. a large agentMessage) well over 16 MiB, and
// such a line must NOT be misread as codex dying. Only a genuine read error
// (typically io.EOF when the child closes stdout) tears the AppServer down.
func (a *AppServer) startReader() {
	if a.readerDone == nil {
		a.readerDone = make(chan struct{})
	}
	a.readerStarted.Store(true)
	go func() {
		defer close(a.readerDone)
		defer close(a.done)
		reader := bufio.NewReaderSize(a.stdout, 64*1024)
		for {
			line, err := reader.ReadBytes('\n')
			// ReadBytes returns the data read so far even on error, so process
			// a trailing unterminated frame before reacting to the error.
			if len(line) > 0 {
				trimmed := bytes.TrimRight(line, "\r\n")
				if len(trimmed) > 0 {
					a.handleInbound(trimmed)
				}
			}
			if err != nil {
				wrapped := fmt.Errorf("%w: %v", ErrCodexExited, err)
				a.doneErr.Store(&wrapped)
				a.failPendingRequests(wrapped)
				return
			}
		}
	}()
}

// handleInbound is called for each line from codex's stdout.
func (a *AppServer) handleInbound(line []byte) {
	var probe struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return // malformed; drop
	}
	switch {
	case probe.ID != nil && probe.Method != "":
		// Server→client request (approval / elicitation / etc).
		a.handleServerRequest(*probe.ID, probe.Method, probe.Params)
	case probe.ID != nil:
		// Response to one of our requests.
		if ch, ok := a.pending.LoadAndDelete(*probe.ID); ok {
			chresp, _ := ch.(chan *rpcResponse)
			chresp <- &rpcResponse{ID: probe.ID, Result: probe.Result, Error: probe.Error}
		}
	case probe.Method != "":
		// Server notification.
		a.routeNotification(Notification{Method: probe.Method, Params: probe.Params})
	}
}

// handleServerRequest answers a server→client request from codex.
//
// We always start threads/turns with approvalPolicy="never", so codex SHOULD
// never block waiting on us for a command/patch approval. But the app-server
// protocol still permits a handful of server-initiated requests (approval and
// elicitation prompts), and a blanket -32601 "Method not found" is the wrong
// answer for those: a codex build that does ask would treat the JSON-RPC error
// as a failed approval (or, worse, wait/retry), which can wedge the turn. Since
// our operating contract is non-interactive auto-approval, we reply to the
// known approval/elicitation requests with a benign "approved"/"accepted"
// result so codex proceeds without a human. Genuinely unknown request methods
// still get -32601 (codex must tolerate that for methods it didn't truly
// expect us to implement). Either way we ALWAYS send exactly one response so
// codex never hangs awaiting one.
func (a *AppServer) handleServerRequest(id int64, method string, _ json.RawMessage) {
	switch method {
	case
		// Command / patch approval prompts. With approvalPolicy=never these
		// should not fire, but auto-approve defensively if they do.
		"applyPatchApproval",
		"execCommandApproval",
		"item/approvalRequest",
		"turn/approvalRequest",
		"approvalRequest":
		// codex's ReviewDecision enum: "approved" lets the action proceed.
		a.sendResult(id, map[string]any{"decision": "approved"})
	case
		// Elicitation: codex asking the client for free-form input. We are
		// non-interactive, so decline politely with an empty/declined result
		// rather than erroring (which some builds treat as fatal).
		"elicitation/create",
		"input/request",
		"elicitationRequest":
		a.sendResult(id, map[string]any{"action": "decline"})
	default:
		// Truly unknown server request: report not-implemented. This is safe
		// for methods codex does not actually require us to handle.
		a.sendError(id, -32601, "Method not found")
	}
}

// sendResult writes a JSON-RPC success response for a server→client request.
func (a *AppServer) sendResult(id int64, result interface{}) {
	resp := map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": result,
	}
	b, _ := json.Marshal(resp)
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.stdin != nil {
		_, _ = a.stdin.Write(b)
		_, _ = a.stdin.Write([]byte("\n"))
	}
}

// decodeTurn extracts the *Turn from a turn/completed notification's params.
// Returns nil if the params have no turn object.
func decodeTurn(params json.RawMessage) *Turn {
	var hdr struct {
		Turn *struct {
			ID         string     `json:"id"`
			Status     string     `json:"status"`
			Error      *TurnError `json:"error"`
			DurationMs int64      `json:"durationMs"`
			Items      []TurnItem `json:"items"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &hdr); err != nil || hdr.Turn == nil {
		return nil
	}
	return &Turn{
		ID:         hdr.Turn.ID,
		Status:     hdr.Turn.Status,
		Error:      hdr.Turn.Error,
		DurationMs: hdr.Turn.DurationMs,
		Items:      hdr.Turn.Items,
	}
}

// routeNotification hands a notification to the matching turn's pump via the
// reader->pump inbox. The reader NEVER blocks on a per-turn consumer channel:
// it only ever sends onto inbox (which the dedicated pump drains) and, for
// turn/completed, closes the inbox so the pump finishes. Decoding the *Turn and
// closing the consumer-facing events/result channels is the pump's job, so a
// blocked consumer on this (or any) turn cannot stall the shared reader and a
// concurrent cancel can never race a close on events/result.
func (a *AppServer) routeNotification(n Notification) {
	if a.notifFn != nil {
		a.notifFn(n)
	}
	var hdr struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(n.Params, &hdr)

	// Look up the turn handle by threadID (per-thread routing).
	a.turnsMu.Lock()
	ts := a.turns[hdr.ThreadID]
	a.turnsMu.Unlock()

	if ts == nil || ts.done.Load() {
		return
	}

	if n.Method == "turn/completed" {
		// Terminal frame. Stash the decoded *Turn, mark the turn terminal and
		// unregister it (so a late duplicate is ignored), then close the inbox.
		// The pump emits finalTurn on the resulting inbox-close — this delivery
		// is independent of inbox buffer capacity, so it survives even if a
		// stalled consumer caused earlier non-terminal frames to overflow.
		if turn := decodeTurn(n.Params); turn != nil {
			ts.finalTurn.Store(turn)
		}
		// Best-effort: also try to hand the terminal notification itself to the
		// consumer, but never block the shared reader on it.
		select {
		case ts.inbox <- n:
		default:
		}
		ts.done.Store(true)
		a.turnsMu.Lock()
		delete(a.turns, ts.threadID)
		a.turnsMu.Unlock()
		ts.closeInbox()
		return
	}

	// Non-terminal frame: non-blocking handoff to the pump. The reader must
	// NEVER block on a per-turn channel — a slow consumer on this turn would
	// otherwise stall delivery for every other turn (head-of-line blocking). If
	// the inbox buffer is full, record the loss and drop rather than block.
	select {
	case ts.inbox <- n:
	default:
		ts.markEventsLost()
	}
}

// failPendingRequests is called by the reader goroutine when codex's stdout
// EOFs (or another fatal read error occurs). It unblocks every pending call()
// and tears down all in-flight turns. Crucially it does NOT close ts.events /
// ts.result directly — those are owned solely by each turn's pump. It marks the
// turn terminal and closes the reader->pump inbox (the reader owns inbox close),
// so each pump observes end-of-turn and closes its consumer channels.
//
// Before closing the inbox it stamps a synthetic codex-exited *Turn into
// finalTurn (only when no real turn/completed already set it). The pump then
// emits that *Turn on the natural inbox-close path, so a turn that was draining
// when the shared child died receives a non-nil Turn whose status is "failed"
// with the codexExitedTurnError code — distinct from a turn that genuinely
// completed and, critically, distinct from a closed-empty Result that a consumer
// would otherwise mislabel as a completed-but-unknown (exit 64) run. This is
// what stops a peer turn from being silently mislabelled when a sibling turn's
// child is killed. It still does NOT touch ts.events/ts.result, so the
// reader-vs-Cancel close race remains impossible.
func (a *AppServer) failPendingRequests(err error) {
	a.pending.Range(func(k, v any) bool {
		ch, _ := v.(chan *rpcResponse)
		a.pending.Delete(k)
		select {
		case ch <- &rpcResponse{Error: &rpcError{Code: -32000, Message: err.Error()}}:
		default:
		}
		return true
	})
	a.turnsMu.Lock()
	for tid, ts := range a.turns {
		ts.done.Store(true)
		ts.setCodexDied(err)
		ts.closeInbox()
		delete(a.turns, tid)
	}
	a.turnsMu.Unlock()
}

func (a *AppServer) sendError(id int64, code int, msg string) {
	resp := map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	}
	b, _ := json.Marshal(resp)
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.stdin != nil {
		_, _ = a.stdin.Write(b)
		_, _ = a.stdin.Write([]byte("\n"))
	}
}

// --------------------------------------------------------------------------
// Request / notification senders (R2).
// --------------------------------------------------------------------------

// call sends a JSON-RPC request and blocks until a response arrives, ctx
// expires, or codex dies.
func (a *AppServer) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id := a.nextID.Add(1)
	ch := make(chan *rpcResponse, 1)
	a.pending.Store(id, ch)

	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			a.pending.Delete(id)
			return nil, err
		}
		raw = b
	}
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: raw}
	if err := a.writeFrame(req); err != nil {
		a.pending.Delete(id)
		return nil, err
	}

	select {
	case <-ctx.Done():
		a.pending.Delete(id)
		return nil, ctx.Err()
	case <-a.done:
		if pErr := a.doneErr.Load(); pErr != nil {
			return nil, *pErr
		}
		return nil, ErrCodexExited
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// notify sends a JSON-RPC notification (no response expected).
func (a *AppServer) notify(method string, params interface{}) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	note := rpcNotification{JSONRPC: "2.0", Method: method, Params: raw}
	return a.writeFrame(note)
}

// writeFrame marshals v and writes it as a single newline-terminated line.
func (a *AppServer) writeFrame(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if _, err := a.stdin.Write(b); err != nil {
		return err
	}
	_, err = a.stdin.Write([]byte("\n"))
	return err
}

// --------------------------------------------------------------------------
// Initialize handshake (R2).
// --------------------------------------------------------------------------

// initialize sends the JSON-RPC initialize request, waits for the response,
// then sends the initialized notification. Idempotent on the success path
// (subsequent calls return nil without re-handshaking).
func (a *AppServer) initialize(ctx context.Context) error {
	a.initOnce.Do(func() {
		a.startReader()
		params := map[string]any{
			"clientInfo": map[string]string{
				"name":    "codex-dispatch",
				"version": pluginVersion(),
			},
		}
		if _, err := a.call(ctx, "initialize", params); err != nil {
			a.initErr = fmt.Errorf("initialize: %w", err)
			return
		}
		if err := a.notify("initialized", map[string]any{}); err != nil {
			a.initErr = fmt.Errorf("initialized notification: %w", err)
			return
		}
		a.initialized.Store(true)
	})
	return a.initErr
}

// pluginVersion returns the plugin version string; thin wrapper so the
// value can be overridden in tests without messing with build flags.
var pluginVersion = func() string { return "0.5.0" }

// --------------------------------------------------------------------------
// Spawn (R2).
// --------------------------------------------------------------------------

// Spawn forks `codex app-server` and performs the initialize handshake.
// Idempotent: subsequent calls return the same outcome. On crash, the
// AppServer is dead and the broker should construct a new one.
func (a *AppServer) Spawn(ctx context.Context) error {
	a.spawnMu.Lock()
	defer a.spawnMu.Unlock()
	if a.initialized.Load() {
		return nil
	}
	if a.cmd != nil {
		// Spawn was called before and failed; report the saved error.
		return a.initErr
	}

	cmd := exec.Command(a.cmdPath, a.cmdArgs...)
	cmd.Env = a.env
	cmd.Dir = a.cwd
	cmd.Stderr = io.Discard // codex stderr is noisy; broker writes its own diagnostics
	// Put the child in its own process group and (on Linux) request a SIGKILL
	// when the broker dies, so a crashed/SIGKILL'd broker can never leave an
	// orphaned codex app-server child (with danger-full-access) running.
	setChildProcAttr(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return fmt.Errorf("start codex: %w", err)
	}
	a.cmd = cmd
	a.stdin = stdin
	a.stdout = stdout

	// Enroll the child in its platform kill-domain (Job Object on Windows;
	// no-op on POSIX where the process group is set via setChildProcAttr) before
	// any turn can run a shell command, so a later reap tears down the whole
	// subtree and a broker crash cannot orphan a danger-full-access child.
	if err := a.ctl().arm(cmd); err != nil {
		a.closeOnce.Do(func() { a.reapChild() })
		return fmt.Errorf("arm child controller: %w", err)
	}

	if err := a.initialize(ctx); err != nil {
		// Handshake failed after the child was already started. Reap it (and
		// stop the reader goroutine started by initialize) so a failed Spawn
		// never leaks a running codex child or a hung reader. Route through
		// closeOnce so the child is Wait()ed exactly once even if Close is
		// later called on this dead instance.
		a.closeOnce.Do(func() { a.reapChild() })
		return err
	}
	return nil
}

// setChildProcAttr configures the codex child so it cannot outlive the broker.
// It is defined per-platform in procattr_*.go: Linux sets Pdeathsig=SIGKILL
// (kernel kills the child when the broker dies) plus Setpgid; darwin/BSD set
// Setpgid only (Pdeathsig is Linux-only); other OSes get a no-op. reapChild
// covers explicit teardown on all platforms.

// ctl returns the platform teardown controller, lazily constructing one for
// AppServer values built as a struct literal (some tests) rather than via New().
func (a *AppServer) ctl() childController {
	if a.childCtl == nil {
		a.childCtl = newChildController()
	}
	return a.childCtl
}

// reapChild closes stdin/stdout, terminates the codex child subtree, waits for
// it, and joins the reader goroutine. Used on handshake failure and from Close.
// Safe to call when no child was started.
func (a *AppServer) reapChild() {
	if a.stdin != nil {
		_ = a.stdin.Close()
	}
	if a.cmd == nil || a.cmd.Process == nil {
		// No live child; just make sure the reader (if any) is unblocked by the
		// stdout close and joined.
		if a.stdout != nil {
			_ = a.stdout.Close()
		}
		a.joinReader()
		a.ctl().close()
		return
	}
	// Best-effort graceful then forceful termination of the whole child kill-
	// domain (process group on POSIX, Job Object on Windows). Because stdout is
	// owned by startReader via exec.Cmd.StdoutPipe, wait for that reader to drain
	// before calling Wait; Wait closes the pipe and can otherwise race the reader.
	if !a.waitReader(2 * time.Second) {
		_ = a.ctl().signal(a.cmd.Process.Pid, childSIGTERM)
		if !a.waitReader(2 * time.Second) {
			_ = a.ctl().signal(a.cmd.Process.Pid, childSIGKILL)
			a.joinReader()
		}
	}
	if a.stdout != nil {
		_ = a.stdout.Close()
	}
	a.joinReader()
	a.waitChild()
	a.ctl().close()
}

// joinReader blocks until the reader goroutine has fully returned, if one was
// started. No-op when the reader was never started.
func (a *AppServer) joinReader() {
	if !a.readerStarted.Load() || a.readerDone == nil {
		return
	}
	_ = a.waitReader(2 * time.Second)
}

func (a *AppServer) waitReader(timeout time.Duration) bool {
	if !a.readerStarted.Load() || a.readerDone == nil {
		return true
	}
	select {
	case <-a.readerDone:
		return true
	case <-time.After(timeout):
		// The reader is wedged on a stdout read that never returns; do not block
		// shutdown forever. This should not happen once stdout is closed.
		return false
	}
}

func (a *AppServer) waitChild() {
	done := make(chan struct{})
	go func() {
		_ = a.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = a.ctl().signal(a.cmd.Process.Pid, childSIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = a.ctl().signal(a.cmd.Process.Pid, childSIGKILL)
			<-done
		}
	}
}

// IsDead reports whether the reader goroutine has exited. Cheap; callers
// use this to know when to throw away the AppServer and construct a new one.
func (a *AppServer) IsDead() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

// ActiveTurns returns the number of in-flight turns the AppServer is still
// routing notifications to. A dead instance with ActiveTurns() > 0 has peer
// turns still draining their per-turn pumps; the broker uses this to avoid
// reaping (Close-ing) the shared child out from under a peer turn's reader.
// Note: on codex death the reader's failPendingRequests deletes every turn from
// the map, so a fully-dead-and-drained instance reports 0.
func (a *AppServer) ActiveTurns() int {
	a.turnsMu.Lock()
	defer a.turnsMu.Unlock()
	return len(a.turns)
}

// ExitErr returns the error that tore the AppServer down (codex stdout EOF or a
// fatal read error), or nil if it is still live. Callers use this to attribute a
// turn that ended because the shared child died, rather than guessing.
func (a *AppServer) ExitErr() error {
	if pErr := a.doneErr.Load(); pErr != nil {
		return *pErr
	}
	return nil
}

// --------------------------------------------------------------------------
// Thread methods (R3).
// --------------------------------------------------------------------------

// sandboxModeString validates an incoming sandbox mode and returns the codex
// SandboxMode enum string. Empty input is mapped to "danger-full-access" (the
// dispatch default).
//
// Note: ThreadStartParams uses `sandbox` (SandboxMode enum string).
// TurnStartParams uses `sandboxPolicy` (SandboxPolicy discriminated union).
// These are NOT the same field and NOT the same shape — see the v2 schema.
func sandboxModeString(mode string) (string, error) {
	switch mode {
	case "read-only", "workspace-write", "danger-full-access":
		return mode, nil
	case "":
		return "danger-full-access", nil
	default:
		return "", fmt.Errorf("unknown sandbox mode %q", mode)
	}
}

// StartThread sends thread/start with approvalPolicy=never.
func (a *AppServer) StartThread(ctx context.Context, opts ThreadStartOptions) (*Thread, error) {
	if !a.initialized.Load() {
		return nil, ErrNotInitialized
	}
	sandbox, err := sandboxModeString(opts.Sandbox)
	if err != nil {
		return nil, err
	}
	params := map[string]any{
		"approvalPolicy": "never",
		"sandbox":        sandbox,
	}
	if opts.CWD != "" {
		params["cwd"] = opts.CWD
	}
	if opts.DeveloperInstructions != "" {
		params["developerInstructions"] = opts.DeveloperInstructions
	}
	if opts.Model != "" {
		params["model"] = opts.Model
	}

	result, err := a.call(ctx, "thread/start", params)
	if err != nil {
		return nil, fmt.Errorf("thread/start: %w", err)
	}
	var resp struct {
		Thread Thread `json:"thread"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("thread/start response: %w", err)
	}
	return &resp.Thread, nil
}

// ResumeThread sends thread/resume. If codex reports the thread doesn't exist,
// returns ErrStaleSession (wrapped) so the caller can fall back to fresh.
func (a *AppServer) ResumeThread(ctx context.Context, threadID string, _ ThreadResumeOptions) (*Thread, error) {
	if !a.initialized.Load() {
		return nil, ErrNotInitialized
	}
	params := map[string]any{"threadId": threadID}
	result, err := a.call(ctx, "thread/resume", params)
	if err != nil {
		if isStaleError(err) {
			return nil, fmt.Errorf("%w: %v", ErrStaleSession, err)
		}
		return nil, fmt.Errorf("thread/resume: %w", err)
	}
	var resp struct {
		Thread Thread `json:"thread"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("thread/resume response: %w", err)
	}
	return &resp.Thread, nil
}

// isStaleError inspects an error from call() and reports whether it
// represents "thread doesn't exist" semantics. Codex's exact error code for
// this case is one of the open questions in the spec; we accept either a
// well-known code (when known) or a message substring as a defensive match.
func isStaleError(err error) bool {
	var rerr *rpcError
	if !errors.As(err, &rerr) {
		return false
	}
	// Codes that codex *might* use for "no such thread". We'll lock this down
	// once we observe real codex in the integration test (R10).
	switch rerr.Code {
	case -32004, -32602: // -32602 = "Invalid params", common when an id ref is unknown
		return true
	}
	msg := strings.ToLower(rerr.Message)
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such thread")
}

// StartTurn sends turn/start. Returns a TurnHandle whose Events stream is
// fed by the multiplexer and whose Result receives the final Turn when
// codex emits turn/completed.
//
// approvalPolicy is pinned to "never" for the turn (and inherited from
// thread/start; this is belt-and-suspenders).
func (a *AppServer) StartTurn(ctx context.Context, threadID, prompt string, _ TurnStartOptions) (*TurnHandle, error) {
	if !a.initialized.Load() {
		return nil, ErrNotInitialized
	}

	ts := &turnState{
		threadID: threadID,
		events:   make(chan Notification, 64),
		result:   make(chan *Turn, 1),
		inbox:    make(chan Notification, 256),
		canceled: make(chan struct{}),
	}
	a.turnsMu.Lock()
	if existing, ok := a.turns[threadID]; ok && !existing.done.Load() {
		a.turnsMu.Unlock()
		return nil, fmt.Errorf("turn already in flight for thread %s", threadID)
	}
	a.turns[threadID] = ts
	a.turnsMu.Unlock()
	ts.startPump()

	params := map[string]any{
		"threadId":       threadID,
		"approvalPolicy": "never",
		"input": []map[string]any{
			{"type": "text", "text": prompt},
		},
	}
	result, err := a.call(ctx, "turn/start", params)
	if err != nil {
		a.turnsMu.Lock()
		delete(a.turns, threadID)
		a.turnsMu.Unlock()
		ts.signalCancel() // stop the pump (it closes events/result with no Turn)
		return nil, fmt.Errorf("turn/start: %w", err)
	}
	var resp struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		a.turnsMu.Lock()
		delete(a.turns, threadID)
		a.turnsMu.Unlock()
		ts.signalCancel()
		return nil, fmt.Errorf("turn/start response: %w", err)
	}

	handle := &TurnHandle{
		TurnID: resp.Turn.ID,
		Events: ts.events,
		Result: ts.result,
		// Cancel is idempotent and never closes events/result itself: it only
		// marks the turn terminal, unregisters it from routing, and signals the
		// pump (which is the sole closer of the consumer channels). A blocked
		// reader send can therefore never race a Cancel-driven close.
		cancel: func() {
			if ts.done.CompareAndSwap(false, true) {
				a.turnsMu.Lock()
				delete(a.turns, ts.threadID)
				a.turnsMu.Unlock()
				ts.signalCancel()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, _ = a.call(ctx, "turn/interrupt", map[string]any{
					"turnId": resp.Turn.ID,
				})
			}
		},
	}
	return handle, nil
}

// --------------------------------------------------------------------------
// Standalone command exec / sandbox preflight.
// --------------------------------------------------------------------------

// CommandExecResult is the buffered outcome of a command/exec RPC.
type CommandExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// sandboxPolicyForMode maps a SandboxMode enum string (as used by thread/start)
// to the codex SandboxPolicy discriminated union that command/exec (and
// turn/start) expect. Reports ok=false for an unknown mode. The empty string is
// intentionally NOT mapped here — a caller that wants the dispatch default
// should resolve it via sandboxModeString first.
//
// Note: thread/start's `sandbox` is a kebab-case SandboxMode enum, while
// command/exec's `sandboxPolicy` is a discriminated union keyed on a camelCase
// `type`. These are NOT the same shape — see ThreadStartParams vs
// CommandExecParams in the v2 schema.
func sandboxPolicyForMode(mode string) (map[string]any, bool) {
	switch mode {
	case "read-only":
		return map[string]any{"type": "readOnly"}, true
	case "workspace-write":
		return map[string]any{"type": "workspaceWrite"}, true
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}, true
	default:
		return nil, false
	}
}

// CommandExec runs a standalone argv via codex's `command/exec` RPC — no thread,
// no turn, no model — and returns the buffered exit code, stdout and stderr. A
// nil sandboxPolicy defers to codex's configured default policy; pass one of the
// SandboxPolicy union shapes (see sandboxPolicyForMode) to pin it. Used by the
// broker's sandbox preflight to detect a host whose bubblewrap-based Linux
// sandbox cannot initialize, before a real turn buries that failure in
// per-command output while the model limps on with every shell command failing.
func (a *AppServer) CommandExec(ctx context.Context, argv []string, sandboxPolicy any) (CommandExecResult, error) {
	if !a.initialized.Load() {
		return CommandExecResult{}, ErrNotInitialized
	}
	params := map[string]any{"command": argv}
	if sandboxPolicy != nil {
		params["sandboxPolicy"] = sandboxPolicy
	}
	raw, err := a.call(ctx, "command/exec", params)
	if err != nil {
		return CommandExecResult{}, fmt.Errorf("command/exec: %w", err)
	}
	var resp struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return CommandExecResult{}, fmt.Errorf("command/exec response: %w", err)
	}
	return CommandExecResult{ExitCode: resp.ExitCode, Stdout: resp.Stdout, Stderr: resp.Stderr}, nil
}

// ProbeSandbox runs a trivial no-op (`true`) under the given sandbox MODE via
// CommandExec, so a caller can tell whether that mode's sandbox actually starts
// on this host. mode is a SandboxMode string; an unknown mode is rejected. On a
// host whose bubblewrap sandbox cannot create a user namespace, codex never runs
// the command: the returned Stderr carries the `bwrap: ...` diagnostic with a
// nonzero ExitCode. A working sandbox runs `true` → ExitCode 0, empty Stderr.
func (a *AppServer) ProbeSandbox(ctx context.Context, mode string) (CommandExecResult, error) {
	policy, ok := sandboxPolicyForMode(mode)
	if !ok {
		return CommandExecResult{}, fmt.Errorf("unknown sandbox mode %q", mode)
	}
	return a.CommandExec(ctx, []string{"true"}, policy)
}

// Close releases the codex child. It closes stdin (codex's stdio-transport
// shutdown signal), waits up to 2 seconds for the child to exit, escalating to
// SIGTERM then SIGKILL, then closes the stdout read end and JOINS the reader
// goroutine before returning. Joining the reader is what removes the
// Close-vs-reader race: the previous code could return while the reader was
// still calling cmd.Wait() concurrently, so two goroutines reaped the same
// child. Now exactly one path (Close, via reapChild) reaps the child and the
// reader is guaranteed to have returned by the time Close does. Idempotent and
// safe to call from one goroutine at a time.
func (a *AppServer) Close(_ context.Context) error {
	a.closeOnce.Do(func() {
		a.reapChild()
	})
	return nil
}
