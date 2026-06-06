package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/broker"
	"github.com/semanta-dev/codex-dispatch/internal/dispatch"
)

func TestPrintVersion(t *testing.T) {
	version = "9.9.9-test"
	t.Cleanup(func() { version = "dev" })

	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "--version"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("--version exit = %d, want 0", rc)
	}
	if stderr.Len() != 0 {
		t.Fatalf("--version wrote unexpected stderr: %q", stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if got != "codex-dispatch 9.9.9-test" {
		t.Fatalf("--version stdout = %q, want %q", got, "codex-dispatch 9.9.9-test")
	}
}

func TestUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "bogus"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("unknown subcommand exit = %d, want 64", rc)
	}
	if !strings.Contains(stderr.String(), "codex-dispatch:") {
		t.Fatalf("stderr missing prefix: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "bogus") {
		t.Fatalf("stderr missing subcommand name: %q", stderr.String())
	}
	// The unknown-subcommand message must list every subcommand so the operator
	// can recover without consulting the docs.
	for _, sc := range subcommands {
		if !strings.Contains(stderr.String(), sc.name) {
			t.Fatalf("unknown-subcommand stderr missing %q: %q", sc.name, stderr.String())
		}
	}
}

func TestNoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("no subcommand exit = %d, want 64", rc)
	}
	if !strings.Contains(stderr.String(), "codex-dispatch:") {
		t.Fatalf("stderr missing prefix: %q", stderr.String())
	}
	// The no-subcommand usage line must list every subcommand, not a stale subset.
	for _, sc := range subcommands {
		if !strings.Contains(stderr.String(), sc.name) {
			t.Fatalf("no-subcommand stderr missing %q: %q", sc.name, stderr.String())
		}
	}
}

// TestHelpListsAllSubcommands asserts every help alias prints all subcommands,
// includes a README pointer, and exits 0 on stdout (not stderr).
func TestHelpListsAllSubcommands(t *testing.T) {
	for _, alias := range []string{"help", "--help", "-h"} {
		alias := alias
		t.Run(alias, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rc := run([]string{"codex-dispatch", alias}, &stdout, &stderr)
			if rc != 0 {
				t.Fatalf("%s exit = %d, want 0", alias, rc)
			}
			if stderr.Len() != 0 {
				t.Fatalf("%s wrote unexpected stderr: %q", alias, stderr.String())
			}
			out := stdout.String()
			for _, sc := range subcommands {
				if !strings.Contains(out, sc.name) {
					t.Fatalf("%s help missing subcommand %q:\n%s", alias, sc.name, out)
				}
			}
			if !strings.Contains(out, "README.md") {
				t.Fatalf("%s help missing README pointer:\n%s", alias, out)
			}
		})
	}
}

func TestPickIterationsDeterministicFallback(t *testing.T) {
	t.Setenv("PICK_TASK", "tiny task")
	t.Setenv("PICK_ACCEPTANCE", "")
	t.Setenv("PICK_DISABLE_LLM", "1")
	t.Setenv("PATH", "/usr/bin:/bin") // make sure no real claude is found

	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "pick-iterations"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("exit = %d, stderr=%q", rc, stderr.String())
	}
	s := strings.TrimSpace(stdout.String())
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("stdout = %q, want integer", s)
	}
	if n < 2 || n > 5 {
		t.Fatalf("n = %d, want in [2,5]", n)
	}
}

