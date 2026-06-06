package dispatch

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
)

// Sentinel errors. main.go matches with errors.Is to choose an exit code.
var (
	ErrUsage         = errors.New("usage")           // → exit 64
	ErrNotInGitRepo  = errors.New("not in git repo") // → exit 2
	ErrCodexNotFound = errors.New("codex not found") // → exit 3
	ErrEmptyRepo     = errors.New("empty repo")      // → exit 2 (unborn branch)
)

var validSandbox = map[string]struct{}{
	"read-only":          {},
	"workspace-write":    {},
	"danger-full-access": {},
}

// Validate runs all pre-side-effect checks. The order matches the Bash script
// so behavior is at parity if multiple things are wrong.
func Validate(env Env) error {
	if !isGitRepo(env.WorkDir) {
		return fmt.Errorf("%w: cwd is not inside a git repository", ErrNotInGitRepo)
	}
	if !hasCommits(env.WorkDir) {
		return fmt.Errorf("%w: repository has no commits yet (unborn branch); commit at least once before dispatching", ErrEmptyRepo)
	}
	if _, err := exec.LookPath("codex"); err != nil {
		return fmt.Errorf("%w: codex binary not found on PATH", ErrCodexNotFound)
	}
	if env.Task == "" {
		return fmt.Errorf("%w: CODEX_TASK is required", ErrUsage)
	}
	if env.Acceptance == "" {
		return fmt.Errorf("%w: CODEX_ACCEPTANCE is required", ErrUsage)
	}
	if _, ok := validSandbox[env.Sandbox]; !ok {
		return fmt.Errorf("%w: CODEX_SANDBOX must be one of read-only|workspace-write|danger-full-access (got: %s)", ErrUsage, env.Sandbox)
	}
	return nil
}

func isGitRepo(workdir string) bool {
	if workdir == "" {
		return false
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return false
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = abs
	return cmd.Run() == nil
}

// hasCommits reports whether the repository has at least one commit on the
// current branch. A freshly `git init`'d repo (unborn branch) has no HEAD, so
// `git rev-parse --verify HEAD` exits non-zero; detecting it here lets Validate
// return a clear typed error instead of run.go surfacing a raw rev-parse failure.
func hasCommits(workdir string) bool {
	if workdir == "" {
		return false
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return false
	}
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "HEAD")
	cmd.Dir = abs
	return cmd.Run() == nil
}
