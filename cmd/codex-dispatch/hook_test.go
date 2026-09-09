package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHookSessionStartContinuesAlways(t *testing.T) {
	in := bytes.NewReader([]byte(`{"session_id":"s1","cwd":"/no/such/repo","hook_event_name":"SessionStart"}`))
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "hook", "session-start"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"continue":true`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	_ = in
}

func TestHookStopBrokerUnreachableContinues(t *testing.T) {
	t.Setenv("CODEX_DISPATCH_BROKER_SOCKET", "/tmp/does-not-exist.sock")
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "hook", "stop"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	var resp struct {
		Continue bool `json:"continue"`
	}
	_ = json.Unmarshal(stdout.Bytes(), &resp)
	if !resp.Continue {
		t.Fatalf("expected continue=true; stdout = %q", stdout.String())
	}
}

func TestHookOptOutEnvVar(t *testing.T) {
	t.Setenv("CODEX_DISPATCH_DISABLE_HOOKS", "1")
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "hook", "stop"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if !strings.Contains(stdout.String(), `"continue":true`) {
		t.Fatalf("opt-out should produce continue=true")
	}
}

func TestHookOptOutStopOnly(t *testing.T) {
	t.Setenv("CODEX_DISPATCH_DISABLE_HOOK_STOP", "1")
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "hook", "stop"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if !strings.Contains(stdout.String(), `"continue":true`) {
		t.Fatalf("stderr=%q stdout=%q", stderr.String(), stdout.String())
	}
}

func TestHookUnknownEventReturns64(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "hook", "bogus-event"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64", rc)
	}
	_ = stdout
	_ = stderr
}

func TestHookMissingEventReturns64(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "hook"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64", rc)
	}
}

// fakeLineBroker supplies authenticated HTTP discovery to hook tests.
func fakeLineBroker(t *testing.T, handler func(string) (any, bool)) string {
	t.Helper()
	token := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		result, ok := handler(req.Method)
		if !ok {
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	raw, _ := json.Marshal(map[string]any{"version": 2, "address": strings.TrimPrefix(srv.URL, "http://"), "token": token})
	return string(raw)
}

// TestHookStopBlockEmitsSchemaValidDecision asserts the Stop "block" decision
// has EXACTLY decision+reason (plus the advisory systemMessage) and OMITS
// continue/stopReason. The old code emitted continue:false + decision:block +
// stopReason simultaneously, which is not a valid Stop-hook decision; this test
// fails against that behavior.
func TestHookStopBlockEmitsSchemaValidDecision(t *testing.T) {
	sock := fakeLineBroker(t, func(method string) (any, bool) {
		if method == "task.list" {
			return map[string]any{
				"tasks": []map[string]any{
					{"task_id": "t1", "state": "running", "started_at": "now"},
				},
			}, true
		}
		return map[string]any{}, true
	})
	t.Setenv("CODEX_DISPATCH_BROKER_ADDR", sock)

	in := strings.NewReader(`{"session_id":"s1","cwd":"/x","hook_event_name":"Stop"}`)
	var stdout, stderr bytes.Buffer
	rc := runHook([]string{"stop"}, in, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %s", rc, stderr.String())
	}

	// Decode into a permissive map and assert exact field presence/absence.
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (out=%q)", err, stdout.String())
	}
	if got["decision"] != "block" {
		t.Fatalf("decision = %v, want block (out=%q)", got["decision"], stdout.String())
	}
	reason, ok := got["reason"].(string)
	if !ok || !strings.Contains(reason, "codex tasks still active") {
		t.Fatalf("reason = %v, want a non-empty active-tasks reason (out=%q)", got["reason"], stdout.String())
	}
	if _, present := got["continue"]; present {
		t.Fatalf("block decision must OMIT continue; out=%q", stdout.String())
	}
	if _, present := got["stopReason"]; present {
		t.Fatalf("block decision must OMIT stopReason (pair reason with decision); out=%q", stdout.String())
	}
}

// TestHookStopHonorsStopHookActive verifies that when Claude re-invokes the
// Stop hook with stop_hook_active=true (because a prior Stop already blocked),
// the hook does NOT block again — it continues — preventing an infinite loop.
// The broker would report a running task, so a block would otherwise occur.
func TestHookStopHonorsStopHookActive(t *testing.T) {
	sock := fakeLineBroker(t, func(method string) (any, bool) {
		if method == "task.list" {
			return map[string]any{
				"tasks": []map[string]any{
					{"task_id": "t1", "state": "running", "started_at": "now"},
				},
			}, true
		}
		return map[string]any{}, true
	})
	t.Setenv("CODEX_DISPATCH_BROKER_ADDR", sock)

	in := strings.NewReader(`{"session_id":"s1","cwd":"/x","hook_event_name":"Stop","stop_hook_active":true}`)
	var stdout, stderr bytes.Buffer
	rc := runHook([]string{"stop"}, in, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %s", rc, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (out=%q)", err, stdout.String())
	}
	if got["continue"] != true {
		t.Fatalf("stop_hook_active must yield continue:true, got %q", stdout.String())
	}
	if _, present := got["decision"]; present {
		t.Fatalf("stop_hook_active must NOT re-block; out=%q", stdout.String())
	}
}

// TestHookStopTimesOutOnHungBroker verifies a hung-but-reachable broker port
// does not stall the Stop hook: the per-call RPC timeout fires, the hook
// abandons the call and continues. Without the timeout the hook would block
// until the harness's ~60s deadline. The test asserts the hook returns well
// under that bound.
func TestHookStopTimesOutOnHungBroker(t *testing.T) {
	// A TCP listener that accepts connections but never speaks HTTP — the
	// production (HTTP) broker-addr path. CODEX_DISPATCH_BROKER_ADDR routes the
	// hook to broker.Dial(addr), whose RPC honors the per-call ctx deadline.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var connMu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		connMu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		connMu.Unlock()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without ever responding.
			connMu.Lock()
			conns = append(conns, conn)
			connMu.Unlock()
		}
	}()
	record, _ := json.Marshal(map[string]any{"version": 2, "address": ln.Addr().String(), "token": strings.Repeat("a", 64)})
	t.Setenv("CODEX_DISPATCH_BROKER_ADDR", string(record))

	in := strings.NewReader(`{"session_id":"s1","cwd":"/x","hook_event_name":"Stop"}`)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- runHook([]string{"stop"}, in, &stdout, &stderr) }()

	select {
	case rc := <-done:
		connMu.Lock()
		accepted := len(conns)
		connMu.Unlock()
		if accepted == 0 {
			t.Fatal("hook did not reach the authenticated HTTP transport")
		}
		if rc != 0 {
			t.Fatalf("rc = %d, stderr = %s", rc, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"continue":true`) {
			t.Fatalf("hung broker should fall back to continue:true, got %q", stdout.String())
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("hook took %s; the per-call timeout did not bound the hung broker", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("hook did not return; per-call RPC timeout is not enforced")
	}
}
