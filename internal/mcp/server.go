package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/semanta-dev/codex-dispatch/internal/broker"
)

// Tool is one MCP tool definition.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Server is the MCP stdio server. It wraps a broker client and exposes tools.
type Server struct {
	client *broker.Client
	tools  []Tool
	mu     sync.Mutex
	stdout *bufio.Writer
}

// NewServer creates an MCP server that proxies to the broker at addr.
func NewServer(addr string) (*Server, error) {
	client, err := broker.Dial(addr)
	if err != nil {
		return nil, fmt.Errorf("broker dial: %w", err)
	}
	s := &Server{
		client: client,
		stdout: bufio.NewWriter(os.Stdout),
		tools:  defaultTools(),
	}
	return s, nil
}

// Close releases the broker client.
func (s *Server) Close() error { return s.client.Close() }

// Serve reads MCP JSON-RPC messages from stdin and writes responses to stdout.
func (s *Server) Serve(ctx context.Context) error {
	reader := bufio.NewReader(os.Stdin)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = trimRightNewline(line)
		if line == "" {
			continue
		}

		response := s.handleRequest(ctx, []byte(line))
		if response != nil {
			data, jsonErr := json.Marshal(response)
			if jsonErr == nil {
				s.mu.Lock()
				s.stdout.Write(data)
				s.stdout.WriteByte('\n')
				s.stdout.Flush()
				s.mu.Unlock()
			}
		}
	}
}

// handleRequest dispatches one MCP JSON-RPC request and returns the response (or nil for notifications).
func (s *Server) handleRequest(ctx context.Context, raw []byte) map[string]any {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse(nil, -32700, err.Error())
	}

	switch req.Method {
	case "initialize":
		return resultResponse(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "codex-dispatch", "version": "0.1.0"},
		})
	case "tools/list":
		return resultResponse(req.ID, map[string]any{"tools": s.tools})
	case "tools/call":
		return s.handleToolCall(ctx, req.ID, req.Params)
	default:
		if req.ID == nil {
			return nil // notification
		}
		return errorResponse(req.ID, -32601, "method not found: "+req.Method)
	}
}

func trimRightNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func resultResponse(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func errorResponse(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func defaultTools() []Tool {
	return []Tool{
		{Name: "ping", Description: "Check broker health and load", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "task_start", Description: "Start a detached dispatch task", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"}, "sandbox": map[string]any{"type": "string", "default": "workspace-write"}, "claim_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}},
		{Name: "task_status", Description: "Get status of a detached task", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"task_id": map[string]any{"type": "string"}}, "required": []string{"task_id"}}},
		{Name: "task_cancel", Description: "Cancel a running or queued task", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"task_id": map[string]any{"type": "string"}}, "required": []string{"task_id"}}},
		{Name: "task_list", Description: "List all tasks known to the broker", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "claim_acquire", Description: "Acquire a claim on file paths for a named agent", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}, "files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "intent": map[string]any{"type": "string"}, "ttl_ms": map[string]any{"type": "integer"}}, "required": []string{"owner", "files", "intent"}}},
		{Name: "claim_release", Description: "Release a claim", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"claim_id": map[string]any{"type": "string"}, "owner": map[string]any{"type": "string"}, "fencing_token": map[string]any{"type": "integer"}}, "required": []string{"claim_id", "owner", "fencing_token"}}},
		{Name: "claim_list", Description: "List all active claims", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}}}},
	}
}
