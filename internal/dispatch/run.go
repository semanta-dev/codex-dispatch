package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/codex"
	"github.com/semanta-dev/codex-dispatch/internal/diff"
	"github.com/semanta-dev/codex-dispatch/internal/prompt"
	"github.com/semanta-dev/codex-dispatch/internal/result"
)

// Run executes the dispatch flow end-to-end. It returns an exit code; err is
// non-nil only when a validation/setup error occurred. After validation
// succeeds, Run always returns rc=0 (codex's own exit lives in result.json).
//
// The codex invocation runs under a context whose optional timeout is sourced
// from CODEX_DISPATCH_TIMEOUT_MS (default: no timeout). A cancelled or
// timed-out dispatch returns promptly with a clear status recorded in
// result.json rather than a bare setup error.
func Run(env Env, stdout, stderr io.Writer) (int, error) {
	ctx := context.Background()
	if d := dispatchTimeout(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	return runWithContext(ctx, env, stdout, stderr)
}

// dispatchTimeout returns the per-dispatch timeout from CODEX_DISPATCH_TIMEOUT_MS
// (in milliseconds). A missing/zero/invalid value means no timeout, matching the
// CODEX_BROKER_IDLE_MS parsing convention in cmd/codex-dispatch/broker.go.
func dispatchTimeout() time.Duration {
	if s := os.Getenv("CODEX_DISPATCH_TIMEOUT_MS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 0
}

// runWithContext is the context-aware body of Run. ctx governs the codex
// invocation; if it is cancelled or its deadline elapses, the dispatch returns
// promptly with a clear status in result.json instead of hanging on codex.
func runWithContext(ctx context.Context, env Env, stdout, stderr io.Writer) (int, error) {
	if err := Validate(env); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %s\n", stripSentinel(err))
		switch {
		case errors.Is(err, ErrNotInGitRepo):
			return 2, err
		case errors.Is(err, ErrEmptyRepo):
			return 2, err
		case errors.Is(err, ErrCodexNotFound):
			return 3, err
		case errors.Is(err, ErrUsage):
			return 64, err
		default:
			return 1, err
		}
	}

	repoRoot, err := topLevel(env.WorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1, err
	}

	// Monorepo auto-scoping: when the caller did not pin a working directory
	// (WorkDir is still the repo root), derive the go.work/module subdir that
	// owns the seeded files and run codex there. Files spanning multiple modules
	// (or none) leave WorkDir at the repo root.
	if SameDir(env.WorkDir, repoRoot) {
		if mod := DeriveModuleDir(repoRoot, env.Files); mod != "" {
			env.WorkDir = mod
		}
	}

	resultDir, err := ensureResultDir(env, repoRoot)
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1, err
	}

	// --- baseline capture --------------------------------------------------
	headSha, err := gitOutput(env.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1, err
	}
	headSha = strings.TrimSpace(headSha)
	if err := os.WriteFile(filepath.Join(resultDir, "baseline-head.txt"), []byte(headSha+"\n"), 0o644); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1, err
	}
	prePatch, _ := gitOutput(env.WorkDir, "-c", "core.quotepath=false", "diff", "HEAD")
	_ = os.WriteFile(filepath.Join(resultDir, "baseline-pre.patch"), []byte(prePatch), 0o644)
	// Record pre-existing dirty/untracked paths and a content signature for each
	// so post-run attribution excludes pre-existing WIP while still attributing
	// a codex edit to an already-dirty file. internal/diff owns this format.
	//
	// A baseline-capture failure is surfaced (not swallowed): without the
	// baseline, post-run attribution cannot distinguish codex's edits from
	// pre-existing WIP, so proceeding would silently mis-attribute files_changed
	// and the exit_code=4 gate. Fail the run with a clear message instead.
	if err := diff.CaptureBaseline(env.WorkDir, resultDir); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: capture-baseline failed (diff attribution would be unreliable): %v\n", err)
		return 1, fmt.Errorf("capture-baseline failed: %w", err)
	}

	// --- prompt build ------------------------------------------------------
	promptText, err := buildPromptForEnv(env, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1, err
	}
	if err := os.WriteFile(filepath.Join(resultDir, "prompt.txt"), []byte(promptText), 0o644); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1, err
	}

	// --- codex invocation --------------------------------------------------
	logPath := filepath.Join(resultDir, "stdout.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1, err
	}
	var (
		run             codex.Run
		fellBackToFresh bool
	)
	if env.SessionID != "" {
		run, err = codex.Resume(ctx, env.SessionID, promptText, env.Sandbox, env.Model, logPath, env.WorkDir)
		if err != nil {
			if rc, cerr, handled := handleCanceled(ctx, err, resultDir, logPath, stderr); handled {
				return rc, cerr
			}
			fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
			return 1, err
		}
		if run.FellBackToFresh {
			// The broker already handled the stale resume internally; the
			// marker is written and the fresh attempt has already run.
			fellBackToFresh = true
		}
	} else {
		run, err = codex.Fresh(ctx, promptText, env.Sandbox, env.Model, logPath, env.WorkDir)
		if err != nil {
			if rc, cerr, handled := handleCanceled(ctx, err, resultDir, logPath, stderr); handled {
				return rc, cerr
			}
			fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
			return 1, err
		}
	}

	// --- session id from broker response ----------------------------------
	// The broker returns the codex Thread.id directly; no need to parse the
	// log. Phase 1's codex.ParseSessionID was a byte-level scanner over the
	// old `codex exec --json` log format; with codex app-server we have a
	// typed response instead.
	sessionID := run.SessionID

	// --- diff capture (warn on error, do not fail dispatch) ----------------
	stats, err := diff.CaptureInDir(env.WorkDir, headSha, resultDir)
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: warning: capture-diff failed: %v\n", err)
		stats = diff.Stats{FilesChanged: []string{}}
	}

	exitCode := run.ExitCode
	errorMessage := run.ErrorMessage
	if exitCode == 0 && len(stats.FilesChanged) == 0 {
		exitCode = 4
		errorMessage = "codex completed without meaningful repository edits"
	}

	// --- result.json -------------------------------------------------------
	res := result.Result{
		ExitCode:                exitCode,
		SessionID:               sessionID,
		FilesChanged:            stats.FilesChanged,
		LinesAdded:              stats.LinesAdded,
		LinesRemoved:            stats.LinesRemoved,
		StdoutPath:              logPath,
		DiffPath:                filepath.Join(resultDir, "diff.patch"),
		FellBackToFresh:         fellBackToFresh,
		ErrorMessage:            errorMessage,
		FilesChangedOutsideSeed: filesOutsideSeed(env.Files, stats.FilesChanged),
	}
	if err := result.Write(resultDir, res); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1, err
	}

	fmt.Fprintln(stdout, resultDir)
	return 0, nil
}

