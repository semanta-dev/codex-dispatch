package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PingResult is the typed response for broker.ping.
type PingResult struct {
	Version      string `json:"version"`
	StartedAt    string `json:"started_at,omitempty"`
	TaskCount    int    `json:"task_count"`
	RunningCount int    `json:"running_count"`
	IdleSince    string `json:"idle_since,omitempty"`
}

// RPCError is the typed JSON-RPC error returned by the server. Callers use
// errors.As to extract it from a Client method return.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// isNullID reports whether a response id is absent or the literal JSON null.
// The server stamps id:null on parse / invalid-request errors (which carry no
// usable request id). Such an error applies to the request in flight and MUST
// be surfaced — not skipped as if it were an unrelated notification — or the
// client hangs on a malformed line until EOF/deadline.
func isNullID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	return bytes.Equal(bytes.TrimSpace(id), []byte("null"))
}

// Client is a JSON-RPC client. Production clients use HTTP; tests can still
// wrap an in-memory net.Conn with NewClientFromConn.
type Client struct {
	conn     net.Conn
	reader   *bufio.Reader
	endpoint string
	http     *http.Client

	mu     sync.Mutex // serialises writes
	nextID int64
}

// NewClientFromConn wraps an existing connection. The caller owns Conn's lifecycle.
func NewClientFromConn(conn net.Conn) *Client {
	return &Client{conn: conn, reader: bufio.NewReaderSize(conn, 64*1024)}
}

// Dial connects to a localhost HTTP broker address such as "127.0.0.1:43123".
func Dial(addr string) (*Client, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, fmt.Errorf("broker address required")
	}
	return &Client{
		endpoint: "http://" + strings.TrimSpace(addr) + "/rpc",
		http: &http.Client{
			Timeout: 0,
		},
	}, nil
}

// Close shuts the underlying connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Ping calls broker.ping.
func (c *Client) Ping(ctx context.Context) (*PingResult, error) {
	raw, err := c.rawCall(ctx, "broker.ping", map[string]any{})
	if err != nil {
		return nil, err
	}
	var r PingResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode PingResult: %w", err)
	}
	return &r, nil
}

// Shutdown calls broker.shutdown. force=true cancels running tasks first.
func (c *Client) Shutdown(ctx context.Context, force bool) error {
	_, err := c.rawCall(ctx, "broker.shutdown", map[string]any{"force": force})
	return err
}

