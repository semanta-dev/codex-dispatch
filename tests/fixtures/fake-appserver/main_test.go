package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	rc := run([]string{"fake-appserver", "--version"}, strings.NewReader(""), &out, io.Discard, func(string) string { return "" })
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(out.String(), "codex-cli 0.130.0") {
		t.Fatalf("version: %q", out.String())
	}
}

func TestInitializeAndShutdown(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","method":"initialized","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, func(string) string { return "" })
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	s := out.String()
	if !strings.Contains(s, `"id":1`) || !strings.Contains(s, `"serverInfo"`) {
		t.Fatalf("initialize response missing: %s", s)
	}
	if !strings.Contains(s, `"id":2`) {
		t.Fatalf("shutdown response missing: %s", s)
	}
}

func TestThreadStartHappy(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_SESSION" {
			return "my-session-id"
		}
		return ""
	}
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	s := out.String()
	if !strings.Contains(s, `"id":"my-session-id"`) {
		t.Fatalf("session id not in response: %s", s)
	}
	if !strings.Contains(s, `"method":"thread/started"`) {
		t.Fatalf("thread/started notification missing: %s", s)
	}
}

func TestThreadResumeStale(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/resume","params":{"threadId":"stale-id"}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_STALE_RESUME" {
			return "stale-id"
		}
		return ""
	}
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(out.String(), `"code":-32004`) {
		t.Fatalf("expected -32004: %s", out.String())
	}
}

func TestTurnStartEmitsEdits(t *testing.T) {
	tmp := t.TempDir()
	editPath := filepath.Join(tmp, "hello.txt")
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"t","input":[]}}` + "\n" +
			`{"jsonrpc":"2.0","id":4,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_EDIT" {
			return editPath + ":Hello"
		}
		return ""
	}
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	s := out.String()
	if !strings.Contains(s, `"method":"turn/completed"`) {
		t.Fatalf("turn/completed missing: %s", s)
	}
	if !strings.Contains(s, `"method":"item/completed"`) {
		t.Fatalf("item/completed missing: %s", s)
	}
	got, err := os.ReadFile(editPath)
	if err != nil {
		t.Fatalf("read edit file: %v", err)
	}
	if string(got) != "Hello" {
		t.Fatalf("edit file content: %q", got)
	}
}

func TestTurnStartCustomExit(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":4,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_EXIT" {
			return "failed"
		}
		return ""
	}
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(out.String(), `"status":"failed"`) {
		t.Fatalf("expected failed status: %s", out.String())
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"some/bogus","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, func(string) string { return "" })
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(out.String(), `"code":-32601`) {
		t.Fatalf("expected -32601: %s", out.String())
	}
}

func TestRPCLog(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "rpc.log")
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_RPC_LOG" {
			return logPath
		}
		return ""
	}
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "initialize\t") {
		t.Fatalf("rpc log: %s", data)
	}
	if !strings.Contains(string(data), "shutdown\t") {
		t.Fatalf("rpc log: %s", data)
	}
}

func TestArgvLogBackcompat(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "argv.log")
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_ARGV_LOG" {
			return logPath
		}
		return ""
	}
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"initialize", "thread/start", "shutdown"}
	if len(lines) != len(want) {
		t.Fatalf("argv log lines: got %d want %d (content: %q)", len(lines), len(want), data)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("argv log[%d]: got %q want %q", i, lines[i], w)
		}
	}
}

func TestTurnDelayDefersCompletion(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":4,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_TURN_DELAY_MS" {
			return "120"
		}
		return ""
	}
	start := time.Now()
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	elapsed := time.Since(start)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("turn completed too early: elapsed=%s want >=120ms", elapsed)
	}
	if !strings.Contains(out.String(), `"method":"turn/completed"`) {
		t.Fatalf("turn/completed missing: %s", out.String())
	}
}

func TestTurnDelayUnsetIsPrompt(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":4,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	start := time.Now()
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, func(string) string { return "" })
	elapsed := time.Since(start)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("unset delay should be prompt: elapsed=%s", elapsed)
	}
	if !strings.Contains(out.String(), `"method":"turn/completed"`) {
		t.Fatalf("turn/completed missing: %s", out.String())
	}
}

func TestOversizedFrameEmitted(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":4,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_OVERSIZED_FRAME" {
			return "1"
		}
		return ""
	}
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	// Find the single output line that exceeds 16 MiB.
	var oversized []byte
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if len(line) > 16*1024*1024 {
			oversized = line
			break
		}
	}
	if oversized == nil {
		t.Fatalf("no notification line exceeded 16 MiB")
	}
	// It must still be a well-formed JSON-RPC notification frame.
	var frame struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(oversized, &frame); err != nil {
		t.Fatalf("oversized line is not valid JSON: %v", err)
	}
	if frame.Method != "item/started" {
		t.Fatalf("oversized frame method: got %q", frame.Method)
	}
	if !strings.Contains(out.String(), `"method":"turn/completed"`) {
		t.Fatalf("turn/completed missing after oversized frame")
	}
}

func TestServerRequestIssuedMidTurn(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":4,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	getenv := func(k string) string {
		if k == "FAKE_APPSERVER_SERVER_REQUEST" {
			return "applyPatchApproval"
		}
		return ""
	}
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, getenv)
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	// The server→client request must be a frame carrying BOTH an id and a method.
	var found bool
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var frame struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			continue
		}
		if frame.ID != nil && frame.Method == "applyPatchApproval" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("server→client request (id+method) not emitted: %s", out.String())
	}
	if !strings.Contains(out.String(), `"method":"turn/completed"`) {
		t.Fatalf("turn/completed missing after server request")
	}
}

func TestNewKnobsUnsetUnchanged(t *testing.T) {
	// Regression: with none of the new knobs set, turn/start behaves exactly as
	// before — a single turn/completed and no oversized line or server request.
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":4,"method":"shutdown","params":{}}` + "\n",
	)
	var out bytes.Buffer
	rc := run([]string{"fake-appserver", "app-server"}, in, &out, io.Discard, func(string) string { return "" })
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	s := out.String()
	if !strings.Contains(s, `"method":"turn/completed"`) {
		t.Fatalf("turn/completed missing: %s", s)
	}
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if len(line) > 16*1024*1024 {
			t.Fatalf("unexpected oversized line with knobs unset")
		}
	}
	if strings.Contains(s, "applyPatchApproval") {
		t.Fatalf("unexpected server request with knobs unset: %s", s)
	}
}
