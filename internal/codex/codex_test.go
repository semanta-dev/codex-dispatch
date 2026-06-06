package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestFreshOutsideGitRepoReturnsError verifies the broker-socket resolution
// step rejects a cwd that isn't under a git repository, before any subprocess
// would be spawned. End-to-end coverage of Fresh/Resume + broker lives in
// internal/broker/handlers_dispatch_test.go (which exercises the same
// dispatch.run path that codex.Fresh routes through).
func TestFreshOutsideGitRepoReturnsError(t *testing.T) {
	tmp := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	_, err = Fresh(context.Background(), "x", "workspace-write", "", filepath.Join(tmp, "stdout.log"))
	if err == nil {
		t.Fatalf("Fresh outside a git repo should return error")
	}
}

// TestResumeOutsideGitRepoReturnsError mirrors the Fresh check for the resume
// code path.
func TestResumeOutsideGitRepoReturnsError(t *testing.T) {
	tmp := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	_, err = Resume(context.Background(), "prev-sess", "x", "workspace-write", "", filepath.Join(tmp, "stdout.log"))
	if err == nil {
		t.Fatalf("Resume outside a git repo should return error")
	}
}

// TestFreshCancelledContextReturnsError verifies pre-cancelled ctx is honored
// before any IO happens. Combined with the broker-side tests, this keeps the
// Phase 1 "cancelled ctx -> error" contract.
func TestFreshCancelledContextReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tmp := t.TempDir()
	_, err := Fresh(ctx, "x", "workspace-write", "", filepath.Join(tmp, "stdout.log"))
	if err == nil {
		t.Fatalf("Fresh with cancelled ctx should return error")
	}
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestResolveBrokerEndpointHonorsAddrPathOverride(t *testing.T) {
	tmp := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Setenv("CODEX_BROKER_ADDR_PATH", "shared/broker.addr")

	addrPath, repoRoot, err := ResolveBrokerEndpoint()
	if err != nil {
		t.Fatalf("ResolveBrokerEndpoint: %v", err)
	}
	if repoRoot != tmp {
		t.Fatalf("repoRoot = %q, want %q", repoRoot, tmp)
	}
	if !strings.HasSuffix(addrPath, filepath.Join("shared", "broker.addr")) {
		t.Fatalf("addrPath = %q, want shared broker path", addrPath)
	}
}

// TestRepoRootStartArgWalksToGitDir verifies the shared resolver walks up from
// an explicit start dir to the directory containing a `.git` DIRECTORY, and
// that BrokerAddrPath / ResolveBrokerAddr build the address file under it. This
// is the single resolver the hook + broker subcommands now share.
func TestRepoRootStartArgWalksToGitDir(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	deep := filepath.Join(tmp, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	root, err := RepoRoot(deep)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if root != tmp {
		t.Fatalf("RepoRoot = %q, want %q", root, tmp)
	}
	addr, err := ResolveBrokerAddr(deep)
	if err != nil {
		t.Fatalf("ResolveBrokerAddr: %v", err)
	}
	want := filepath.Join(tmp, ".codex-dispatch", "broker.addr")
	if addr != want {
		t.Fatalf("ResolveBrokerAddr = %q, want %q", addr, want)
	}
}

// TestRepoRootSkipsWorktreeGitFile is the regression for "do not treat a linked
// worktree's `.git` FILE as the root": a `.git` file (the worktree marker that
// points at the main repo's gitdir) must be skipped, and the walk must continue
// up to the main worktree whose `.git` is a real directory. The broker addr
// file and .codex-dispatch state live in the main worktree, so resolving to the
// linked worktree would point at the wrong (missing) address file.
func TestRepoRootSkipsWorktreeGitFile(t *testing.T) {
	tmp := t.TempDir()
	// Main worktree root: .git directory.
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git dir: %v", err)
	}
	// Linked worktree under the main one: .git FILE (not a directory).
	wt := filepath.Join(tmp, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+tmp+"/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	root, err := RepoRoot(wt)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if root != tmp {
		t.Fatalf("RepoRoot from a linked worktree = %q, want main worktree %q", root, tmp)
	}
}

