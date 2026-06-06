package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// State of a task.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateDone      State = "done"
	StateCancelled State = "cancelled"
	StateErrored   State = "errored"
)

// IsTerminal reports whether a state can transition further.
func (s State) IsTerminal() bool {
	switch s {
	case StateDone, StateCancelled, StateErrored:
		return true
	}
	return false
}

// TaskParams is the set of fields needed to start a task. Subset of the
// dispatch params; the dispatch handler will pass the rest through to codex
// app-server directly.
type TaskParams struct {
	Mode        string // "fresh" | "resume"
	Prompt      string
	Sandbox     string
	PrevSession string // empty when Mode != "resume"
	ResultDir   string
	LogPath     string // <run_dir>/stdout.log
	CWD         string // request working tree; may differ from broker process cwd
}

// Task is one entry in the table.
type Task struct {
	ID              string
	SessionID       string
	State           State
	Params          TaskParams
	EnqueuedAt      time.Time // set in Start(); never reset
	StartedAt       time.Time // set in MarkRunning(); zero while State == StateQueued
	FinishedAt      time.Time // zero if !IsTerminal()
	ExitCode        int       // valid only when State == StateDone or StateErrored
	CodexSession    string    // thread_id or session_id parsed from the run
	FellBackToFresh bool
	EventCount      int
	ErrorMessage    string // broker-side failure reason; set by MarkErrored / timeout
}

// Event is one entry in a task's ring buffer.
type Event struct {
	Seq     int64
	Type    string // "codex.*" or "task.*"
	Payload []byte // raw JSON for the event body
}

// Sentinel errors.
var (
	ErrTaskNotFound = errors.New("task not found")
	// ErrEventsLost signals ring OVERFLOW: the requested sinceSeq is older than
	// the oldest event still buffered in memory (the ring holds only the most
	// recent ringSize events). It is the in-memory replay's "you fell behind"
	// signal; the durable record is always stdout.log on disk. See Events for
	// the full event-loss/replay contract.
	ErrEventsLost          = errors.New("events lost from ring")
	ErrTaskAlreadyTerminal = errors.New("task is in a terminal state")
	ErrIllegalTransition   = errors.New("illegal state transition")
)

// Table is the in-memory task table. Safe for concurrent use.
type Table struct {
	mu sync.Mutex

	concurrencyCap int // max concurrent running tasks
	ringSize       int // events kept per task

	// Eviction policy bounds task-table growth so a long-lived broker that has
	// run thousands of tasks does not retain every terminal Task and its
	// ringSize-slot event ring forever (a steady, unbounded leak).
	//
	//   maxTerminalTasks: when > 0, never keep more than this many TERMINAL
	//     tasks; the oldest terminal tasks (by FinishedAt, insertion order as a
	//     tiebreak) are dropped once the count exceeds the cap. Non-terminal
	//     tasks (queued/running) are never evicted.
	//   terminalTTL: when > 0, a terminal task whose FinishedAt is older than
	//     the TTL is dropped at the next mutation regardless of the cap.
	//
	// A dropped task's record (and therefore its event ring) becomes
	// unreachable and is reclaimed by the GC; a subsequent task.status for an
	// evicted id returns ErrTaskNotFound, and stdout.log on disk remains the
	// durable record (see the Events replay contract).
	maxTerminalTasks int
	terminalTTL      time.Duration
	// now is the clock used for TTL comparisons; overridable in tests.
	now func() time.Time

	tasks    map[string]*taskRecord
	order    []string // insertion order for List
	sessions map[string]struct{}

	// onActivity, when set, is invoked (outside the table lock) whenever a
	// task is created, transitions state, or appends an event. The broker
	// wires this to the idle timer's Reset so any task progress — including a
	// detached run streaming notifications — postpones idle-out. nil by
	// default so tests that build a bare Table are unaffected.
	onActivity func()

	// detached, when set, owns the broker-lifetime context and WaitGroup that
	// bind background (task.start) goroutines to the broker's lifecycle. Stored
	// on the Table because BrokerState is wired through state.Table everywhere
	// and the detached path (runDetached) and the broker shutdown path both
	// need it. nil by default → detached runs fall back to context.Background()
	// (unchanged legacy behavior for bare-Table tests).
	detached *DetachedRunner
}

