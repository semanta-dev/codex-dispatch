// Package result owns the result.json wire format. Field order in this
// struct IS the on-disk order — do not reorder without bumping the
// distribution version and reviewing every consumer (subagents, /codex
// runbook, tests).
package result

import (
	"encoding/json"

	"github.com/semanta-dev/codex-dispatch/internal/artifact"
)

// Result is the spec §5.1 frozen shape. Field tags and order are normative.
// ErrorMessage was added in v0.3.0 (real-protocol Phase 2) and is omitempty
// so successful runs preserve the original byte shape.
type Result struct {
	ExitCode        int      `json:"exit_code"`
	SessionID       string   `json:"session_id"`
	FilesChanged    []string `json:"files_changed"`
	LinesAdded      int      `json:"lines_added"`
	LinesRemoved    int      `json:"lines_removed"`
	StdoutPath      string   `json:"stdout_path"`
	DiffPath        string   `json:"diff_path"`
	FellBackToFresh bool     `json:"fell_back_to_fresh"`
	ErrorMessage    string   `json:"error_message,omitempty"`
	// FilesChangedOutsideSeed lists changed files NOT covered by the CODEX_FILES
	// seed (the advisory relevant-files hint). It is a scope-creep signal for the
	// reviewer, not a hard constraint: empty/omitted when no seed was provided or
	// every change falls within it. Added v0.3.3 — append-only and omitempty, so
	// the byte shape of seed-less / in-scope runs is unchanged (same discipline as
	// ErrorMessage). Never normalized to [] (unlike FilesChanged) so "no signal"
	// stays absent rather than an empty array.
	FilesChangedOutsideSeed []string `json:"files_changed_outside_seed,omitempty"`
}

// Write marshals r to <dir>/result.json. Normalizes nil slice to empty array
// so the JSON shape is stable.
func Write(dir string, r Result) error {
	b, err := Encode(r)
	if err != nil {
		return err
	}
	return artifact.WriteAtomic(dir, "result.json", b, 0600)
}

// Encode preserves the public field order and normalized empty file list.
func Encode(r Result) ([]byte, error) {
	if r.FilesChanged == nil {
		r.FilesChanged = []string{}
	}
	return json.Marshal(r)
}
