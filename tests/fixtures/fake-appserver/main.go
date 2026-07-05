// fake-appserver is a Go binary that impersonates `codex` for tests.
//
//	codex --version    → prints "codex-cli <FAKE_CODEX_VERSION or 0.130.0>"
//	codex app-server   → speaks the real codex app-server JSON-RPC v2
//	                     protocol on stdin/stdout (subset our broker uses)
//
// JSON-RPC client→server methods we handle:
//
//	initialize        → respond {serverInfo, capabilities}
//	thread/start      → respond {thread: {id, cwd, status}}, then emit thread/started
//	thread/resume     → respond {thread: {...}}, or -32004 if id matches
//	                    FAKE_APPSERVER_STALE_RESUME
//	turn/start        → respond {turn: {id, status:"pending"}} then emit
//	                    turn/started, optional script events, FAKE_APPSERVER_EDIT
//	                    item events, and turn/completed
//	command/exec      → respond {exitCode, stdout, stderr}; healthy (exit 0) by
//	                    default, or a bwrap uid-map failure under
//	                    FAKE_APPSERVER_BWRAP_BROKEN (the sandbox-preflight probe)
//	shutdown          → respond {} and exit
//
// JSON-RPC client→server notifications we accept:
//
//	initialized       → no-op
//
// Anything else → -32601 Method not found.
//
// Env vars (read via the getenv parameter passed to run):
//
//	FAKE_CODEX_VERSION          — version reported by --version (default 0.130.0)
//	FAKE_APPSERVER_EDIT="P:C"   — newline-separated path:content edits, applied
//	                              during turn/start
//	FAKE_APPSERVER_SESSION="ID" — thread.id returned by thread/start
//	FAKE_APPSERVER_EXIT="N"     — turn.status: "completed" | "failed" |
//	                              "cancelled" (default "completed")
//	FAKE_APPSERVER_STALE_RESUME — id that thread/resume refuses with -32004
//	FAKE_APPSERVER_ARGV_LOG     — append every received method to this file
//	                              (legacy compat — one method per line)
//	FAKE_APPSERVER_RPC_LOG      — append "<method>\t<params-json>\n" per
//	                              received frame (preferred for bats assertions)
//	FAKE_APPSERVER_EVENT_SCRIPT — file with one JSON {method, params} per line,
//	                              emitted during turn/start before edit events
//	FAKE_APPSERVER_TURN_DELAY_MS — sleep this many ms before emitting
//	                              turn/completed (so cancel/idle-out can race a
//	                              genuinely in-flight turn)
//	FAKE_APPSERVER_OVERSIZED_FRAME — emit a single notification line larger than
//	                              16 MiB during turn/start (a huge JSON string
//	                              field) to exercise oversized-frame handling
//	FAKE_APPSERVER_SERVER_REQUEST — issue a server→client request (a JSON-RPC
//	                              request carrying an id the client is expected
//	                              to answer) mid-turn; the value names the method.
//	                              Unset: no server request is issued.
//	FAKE_APPSERVER_IGNORE_CLOSE_AND_TERM — keep the process alive after stdin EOF
//	                              and ignore SIGTERM, so shutdown tests exercise
//	                              the SIGKILL rung.
//	FAKE_APPSERVER_BWRAP_BROKEN — make command/exec report a bubblewrap failure
//	                              (exit 1 + "bwrap: setting up uid map: Permission
//	                              denied" on stderr) so the broker's sandbox
//	                              preflight sees an unusable host sandbox. Unset:
//	                              command/exec reports a healthy exit 0.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(argv) < 2 {
		fmt.Fprintln(stderr, "fake-codex: missing subcommand")
		return 64
	}
	switch argv[1] {
	case "--version":
		v := getenv("FAKE_CODEX_VERSION")
		if v == "" {
			v = "0.130.0"
		}
		fmt.Fprintf(stdout, "codex-cli %s\n", v)
		return 0
	case "app-server":
		// Record the full argv so a test can assert the broker's -c overrides
		// (e.g. the MCP-disable flags) reached the spawned app-server.
		if rec := getenv("FAKE_APPSERVER_RECORD_ARGV"); rec != "" {
			_ = os.WriteFile(rec, []byte(strings.Join(argv, "\n")), 0o644)
		}
		return runAppServer(stdin, stdout, stderr, getenv)
	case "mcp":
		// Emulate `codex mcp list --json` so the broker's MCP-disable enumeration
		// has servers to read. FAKE_APPSERVER_MCP_SERVERS is a comma-separated list
		// of server names (empty → no servers).
		if len(argv) >= 3 && argv[2] == "list" {
			fmt.Fprint(stdout, mcpListJSON(splitCSVList(getenv("FAKE_APPSERVER_MCP_SERVERS"))))
			return 0
		}
		return 0
	default:
		fmt.Fprintf(stderr, "fake-codex: unknown subcommand %q\n", argv[1])
		return 64
	}
}

