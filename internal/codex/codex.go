// Package codex is the dispatch-side wrapper around the codex broker. Fresh
// and Resume route through internal/broker, which speaks the real codex
// app-server JSON-RPC protocol.
package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/broker"
)

// Run is the broker's terminal response surfaced to internal/dispatch.
type Run struct {
	ExitCode        int
	SessionID       string
	FellBackToFresh bool
	ErrorMessage    string
}

// LogHasFallbackMarker reports whether the broker's fall-back-to-fresh marker
// is present in the log. Retained for the e2e smoke procedure and any future
// log-introspection tooling; the runtime detection lives in the broker now.
func LogHasFallbackMarker(logPath string) bool {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return false
	}
	return hasFallbackMarker(b)
}

func hasFallbackMarker(b []byte) bool {
	const marker = "==== fell back to fresh dispatch ===="
	for i := 0; i+len(marker) <= len(b); i++ {
		if string(b[i:i+len(marker)]) == marker {
			return true
		}
	}
	return false
}

// Fresh dispatches a fresh codex turn for the given prompt + sandbox, writing
// codex events to logPath. Blocks until codex completes. Public API matches
// Phase 1 for callers in internal/dispatch.
func Fresh(ctx context.Context, prompt, sandbox, model, logPath string) (Run, error) {
	return dispatchViaBroker(ctx, "fresh", "", prompt, sandbox, model, logPath)
}

// Resume dispatches a resume codex turn against sessionID. sandbox is accepted
// for API symmetry; the broker decides whether to pass it through.
func Resume(ctx context.Context, sessionID, prompt, sandbox, model, logPath string) (Run, error) {
	return dispatchViaBroker(ctx, "resume", sessionID, prompt, sandbox, model, logPath)
}

func dispatchViaBroker(ctx context.Context, mode, prevSessionID, prompt, sandbox, model, logPath string) (Run, error) {
	if ctx.Err() != nil {
		return Run{ExitCode: -1}, ctx.Err()
	}
	addrPath, repoRoot, err := ResolveBrokerEndpoint()
	if err != nil {
		return Run{ExitCode: -1}, err
	}
	if err := EnsureBrokerRunning(addrPath, repoRoot); err != nil {
		return Run{ExitCode: -1}, err
	}
	addr, err := readBrokerAddr(addrPath)
	if err != nil {
		return Run{ExitCode: -1}, err
	}
	client, err := broker.Dial(addr)
	if err != nil {
		return Run{ExitCode: -1}, fmt.Errorf("broker dial: %w", err)
	}
	defer client.Close()

	resultDir := filepath.Dir(logPath)
	res, err := client.DispatchRun(ctx, broker.DispatchRunParams{
		SessionID:     DeriveSessionID(),
		Mode:          mode,
		Prompt:        prompt,
		Sandbox:       sandbox,
		PrevSessionID: prevSessionID,
		ResultDir:     resultDir,
		LogPath:       logPath,
		CWD:           repoRoot,
		Model:         model,
	}, nil)
	if err != nil {
		return Run{ExitCode: -1}, err
	}
	return Run{
		ExitCode:        res.ExitCode,
		SessionID:       res.SessionID,
		FellBackToFresh: res.FellBackToFresh,
		ErrorMessage:    res.ErrorMessage,
	}, nil
}

// RepoRoot walks up from start (or the current working directory when start is
// empty) and returns the directory whose `.git` is a real repository — i.e. a
// `.git` *directory* (the main worktree). A bare `.git` *file* marks a linked
// git worktree, which points at the main repo's gitdir; the broker's address
// file and `.codex-dispatch` state live in the main worktree, so a linked
// worktree's `.git` file is intentionally NOT treated as the root and the walk
// continues upward. If no `.git` directory is found before the filesystem root,
// an error is returned.
//
// This is the single shared repo-root resolver; cmd/codex-dispatch's broker and
// hook subcommands call it (via ResolveBrokerEndpoint / ResolveBrokerAddr) so
// all three former copies share identical semantics.
func RepoRoot(start string) (string, error) {
	root := start
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = wd
	}
	for {
		info, err := os.Stat(filepath.Join(root, ".git"))
		if err == nil && info.IsDir() {
			return root, nil
		}
		parent := filepath.Dir(root)
		if parent == root {
			if start == "" {
				return "", fmt.Errorf("not inside a git repository")
			}
			return "", fmt.Errorf("not inside a git repository (from %s)", start)
		}
		root = parent
	}
}

