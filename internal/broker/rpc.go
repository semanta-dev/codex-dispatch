// Package broker implements the codex-dispatch broker: a per-cwd localhost
// HTTP JSON-RPC 2.0 server that fronts one supervised codex app-server child and
// owns the task table that `internal/codex` and the CLI's --status/--cancel/--list
// paths query.
package broker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// MaxMessageSize caps a single JSON-RPC line at 4 MiB (matches the codex
// event-stream scanner buffer from Phase 1).
const MaxMessageSize = 4 * 1024 * 1024

// ProtocolVersion is baked into every successful response so clients can
// refuse a mismatched major version at the wire boundary.
const ProtocolVersion = "1"

// Sentinel errors. ParseRequest wraps these with %w; callers use errors.Is.
var (
	ErrParseError      = errors.New("parse error")
	ErrInvalidRequest  = errors.New("invalid request")
	ErrMessageTooLarge = errors.New("message too large")
)

// Request is a parsed JSON-RPC 2.0 request. ID is nil for notifications.
type Request struct {
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params,omitempty"`
	ID     *json.RawMessage `json:"id,omitempty"`
}

// ParseRequest parses one JSON-RPC line. Returns ErrParseError on malformed
// JSON or ErrInvalidRequest when required fields are missing or wrong.
func ParseRequest(line []byte) (*Request, error) {
	var env struct {
		JSONRPC string           `json:"jsonrpc"`
		Method  string           `json:"method"`
		Params  json.RawMessage  `json:"params"`
		ID      *json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseError, err)
	}
	if env.JSONRPC != "2.0" {
		return nil, fmt.Errorf("%w: jsonrpc must be \"2.0\" (got %q)", ErrInvalidRequest, env.JSONRPC)
	}
	if env.Method == "" {
		return nil, fmt.Errorf("%w: method required", ErrInvalidRequest)
	}
	return &Request{Method: env.Method, Params: env.Params, ID: env.ID}, nil
}

// MarshalSuccess encodes a successful response with the protocol-version
// stamp. Result must JSON-marshal cleanly.
func MarshalSuccess(id *json.RawMessage, result any) ([]byte, error) {
	type resp struct {
		JSONRPC         string          `json:"jsonrpc"`
		ProtocolVersion string          `json:"_protocol_version"`
		Result          any             `json:"result"`
		ID              json.RawMessage `json:"id"`
	}
	var raw json.RawMessage
	if id != nil {
		raw = *id
	} else {
		raw = json.RawMessage("null")
	}
	r := resp{JSONRPC: "2.0", ProtocolVersion: ProtocolVersion, Result: result, ID: raw}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	return b, nil
}

// MarshalError encodes an error response.
func MarshalError(id *json.RawMessage, code int, message string, data any) ([]byte, error) {
	type errBody struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	}
	type resp struct {
		JSONRPC string          `json:"jsonrpc"`
		Error   errBody         `json:"error"`
		ID      json.RawMessage `json:"id"`
	}
	var raw json.RawMessage
	if id != nil {
		raw = *id
	} else {
		raw = json.RawMessage("null")
	}
	r := resp{JSONRPC: "2.0", Error: errBody{Code: code, Message: message, Data: data}, ID: raw}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	return b, nil
}

// MarshalNotification encodes a server-pushed notification (no id, no
// protocol-version stamp — only on responses).
func MarshalNotification(method string, params any) ([]byte, error) {
	type notif struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}
	b, err := json.Marshal(notif{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	return b, nil
}

// ReadLine reads one newline-delimited JSON message from r, rejecting any
// line that exceeds MaxMessageSize.
func ReadLine(r *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		chunk, err := r.ReadSlice('\n')
		buf.Write(chunk)
		if buf.Len() > MaxMessageSize {
			return nil, fmt.Errorf("%w: line exceeded %d bytes", ErrMessageTooLarge, MaxMessageSize)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Strip trailing newline + an optional preceding CR so CRLF-terminated
		// lines from non-Unix clients parse cleanly.
		out := buf.Bytes()
		out = out[:len(out)-1] // drop '\n'
		if len(out) > 0 && out[len(out)-1] == '\r' {
			out = out[:len(out)-1]
		}
		return out, nil
	}
}
