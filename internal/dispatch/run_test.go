package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/broker"
)

// chdirTo changes the test process's cwd for the duration of the test. The
// codex subprocess inherits cwd, so tests that exercise FAKE_APPSERVER_EDIT
// should chdir into the temp repo to keep stray artifacts out of the source tree.
func chdirTo(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// installFakeAppserver builds tests/fixtures/fake-appserver as `codex` on
// PATH so the broker's appserver.Dispatch can talk to it. Mirrors the helper
// in internal/broker/handlers_dispatch_test.go.
func installFakeAppserver(t *testing.T, env map[string]string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-appserver unavailable on Windows")
	}
	wd, _ := os.Getwd()
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(root, "tests/fixtures/fake-appserver")); err == nil {
				break
			}
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("repo root not found from %s", wd)
		}
		root = parent
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join(root, "tests/fixtures/fake-appserver")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build fake-appserver: %v: %s", err, out)
	}
	_ = os.Chmod(bin, fs.FileMode(0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// startInProcessBroker spins up a broker.Server and writes
// <repoDir>/.codex-dispatch/broker.addr so codex.Fresh / codex.Resume can
// dial it without needing to spawn an out-of-process broker binary.
// (os.Executable() in `go test` points at the test binary, which doesn't
// have a `broker` subcommand.)
func startInProcessBroker(t *testing.T, repoDir string) {
	t.Helper()
	dir := filepath.Join(repoDir, ".codex-dispatch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir broker dir: %v", err)
	}
	addrPath := filepath.Join(dir, "broker.addr")

	srv := broker.NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)
	table := broker.NewTable(8, 2048)
	state := &broker.BrokerState{Table: table}

	srv.HandleFunc("dispatch.run", broker.HandleDispatchRun(state))
	srv.HandleFunc("task.start", broker.HandleTaskStart(state))
	srv.HandleFunc("broker.ping", broker.HandleBrokerPing(state))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(addrPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", addrPath)
}

func TestRunHappyPathWritesResultJSON(t *testing.T) {
	repo := setupGitRepo(t)
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "sess-123",
		"FAKE_APPSERVER_EDIT":    filepath.Join(repo, "hello.txt") + ":Hello world",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run"),
	}

	var stdout, stderr strings.Builder
	rc, err := Run(env, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v (stderr=%s)", err, stderr.String())
	}
	if rc != 0 {
		t.Fatalf("exit = %d, want 0", rc)
	}
	out := strings.TrimRight(stdout.String(), "\n")
	last := out
	if i := strings.LastIndex(out, "\n"); i >= 0 {
		last = out[i+1:]
	}
	if last != filepath.Join(repo, "run") {
		t.Fatalf("last stdout line = %q, want %q", last, env.ResultDir)
	}
	raw, err := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	for _, k := range []string{"exit_code", "session_id", "files_changed", "lines_added", "lines_removed", "stdout_path", "diff_path", "fell_back_to_fresh"} {
		if _, ok := r[k]; !ok {
			t.Fatalf("result.json missing %q: %s", k, raw)
		}
	}
}

func TestRunDefaultResultDirUnderRepo(t *testing.T) {
	repo := setupGitRepo(t)
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "s",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{WorkDir: repo, Task: "t", Acceptance: "a", Sandbox: "workspace-write"}
	if _, err := Run(env, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".codex-dispatch", "runs")); err != nil {
		t.Fatalf("default runs dir not created: %v", err)
	}
}

func TestRunFailedTurnPreservedInResult(t *testing.T) {
	repo := setupGitRepo(t)
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_EXIT":    "failed",
		"FAKE_APPSERVER_SESSION": "s-fail",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run-fail"),
	}
	rc, _ := Run(env, io.Discard, io.Discard)
	// Codex Turn.Status=="failed" maps to exit_code=2 inside result.json;
	// the dispatch process itself exits 0 (the failure is data, not a crash).
	if rc != 0 {
		t.Fatalf("dispatch exit = %d, want 0 (failed status should live in result.json)", rc)
	}
	raw, _ := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	var r map[string]any
	_ = json.Unmarshal(raw, &r)
	if int(r["exit_code"].(float64)) != 2 {
		t.Fatalf("result.exit_code = %v, want 2 (failed status)", r["exit_code"])
	}
}