// DetachedRunner binds background (task.start) goroutines to the broker
// lifecycle. Its context is derived from the broker-lifetime ctx (cancelled on
// shutdown/idle-out), and its WaitGroup lets the serve loop drain in-flight
// detached runs before the process exits — so a detached codex turn is never
// yanked out from under itself. Safe for concurrent use.
type DetachedRunner struct {
	ctx context.Context
	wg  sync.WaitGroup
}

// NewDetachedRunner returns a runner whose Context derives from parent. When
// parent is cancelled (broker shutdown / idle-out), every detached task's
// context is cancelled too, so its drain returns and releases the run slot.
func NewDetachedRunner(parent context.Context) *DetachedRunner {
	if parent == nil {
		parent = context.Background()
	}
	return &DetachedRunner{ctx: parent}
}

// Context returns the broker-lifetime context detached runs derive from.
func (r *DetachedRunner) Context() context.Context {
	if r == nil {
		return context.Background()
	}
	return r.ctx
}

// Go launches fn in a tracked goroutine. Wait blocks until all launched fn
// return. A nil runner runs fn inline-tracked via a throwaway goroutine so
// callers don't have to nil-check.
func (r *DetachedRunner) Go(fn func()) {
	if r == nil {
		go fn()
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn()
	}()
}

// Wait blocks until all goroutines launched via Go have returned. Used by the
// broker serve loop on shutdown to drain detached runs.
func (r *DetachedRunner) Wait() {
	if r == nil {
		return
	}
	r.wg.Wait()
}

// SetDetachedRunner installs the broker-lifetime detached runner. Pass nil to
// clear. Safe to call before serving.
func (t *Table) SetDetachedRunner(r *DetachedRunner) {
	t.mu.Lock()
	t.detached = r
	t.mu.Unlock()
}

// DetachedRunner returns the installed runner (or nil).
func (t *Table) DetachedRunner() *DetachedRunner {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.detached
}

type taskRecord struct {
	task    Task
	events  []Event // ring buffer; sized at ringSize
	head    int     // next write index in events
	totalEv int     // total events appended (including evicted)
}

// NewTable constructs a Table with the given concurrency cap and per-task
// event ring buffer size. The terminal-task eviction policy defaults from the
// environment (CODEX_BROKER_MAX_TERMINAL_TASKS, CODEX_BROKER_TASK_TTL_MS) with
// a built-in cap so a long-lived broker cannot leak unboundedly even when the
// operator sets nothing. broker.go does not need to wire the policy explicitly;
// tests override it deterministically via SetEvictionPolicy.
func NewTable(concurrencyCap, ringSize int) *Table {
	if concurrencyCap < 1 {
		concurrencyCap = 1
	}
	if ringSize < 1 {
		ringSize = 1
	}
	return &Table{
		concurrencyCap:   concurrencyCap,
		ringSize:         ringSize,
		maxTerminalTasks: envTerminalCap(),
		terminalTTL:      envTerminalTTL(),
		now:              func() time.Time { return time.Now().UTC() },
		tasks:            map[string]*taskRecord{},
		sessions:         map[string]struct{}{},
	}
}

// defaultMaxTerminalTasks bounds retained terminal tasks when the operator sets
// no override. Large enough that interactive use never evicts a task the user
// still cares about, small enough that an unattended long-lived broker cannot
// leak unboundedly.
const defaultMaxTerminalTasks = 1024

// envTerminalCap returns the terminal-task cap from
// CODEX_BROKER_MAX_TERMINAL_TASKS. A value of 0 disables the cap (unbounded);
// a negative/unparseable value falls back to the built-in default.
func envTerminalCap() int {
	if s := os.Getenv("CODEX_BROKER_MAX_TERMINAL_TASKS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return n
		}
	}
	return defaultMaxTerminalTasks
}

