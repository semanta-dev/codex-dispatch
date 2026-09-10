---
description: Delegate a coding task to the Codex CLI and review the result with an iteration loop.
argument-hint: '[--max-iter N] [--acceptance "..."] [--files a,b,c] [--workdir dir] [--constraints "..."] [--no-tests] [--test-cmd "..."] [--verify-cmd "..."] [--clean-verify] [--no-resume] <task>'
allowed-tools: [Bash, Read, Grep, Glob]
model: claude-haiku-4-5-20251001
effort: low
---

# /codex

You own this invocation's dispatch, independent review, and iteration loop.
Use the following contract directly. Do not delegate to another Claude agent.
The raw invocation arguments are:

$ARGUMENTS

The implementation contract (trusted plugin instructions):

!`cat "${CLAUDE_PLUGIN_ROOT}/agents/codex-orchestrator.md"`
