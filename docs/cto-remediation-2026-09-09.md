# CTO remediation plan: 2026-09-09

**Decision: NO-GO for release or expanded adoption at `beb8446`. Recommended: Plan A (Repair).**

This is Maxwell's synthesis of the independent CTO and 10x Engineer reviews plus reproduced evidence in [the teardown](repo-teardown-2026-09-09.md). Prior Plan A measurements remain historical scoped evidence; this decision supersedes their promotion authority. This document proposes repairs and gates; it does not claim implementation or enforcement of a release freeze.

Maxwell performed a final accuracy check of both documents and confirmed the decision, finding mappings, sequencing, and gates.

## Sequence, ownership, and acceptance

Owners below are roles to assign, not assertions that staff have accepted work. Estimates are engineer effort, not delivery promises.

| Phase | Findings | Owner | Dependencies | Estimate |
| --- | --- | --- | --- | --- |
| 1. Contain and supervise execution | F01, F02, F07 | Execution/runtime engineer + security reviewer | Immediate | 0.5-1 day containment; 5-10 days durable repair |
| 2. Restore truthful completion | F03-F06 | Evidence/plan-runtime engineer | Shared evidence contract coordinated with phase 1 | 3-5 days |
| 3. Repair delivery and its tests | F08, F09, F12 | Release/tooling engineer | Parallel with phases 1-2; publication waits for qualification | 1.5-3 days |
| 4. Unify route guarantees | F10, F11 | Plugin/controller engineer | Stabilized execution/evidence contracts | 3-5 days |
| 5. Requalify frozen candidate | All | Independent reviewer + release owner | Phases 1-4 | 2-3 days plus CI/model runtime |

CTO planning envelope: approximately **15-26 engineer-days** for durable repair and qualification; allow immediate containment separately. Cross-platform confinement is the largest uncertainty. Phases 1-3 can overlap with explicit file ownership; concurrency is an option for staffing, not permission to edit shared evidence code without coordination.

### Phase 1: stop unsafe reviewed completion

Prevent unconfined verification from granting reviewed completion. Establish one owned subprocess supervisor for hooks, evidence collection, and plan verification, with an explicit working directory, minimal environment, filesystem/network policy, wall deadline, bounded memory, and process-tree termination. Keep reviewer credentials within the HTTP worker. Stream full logs to artifacts with bounded tails. A clean worktree is not a sandbox; environment filtering alone does not restrict HOME reads or network access. When confinement is unavailable, fail closed with an actionable diagnostic.

**Exit gate:** native tests for SIGINT, SIGTERM, repeated interruption, timeout, and parent failure leave no child able to write after cancellation and persist terminal evidence. Controlled commands cannot read fake credentials, write outside authorized locations, or reach disallowed network endpoints. Hung and noisy verification terminates predictably. Cover each supported native platform, including the platform's equivalent termination semantics.

### Phase 2: make PASS describe the delivered work

Define the acceptance instant. Fingerprint relevant live contents, types, modes, and index state at evidence collection and recheck immediately before acceptance under a defined mutation-coordination contract. Reject drift without reverting edits. Reject duplicate normalized packet IDs before dispatch and validate dependencies. Reconcile every packet against terminal inventory. Share the effective module directory among dispatch, automatic test detection, verification, and clean verification. Persist complete evidence envelopes for every terminal dispatch outcome before finalizing ledger rows.

**Exit gate:** edits, deletions, mode changes, and symlink replacement during a blocked reviewer response prevent stale PASS. `Packet 1` plus `Packet 001` fails with zero dispatches and identifies both headings. Root-pass/module-fail cases reject under explicit and automatic module selection, including clean verification. Failed/no-change dispatch and failed repair retain identity, artifacts, partial edits, accounting, iteration history, and normal structured failure reports without unnecessary reviewer calls. Every planned packet is executed, validly cached, or explicitly blocked.

### Phase 3: make installation real and observable

Repair scoped cleanup in the no-flock installer and preserve actual concurrent child exit statuses. Align VERSION, manifests, selected release assets, and quickstart instructions. Until a compatible release exists, document a source build and explicit override path. Do not silently fall back to an old release.

**Exit gate:** clean-cache installation succeeds with and without flock; failure and interruption clean up owned resources; concurrent installs produce a verified usable binary. Injected child exit 17 makes the test fail. Exercise normal cache/download resolution without `CODEX_DISPATCH_BIN` overrides on supported native platforms. Validate candidate packaging before publication, then smoke-test the actual published asset. Publishing remains a separate release action, not an action performed by this review.

### Phase 4: one supported review contract

Route supported direct commands, orchestration, and delegated simple tasks through a deterministic parser/controller and receipt validator. Keep presentation wrappers thin. Publish a route capability matrix and label legacy routes until migrated. Correct the parallelism claim and speed-routing expectations to reflect current serialization.

**Exit gate:** equivalent requests yield equivalent argument semantics, iteration limits, verification policy, terminal validation, and failure handling. Route-conformance tests establish this equivalence. Documentation agrees with measured scheduling. Retain the global checkout lock; removing it to improve a concurrency metric is not a repair.

### Phase 5: independent release requalification

Freeze the executable candidate and map every finding F01-F12 to a regression and result. Require complete supported-native CI for process lifecycle, verifier boundaries, live state, workdir, packet identity, terminal evidence, and cold installation. Resolve the Windows `.exe` and Claude compatibility questions as test gaps rather than assuming defects.

Re-run route conformance, all nine reviewer fixtures, and a fresh 30+30 paired cohort on the final candidate. Retain every failure and retry. Preserve explicit profile, transport, pricing basis, tail behavior, and the small-corpus limitation in the report.

**New GO requires all of:**

- No open P0/P1 findings and corrected F11 documentation/contract.
- Complete native CI and a traceable regression matrix for all twelve findings.
- Each reviewer fixture scores at least 8/10.
- Plugin acceptance at least 27/30 and no lower than direct acceptance.
- Zero critical violations.
- Latency and all-spend-per-accepted-task ratios at most 1.25 times direct.
- A reviewer independent of implementation signs a new decision against the frozen candidate.

These are CTO-proposed requalification gates, not results already obtained in this teardown.

## Non-goals and immediate action

Do not rewrite the Go broker, add agents or commands, build true parallel execution, tune prompts for benchmark speed, or weaken review gates during containment. Avoid treating environment redaction or a clean worktree as a security boundary.

The release owner must hold promotion pending these gates. Assign an execution owner to F01/F02 first, establish fail-closed containment, then execute Plan A (Repair) in the dependency order above. Implementation was not performed in this review/planning task.