// BrokerAddrPath returns the broker address file for a resolved repo root,
// honoring the CODEX_BROKER_ADDR_PATH override (absolute paths are used as-is;
// relative paths are resolved under root). Without the override it defaults to
// <root>/.codex-dispatch/broker.addr.
func BrokerAddrPath(root string) string {
	if override := os.Getenv("CODEX_BROKER_ADDR_PATH"); override != "" {
		if filepath.IsAbs(override) {
			return override
		}
		return filepath.Join(root, override)
	}
	return filepath.Join(root, ".codex-dispatch", "broker.addr")
}

// ResolveBrokerAddr resolves the broker address file starting from a specific
// directory (the hook's reported cwd). It is the cwd-scoped twin of
// ResolveBrokerEndpoint and shares the same root + override semantics.
func ResolveBrokerAddr(start string) (string, error) {
	root, err := RepoRoot(start)
	if err != nil {
		return "", err
	}
	return BrokerAddrPath(root), nil
}

// ResolveBrokerEndpoint walks up from the current working directory looking for
// a `.git` directory and returns the broker address file under
// <root>/.codex-dispatch/broker.addr (honoring CODEX_BROKER_ADDR_PATH).
func ResolveBrokerEndpoint() (string, string, error) {
	root, err := RepoRoot("")
	if err != nil {
		return "", "", err
	}
	return BrokerAddrPath(root), root, nil
}

// EnsureBrokerRunning pings the broker; if unreachable, spawns one via
// os.Executable() and polls for its address file to become reachable.
func EnsureBrokerRunning(addrPath, repoRoot string) error {
	if addr, err := readBrokerAddr(addrPath); err == nil && pingBroker(addr) == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(addrPath), 0o755); err != nil {
		return fmt.Errorf("mkdir broker dir: %w", err)
	}
	_ = os.Remove(addrPath)
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	cmd := exec.Command(exe, "broker")
	cmd.Dir = repoRoot
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn broker: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		addr, err := readBrokerAddr(addrPath)
		if err == nil && pingBroker(addr) == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("broker did not come up within 2s at %s", addrPath)
}

func readBrokerAddr(addrPath string) (string, error) {
	b, err := os.ReadFile(addrPath)
	if err != nil {
		return "", err
	}
	addr := strings.TrimSpace(string(b))
	if addr == "" {
		return "", fmt.Errorf("empty broker addr file: %s", addrPath)
	}
	return addr, nil
}

func pingBroker(addr string) error {
	client, err := broker.Dial(addr)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = client.Ping(ctx)
	return err
}

// DeriveSessionID returns the Claude session id from the CLAUDE_SESSION_ID
// env var (set by the SessionStart hook), or a unique process-scoped id when
// hooks haven't run.
//
// The fallback id must be unique per dispatch: parallel dispatches that share a
// pid (or land on a reused pid) would otherwise collide, and a SessionEnd for
// one would cancel the other's queued tasks on the broker. We append the
// process start time (nanoseconds) and a random suffix so distinct dispatches —
// even concurrent ones in the same process — get distinct ids. The --detach
// path in cmd/codex-dispatch reuses this so detached tasks share the same
// collision-free id scheme.
func DeriveSessionID() string {
	if v := os.Getenv("CLAUDE_SESSION_ID"); v != "" {
		return v
	}
	return fmt.Sprintf("pid-%d-%d-%s", os.Getpid(), time.Now().UnixNano(), randomSuffix())
}

// randomSuffix returns a short random hex string, falling back to a
// nanosecond timestamp if the system RNG is unavailable so the id is never
// empty.
func randomSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// IsBrokerProcessError reports whether err is consistent with the broker
// dying mid-call (used by callers to format a clearer stderr message).
func IsBrokerProcessError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, exec.ErrNotFound)
}
