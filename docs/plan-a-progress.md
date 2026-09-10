# Plan A implementation progress

Baseline: `b09902c2343e0d07d3006c94ab2cc960ccdbad2e`.
Plan: [repository teardown](repo-teardown-2026-09-08.md).

## 2026-09-09: authenticated broker transport

Implemented the authentication portion of F01:

- Atomic version-2 discovery with a fresh 256-bit bearer credential per broker.
- Credential file created with Unix mode 0600 or a protected current-user-only
  Windows DACL supplied at file creation, avoiding an inherited-permission window.
- Loopback-only listening and numeric-loopback-only client destinations.
- Authentication before body parsing or handler invocation; browser Origin and
  fetch metadata, non-loopback Host, and unsupported Content-Type rejected.
- Proxy-free HTTP transport; redirects rejected to prevent forwarding credentials.
- All existing HTTP client routes consume authenticated discovery. Legacy records
  fail closed with an explicit stop/remove-stale-discovery/retry procedure.
- Regression coverage for unauthorized mutation, browser metadata, hostile Host,
  listener binding, invalid discovery, credential rotation/permissions, redirects,
  proxy configuration, migration, and an actually connected hung hook request.

The CTO reviewed this bounded slice and identified the Windows creation-time
ACL requirement and migration recovery gap; both are addressed in source.
Windows native execution remains unverified on this Linux host.

## 2026-09-09: execution destinations and admission

Implemented production policy wrappers for synchronous and detached execution:
registered-worktree/CWD validation, server-configured external result roots,
mode/sandbox/resume validation, and confined creation of a newly owned log file.
Destinations are rechecked after queueing and before the model turn. Existing log
hard links are replaced without altering their target; symlink escapes are rejected.
Relative result paths retain caller-directory semantics, and clients no longer
truncate log files before broker validation. Go 1.25 is now the declared minimum
for the confined filesystem APIs; workflow toolchain versions match.

Admission is atomic at 64 queued/running tasks before ring-buffer allocation or
background launch. A separate 16-check limit bounds request-time Git discovery.
Tests cover parallel saturation/recovery, both execution routes, invalid requests
before side effects, registered/unregistered worktrees, external results, symlink
escapes, queued path replacement, hard links, and relative result paths.

Residual boundary: CWD remains a path supplied to Codex; this is not an isolation
mechanism against concurrent hostile filesystem mutation by the same OS user.
The overall NO-GO remains until preservation and acceptance gates are implemented.

## 2026-09-09: fresh/resumed thread settings

Broker-created fresh, resumed, and fallback threads now receive the same CWD,
sandbox, model, and noninteractive approval policy. Before starting a turn, the
broker requires response evidence matching requested CWD/model/sandbox mode and
approval policy. Missing or contradictory settings fail the dispatch. Protocol
tests cover each mismatch on both fresh and resumed paths.
Unexpected legacy approval requests now receive explicit denial instead of
automatic consent, with a regression check that the request still receives a response.

Invalid-parameter errors are no longer automatically classified as stale sessions.
A real app-server probe observed `-32600: no rollout found for thread id ...` when
resuming a newly created thread without a saved turn; that precise response now
has stale-session regression coverage. The probe started no model turn and did
not establish successful live resume interoperability. Live model-turn and native
platform validation remain outstanding release gates.

## 2026-09-09: preservation and replay checkpoint

F05 now compares raw pre/post Git trees, retaining pre-run WIP under a dedicated
local ref and copying the original index. Regression tests cover dirty-to-HEAD,
untracked deletion, binary replay, literal pathspec names, symlinks, modes, lossy
clean filters, missing/corrupt evidence, and unchanged live staging. Dispatch
requires its snapshot and reports capture failure explicitly, with no usable
patch advertised. Initial raw snapshotting invoked Git once per file; a measured performance
regression prompted batching regular-file hashes while retaining raw bytes. Special files/submodules fail
explicitly rather than silently losing their content.

F06 preflights all destinations before writing, stages candidate/original copies,
and rolls back ordinary apply failures. Failed outputs and recovery evidence
survive cleanup and startup GC. CTO review caught clean-CRLF false positives and
an interruption window; Git normalization now decides WIP separately from raw
conflict fingerprints, and rollback records intent before mutations.

F07 requires the patch and recorded full baseline commit, normalizes relative run
paths, preserves apply diagnostics, and verifies at that commit after HEAD moves.
F10 preserves caller-relative seed and convention paths during module auto-scope.

Validation: all nine Go packages passed `go test -race ./...`; the subsequent
capture-error regression passed dispatch race tests. Six clean-verify Bats cases,
fan-in integration cases, and Python filesystem fault-injection tests passed.
Vet and golangci-lint passed (0 issues). Native platform execution, live resumed
model turns, large-repository snapshot cost, and crash-time recovery remain
unverified. These are bounded preservation fixes, not overall release approval.

