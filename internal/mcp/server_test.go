package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
)

func TestServerInitialize(t *testing.T) {
	s := &Server{
		tools:  defaultTools(),
		stdout: bufio.NewWriter(io.Discard),
	}
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"test","version":"0.1.0"}}}`
	resp := s.handleRequest(context.Background(), []byte(req))
	if resp == nil {
		t.Fatal("no response to initialize")
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result, got %T", resp["result"])
	}

	pv, ok := result["protocolVersion"].(string)
	if !ok || pv != "2024-11-05" {
		t.Fatalf("protocolVersion = %v, want 2024-11-05", result["protocolVersion"])
	}
	if got := string(resp["id"].(json.RawMessage)); got != "1" {
		t.Fatalf("response id = %q, want 1", got)
	}
}

func TestServerToolsList(t *testing.T) {
	s := &Server{
		tools:  defaultTools(),
		stdout: bufio.NewWriter(io.Discard),
	}
	req := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	resp := s.handleRequest(context.Background(), []byte(req))
	if resp == nil {
		t.Fatal("no response to tools/list")
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatal("missing result")
	}

	tools, ok := result["tools"].([]Tool)
	if !ok || len(tools) < 8 {
		t.Fatalf("tools = %d, want >= 8", len(tools))
	}
}

func TestServerUnknownMethod(t *testing.T) {
	s := &Server{
		tools:  defaultTools(),
		stdout: bufio.NewWriter(io.Discard),
	}
	req := `{"jsonrpc":"2.0","id":3,"method":"nonexistent"}`
	resp := s.handleRequest(context.Background(), []byte(req))
	if resp == nil {
		t.Fatal("expected error response")
	}

	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatal("missing error field")
	}
	if errObj["code"] != -32601 {
		t.Fatalf("error code = %v, want -32601", errObj["code"])
	}
}