func TestRunCompletedTurnWithoutMeaningfulEditsFailsResult(t *testing.T) {
	repo := setupGitRepo(t)
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "s-noop",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run-noop"),
	}
	rc, err := Run(env, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rc != 0 {
		t.Fatalf("dispatch exit = %d, want 0 (failure should live in result.json)", rc)
	}
	raw, err := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal result.json: %v", err)
	}
	if int(r["exit_code"].(float64)) != 4 {
		t.Fatalf("result.exit_code = %v, want 4 (no meaningful edits)", r["exit_code"])
	}
	if r["error_message"] != "codex completed without meaningful repository edits" {
		t.Fatalf("error_message = %v", r["error_message"])
	}
	if got := r["files_changed"].([]any); len(got) != 0 {
		t.Fatalf("files_changed = %v, want empty", got)
	}
}

func TestRunIgnoresRuntimeArtifactOnlyEdits(t *testing.T) {
	repo := setupGitRepo(t)
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "s-artifact",
		"FAKE_APPSERVER_EDIT":    filepath.Join(repo, ".codex-dispatch", "worker.log") + ":artifact",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run-artifact"),
	}
	if _, err := Run(env, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal result.json: %v", err)
	}
	if int(r["exit_code"].(float64)) != 4 {
		t.Fatalf("result.exit_code = %v, want 4 (runtime artifact-only edit)", r["exit_code"])
	}
	if got := r["files_changed"].([]any); len(got) != 0 {
		t.Fatalf("files_changed = %v, want runtime artifacts filtered", got)
	}
}

func TestRunStaleResumeFallsBack(t *testing.T) {
	repo := setupGitRepo(t)
	stale := "00000000-0000-0000-0000-000000000000"
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":          "0.130.0",
		"FAKE_APPSERVER_STALE_RESUME": stale,
		"FAKE_APPSERVER_EDIT":         filepath.Join(repo, "from-fresh.txt") + ":retry",
		"FAKE_APPSERVER_SESSION":      "fresh-thread-id",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		SessionID:  stale,
		ResultDir:  filepath.Join(repo, "run-stale"),
	}
	if _, err := Run(env, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	var r map[string]any
	_ = json.Unmarshal(raw, &r)
	logBytes, _ := os.ReadFile(filepath.Join(env.ResultDir, "stdout.log"))
	if !strings.Contains(string(logBytes), "fell back to fresh dispatch") {
		t.Fatalf("stdout.log missing fallback marker: %s", logBytes)
	}
	// session_id should be the fresh-retry thread id (broker resolves the
	// fallback transparently before returning to internal/codex.Fresh).
	if r["session_id"] != "fresh-thread-id" {
		t.Fatalf("session_id = %v, want fresh-thread-id", r["session_id"])
	}
}

// TestRunAttributesEditToAlreadyDirtyFile checks that when codex edits a file
// that was already dirty before the run, the edit is attributed (exit_code 0,
// file in files_changed) rather than dropped as pre-existing WIP.
// TestRunPinsModelToCodex asserts the full chain: Env.Model (CODEX_MODEL) flows
// through dispatch -> broker -> app-server thread/start, where the fake codex
// records the model it received.
func TestRunPinsModelToCodex(t *testing.T) {
	repo := setupGitRepo(t)
	rec := filepath.Join(t.TempDir(), "model.txt")
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":          "0.130.0",
		"FAKE_APPSERVER_SESSION":      "s-model",
		"FAKE_APPSERVER_RECORD_MODEL": rec,
		"FAKE_APPSERVER_EDIT":         filepath.Join(repo, "out.txt") + ":done",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		Model:      "gpt-5.5-test",
		ResultDir:  filepath.Join(repo, "run-model"),
	}
	if _, err := Run(env, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("read recorded model: %v", err)
	}
	if string(got) != "gpt-5.5-test" {
		t.Fatalf("codex received model %q, want gpt-5.5-test", got)
	}
}

func TestRunAttributesEditToAlreadyDirtyFile(t *testing.T) {
	repo := setupGitRepo(t)
	// README.md is committed as "x\n"; dirty it before the run.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\nwip\n"), 0o644); err != nil {
		t.Fatalf("dirty README: %v", err)
	}
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "s-dirty-edit",
		"FAKE_APPSERVER_EDIT":    filepath.Join(repo, "README.md") + ":x\nwip\ncodex\n",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run-dirty-edit"),
	}
	if _, err := Run(env, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if int(r["exit_code"].(float64)) != 0 {
		t.Fatalf("exit_code = %v, want 0 (edit to already-dirty file is meaningful)", r["exit_code"])
	}
	var found bool
	for _, f := range r["files_changed"].([]any) {
		if f == "README.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("files_changed = %v, want to contain README.md", r["files_changed"])
	}
}