// envTerminalTTL returns the terminal-task TTL from CODEX_BROKER_TASK_TTL_MS. A
// value <= 0 (or unset/unparseable) disables the TTL (default: off).
func envTerminalTTL() time.Duration {
	if s := os.Getenv("CODEX_BROKER_TASK_TTL_MS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 0
}

// SetEvictionPolicy overrides the terminal-task eviction bounds. maxTerminal<=0
// disables the cap; ttl<=0 disables the TTL. Intended for tests and explicit
// wiring; safe to call before serving. Applying the policy may immediately
// evict already-terminal tasks that violate the new bounds.
func (t *Table) SetEvictionPolicy(maxTerminal int, ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if maxTerminal < 0 {
		maxTerminal = 0
	}
	if ttl < 0 {
		ttl = 0
	}
	t.maxTerminalTasks = maxTerminal
	t.terminalTTL = ttl
	t.evictTerminalLocked()
}

// setClock overrides the TTL clock for deterministic tests.
func (t *Table) setClock(now func() time.Time) {
	t.mu.Lock()
	t.now = now
	t.mu.Unlock()
}

// SetOnActivity registers a callback fired (outside the table lock) on every
// task creation, state transition, and event append. The broker uses it to
// reset the idle timer so an active or long-running task — sync or detached —
// keeps the broker alive. Pass nil to clear. Safe to call before serving.
func (t *Table) SetOnActivity(fn func()) {
	t.mu.Lock()
	t.onActivity = fn
	t.mu.Unlock()
}

// noteActivity invokes the activity callback (if any). It must be called with
// the table lock NOT held, since the callback may itself touch broker state.
func (t *Table) noteActivity(fn func()) {
	if fn != nil {
		fn()
	}
}

// HasNonTerminal reports whether any task is still queued or running. The idle
// guard uses it to refuse idle-out (and re-arm) while work is outstanding so a
// long turn — or a queued task waiting for a slot — is never killed mid-flight.
func (t *Table) HasNonTerminal() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nonTerminalCountLocked() > 0
}

// Start registers a new task. Returns (task_id, queued) where queued=true
// means the task will have to wait for a run slot before it can run.
//
// The queued flag is reconciled with the authoritative run-slot semaphore
// (BrokerState.scheduler, capacity == concurrencyCap): every non-terminal task
// either already holds a slot (running) or is waiting to acquire one (queued),
// so the count of contenders for a slot is exactly nonTerminalCountLocked().
// When that count has already reached the cap, the slots are committed and this
// new task must wait — queued=true. This matches what acquireRunSlot observes:
// it blocks precisely when concurrencyCap slots are taken. (The flag is
// advisory — the semaphore, not this snapshot, is what actually gates the run —
// but it now predicts the semaphore's decision rather than a divergent count.)
func (t *Table) Start(sessionID string, params TaskParams) (string, bool) {
	t.mu.Lock()
	// Drop terminal tasks that have aged out before adding a new one so the
	// table stays bounded across a long-lived broker's lifetime.
	t.evictTerminalLocked()
	id := newTaskID()
	queued := t.nonTerminalCountLocked() >= t.concurrencyCap
	rec := &taskRecord{
		task: Task{
			ID:         id,
			SessionID:  sessionID,
			State:      StateQueued,
			Params:     params,
			EnqueuedAt: t.nowUTC(),
		},
		events: make([]Event, t.ringSize),
	}
	t.tasks[id] = rec
	t.order = append(t.order, id)
	fn := t.onActivity
	t.mu.Unlock()
	t.noteActivity(fn)
	return id, queued
}

// ConcurrencyCap returns the configured maximum number of running tasks.
func (t *Table) ConcurrencyCap() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.concurrencyCap
}

// MarkRunning transitions queued → running. Returns ErrIllegalTransition if
// the task is not in queued state.
func (t *Table) MarkRunning(id string) error {
	t.mu.Lock()
	rec, ok := t.tasks[id]
	if !ok {
		t.mu.Unlock()
		return ErrTaskNotFound
	}
	if rec.task.State != StateQueued {
		err := fmt.Errorf("%w: %s → running", ErrIllegalTransition, rec.task.State)
		t.mu.Unlock()
		return err
	}
	rec.task.State = StateRunning
	rec.task.StartedAt = t.nowUTC()
	fn := t.onActivity
	t.mu.Unlock()
	t.noteActivity(fn)
	return nil
}