// mcpListJSON renders the subset of `codex mcp list --json` the broker reads: an
// array of {name, enabled} objects.
func mcpListJSON(names []string) string {
	type srv struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	list := make([]srv, 0, len(names))
	for _, n := range names {
		list = append(list, srv{Name: n, Enabled: true})
	}
	b, _ := json.Marshal(list)
	return string(b)
}

func splitCSVList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runAppServer(stdin io.Reader, stdout, _ io.Writer, getenv func(string) string) int {
	if getenv("FAKE_APPSERVER_IGNORE_CLOSE_AND_TERM") != "" {
		terms := make(chan os.Signal, 1)
		signal.Notify(terms, syscall.SIGTERM)
		defer signal.Stop(terms)
		go func() {
			for range terms {
			}
		}()
	}
	state := &fakeState{
		getenv:  getenv,
		stdout:  stdout,
		writeMu: &sync.Mutex{},
	}
	reader := bufio.NewReader(stdin)
	for {
		line, err := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			if err != nil {
				break
			}
			continue
		}
		if shouldExit := state.handle(line); shouldExit {
			return 0
		}
		if err != nil {
			break
		}
	}
	if getenv("FAKE_APPSERVER_IGNORE_CLOSE_AND_TERM") != "" {
		select {}
	}
	return 0
}

type fakeState struct {
	getenv   func(string) string
	stdout   io.Writer
	writeMu  *sync.Mutex
	threadID string
	threadMu sync.Mutex
}

