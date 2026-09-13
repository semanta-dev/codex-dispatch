package result

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMarshalGolden(t *testing.T) {
	r := Result{
		ExitCode:        0,
		SessionID:       "abc123",
		FilesChanged:    []string{"app/main.py"},
		LinesAdded:      12,
		LinesRemoved:    3,
		StdoutPath:      "/abs/path/to/stdout.log",
		DiffPath:        "/abs/path/to/diff.patch",
		FellBackToFresh: false,
	}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "result.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Marshal mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestEmptySessionIDIsEmptyStringNotNull(t *testing.T) {
	r := Result{FilesChanged: []string{}}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(got, []byte(`"session_id":""`)) {
		t.Fatalf("empty session_id must marshal as \"\", got: %s", got)
	}
}

func TestEmptyFilesChangedIsEmptyArrayNotNull(t *testing.T) {
	dir := t.TempDir()
	r := Result{StdoutPath: "/x", DiffPath: "/y"} // FilesChanged is nil
	if err := Write(dir, r); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	if !bytes.Contains(got, []byte(`"files_changed":[]`)) {
		t.Fatalf("files_changed must marshal as []: %s", got)
	}
}

// TestErrorMessageOmittedWhenEmpty confirms successful runs still produce
// a byte-stable result.json (error_message is omitempty).
func TestErrorMessageOmittedWhenEmpty(t *testing.T) {
	r := Result{ExitCode: 0, FilesChanged: []string{}, SessionID: "x"}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(got, []byte("error_message")) {
		t.Fatalf("error_message must be omitted on success: %s", got)
	}
}

// TestFilesChangedOutsideSeedOmittedWhenEmpty confirms an in-scope / seed-less
// run keeps the original byte shape (the field is omitempty, never normalized
// to []), so existing consumers and the golden shape are unaffected.
func TestFilesChangedOutsideSeedOmittedWhenEmpty(t *testing.T) {
	r := Result{ExitCode: 0, FilesChanged: []string{"a"}, SessionID: "x"}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(got, []byte("files_changed_outside_seed")) {
		t.Fatalf("files_changed_outside_seed must be omitted when empty: %s", got)
	}
}

// TestFilesChangedOutsideSeedPresentWhenSet confirms the scope signal surfaces.
func TestFilesChangedOutsideSeedPresentWhenSet(t *testing.T) {
	r := Result{
		ExitCode:                0,
		SessionID:               "x",
		FilesChanged:            []string{"app/main.py", "extra/scope_creep.py"},
		FilesChangedOutsideSeed: []string{"extra/scope_creep.py"},
	}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(got, []byte(`"files_changed_outside_seed":["extra/scope_creep.py"]`)) {
		t.Fatalf("files_changed_outside_seed missing when set: %s", got)
	}
}

// TestErrorMessagePresentWhenSet confirms failure runs surface the message.
func TestErrorMessagePresentWhenSet(t *testing.T) {
	r := Result{
		ExitCode:     2,
		SessionID:    "x",
		FilesChanged: []string{},
		ErrorMessage: "turn failed: rate limit",
	}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(got, []byte(`"error_message":"turn failed: rate limit"`)) {
		t.Fatalf("error_message missing on failure path: %s", got)
	}
}

// TestWriteCreatesFile verifies Write creates a private result artifact.
func TestWriteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	r := Result{ExitCode: 0, FilesChanged: []string{"a"}}
	if err := Write(dir, r); err != nil {
		t.Fatalf("Write: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatalf("result.json not written: %v", err)
	}
	if got := fi.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("result.json permissions = %o, want 0600", got)
	}
}

func TestWriteDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "result.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := Write(dir, Result{ExitCode: 0}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("symlink target changed: %q (%v)", got, err)
	}
}
