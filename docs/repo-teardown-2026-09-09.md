# Repository teardown: 2026-09-09

**CTO decision: NO-GO for release or expanded adoption at `beb8446`.**

Independent reviewers: 10x Engineer (Hubble) and CTO (Maxwell), with parent-agent reproduction and installation/security probes. Scope: execution lifecycle, verification/evidence, packet scheduling, public routes, installation, and regression coverage. This is a review and remediation plan; product code has not been repaired.

The prior bounded Plan A GO at product `cedf3ad`, documented by `beb8446`, remains valid historical benchmark evidence. Fresh boundary failures supersede its promotion authority. Passing the earlier benchmark does not establish safe cancellation, verifier confinement, or live-state acceptance.

## Confirmed findings

P0 means containment before promotion; P1 means required repair before requalification; P2 means a material contract/documentation defect. Severity reflects demonstrated behavior and impact, not a claim that every route has been exhaustively audited.

### F01. P0: verification crosses the host privilege and credential boundary

**Location:** `scripts/review-evidence.py:58`, `scripts/hooks/codex-expansion.py:67`, `scripts/api-review.py:117`.

Verification executes repository/model-controlled shell commands as the host user with the inherited environment. A controlled probe set `CODEX_SANDBOX=read-only` and a fake reviewer credential. The actual verifier exited zero, wrote a sibling file outside its temporary repository, and printed the fake key into evidence. The actual API request artifact then contained that key. HTTP was mocked; no real credential or actual transmission was demonstrated.

This is a verification isolation failure, not a demonstrated broker-auth bypass. Restricting the dispatch sandbox does not confine this later verifier. Environment redaction alone cannot prevent reads of HOME credentials or unrestricted filesystem/network use. Require an explicit confined verification policy and minimal environment, isolate reviewer credentials, and fail closed when confinement is unavailable.

**Evidence:** `review-evidence/2026-09-09/repo-teardown-verifier-boundary.py`.

### F02. P0: cancellation leaves child processes able to mutate files

**Location:** `scripts/hooks/codex-expansion.py:89`.

`invoke()` starts a separate process session but cleans it up only on timeout. An actual helper with a delayed sentinel writer survived SIGINT to the hook: `hook interrupted exit: -2`, `child completed mutation after interrupt: True`. A cancelled task can continue changing files and potentially consuming model spend.

Use shared owned-process supervision across signal handling and exceptional exit; drain descendants and persist interrupted evidence. Prove no later writes after interruption during dispatch and verification on supported native platforms.

**Evidence:** `review-evidence/2026-09-09/engineer-cancel-repro.py`.

### F03. P1: PASS can refer to files that changed or disappeared

**Location:** `scripts/codex-reviewed.py:114`.

The validator checks archived results and verification codes without comparing live state with `changed_file_facts`. The actual validator returned `pass` after editing a fingerprinted file and again after deleting it. No artifact tampering was needed.

Bind acceptance to a fresh check of relevant contents, file types, modes, and index state; define the acceptance instant and mutation-coordination contract. Reject drift without reverting the user's edits.

**Evidence:** `review-evidence/2026-09-09/engineer-review-repros.py`.

### F04. P1: duplicate packet IDs silently discard work

**Location:** `scripts/graphrag-plan-runner.py:791`.

Normalized packet identities collapse into a dictionary/set without uniqueness validation. The real parser and scheduler, with only execution stubbed, accepted `Packet 1: first` and `Packet 001: second`: `packets_total: 2`, `packets_run: 1`, `packets_passed: 1`, overall `pass`, exit zero, executed only `second`.

Reject duplicate normalized IDs before dispatch, validate the dependency graph, and reconcile every planned identity with terminal outcomes.

**Evidence:** `review-evidence/2026-09-09/engineer-duplicate-repro.py`.

### F05. P1: verification runs in the wrong directory

**Location:** `scripts/review-evidence.py:97`, `scripts/review-evidence.py:105`, `scripts/review-evidence.py:114`, `scripts/hooks/codex-expansion.py:75`.

The collector and automatic test detection use repository root despite a selected module workdir. With `CODEX_WORKDIR=<repo>/module`, actual test and verification `pwd` output was `<repo>`. Root tests can pass while the intended module remains untested.

Share and record one effective directory across dispatch, test discovery, verification, and equivalent clean-worktree verification. Cover explicit and automatic module selection.

**Evidence:** `review-evidence/2026-09-09/engineer-review-repros.py`.

### F06. P1: normal dispatch failures break the rejection artifact contract

**Location:** `scripts/review-evidence.py:90`, `scripts/review-evidence.py:122`, `scripts/api-review.py:218`.

The collector returns nonzero dispatch outcomes before persisting `review-evidence.json`; history validation unconditionally reads that file. A real failure bundle marked `complete: true` lacked the artifact and raised `FileNotFoundError`. Expected codex-error/no-changes paths become validation errors rather than valid structured rejection reports.

Persist terminal evidence before completing ledger rows for every outcome; preserve partial edits and accounting while avoiding unnecessary reviewer calls.

