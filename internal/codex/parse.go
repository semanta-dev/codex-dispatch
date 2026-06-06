package codex

import (
	"bufio"
	"encoding/json"
	"os"
)

// ParseSessionID scans logPath line-by-line for JSON events, keeping the
// last "thread_id" (preferred) or "session_id" seen. Non-JSON lines are
// ignored. A missing file is treated as empty (returns "" without error).
func ParseSessionID(logPath string) (string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	var last string
	scanner := bufio.NewScanner(f)
	// codex JSONL events can be larger than the 64 KiB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var ev struct {
			ThreadID  string `json:"thread_id"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		switch {
		case ev.ThreadID != "":
			last = ev.ThreadID
		case ev.SessionID != "":
			last = ev.SessionID
		}
	}
	return last, scanner.Err()
}