// TaskListEntry is one row in task.list output.
type TaskListEntry struct {
	TaskID     string `json:"task_id"`
	State      string `json:"state"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// TaskList calls task.list. sessionID="" returns all tasks.
func (c *Client) TaskList(ctx context.Context, sessionID string) ([]TaskListEntry, error) {
	raw, err := c.rawCall(ctx, "task.list", map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []TaskListEntry `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

// TaskStatus is the typed response for task.status.
type TaskStatus struct {
	TaskID          string `json:"task_id"`
	State           string `json:"state"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	FellBackToFresh bool   `json:"fell_back_to_fresh"`
	EventCount      int    `json:"event_count"`
}

// TaskStatusCall calls task.status.
func (c *Client) TaskStatusCall(ctx context.Context, taskID string) (*TaskStatus, error) {
	raw, err := c.rawCall(ctx, "task.status", map[string]any{"task_id": taskID})
	if err != nil {
		return nil, err
	}
	var st TaskStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TaskCancel calls task.cancel.
func (c *Client) TaskCancel(ctx context.Context, taskID string) error {
	_, err := c.rawCall(ctx, "task.cancel", map[string]any{"task_id": taskID})
	return err
}

// TaskStartResult is the response from task.start.
type TaskStartResult struct {
	TaskID string `json:"task_id"`
	Queued bool   `json:"queued"`
}

// TaskStart calls task.start, which enqueues a dispatch in the background and
// returns immediately with the assigned task_id.
func (c *Client) TaskStart(ctx context.Context, p DispatchRunParams) (string, bool, error) {
	raw, err := c.rawCall(ctx, "task.start", p)
	if err != nil {
		return "", false, err
	}
	var r TaskStartResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", false, fmt.Errorf("decode TaskStartResult: %w", err)
	}
	return r.TaskID, r.Queued, nil
}

// SessionRegister calls session.register.
func (c *Client) SessionRegister(ctx context.Context, sessionID, cwd string) error {
	_, err := c.rawCall(ctx, "session.register", map[string]any{"session_id": sessionID, "cwd": cwd})
	return err
}

// SessionDeregister calls session.deregister.
func (c *Client) SessionDeregister(ctx context.Context, sessionID string, cancelQueued bool) ([]string, error) {
	raw, err := c.rawCall(ctx, "session.deregister", map[string]any{"session_id": sessionID, "cancel_queued": cancelQueued})
	if err != nil {
		return nil, err
	}
	var resp struct {
		CancelledTaskIDs []string `json:"cancelled_task_ids"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.CancelledTaskIDs, nil
}

// DispatchEvent is an event delivered to the DispatchRun callback.
type DispatchEvent struct {
	TaskID  string          `json:"task_id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type taskEventAssembly struct {
	taskID string
	seq    int64
	typ    string
	total  int
	parts  [][]byte
}

type taskEventReassembler struct {
	chunks map[string]*taskEventAssembly
}

func (r *taskEventReassembler) handle(method string, params json.RawMessage, onEvent func(DispatchEvent)) (bool, error) {
	switch method {
	case "task.event":
		if onEvent != nil {
			var ev DispatchEvent
			if err := json.Unmarshal(params, &ev); err == nil {
				onEvent(ev)
			}
		}
		return true, nil
	case taskEventChunkMethod:
		var chunk taskEventChunkParams
		if err := json.Unmarshal(params, &chunk); err != nil {
			return true, fmt.Errorf("decode task.event chunk: %w", err)
		}
		ev, err := r.addChunk(chunk)
		if err != nil {
			return true, err
		}
		if ev != nil && onEvent != nil {
			onEvent(*ev)
		}
		return true, nil
	default:
		return false, nil
	}
}

func (r *taskEventReassembler) addChunk(chunk taskEventChunkParams) (*DispatchEvent, error) {
	if chunk.ChunkID == "" {
		return nil, fmt.Errorf("invalid task.event chunk: missing chunk_id")
	}
	if chunk.TotalChunks <= 0 {
		return nil, fmt.Errorf("invalid task.event chunk %s: total_chunks=%d", chunk.ChunkID, chunk.TotalChunks)
	}
	if chunk.ChunkIndex < 0 || chunk.ChunkIndex >= chunk.TotalChunks {
		return nil, fmt.Errorf("invalid task.event chunk %s: chunk_index=%d total_chunks=%d", chunk.ChunkID, chunk.ChunkIndex, chunk.TotalChunks)
	}
	if r.chunks == nil {
		r.chunks = map[string]*taskEventAssembly{}
	}
	a := r.chunks[chunk.ChunkID]
	if a == nil {
		if chunk.ChunkIndex != 0 {
			return nil, fmt.Errorf("invalid task.event chunk %s: gap before chunk %d", chunk.ChunkID, chunk.ChunkIndex)
		}
		a = &taskEventAssembly{
			taskID: chunk.TaskID,
			seq:    chunk.Seq,
			typ:    chunk.Type,
			total:  chunk.TotalChunks,
		}
		r.chunks[chunk.ChunkID] = a
	}
	if a.taskID != chunk.TaskID || a.seq != chunk.Seq || a.typ != chunk.Type || a.total != chunk.TotalChunks {
		return nil, fmt.Errorf("invalid task.event chunk %s: metadata mismatch", chunk.ChunkID)
	}
	if chunk.ChunkIndex < len(a.parts) {
		return nil, fmt.Errorf("invalid task.event chunk %s: duplicate chunk %d", chunk.ChunkID, chunk.ChunkIndex)
	}
	if chunk.ChunkIndex != len(a.parts) {
		return nil, fmt.Errorf("invalid task.event chunk %s: gap before chunk %d", chunk.ChunkID, chunk.ChunkIndex)
	}
	a.parts = append(a.parts, append([]byte(nil), chunk.Payload...))
	if len(a.parts) != a.total {
		return nil, nil
	}
	delete(r.chunks, chunk.ChunkID)
	return &DispatchEvent{
		TaskID:  a.taskID,
		Seq:     a.seq,
		Type:    a.typ,
		Payload: json.RawMessage(bytes.Join(a.parts, nil)),
	}, nil
}

// DispatchRun calls dispatch.run, delivering task.event notifications to
// the callback (nil drops them) and returning the final
// DispatchRunResult. Mirrors rawCall's deadline + cancel handling.
func (c *Client) DispatchRun(ctx context.Context, p DispatchRunParams, onEvent func(DispatchEvent)) (*DispatchRunResult, error) {
	if c.endpoint != "" {
		return c.dispatchRunHTTP(ctx, p, onEvent)
	}
	id, raw, err := c.marshalRequest("dispatch.run", p)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	_, werr := c.conn.Write(raw)
	c.mu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("write request: %w", werr)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
		defer c.conn.SetReadDeadline(time.Time{})
	}
	doneCh := make(chan struct{})
	defer close(doneCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.conn.SetReadDeadline(time.Now())
		case <-doneCh:
		}
	}()

	reassembler := &taskEventReassembler{}
	for {
		line, err := ReadLine(c.reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("broker closed connection")
			}
			return nil, err
		}
		var msg struct {
			JSONRPC         string          `json:"jsonrpc"`
			ProtocolVersion string          `json:"_protocol_version"`
			Method          string          `json:"method"`
			Params          json.RawMessage `json:"params"`
			Result          json.RawMessage `json:"result"`
			Error           *RPCError       `json:"error"`
			ID              json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if msg.ProtocolVersion != "" && msg.ProtocolVersion != ProtocolVersion {
			return nil, fmt.Errorf("protocol version mismatch: server says %q, client speaks %q", msg.ProtocolVersion, ProtocolVersion)
		}
		if handled, err := reassembler.handle(msg.Method, msg.Params, onEvent); handled {
			if err != nil {
				return nil, err
			}
			continue
		}
		// A null-id error response is a server parse / invalid-request error for
		// this dispatch.run — surface it rather than skipping it (see isNullID).
		if isNullID(msg.ID) {
			if msg.Error != nil {
				return nil, msg.Error
			}
			continue
		}
		var gotID int64
		if err := json.Unmarshal(msg.ID, &gotID); err != nil || gotID != id {
			continue
		}
		if msg.Error != nil {
			return nil, msg.Error
		}
		var result DispatchRunResult
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return nil, fmt.Errorf("decode DispatchRunResult: %w", err)
		}
		return &result, nil
	}
}

// rawCall sends a request and waits for the matched response. params may be nil.
func (c *Client) rawCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.endpoint != "" {
		return c.rawCallHTTP(ctx, method, params)
	}
	id, raw, err := c.marshalRequest(method, params)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	_, werr := c.conn.Write(raw)
	c.mu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("write request: %w", werr)
	}

	// Honor ctx via the connection's read deadline. If ctx is cancelled
	// during the read, the deadline propagation goroutine pokes the
	// connection so ReadLine returns promptly.
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
		defer c.conn.SetReadDeadline(time.Time{})
	}
	doneCh := make(chan struct{})
	defer close(doneCh)
	go func() {
		select {
		case <-ctx.Done():
			// Force ReadLine to return by setting an immediate deadline.
			_ = c.conn.SetReadDeadline(time.Now())
		case <-doneCh:
		}
	}()

	// Read until we find the matching id. Notifications (no id) are skipped
	// here — they're consumed by a separate goroutine when streaming.
	for {
		line, err := ReadLine(c.reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("broker closed connection")
			}
			return nil, err
		}
		var env struct {
			JSONRPC         string          `json:"jsonrpc"`
			ProtocolVersion string          `json:"_protocol_version"`
			Result          json.RawMessage `json:"result"`
			Error           *RPCError       `json:"error"`
			ID              json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if env.ProtocolVersion != "" && env.ProtocolVersion != ProtocolVersion {
			return nil, fmt.Errorf("protocol version mismatch: server says %q, client speaks %q", env.ProtocolVersion, ProtocolVersion)
		}
		// A null-id error response is a server parse / invalid-request error for
		// the request in flight (the server could not echo our id). Surface it
		// rather than skipping it — otherwise the client loops until EOF.
		if isNullID(env.ID) {
			if env.Error != nil {
				return nil, env.Error
			}
			continue // genuine notification (no id, no error) — skip
		}
		var gotID int64
		if err := json.Unmarshal(env.ID, &gotID); err != nil {
			continue
		}
		if gotID != id {
			continue
		}
		if env.Error != nil {
			return nil, env.Error
		}
		return env.Result, nil
	}
}

func (c *Client) marshalRequest(method string, params any) (int64, []byte, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      id,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return 0, nil, err
	}
	raw = append(raw, '\n')
	return id, raw, nil
}

func (c *Client) post(ctx context.Context, raw []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("broker http %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (c *Client) rawCallHTTP(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, raw, err := c.marshalRequest(method, params)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := ReadLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("broker closed response")
			}
			return nil, err
		}
		var env struct {
			JSONRPC         string          `json:"jsonrpc"`
			ProtocolVersion string          `json:"_protocol_version"`
			Result          json.RawMessage `json:"result"`
			Error           *RPCError       `json:"error"`
			ID              json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if env.ProtocolVersion != "" && env.ProtocolVersion != ProtocolVersion {
			return nil, fmt.Errorf("protocol version mismatch: server says %q, client speaks %q", env.ProtocolVersion, ProtocolVersion)
		}
		// Surface a null-id parse / invalid-request error for the request in
		// flight instead of silently skipping it (see isNullID).
		if isNullID(env.ID) {
			if env.Error != nil {
				return nil, env.Error
			}
			continue
		}
		var gotID int64
		if json.Unmarshal(env.ID, &gotID) != nil || gotID != id {
			continue
		}
		if env.Error != nil {
			return nil, env.Error
		}
		return env.Result, nil
	}
}

func (c *Client) dispatchRunHTTP(ctx context.Context, p DispatchRunParams, onEvent func(DispatchEvent)) (*DispatchRunResult, error) {
	id, raw, err := c.marshalRequest("dispatch.run", p)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	reassembler := &taskEventReassembler{}
	for {
		line, err := ReadLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("broker closed response")
			}
			return nil, err
		}
		var msg struct {
			JSONRPC         string          `json:"jsonrpc"`
			ProtocolVersion string          `json:"_protocol_version"`
			Method          string          `json:"method"`
			Params          json.RawMessage `json:"params"`
			Result          json.RawMessage `json:"result"`
			Error           *RPCError       `json:"error"`
			ID              json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if msg.ProtocolVersion != "" && msg.ProtocolVersion != ProtocolVersion {
			return nil, fmt.Errorf("protocol version mismatch: server says %q, client speaks %q", msg.ProtocolVersion, ProtocolVersion)
		}
		if handled, err := reassembler.handle(msg.Method, msg.Params, onEvent); handled {
			if err != nil {
				return nil, err
			}
			continue
		}
		// Surface a null-id parse / invalid-request error rather than skipping it.
		if isNullID(msg.ID) {
			if msg.Error != nil {
				return nil, msg.Error
			}
			continue
		}
		var gotID int64
		if json.Unmarshal(msg.ID, &gotID) != nil || gotID != id {
			continue
		}
		if msg.Error != nil {
			return nil, msg.Error
		}
		var result DispatchRunResult
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return nil, fmt.Errorf("decode DispatchRunResult: %w", err)
		}
		return &result, nil
	}
}