**Evidence:** `review-evidence/2026-09-09/engineer-review-repros.py`.

### F07. P1: plan verification has no bounded runtime or output

**Location:** `scripts/graphrag-plan-runner.py:73`, `scripts/graphrag-plan-runner.py:449`.

Verification captures full stdout/stderr and waits without a timeout; truncation happens only afterward. The global checkout lock remains held. The CTO's controlled probe set `CODEX_DISPATCH_TIMEOUT_MS=50`, yet a roughly 0.26-second verifier still succeeded. A hung/watch command can block the plan indefinitely, and noisy output can exhaust memory.

Add verification and overall wall budgets, process-tree cleanup, streaming artifact logs, and bounded in-memory tails. The CTO reported this probe; no standalone reproduction script was archived for this finding.

### F08. P1: the default install selects an unavailable release

**Location:** `VERSION:1`, `scripts/dispatch-codex.sh:27`, `scripts/dispatch-codex.sh:133`, `scripts/dispatch-codex.sh:194`.

The repository pins 0.5.0. At review time, `gh release view v0.5.0` reported release not found; the release list contained only v0.3.3, published 2026-06-06. Default cold-cache resolution therefore cannot obtain its selected release. Source-built binary overrides used in validation bypass this path, and the local-clone quickstart lacks that required build/override step.

Align version, manifest, release availability, and installation instructions. Gate the actual selected artifact using a cold cache without binary overrides. Do not silently downgrade to an incompatible old version. Release availability is a time-scoped observation.

### F09. P1: no-flock installation fails during cleanup

**Location:** `scripts/dispatch-codex.sh:104`, `scripts/dispatch-codex.sh:137`.

Nested RETURN/EXIT cleanup traps conflict with local variable scope. A valid local archive and a controlled PATH without `flock`, including required gzip, caused exit 1 with `tmp: unbound variable`. An earlier invalid probe omitted gzip; that result was discarded.

Use scoped cleanup with clear ownership. Test success, failure, interruption, and concurrent installation without flock.

**Evidence:** `review-evidence/2026-09-09/repo-teardown-launcher-probes.py`.

### F10. P1: equivalent-looking routes have different review guarantees

**Location:** `commands/codex-orchestrate.md:84`, `agents/codex-orchestrator.md:48`, `agents/codex-orchestrator.md:59`.

Orchestration delegates to a legacy agent with model-derived criteria/iteration limits and a separate retry/review loop. It does not share the deterministic hook parser and receipt controller. There is no route-conformance gate establishing equivalent semantics.

Route supported simple tasks through the canonical structured controller and validator. Explicitly label legacy capabilities until migrated. This is a source-confirmed control-path discrepancy, not a measured failure of every model invocation.

### F11. P2: advertised disjoint parallel execution is serialized

**Location:** `scripts/graphrag-plan-runner.py:616`, `commands/graphrag-codex-run.md:18`, `README.md:63`.

Both execution modes hold `__parent_checkout__` through dispatch, fan-in, and verification. The CTO's controlled two-worker probe, with each dispatch taking 0.15 seconds, observed peak concurrency 1 and total duration about 0.303 seconds. Documentation promises disjoint parallel dispatch/verification.

Correct documentation and routing expectations now. Retain the safety lock; true isolated concurrency requires a separate fan-in and mutation-coordination design. No standalone probe script was archived for this finding.

### F12. P1: concurrent-launch test hides child failures

**Location:** `tests/bats/launcher.bats:109`.

`if ! wait "$pid"; then status=$?; fi` stores the negated command's successful status in the failure branch. A standalone child exit 17 produced `test_status=0`. The launcher suite can report success while a concurrent launch failed. Native CI also lacks the fallback installer coverage required here.

Capture the actual child status, wait for all children, and retain diagnostics. An injected exit 17 must make the test fail; exercise resolver fallback paths on native platforms.

## Verification and limits

- `python3 -m unittest discover -s tests/python`: 56 passed.
- `bats tests/bats/launcher.bats`: 7 passed, including the defective assertion described in F12.
- Engineer targeted API review suite: 6 passed, a subset of the Python coverage above.
- Parent independently reran the stale-state, directory, failure-envelope, and duplicate-ID probes. Engineer ran actual cancellation processes; parent reran the archived cancellation probe with the same surviving-write result. Parent ran the verifier and corrected launcher probes. No paid model calls or product source edits.

Passing existing tests demonstrates missing boundary coverage, not resolution of these findings. Archived scripts are review probes, not production regression tests. Some retain the original absolute checkout path; run from this repository after inspecting them. They use temporary repositories, a fake credential, mocked reviewer HTTP, and local launcher assets. Temporary output directories can remain after a run.

Unverified compatibility questions: hooks use an extensionless cache name while Windows installation uses `.exe`, but MSYS may resolve the suffix automatically. Linux simulation is not proof of a Windows defect. Older Claude compatibility also needs native/version testing; the previously verified UserPromptExpansion version was 2.1.266. Neither question is counted among the twelve confirmed findings.

See [the CTO remediation plan](cto-remediation-2026-09-09.md) for sequencing and release gates.
