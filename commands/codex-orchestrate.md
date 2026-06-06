---
description: Let Claude choose the best Codex dispatch route for a task, GraphRAG plan, or Codex-authored spec/plan draft.
argument-hint: '[--prefer speed|cost|quality] [--plan-first] [--dry-run] <task-or-plan-path>'
allowed-tools: [Task, Bash, Read, Grep, Glob]
---

# /codex-orchestrate

The user invoked: `/codex-orchestrate $ARGUMENTS`

You are the route selector. Decide how this work should run, then execute the
selected route unless `--dry-run` is present.

Do not implement the requested code yourself unless the routing decision is
`manual-claude` because the task is exploratory, architectural, or unsafe to
dispatch. Prefer dispatch for scoped implementation work with observable
acceptance criteria.

## Flags

Parse these lightweight flags before routing:

- `--prefer speed`: optimize wall-clock time. This is the default.
- `--prefer cost`: prefer lower token/process overhead. For GraphRAG plan runs,
  add `--shared-broker`.
- `--prefer quality`: prefer stricter GraphRAG packet execution and reviewable
  ledgers. If no plan is supplied for non-trivial implementation work, do not
  use legacy planning workflows; route to Codex-authored GraphRAG plan/spec
  drafting first.
- `--plan-first`: force Codex-authored spec/plan drafting before any
  implementation dispatch.
- `--dry-run`: report the route and exact command/agent call you would use, but
  do not execute.

Everything else is the task or plan selector.

## Routing Policy

Use this order:

1. **Plan/spec authoring request**
   - If `--plan-first` is present, route to `graphrag-codex-plan`.
   - If the user asks to write, draft, create, or update a spec, design doc,
     implementation plan, GraphRAG plan, or packet plan and does not provide an
     existing executable plan path, route to `graphrag-codex-plan`.
   - If `--prefer quality` is present for a non-trivial implementation request
     and no plan path is supplied, route to `graphrag-codex-plan` first.
   - This route dispatches Codex to write docs under `docs/graphrag/`, then
     Claude reviews the diff through the `codex-reviewer` loop. It does not
     execute implementation packets.

2. **GraphRAG plan path**
   - If the input contains an existing path under `docs/graphrag/plans/` or a
     readable `*.plan.md`, inspect it with `Read`/`Grep`.
   - Count packet headings matching `^## Packet`.
   - If a specific packet selector is present after the path, route to
     `graphrag-codex-dispatch`.
   - If there is exactly one packet, route to `graphrag-codex-dispatch`.
   - If there are multiple packets, route to `scripts/graphrag-plan-runner.py`.
     Use `--jobs` equal to the number of packets, capped at `6`, unless the
     user supplied `--jobs`.
   - Add `--shared-broker` only when `--prefer cost` is present.

3. **Explicit packet execution language**
   - If the user mentions "packet", "GraphRAG", or "plan" and gives a plan
     path, follow the GraphRAG plan path route above.

4. **Simple scoped coding task**
   - If the input is a normal implementation request and does not point at a
     plan file, delegate to the existing `codex-orchestrator` subagent exactly
     as `/codex` does.

5. **Manual Claude route**
   - Choose `manual-claude` only when the work is primarily design,
     brainstorming, review of existing artifacts, high-risk architecture, or
     lacks enough acceptance criteria to dispatch safely.
   - In this case, report that dispatch is not appropriate and proceed with
     direct Claude analysis or ask for the missing concrete scope.

## Execution Details

### Route: codex

Use `Task`:

- `subagent_type`: `codex-orchestrator`
- `description`: `/codex orchestration`
- `prompt`: the remaining task text, preserving user flags such as
  `--files`, `--acceptance`, `--test-cmd`, `--max-iter`, and `--no-tests`.

Return the subagent response verbatim after a one-line route note.

### Route: graphrag-plan-authoring

Use `Task`:

- `subagent_type`: `codex-dispatch`
- `description`: `Codex GraphRAG spec and plan authoring`
- `prompt`: a labeled `codex-dispatch` prompt that asks Codex to write
  `docs/graphrag/specs/YYYY-MM-DD-<slug>.md` and/or
  `docs/graphrag/plans/YYYY-MM-DD-<slug>.plan.md`, with `TEST POLICY` set to
  `skip`, `MAX ITER` set to `3`, and acceptance criteria requiring:
  - only `docs/graphrag/` planning/spec files are changed;
  - no implementation, test, dependency, build, generated, or runtime files are
    changed;
  - specs include problem, goals, non-goals, proposed design, risks,
    verification strategy, and open questions;
  - plans use `## Packet NNN:` sections with Objective, Allowed files,
    Disallowed changes, Acceptance criteria, Implementation notes, GraphRAG
    context, Verification, and Progress record sections.

This is equivalent to routing through `/graphrag-codex-plan`; use that command's
contract when you need the full prompt template. Return the `codex-dispatch`
response verbatim after a one-line route note. Do not execute the generated
plan.

### Route: graphrag-single-packet

Use `Task`:

- `subagent_type`: `graphrag-codex-dispatch`
- `description`: `GraphRAG Codex packet dispatch`
- `prompt`: `<plan-path> [packet selector]`

Return the subagent response verbatim after a one-line route note.

### Route: graphrag-plan-runner

Use `Bash` from the repo root containing the plan:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/graphrag-plan-runner.py" <plan-path> \
  --out ".codex-dispatch/graphrag-plan-runs/$(date -u +%Y%m%dT%H%M%SZ)" \
  --jobs <N> [--shared-broker]
```

After it finishes, read `<out>/ledger.json` and report:

- route selected
- ledger path
- overall status
- packets run and passed
- failed packet headings and verification failures, if any
- whether `--shared-broker` was used

Do not manually edit the plan or packet files.

## Dry Run Output

For `--dry-run`, print:

```text
codex orchestrate route
- route: <codex|graphrag-plan-authoring|graphrag-single-packet|graphrag-plan-runner|manual-claude>
- reason: <one sentence>
- command: <exact command or Task target>
- preference: <speed|cost|quality>
```

## Safety

- Never commit, branch, push, reset, stash, or revert.
- Never broaden GraphRAG packet scope.
- Do not run multiple packet dispatches in the same checkout; use the plan
  runner for fanout.
- Do not route dispatch planning through Superpowers docs or skills. Active
  dispatch plans live under `docs/graphrag/plans/`.
- For plan/spec authoring, Codex writes the draft and Claude reviews it; Claude
  must not silently replace Codex's authored files with direct edits in the same
  command run.
- If routing is ambiguous, choose the safer narrower route and say why.
