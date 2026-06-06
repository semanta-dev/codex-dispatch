# GraphRAG Workflow Integration

`codex-dispatch` now includes Codex-authored GraphRAG planning and a GraphRAG
packet bridge:

```text
/graphrag-codex-plan <feature-or-change-request>
/graphrag-codex docs/graphrag/plans/YYYY-MM-DD-feature.plan.md
/graphrag-codex docs/graphrag/plans/YYYY-MM-DD-feature.plan.md 002
```

`/graphrag-codex-plan` dispatches Codex to write the spec and packet plan docs,
then uses Claude's reviewer loop to validate the diff. It does not execute the
plan. The bridge commands execute reviewed packet plans later.

The bridge is intentionally narrow. It executes one packet from a `graphrag-codex-planner` plan by translating the packet contract into the existing `codex-dispatch` subagent contract.

## Expected Plan Shape

Use `/graphrag-codex-plan` to have Codex create reviewed planning artifacts
under:

```text
docs/graphrag/specs/
docs/graphrag/plans/
```

Each packet should include:

- `Objective`
- `Allowed files`
- `Disallowed changes`
- `Acceptance criteria`
- `Implementation notes`
- `GraphRAG context`
- `Verification`
- `Progress record`

The packet's `.done.md` and `.blocked.md` progress record paths must appear in `Allowed files`. That lets Codex write exactly one progress record without violating packet scope.

## Execution Flow

### Plan/spec authoring

1. Claude invokes `/graphrag-codex-plan <feature-or-change-request>`.
2. The command calls `codex-dispatch` with planning-only acceptance criteria.
3. Codex writes only `docs/graphrag/` spec/plan files.
4. Claude's `codex-reviewer` reviews the diff for scope, plan shape, and
   acceptance criteria.
5. The reviewed artifacts become the input to later packet execution.

### Packet execution

1. Claude invokes `/graphrag-codex <plan> [packet]`.
2. The `graphrag-codex-dispatch` agent reads the plan and selects one packet.
3. The bridge passes the packet objective, allowed files, acceptance criteria, verification command, and progress-record requirement to the existing `codex-dispatch` subagent.
4. `codex-dispatch` runs Codex, reviews the diff, iterates, and returns its structured JSON result.
5. The bridge reports packet status, changed files, verification command, progress record path, allowed-file audit, and the next packet.

## Scope Audit

The bridge uses `scripts/graphrag-scope-audit.sh` after each Codex dispatch.

The helper audits:

```bash
git status --short --untracked-files=all
```

This matters because packet-created files often start as untracked files, and `git diff --name-only` misses them. The audit fails if any non-runtime changed path is not listed in the packet's `Allowed files`.

Ignored untracked generated artifacts include project-specific `gitignore`
matches plus a small built-in set of low-risk runtime/cache outputs:

- `.codex-dispatch/`
- `node_modules/`
- Python bytecode and caches: `__pycache__/`, `*.pyc`, `.pytest_cache/`, `.mypy_cache/`, `.ruff_cache/`
- Python virtualenv/test outputs: `.venv/`, `venv/`, `env/`, `.tox/`, `.coverage`, `htmlcov/`
- Temp/log outputs: `tmp/`, `temp/`, `.tmp/`, `logs/`, `*.log`
- Frontend caches/builds: `.next/`, `.nuxt/`, `.svelte-kit/`, `.turbo/`, `coverage/`

These ignores apply to untracked files only. If a project already tracks a file
under a generated-looking path, such as `bin/tool`, modifications to that file
are still audited. Broad build directories such as `dist/`, `build/`,
`target/`, `bin/`, and `obj/` are ignored only when the repository's own
ignore policy or `GRAPHRAG_SCOPE_AUDIT_IGNORE` says so. Additional
newline-separated ignore globs may be supplied with
`GRAPHRAG_SCOPE_AUDIT_IGNORE`.

Everything else must be explicitly allowed by the packet.

## Parallel Packet Dispatch: Single Tree by Default

Parallel packet fanout runs in the **single working tree on the current feature
branch** by default. Concurrency is made safe by the plan runner itself, not by
git worktrees:

- A deterministic, pre-flight **allowed-file overlap partition** treats any file
  written by two packets as a scheduling constraint, so co-writing packets are
  never placed in the same wave.
