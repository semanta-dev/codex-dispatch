package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/codex/appserver"
)

// AppServerFactory builds a fresh AppServer instance. Defaults to spawning
// real `codex app-server`; tests override it to inject in-process pipes.
type AppServerFactory func(cwd string) *appserver.AppServer

var defaultAppServerFactory AppServerFactory = func(cwd string) *appserver.AppServer {
	cmdPath := "codex"
	if v := os.Getenv("CODEX_BROKER_CODEX_BIN"); v != "" {
		cmdPath = v
	}
	env := os.Environ()
	args := []string{"app-server"}
	// Disable codex's configured MCP servers for dispatch runs by default, so a
	// dispatched codex focuses on the repository and cannot be derailed by a slow
	// or failing MCP / web tool (a local MCP endpoint, playwright, semanta, etc.)
	// before it even reads the code. codex exposes no global MCP switch, so each
	// configured server is disabled individually via
	// `-c mcp_servers.<name>.enabled=false`. Opt out (leave MCP as configured) by
	// setting CODEX_DISPATCH_KEEP_MCP to a non-empty value.
	if os.Getenv("CODEX_DISPATCH_KEEP_MCP") == "" {
		args = append(args, mcpDisableArgs(listMCPServerNames(cmdPath, cwd, env))...)
	}
	return appserver.New(cmdPath, args, env, cwd)
}

// listMCPServerNames asks codex for its configured MCP servers so the dispatch
// spawn can disable them. Best-effort: any failure (codex missing, timeout,
// unparseable output) returns nil and the spawn proceeds with MCP left as
// configured — never worse than before this behavior existed.
func listMCPServerNames(codexBin, cwd string, env []string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBin, "mcp", "list", "--json")
	cmd.Dir = cwd
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseMCPServerNames(out)
}

// parseMCPServerNames extracts server names from `codex mcp list --json` output,
// keeping only names that are safe to embed in a `-c` dotted key path.
func parseMCPServerNames(jsonOut []byte) []string {
	var servers []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(jsonOut, &servers); err != nil {
		return nil
	}
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		if isSafeMCPName(s.Name) {
			names = append(names, s.Name)
		}
	}
	return names
}

// mcpDisableArgs turns MCP server names into codex `-c` overrides that disable
// each one. codex has no global MCP switch and merges (rather than replaces) a
// `mcp_servers={}` override, so disabling is necessarily per-server.
func mcpDisableArgs(names []string) []string {
	args := make([]string, 0, len(names)*2)
	for _, name := range names {
		args = append(args, "-c", fmt.Sprintf("mcp_servers.%s.enabled=false", name))
	}
	return args
}

// isSafeMCPName reports whether name is a bare TOML key (letters, digits, '-',
// '_') so it can be embedded in a `mcp_servers.<name>.enabled` dotted path
// without quoting (codex rejects quoted key segments in `-c`). A name with any
// other character is skipped rather than risk a malformed override that would
// fail the whole app-server spawn.
func isSafeMCPName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// SetAppServerFactory replaces the constructor used by EnsureAppServer.
// Returns the previous factory so callers can restore it on test teardown.
// Safe to call from one goroutine at a time (only used in tests).
func (b *BrokerState) SetAppServerFactory(f AppServerFactory) AppServerFactory {
	b.appserverMu.Lock()
	defer b.appserverMu.Unlock()
	prev := b.appserverFactory
	b.appserverFactory = f
	return prev
}

// EnsureAppServer returns a live AppServer, spawning one if needed or
// replacing a dead one. Idempotent and safe for concurrent callers.
//
// Recycle safety: when the current instance is dead it is DETACHED from the
// singleton slot and reaped in the BACKGROUND rather than Close-d synchronously
// under appserverMu. A synchronous Close would (a) hold appserverMu while
// reapChild waits up to ~4s for the child, stalling every peer dispatch, and
// (b) tear the shared codex child down while a peer turn that was in-flight on
// the same instance is still draining its per-turn pump — exactly the race this
// packet closes. reapInstance honours the instance's own teardown discipline
// (Close is idempotent via closeOnce; the reader has already exited on a dead
// instance) and only reaps once ActiveTurns() drains to zero, so a peer turn's
// reader is never raced by the recycle path.
func (b *BrokerState) EnsureAppServer(ctx context.Context) (*appserver.AppServer, error) {
	b.appserverMu.Lock()
	defer b.appserverMu.Unlock()
	if b.appserver != nil && !b.appserver.IsDead() {
		return b.appserver, nil
	}
	if b.appserver != nil {
		// Detach the dead instance and reap it off the lock once its peer turns
		// finish draining; never Close it inline under appserverMu.
		dead := b.appserver
		b.appserver = nil
		go reapInstance(dead)
	}
	factory := b.appserverFactory
	if factory == nil {
		factory = defaultAppServerFactory
	}
	a := factory(b.CWD)
	if err := a.Spawn(ctx); err != nil {
		return nil, err
	}
	b.appserver = a
	return a, nil
}

// reapInstance drains any still-referenced turns on a detached dead instance,
// then Closes it (reaps the child + joins the reader). Run in a background
// goroutine from EnsureAppServer (so the caller never blocks under appserverMu)
// and synchronously from CloseAppServer (so explicit teardown is complete on
// return). Waiting for ActiveTurns() to drain to zero before Close is what
// prevents the recycle/shutdown path from racing a peer turn's reader: on codex
// death the reader's failPendingRequests has already removed every turn, so this
// usually returns immediately; the bounded poll only guards the rare
// turn-mid-teardown interleaving. Close is idempotent (closeOnce), so a
// background reap and a later CloseAppServer touching the same instance is safe.
func reapInstance(a *appserver.AppServer) {
	if a == nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.ActiveTurns() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_ = a.Close(context.Background())
}

// CloseAppServer tears down the singleton if present. Called by the broker
// idle-out and shutdown paths. The instance is detached under the lock and
// reaped off it (drain peer turns, then Close) synchronously, so callers that
// expect the child gone on return — broker shutdown, test cleanup — get that
// guarantee, while the lock is never held across the multi-second child wait.
func (b *BrokerState) CloseAppServer(_ context.Context) {
	b.appserverMu.Lock()
	dead := b.appserver
	b.appserver = nil
	b.appserverMu.Unlock()
	reapInstance(dead)
}
