// Package dispatch is the top-level dispatch subcommand orchestrator. This
// file holds shared types and small helpers; Run() lives in run.go (Task 14).
package dispatch

import (
	"os/exec"
	"path/filepath"
)

// Env is the typed view of the CODEX_* environment for the dispatch subcommand.
type Env struct {
	WorkDir         string
	Task            string
	Acceptance      string
	Files           string
	Constraints     string
	Feedback        string
	SessionID       string
	Sandbox         string
	Model           string
	ResultDir       string
	ConventionsFile string
}

// EnvFromOS builds an Env from a getenv function (use os.Getenv in production).
// WorkDir is intentionally not set here — callers (main, tests) set it.
func EnvFromOS(getenv func(string) string) Env {
	sb := getenv("CODEX_SANDBOX")
	if sb == "" {
		sb = "danger-full-access"
	}
	return Env{
		Task:            getenv("CODEX_TASK"),
		Acceptance:      getenv("CODEX_ACCEPTANCE"),
		Files:           getenv("CODEX_FILES"),
		Constraints:     getenv("CODEX_CONSTRAINTS"),
		Feedback:        getenv("CODEX_FEEDBACK"),
		SessionID:       getenv("CODEX_SESSION_ID"),
		Model:           getenv("CODEX_MODEL"),
		Sandbox:         sb,
		ResultDir:       getenv("CODEX_RESULT_DIR"),
		ConventionsFile: getenv("CODEX_CONVENTIONS_FILE"),
	}
}

// ResolveWorkDir applies the CODEX_WORKDIR override to the process cwd. An
// absolute override is used as-is; a relative one is resolved under cwd; an
// unset override leaves cwd unchanged. The result seeds Env.WorkDir, which
// run.go may further narrow via monorepo auto-scoping.
func ResolveWorkDir(cwd string, getenv func(string) string) string {
	wd := getenv("CODEX_WORKDIR")
	if wd == "" {
		return cwd
	}
	if filepath.IsAbs(wd) {
		return wd
	}
	return filepath.Join(cwd, wd)
}

// newGit is a small helper for tests; production code uses internal/diff and
// internal/codex which manage their own exec.Cmd.
func newGit(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}
