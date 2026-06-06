package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerListenAndPing(t *testing.T) {
	dir := t.TempDir()
	addrPath := filepath.Join(dir, "broker.addr")

	srv := NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)
	srv.HandleFunc("broker.ping", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"version": "1.0.0"}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	addr := waitAddrFile(t, addrPath)
	resp, err := httpPost(addr, []byte(`{"jsonrpc":"2.0","method":"broker.ping","params":{},"id":1}`+"\n"))
	if err != nil {
		t.Fatalf("httpPost: %v", err)
	}
	line := string(resp)
	if !strings.Contains(string(line), `"version":"1.0.0"`) {
		t.Fatalf("response missing version: %s", line)
	}
	if !strings.Contains(string(line), `"_protocol_version":"1"`) {
		t.Fatalf("response missing _protocol_version: %s", line)
	}
}

func TestServerMethodNotFound(t *testing.T) {
	dir := t.TempDir()
	addrPath := filepath.Join(dir, "broker.addr")
	srv := NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx)

	addr := waitAddrFile(t, addrPath)
	line, err := httpPost(addr, []byte(`{"jsonrpc":"2.0","method":"no.such.method","id":1}`+"\n"))
	if err != nil {
		t.Fatalf("httpPost: %v", err)
	}
	if !strings.Contains(string(line), `"code":-32601`) {
		t.Fatalf("expected -32601, got: %s", line)
	}
}

func TestServerWritesAndRemovesAddrFile(t *testing.T) {
	dir := t.TempDir()
	addrPath := filepath.Join(dir, "broker.addr")

	srv := NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	if addr := waitAddrFile(t, addrPath); addr == "" {
		t.Fatalf("empty addr")
	}
	cancel()
	<-done
	if _, err := os.Stat(addrPath); !os.IsNotExist(err) {
		t.Fatalf("addr file should be removed on shutdown, stat err=%v", err)
	}
}

func TestServerShutsDownOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	addrPath := filepath.Join(dir, "broker.addr")
	srv := NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	_ = waitAddrFile(t, addrPath)

	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Serve returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve did not return after context cancel")
	}
}

func waitAddrFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("addr file %s did not appear", path)
	return ""
}

func httpPost(addr string, body []byte) ([]byte, error) {
	resp, err := http.Post("http://"+addr+"/rpc", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// TestServerNotificationProducesNoResponse verifies a JSON-RPC notification (a
// well-formed request with no id) gets NO response, even though its handler runs
// for side effects. Against the OLD dispatch (which always wrote a response) the
// notification would get a spurious reply, so the request that follows it would
// read the notification's response instead of its own.
func TestServerNotificationProducesNoResponse(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	var handlerCalls int32
	srv := NewServer("")
	srv.HandleFunc("note.method", func(_ context.Context, _ json.RawMessage) (any, error) {
		atomic.AddInt32(&handlerCalls, 1)
		return map[string]any{"ok": true}, nil
	})
	srv.HandleFunc("broker.ping", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"version": "1.0.0"}, nil
	})
	go srv.serveConn(context.Background(), c1)

	reader := bufio.NewReader(c2)

	// 1. Send a notification (no id). The server must write nothing back.
	if _, err := c2.Write([]byte(`{"jsonrpc":"2.0","method":"note.method","params":{}}` + "\n")); err != nil {
		t.Fatalf("write notification: %v", err)
	}

	// 2. Send a real request (with id). The FIRST line we read back must be this
	//    request's response — proving the notification produced no line ahead of
	//    it. If a notification response had been written, this read would return
	//    that instead (an id we did not send).
	if _, err := c2.Write([]byte(`{"jsonrpc":"2.0","method":"broker.ping","params":{},"id":7}` + "\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}

	line, err := ReadLine(reader)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	var env struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("decode: %v (line=%s)", err, line)
	}
	if string(env.ID) != "7" {
		t.Fatalf("first response id = %s, want 7 (notification leaked a response)", env.ID)
	}
	if env.Error != nil {
		t.Fatalf("ping returned error: %v", env.Error)
	}
	// The notification's handler should have run exactly once (side effects),
	// even though it produced no wire response.
	if got := atomic.LoadInt32(&handlerCalls); got != 1 {
		t.Fatalf("notification handler calls = %d, want 1", got)
	}
}

// TestServerUnknownNotificationProducesNoResponse verifies that even an UNKNOWN
// method delivered as a notification gets no response (no -32601).
func TestServerUnknownNotificationProducesNoResponse(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	srv := NewServer("")
	srv.HandleFunc("broker.ping", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"version": "1.0.0"}, nil
	})
	go srv.serveConn(context.Background(), c1)

	reader := bufio.NewReader(c2)
	// Unknown-method notification (no id) → must produce no response.
	if _, err := c2.Write([]byte(`{"jsonrpc":"2.0","method":"no.such.method"}` + "\n")); err != nil {
		t.Fatalf("write notification: %v", err)
	}
	// Follow with a real ping; its response must be first on the wire.
	if _, err := c2.Write([]byte(`{"jsonrpc":"2.0","method":"broker.ping","id":9}` + "\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	line, err := ReadLine(reader)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	var env struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(env.ID) != "9" {
		t.Fatalf("first response id = %s, want 9 (unknown notification leaked -32601)", env.ID)
	}
}

// TestServerParseErrorReturnsNullIDResponse verifies a malformed line yields an
// error response with id:null (a parse error always gets a response so the
// client can surface it rather than hang).
func TestServerParseErrorReturnsNullIDResponse(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	go srv.serveConn(context.Background(), c1)

	reader := bufio.NewReader(c2)
	if _, err := c2.Write([]byte("{not json}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := ReadLine(reader)
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	var env struct {
		ID    json.RawMessage `json:"id"`
		Error *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("decode: %v (line=%s)", err, line)
	}
	if strings.TrimSpace(string(env.ID)) != "null" {
		t.Fatalf("parse-error id = %s, want null", env.ID)
	}
	if env.Error == nil || env.Error.Code != -32700 {
		t.Fatalf("parse-error envelope = %+v, want code -32700", env.Error)
	}
}
