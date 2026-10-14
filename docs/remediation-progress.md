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
9. Checkpoint `d766ab3` passed all ten development suites in an isolated,
   unchanged checkout. Real native CI run `34439706249` then exposed Linux
   bubblewrap bootstrap failures on both architectures. Native Windows probe
   run `34439690985` stopped at LPAC token inspection on both architectures,
   before executing any child probe. These failed runs are retained, not
   counted as qualification. Targeted AppArmor prerequisite diagnostics and
   a reachable-host loopback denial control are the next Linux experiment.

## Automated workflows

### Retained native checkpoints

These are separate development candidates and cannot be combined into a final
same-candidate promotion matrix.

| Candidate / run | Observed result | Remaining limitation |
| --- | --- | --- |
| `cad8873`, CI `34440104903` | Linux amd64 native job passed; both Linux architectures passed ten development suites after targeted AppArmor provisioning | ARM shell interruption test exposed a cleanup race in the test; generated files dirtied the checkout |
| `a63d084`, CI `34440640315` | Linux arm64 native job passed with unchanged clean source | Linux amd64 twice exceeded the existing three-second hang/noisy bound; command/cleanup diagnostics added, budget unchanged |
| `cad8873`, probe `34440086262` | Seven actual HANDLE boundary tests passed on Windows amd64 and arm64 | Go standard-library file I/O and MSYS Bash fail inside LPAC |
| `63f25ae`, probe `34440756624` | Raw Windows denial and breakaway controls passed on both architectures; ARM64 supervisor-death cleanup passed | amd64 lifecycle remains unresolved; raw Win32 probes do not establish Go/Bash compatibility |
| `a360d5c`, probe `34440949149` | Six bounded Windows evidence integration tests passed on both architectures, including static no-tests collection, symlink parity, drift and a stalled-reader timeout | Full Windows verification remains unsupported |
| `a360d5c`, probe `34440949208` | Retained native macOS crash reports and fixed `/bin/sh` and `/usr/bin/true` startup failures on both architectures | No child boundary or lifecycle pass; literal-root startup experiment pending |

The CTO rejected the initial Windows worker pipe protocol because Windows can
block in a synchronous stdin write before `communicate(timeout)` begins its
timed wait. Requests now use a bounded temporary input file and a timed wait
with direct kill/reap. A one-MiB request to a child that never reads exercises
the original deadlock condition; this regression passed natively.

The Linux amd64 timing failure did not reproduce in thirty local trials
(worst observed 1.335 seconds). Diagnostics now include command, owner exit,
cleanup duration and remaining process identities on a drain timeout. No
timeout assertion or production deadline was raised to obtain a pass.

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
