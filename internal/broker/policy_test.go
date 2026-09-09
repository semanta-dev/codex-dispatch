package broker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func policyRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	policyGit(t, repo, "init", "-q")
	policyGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "init")
	return repo
}

func policyGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git: %v: %s", e, b)
	}
}

func TestExecutionPolicyRejectsBeforeSideEffects(t *testing.T) {
	repo := policyRepo(t)
	out := t.TempDir()
	link := filepath.Join(repo, "stdout.log")
	for _, tc := range []struct {
		name  string
		alter func(*DispatchRunParams)
	}{
		{"outside cwd", func(p *DispatchRunParams) { p.CWD = out }},
		{"outside result", func(p *DispatchRunParams) { p.ResultDir = out; p.LogPath = filepath.Join(out, "stdout.log") }},
		{"arbitrary log", func(p *DispatchRunParams) { p.LogPath = filepath.Join(repo, "important.txt") }},
		{"sandbox", func(p *DispatchRunParams) { p.Sandbox = "typo" }},
		{"resume", func(p *DispatchRunParams) { p.Mode = "resume" }},
		{"blank prompt", func(p *DispatchRunParams) { p.Prompt = " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DispatchRunParams{Mode: "fresh", Prompt: "task", CWD: repo, ResultDir: repo, LogPath: link}
			tc.alter(&p)
			called := false
			h := (ExecutionPolicy{Root: repo}).Wrap(func(context.Context, json.RawMessage) (any, error) { called = true; return nil, nil })
			b, _ := json.Marshal(p)
			if _, err := h(context.Background(), b); err == nil {
				t.Fatal("invalid request accepted")
			}
			if called {
				t.Fatal("handler invoked for invalid request")
			}
			if _, err := os.Stat(link); !os.IsNotExist(err) {
				t.Fatal("log created")
			}
		})
	}
}

func TestExecutionPolicyWorktreeAndExternalResults(t *testing.T) {
	repo := policyRepo(t)
	wt := filepath.Join(t.TempDir(), "linked")
	policyGit(t, repo, "worktree", "add", "--detach", wt)
	out := t.TempDir()
	p := DispatchRunParams{Mode: "fresh", Prompt: "task", CWD: wt, ResultDir: out, LogPath: filepath.Join(out, "stdout.log")}
	policy := ExecutionPolicy{Root: repo, ResultRoots: []string{out}}
	if err := policy.validate(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Sandbox != "workspace-write" {
		t.Fatal("empty sandbox not pinned")
	}
	policyGit(t, repo, "worktree", "remove", wt)
	if err := os.MkdirAll(wt, 0700); err != nil {
		t.Fatal(err)
	}
	if err := policy.validate(context.Background(), &p); err == nil {
		t.Fatal("unregistered worktree accepted")
	}
}

func TestExecutionPolicyRejectsSymlinkEscape(t *testing.T) {
	repo := policyRepo(t)
	out := t.TempDir()
	link := filepath.Join(repo, "escape")
	if err := os.Symlink(out, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := DispatchRunParams{Mode: "fresh", Prompt: "task", CWD: link, ResultDir: repo, LogPath: filepath.Join(repo, "stdout.log")}
	policy := ExecutionPolicy{Root: repo}
	if err := policy.validate(context.Background(), &p); err == nil {
		t.Fatal("cwd symlink escape accepted")
	}
	p.CWD = repo
	target := filepath.Join(out, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p.LogPath); err != nil {
		t.Fatal(err)
	}
	if err := policy.validate(context.Background(), &p); err == nil {
		t.Fatal("log symlink accepted")
	}
}

func TestAdmissionBoundIsAtomicAndReleasesCapacity(t *testing.T) {
	table := NewTable(8, 1)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := table.Admit("s", TaskParams{}); err == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 64 || len(table.List("")) != 64 {
		t.Fatalf("admitted %d tasks", accepted.Load())
	}
	id := table.List("")[0].ID
	if err := table.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := table.Admit("s", TaskParams{}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyLogDoesNotWriteThroughHardLink(t *testing.T) {
	repo := policyRepo(t)
	target := filepath.Join(t.TempDir(), "preserve")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(repo, "stdout.log")
	if err := os.Link(target, log); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	p := DispatchRunParams{Mode: "fresh", Prompt: "task", CWD: repo, ResultDir: repo, LogPath: log}
	w, err := (ExecutionPolicy{Root: repo}).openLog(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteSyntheticError("test", "new log"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "original" {
		t.Fatalf("hard link target altered: %q %v", b, err)
	}
}

func TestPolicyRechecksQueuedLogDestination(t *testing.T) {
	repo := policyRepo(t)
	result := filepath.Join(repo, "run")
	if err := os.Mkdir(result, 0700); err != nil {
		t.Fatal(err)
	}
	p := DispatchRunParams{Mode: "fresh", Prompt: "task", CWD: repo, ResultDir: result, LogPath: filepath.Join(result, "stdout.log")}
	policy := ExecutionPolicy{Root: repo}
	if err := policy.validate(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(result); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.Symlink(out, result); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if w, err := policy.openLog(context.Background(), &p); err == nil {
		w.Close()
		t.Fatal("replaced result directory accepted")
	}
	if _, err := os.Stat(filepath.Join(out, "stdout.log")); !os.IsNotExist(err) {
		t.Fatal("outside log created")
	}
}

func TestAdmissionRejectsBothExecutionRoutes(t *testing.T) {
	table := NewTable(1, 1)
	for i := 0; i < 64; i++ {
		if _, _, err := table.Admit("s", TaskParams{}); err != nil {
			t.Fatal(err)
		}
	}
	state := &BrokerState{Table: table, CWD: t.TempDir()}
	for _, handler := range []Handler{HandleDispatchRun(state), HandleTaskStart(state)} {
		_, err := handler(context.Background(), json.RawMessage(`{"mode":"fresh","prompt":"task"}`))
		if rpc := ToRPCError(err); rpc == nil || rpc.Code != -32008 {
			t.Fatalf("expected capacity rejection, got %v", err)
		}
	}
	if len(table.List("")) != 64 {
		t.Fatal("rejected execution registered task")
	}
}