// TestRunNoOpOnAlreadyDirtyFileYieldsExit4 checks that a pre-existing dirty
// file that codex does NOT touch is excluded, so a run with no other edits is
// classified exit_code=4.
func TestRunNoOpOnAlreadyDirtyFileYieldsExit4(t *testing.T) {
	repo := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\nwip\n"), 0o644); err != nil {
		t.Fatalf("dirty README: %v", err)
	}
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":     "0.130.0",
		"FAKE_APPSERVER_SESSION": "s-dirty-noop",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run-dirty-noop"),
	}
	if _, err := Run(env, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	if int(r["exit_code"].(float64)) != 4 {
		t.Fatalf("exit_code = %v, want 4 (pre-existing WIP only, no codex edit)", r["exit_code"])
	}
	if got := r["files_changed"].([]any); len(got) != 0 {
		t.Fatalf("files_changed = %v, want empty", got)
	}
}

func TestRunValidationErrorReturnsTypedCode(t *testing.T) {
	env := Env{WorkDir: t.TempDir()}
	rc, err := Run(env, io.Discard, io.Discard)
	if rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if err == nil {
		t.Fatalf("err should be non-nil for typed code mapping")
	}
}

// TestRunCanceledContextReturnsPromptlyWithStatus checks that a context
// cancelled before codex completes makes the dispatch return promptly with a
// clear status recorded in result.json, rather than surfacing a bare broker
// error. The cancelled ctx short-circuits codex.Fresh before any broker dial,
// so no fake appserver/broker is needed.
func TestRunCanceledContextReturnsPromptlyWithStatus(t *testing.T) {
	requireCodex(t)
	repo := setupGitRepo(t)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run-canceled"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before codex runs

	done := make(chan struct{})
	var rc int
	var err error
	go func() {
		rc, err = runWithContext(ctx, env, io.Discard, io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("runWithContext did not return promptly on cancellation")
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (cancellation is data in result.json)", rc)
	}
	raw, rerr := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	if rerr != nil {
		t.Fatalf("read result.json: %v", rerr)
	}
	var r map[string]any
	if uerr := json.Unmarshal(raw, &r); uerr != nil {
		t.Fatalf("unmarshal: %v: %s", uerr, raw)
	}
	if int(r["exit_code"].(float64)) == 0 {
		t.Fatalf("exit_code = %v, want non-zero for a canceled dispatch", r["exit_code"])
	}
	msg, _ := r["error_message"].(string)
	if !strings.Contains(msg, "cancel") {
		t.Fatalf("error_message = %q, want a clear cancellation status", msg)
	}
	// No diff is captured on the cancel path, so diff_path must be empty rather
	// than point at a diff.patch that was never written (consumers read it).
	if dp, _ := r["diff_path"].(string); dp != "" {
		t.Fatalf("diff_path = %q, want empty on cancellation (no diff.patch written)", dp)
	}
	if _, serr := os.Stat(filepath.Join(env.ResultDir, "diff.patch")); serr == nil {
		t.Fatalf("diff.patch exists on cancel path; result.diff_path must not advertise a non-existent file")
	}
}

// TestRunTimeoutFromEnvReturnsPromptlyWithStatus checks the
// CODEX_DISPATCH_TIMEOUT_MS path: a timeout much smaller than an in-flight
// turn's delay aborts the dispatch with a timeout-specific status in
// result.json. The fake appserver holds the turn open via TURN_DELAY so the
// deadline reliably fires while DispatchRun is blocked reading, exercising the
// real cancel-during-codex path (not just the upfront ctx.Err() shortcut).
func TestRunTimeoutFromEnvReturnsPromptlyWithStatus(t *testing.T) {
	repo := setupGitRepo(t)
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":           "0.130.0",
		"FAKE_APPSERVER_SESSION":       "s-timeout",
		"FAKE_APPSERVER_TURN_DELAY_MS": "5000",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	t.Setenv("CODEX_DISPATCH_TIMEOUT_MS", "200")
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run-timeout"),
	}

	done := make(chan struct{})
	start := time.Now()
	var rc int
	var err error
	go func() {
		rc, err = Run(env, io.Discard, io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return promptly on timeout")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Run took %s, want prompt return well before the 5s turn delay", elapsed)
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (timeout is data in result.json)", rc)
	}
	raw, rerr := os.ReadFile(filepath.Join(env.ResultDir, "result.json"))
	if rerr != nil {
		t.Fatalf("read result.json: %v", rerr)
	}
	var r map[string]any
	if uerr := json.Unmarshal(raw, &r); uerr != nil {
		t.Fatalf("unmarshal: %v: %s", uerr, raw)
	}
	msg, _ := r["error_message"].(string)
	if !strings.Contains(msg, "timed out") {
		t.Fatalf("error_message = %q, want a clear timeout status", msg)
	}
}

