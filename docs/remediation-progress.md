# Plan A (Repair): adversarial remediation progress

Goal: survive adversarial CTO and 10x Engineer review and earn an evidence-backed
CTO GO. **Goal remains active. Overall promotion rating remains NO-GO.**

Baseline: `beb8446`; findings and gates are recorded in
[the teardown](repo-teardown-2026-09-09.md) and
[the CTO plan](cto-remediation-2026-09-09.md). The historical bounded GO is not
current promotion authority. This checkpoint implements repairs; it does not
claim completed native qualification or authorize release.

## Repairs implemented

| Finding | Current implementation | Remaining qualification |
| --- | --- | --- |
| F01 | Linux bubblewrap, minimal environment, disposable nonignored input snapshot, no host HOME/runtime artifacts/network; clean materialization reads raw Git objects without host hooks/filters | Native macOS/Windows backends, legitimate toolchain/dependency policy and public-route probes |
| F02 | Shared supervised execution, Linux dedicated subreaper, parent-death cleanup, cancellation admission and terminal checks; explicit validated detach handoff | Full native ownership and public dispatch/reviewer interruption matrix |
| F03 | Versioned whole-tree/index fingerprint captured before checks, checked after checks and again at final acceptance | Frozen candidate native drift matrix and reviewed synchronous route evidence |
| F04 | Duplicate normalized IDs, missing dependencies and cycles rejected before dispatch; cached packets explicitly accounted | Final candidate native inventory matrix |
| F05 | Go writes authoritative effective directory after the turn using safe leaf replacement; collector detection and clean verification use that directory | Native module-scoping matrix and toolchain compatibility |
| F06 | Nonzero dispatch persists evidence; API history reconciles deterministic failure reason | Full failed-repair/public-route accounting qualification |
| F07 | Finite verifier/plan budgets, disk logs with bounded tails, file size/count/read deadlines, special-file rejection | Native resource/lifecycle qualification and broader resource policy |
| F08 | Prerelease source-build instructions; release remains gated on selected artifact evidence | v0.5.0 is not published; cold installation of final selected release assets |
| F09 | Scoped installer cleanup, cache recheck under lock and atomic publication | Native resolver evidence |
| F10 | Delegated requests use canonical adapter; default direct API slash validates completion, exact prompt receipts and visible result; background operations need no reviewer key | Actual Claude UI/route-conformance qualification; explicit existing-session CLI route is legacy/unqualified |
| F11 | Documentation states serialized dispatch/verification in both modes; safety lock retained | Final scheduling regression on candidate |
| F12 | Correct concurrent child status, injected exit17 regression, no-flock failure/concurrency/interruption coverage | Native launcher suite |

## Adversarial iterations

1. Initial implementation review rejected a fingerprint captured too late and
   model-writable pre-turn workdir metadata. Both were corrected; workdir
   publication also received a symlink regression. Launcher tests grew to 14.
2. CTO reproduced a host post-checkout hook bypass, FIFO post-audit hang, and
   signal-handler leak on failed launch. Clean materialization was replaced,
   file audits bounded, and signal restoration moved around all setup paths.
   CTO independently confirmed those reproductions no longer succeed.
3. CTO found precancelled/late-completion acceptance and a fractional fixture
   score without ten observations. Admission/terminal checks and exact retained
   fixture observations now reject them. CTO independently verified Linux
   double-fork cleanup after normal exit, SIGINT, SIGTERM and supervisor SIGKILL.
4. CTO found that fixture request bodies could be unrelated to the named case.
   The gate now reconstructs the exact payload through the harness's shared pure
   builder. It never runs fixture commands while validating promotion.
5. The 10x Engineer found direct-route integration defects: unresolved auto-test
   sentinel, absent final validation, session-wide ambiguous receipt lookup,
   hidden results, and unnecessary background API credentials. Targeted fixes
   were independently approved in round 4; 11 expansion, 12 compact-profile and
   3 adapter tests passed. Actual Claude UI rendering remains unverified.
6. A real cold-start detach test verifies explicit broker ownership handoff
   through the hook supervisor. This exception requires the known detach
   launcher, successful exit, and a valid returned task ID; ordinary transient
   children are still drained.
7. CTO adversarial handoff reviews exposed premature detach, forged workspace
   commitment, and same-user reopening of anonymous control pipes through procfs.
   Handoff now requires a private ready/commit exchange. Both controller and
   owner disable Linux dumpability before launch. CTO independently reproduced
   EACCES for both pipe holders and verified cancellation leaves no late writer;
   all four handoff regressions pass. This closes the demonstrated unconfined
   child attack, not a demonstrated escape from the Codex sandbox.
8. The engineer found evidence output exclusion could hide source drift when
   the output directory was the repository root. Development evidence must now
   live outside the repository, and source files are never excluded by output.

## Automated workflows

- `scripts/remediation-gates.py --out <directory-outside-repository>` runs development regressions,
  archives commands/log hashes and a source manifest, and invalidates evidence
  if HEAD/index/files change during the run. It never generates approval.
- Native CI installs Linux bubblewrap, runs launcher and lifecycle suites, then
  retains remediation-gate artifacts even when a required check fails.
- `config/promotion-policy.json` fixes the six native platforms, twelve finding
  checks, positive controls, lifecycle checks, nine reviewer fixtures and paired
  benchmark thresholds.
- `scripts/promotion-gate.py` rejects missing/unsupported/stale evidence, altered
  artifacts, inconsistent fixture observations, wrong reviewer inputs/profile,
  missing independent decisions, and absent external record approval.
- Release requires a successful same-candidate trusted CI run, an independently
  reviewed protected environment and approved record digest, and publishes only
  the six approved archives. No real promotion record or complete evidence
  producer exists yet. Missing evidence must keep release blocked.

## Next work, in dependency order

1. Complete native confined verification and lifecycle backends. The
   [Windows LPAC design](windows-verification-design.md) records concrete APIs,
   creation-time job ownership and the first native compatibility probes.
   macOS needs a demonstrated sandbox and descendant ownership mechanism.
2. Establish explicit usable toolchain/dependency inputs without exposing host
   credentials or broad HOME access. Positive toy commands alone do not prove
   real project verification compatibility.
3. Run the repaired routes on a frozen candidate, including Claude-visible
   output, cold background ownership, failed repair accounting, effective module
   directories and all six native platforms. Retain every failure.
4. Produce genuine promotion evidence, rerun all nine reviewer fixtures and a
   fresh paired 30+30 cohort, then request independent final decisions. Keep
   cost/latency/acceptance thresholds unchanged. No retrospective reuse of the
   earlier bounded benchmark as proof for these new execution paths.

No paid model calls were made during this repair checkpoint. Local synthetic
promotion records test rejection logic only; they are not release evidence.
