package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarshalNotificationFramesLeavesNormalTaskEventUnchanged(t *testing.T) {
	params := taskEventParams{
		TaskID:  "task-1",
		Seq:     7,
		Type:    "item/started",
		Payload: json.RawMessage(`{"text":"small"}`),
	}
	want, err := MarshalNotification("task.event", params)
	if err != nil {
		t.Fatalf("MarshalNotification: %v", err)
	}
	got, err := MarshalNotificationFrames("task.event", params)
	if err != nil {
		t.Fatalf("MarshalNotificationFrames: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("frames = %d, want 1", len(got))
	}
	if !bytes.Equal(got[0], want) {
		t.Fatalf("normal task.event frame changed:\n got=%s\nwant=%s", got[0], want)
	}
}

func TestMarshalNotificationFramesChunksOversizedTaskEvent(t *testing.T) {
	payload, err := json.Marshal(map[string]string{
		"text": strings.Repeat("x", MaxMessageSize+1024),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	params := taskEventParams{
		TaskID:  "task-big",
		Seq:     3,
		Type:    "item/started",
		Payload: payload,
	}
	frames, err := MarshalNotificationFrames("task.event", params)
	if err != nil {
		t.Fatalf("MarshalNotificationFrames: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("frames = %d, want chunked", len(frames))
	}
	for i, frame := range frames {
		if len(frame) > MaxMessageSize {
			t.Fatalf("frame %d len = %d, want <= %d", i, len(frame), MaxMessageSize)
		}
		var msg struct {
			Method string               `json:"method"`
			Params taskEventChunkParams `json:"params"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(frame), &msg); err != nil {
			t.Fatalf("decode frame %d: %v", i, err)
		}
		if msg.Method != taskEventChunkMethod {
			t.Fatalf("frame %d method = %q, want %q", i, msg.Method, taskEventChunkMethod)
		}
		if msg.Params.ChunkIndex != i {
			t.Fatalf("frame %d chunk_index = %d", i, msg.Params.ChunkIndex)
		}
	}
}

func TestTaskEventReassemblerRejectsGapsAndDuplicates(t *testing.T) {
	gap := &taskEventReassembler{}
	if _, err := gap.addChunk(taskEventChunkParams{
		TaskID:      "task-1",
		Seq:         1,
		Type:        "item/started",
		ChunkID:     "task-1:1",
		ChunkIndex:  1,
		TotalChunks: 2,
		Payload:     []byte(`"b"`),
	}); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("gap error = %v, want gap rejection", err)
	}

	dup := &taskEventReassembler{}
	first := taskEventChunkParams{
		TaskID:      "task-1",
		Seq:         1,
		Type:        "item/started",
		ChunkID:     "task-1:1",
		ChunkIndex:  0,
		TotalChunks: 2,
		Payload:     []byte(`{"a":`),
	}
	if ev, err := dup.addChunk(first); err != nil || ev != nil {
		t.Fatalf("first chunk ev=%v err=%v, want pending nil", ev, err)
	}
	if _, err := dup.addChunk(first); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v, want duplicate rejection", err)
	}
}

func TestDispatchRunReassemblesOversizedTaskEventPayload(t *testing.T) {
	repoDir := t.TempDir()
	logPath := filepath.Join(repoDir, "stdout.log")
	const threadID = "thread-oversized"
	paramsRaw, err := json.Marshal(struct {
		ThreadID string `json:"threadId"`
		Text     string `json:"text"`
	}{
		ThreadID: threadID,
		Text:     strings.Repeat("x", MaxMessageSize+1024),
	})
	if err != nil {
		t.Fatalf("marshal oversized params: %v", err)
	}
	scriptLine, err := json.Marshal(struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{
		Method: "item/started",
		Params: paramsRaw,
	})
	if err != nil {
		t.Fatalf("marshal script line: %v", err)
	}
	scriptPath := filepath.Join(repoDir, "events.ndjson")
	if err := os.WriteFile(scriptPath, append(scriptLine, '\n'), 0o644); err != nil {
		t.Fatalf("write event script: %v", err)
	}
	setupFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":          "0.130.0",
		"FAKE_APPSERVER_SESSION":      threadID,
		"FAKE_APPSERVER_EVENT_SCRIPT": scriptPath,
	})

	table := NewTable(8, 2048)
	state := &BrokerState{Table: table, CWD: repoDir}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := NewServer("")
	srv.HandleFunc("dispatch.run", HandleDispatchRun(state))
	go srv.serveConn(context.Background(), c1)
	client := NewClientFromConn(c2)

	var gotPayload json.RawMessage
	result, err := client.DispatchRun(context.Background(), DispatchRunParams{
		SessionID: "s",
		Mode:      "fresh",
		Prompt:    "t",
		Sandbox:   "workspace-write",
		LogPath:   logPath,
	}, func(ev DispatchEvent) {
		if ev.Type == "item/started" {
			gotPayload = append(gotPayload[:0], ev.Payload...)
		}
	})
	if err != nil {
		t.Fatalf("DispatchRun: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", result.ExitCode)
	}
	if !bytes.Equal(gotPayload, paramsRaw) {
		t.Fatalf("reassembled payload mismatch: got len=%d want len=%d equal=%v", len(gotPayload), len(paramsRaw), bytes.Equal(gotPayload, paramsRaw))
	}
}