// TestBrokerAddrPathHonorsOverride covers both the absolute and relative
// CODEX_BROKER_ADDR_PATH override forms on the shared helper.
func TestBrokerAddrPathHonorsOverride(t *testing.T) {
	root := "/repo/root"

	t.Setenv("CODEX_BROKER_ADDR_PATH", "shared/broker.addr")
	if got, want := BrokerAddrPath(root), filepath.Join(root, "shared", "broker.addr"); got != want {
		t.Fatalf("relative override: BrokerAddrPath = %q, want %q", got, want)
	}

	abs := filepath.Join(t.TempDir(), "abs", "broker.addr")
	t.Setenv("CODEX_BROKER_ADDR_PATH", abs)
	if got := BrokerAddrPath(root); got != abs {
		t.Fatalf("absolute override: BrokerAddrPath = %q, want %q", got, abs)
	}

	os.Unsetenv("CODEX_BROKER_ADDR_PATH")
	if got, want := BrokerAddrPath(root), filepath.Join(root, ".codex-dispatch", "broker.addr"); got != want {
		t.Fatalf("default: BrokerAddrPath = %q, want %q", got, want)
	}
}

// TestDeriveSessionIDHonorsEnv verifies an explicit CLAUDE_SESSION_ID is used
// verbatim (the SessionStart-hook path), unchanged by the collision-free
// fallback scheme.
func TestDeriveSessionIDHonorsEnv(t *testing.T) {
	t.Setenv("CLAUDE_SESSION_ID", "claude-abc-123")
	if got := DeriveSessionID(); got != "claude-abc-123" {
		t.Fatalf("DeriveSessionID() = %q, want the env value verbatim", got)
	}
}

// TestDeriveSessionIDDistinctParallel is the regression for pid-reuse session
// collisions: distinct parallel dispatches (no CLAUDE_SESSION_ID, same pid)
// must get distinct ids, otherwise a SessionEnd for one cancels another's
// queued tasks on the broker. The old "pid-%d" scheme produced identical ids
// for every call in the process.
func TestDeriveSessionIDDistinctParallel(t *testing.T) {
	// Ensure the env-supplied path is not taken so we exercise the fallback.
	old, had := os.LookupEnv("CLAUDE_SESSION_ID")
	os.Unsetenv("CLAUDE_SESSION_ID")
	t.Cleanup(func() {
		if had {
			os.Setenv("CLAUDE_SESSION_ID", old)
		} else {
			os.Unsetenv("CLAUDE_SESSION_ID")
		}
	})

	const n = 200
	seen := make(map[string]struct{}, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := DeriveSessionID()
			mu.Lock()
			seen[id] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("got %d distinct session ids from %d concurrent dispatches; want all distinct", len(seen), n)
	}
}

// TestLogHasFallbackMarker verifies the broker's fall-back-to-fresh marker
// is detectable in the log. Stale detection has moved to the RPC layer
// (appserver.ErrStaleSession); the byte-grep that used to live here is
// gone with the appserver.dispatch invention.
func TestLogHasFallbackMarker(t *testing.T) {
	tmp := t.TempDir()
	with := filepath.Join(tmp, "with")
	body := `{"method":"thread/started","params":{}}` + "\n" +
		"\n==== fell back to fresh dispatch ====\n" +
		`{"method":"thread/started","params":{}}` + "\n"
	if err := os.WriteFile(with, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !LogHasFallbackMarker(with) {
		t.Fatalf("LogHasFallbackMarker should be true when marker present")
	}

	without := filepath.Join(tmp, "without")
	if err := os.WriteFile(without, []byte(`{"method":"thread/started","params":{}}`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if LogHasFallbackMarker(without) {
		t.Fatalf("LogHasFallbackMarker should be false when marker absent")
	}
}
