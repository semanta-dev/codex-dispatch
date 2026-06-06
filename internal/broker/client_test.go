package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientPingRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	srv := NewServer("") // we plug the pipe directly
	srv.HandleFunc("broker.ping", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"version": "1.0.0", "task_count": 0}, nil
	})
	go srv.serveConn(context.Background(), c1)

	client := NewClientFromConn(c2)
	resp, err := client.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if resp.Version != "1.0.0" {
		t.Fatalf("Version = %q, want 1.0.0", resp.Version)
	}
}

func TestClientMapsMethodNotFoundToTypedErr(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	go srv.serveConn(context.Background(), c1)

	client := NewClientFromConn(c2)
	// Call a method that has no handler registered.
	_, err := client.rawCall(context.Background(), "no.such.method", nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %T, want *RPCError", err)
	}
	if rpcErr.Code != -32601 {
		t.Fatalf("code = %d, want -32601", rpcErr.Code)
	}
}

func TestClientProtocolVersionMismatch(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Build a fake server that responds with version 2.
	go func() {
		reader := bufio.NewReader(c1)
		_, _ = ReadLine(reader)
		_, _ = c1.Write([]byte(`{"jsonrpc":"2.0","_protocol_version":"2","result":{"version":"2.0.0"},"id":1}` + "\n"))
	}()

	client := NewClientFromConn(c2)
	_, err := client.Ping(context.Background())
	if err == nil {
		t.Fatalf("expected ProtocolVersionMismatch")
	}
	if !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("err = %v, want protocol version message", err)
	}
}

func TestClientShutdown(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	srv := NewServer("")
	srv.HandleFunc("broker.shutdown", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	go srv.serveConn(context.Background(), c1)

	client := NewClientFromConn(c2)
	if err := client.Shutdown(context.Background(), false); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestClientTaskListRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	table := NewTable(8, 2048)
	_, _ = table.Start("sess-x", TaskParams{Mode: "fresh"})
	state := &BrokerState{Table: table}

	srv := NewServer("")
	srv.HandleFunc("task.list", HandleTaskList(state))
	go srv.serveConn(context.Background(), c1)

	client := NewClientFromConn(c2)
	entries, err := client.TaskList(context.Background(), "sess-x")
	if err != nil {
		t.Fatalf("TaskList: %v", err)
	}
	if len(entries) != 1 || entries[0].State != "queued" {
		t.Fatalf("entries = %+v, want one queued entry", entries)
	}
}

func TestClientTaskCancelMapsTaskNotFound(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	state := &BrokerState{Table: NewTable(8, 2048)}
	srv := NewServer("")
	srv.HandleFunc("task.cancel", HandleTaskCancel(state))
	go srv.serveConn(context.Background(), c1)

	client := NewClientFromConn(c2)
	err := client.TaskCancel(context.Background(), "no-such")
	if err == nil {
		t.Fatalf("expected error")
	}
	rpcErr := ToRPCError(err)
	if rpcErr == nil || rpcErr.Code != -32001 {
		t.Fatalf("err = %v, want -32001", err)
	}
}

// TestClientSurfacesNullIDParseError verifies the client returns a server's
// id:null parse/invalid-request error instead of skipping it and hanging. The
// server stamps id:null on a parse error (the client's id can't be echoed), and
// `null` unmarshals into the client's int64 id as 0, so the OLD read loop's
// `gotID != id` check skipped it and the call blocked until the 200ms deadline.
// The fix surfaces the RPCError promptly.
func TestClientSurfacesNullIDParseError(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Fake server: read the client's request line, reply with an id:null error.
	go func() {
		reader := bufio.NewReader(c1)
		if _, err := ReadLine(reader); err != nil {
			return
		}
		_, _ = c1.Write([]byte(`{"jsonrpc":"2.0","_protocol_version":"1","error":{"code":-32700,"message":"parse error: bad json"},"id":null}` + "\n"))
	}()

	client := NewClientFromConn(c2)
	// A deadline well above prompt-return but used only as a safety net: a
	// correct client returns the RPCError immediately, far under this.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.rawCall(ctx, "broker.ping", map[string]any{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected the server's parse error to surface")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %T (%v), want *RPCError surfaced from id:null response", err, err)
	}
	if rpcErr.Code != -32700 {
		t.Fatalf("code = %d, want -32700", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "parse error") {
		t.Fatalf("message = %q, want parse-error text", rpcErr.Message)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("rawCall took %v to surface id:null error; it was masked/hung", elapsed)
	}
}

// TestClientNotificationWithoutErrorIsSkipped verifies the client still skips a
// genuine notification (no id, no error) on the rawCall path rather than
// treating it as the response — the null-id surfacing only triggers on errors.
func TestClientNotificationWithoutErrorIsSkipped(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() {
		reader := bufio.NewReader(c1)
		if _, err := ReadLine(reader); err != nil {
			return
		}
		// A server-pushed notification (no id, no error) first...
		_, _ = c1.Write([]byte(`{"jsonrpc":"2.0","method":"task.event","params":{"x":1}}` + "\n"))
		// ...then the real matched response.
		_, _ = c1.Write([]byte(`{"jsonrpc":"2.0","_protocol_version":"1","result":{"version":"1.0.0"},"id":1}` + "\n"))
	}()

	client := NewClientFromConn(c2)
	resp, err := client.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if resp.Version != "1.0.0" {
		t.Fatalf("Version = %q, want 1.0.0 (notification should have been skipped)", resp.Version)
	}
}

// TestClientHTTPSurfacesNullIDParseError covers the HTTP transport path
// (rawCallHTTP) of the id:null surfacing fix: a Dial-ed client whose server
// responds with an id:null error must surface that RPCError rather than loop
// past it. Against the OLD HTTP read loop the `null` id unmarshalled to 0,
// `gotID != id` skipped the line, and the call hung until "broker closed
// response". The conn-path test covers NewClientFromConn; this covers Dial.
func TestClientHTTPSurfacesNullIDParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","_protocol_version":"1","error":{"code":-32700,"message":"parse error: bad json"},"id":null}` + "\n"))
	}))
	defer srv.Close()

	// Dial expects a host:port and builds http://<addr>/rpc; strip the scheme.
	addr := strings.TrimPrefix(srv.URL, "http://")
	client, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err = client.rawCall(ctx, "broker.ping", map[string]any{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected the server's id:null parse error to surface over HTTP")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %T (%v), want *RPCError surfaced from id:null HTTP response", err, err)
	}
	if rpcErr.Code != -32700 {
		t.Fatalf("code = %d, want -32700", rpcErr.Code)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("rawCallHTTP took %v to surface id:null error; it was masked/hung", elapsed)
	}
}