## 2026-09-09: acceptance, release gates, and durable outcomes

F03/F04 now use one acceptance decision for progress and ledger results. The
runner requires strict successful task evidence matching independently observed
raw dispatch edits, audits the union of dispatch and verification changes, and
requires passing verification. It serializes each parent lifecycle in both
isolation modes. Legacy done text cannot bypass execution; accepted evidence must
match current file fingerprints and a valid completion record. Explicit skipped
verification remains unverified. Scope violations are reported, not automatically
reverted in the operator checkout.

F08 routes tag publication through the reusable CI workflow, with publication
permissions confined to the dependent release job. F09 enforces an 80 percent
per-fixture reviewer threshold (default ten runs), rejects failed invocations,
and distinguishes skipped execution (77) from success. Fake CLI tests exercise
failure, count validation, empty selection, skip, and 8/10 versus 7/10 thresholds.

F11 persists task status before admission/state-transition acknowledgements.
Restarted unfinished tasks become explicit unknown-outcome errors without replay;
completed outcomes survive memory eviction. CTO review caught readable stale
archives after failed persistence: failed terminal states are now pinned and new
admission stops until storage repair/restart. Regression tests cover that failure
with real write-denying permissions, restart, eviction, event loss, and unsafe IDs.

The fixed corpus and premeasurement promotion targets are in
`docs/plan-a-benchmark.md`. No live benchmark result is claimed.

## Remaining promotion work

1. Final local integration passed: 131 Bats cases, eight Python tests, all nine
   Go race packages, vet/lint, and Windows amd64/macOS arm64 cross-builds. The
   added broker process-restart Bats case and persistence-failure race tests
   also passed. CTO found no new blocker after the persistence-eviction fix.
2. Exercise native Windows/macOS authentication, filesystem and restart behavior.
3. Live Linux smoke passed on codex-cli 0.153.4: fresh and resumed turns
   shared one session, returned task exit zero, and produced exact requested
   hello/world contents without fallback. Evidence: `/tmp/plan-a-live-d9_ceete`.
   Broader vendor/configuration coverage remains unverified.
4. Run the fixed live benchmark/reviewer corpus, including snapshot overhead.
5. Exercise the reusable workflow in hosted CI before any release publication.

All original findings have implementation changes or explicit bounded contract
changes. This does not constitute release approval: live/native/hosted gates are
still partly unverified. Plan A (Repair) remains incomplete and the original NO-GO
recommendation remains. No release, commit, push, or remote setting was changed.
The goal tracker was observed paused; task tools cannot resume that state.

Final performance screening found and corrected per-file process overhead:
regular-file raw hashing now uses one Git batch with byte-safe C quoting.
Single observations improved baseline-plus-capture from 10.287s to 0.377s at
1,000 files and 103.110s to 2.080s at 10,000 files. See the benchmark protocol
for measurement limits. Invalid-UTF-8 filesystem names were unsupported on this
host; their quoting is unit-tested separately from supported-name replay tests.


## Native and live evaluation checkpoint, 2026-09-09

Plan A (Repair) continues under CTO NO-GO. Review branch `codex/plan-a-repair`
has been pushed for authorized hosted validation; no merge or release occurred.
Candidate 0be939f passed local nine-package Go race tests, lint (zero issues),
80 selected native shell cases and eight Python cases. Hosted run 34417094793
passed Linux amd64/arm64 and macOS arm64; Windows exposed an ineffective
permission fault fixture, now changed to native sharing-lock replacement denial.
The prior failed runs remain available as evidence.

The initial complete reviewer cohort in `/tmp/plan-a-reviewer-20260909` scored
7/10 for fail-approach and 10/10 for each other fixture. The decision ordering
has been corrected in both reviewer and inline orchestrator instructions; a
separate complete cohort is running in `/tmp/plan-a-reviewer-20260909-v2`.
Standalone Sonnet accuracy is not evidence of inline Haiku accuracy.

Additional native tests exercise protected Windows credentials against a second
unprivileged principal with an accessible control, Windows Job descendant
termination/handle-close/owner-crash, and the production broker restart shell
path. Cross-compilation is not credited as native execution. Full hosted pass,
reviewed-route benchmark and CTO final review remain open gates.


### Evidence collection correction and policy confirmation

The v2 reviewer responses matched all 70 expected judgments, but the harness
itself exited with a syntax error after a concurrent comment edit shifted Bash's
read offset. It is retained as a failed harness run, not a passing gate. The full
cohort has restarted from `/tmp/plan-a-reviewer-frozen-v3`, with results in
`/tmp/plan-a-reviewer-20260909-v3`.

Live policy confirmation in `/tmp/plan-a-live-policy-v2` exercised fresh and
same-session resume against a conflicting danger-full-access configuration.
Both actual turn contexts record workspace-write, approval never and gpt-5.5;
hello/world output is exact. The earlier mismatched smoke came from a local
zsh startup override before broker admission, not a demonstrated broker bypass.