func TestPickIterationsRespectsBounds(t *testing.T) {
	t.Setenv("PICK_TASK", "x")
	t.Setenv("PICK_FLOOR", "4")
	t.Setenv("PICK_CEILING", "4")
	t.Setenv("PICK_DISABLE_LLM", "1")

	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "pick-iterations"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("exit = %d, stderr=%q", rc, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "4" {
		t.Fatalf("stdout = %q, want %q", got, "4")
	}
}

func TestPickIterationsEmptyTaskUsesFloor(t *testing.T) {
	// Clear PICK_TASK explicitly with proper restore-on-cleanup.
	for _, k := range []string{"PICK_TASK", "PICK_ACCEPTANCE", "PICK_FLOOR", "PICK_CEILING", "PICK_MODEL"} {
		k := k // capture for closure
		old, had := os.LookupEnv(k)
		os.Unsetenv(k)
		if had {
			t.Cleanup(func() { os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { os.Unsetenv(k) })
		}
	}
	t.Setenv("PICK_DISABLE_LLM", "1")

	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "pick-iterations"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("exit = %d", rc)
	}
	if got := strings.TrimSpace(stdout.String()); got != "2" {
		t.Fatalf("stdout = %q, want %q (floor)", got, "2")
	}
}

func TestPickIterationsStdoutIsOneIntegerLine(t *testing.T) {
	t.Setenv("PICK_TASK", "task")
	t.Setenv("PICK_DISABLE_LLM", "1")

	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "pick-iterations"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("exit = %d", rc)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got: %q", stderr.String())
	}
	out := stdout.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("stdout newlines = %d, want exactly 1: %q", strings.Count(out, "\n"), out)
	}
}

func TestCaptureDiffMissingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "capture-diff"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("exit = %d, want 64", rc)
	}
	if !strings.Contains(stderr.String(), "capture-diff") {
		t.Fatalf("stderr should mention capture-diff: %q", stderr.String())
	}
}

func TestCaptureDiffOneArgIsError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "capture-diff", "abc123"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("exit = %d, want 64", rc)
	}
}

func TestRunDispatchEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-appserver requires posix exec/signal handling")
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
			t.Fatalf("repo root not found")
		}
		root = parent
	}
	// Build fake-appserver as `codex` and put it first on PATH.
	fakeDir := t.TempDir()
	bin := filepath.Join(fakeDir, "codex")
	bcmd := exec.Command("go", "build", "-o", bin, ".")
	bcmd.Dir = filepath.Join(root, "tests/fixtures/fake-appserver")
	if out, err := bcmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake-appserver: %v: %s", err, out)
	}
	_ = os.Chmod(bin, fs.FileMode(0o755))
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CODEX_VERSION", "0.130.0")
	t.Setenv("FAKE_APPSERVER_SESSION", "s-end-to-end")

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\n"), 0o644)
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-q", "-m", "init"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// Spin up an in-process broker on the expected address file so
	// codex.Fresh can dial without needing an out-of-process broker
	// (os.Executable in `go test` doesn't have a broker subcommand).
	if err := os.MkdirAll(filepath.Join(repo, ".codex-dispatch"), 0o755); err != nil {
		t.Fatalf("mkdir broker dir: %v", err)
	}
	addrPath := filepath.Join(repo, ".codex-dispatch", "broker.addr")
	srv := broker.NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)
	table := broker.NewTable(8, 2048)
	state := &broker.BrokerState{Table: table}
	srv.HandleFunc("dispatch.run", broker.HandleDispatchRun(state))
	srv.HandleFunc("broker.ping", broker.HandleBrokerPing(state))
	bctx, bcancel := context.WithCancel(context.Background())
	bdone := make(chan struct{})
	go func() { _ = srv.Serve(bctx); close(bdone) }()
	t.Cleanup(func() {
		bcancel()
		<-bdone
	})
	waitFile(t, addrPath)

	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Setenv("CODEX_TASK", "x")
	t.Setenv("CODEX_ACCEPTANCE", "y")
	t.Setenv("CODEX_RESULT_DIR", filepath.Join(repo, "run"))

	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %s", rc, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repo, "run", "result.json")); err != nil {
		t.Fatalf("result.json missing: %v", err)
	}
	last := strings.TrimRight(stdout.String(), "\n")
	if i := strings.LastIndex(last, "\n"); i >= 0 {
		last = last[i+1:]
	}
	if last != filepath.Join(repo, "run") {
		t.Fatalf("stdout last line = %q, want %q", last, filepath.Join(repo, "run"))
	}
}

func TestDispatchListFlag(t *testing.T) {
	// Smoke: --list with no broker reachable produces a clear error.
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })

	dir := t.TempDir()
	// not a git repo
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch", "--list"}, &stdout, &stderr)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (not-in-repo)", rc)
	}
	if !strings.Contains(stderr.String(), "git repository") {
		t.Fatalf("stderr = %q, want git-repo error", stderr.String())
	}
}

func TestDispatchStatusFlagMissingArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch", "--status"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64", rc)
	}
	if !strings.Contains(stderr.String(), "task_id") {
		t.Fatalf("stderr = %q, want task_id mention", stderr.String())
	}
}

func TestDispatchCancelFlagMissingArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch", "--cancel"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64", rc)
	}
	if !strings.Contains(stderr.String(), "task_id") {
		t.Fatalf("stderr = %q, want task_id mention", stderr.String())
	}
}

func TestDispatchUnknownFlagRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch", "--bogus"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64 for unknown flag", rc)
	}
	if !strings.Contains(stderr.String(), "unknown dispatch flag") {
		t.Fatalf("stderr = %q, want unknown-flag message", stderr.String())
	}
}

func TestDispatchPositionalArgRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch", "somearg"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64 for positional arg", rc)
	}
	if !strings.Contains(stderr.String(), "no positional arguments") {
		t.Fatalf("stderr = %q, want positional-arg rejection", stderr.String())
	}
}

func TestDispatchListRejectsTrailingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch", "--list", "extra"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64 for --list with trailing args", rc)
	}
	if !strings.Contains(stderr.String(), "--list takes no arguments") {
		t.Fatalf("stderr = %q, want --list trailing-arg rejection", stderr.String())
	}
}

func TestDispatchDetachRejectsTrailingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch", "--detach", "extra"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64 for --detach with trailing args", rc)
	}
	if !strings.Contains(stderr.String(), "--detach takes no arguments") {
		t.Fatalf("stderr = %q, want --detach trailing-arg rejection", stderr.String())
	}
}

func TestDispatchStatusRejectsExtraArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := run([]string{"codex-dispatch", "dispatch", "--status", "id1", "id2"}, &stdout, &stderr)
	if rc != 64 {
		t.Fatalf("rc = %d, want 64 for --status with extra args", rc)
	}
}

// installStubCodex puts a do-nothing executable named `codex` first on PATH so
// dispatch.Validate's exec.LookPath("codex") check passes. This makes the
// detach-failure tests hermetic: without it, on a machine that has no real
// codex binary, Validate returns ErrCodexNotFound BEFORE PrepareRunDir is
// reached, the run dir is never created, and the cleanup assertions pass
// trivially without exercising the cleanup path. The stub is never executed —
// these tests fail at the broker's missing task.start handler, long before any
// codex run — so a minimal script is sufficient and avoids a build.
func installStubCodex(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if runtime.GOOS == "windows" {
		bin += ".bat"
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), fs.FileMode(0o755)); err != nil {
		t.Fatalf("write stub codex: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestDetachFailedSetupLeavesNoOrphanDir is the regression for the orphan run
// dir: when detached setup fails AFTER PrepareRunDir creates the run dir (here,
// task.start is rejected because the broker registers only broker.ping), the
// run dir we created must be removed, not left behind.
func TestDetachFailedSetupLeavesNoOrphanDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("broker fixture requires posix exec/signal handling")
	}
	// Stub codex on PATH so Validate passes and execution reaches PrepareRunDir
	// (which creates the run dir we then assert is cleaned up). Without this the
	// test is environment-dependent: it would pass trivially via ErrCodexNotFound
	// before the run dir is ever created.
	installStubCodex(t)

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-q", "-m", "init"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// In-process broker that answers broker.ping (so connect/EnsureBroker
	// succeeds) but has NO task.start handler, so TaskStart fails after the run
	// dir is created — exercising the cleanup path without a real codex run.
	if err := os.MkdirAll(filepath.Join(repo, ".codex-dispatch"), 0o755); err != nil {
		t.Fatalf("mkdir broker dir: %v", err)
	}
	addrPath := filepath.Join(repo, ".codex-dispatch", "broker.addr")
	srv := broker.NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)
	table := broker.NewTable(8, 2048)
	state := &broker.BrokerState{Table: table}
	srv.HandleFunc("broker.ping", broker.HandleBrokerPing(state))
	bctx, bcancel := context.WithCancel(context.Background())
	bdone := make(chan struct{})
	go func() { _ = srv.Serve(bctx); close(bdone) }()
	t.Cleanup(func() {
		bcancel()
		<-bdone
	})
	waitFile(t, addrPath)

	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// A run dir that does NOT pre-exist, so cleanup is allowed to remove it.
	resultDir := filepath.Join(repo, "run-orphan")
	t.Setenv("CODEX_TASK", "x")
	t.Setenv("CODEX_ACCEPTANCE", "y")
	t.Setenv("CODEX_RESULT_DIR", resultDir)

	var stdout, stderr bytes.Buffer
	rc := runDispatchDetach(&stdout, &stderr)
	if rc == 0 {
		t.Fatalf("detach unexpectedly succeeded; want failure to exercise cleanup")
	}
	if _, err := os.Stat(resultDir); !os.IsNotExist(err) {
		t.Fatalf("orphan run dir still present after failed detach: stat err = %v", err)
	}
}

