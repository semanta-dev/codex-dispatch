---
description: Execute one GraphRAG Codex task packet through codex-dispatch.
argument-hint: '<plan-path> [packet number|packet title]'
allowed-tools: [Task]
---

# /graphrag-codex

The user invoked: `/graphrag-codex $ARGUMENTS`

Delegate this entire invocation to the `graphrag-codex-dispatch` subagent.

Use the `Task` tool with:

- `subagent_type`: `graphrag-codex-dispatch`
- `description`: `GraphRAG Codex packet dispatch`
- `prompt`: `$ARGUMENTS`

Return the subagent's response to the user verbatim. Do not parse, summarize, or reformat it.

Do not execute the packet yourself. This command is only the routing surface from GraphRAG packet plans into the codex-dispatch implementation loop.