// MarkDone transitions running → done with the codex exit code and session id.
func (t *Table) MarkDone(id string, exitCode int, codexSession string, fellBack bool) error {
	t.mu.Lock()
	rec, ok := t.tasks[id]
	if !ok {
		t.mu.Unlock()
		return ErrTaskNotFound
	}
	if rec.task.State != StateRunning {
		err := fmt.Errorf("%w: %s → done", ErrIllegalTransition, rec.task.State)
		t.mu.Unlock()
		return err
	}
	rec.task.State = StateDone
	rec.task.FinishedAt = t.nowUTC()
	rec.task.ExitCode = exitCode
	rec.task.CodexSession = codexSession
	rec.task.FellBackToFresh = fellBack
	t.evictTerminalLocked()
	fn := t.onActivity
	t.mu.Unlock()
	t.noteActivity(fn)
	return nil
}

// MarkErrored transitions running → errored with a broker-side error. The
// reason is retained on the task and surfaced via task.status so a failed
// detached run (which writes no result.json) still has a machine-readable
// error_message.
func (t *Table) MarkErrored(id string, exitCode int, reason string) error {
	t.mu.Lock()
	rec, ok := t.tasks[id]
	if !ok {
		t.mu.Unlock()
		return ErrTaskNotFound
	}
	if rec.task.State != StateRunning && rec.task.State != StateQueued {
		err := fmt.Errorf("%w: %s → errored", ErrIllegalTransition, rec.task.State)
		t.mu.Unlock()
		return err
	}
	rec.task.State = StateErrored
	rec.task.FinishedAt = t.nowUTC()
	rec.task.ExitCode = exitCode
	rec.task.ErrorMessage = reason
	t.evictTerminalLocked()
	fn := t.onActivity
	t.mu.Unlock()
	t.noteActivity(fn)
	return nil
}

// Cancel transitions a non-terminal task to cancelled. From queued it goes
// directly; from running the caller is responsible for terminating the
// underlying codex process.
func (t *Table) Cancel(id string) error {
	t.mu.Lock()
	rec, ok := t.tasks[id]
	if !ok {
		t.mu.Unlock()
		return ErrTaskNotFound
	}
	if rec.task.State.IsTerminal() {
		err := fmt.Errorf("%w: state=%s", ErrTaskAlreadyTerminal, rec.task.State)
		t.mu.Unlock()
		return err
	}
	rec.task.State = StateCancelled
	rec.task.FinishedAt = t.nowUTC()
	t.evictTerminalLocked()
	fn := t.onActivity
	t.mu.Unlock()
	t.noteActivity(fn)
	return nil
}

// MarkCancelled records a terminal cancelled state with an exit code, used by
// the dispatch drain when a running turn is interrupted (operator cancel or a
// per-turn deadline). Unlike Cancel it is a no-op when the task is already
// terminal (e.g. a concurrent task.cancel already moved it to cancelled), so
// the drain can call it unconditionally after interrupting the turn.
func (t *Table) MarkCancelled(id string, exitCode int) error {
	t.mu.Lock()
	rec, ok := t.tasks[id]
	if !ok {
		t.mu.Unlock()
		return ErrTaskNotFound
	}
	if rec.task.State.IsTerminal() {
		t.mu.Unlock()
		return nil
	}
	rec.task.State = StateCancelled
	rec.task.FinishedAt = t.nowUTC()
	rec.task.ExitCode = exitCode
	t.evictTerminalLocked()
	fn := t.onActivity
	t.mu.Unlock()
	t.noteActivity(fn)
	return nil
}

// Status returns a snapshot of the task.
func (t *Table) Status(id string) (Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.tasks[id]
	if !ok {
		return Task{}, ErrTaskNotFound
	}
	task := rec.task
	task.EventCount = rec.totalEv
	return task, nil
}

// List returns all tasks (insertion order); when sessionID != "" filters to
// that session.
func (t *Table) List(sessionID string) []Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Task, 0, len(t.order))
	for _, id := range t.order {
		rec, ok := t.tasks[id]
		if !ok {
			continue // evicted; order is compacted lazily under the same lock
		}
		if sessionID != "" && rec.task.SessionID != sessionID {
			continue
		}
		task := rec.task
		task.EventCount = rec.totalEv
		out = append(out, task)
	}
	return out
}