// TestDetachPreexistingResultDirPreserved verifies cleanup never removes an
// operator-supplied CODEX_RESULT_DIR that already existed before the run, even
// when detached setup fails.
func TestDetachPreexistingResultDirPreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("broker fixture requires posix exec/signal handling")
	}
	// Stub codex on PATH so Validate passes and execution reaches PrepareRunDir,
	// exercising the cleanup path (which must preserve the pre-existing dir)
	// rather than bailing out early via ErrCodexNotFound.
	installStubCodex(t)

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-q", "-m", "init"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := os.MkdirAll(filepath.Join(repo, ".codex-dispatch"), 0o755); err != nil {
		t.Fatalf("mkdir broker dir: %v", err)
	}
	addrPath := filepath.Join(repo, ".codex-dispatch", "broker.addr")
	srv := broker.NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)
	table := broker.NewTable(8, 2048)
	state := &broker.BrokerState{Table: table}
	srv.HandleFunc("broker.ping", broker.HandleBrokerPing(state))
	bctx, bcancel := context.WithCancel(context.Background())
	bdone := make(chan struct{})
	go func() { _ = srv.Serve(bctx); close(bdone) }()
	t.Cleanup(func() {
		bcancel()
		<-bdone
	})
	waitFile(t, addrPath)

	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// A pre-existing operator-owned result dir with a sentinel file.
	resultDir := filepath.Join(repo, "preexisting-run")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatalf("mkdir result dir: %v", err)
	}
	sentinel := filepath.Join(resultDir, "operator-data.txt")
	if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	t.Setenv("CODEX_TASK", "x")
	t.Setenv("CODEX_ACCEPTANCE", "y")
	t.Setenv("CODEX_RESULT_DIR", resultDir)

	var stdout, stderr bytes.Buffer
	rc := runDispatchDetach(&stdout, &stderr)
	if rc == 0 {
		t.Fatalf("detach unexpectedly succeeded; want failure")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("pre-existing operator data was clobbered on failed detach: %v", err)
	}
}

func TestBrokerSubcommandRoutes(t *testing.T) {
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		done <- run([]string{"codex-dispatch", "broker"}, &stdout, &stderr)
	}()

	// Poll for the broker's address to appear (proves signal.Notify has run
	// inside runBroker — both happen synchronously before Serve blocks on Accept).
	addrPath := filepath.Join(dir, ".codex-dispatch", "broker.addr")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(addrPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Now safe to signal.
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)

	select {
	case rc := <-done:
		if rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
	case <-ctx.Done():
		t.Fatalf("broker did not exit after SIGTERM")
	}
}

