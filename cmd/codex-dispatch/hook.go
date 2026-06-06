package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/semanta-dev/codex-dispatch/internal/broker"
	"github.com/semanta-dev/codex-dispatch/internal/codex"
)

type hookContext struct {
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	// StopHookActive is set by Claude Code when the Stop hook is re-invoked
	// because a previous Stop hook already blocked. Honoring it prevents an
	// infinite block loop: once active, the Stop hook must not block again.
	StopHookActive bool `json:"stop_hook_active"`
}

// hookRPCTimeout bounds a single broker RPC made from a session hook so a
// hung-but-reachable broker port cannot stall the Claude session hook (which
// the harness gives ~60s) — the call is cancelled and the hook continues.
const hookRPCTimeout = 2 * time.Second

// hookCtx returns a context bounded by hookRPCTimeout. Every broker RPC issued
// from a hook uses it so a wedged broker is abandoned promptly. The caller must
// invoke the returned cancel func.
func hookCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), hookRPCTimeout)
}

func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "codex-dispatch: hook requires exactly one event name (session-start | stop | session-end)")
		return 64
	}

	if os.Getenv("CODEX_DISPATCH_DISABLE_HOOKS") != "" {
		return emitContinue(stdout)
	}
	if args[0] == "stop" && os.Getenv("CODEX_DISPATCH_DISABLE_HOOK_STOP") != "" {
		return emitContinue(stdout)
	}

	var ctx hookContext
	body, _ := io.ReadAll(stdin)
	_ = json.Unmarshal(body, &ctx)

	switch args[0] {
	case "session-start":
		return runHookSessionStart(ctx, stdout, stderr)
	case "stop":
		return runHookStop(ctx, stdout, stderr)
	case "session-end":
		return runHookSessionEnd(ctx, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "codex-dispatch: unknown hook event %q\n", args[0])
		return 64
	}
}

func emitContinue(stdout io.Writer) int {
	fmt.Fprintln(stdout, `{"continue":true}`)
	return 0
}

func runHookSessionStart(ctx hookContext, stdout, stderr io.Writer) int {
	client, err := dialBrokerForHook(ctx.Cwd)
	if err != nil {
		return emitContinue(stdout)
	}
	defer client.Close()
	rpcCtx, cancel := hookCtx()
	defer cancel()
	if err := client.SessionRegister(rpcCtx, ctx.SessionID, ctx.Cwd); err != nil {
		if os.Getenv("CODEX_DISPATCH_DEBUG") != "" {
			fmt.Fprintf(stderr, "codex-dispatch: hook session-start: %v\n", err)
		}
	}
	return emitContinue(stdout)
}

func runHookStop(ctx hookContext, stdout, stderr io.Writer) int {
	// If a previous Stop hook already blocked, Claude re-invokes this hook with
	// stop_hook_active=true. Re-blocking would loop forever, so allow the stop.
	if ctx.StopHookActive {
		return emitContinue(stdout)
	}
	client, err := dialBrokerForHook(ctx.Cwd)
	if err != nil {
		return emitContinue(stdout)
	}
	defer client.Close()
	rpcCtx, cancel := hookCtx()
	defer cancel()
	tasks, err := client.TaskList(rpcCtx, ctx.SessionID)
	if err != nil {
		if os.Getenv("CODEX_DISPATCH_DEBUG") != "" {
			fmt.Fprintf(stderr, "codex-dispatch: hook stop: %v\n", err)
		}
		return emitContinue(stdout)
	}
	var running []string
	for _, task := range tasks {
		if task.State == "running" || task.State == "queued" {
			running = append(running, fmt.Sprintf("%s (%s)", task.TaskID, task.State))
		}
	}
	if len(running) == 0 {
		return emitContinue(stdout)
	}
	// Block-and-warn: a Stop "block" decision pairs with "reason" (shown to
	// Claude) and must OMIT continue/stopReason — emitting both is not a valid
	// Stop-hook decision and Claude ignores the conflicting fields. systemMessage
	// is an independent advisory line and is allowed alongside decision/reason.
	out := map[string]any{
		"decision":      "block",
		"reason":        fmt.Sprintf("%d codex tasks still active: %s", len(running), strings.Join(running, ", ")),
		"systemMessage": "Run `/codex --list` to see them, `/codex --cancel <id>` to stop, or wait for completion.",
	}
	raw, _ := json.Marshal(out)
	fmt.Fprintln(stdout, string(raw))
	return 0
}

func runHookSessionEnd(ctx hookContext, stdout, stderr io.Writer) int {
	client, err := dialBrokerForHook(ctx.Cwd)
	if err != nil {
		return emitContinue(stdout)
	}
	defer client.Close()
	rpcCtx, cancel := hookCtx()
	defer cancel()
	if _, err := client.SessionDeregister(rpcCtx, ctx.SessionID, true); err != nil {
		if os.Getenv("CODEX_DISPATCH_DEBUG") != "" {
			fmt.Fprintf(stderr, "codex-dispatch: hook session-end: %v\n", err)
		}
	}
	return emitContinue(stdout)
}

func dialBrokerForHook(cwd string) (*broker.Client, error) {
	if addr := os.Getenv("CODEX_DISPATCH_BROKER_ADDR"); addr != "" {
		return broker.Dial(addr)
	}
	// CODEX_DISPATCH_BROKER_SOCKET is a legacy test-only override retained for
	// older tests that plug the line protocol directly into a Unix socket.
	if sock := os.Getenv("CODEX_DISPATCH_BROKER_SOCKET"); sock != "" {
		c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
		if err != nil {
			return nil, err
		}
		return broker.NewClientFromConn(c), nil
	}
	// codex.ResolveBrokerAddr is the single shared repo-root + broker.addr
	// resolver (honors CODEX_BROKER_ADDR_PATH; a linked worktree's .git FILE is
	// not treated as the root). An empty cwd falls back to the broker's getwd.
	addrPath, err := codex.ResolveBrokerAddr(cwd)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(addrPath)
	if err != nil {
		return nil, err
	}
	return broker.Dial(strings.TrimSpace(string(raw)))
}