// RunningCount returns the number of tasks currently in the running state. The
// broker uses it for broker.ping reporting and to gate a non-forced shutdown
// (refuse while any turn is in flight) without re-walking the whole table at
// each call site.
func (t *Table) RunningCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runningCountLocked()
}

// AppendEvent records an event for the given task and returns its sequence
// number (monotonic per task starting at 1).
func (t *Table) AppendEvent(id, eventType string, payload []byte) (int64, error) {
	t.mu.Lock()
	rec, ok := t.tasks[id]
	if !ok {
		t.mu.Unlock()
		return 0, ErrTaskNotFound
	}
	rec.totalEv++
	rec.events[rec.head] = Event{Seq: int64(rec.totalEv), Type: eventType, Payload: append([]byte(nil), payload...)}
	rec.head = (rec.head + 1) % len(rec.events)
	rec.task.EventCount = rec.totalEv
	seq := int64(rec.totalEv)
	fn := t.onActivity
	t.mu.Unlock()
	// Reset idle on streaming progress: a genuinely in-flight turn emits a
	// steady stream of notifications, so this keeps a long task — sync or
	// detached — alive even with a small CODEX_BROKER_IDLE_MS.
	t.noteActivity(fn)
	return seq, nil
}

// Events returns events with Seq >= sinceSeq (inclusive). Returns
// ErrEventsLost when sinceSeq < oldest, where oldest is the smallest seq
// still in the ring (computed as max(1, totalEv-ringSize+1)).
//
// Event-loss / replay contract (authoritative for BOTH the streaming
// dispatch.run path and the detached task.start replay path):
//
//   - The per-task ring holds at most ringSize (default 2048) of the most
//     recent events. Once a task emits MORE than ringSize events, the oldest
//     are overwritten and are no longer replayable from memory — this is ring
//     OVERFLOW, signalled by ErrEventsLost.
//   - Seq is monotonic per task starting at 1 and is never reused, so a caller
//     can always detect a gap: after processing up to last_seen, the next call
//     Events(id, last_seen+1) returns either the strictly-newer events or
//     ErrEventsLost (meaning last_seen+1 has already been evicted).
//   - The ring is a streaming OPTIMISATION, not the system of record. Every
//     event is also written to <run_dir>/stdout.log (LogWriter) before it is
//     relayed, so the on-disk log is the durable, complete representation. On
//     ErrEventsLost a caller MUST fall back to stdout.log to recover the gap;
//     it must not treat lost events as "no events".
//   - Eviction interaction: a terminal task may be dropped from the table
//     entirely (see the eviction policy), after which Events returns
//     ErrTaskNotFound rather than ErrEventsLost. Callers distinguish the two:
//     ErrEventsLost ⇒ task still present, replay from stdout.log; ErrTaskNotFound
//     ⇒ task aged out, the only record is stdout.log on disk.
//
// To safely catch up after a disconnect, callers should:
//  1. Track the highest seq they have already processed (last_seen).
//  2. Call Events(id, last_seen+1) to get strictly newer events.
//  3. On ErrEventsLost (ring overflow) OR ErrTaskNotFound (task evicted), fall
//     back to stdout.log on disk for the durable, complete record.
func (t *Table) Events(id string, sinceSeq int64) ([]Event, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.tasks[id]
	if !ok {
		return nil, ErrTaskNotFound
	}
	oldest := int64(rec.totalEv - len(rec.events) + 1)
	if oldest < 1 {
		oldest = 1
	}
	if sinceSeq < oldest {
		return nil, fmt.Errorf("%w: requested sinceSeq=%d, oldest available=%d", ErrEventsLost, sinceSeq, oldest)
	}
	if sinceSeq > int64(rec.totalEv) {
		return nil, nil
	}

	// Collect events in chronological order from the ring.
	// sinceSeq is inclusive: return events with Seq >= sinceSeq.
	out := make([]Event, 0, int64(rec.totalEv)-sinceSeq+1)
	// Walk backwards from head looking for events with Seq >= sinceSeq.
	for i := 0; i < len(rec.events); i++ {
		idx := (rec.head - 1 - i + len(rec.events)) % len(rec.events)
		ev := rec.events[idx]
		if ev.Seq == 0 {
			break // empty slot; nothing more
		}
		if ev.Seq < sinceSeq {
			break
		}
		out = append([]Event{ev}, out...)
	}
	return out, nil
}

