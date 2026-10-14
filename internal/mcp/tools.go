package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// handleToolCall dispatches a tools/call request to the appropriate broker RPC.
func (s *Server) handleToolCall(ctx context.Context, id json.RawMessage, params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid tool call params: "+err.Error())
	}

	args := map[string]any{}
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return errorResponse(id, -32602, "invalid tool arguments: "+err.Error())
		}
	}

	result, err := s.dispatchTool(ctx, p.Name, args)
	if err != nil {
		// MCP tool calls report errors as content with isError: true.
		return resultResponse(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		})
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return resultResponse(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "marshal tool result: " + err.Error()}},
			"isError": true,
		})
	}
	return resultResponse(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(encoded)}},
	})
}

// dispatchTool routes a tool name to the corresponding broker RPC.
func (s *Server) dispatchTool(ctx context.Context, name string, args map[string]any) (any, error) {
	switch name {
	case "ping":
		return s.client.Ping(ctx)

	case "task_status":
		taskID, ok := args["task_id"].(string)
		if !ok {
			return nil, fmt.Errorf("task_id is required")
		}
		return s.client.TaskStatusCall(ctx, taskID)

	case "task_cancel":
		taskID, ok := args["task_id"].(string)
		if !ok {
			return nil, fmt.Errorf("task_id is required")
		}
		err := s.client.TaskCancel(ctx, taskID)
		return map[string]any{"ok": err == nil, "error": errStr(err)}, nil

	case "task_list":
		return s.client.TaskList(ctx, "")

	case "claim_acquire":
		return s.client.ClaimAcquire(ctx, args)

	case "claim_release":
		return s.client.ClaimRelease(ctx, args)

	case "claim_list":
		return s.client.ClaimList(ctx, args)

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