func (s *fakeState) handle(line []byte) bool {
	var msg struct {
		ID     *json.Number    `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return false
	}
	s.logRPC(msg.Method, msg.Params)

	if msg.ID == nil {
		// notification — only `initialized` is meaningful (and a no-op).
		return false
	}
	id := msg.ID

	switch msg.Method {
	case "initialize":
		s.respond(id, map[string]any{
			"serverInfo":   map[string]string{"name": "fake-appserver", "version": s.version()},
			"capabilities": map[string]any{},
		})
	case "thread/start":
		return s.handleThreadStart(id, msg.Params)
	case "thread/resume":
		return s.handleThreadResume(id, msg.Params)
	case "turn/start":
		return s.handleTurnStart(id)
	case "command/exec":
		s.handleCommandExec(id)
	case "shutdown":
		s.respond(id, map[string]any{})
		return true
	default:
		s.respondError(id, -32601, "Method not found: "+msg.Method)
	}
	return false
}

func (s *fakeState) version() string {
	if v := s.getenv("FAKE_CODEX_VERSION"); v != "" {
		return v
	}
	return "0.130.0"
}

func (s *fakeState) sessionID() string {
	if v := s.getenv("FAKE_APPSERVER_SESSION"); v != "" {
		return v
	}
	return "fake-thread-0001"
}

func (s *fakeState) setThreadID(id string) {
	s.threadMu.Lock()
	s.threadID = id
	s.threadMu.Unlock()
}

func (s *fakeState) getThreadID() string {
	s.threadMu.Lock()
	defer s.threadMu.Unlock()
	return s.threadID
}

func (s *fakeState) handleThreadStart(id *json.Number, params json.RawMessage) bool {
	// FAKE_APPSERVER_RECORD_MODEL=<path>: record the model from thread/start so a
	// test can assert the CODEX_MODEL pin was threaded through to codex (empty
	// file when no model was sent).
	if rec := s.getenv("FAKE_APPSERVER_RECORD_MODEL"); rec != "" {
		var p struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(params, &p)
		_ = os.WriteFile(rec, []byte(p.Model), 0o644)
	}
	// FAKE_APPSERVER_RECORD_CWD=<path>: record the cwd from thread/start so a
	// test can assert the dispatch threaded the invocation working directory (a
	// go.work module subdir, say) through to codex rather than collapsing it to
	// the repo root (empty file when no cwd was sent).
	if rec := s.getenv("FAKE_APPSERVER_RECORD_CWD"); rec != "" {
		var p struct {
			CWD string `json:"cwd"`
		}
		_ = json.Unmarshal(params, &p)
		_ = os.WriteFile(rec, []byte(p.CWD), 0o644)
	}
	tid := s.sessionID()
	s.setThreadID(tid)
	s.respond(id, map[string]any{
		"thread": map[string]any{"id": tid, "cwd": ".", "status": "running"},
	})
	s.notify("thread/started", map[string]any{
		"thread": map[string]any{"id": tid, "cwd": ".", "status": "running"},
	})
	return false
}

func (s *fakeState) handleThreadResume(id *json.Number, params json.RawMessage) bool {
	var p struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(params, &p)
	stale := s.getenv("FAKE_APPSERVER_STALE_RESUME")
	if stale != "" && p.ThreadID == stale {
		s.respondError(id, -32004, "thread not found")
		return false
	}
	tid := p.ThreadID
	if tid == "" {
		tid = s.sessionID()
	}
	s.setThreadID(tid)
	s.respond(id, map[string]any{
		"thread": map[string]any{"id": tid, "cwd": ".", "status": "running"},
	})
	s.notify("thread/started", map[string]any{
		"thread": map[string]any{"id": tid, "cwd": ".", "status": "running"},
	})
	return false
}

func (s *fakeState) handleTurnStart(id *json.Number) bool {
	turnID := "fake-turn-1"
	s.respond(id, map[string]any{
		"turn": map[string]any{"id": turnID, "status": "pending"},
	})

	tid := s.getThreadID()
	s.notify("turn/started", map[string]any{
		"threadId": tid,
		"turn":     map[string]any{"id": turnID, "status": "pending"},
	})

	if reqMethod := s.getenv("FAKE_APPSERVER_SERVER_REQUEST"); reqMethod != "" {
		s.serverRequest(reqMethod, map[string]any{
			"threadId": tid,
			"turnId":   turnID,
		})
	}

	if s.getenv("FAKE_APPSERVER_OVERSIZED_FRAME") != "" {
		s.notify("item/started", map[string]any{
			"threadId": tid,
			"item": map[string]any{
				"id":   "item-oversized",
				"type": "agentMessage",
				"text": strings.Repeat("x", 16*1024*1024+1),
			},
		})
	}

	if scriptPath := s.getenv("FAKE_APPSERVER_EVENT_SCRIPT"); scriptPath != "" {
		if data, err := os.ReadFile(scriptPath); err == nil {
			for _, line := range bytes.Split(data, []byte("\n")) {
				line = bytes.TrimSpace(line)
				if len(line) == 0 {
					continue
				}
				var ev struct {
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.Unmarshal(line, &ev); err == nil {
					s.writeFrame(map[string]any{
						"jsonrpc": "2.0",
						"method":  ev.Method,
						"params":  ev.Params,
					})
				}
			}
		}
	}

	for _, edit := range strings.Split(s.getenv("FAKE_APPSERVER_EDIT"), "\n") {
		edit = strings.TrimSpace(edit)
		if edit == "" {
			continue
		}
		path, content, ok := strings.Cut(edit, ":")
		if !ok {
			continue
		}
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte(content), 0o644)
		itemID := "item-" + path
		s.notify("item/started", map[string]any{
			"threadId": tid,
			"item":     map[string]any{"id": itemID, "type": "fileChange", "path": path},
		})
		s.notify("item/completed", map[string]any{
			"threadId": tid,
			"item":     map[string]any{"id": itemID, "type": "fileChange", "path": path},
		})
	}

	if ms := s.getenv("FAKE_APPSERVER_TURN_DELAY_MS"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n > 0 {
			time.Sleep(time.Duration(n) * time.Millisecond)
		}
	}

	status := s.getenv("FAKE_APPSERVER_EXIT")
	if status == "" {
		status = "completed"
	}
	s.notify("turn/completed", map[string]any{
		"threadId": tid,
		"turn":     map[string]any{"id": turnID, "status": status, "durationMs": 42},
	})
	return false
}

// handleCommandExec answers the command/exec RPC the broker's sandbox preflight
// uses. By default it reports a healthy run (exit 0, empty output). With
// FAKE_APPSERVER_BWRAP_BROKEN set it mimics a host whose bubblewrap sandbox
// cannot start: a nonzero exit carrying the real bwrap uid-map diagnostic on
// stderr, exactly what codex surfaces on a host that restricts unprivileged user
// namespaces.
func (s *fakeState) handleCommandExec(id *json.Number) {
	if s.getenv("FAKE_APPSERVER_BWRAP_BROKEN") != "" {
		s.respond(id, map[string]any{
			"exitCode": 1,
			"stdout":   "",
			"stderr":   "bwrap: setting up uid map: Permission denied\n",
		})
		return
	}
	s.respond(id, map[string]any{"exitCode": 0, "stdout": "", "stderr": ""})
}

func (s *fakeState) respond(id *json.Number, result any) {
	s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (s *fakeState) respondError(id *json.Number, code int, msg string) {
	s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
}

func (s *fakeState) notify(method string, params any) {
	s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// serverRequest issues a server→client JSON-RPC request: a frame with both a
// method and an id, which the client is expected to respond to (e.g. approval
// or elicitation). The id is a string so it cannot collide with the numeric
// ids the client assigns to its own requests.
func (s *fakeState) serverRequest(method string, params any) {
	s.writeFrame(map[string]any{
		"jsonrpc": "2.0",
		"id":      "fake-server-req-1",
		"method":  method,
		"params":  params,
	})
}

func (s *fakeState) writeFrame(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.stdout.Write(b)
	_, _ = s.stdout.Write([]byte("\n"))
}

func (s *fakeState) logRPC(method string, params json.RawMessage) {
	if path := s.getenv("FAKE_APPSERVER_ARGV_LOG"); path != "" {
		_ = appendFile(path, []byte(method+"\n"))
	}
	if path := s.getenv("FAKE_APPSERVER_RPC_LOG"); path != "" {
		_ = appendFile(path, []byte(method+"\t"+string(params)+"\n"))
	}
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