// TestDetachPromptParity asserts the detached prompt builder produces text that
// is byte-identical to the prompt a synchronous dispatch writes (prompt.txt) for
// the same Env, including constraints, conventions, relevant files and feedback.
// This is the regression guard for the bug where --detach sent only
// task+acceptance and silently dropped the rest of the prompt.
func TestDetachPromptParity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-appserver requires posix exec/signal handling")
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
			t.Fatalf("repo root not found")
		}
		root = parent
	}
	// Build fake-appserver as `codex` and put it first on PATH so the sync
	// dispatch can run end-to-end and write prompt.txt.
	fakeDir := t.TempDir()
	bin := filepath.Join(fakeDir, "codex")
	bcmd := exec.Command("go", "build", "-o", bin, ".")
	bcmd.Dir = filepath.Join(root, "tests/fixtures/fake-appserver")
	if out, err := bcmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake-appserver: %v: %s", err, out)
	}
	_ = os.Chmod(bin, fs.FileMode(0o755))
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CODEX_VERSION", "0.130.0")
	t.Setenv("FAKE_APPSERVER_SESSION", "s-parity")

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// A conventions file and a relevant file so the prompt exercises every
	// section, not just task+acceptance.
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("USE TABS\n"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "foo.txt"), []byte("FOO\n"), 0o644); err != nil {
		t.Fatalf("write foo.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "init"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// Spin up an in-process broker on the expected address file so codex.Fresh
	// can dial without an out-of-process broker (mirrors TestRunDispatchEndToEnd).
	if err := os.MkdirAll(filepath.Join(repo, ".codex-dispatch"), 0o755); err != nil {
		t.Fatalf("mkdir broker dir: %v", err)
	}
	addrPath := filepath.Join(repo, ".codex-dispatch", "broker.addr")
	srv := broker.NewServer("127.0.0.1:0")
	srv.SetAddrFile(addrPath)
	table := broker.NewTable(8, 2048)
	state := &broker.BrokerState{Table: table}
	srv.HandleFunc("dispatch.run", broker.HandleDispatchRun(state))
	srv.HandleFunc("broker.ping", broker.HandleBrokerPing(state))
	bctx, bcancel := context.WithCancel(context.Background())
	bdone := make(chan struct{})
	go func() { _ = srv.Serve(bctx); close(bdone) }()
	t.Cleanup(func() {
		bcancel()
		<-bdone
	})
	waitFile(t, addrPath)

	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Setenv("CODEX_TASK", "do the work")
	t.Setenv("CODEX_ACCEPTANCE", "it is done")
	t.Setenv("CODEX_CONSTRAINTS", "ONLY edit foo.txt")
	t.Setenv("CODEX_FEEDBACK", "prior run missed the edge case")
	t.Setenv("CODEX_FILES", "foo.txt")
	resultDir := filepath.Join(repo, "run")
	t.Setenv("CODEX_RESULT_DIR", resultDir)

	// Synchronous dispatch writes prompt.txt with the canonical prompt.
	env := dispatch.EnvFromOS(os.Getenv)
	env.WorkDir = repo
	rc, derr := dispatch.Run(env, &bytes.Buffer{}, &bytes.Buffer{})
	if rc != 0 || derr != nil {
		t.Fatalf("sync dispatch rc=%d err=%v", rc, derr)
	}
	syncPrompt, err := os.ReadFile(filepath.Join(resultDir, "prompt.txt"))
	if err != nil {
		t.Fatalf("read sync prompt.txt: %v", err)
	}

	// The detach builder must produce byte-identical text for the same env.
	detachPrompt, err := buildDetachPrompt(env, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("buildDetachPrompt: %v", err)
	}
	if detachPrompt != string(syncPrompt) {
		t.Fatalf("detached prompt differs from synchronous prompt:\n--- detach ---\n%s\n--- sync ---\n%s", detachPrompt, syncPrompt)
	}

	// Sanity: the prompt actually carries the sections that --detach used to
	// drop, so the parity assertion is meaningful (not both-empty).
	for _, want := range []string{"CONSTRAINTS", "ONLY edit foo.txt", "CONVENTIONS", "RELEVANT FILES", "PRIOR FEEDBACK"} {
		if !strings.Contains(detachPrompt, want) {
			t.Fatalf("prompt missing %q; parity test is not exercising full prompt:\n%s", want, detachPrompt)
		}
	}
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", path)
}