func (t *Table) runningCountLocked() int {
	n := 0
	for _, rec := range t.tasks {
		if rec.task.State == StateRunning {
			n++
		}
	}
	return n
}

func (t *Table) nonTerminalCountLocked() int {
	n := 0
	for _, rec := range t.tasks {
		if !rec.task.State.IsTerminal() {
			n++
		}
	}
	return n
}

// nowUTC returns the current time via the (overridable) clock, defaulting to
// time.Now().UTC() when no clock is installed (bare-Table tests / zero value).
func (t *Table) nowUTC() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now().UTC()
}

// evictTerminalLocked drops terminal tasks that violate the eviction policy:
// first any whose FinishedAt is older than terminalTTL, then — if a positive
// maxTerminalTasks cap is still exceeded — the oldest terminal tasks (by
// insertion order, which mirrors FinishedAt closely enough and is the order
// List/replay walk) until the cap holds. Non-terminal tasks are never evicted,
// so a running or queued task can never be reaped out from under its drain.
// Dropping a record drops its event ring with it (GC-reclaimed). Must be called
// with t.mu held.
func (t *Table) evictTerminalLocked() {
	if t.terminalTTL <= 0 && t.maxTerminalTasks <= 0 {
		return
	}

	// Pass 1: TTL eviction.
	if t.terminalTTL > 0 {
		cutoff := t.nowUTC().Add(-t.terminalTTL)
		for id, rec := range t.tasks {
			if rec.task.State.IsTerminal() && !rec.task.FinishedAt.IsZero() && rec.task.FinishedAt.Before(cutoff) {
				delete(t.tasks, id)
			}
		}
		t.compactOrderLocked()
	}

	// Pass 2: cap eviction. Walk insertion order (oldest first) and drop
	// terminal tasks until the terminal count is within the cap.
	if t.maxTerminalTasks > 0 {
		terminal := t.terminalCountLocked()
		if terminal <= t.maxTerminalTasks {
			return
		}
		toDrop := terminal - t.maxTerminalTasks
		for _, id := range t.order {
			if toDrop <= 0 {
				break
			}
			rec, ok := t.tasks[id]
			if !ok {
				continue
			}
			if rec.task.State.IsTerminal() {
				delete(t.tasks, id)
				toDrop--
			}
		}
		t.compactOrderLocked()
	}
}

// terminalCountLocked counts tasks in a terminal state. Must hold t.mu.
func (t *Table) terminalCountLocked() int {
	n := 0
	for _, rec := range t.tasks {
		if rec.task.State.IsTerminal() {
			n++
		}
	}
	return n
}

// compactOrderLocked rebuilds t.order to drop ids no longer present in t.tasks,
// preserving the surviving insertion order. Must hold t.mu.
func (t *Table) compactOrderLocked() {
	kept := t.order[:0]
	for _, id := range t.order {
		if _, ok := t.tasks[id]; ok {
			kept = append(kept, id)
		}
	}
	// Zero the tail so evicted ids are not pinned by the backing array.
	for i := len(kept); i < len(t.order); i++ {
		t.order[i] = ""
	}
	t.order = kept
}

// RegisterSession marks a Claude session as active. Idempotent.
func (t *Table) RegisterSession(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessions == nil {
		t.sessions = map[string]struct{}{}
	}
	t.sessions[sessionID] = struct{}{}
}

// DeregisterSession marks a Claude session as inactive. When cancelQueued is
// true, all queued tasks for that session are cancelled and their ids returned.
// Running tasks for the session keep running.
func (t *Table) DeregisterSession(sessionID string, cancelQueued bool) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, sessionID)
	if !cancelQueued {
		return nil
	}
	var cancelled []string
	for _, id := range t.order {
		rec, ok := t.tasks[id]
		if !ok {
			continue
		}
		if rec.task.SessionID != sessionID {
			continue
		}
		if rec.task.State == StateQueued {
			rec.task.State = StateCancelled
			rec.task.FinishedAt = t.nowUTC()
			cancelled = append(cancelled, id)
		}
	}
	return cancelled
}

func newTaskID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal; fall back to a coarse counter.
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return "t_" + hex.EncodeToString(b)
}
