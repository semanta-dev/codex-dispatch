# Independent CTO promotion decision

Decision: **GO**, issued 2026-09-09 local (2026-09-10 UTC).
Measured product: `cedf3ad` on `codex/plan-a-repair`.

The CTO approved Plan A promotion for the documented `--review-transport api`
profile, using the measured localhost transport, Haiku 4.5 with thinking disabled,
GPT-5.5 medium, and published-rate cost equivalents.

The independent audit at `/tmp/cto-plan-a-full-independent-audit.json` matches
`/tmp/plan-a-paired-full-v1.audit.json` exactly as JSON. The reviewer independently
verified all 60 row accounting results, 30 receipt-bound API histories and unique
responses, zero parent model usage, frozen evidence hashes, 90/90 fixture
judgments, and all nine CI jobs passing at the same product commit.

No agreed promotion blockers remain. Accepted outcomes: 30/30 per arm, no human
repair, zero critical violations. Latency ratio: 1.2033531994; all-spend per
accepted task ratio: 1.0755470858. Both are below the unchanged 1.25 limits.
The 10,000-file snapshot median is 9.0704% of plugin median live latency.

Approval excludes ordinary CLI/existing-session slash performance, actual proxy
billing claims, and broad real-world coding reliability. Plugin p95 and maximum
latency remain higher than direct. Confidence is high for this bounded promotion
screen. No merge, release or deployment was authorized by this decision.

The CTO explicitly permitted documentation-only closeout without repeating the
frozen product measurements.