// handleCanceled detects a dispatch aborted by ctx cancellation or a timeout.
// When the context is done, it writes a result.json with a clear status so the
// dispatch returns promptly (rc=0, the failure is data in result.json, matching
// the failed-turn convention) rather than surfacing a bare broker/read error.
// handled is false when ctx is still live (the error was unrelated to ctx), in
// which case the caller falls back to its normal error handling.
func handleCanceled(ctx context.Context, runErr error, resultDir, logPath string, stderr io.Writer) (rc int, err error, handled bool) {
	if ctx.Err() == nil {
		return 0, nil, false
	}
	msg := "codex dispatch canceled before completion"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		msg = "codex dispatch timed out before completion (CODEX_DISPATCH_TIMEOUT_MS)"
	}
	fmt.Fprintf(stderr, "codex-dispatch: %s: %v\n", msg, runErr)
	// No diff was captured on the cancel/timeout path, so leave DiffPath empty
	// rather than point at a diff.patch that was never written. A consumer that
	// reads diff_path must treat the empty string as "no diff artifact present".
	res := result.Result{
		ExitCode:     124, // conventional timeout/abort code
		FilesChanged: []string{},
		StdoutPath:   logPath,
		DiffPath:     "",
		ErrorMessage: msg,
	}
	if werr := result.Write(resultDir, res); werr != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", werr)
		return 1, werr, true
	}
	return 0, nil, true
}

