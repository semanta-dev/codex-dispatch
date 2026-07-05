# codex-dispatch

A Claude Code plugin that delegates well-scoped coding tasks to the local OpenAI Codex CLI and reviews Codex's output through a structured verdict loop.

> **Status:** v0.3.3. The dispatch core is a single Go binary (`codex-dispatch`) distributed via GitHub Releases; the shell scripts are thin launchers that resolve and exec the binary. New dispatch planning should use the GraphRAG packet workflow under `docs/graphrag/plans/`; the old Superpowers planning docs are historical reference only and are not required at runtime.

## Quickstart

Zero to a first dispatch in under five minutes.

**1. Enable the plugin (local clone).** From a clone of this repo, register it as a local Claude Code plugin and restart Claude Code so the manifest, commands, and subagents load:

```bash
claude plugin add ./codex-dispatch   # path to your clone
# then restart Claude Code
```

(Use whichever local-plugin install method your Claude Code version documents; the requirement is that this directory's `.claude-plugin/plugin.json` is picked up.)

**2. Sanity-check codex.** The [codex CLI](https://github.com/openai/codex) must be on `$PATH` at **version 0.130.0 or later** — that release introduced the app-server protocol the broker speaks, and an older codex fails the handshake. Verify before your first dispatch:

```bash
codex --version   # must report >= 0.130.0
```

**3. Run your first dispatch.** From inside any git repo with at least one commit:

```text
/codex add a --json flag to cmd/list that prints the results as a JSON array
```

`/codex` derives acceptance criteria, asks Codex to implement, reviews the diff with a Claude subagent, and iterates until it passes or hits the iteration cap.

**4. Read the result.** A successful run ends with a block like:

```text
PASS after 2 iterations                         # verdict + how many dispatch→review loops it took
files changed: cmd/list.go, cmd/list_test.go    # files Codex actually touched (baseline-filtered)
run artifacts: .codex-dispatch/runs/20260602T141133Z-48217/   # result.json, diff.patch, prompt.txt, stdout.log
edits are in the working tree — review, then commit when you're happy   # the plugin never commits for you
```

Codex's edits land directly in your working tree. The plugin never commits, branches, or pushes — you own the next move.

**Knobs and codes:** every environment variable, exit code, command, and troubleshooting step is in [`docs/configuration.md`](docs/configuration.md). Highlights are summarized in [Environment variables](#environment-variables), [Exit codes](#exit-codes), and [Troubleshooting](#troubleshooting) below.

## Which command should I use?

| You want to... | Use |
|---|---|
| Run one well-scoped task | `/codex <task>` |
| Let Claude pick the best route (unsure which) | `/codex-orchestrate <task-or-plan>` |
| Have Codex write a GraphRAG spec + packet plan | `/graphrag-codex-plan <feature>` |
| Run a single packet from an existing plan | `/graphrag-codex <plan> [packet]` |
| Run a whole multi-packet plan | `/graphrag-codex-run <plan> [--jobs N]` |

## What it does

- `/codex <task>` — slash command that dispatches Codex on a task, reviews the diff with a Claude subagent, and iterates until the work passes review or the iteration cap is hit.
- `/codex-orchestrate <task-or-plan>` — route selector that lets Claude choose Codex-authored GraphRAG spec/plan drafting, `/codex`, one GraphRAG packet, full-plan fanout, shared-broker cost mode, or direct Claude handling based on task shape.
- `/graphrag-codex-plan <feature>` — dispatches Codex to write GraphRAG spec and packet plan docs under `docs/graphrag/`, then uses Claude review before any implementation runs.
- `/graphrag-codex <plan> [packet]` — GraphRAG packet bridge that executes exactly one `graphrag-codex-planner` packet through the same Codex implementation loop.
- `/graphrag-codex-run <plan> [--jobs N] [--isolation none|worktree]` — plan-level GraphRAG runner that schedules independent packets in the single working tree by default (`--isolation none`, overlap-partitioned with per-file locks), verifies each packet, and writes a JSON execution ledger. `--isolation worktree` is the opt-in isolated-tree fallback.
- `codex-dispatch` subagent — autonomous route for the main Claude agent to delegate self-contained implementation tasks to Codex.
- `graphrag-codex-dispatch` subagent — packet-oriented route that preserves GraphRAG allowed-file, verification, and progress-record rules.
- `codex-reviewer` subagent — structured pass / needs-changes / fail verdicts that drive the iteration loop and decide whether to resume Codex's session or start fresh.

All surfaces share a single shell dispatch core (`scripts/dispatch-codex.sh`) and a per-run artifact contract.

## Prerequisites

- A POSIX shell environment: Linux (x86_64/arm64), macOS (amd64/arm64), or Windows (amd64/arm64) via Git Bash / MSYS2.
- `git` — the working directory must be a git repository.
- [`codex`](https://github.com/openai/codex) CLI **version 0.130.0 or later** installed and on `$PATH` (`codex --version` must report `≥ 0.130.0`).
- `tar`, `curl` (or `wget`), and `sha256sum` (or `shasum`) — used by the launcher to download and verify the `codex-dispatch` binary on first use.
- Claude Code (the plugin runs as a Claude Code plugin).
- Optional: [`bats-core`](https://github.com/bats-core/bats-core), `shellcheck`, and Go 1.22+ to run the test and lint suite locally.

## Installation

Until the plugin is published, install it from a local clone:

1. Clone this repo.
2. Add it as a local plugin in Claude Code (see Claude Code's plugin docs for your install method).
3. Restart Claude Code so the manifest, commands, and subagents are picked up.

For project-scoped GraphRAG dispatch, install or enable only this plugin plus the
GraphRAG workflow plugin/skills you use for planning and review. Superpowers is
not required for dispatch and can be disabled per project:

```bash
claude plugin disable --scope project superpowers@claude-plugins-official
```

## Slash command

```
/codex <task>
/codex-orchestrate [--prefer speed|cost|quality] [--plan-first] [--dry-run] <task-or-plan-path>
/graphrag-codex-plan [--spec-only|--plan-only] <feature-or-change-request>
/codex --max-iter 5 <task>
/codex --acceptance "X must Y; Z must W" <task>
/codex --files src/foo.ts,src/bar.ts <task>
/codex --constraints "don't touch tests/" <task>
/codex --no-tests <task>
/codex --test-cmd "make test" <task>
/codex --no-resume <task>
```

Flags:

| Flag | Purpose |
|---|---|
| `--max-iter N` | Iteration cap. If unset, the plugin asks a small Claude model to pick a number in `[2, 5]`. |
| `--acceptance "..."` | Explicit acceptance criteria. If unset, Claude derives them from the task. |
| `--files a,b,c` | Comma-separated relevant file paths to seed Codex's prompt. |
| `--constraints "..."` | Out-of-scope or don't-touch list, appended to the default constraint set. |
| `--no-tests` | Skip the reviewer's unit-test step. |
| `--test-cmd "..."` | Override the auto-detected **unit** test command. |
| `--verify-cmd "..."` | An **integration/e2e** command that exercises the runtime behavior the acceptance criteria describe (distinct from unit tests). When acceptance criteria describe runtime/flow/deploy behavior and no `--verify-cmd` ran, the reviewer withholds `pass` (`verification-insufficient`) rather than trusting unit tests alone. |
| `--clean-verify` | Run `--verify-cmd` in a throwaway `git worktree` (HEAD + Codex's diff) so it can't pass by reading dirty-tree or gitignored state. |
| `--no-resume` | Always start a fresh Codex session per iteration; never resume. |

### Route selection

Use `/codex-orchestrate` when you want Claude to choose the execution route:

```text
/codex-orchestrate --dry-run docs/graphrag/plans/feature.plan.md
/codex-orchestrate --plan-first "add offline save support"
/codex-orchestrate --prefer quality "add offline save support"
/codex-orchestrate --prefer speed docs/graphrag/plans/feature.plan.md
/codex-orchestrate --prefer cost docs/graphrag/plans/feature.plan.md
/codex-orchestrate --files src/foo.ts --test-cmd "npm test" implement validation
```

The route selector uses these defaults:

- spec/plan authoring request or `--plan-first` → Codex-authored GraphRAG
  spec/plan draft, reviewed by Claude;
- `--prefer quality` on non-trivial work with no plan → Codex-authored
  GraphRAG spec/plan draft first;
- plain scoped implementation task → `/codex`;
- one selected GraphRAG packet → `/graphrag-codex`;
- multi-packet GraphRAG plan → `/graphrag-codex-run <plan> --jobs N` (single-tree `--isolation none` by default);
- `--prefer cost` on a plan → adds `--shared-broker`;
- exploratory design, review, or unsafe broad changes → direct Claude route instead of dispatch.

### Codex-authored specs and plans

Use `/graphrag-codex-plan` when you want Codex to do the planning and spec
writing before implementation:

```text
/graphrag-codex-plan add offline save support
/graphrag-codex-plan --spec-only redesign project settings
/graphrag-codex-plan --plan-only implement docs/graphrag/specs/2026-05-28-settings.md
```

The command dispatches Codex through `codex-dispatch` with `TEST POLICY=skip`
and strict acceptance criteria that allow only `docs/graphrag/` planning files.
Claude's `codex-reviewer` then reviews the diff for packet shape, scope, and
planning quality. It does not execute the generated packets; run
`/graphrag-codex` or `/graphrag-codex-run` only after the reviewed plan is
accepted.

### Background tasks (since v0.3.0)

The plugin runs a small broker process per repo working tree. `/codex` is sync by default, but four opt-in flags expose background tasks:

| Flag | Effect |
|---|---|
| `--detach` | Start the dispatch in the background; print `task_id` and return immediately. |
| `--status <id>` | Print the current state as JSON. |
| `--cancel <id>` | Cancel a running or queued task. |
| `--list` | Print all tasks the broker knows about. |

The broker auto-starts on first use and self-exits after 5 minutes of inactivity (`CODEX_BROKER_IDLE_MS` to override). Idle-out is **guarded**: the broker refuses to idle-out (and re-arms the timer) while any task is queued or running, and every streamed codex notification resets the idle clock — so a long or quiet turn is never killed mid-flight, even with a small `CODEX_BROKER_IDLE_MS`. Concurrency cap defaults to 8 simultaneous tasks (`CODEX_BROKER_MAX_CONCURRENT`).

#### Detached task contract (`--status`)

A detached (`--detach`) task is driven by a broker-owned background goroutine that is bound to the broker lifecycle: its context derives from the broker-lifetime context, so on broker shutdown or idle-out the broker **cancels and drains** in-flight detached runs (interrupting the turn and freeing the run slot) before tearing down the shared `codex app-server` child — a detached run is never orphaned or yanked mid-turn.

Unlike a synchronous `/codex` run, a detached task does **not** write a `result.json` or compute a filtered `diff.patch`; its durable record is the streaming `stdout.log` plus the task table. Its observable contract is the `--status <id>` JSON returned by the broker's `task.status`:

| Field | When present | Meaning |
|---|---|---|
| `task_id` | always | broker task id |
| `state` | always | `queued` \| `running` \| `done` \| `cancelled` \| `errored` |
| `started_at` | always | RFC3339 (zero until the task starts running) |
| `event_count` | always | notifications streamed so far |
| `fell_back_to_fresh` | always | a stale `resume` retried as a fresh thread |
| `finished_at` | terminal states | RFC3339 completion time |
| `exit_code` | terminal states | codex turn exit code (`0` ok, `2` failed, `64` broker/turn error). `4` (no meaningful edits) **never** occurs for detached runs — no diff is computed, so the no-edits gate cannot fire. |
| `session_id` | once a thread exists | codex thread/session id |
| `error_message` | on broker/turn failure | machine-readable failure reason (the field a detached observer reads in place of `result.json.error_message`) |

`files_changed` and `lines_*` are **not** available for detached runs (no diff is computed); use a synchronous dispatch or read `stdout.log` if you need the touched-file set.

Three Claude Code hooks (`SessionStart`, `Stop`, `SessionEnd`) register sessions with the broker; the `Stop` hook surfaces running detached tasks before Claude wraps up. Disable with `CODEX_DISPATCH_DISABLE_HOOKS=1` (all three) or `CODEX_DISPATCH_DISABLE_HOOK_STOP=1` (just Stop).

## GraphRAG packet workflow

For projects using the GraphRAG planning skills, use `/graphrag-codex` instead of raw `/codex`:

```
/graphrag-codex docs/graphrag/plans/2026-05-16-feature.plan.md
/graphrag-codex docs/graphrag/plans/2026-05-16-feature.plan.md 002
```

The command delegates to `graphrag-codex-dispatch`, which reads one packet from a `graphrag-codex-planner` plan and translates it into the stricter `codex-dispatch` input contract.

Packet execution rules:

- Executes exactly one packet.
- Requires `Allowed files`, `Acceptance criteria`, `Verification`, and `Progress record` sections.
- Passes the packet's allowed files and disallowed changes into the Codex prompt as hard constraints.
- Requires Codex to write the packet `.done.md` progress record, or stop with a clear failure if the packet cannot be completed within scope.
- Audits tracked and untracked files against `Allowed files`, ignoring `.gitignore` matches plus low-risk untracked runtime/cache artifacts such as `.codex-dispatch/`, dependency folders, temp directories, caches, and bytecode. Broad build directories are policy-driven via `.gitignore` or `GRAPHRAG_SCOPE_AUDIT_IGNORE`.
- For full-plan orchestration, `scripts/graphrag-plan-runner.py <plan> --out <dir> --jobs <N>` parses packet dependencies and runs unblocked packets. **The default is single-tree (`--isolation none`):** every packet dispatches directly in the current working tree on your feature branch, made safe by a deterministic, pre-flight allowed-file overlap partition plus per-file locks — no git worktrees are created. It verifies each accepted packet, writes missing progress records itself, and writes `<dir>/ledger.json` for GraphRAG review/ingestion. Use `--context-mode full` only when packet `Inputs` must be embedded in the worker prompt; the default `targets` mode keeps prompts smaller. Use `--shared-broker` as a token/process-overhead optimization, not the default low-latency path.
- For the opt-in worktree fallback, pass `--isolation worktree`. The runner then routes each packet through `scripts/graphrag-worktree-dispatch.sh`, which executes it in an isolated temporary git worktree, audits scope there, and fans in only allowed paths. Use this when a packet's verification needs a fully isolated tree; the single-tree overlap partition is otherwise sufficient for correct parallel dispatch.
- `GRAPHRAG_DISPATCH_ATTEMPTS` controls packet retries under `--isolation worktree`. The default is `2`; failed underlying `result.json.exit_code` attempts are discarded and retried in a fresh worktree, and only a clean attempt is fanned in.
- Under `--isolation worktree`, the runner persists the selected attempt's `dispatch-result.json` and `dispatch-stdout.log` in the run directory so benchmarks can aggregate real worker metrics after temporary worktrees are removed.
- Benchmark helpers under `tests/sdk/` can aggregate persistent dispatch metrics and repeat a benchmark command for pass-rate plus p50/p95 wall-time comparisons.
- Reports the selected packet, changed files, verification command, progress record path, allowed-file audit, and next packet.

See [`docs/graphrag-workflow-integration.md`](docs/graphrag-workflow-integration.md) for the expected plan shape and recommended plan → execute → review loop.

## Subagent use

The main Claude agent can delegate to the `codex-dispatch` subagent autonomously for self-contained implementation work. The subagent runs the same dispatch → review → decide loop as the slash command but with stricter inputs and a structured return.

**Required inputs** (passed in the parent prompt as labeled fields):

- `TASK` — the natural-language task.
- `ACCEPTANCE CRITERIA` — one verifiable claim per line.

**Optional inputs:** `CONSTRAINTS`, `FILES`, `TEST POLICY` (`run` / `skip`), `TEST CMD`, `MAX ITER`, `NO RESUME`.

**Return shape** (JSON, last block of the response):

```json
{
  "status": "pass | fail",
  "iterations": 2,
  "files_changed": ["app/main.py"],
  "summary": "completed in 2 iterations; modified app/main.py",
  "final_feedback": ""
}
```

**Limitations:**

- Refuses to invent acceptance criteria. If `ACCEPTANCE CRITERIA` is missing or empty, returns `status: fail` with `summary: "underspecified: ..."` immediately.
- Never asks the user (or the parent agent) questions — the loop is automated.
- Never commits, branches, pushes, or reverts. Codex's edits land in the working tree; the parent agent owns the next move.
- Best for tasks with clear behavioral acceptance and well-bounded files. For exploratory work, design decisions, or refactors without behavioral checks, do the design first and dispatch the resulting concrete task.

## Run artifacts

Each **synchronous** invocation writes to `.codex-dispatch/runs/<timestamp>-<pid>/` (detached `--detach` runs write only `stdout.log` + the task table — see [Detached task contract](#detached-task-contract---status)):

| File | Contents |
|---|---|
| `result.json` | `exit_code`, `session_id`, `files_changed`, line counts, `stdout_path`, `diff_path`, `fell_back_to_fresh`, `error_message` (omitempty), `files_changed_outside_seed` (omitempty — changed files not covered by the `--files` seed; a scope-creep signal for the reviewer). |
| `stdout.log` | Codex app-server notification stream, one JSON object per line in `{method, params}` form: `thread/started`, `turn/started`, `item/started`, `item/completed`, `turn/completed`, etc. Broker-side failures appear as `broker/dispatch/error` lines. The `\n==== fell back to fresh dispatch ====\n` marker separates a stale-resume attempt from its fresh retry. |
| `diff.patch` | Working-tree diff filtered to files Codex touched, computed against the pre-run baseline. |
| `prompt.txt` | The prompt assembled and sent to Codex. |

The `.codex-dispatch/` directory is gitignored and excluded from changed-file attribution. Codex's edits land in your working tree directly — the plugin does not commit, branch, or revert.

A completed Codex turn that produces no meaningful repository edits is recorded as `exit_code: 4` with `error_message: "codex completed without meaningful repository edits"`. This prevents runtime-only artifacts or explanatory final text from being treated as successful implementation work.

## Safety constraints

- Codex runs as `codex app-server` (JSON-RPC v2 over stdio), pinned to `approvalPolicy: "never"`. By default dispatch uses `danger-full-access` to avoid Codex CLI sandbox failures in plugin launches. Override with `CODEX_SANDBOX` before invoking the slash command or subagent if you want `read-only` or `workspace-write`. The broker spawns one long-lived `codex app-server` child per working tree and recycles it on broker idle-out (5 min default).
- The plugin does not commit, push, branch, or modify git history. You own the working tree.
- Pre-existing uncommitted changes are preserved; the reviewer only sees Codex-attributable diffs.
- No remote Codex endpoints (Codex Cloud, OpenAI API). Subprocess to the local CLI only.

## Failure modes & guardrails

Codex is strong at *making* changes and weak at *bounding and proving* them; the loop's value is
the harness around it. Three failure modes — observed driving this plugin through a real
multi-round review on a security-critical task — shaped the reviewer/loop contract. Keep these
guardrails when editing `agents/codex-reviewer.md` and `agents/codex-orchestrator.md` (they must
stay mirrored):

- **Verification altitude — unit tests are not the acceptance criteria.** A run can pass
  `go test`/`pytest` while the *runtime* is broken (a flow that denies everyone, an unreachable
  dependency, a crash on boot). The reviewer therefore refuses `pass` when an acceptance criterion
  describes runtime/flow/deploy behavior but only unit tests ran (`verification-insufficient`);
  pass `--verify-cmd` (optionally `--clean-verify`) to actually exercise it.
- **Scope creep hides inside allowed files.** Path-based scope checks miss a large unrequested
  subsystem added *within* a permitted file. The reviewer ties every hunk to a criterion and uses
  `result.json` (`files_changed_outside_seed`, `lines_added`) as magnitude signals; unrequested
  "improvements" are `scope-creep`. Unrequested code is liability, not bonus — it widens the review
  surface and is where regressions hide.
- **Oscillation & regression — a later round can make things worse.** The loop treats every
  criterion as a standing requirement (re-checked each iteration, with `REGRESSION:` flags when a
  prior win breaks), feeds Codex a `PROTECT:` "don't regress what passes" guard, and stops early
  (`not-converging`) instead of burning the iteration budget when the verdict stops improving.
  "Revert to last good" is done by *feedback* — Codex undoes its own regression on the next turn —
  never by the harness touching git, preserving the *Codex owns all edits* invariant.

## Environment variables

Common knobs are summarized below; the full, source-verified reference (broker, picker, hooks, and GraphRAG runner vars with their defaults and edge-case behavior) lives in [`docs/configuration.md`](docs/configuration.md#environment-variables).

| Variable | Default | Effect |
|---|---|---|
| `CODEX_SANDBOX` | `danger-full-access` | Codex sandbox policy: `read-only`, `workspace-write`, or `danger-full-access`. |
| `CODEX_WORKDIR` | unset (auto-derived) | Pin codex's thread cwd to a module subdirectory of a `go.work` monorepo (absolute, or relative to the invocation cwd). When unset, the module is auto-derived from `CODEX_FILES` (nearest ancestor of the seeded files with a module manifest). The broker stays keyed on the repo root; only the per-thread cwd changes. Outside the repo root → falls back to the root. |
| `CODEX_MODEL` | unset (codex default) | Pin the codex model (e.g. `gpt-5.5`); unset uses codex's configured default. |
| `CODEX_DISPATCH_TIMEOUT_MS` | unset (no timeout) | Per-dispatch wall-clock budget in ms; on timeout the run records `exit_code: 124` in `result.json`. |
| `CODEX_DISPATCH_BIN` | unset | Absolute path to a prebuilt binary; the launcher execs it and skips download/checksum. |
| `CODEX_DISPATCH_KEEP_MCP` | unset (MCP disabled) | By default a dispatched codex runs with its configured MCP servers disabled so it focuses on the repo and can't stall on a failing MCP/web tool; set to any non-empty value to keep them enabled. |
| `CODEX_CONVENTIONS_FILE` | auto-detected | Conventions file injected into Codex's prompt. |
| `CODEX_FILES` | unset | Comma-separated relevant file paths seeded into the prompt. |
| `CODEX_RESULT_DIR` | `<repo>/.codex-dispatch/runs/<ts>-<pid>/` | Override the run-artifact directory. |
| `CODEX_DISPATCH_DEBUG` | unset | Extra hook diagnostics on stderr when non-empty. |
| `CODEX_BROKER_IDLE_MS` | `300000` (5 min) | Broker idle timeout before self-exit (guarded: never kills an in-flight turn). |
| `CODEX_BROKER_ADDR_PATH` | `<repo>/.codex-dispatch/broker.addr` | Broker address-file path (absolute used as-is; relative under repo root). |
| `CODEX_BROKER_MAX_CONCURRENT` | `8` | Max simultaneously running tasks. |
| `CODEX_BROKER_TURN_TIMEOUT_MS` | unset (off) | Per-turn deadline; a wedged turn is interrupted once it elapses. |
| `CODEX_BROKER_TASK_TTL_MS` | unset (off) | TTL for terminal tasks in the task table. |
| `CODEX_BROKER_MAX_TERMINAL_TASKS` | `1024` | Cap on retained terminal tasks (`0` = unbounded). |
| `PICK_FLOOR` / `PICK_CEILING` | `2` / `5` | Iteration-count bounds for the picker. |
| `PICK_DISABLE_LLM` | unset | Skip the LLM iteration estimate; use the deterministic score only. |
| `CODEX_DISPATCH_DISABLE_HOOKS` | unset | Disable all three Claude Code hooks when non-empty (`CODEX_DISPATCH_DISABLE_HOOK_STOP` disables only `Stop`). |

## Exit codes

Two layers return codes from disjoint ranges. Full table in [`docs/configuration.md`](docs/configuration.md#exit-codes).

**Binary (`codex-dispatch dispatch`):**

| Code | Meaning |
|---|---|
| `0` | OK — validation passed and the dispatch completed (codex's turn result is in `result.json`). |
| `2` | Not a usable git repo: cwd is not in a git repository, or the repo has no commits yet (unborn branch). |
| `3` | `codex` binary not found on `$PATH`. |
| `64` | Usage error: missing `CODEX_TASK`/`CODEX_ACCEPTANCE`, invalid `CODEX_SANDBOX`, missing subcommand, or bad flag. |

`exit_code: 4` is **not** a process exit code — it is a value written into `result.json` when a codex turn completes (`0`) but makes no meaningful repository edits; the process still exits `0`. (`result.json.exit_code` separately uses `124` for a timed-out/cancelled dispatch.)

**Launcher (`scripts/dispatch-codex.sh`, download/verify trust boundary):**

| Code | Meaning |
|---|---|
| `5` | Checksum mismatch (or no `checksums.txt` entry for the platform archive). |
| `6` | Required tool missing (`tar` or `unzip` on Windows, `curl`/`wget`, `sha256sum`/`shasum`), unsupported OS/arch, or missing `VERSION`. |
| `7` | Network unreachable while downloading the archive or `checksums.txt`. |
| `8` | Archive corrupt despite a matching checksum. |

## Troubleshooting

| Symptom | Fix |
|---|---|
| **Dispatch fails right after the broker starts (app-server handshake error)** | codex-dispatch needs `codex >= 0.130.0` (the app-server protocol). Run `codex --version` and upgrade the [codex CLI](https://github.com/openai/codex) if it is older. |
| **`codex` not found (exit 3)** | Put `codex` on `$PATH`, or point the broker at it with `CODEX_BROKER_CODEX_BIN`. |
| **Broker won't start / stale `broker.addr`** | The dispatch path auto-heals a dead endpoint (pings, then respawns). To force a clean restart: `codex-dispatch dispatch --list`, then `rm -f .codex-dispatch/broker.addr .codex-dispatch/broker.pid` and re-dispatch. If you set `CODEX_BROKER_ADDR_PATH`, remove that path instead. |
| **Codex sandbox-denied / `bwrap: setting up uid map: Permission denied`** | Codex's Linux sandbox uses bubblewrap, which needs unprivileged user namespaces. On hosts that restrict them (e.g. Ubuntu's `kernel.apparmor_restrict_unprivileged_userns=1`), `read-only`/`workspace-write` can't start the sandbox. The default `CODEX_SANDBOX=danger-full-access` skips the sandbox and is unaffected. If you request a sandboxed mode on such a host, the dispatch now **fails fast** (exit `64`) with an actionable message rather than letting every shell command fail cryptically — either use `danger-full-access`, or lift the restriction (`sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`). Any other `CODEX_SANDBOX` value fails validation with exit `64`. |
| **Network failure downloading the binary (exit 7)** | Offline-install: download `codex-dispatch_<os>-<arch>.tar.gz` (`.zip` on Windows) + `checksums.txt` from the [releases page](https://github.com/semanta-dev/codex-dispatch/releases) into `${XDG_CACHE_HOME:-$HOME/.cache}/codex-dispatch/v<VERSION>/manual/` and re-run, or set `CODEX_DISPATCH_BIN` to a prebuilt binary. |
| **Run reports `exit_code: 4` (no meaningful edits)** | The turn produced no repo edits. Tighten the task/acceptance, add relevant `--files`, and re-dispatch. (This is a `result.json` value, not a process exit code; detached runs never report it.) |

See [`docs/configuration.md#troubleshooting`](docs/configuration.md#troubleshooting) for the expanded version.

## Tests

```
go test -race ./...      # Go unit tests for all internal packages (cmd, broker, pick, diff, result, prompt, codex, codex/appserver, dispatch)
bats tests/bats          # shell-level dispatch, pick-iterations, launcher, detach, and hooks tests against a fake codex app-server
shellcheck scripts/*.sh scripts/hooks/*.sh tests/bats/helpers/setup.bash tests/integration/real-codex.sh
jq . .claude-plugin/plugin.json
```

The Bats suites resolve the `codex-dispatch` binary via `tests/bats/helpers/setup.bash`: a preset `CODEX_DISPATCH_BIN` is honored, otherwise the helper does a local `go build` into `./dist/`.

Reviewer-subagent fixtures live under `tests/fixtures/reviewer/` and are exercised by `tests/reviewer/run-fixtures.sh` when headless Claude invocation is configured. The end-to-end smoke procedure for the `/codex` command (with a fake codex binary, no API costs) is documented at `tests/e2e/codex-command-smoke.md`.

`tests/integration/real-codex.sh` is an opt-in (`REAL_CODEX=1`) smoke against real `codex app-server`. Costs a few cents in OpenAI charges per run; not in CI. Use it before tagging a release to confirm the broker still matches codex's actual protocol.

## Documentation

- Configuration reference (environment variables, exit codes, command guide, troubleshooting): [`docs/configuration.md`](docs/configuration.md)
- GraphRAG workflow integration: [`docs/graphrag-workflow-integration.md`](docs/graphrag-workflow-integration.md)
- Historical design and implementation notes live under `docs/superpowers/`; they are retained only as archive material and are not part of the active dispatch workflow.

## License

MIT
