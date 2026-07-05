// Package main is the entry point for the codex-dispatch binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanta-dev/codex-dispatch/internal/broker"
	"github.com/semanta-dev/codex-dispatch/internal/codex"
	"github.com/semanta-dev/codex-dispatch/internal/diff"
	"github.com/semanta-dev/codex-dispatch/internal/dispatch"
	"github.com/semanta-dev/codex-dispatch/internal/pick"
	"github.com/semanta-dev/codex-dispatch/internal/prompt"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// subcommands lists every codex-dispatch subcommand with a one-line
// description, in the order they are printed by help and the usage line. Keep
// this in sync with the switch in run; the help test asserts every name here is
// reachable and documented.
var subcommands = []struct {
	name, desc string
}{
	{"dispatch", "delegate a coding task to codex (sync; flags: --list/--status/--cancel/--detach)"},
	{"capture-diff", "snapshot a result-dir diff: capture-diff <baseline-head> <result-dir>"},
	{"pick-iterations", "print the recommended iteration count for the current task"},
	{"broker", "run the persistent codex app-server broker (JSON-RPC over a unix socket)"},
	{"hook", "run as a Claude Code hook (reads the hook payload on stdin)"},
	{"version", "print the codex-dispatch version (also --version, -v)"},
	{"help", "show this help (also --help, -h)"},
}

// subcommandNames returns just the subcommand names, for the compact usage line.
func subcommandNames() string {
	names := make([]string, len(subcommands))
	for i, sc := range subcommands {
		names[i] = sc.name
	}
	return strings.Join(names, ", ")
}

// printHelp writes the full subcommand listing and a README pointer to w.
func printHelp(w io.Writer) {
	fmt.Fprintf(w, "codex-dispatch %s\n\n", version)
	fmt.Fprintln(w, "Usage: codex-dispatch <subcommand> [args]")
	fmt.Fprintln(w, "\nSubcommands:")
	for _, sc := range subcommands {
		fmt.Fprintf(w, "  %-15s %s\n", sc.name, sc.desc)
	}
	fmt.Fprintln(w, "\nSee README.md for full usage, environment variables, and examples.")
}

func run(argv []string, stdout, stderr io.Writer) int {
	if len(argv) < 2 {
		fmt.Fprintf(stderr, "codex-dispatch: missing subcommand (want one of: %s)\n", subcommandNames())
		return 64
	}
	switch argv[1] {
	case "--version", "-v", "version":
		fmt.Fprintf(stdout, "codex-dispatch %s\n", version)
		return 0
	case "--help", "-h", "help":
		printHelp(stdout)
		return 0
	case "pick-iterations":
		return runPickIterations(stdout, stderr)
	case "capture-diff":
		return runCaptureDiff(argv[2:], stderr)
	case "dispatch":
		return runDispatchCmd(argv[2:], stdout, stderr)
	case "broker":
		return runBroker(argv[2:], os.Stdin, stdout, stderr)
	case "hook":
		return runHook(argv[2:], os.Stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "codex-dispatch: unknown subcommand %q (want one of: %s)\n", argv[1], subcommandNames())
		return 64
	}
}

func runPickIterations(stdout, stderr io.Writer) int {
	opts := pick.OptionsFromEnv(os.Getenv)
	var runner pick.Runner
	if !opts.DisableLLM {
		runner = pick.ClaudeRunner{
			HasAPIKey: os.Getenv("ANTHROPIC_API_KEY") != "",
		}
	}
	n := pick.Pick(context.Background(), os.Getenv("PICK_TASK"), os.Getenv("PICK_ACCEPTANCE"), opts, runner)
	fmt.Fprintf(stdout, "%d\n", n)
	return 0
}

// runDispatchCmd routes the dispatch subcommand based on top-level flags.
// Sync path (no flag) goes to the existing dispatch.Run orchestration.
// --list/--status/--cancel/--detach route to the broker directly.
//
// Flag arity is validated up front: --list and --detach take no extra args,
// --status and --cancel take exactly one task_id, and an unknown leading flag
// is rejected rather than silently falling through to the synchronous dispatch
// (where a typo'd flag would be ignored and a full codex run launched).
func runDispatchCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runDispatchSync(stdout, stderr)
	}
	switch args[0] {
	case "--list":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "codex-dispatch: --list takes no arguments, got %v\n", args[1:])
			return 64
		}
		return runDispatchList(stdout, stderr)
	case "--status":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "codex-dispatch: --status requires exactly one task_id")
			return 64
		}
		return runDispatchStatus(args[1], stdout, stderr)
	case "--cancel":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "codex-dispatch: --cancel requires exactly one task_id")
			return 64
		}
		return runDispatchCancel(args[1], stdout, stderr)
	case "--detach":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "codex-dispatch: --detach takes no arguments, got %v\n", args[1:])
			return 64
		}
		return runDispatchDetach(stdout, stderr)
	default:
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(stderr, "codex-dispatch: unknown dispatch flag %q\n", args[0])
			return 64
		}
		fmt.Fprintf(stderr, "codex-dispatch: dispatch takes no positional arguments, got %v\n", args)
		return 64
	}
}