// TestRunBaselineCaptureFailureIsSurfaced checks that a failure to write the
// diff baseline aborts the run with a surfaced error instead of proceeding to
// codex (which would silently mis-attribute files_changed). The failure is
// forced by pre-creating baseline-pre-files.txt as a directory so the baseline
// write cannot succeed.
func TestRunBaselineCaptureFailureIsSurfaced(t *testing.T) {
	requireCodex(t)
	repo := setupGitRepo(t)
	chdirTo(t, repo)
	resultDir := filepath.Join(repo, "run-baseline-fail")
	if err := os.MkdirAll(filepath.Join(resultDir, "baseline-pre-files.txt"), 0o755); err != nil {
		t.Fatalf("seed obstructing dir: %v", err)
	}
	env := Env{
		WorkDir:    repo,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  resultDir,
	}
	var stderr strings.Builder
	rc, err := Run(env, io.Discard, &stderr)
	if err == nil {
		t.Fatalf("Run err = nil, want surfaced baseline-capture failure")
	}
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 for surfaced baseline failure", rc)
	}
	if !strings.Contains(err.Error(), "capture-baseline") {
		t.Fatalf("err = %q, should reference capture-baseline", err.Error())
	}
	// The run must abort before writing result.json (no mis-attributed result).
	if _, serr := os.Stat(filepath.Join(resultDir, "result.json")); serr == nil {
		t.Fatalf("result.json was written despite baseline failure; attribution would be unreliable")
	}
}

// TestRunThreadsSubdirCwdToCodex verifies an explicit WorkDir set to a module
// subdir is threaded all the way into codex's thread cwd (not collapsed to the
// repo root, which was the pre-scoping behavior).
func TestRunThreadsSubdirCwdToCodex(t *testing.T) {
	repo := setupGitRepo(t)
	sub := filepath.Join(repo, "server")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	rec := filepath.Join(t.TempDir(), "cwd.txt")
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":        "0.130.0",
		"FAKE_APPSERVER_SESSION":    "s-cwd",
		"FAKE_APPSERVER_RECORD_CWD": rec,
		"FAKE_APPSERVER_EDIT":       filepath.Join(sub, "out.txt") + ":done",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    sub,
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		ResultDir:  filepath.Join(repo, "run-cwd"),
	}
	if _, err := Run(env, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("read recorded cwd: %v", err)
	}
	if string(got) != sub {
		t.Fatalf("codex received cwd %q, want module subdir %q (cwd collapsed to repo root is the bug)", got, sub)
	}
}

// TestRunAutoScopesToModuleFromFiles verifies that, invoked at the repo root
// with no CODEX_WORKDIR, the run auto-derives the module owning CODEX_FILES and
// runs codex there.
func TestRunAutoScopesToModuleFromFiles(t *testing.T) {
	repo := setupGitRepo(t)
	sub := filepath.Join(repo, "server")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir module: %v", err)
	}
	// The module manifest is what makes server/ a module for derivation.
	if err := os.WriteFile(filepath.Join(sub, "go.mod"), []byte("module x/server\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	rec := filepath.Join(t.TempDir(), "cwd.txt")
	installFakeAppserver(t, map[string]string{
		"FAKE_CODEX_VERSION":        "0.130.0",
		"FAKE_APPSERVER_SESSION":    "s-autoscope",
		"FAKE_APPSERVER_RECORD_CWD": rec,
		"FAKE_APPSERVER_EDIT":       filepath.Join(sub, "server_hello.go") + ":done",
	})
	startInProcessBroker(t, repo)
	chdirTo(t, repo)
	env := Env{
		WorkDir:    repo, // invoked at the repo ROOT, no CODEX_WORKDIR
		Task:       "t",
		Acceptance: "a",
		Sandbox:    "workspace-write",
		Files:      "server/server_hello.go", // the only scoping signal
		ResultDir:  filepath.Join(repo, "run-autoscope"),
	}
	if _, err := Run(env, io.Discard, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("read recorded cwd: %v", err)
	}
	if string(got) != sub {
		t.Fatalf("codex cwd = %q, want auto-derived module %q (scoping from CODEX_FILES failed)", got, sub)
	}
}
