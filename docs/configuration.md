# Configuration reference

Complete reference for the environment variables, exit codes, and commands that
configure `codex-dispatch`. The README links here from its Quickstart,
Environment variables, and Exit codes sections.

- [Environment variables](#environment-variables)
- [Exit codes](#exit-codes)
- [Command decision guide](#command-decision-guide)
- [Troubleshooting](#troubleshooting)

Every variable below is verified against the Go source under `internal/` and
`cmd/codex-dispatch/`, plus the launcher in `scripts/`. Nothing here is
aspirational — if a knob is listed, the binary or a script reads it.

## Environment variables

### Dispatch (the synchronous `/codex` run)

| Variable | Default | Effect |
|---|---|---|
| `CODEX_SANDBOX` | `danger-full-access` | Codex sandbox policy. One of `read-only`, `workspace-write`, `danger-full-access`. Any other value fails validation (exit `64`). |
| `CODEX_WORKDIR` | unset (auto-derived) | Directory codex should treat as its thread cwd. Used as-is if absolute, else resolved relative to the invocation cwd. Lets you pin a dispatch to a module subdirectory of a `go.work` monorepo (one git repo at the parent, modules like `./shared ./server` beneath it) without changing process cwd. **When unset and the dispatch is invoked at the repo root, the module is auto-derived from `CODEX_FILES`** — codex runs in the nearest ancestor of all seeded files that carries a module manifest (`go.mod`, `package.json`, `pyproject.toml`, `Cargo.toml`, `composer.json`, `build.gradle`, `pom.xml`); files spanning two modules (or none with a sub-manifest) stay at the repo root. The broker is still keyed on the repo root (one broker per repo); only codex's per-thread cwd changes. A value outside the repo root falls back to the repo root. |
| `CODEX_MODEL` | unset (codex default) | Pin the codex model for the dispatch (e.g. `gpt-5.5`); passed through to the app-server `thread/start`. When unset, codex uses its configured default. Set this to match the model your `codex exec` uses when you need consistent behavior (e.g. image/vision reads). |
| `CODEX_DISPATCH_TIMEOUT_MS` | unset (no timeout) | Per-dispatch wall-clock budget in milliseconds. On timeout the run returns promptly with `exit_code: 124` recorded in `result.json`. A missing/zero/invalid value means no timeout. |
| `CODEX_DISPATCH_BIN` | unset | Escape hatch: absolute path to a prebuilt `codex-dispatch` binary. When set and executable, the launcher (`scripts/dispatch-codex.sh`) execs it directly and skips the download/checksum path. Used for local dev and CI. |
| `CODEX_DISPATCH_KEEP_MCP` | unset (MCP disabled) | By default a dispatched codex has its configured MCP servers **disabled**, so a dispatch focuses on the repository and cannot be derailed by a slow or failing MCP / web tool (a local MCP endpoint, playwright, etc.) before it reads the code. The broker enumerates codex's servers (`codex mcp list --json`) and disables each with `-c mcp_servers.<name>.enabled=false` when spawning the app-server. Set this to any non-empty value to leave MCP servers as configured. (Read at broker-spawn time — the first dispatch in a repo fixes it until the broker restarts.) |
| `CODEX_CONVENTIONS_FILE` | auto-detected | Path to a conventions file injected into Codex's prompt. When unset, the dispatch auto-detects one. When set but missing, the run warns and proceeds with a "conventions missing" tag. |
| `CODEX_FILES` | unset | Comma-separated relevant file paths seeded into the prompt. Relative paths resolve against the working directory; a missing path is warned about, not fatal. |
| `CODEX_TASK` | required | The natural-language task. Empty fails validation (exit `64`). Normally set by the slash command / subagent, not by hand. |
| `CODEX_ACCEPTANCE` | required | Acceptance criteria, one verifiable claim per line. Empty fails validation (exit `64`). |
| `CODEX_CONSTRAINTS` | unset | Out-of-scope / don't-touch list appended to the default constraint set. |
| `CODEX_FEEDBACK` | unset | Reviewer feedback fed back into the next iteration's prompt. |
| `CODEX_SESSION_ID` | unset | Codex thread/session id to resume. When set, the dispatch resumes that thread instead of starting fresh (with automatic fall-back to fresh if the thread is stale). |
| `CODEX_RESULT_DIR` | `<repo>/.codex-dispatch/runs/<ts>-<pid>/` | Override the run-artifact directory. When set, the directory is created if absent and reused if present. |
| `CODEX_DISPATCH_DEBUG` | unset | When non-empty, the hook path emits extra diagnostics to stderr. |

### Broker (the per-working-tree background process)

The broker auto-starts on first use, owns one long-lived `codex app-server`
child per working tree, and self-exits when idle.

| Variable | Default | Effect |
|---|---|---|
| `CODEX_BROKER_IDLE_MS` | `300000` (5 min) | Idle timeout before the broker self-exits. Idle-out is guarded: the broker re-arms while any task is queued/running and resets the clock on every streamed codex notification, so a long/quiet turn is never killed mid-flight. |
| `CODEX_BROKER_ADDR_PATH` | `<repo>/.codex-dispatch/broker.addr` | Path to the broker address file. Absolute paths are used as-is; relative paths resolve under the repo root. |
| `CODEX_BROKER_MAX_CONCURRENT` | `8` | Maximum simultaneously running tasks. Values `<= 0`, invalid, or unset fall back to the default. |
| `CODEX_BROKER_RING_SIZE` | `2048` | Per-task event ring buffer size (how many notifications are retained for replay). |
| `CODEX_BROKER_TURN_TIMEOUT_MS` | unset (off) | Per-turn deadline. A wedged turn that never reaches `turn/completed` is interrupted once this elapses, freeing the run slot. `<= 0` or unset disables it. |
| `CODEX_BROKER_TASK_TTL_MS` | unset (off) | Time-to-live for terminal (done/cancelled/errored) tasks in the task table before eviction. `<= 0` or unset disables it. |
| `CODEX_BROKER_MAX_TERMINAL_TASKS` | `1024` | Cap on retained terminal tasks. `0` disables the cap (unbounded); negative/invalid falls back to the default. |
| `CODEX_BROKER_CODEX_BIN` | `codex` on `$PATH` | Override the `codex` binary the broker spawns for `codex app-server`. |
| `CODEX_DISPATCH_BROKER_ADDR` | unset | Hook-side override: connect to a broker at this address directly. |
| `CODEX_DISPATCH_BROKER_SOCKET` | unset | Legacy **test-only** hook-side override (socket path); retained for backward compatibility — prefer `CODEX_DISPATCH_BROKER_ADDR`. |

### Picker (iteration-count selection)

The picker chooses a max-iteration count in `[floor, ceiling]`, preferring an
LLM estimate when available, otherwise a deterministic score from task length
and acceptance-criteria line count. It always returns a positive integer
(fails closed).

| Variable | Default | Effect |
|---|---|---|
| `PICK_FLOOR` | `2` | Lower bound on iterations. Values `<= 0` or invalid fall back to `2`. |
| `PICK_CEILING` | `5` | Upper bound on iterations. Values `<= 0` or invalid fall back to `5`. |
| `PICK_DISABLE_LLM` | unset | When non-empty, skip the LLM estimate and use the deterministic score only. |
| `PICK_MODEL` | `claude-haiku-4-5-20251001` | Model used for the LLM iteration estimate. |
| `PICK_TASK` | unset | Task text the picker scores (set by the orchestrator). |
| `PICK_ACCEPTANCE` | unset | Acceptance text the picker scores (set by the orchestrator). |
| `ANTHROPIC_API_KEY` | unset | Presence enables the Claude-backed iteration estimate; absence forces the deterministic path. |

### Hooks (Claude Code session integration)

Three hooks (`SessionStart`, `Stop`, `SessionEnd`) register sessions with the
broker and surface running detached tasks before Claude wraps up.

| Variable | Default | Effect |
|---|---|---|
| `CODEX_DISPATCH_DISABLE_HOOKS` | unset | When non-empty, disables all three hooks. |
| `CODEX_DISPATCH_DISABLE_HOOK_STOP` | unset | When non-empty, disables only the `Stop` hook (keeps `SessionStart`/`SessionEnd`). |
| `CLAUDE_SESSION_ID` | set by hook | The Claude session id used to associate dispatches with a session. When absent, a collision-free fallback id is derived per dispatch. |

### GraphRAG plan runner (`scripts/graphrag-plan-runner.py`)

| Variable | Default | Effect |
|---|---|---|
| `GRAPHRAG_DISPATCH_ATTEMPTS` | `2` | Per-packet retry count for `--isolation worktree`. Failed `result.json.exit_code` attempts are discarded and retried in a fresh worktree; only a clean attempt is fanned in. |
| `GRAPHRAG_SCOPE_AUDIT_IGNORE` | unset | Extra ignore patterns for the allowed-file scope audit, layered on top of `.gitignore`. |
| `GRAPHRAG_WORKTREE_KEEP` | unset | When set, keep the temporary worktree after a `--isolation worktree` dispatch for debugging. |

## Exit codes

Two layers return exit codes: the **launcher** (`scripts/dispatch-codex.sh`,
which resolves/downloads the binary) and the **binary** itself
(`codex-dispatch dispatch`). They use disjoint code ranges so a failure is
unambiguous.

### Binary (`codex-dispatch dispatch`)

| Code | Meaning | Source |
|---|---|---|
| `0` | OK. Validation passed and the dispatch completed; codex's own turn result lives in `result.json` (`result.json.exit_code` may still be non-zero). | `internal/dispatch/run.go` |
| `2` | Not usable as a git repo: cwd is not inside a git repository, **or** the repository has no commits yet (unborn branch — commit once first). | `ErrNotInGitRepo` / `ErrEmptyRepo`, `internal/dispatch/validate.go` |
| `3` | `codex` binary not found on `$PATH`. | `ErrCodexNotFound`, `internal/dispatch/validate.go` |
| `64` | Usage error: missing `CODEX_TASK` / `CODEX_ACCEPTANCE`, an invalid `CODEX_SANDBOX` value, a missing subcommand, or a bad dispatch flag. | `ErrUsage`, `internal/dispatch/validate.go` and `cmd/codex-dispatch/main.go` |

Note: `exit_code: 4` is **not** a process exit code from the binary. It is a
value written **into `result.json`** when a codex turn completes successfully
(`0`) but produced no meaningful repository edits
(`error_message: "codex completed without meaningful repository edits"`). The
binary process still exits `0` in that case. See
[run artifacts](../README.md#run-artifacts).

`result.json.exit_code` (distinct from the process exit code) uses: `0` ok,
`2` codex turn failed, `4` no meaningful edits, `64` broker/turn error, `124`
dispatch timed out / cancelled (`CODEX_DISPATCH_TIMEOUT_MS`).

`result.json.files_changed_outside_seed` (added v0.3.3, `omitempty`) lists changed
files not covered by the `CODEX_FILES` seed — a scope-creep signal the reviewer
consumes. It is omitted when no seed was given or every change falls within it, so
seed-less / in-scope runs keep their original byte shape.

### Launcher (`scripts/dispatch-codex.sh`)

These cover only the trust boundary (download + checksum verification) and are
distinct from the binary's `0/2/3/64`.

| Code | Meaning |
|---|---|
| `5` | Checksum mismatch (or `checksums.txt` has no entry for the platform archive). |
| `6` | Required tool missing (`tar` — or `unzip` on Windows —, `curl`/`wget`, `sha256sum`/`shasum`), unsupported OS/arch, or missing `VERSION` file. |
| `7` | Network unreachable while downloading the archive or `checksums.txt`. |
| `8` | Tarball corrupt despite a matching checksum (extraction failed or the archive is missing the binary). |

The other helper scripts (`pick-iterations.sh`, `capture-diff.sh`) exit `6`
when their required tooling is missing.

`clean-verify.sh` (used by `--clean-verify`) runs a verify command against HEAD +
a run's `diff.patch` in a throwaway `git worktree`. It propagates the verify
command's own exit code, and otherwise: `2` usage error, `6` git missing or
`run_dir` not found, `65` the diff did not apply cleanly to HEAD (the change
depends on uncommitted/gitignored state — itself a signal). Set
`CLEAN_VERIFY_KEEP=1` to keep the worktree for debugging.

## Command decision guide

| You want to... | Use |
|---|---|
| Run one well-scoped task | `/codex <task>` |
| Let Claude pick the best route (unsure which) | `/codex-orchestrate <task-or-plan>` |
| Have Codex write a GraphRAG spec + packet plan | `/graphrag-codex-plan <feature>` |
| Run a single packet from an existing plan | `/graphrag-codex <plan> [packet]` |
| Run a whole multi-packet plan | `/graphrag-codex-run <plan> [--jobs N] [--isolation none\|worktree]` |

`/graphrag-codex-run` defaults to `--isolation none`: every packet runs in the
single working tree on the current branch, made safe by a pre-flight
allowed-file overlap partition plus per-file locks. `--isolation worktree` is
the opt-in fallback for packets whose verification needs a fully isolated tree.

## Troubleshooting

### Dispatch fails on the app-server handshake / "codex >= 0.130.0 required"

codex-dispatch speaks the codex app-server protocol introduced in `0.130.0`, so
an older `codex` fails the handshake. The minimum is pinned as `MinCodexVersion`
in `internal/codex/appserver/appserver.go` (with a `CheckCodexVersion` helper);
wiring that helper into a single fail-fast pre-flight error is a tracked
follow-up — for now, confirm your version yourself before dispatching:

```bash
codex --version   # must report >= 0.130.0
```

### "codex binary not found" (exit 3)

`codex` is not on `$PATH`. Install the [codex CLI](https://github.com/openai/codex)
and confirm `command -v codex` resolves. If it lives in a non-standard
location, point the broker at it with `CODEX_BROKER_CODEX_BIN`.

### Broker won't start / stale `broker.addr`

The broker writes `<repo>/.codex-dispatch/broker.addr` and a `broker.pid`
lock. If a previous broker died uncleanly, the addr file can point at a dead
endpoint. The dispatch path auto-heals this: it pings the existing addr, and if
unreachable removes the stale file and respawns. To force a clean restart
manually:

```bash
codex-dispatch dispatch --list                 # probe; auto-starts a broker if needed
rm -f .codex-dispatch/broker.addr .codex-dispatch/broker.pid   # then re-dispatch
```

If you relocated the addr file with `CODEX_BROKER_ADDR_PATH`, remove that path
instead. The broker self-exits after `CODEX_BROKER_IDLE_MS` (5 min) of idle.

### Codex sandbox errors

By default dispatch uses `CODEX_SANDBOX=danger-full-access` to avoid codex CLI
sandbox failures in plugin launches. If you set a stricter policy and hit
sandbox-denied errors, the valid values are `read-only`, `workspace-write`,
and `danger-full-access`; anything else fails validation with exit `64`.

Codex's Linux sandbox uses **bubblewrap**, which needs unprivileged user
namespaces. On hosts that restrict them — e.g. Ubuntu with
`kernel.apparmor_restrict_unprivileged_userns=1` — a `read-only` or
`workspace-write` run cannot start the sandbox and every shell command inside a
turn fails with `bwrap: setting up uid map: Permission denied`. To avoid burying
that in per-command output, the broker runs a one-shot `command/exec` preflight
before starting the turn: when the requested sandboxed mode can't initialize, the
dispatch **fails fast** with exit `64` and an actionable message. The default
`danger-full-access` never invokes bubblewrap, so it is skipped by the preflight
and unaffected. Fixes: use `danger-full-access`, or lift the host restriction
with `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`.

### Network failure during binary download (exit 7)

The launcher downloads the `codex-dispatch` binary from GitHub Releases on
first use. If the network is unreachable, install offline: download
`codex-dispatch_<os>-<arch>.tar.gz` (`.zip` on Windows) and `checksums.txt` from the
[release page](https://github.com/semanta-dev/codex-dispatch/releases),
place them in
`${XDG_CACHE_HOME:-$HOME/.cache}/codex-dispatch/v<VERSION>/manual/`, then
re-run. Or set `CODEX_DISPATCH_BIN` to a prebuilt binary to bypass the
download entirely.

### A run reports `exit_code: 4` (no meaningful edits)

The codex turn completed but touched no repository files (it may have only
produced explanatory text or runtime-only artifacts). Tighten the task and
acceptance criteria, or supply relevant `--files`, and re-dispatch. This is a
`result.json` value, not a process exit code; detached runs never report it
(no diff is computed).
### Codex's edits vanished after I aborted a run

This is codex's turn model, not codex-dispatch reverting your tree: the dispatch
core never runs `git clean`/`checkout`/`restore`/`reset`/`stash` against the
working tree (only the opt-in `--isolation worktree` and `--clean-verify` paths
touch git, and they operate on separate worktree directories). Codex treats a
**turn atomically** — when you abort, codex-dispatch sends `turn/interrupt` and
codex rolls back the in-progress turn, undoing the files it was creating.

The work is recoverable: every run's `stdout.log` records the turn's
`turn/diff/updated` events, the last of which is a full unified diff. Recover it
with:

```bash
log=$(ls -t .codex-dispatch/runs/*/stdout.log | head -1)   # newest run
grep '"method":"turn/diff/updated"' "$log" | tail -1 \
  | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['params']['diff'])" \
  | git apply
```

To avoid the rollback, let the turn complete instead of aborting (completion is
what persists the diff).

### Untracked files disappeared during parallel / multi-agent work

A codex turn (completed *or* aborted) does **not** delete a bystander's untracked
or uncommitted files — this was verified empirically: an untracked file and an
uncommitted edit both survive a concurrent dispatch in the same tree. So when a
peer's untracked work vanishes during parallel agents, codex-dispatch is not the
cause. The real culprits are:

- **Claude Code git worktrees.** A subagent running under worktree isolation
  (`Agent` with `isolation: worktree`, the `EnterWorktree` tool, or the
  `parallel-subagents` skill) writes into `.claude/worktrees/…`. A `Write` there
  succeeds but the files are not in your main checkout, and are removed when the
  worktree is torn down.
- **A direct `git` command** (`git clean -fd`, `git checkout`, `git reset
  --hard`, `git stash`) run by some agent via Bash — these wipe untracked files
  from any source.

Untracked files are unprotected against all of the above. The durable guard is to
**commit (or `git add`) work before any concurrent or destructive operation** —
committed files survive `git clean` and worktree teardown — and to coordinate
parallel agents (e.g. claim → commit → release) rather than co-write a shared
tree with uncommitted state.

To hard-block worktree creation in Claude Code entirely (so no agent or skill can
spin one up), add to `~/.claude/settings.json` (global) or a project
`.claude/settings.json`:

```json
{
  "permissions": {
    "deny": ["EnterWorktree", "Agent(isolation:worktree)"]
  },
  "hooks": {
    "WorktreeCreate": [
      { "hooks": [ { "type": "command",
        "command": "echo 'git worktrees are disabled by user policy' >&2; exit 1" } ] }
    ]
  }
}
```

The `permissions.deny` rules remove `EnterWorktree` from Claude's context and
reject any subagent requesting worktree isolation; the `WorktreeCreate` hook is
the catch-all backstop (any non-zero exit aborts creation). Normal and parallel
subagent dispatch is unaffected — only worktree isolation is blocked, and
subagents run in the shared working tree on the current branch.