- **Per-file locks** then let disjoint packets in a wave dispatch and verify in
  parallel up to `--jobs N`, while only file-sharing packets serialize.

This local partition is the **safety mechanism**. It needs no external session
state and is what actually makes git worktrees unnecessary for correct
`files_changed` attribution. (Residual caveat: a verification command that reads
files a packet does not claim — e.g. `go build ./...` — can still observe a
concurrent peer's in-flight edits to those unclaimed files; run such packets
with `--jobs 1` or under `--isolation worktree` when verification needs a fully
quiescent tree.)

### Claude-layer coordination (AgentHub, when available)

When parallel dispatch is driven from a Claude session that can reach the
`llm-graphrag-context` MCP/AgentHub tools, the orchestrator (`/graphrag-codex-run`,
the `graphrag-codex-dispatch` agent) layers advisory coordination on top of the
local partition:

1. **Register once** — `agent_register` a single time per run; reuse the agent id.
2. **Claim before dispatch** — `agent_claim` the packet's implementation files
   (its `Allowed files` minus the progress record) before dispatching it.
3. **Defer on conflict** — if a claim conflicts with an active peer agent, defer
   that packet to a later wave instead of dispatching it; retry the claim after
   the holder releases.
4. **Release on completion** — `agent_release` exactly the claimed files when the
   packet finishes (pass or fail).

"When available" means the MCP/AgentHub tools are reachable in the session. When
they are absent, the orchestrator skips claim/defer/release and relies solely on
the local overlap partition. AgentHub claims are **advisory/visibility** — the
hub allows partial-success claims and only *surfaces* conflicts, so claims alone
cannot replace isolation. They let cooperating agents see each other and defer
rather than collide; the deterministic single-tree partition remains the
correctness guarantee.

## Opt-in Worktree Isolation (`--isolation worktree`)

Git worktrees are the **opt-in fallback**, not the default. Use them when a
packet needs a fully isolated checkout — for example when its verification must
run against a quiescent tree, or when running against an actively dirty parent
tree. Select them with `--isolation worktree` on `graphrag-plan-runner.py`,
which routes each packet through `scripts/graphrag-worktree-dispatch.sh`. That
helper can also be invoked directly:

```bash
GRAPHRAG_ALLOWED_FILES="$(cat /tmp/allowed-files)" \
CODEX_TASK="$TASK" \
CODEX_ACCEPTANCE="$ACCEPTANCE" \
CODEX_FILES="$FILES" \
CODEX_CONSTRAINTS="$CONSTRAINTS" \
  scripts/graphrag-worktree-dispatch.sh
```

The helper:

1. Creates a detached temporary git worktree from `HEAD`.
2. Runs `scripts/dispatch-codex.sh` inside that worktree.
3. Runs `scripts/graphrag-scope-audit.sh` inside that worktree.
4. Copies only changed paths listed in `Allowed files` back to the parent checkout.
5. Leaves run artifacts under `.codex-dispatch/graphrag-worktree-runs/`.

If `CODEX_FILES` names parent-checkout files that are not present in `HEAD`
yet, the helper seeds them into the temporary worktree as read inputs. Unchanged
seeded input files that are not in `Allowed files` are removed before the scope
audit, so synthesis packets can read prior packet outputs without claiming them
as edits. If Codex changes a seeded input that is not allowed, the audit fails.

Set `GRAPHRAG_WORKTREE_KEEP=1` to keep the temporary worktree for debugging.
Set `GRAPHRAG_DISPATCH_COMMAND` only in tests or benchmark harnesses that need a
fake dispatch command.
Set `GRAPHRAG_DISPATCH_ATTEMPTS=N` to retry a packet in a fresh isolated
worktree when the dispatch process returns successfully but its underlying
`result.json` has a non-zero `exit_code`. The default is `2`. Only a clean
attempt is scope-audited and fanned into the parent checkout.

The selected attempt's `result.json` and `stdout.log` are copied to
`dispatch-result.json` and `dispatch-stdout.log` in the persistent worktree run
directory before the temporary worktree is cleaned up. Benchmark harnesses
should read those files instead of scraping removed worktrees.