func runDispatchSync(stdout, stderr io.Writer) int {
	env := dispatch.EnvFromOS(os.Getenv)
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1
	}
	env.WorkDir = dispatch.ResolveWorkDir(wd, os.Getenv)
	rc, _ := dispatch.Run(env, stdout, stderr)
	return rc
}

func runDispatchList(stdout, stderr io.Writer) int {
	client, err := connectBrokerOrFail(stderr)
	if err != nil {
		return 1
	}
	defer client.Close()
	entries, err := client.TaskList(context.Background(), "")
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1
	}
	raw, _ := json.Marshal(entries)
	fmt.Fprintln(stdout, string(raw))
	return 0
}

func runDispatchStatus(taskID string, stdout, stderr io.Writer) int {
	client, err := connectBrokerOrFail(stderr)
	if err != nil {
		return 1
	}
	defer client.Close()
	st, err := client.TaskStatusCall(context.Background(), taskID)
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1
	}
	raw, _ := json.Marshal(st)
	fmt.Fprintln(stdout, string(raw))
	return 0
}

func runDispatchCancel(taskID string, stdout, stderr io.Writer) int {
	client, err := connectBrokerOrFail(stderr)
	if err != nil {
		return 1
	}
	defer client.Close()
	if err := client.TaskCancel(context.Background(), taskID); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, `{"ok":true}`)
	return 0
}

func runDispatchDetach(stdout, stderr io.Writer) int {
	env := dispatch.EnvFromOS(os.Getenv)
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1
	}
	env.WorkDir = dispatch.ResolveWorkDir(wd, os.Getenv)

	if err := dispatch.Validate(env); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		// Mirror the synchronous dispatch.Run exit-code mapping so --detach and
		// the sync path agree on validation failures. ErrEmptyRepo (unborn
		// branch) maps to exit 2 alongside ErrNotInGitRepo; the parity gap here
		// was introduced when ErrEmptyRepo was added on the sync side.
		if errors.Is(err, dispatch.ErrNotInGitRepo) || errors.Is(err, dispatch.ErrEmptyRepo) {
			return 2
		}
		if errors.Is(err, dispatch.ErrCodexNotFound) {
			return 3
		}
		if errors.Is(err, dispatch.ErrUsage) {
			return 64
		}
		return 1
	}

	// Build the same run-dir scaffolding the sync path would, then
	// hand off to the broker via task.start. Track whether PrepareRunDir
	// created the run dir (vs. an operator-supplied CODEX_RESULT_DIR that
	// already existed) so a failure AFTER setup can remove only a dir we own,
	// leaving no orphan run dir behind without clobbering operator data.
	preexisted := env.ResultDir != "" && dirExists(env.ResultDir)
	resultDir, logPath, err := dispatch.PrepareRunDir(env)
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return 1
	}
	cleanupRunDir := func() {
		if !preexisted {
			_ = os.RemoveAll(resultDir)
		}
	}

	client, err := connectBrokerOrFail(stderr)
	if err != nil {
		cleanupRunDir()
		return 1
	}
	defer client.Close()

	// Detached fallback ids share codex.DeriveSessionID's collision-free scheme
	// (pid + start time + random suffix) so parallel detached dispatches don't
	// alias and a SessionEnd for one can't cancel another's queued tasks.
	sessionID := codex.DeriveSessionID()

	// Build the full prompt (constraints/conventions/files/feedback included) so
	// the detached task receives byte-identical instructions to a synchronous
	// dispatch of the same env. Previously --detach sent only task+acceptance,
	// silently dropping the operator's constraints, conventions, relevant files
	// and prior feedback.
	promptText, err := buildDetachPrompt(env, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		cleanupRunDir()
		return 1
	}

	taskID, _, err := client.TaskStart(context.Background(), broker.DispatchRunParams{
		SessionID: sessionID,
		Mode:      "fresh",
		Prompt:    promptText,
		Sandbox:   env.Sandbox,
		ResultDir: resultDir,
		LogPath:   logPath,
	})
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		cleanupRunDir()
		return 1
	}
	fmt.Fprintln(stdout, taskID)
	return 0
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// buildDetachPrompt assembles the prompt for a detached dispatch. It mirrors the
// synchronous dispatch's buildPromptForEnv (internal/dispatch/run.go) field for
// field so that, for the same Env, the detached and synchronous prompts are
// byte-identical. Keep these two builders in lockstep; TestDetachPromptParity
// guards the invariant by comparing against the real prompt.txt a sync run
// writes.
func buildDetachPrompt(env dispatch.Env, stderr io.Writer) (string, error) {
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

func connectBrokerOrFail(stderr io.Writer) (*broker.Client, error) {
	addrPath, repoRoot, err := codex.ResolveBrokerEndpoint()
	if err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return nil, err
	}
	if err := codex.EnsureBrokerRunning(addrPath, repoRoot); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: %v\n", err)
		return nil, err
	}
	addr, err := os.ReadFile(addrPath)
	if err != nil {
		return nil, err
	}
	return broker.Dial(string(addr))
}

func runCaptureDiff(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "codex-dispatch: capture-diff requires two args: <baseline-head> <result-dir>")
		return 64
	}
	if _, err := diff.Capture(args[0], args[1]); err != nil {
		fmt.Fprintf(stderr, "codex-dispatch: capture-diff: %v\n", err)
		return 1
	}
	return 0
}