Windows native Go tests passed at 2b30bc4. Windows shell execution then exposed
hard-coded `/bin/bash` and POSIX-to-native result-path handling, fixed in b9f6ca4;
hosted run 34418133987 is validating those corrections. Linux and both macOS
architectures have passed native Go, artifact launch and shell gates.


### Reviewed-route repair candidate

The internal evidence helper preserves full diffs and command logs, deduplicates
identical test/verify commands, and leaves acceptance to independent Haiku review.
It fails closed on incomplete/oversized evidence and tracks verification source
and staging mutations. Clean verification now audits inside its temporary
worktree before cleanup and exits 66 if verification alters the reviewed tree.
The zero-text-line no-changes heuristic was removed from all four review routes;
binary and mode-only edits remain meaningful. Seventeen Python regressions and
six clean-verify Bats cases pass locally. CTO bounded recheck found no further
blocker in these corrections; promotion GO remains pending live/native gates.

### Direct-review smoke and native coverage checkpoint

The immutable nine-fixture reviewer cohort in
`/tmp/plan-a-reviewer-20260909-v4` completed successfully: every fixture scored
10/10, including binary deletion and mode-only changes. The original failed
reviewer and harness evaluations remain preserved.

Hosted run 34419044616 passed Go race/lint, shell, Linux and both macOS native
jobs. Windows x64 failed the interruption shell fixture; Windows arm64 exposed
a test that incorrectly required a ping after the valid idle deadline. Repairs
use native CTRL_BREAK interruption and assert survival through active dispatch.
Additional Windows tests now run rather than skip vendor lookup, handshake,
detached setup, prompt parity and goroutine recycling. Local race tests, lint,
Windows cross-compilation, shell checks and 25 Python regressions pass; native
execution of these changes remains required.

The bundled delegated smoke v3 passed all twelve behavior/route audits but
missed economics: median latency 61.653s versus 16.655s direct (3.70x), total
model spend $1.0405399 versus $0.448473. These are excluded smoke observations.
Direct-command smoke v4 loaded the exact contract on Haiku but bypassed Codex,
edited the task file itself, and claimed success without a run/session. Its
31.26s result is rejected as false completion, not credited as an improvement.
Evidence is preserved in `/tmp/plan-a-paired-smoke-v4`, including the persisted
command transcript. Promotion remains CTO NO-GO. The next architecture under
investigation dispatches deterministically before independent review inference.

The auditor now retains failed-attempt spend, unknown-cost vetoes, all planned
rows and typed safety violations, and verifies actual contract expansion.
Median individual task cost is diagnostic; the fixed economic gate remains
total model spend divided by accepted tasks, at most 1.25x direct.

### Deterministic command entry and compact-profile candidate

The installed Claude 2.1.266 probe confirmed UserPromptExpansion receives
literal argument text through JSON stdin and can block before any inference.
The hook now dispatches before Haiku review and injects an identity-bound
receipt. Exclusive creation prevents duplicate execution; failed/pending
receipts cannot be silently retried. Complete hook smoke v5 passed all twelve
independent audits but failed economics (see benchmark protocol).

The next measured candidate is the shipped compact-review launcher, approved
by the CTO before measurement with all gates unchanged. It disables Haiku
thinking only in its child process and uses a review-only system role. A new
immutable fixture harness measures the exact deployed rubric on that model and
configuration. Pure-function/static criteria are distinguished from runtime
integrations, matching the standalone reviewer's existing rule.

Hosted run 34420880831 passed Go/race/lint and all Linux/macOS native gates.
Both Windows architectures passed native Go and 90/91 shell cases. The remaining
interruption fixture failed because AllocConsole was called after a zero window
handle and returned access denied. The next correction uses the console process
list to detect attachment; the headless-console explanation remains a hypothesis
until native validation. No interruption test was skipped or weakened.

### Native cancellation result repair

Hosted run 34422145400 exposed a genuine cancellation race on macOS arm64:
`turn/start` returned context cancellation after the table was already cancelled,
but dispatchFailure unconditionally emitted/returned errored. A deterministic
three-state regression reproduced the mismatch before repair. Failure handling
now preserves an existing terminal outcome, and focused cancellation race tests
pass twenty repetitions.

Cancellation records now always carry a nonzero code. The dispatch adapter also
rejects zero-success for every state other than done, preventing older/malformed
cancelled replies with real edits from writing success result.json. An actual
broker-to-result regression preserves partial edits while requiring failure.
Windows now passes all 91 native shell cases; the newly reached Python failure
was a fixture assuming chmod creates POSIX execute bits on Windows. It now
checks the source mode actually preserved, retaining the explicit POSIX 0755,
native symlink and deletion checks.
