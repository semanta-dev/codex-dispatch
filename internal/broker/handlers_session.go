package broker

import (
	"context"
	"encoding/json"
	"fmt"
)

// HandleSessionRegister returns a Handler for session.register.
func HandleSessionRegister(state *BrokerState) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var params struct {
			SessionID string `json:"session_id"`
			Cwd       string `json:"cwd"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if params.SessionID == "" {
			return nil, fmt.Errorf("session_id required")
		}
		state.Table.RegisterSession(params.SessionID)
		return map[string]any{"ok": true}, nil
	}
}

// HandleSessionDeregister returns a Handler for session.deregister.
func HandleSessionDeregister(state *BrokerState) Handler {
	return func(_ context.Context, raw json.RawMessage) (any, error) {
		var params struct {
			SessionID    string `json:"session_id"`
			CancelQueued bool   `json:"cancel_queued"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if params.SessionID == "" {
			return nil, fmt.Errorf("session_id required")
		}
		ids := state.Table.DeregisterSession(params.SessionID, params.CancelQueued)
		if ids == nil {
			ids = []string{}
		}
		return map[string]any{"cancelled_task_ids": ids}, nil
	}
}
