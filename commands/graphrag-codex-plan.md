---
description: Dispatch Codex to write GraphRAG spec and packet plan docs, then review them with Claude.
argument-hint: '[--spec-only|--plan-only] <feature-or-change-request>'
allowed-tools: [Task, Bash, Glob]
---

# /graphrag-codex-plan

The user invoked: `/graphrag-codex-plan $ARGUMENTS`

Use Codex for the planning/spec-writing work, then rely on the normal Claude
review loop inside `codex-dispatch`. Do not write the spec or plan yourself.

## Intent

This command is for turning a feature/change request into reviewable GraphRAG
artifacts:

- `docs/graphrag/specs/YYYY-MM-DD-<slug>.md`
- `docs/graphrag/plans/YYYY-MM-DD-<slug>.plan.md`

The Codex run must stop after authoring planning documents. It must not
implement the feature.

## Flags

- `--spec-only`: write only the spec file.
- `--plan-only`: write only the packet plan file.

If neither flag is present, write both a spec and a packet plan. If both flags
are present, prefer both artifacts and note the conflict in the Task prompt.

## Preflight

Use `Bash` only for read-only project context:

```bash
date +%F
```

Use `Glob` to inspect existing GraphRAG docs:

- `docs/graphrag/specs/*.md`
- `docs/graphrag/plans/*.plan.md`
- `docs/graphrag/progress/*.md`

If the directories do not exist, let Codex create only the needed
`docs/graphrag/...` directories and files.

Derive a short lowercase hyphenated slug from the feature request. Use the
current date from `date +%F` for filenames.

## Dispatch

Invoke `Task`:

- `subagent_type`: `codex-dispatch`
- `description`: `Codex GraphRAG spec and plan authoring`
- `prompt`: labeled fields in this exact shape:

```text
TASK
Write GraphRAG planning artifacts for this request:

<original user request without command flags>

Create the requested artifact set:
- spec path: docs/graphrag/specs/<YYYY-MM-DD>-<slug>.md
- plan path: docs/graphrag/plans/<YYYY-MM-DD>-<slug>.plan.md

If --spec-only was supplied, create only the spec path. If --plan-only was
supplied, create only the plan path. Do not implement the feature.

ACCEPTANCE CRITERIA
- Creates only GraphRAG planning/specification documents under docs/graphrag/.
- Does not modify implementation, test, dependency, build, generated, or runtime files.
- Spec document, when requested, explains the problem, goals, non-goals, proposed design, risks, verification strategy, and open questions.
- Plan document, when requested, uses GraphRAG packet format with `## Packet NNN:` headings.
- Each plan packet includes Objective, Allowed files, Disallowed changes, Acceptance criteria, Implementation notes, GraphRAG context, Verification, and Progress record sections.
- Packet scopes are small enough for independent Codex dispatch where possible and list concrete allowed paths.
- Progress record paths point under docs/graphrag/progress/ and include corresponding `.done.md` or `.blocked.md` names.
- The plan does not claim implementation is complete and does not execute any packet.

CONSTRAINTS
- Planning/spec-writing only.
- Do not edit source code, tests, dependency manifests, lockfiles, generated outputs, or runtime artifacts.
- Prefer existing project conventions and existing docs/graphrag naming if present.
- Use concrete paths and verification commands where the repository makes them discoverable; otherwise state the uncertainty in the relevant packet.

FILES
<comma-separated existing GraphRAG spec/plan/progress files discovered by Glob, or empty>

TEST POLICY
skip

MAX ITER
3
```

Return the `codex-dispatch` response verbatim after a one-line route note. The
Claude reviewer inside `codex-dispatch` is the required review gate for these
planning artifacts.

## Safety

- Do not call `/graphrag-codex` or `/graphrag-codex-run` from this command.
- Do not implement any packet.
- Do not manually edit the generated spec or plan after Codex returns. If the
  reviewer fails the result, surface the review feedback so a later dispatch can
  iterate with tighter criteria.
