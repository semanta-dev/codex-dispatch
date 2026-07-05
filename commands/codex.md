---
description: Delegate a coding task to the Codex CLI and review the result with an iteration loop.
argument-hint: '[--max-iter N] [--acceptance "..."] [--files a,b,c] [--workdir dir] [--constraints "..."] [--no-tests] [--test-cmd "..."] [--verify-cmd "..."] [--clean-verify] [--no-resume] <task>'
allowed-tools: [Task]
---

# /codex

The user invoked: `/codex $ARGUMENTS`

Delegate this entire invocation to the `codex-orchestrator` subagent. That subagent runs on Haiku — orchestration (flag parsing, dispatch invocation, verdict judgment, iteration control) does not need a flagship model, and routing to Haiku saves ~10× the per-token cost vs Sonnet.

Use the `Task` tool with:

- `subagent_type`: `codex-orchestrator`
- `description`: `/codex orchestration`
- `prompt`: `$ARGUMENTS`

Return the orchestrator's response to the user verbatim — do not summarize, paraphrase, or re-format it.

Do NOT do any parsing, dispatch, review, or iteration work yourself. The orchestrator owns the entire loop. Your only job is the single Task call above.