// TestClientHTTPDispatchRunSurfacesNullIDParseError covers the streaming HTTP
// path (dispatchRunHTTP): a task.event notification is delivered to the
// callback, then an id:null error must be surfaced (not skipped). This proves
// the isNullID guard applies on the dispatch.run HTTP read loop too.
func TestClientHTTPDispatchRunSurfacesNullIDParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		// One genuine notification (must be delivered to onEvent, not surfaced)...
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","method":"task.event","params":{"task_id":"t1","seq":1,"type":"x","payload":{}}}` + "\n"))
		// ...then an id:null parse error that must be surfaced.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","_protocol_version":"1","error":{"code":-32600,"message":"invalid request"},"id":null}` + "\n"))
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	client, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	var events int
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = client.DispatchRun(ctx, DispatchRunParams{Mode: "fresh"}, func(DispatchEvent) {
		events++
	})
	if err == nil {
		t.Fatalf("expected the server's id:null error to surface from DispatchRun over HTTP")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %T (%v), want *RPCError surfaced from id:null HTTP response", err, err)
	}
	if rpcErr.Code != -32600 {
		t.Fatalf("code = %d, want -32600", rpcErr.Code)
	}
	if events != 1 {
		t.Fatalf("onEvent calls = %d, want 1 (the genuine notification before the error)", events)
	}
}

func TestClientRawCallRespectsContextCancel(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Drain c1 so the client's Write unblocks, but never send a response.
	// This leaves rawCall blocked on ReadLine.
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := c1.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	client := NewClientFromConn(c2)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 50ms.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := client.rawCall(ctx, "broker.ping", map[string]any{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("rawCall took %v after cancel; deadline not propagated", elapsed)
	}
}
