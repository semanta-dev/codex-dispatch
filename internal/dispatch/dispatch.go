// Package dispatch is the top-level dispatch subcommand orchestrator. This
// file holds shared types and small helpers; Run() lives in run.go (Task 14).
package dispatch

import (
	"os/exec"
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

// newGit is a small helper for tests; production code uses internal/diff and
// internal/codex which manage their own exec.Cmd.
func newGit(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}
