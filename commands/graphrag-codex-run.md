---
description: Run a GraphRAG Codex plan with single-tree parallel packet dispatch and AgentHub coordination.
argument-hint: '<plan-path> [--jobs N] [--isolation none|worktree]'
allowed-tools: [Bash]
---

# /graphrag-codex-run

The user invoked: `/graphrag-codex-run $ARGUMENTS`

Run the packetized GraphRAG plan through the local orchestrator:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/graphrag-plan-runner.py" $ARGUMENTS \
  --out ".codex-dispatch/graphrag-plan-runs/$(date -u +%Y%m%dT%H%M%SZ)"
```

By default the runner executes every packet in the single working tree on the
current feature branch (`--isolation none`). It never creates git worktrees in
this mode. Concurrency safety comes from a deterministic, pre-flight
allowed-file overlap partition plus per-file locks: two packets that write the
same file are never placed in the same wave, and disjoint packets in a wave
dispatch and verify in parallel up to `--jobs N`. This local partition is the
**safety mechanism** and works with no extra session state. Pass
`--isolation worktree` to opt into the git-worktree fallback (see below).

## Coordination layer (AgentHub, when available)

When this command runs inside a Claude session that can reach the
`llm-graphrag-context` MCP/AgentHub tools, layer advisory coordination on top of
the local partition before invoking the runner:

1. **Register once.** Call `agent_register` a single time for this run (e.g.
   name `graphrag-codex-run`, goal "execute <plan-path>"). Reuse the returned
   agent id for the whole run; do not re-register per packet.
2. **Claim before dispatch.** For each packet about to be dispatched, call
   `agent_claim` on that packet's implementation files (its `Allowed files`
   minus the progress record) with an intent describing the packet.
3. **Defer on conflict.** If `agent_claim` reports a conflict on any file (an
   active peer agent already holds it), **defer** that packet to a later wave —
   do not dispatch it now. Retry the claim on a subsequent wave once the holder
   releases.
4. **Release on completion.** After the packet finishes (verified and recorded,
   or failed), call `agent_release` on exactly the files it claimed so peers can
   proceed.

"When available" means the MCP/AgentHub tools are reachable in the session. When
they are absent (no MCP session, tools not connected), skip steps 1–4 and rely
solely on the runner's local overlap partition. AgentHub claims are
**advisory/visibility** — they make concurrent agents visible to each other and
let you defer rather than collide. They do not replace isolation; the
deterministic local partition (single-tree default: `--isolation none` plus
per-file locks and the `overlap_conflicts`/`select_wave` scheduler) is what
actually guarantees co-writing packets never share a wave.

## Reporting

After it finishes, report:

- `ledger.json` path
- overall status
- isolation mode used (`none` single-tree default, or `worktree`)
- whether AgentHub coordination was active or the local-partition fallback was used
- packets run and passed
- packets deferred on claim conflict, if any
- failed packet summaries, if any
- verification failures, if any

Do not edit the plan manually. In the single-tree default the runner dispatches
implementation files directly in the parent tree, partitions co-writing packets
across waves, writes successful progress records after verification, and records
only allowed files. With `--isolation worktree` it instead routes each packet
through a temporary git worktree and fans in only changed allowed paths.
