package codex

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLog(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdout.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return p
}

func TestParseSessionIDPrefersThreadID(t *testing.T) {
	log := writeLog(t, `{"type":"thread.started","thread_id":"thread-abc"}
{"type":"turn.completed"}
`)
	id, err := ParseSessionID(log)
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	if id != "thread-abc" {
		t.Fatalf("id = %q, want thread-abc", id)
	}
}

func TestParseSessionIDFallsBackToSessionID(t *testing.T) {
	log := writeLog(t, `{"type":"x","session_id":"sess-1"}
`)
	id, err := ParseSessionID(log)
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	if id != "sess-1" {
		t.Fatalf("id = %q, want sess-1", id)
	}
}

func TestParseSessionIDMultipleEventsPickLast(t *testing.T) {
	log := writeLog(t, `{"type":"a","thread_id":"first"}
{"type":"b","thread_id":"second"}
{"type":"c","thread_id":"third"}
`)
	id, err := ParseSessionID(log)
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	if id != "third" {
		t.Fatalf("id = %q, want third (last)", id)
	}
}

func TestParseSessionIDEmptyLogReturnsEmpty(t *testing.T) {
	log := writeLog(t, "")
	id, err := ParseSessionID(log)
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	if id != "" {
		t.Fatalf("id = %q, want empty", id)
	}
}

func TestParseSessionIDIgnoresNonJSONLines(t *testing.T) {
	log := writeLog(t, `Error: blah
{"type":"thread.started","thread_id":"good"}
random text
`)
	id, err := ParseSessionID(log)
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	if id != "good" {
		t.Fatalf("id = %q, want good", id)
	}
}

func TestParseSessionIDMissingFileReturnsEmpty(t *testing.T) {
	id, err := ParseSessionID("/no/such/file")
	if err != nil {
		t.Fatalf("ParseSessionID should be lenient: %v", err)
	}
	if id != "" {
		t.Fatalf("id = %q, want empty", id)
	}
}
