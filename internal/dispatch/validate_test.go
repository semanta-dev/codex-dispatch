package dispatch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireCodex skips the calling test when the codex binary is not on PATH.
// Validate's check order is git-repo → codex-on-PATH → env vars → sandbox, so
// any test that expects Validate to reach the post-LookPath checks must call
// this guard.
func requireCodex(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not on PATH; skipping test")
	}
}

func setupGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := newGit(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := newGit(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestValidateMissingTask(t *testing.T) {
	requireCodex(t)
	env := Env{WorkDir: setupGitRepo(t), Acceptance: "ok", Sandbox: "workspace-write"}
	err := Validate(env)
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want ErrUsage", err)
	}
	if !strings.Contains(err.Error(), "CODEX_TASK") {
		t.Fatalf("err = %q, should mention CODEX_TASK", err.Error())
	}
}

func TestValidateMissingAcceptance(t *testing.T) {
	requireCodex(t)
	env := Env{WorkDir: setupGitRepo(t), Task: "do x", Sandbox: "workspace-write"}
	err := Validate(env)
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want ErrUsage", err)
	}
	if !strings.Contains(err.Error(), "CODEX_ACCEPTANCE") {
		t.Fatalf("err = %q, should mention CODEX_ACCEPTANCE", err.Error())
	}
}

func TestValidateInvalidSandbox(t *testing.T) {
	requireCodex(t)
	env := Env{
		WorkDir:    setupGitRepo(t),
		Task:       "x",
		Acceptance: "y",
		Sandbox:    "totally-bogus",
	}
	err := Validate(env)
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want ErrUsage", err)
	}
	if !strings.Contains(err.Error(), "CODEX_SANDBOX") {
		t.Fatalf("err = %q, should mention CODEX_SANDBOX", err.Error())
	}
}

func TestValidateAllSandboxValuesAllowed(t *testing.T) {
	requireCodex(t)
	for _, sb := range []string{"read-only", "workspace-write", "danger-full-access"} {
		env := Env{WorkDir: setupGitRepo(t), Task: "x", Acceptance: "y", Sandbox: sb}
		if err := Validate(env); err != nil {
			t.Fatalf("sandbox=%q rejected: %v", sb, err)
		}
	}
}

func TestValidateNotInGitRepo(t *testing.T) {
	env := Env{WorkDir: t.TempDir(), Task: "x", Acceptance: "y", Sandbox: "workspace-write"}
	err := Validate(env)
	if !errors.Is(err, ErrNotInGitRepo) {
		t.Fatalf("err = %v, want ErrNotInGitRepo", err)
	}
}

// setupEmptyGitRepo initializes a repo with no commits (unborn branch) so HEAD
// does not resolve.
func setupEmptyGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := newGit(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestValidateEmptyRepoNoCommits(t *testing.T) {
	// The empty-repo check runs before the codex-on-PATH check, so this does
	// not need codex installed.
	env := Env{WorkDir: setupEmptyGitRepo(t), Task: "x", Acceptance: "y", Sandbox: "workspace-write"}
	err := Validate(env)
	if !errors.Is(err, ErrEmptyRepo) {
		t.Fatalf("err = %v, want ErrEmptyRepo", err)
	}
	if !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("err = %q, should explain the repo has no commits", err.Error())
	}
}

func TestEnvFromOSPopulatesDefaults(t *testing.T) {
	t.Setenv("CODEX_TASK", "t")
	t.Setenv("CODEX_ACCEPTANCE", "a")
	t.Setenv("CODEX_SANDBOX", "")
	env := EnvFromOS(os.Getenv)
	if env.Sandbox != "workspace-write" {
		t.Fatalf("default sandbox = %q, want workspace-write", env.Sandbox)
	}
	if env.Task != "t" || env.Acceptance != "a" {
		t.Fatalf("Task/Acceptance not populated: %+v", env)
	}
	if env.Model != "" {
		t.Fatalf("Model should be empty when CODEX_MODEL is unset, got %q", env.Model)
	}
}

func TestEnvFromOSReadsModel(t *testing.T) {
	t.Setenv("CODEX_MODEL", "gpt-5.5")
	env := EnvFromOS(os.Getenv)
	if env.Model != "gpt-5.5" {
		t.Fatalf("Model = %q, want gpt-5.5 (from CODEX_MODEL)", env.Model)
	}
}