For benchmark runs, `tests/sdk/aggregate-dispatch-metrics.py` summarizes those
persistent files across worktree dispatches, and
`tests/sdk/repeat-command-benchmark.py --runs 10 --out <dir> -- <command>` gives
a repeatable pass-rate and p50/p95 wall-time wrapper for direct comparisons.

## Plan-Level Orchestration

Use `scripts/graphrag-plan-runner.py` when a plan has multiple independent
packets:

```bash
scripts/graphrag-plan-runner.py docs/graphrag/plans/<feature>.plan.md \
  --out .codex-dispatch/graphrag-plan-runs/<run-id> \
  --jobs 4
```

The runner:

1. Parses every `## Packet NNN:` section.
2. Validates required packet contract sections.
3. Reads optional `Depends on:` or `Blocked by:` packet dependencies.
4. Builds the allowed-file overlap partition so co-writing packets never share a
   wave, then runs unblocked, non-conflicting packets concurrently. In the
   default `--isolation none`, each packet dispatches directly via
   `scripts/dispatch-codex.sh` in the parent tree under per-file locks; with
   `--isolation worktree` it routes through `graphrag-worktree-dispatch.sh`.
5. Runs each packet's `Verification` command (in single-tree mode under the same
   per-file lock; in worktree mode after fan-in).
6. Writes the packet progress record with `Status: done` after successful
   dispatch and verification.
7. Writes a crash-durable `ledger.json` with packet status, wall time, run
   directories, verification output, changed files, the overlap conflicts that
   shaped scheduling, and dispatch result metadata.

`--isolation` selects the execution model: `none` (default) is single-tree on
the current branch with the overlap partition as the safety mechanism;
`worktree` is the opt-in git-worktree fallback. When the runner is driven from a
Claude session with the `llm-graphrag-context` MCP/AgentHub tools reachable, the
orchestrator additionally registers once and claims each packet's implementation
files before dispatch, defers a packet on any claim conflict, and releases on
completion (see "Claude-layer coordination" above). Without that session, the
local overlap partition alone keeps parallel dispatch correct.

For performance, the runner keeps the worker prompt implementation-focused:

- progress records are not part of the worker's editable scope by default;
- `--context-mode targets` includes only existing target files in `CODEX_FILES`;
- `--context-mode full` includes packet `Inputs` plus target files when a packet
  needs verifier/spec content in the model prompt;
- `--context-mode none` sends no file bodies and relies on the task text plus
  tools.
- `--shared-broker` reuses one broker/app-server across packet dispatches. In
  benchmarks this reduced total token/runtime work but was slower on wall time
  for the six-packet ops task, likely because the shared app-server serialized
  more work internally. Use it when token/cost reduction matters more than
  wall-clock latency.

The ledger is the handoff point for GraphRAG review and memory. A follow-up
GraphRAG wrapper can ingest accepted files, record packet summaries, and create
repair packets from failed ledger entries without scraping terminal output.

## Why This Exists

The regular `/codex` command is task-oriented: it can derive acceptance criteria from a user prompt and report a human-readable result.

GraphRAG packet execution is contract-oriented:

- one packet only;
- allowed files only;
- no inferred scope;
- verification required;
- `.done.md` or `.blocked.md` progress record required.

The bridge keeps those packet rules while reusing the faster Codex implementation loop.

## Recommended Workflow

1. Plan with GraphRAG:

   ```text
   /graphrag-codex-plan <feature-or-change-request>
   ```

2. Execute the next packet:

   ```text
   /graphrag-codex docs/graphrag/plans/<feature>.plan.md
   ```

3. Or execute a parallelizable plan:

   ```text
   /graphrag-codex-run docs/graphrag/plans/<feature>.plan.md --jobs 4
   ```

4. Review with GraphRAG:

   ```text
   Use graphrag-codex-reviewer on the packet progress record and diff.
   ```

5. Repeat until no packets remain.

## Current Limits

- The single-packet bridge slash command still dispatches one selected packet at a time. Use the plan runner for independent packet fanout.
- Full cost summaries report Claude-side cost and Codex token usage separately; Codex dollar conversion is not yet built in.
- The bridge relies on packet text structure. Malformed packet headings or missing fields stop dispatch instead of guessing.