func ensureResultDir(env Env, repoRoot string) (string, error) {
	if env.ResultDir != "" {
		if err := os.MkdirAll(env.ResultDir, 0o755); err != nil {
			return "", err
		}
		return env.ResultDir, nil
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(repoRoot, ".codex-dispatch", "runs", fmt.Sprintf("%s-%d", ts, os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// PrepareRunDir creates the result directory (honoring env.ResultDir when set,
// otherwise defaulting to <repoRoot>/.codex-dispatch/runs/<ts>-<pid>/) and
// returns (resultDir, logPath). It is used by the --detach path in main.go so
// that detached tasks use the same dir layout as the synchronous dispatch.Run.
func PrepareRunDir(env Env) (resultDir, logPath string, err error) {
	repoRoot, err := topLevel(env.WorkDir)
	if err != nil {
		return "", "", err
	}
	resultDir, err = ensureResultDir(env, repoRoot)
	if err != nil {
		return "", "", err
	}
	logPath = filepath.Join(resultDir, "stdout.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		return "", "", err
	}
	return resultDir, logPath, nil
}

func buildPromptForEnv(env Env, stderr io.Writer) (string, error) {
	in := prompt.Inputs{
		Task:        env.Task,
		Acceptance:  env.Acceptance,
		Constraints: env.Constraints,
		Feedback:    env.Feedback,
	}
	convPath := env.ConventionsFile
	if convPath == "" {
		convPath = prompt.DetectConventions(env.WorkDir)
	}
	if convPath != "" {
		if _, err := os.Stat(convPath); err == nil {
			content, err := prompt.ReadConventions(convPath)
			if err != nil {
				return "", err
			}
			in.ConventionsTag = convPath
			in.Conventions = content
		} else if env.ConventionsFile != "" {
			in.ConventionsTag = env.ConventionsFile
			in.ConventionsMissing = true
			fmt.Fprintf(stderr, "codex-dispatch: warning: CODEX_CONVENTIONS_FILE not found: %s\n", env.ConventionsFile)
		}
	}
	if env.Files != "" {
		for _, p := range strings.Split(env.Files, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			fs := prompt.FileSection{Path: p}
			// Files paths are resolved relative to the work directory when
			// not absolute, matching the Bash script which runs from cwd.
			readPath := p
			if !filepath.IsAbs(readPath) {
				readPath = filepath.Join(env.WorkDir, readPath)
			}
			content, err := os.ReadFile(readPath)
			if err != nil {
				fs.Missing = true
				fmt.Fprintf(stderr, "codex-dispatch: warning: CODEX_FILES path not found: %s\n", p)
			} else {
				fs.Content = string(content)
			}
			in.FilesIncluded = append(in.FilesIncluded, fs)
		}
	}
	return prompt.Build(in), nil
}

// filesOutsideSeed returns the changed files not covered by the CODEX_FILES
// seed (the advisory relevant-files hint). A file is "covered" when it equals a
// seed entry or lives under a seed directory entry. The seed is advisory, so an
// empty seed (or all-covered changes) yields nil — the result.json field is
// omitempty, so "no signal" stays absent rather than an empty array. Order
// follows `changed`. This is a scope-creep signal for the reviewer, not a gate.
func filesOutsideSeed(seedCSV string, changed []string) []string {
	if strings.TrimSpace(seedCSV) == "" || len(changed) == 0 {
		return nil
	}
	var seeds []string
	for _, p := range strings.Split(seedCSV, ",") {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if p != "" {
			seeds = append(seeds, filepath.Clean(p))
		}
	}
	if len(seeds) == 0 {
		return nil
	}
	var outside []string
	for _, f := range changed {
		cf := filepath.Clean(strings.TrimSpace(f))
		covered := false
		for _, s := range seeds {
			if cf == s || strings.HasPrefix(cf, s+"/") {
				covered = true
				break
			}
		}
		if !covered {
			outside = append(outside, f)
		}
	}
	return outside
}

func topLevel(workdir string) (string, error) {
	out, err := gitOutput(workdir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func gitOutput(workdir string, args ...string) (string, error) {
	cmd := newGit(workdir, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// stripSentinel removes the leading "<sentinel>: " prefix added by fmt.Errorf
// wrapping so the user-facing message is the second half.
func stripSentinel(err error) string {
	if _, rest, found := strings.Cut(err.Error(), ": "); found {
		return rest
	}
	return err.Error()
}
