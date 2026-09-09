# Repository teardown and CTO remediation path

Review target: `b09902c2343e0d07d3006c94ab2cc960ccdbad2e`, plugin version 0.5.0. Review requested September 8, 2026. Working tree was clean before the review. This report is the only repository change; reproduction code ran in disposable repositories.

## Decision: NO-GO for trusted autonomous execution

The repo's central promise is bounded, verified delegation. Its default plan runner can declare a failed task complete, its scope boundary is optional depending on the execution route, and its broker exposes execution capabilities without authenticating callers. The safeguards do not compose into a trustworthy system.

The independent CTO review used `/home/jdziat/.claude/agents/cto.md` through a Codex subagent. It was not a separate Claude/Opus invocation. The CTO's final ordering is: close execution access, preserve user changes, restore trustworthy completion, then enforce release gates.

**Assessment: D overall; F for execution trust and completion correctness.** Confidence is high in reproduced defects and medium in overall launch readiness. This is a recommendation to withhold release, not a claim that any release or feature was actually disabled.

## Findings, ordered by risk

### F01 · Critical: localhost execution RPC has no caller authentication

Evidence: `internal/broker/server.go:119`, `internal/broker/handlers_task.go:103`, `internal/broker/handlers_dispatch.go:87`, `cmd/codex-dispatch/broker.go:44`.

`POST /rpc` dispatches requests without a token, peer identity, Origin check, Host check, or Content-Type restriction. A mode-0600 address file does not authenticate traffic to the TCP port. `task.start` accepts caller-supplied prompt, sandbox, working directory, and log path; these are forwarded to execution and file-opening code. A caller can request `danger-full-access` directly. Client-side path selection is not a server authorization boundary.

**Reproduced:** an unauthenticated `text/plain` request with `Origin: https://untrusted.example` invoked a registered `task.start` handler and received HTTP 200. The probe used `httptest` and a harmless handler. Actual arbitrary commands were not executed. Reachability from other local users is a consequence of the TCP transport; browser exploitation also depends on port discovery and the browser's local-network protections, which were not tested. This is not evidence of direct internet exposure.

**Fix:** authenticate every RPC before dispatch, using a per-broker secret distributed through an owner-only file or an equivalent authenticated local transport. Reject unsupported origins and content types as additional defenses. Validate canonical working-directory and artifact destinations at the broker, with an explicit policy for legitimate linked worktrees and externally configured result directories. Pin the permitted sandbox policy server-side. Add admission limits; the running-task semaphore does not cap the queued-task allocation in `Table.Start`.

### F02 · Major, security-sensitive: resume ignores requested execution policy

Evidence: `internal/broker/handlers_dispatch.go:194`, `internal/codex/appserver/appserver.go:1058`, `internal/codex/appserver/appserver.go:1125`.

Fresh threads receive CWD, sandbox, and model. Existing threads are resumed with only `threadId`; the subsequent turn supplies none of those overrides. A caller requesting a narrower sandbox or different module/model cannot rely on that request being honored during resume. Sandbox preflight probes the requested policy but does not bind the resumed thread to it.

**Boundary verified:** generated JSON Schema from installed `codex-cli 0.153.4` explicitly supports `cwd`, `sandbox`, `model`, and `approvalPolicy` on `ThreadResumeParams`. The resume response also exposes effective policy information, which the wrapper currently discards. Effective policy inheritance in a live resumed model turn was not exercised.

**Fix:** carry the same validated execution contract through fresh, resumed, and fallback paths; verify effective response settings before starting a turn. Add resume tests that change each setting and reject a policy mismatch. Audit the blanket legacy approval handler at `internal/codex/appserver/appserver.go:524` against that contract instead of treating unexpected approval requests as automatic consent.

### F03 · Major: failed or undocumented dispatches become completed packets

Evidence: `scripts/graphrag-plan-runner.py:454`, `scripts/graphrag-plan-runner.py:487`, `scripts/graphrag-plan-runner.py:503`, `internal/dispatch/run.go:20`.

Go intentionally returns process success after a completed dispatch transaction while putting task failure in `result.json.exit_code`. The plan runner checks the process status, not the task status. Missing/malformed result files become an empty dictionary. It can then write `Status: done` and unblock dependent packets.

**CTO reproduced in disposable repos:** a zero-exit fake dispatch returning task exit 124 produced runner exit 0, a passing ledger, and a done record. A zero-exit dispatch producing no result file did the same. A `true` verification command isolated the broken harness contract; it did not establish successful implementation.

**Fix:** require a valid, versioned result object, successful task outcome, required artifacts, and all required verification evidence before marking completion. Failure, cancellation, missing evidence, and intentionally skipped verification need distinct states. Bind completion records to the packet content and execution evidence; a free-standing `Status: done` substring is insufficient evidence for future reuse.

### F04 · Major: the default plan route bypasses scope enforcement

Evidence: `scripts/graphrag-plan-runner.py:431`, `scripts/graphrag-plan-runner.py:472`, `internal/dispatch/run.go:325`.

The default single-tree route invokes raw dispatch without the worktree wrapper's allowed-file audit. Go's outside-seed field is expressly advisory. Python locks cover declared paths within one runner process, so they neither enforce what an agent actually writes nor coordinate independent runner processes.

**CTO reproduced:** a packet declaring `allowed.txt` physically wrote and reported `outside.txt`; the runner still returned success and wrote completion.

**Fix:** make scope acceptance mandatory on every route and evaluate it after verification commands as well as agent execution. Shared-tree concurrent attribution cannot reliably distinguish peer edits from task edits using a global working-tree diff. Use isolated snapshots/worktrees for that guarantee, or explicitly restrict the supported route to serial operation with a preserved baseline. An audit detects violations after the fact; filesystem policy must provide prevention where prevention is promised.

### F05 · Major: erasing existing work can disappear from the result

Evidence: `internal/diff/diff.go:129`, `internal/diff/diff.go:161`, `internal/diff/diff.go:43`.

Post-run candidate paths come from a diff against HEAD, and only those candidates receive content-baseline comparison. If an agent restores a dirty tracked file to HEAD, that file is absent from the candidates. Deleting a pre-existing untracked file has the same omission. The algorithm measures remaining divergence from HEAD, not all changes since dispatch began.

**Reproduced:** captured a baseline containing `valuable WIP`, replaced the file with its committed contents, and ran capture. Result: `FilesChanged=[]` despite erased WIP.

Baseline enumeration also swallows Git errors and can produce incomplete attribution inputs. The comment in `run.go` saying baseline failures are surfaced overstates what `CaptureBaseline` actually returns.

**Fix:** compare the union of pre-run and post-run paths against an explicit snapshot including content, type, and mode. Preserve enough state to recover or diagnose deletion and restoration. Treat baseline capture failures as fatal. Keep the task delta separate from an aggregate HEAD-based patch: including an already-dirty file in a HEAD diff also includes its pre-existing changes.

### F06 · Major: failed worktree fan-in can partially modify the parent

Evidence: `scripts/graphrag-worktree-dispatch.sh:375`, `scripts/graphrag-worktree-dispatch.sh:390`, `scripts/graphrag-worktree-dispatch.sh:402`.

Fan-in checks and copies each path in the same loop. A conflict on a later file arrives after earlier files have already been copied or deleted. The script then exits with failure and ordinarily removes the temporary worktree. It has no transaction-wide restoration step.

**Reproduced:** an allowed two-file packet modified `a.txt` and `b.txt`; the parent had pre-existing WIP in `b.txt`. The wrapper exited 1 on `b.txt`, preserved that WIP, but had already changed parent `a.txt` to the agent's output.

**Fix:** preflight all paths before applying anything, stage the complete change, and implement rollback or a recoverable transaction for mid-apply failures. Define coordination with external writers; the wrapper's lock only coordinates cooperating wrappers. Preserve rejected output for inspection. Test late conflicts, copy failures, deletions, symlinks, and mode changes.

### F07 · Major: clean verification can succeed without a patch

Evidence: `scripts/clean-verify.sh:36`, `scripts/clean-verify.sh:52`, `scripts/clean-verify.sh:54`.

The helper checks that the run directory exists but never requires `diff.patch`. A missing patch skips application and verifies bare HEAD. Relative patch paths are checked in the caller's directory and subsequently interpreted by `git -C` inside the new worktree, producing a misleading application failure. It also uses current HEAD rather than the recorded dispatch baseline.

**Reproduced:** missing patch plus `true` returned 0. A valid patch passed with an absolute run path but returned 65 with the equivalent relative run path.

**Fix:** resolve artifact paths before changing directories; require and validate the artifact; distinguish a legitimate empty patch from a missing one; verify against the recorded baseline or an explicitly selected integration base. Preserve the real Git error. Include binary edits in patch replay coverage, since the current patch writer uses plain `git diff` without `--binary`.

### F08 · Major release risk: publication is independent of validation

Evidence: `.github/workflows/release.yml:3`, `.github/workflows/release.yml:20`, `.github/workflows/ci.yml:3`.

The release workflow publishes every matching tag through GoReleaser without requiring the tagged commit's tests, lint, or integration gates. The separate CI workflow runs for main pushes and pull requests. There is no repository-defined dependency between a passing check and publication.

**Fix:** make release validation part of the publication dependency graph, check the exact tagged commit, and reconcile tag, VERSION, and plugin manifest versions. Verify required release checks cannot be bypassed by tagging a failing commit. Remote repository rules were not inspected; they may provide additional controls, but they are not evidence of a gate in this checkout.

### F09 · Major validation gap: reviewer evaluation cannot fail its gate

Evidence: `tests/reviewer/run-fixtures.sh:157`, `tests/reviewer/run-fixtures.sh:183`, `.github/workflows/ci.yml`.

The reviewer harness counts mismatches and prints an expected accuracy target, then exits successfully. It suppresses the model command's failures and is not run in CI. This does not provide an enforceable quality gate for the review loop that differentiates the product.

**CTO reproduced:** a fake Claude returned bogus verdict/reason fields for every fixture. All seven fixtures failed while the harness exited 0.

**Fix:** fail deterministic parser/transport/schema errors immediately, enforce a defined fixture threshold, retain raw review evidence, and make the release evaluation mandatory. Separate inexpensive deterministic contract checks from a budgeted stochastic reviewer evaluation; do not make ordinary CI depend on an unbounded paid model run.

### F10 · Major: module auto-scoping changes the meaning of seed paths

Evidence: `internal/dispatch/run.go:80`, `internal/dispatch/run.go:305`, `internal/dispatch/run_test.go:650`.

For a root invocation with `CODEX_FILES=server/seed.go`, auto-scoping changes WorkDir to `server/`. Prompt construction then reads `server/server/seed.go`. The existing test asserts only the selected CWD, not preservation of seed contents.

**Reproduced:** dispatched with an existing `server/seed.go` containing a unique marker. The resulting prompt omitted the marker after module auto-scoping.

**Fix:** resolve file inputs against the original caller directory before narrowing execution CWD. Keep execution paths, prompt labels, and repository-relative result paths explicit. Test existing seed content, missing seeds, root/module conventions, and explicitly pinned workdirs together.

### F11 · Major operability gap: detached status is not durable

Evidence: `internal/broker/tasks.go:111`, `internal/broker/tasks.go:221`, `internal/broker/handlers_task.go:61`, `README.md:184`.

Detached runs intentionally omit `result.json`; their advertised record combines a streaming log with an in-memory task table. Restarting the broker loses the table, and terminal eviction makes historical task IDs unavailable even without a restart. That limits recovery and auditability for the route users choose to outlive a foreground session. This finding comes from source inspection, not a crash-injection experiment.

**Fix:** persist terminal detached results and identify interrupted work on restart, or narrow the status-retention contract explicitly and provide a durable lookup artifact. Do not imply that an in-memory task table is durable. Define retention and handling of sensitive prompts/tool output stored in logs.

## Why the architecture keeps producing these failures

There is no single owner of acceptance truth. Go records a task outcome, Python interprets a process outcome, the worktree shell script enforces another subset of gates, Markdown tells agents what to do, and a reviewer harness prints judgments without enforcing them. Each component can pass its own tests while the combined workflow accepts invalid work.

A semaphore is not repository isolation. A temporary index protects Git staging operations, not working-tree contents. Prompt fences help structure context but do not enforce filesystem scope. A zero process exit is not successful implementation. An existing done record is not proof that current inputs were verified. Treat these as explicit contracts instead of interchangeable signals.

The broker/app-server lifecycle has substantial race-tested machinery. Preserve that investment. A wholesale language rewrite would consume time without repairing acceptance semantics. First establish one execution contract and one acceptance predicate, then consolidate duplicate policy paths around them.

## CTO scorecard

| Lens | Grade | Blocking basis |
|---|---|---|
| Engineering health | C | Multiple routes implement different acceptance rules. |
| Security and trust | F | Unauthenticated execution endpoint and unbound resume policy. |
| Technical correctness | F | Reproduced false completion, hidden WIP loss, partial fan-in. |
| Product value | C | Clear delegation job; reliable acceptance is the unproven differentiator. |
| Launch readiness | D | Publication independent of checks; reviewer gate ineffective. |
| Strategic suitability | C | Growing scheduler/platform burden without assessed outcome economics. |

Product and strategy grades assess the repository's proposition, not customer demand. No customer, adoption, revenue, or competitor research was performed.

## Plan A (Repair): recommended remediation path

Roles below are proposed accountable owners, not claims that people have accepted assignments. Scope initial support to one well-defined safe execution route. Do not increase supported concurrency or add routing features until the gates pass.

| Phase | Owner | Deliverable | Required exit evidence |
|---|---|---|---|
| 0: Contain | Maintainer and security owner | Hold release promotion; authenticate broker; define allowed path and sandbox policy; bound admission. | Unauthorized requests cannot register a task, open a log, or start a process. Valid clients, hooks, detached calls, linked worktrees, and broker restart still work. Invalid origins are rejected. |
| 1: Preserve | Runtime and Git integration owners | Bind resume policy; correct pre/post attribution; make fan-in transactional/recoverable; repair clean verification and seed paths. | Every F02/F05/F06/F07/F10 reproduction becomes a regression test. Late conflict or injected I/O failure leaves parent contents intact or yields a documented recoverable transaction. Effective resume settings match the request. |
| 2: Accept | Orchestration owner | Versioned execution result; mandatory scope and verification gates; evidence-bound completion; durable detached terminal records. | Task codes 2/4/64/124, absent/corrupt/wrong-shape results, out-of-scope writes, failed verification, cancellation, and stale progress records never release dependents. Test each supported route, not only the worktree branch. |
| 3: Release | Test and release owner | Mandatory tagged-commit validation; effective reviewer thresholds; real-vendor and native-platform smoke. | A failing release check prevents publication. A broken fake reviewer exits nonzero. Real Codex fresh/resume/cancel/error scenarios work on the supported version. Advertised OS routes receive runtime checks or are explicitly experimental. |
| 4: Measure | CTO/product owner | Benchmark accepted outcomes and human repair burden; consolidate duplicate policy definitions. | Report acceptance quality, scope incidents, repair rate, elapsed time, and total model cost against direct execution on a fixed task corpus. Set promotion targets before collecting results. |

Keep phases small enough to review as separate changes. Security containment comes first, but completion-contract work can proceed independently once the shared schema and invariants are agreed. Do not present serialization alone as a fix for false success or unauthorized access.

**Planning envelope from the CTO:** 3–6 engineer-weeks. This is an estimate, not a delivery commitment. Use a 1–2 day design spike to settle authenticated-client migration, baseline representation, and fan-in recovery before committing dates. Native platform scope and authenticated live-vendor testing can change the estimate.

**Migration and rollback:** document how clients discover credentials and reject incompatible brokers; invalidate old unauthenticated brokers when switching. Version new result/progress formats and treat old unverifiable completion records as requiring revalidation. Preserve user baselines and rejected patches outside disposable worktrees. Rolling back to the unauthenticated broker is not an acceptable incident response; stop dispatch while repairing it.

**Skip for now:** a wholesale rewrite, more model-selection heuristics, a new orchestration route, broader concurrency, a dashboard, and speculative abstraction work. None closes the identified execution and acceptance gaps.

**What flips the decision to GO:** a narrow supported route that rejects unauthorized execution, preserves existing user work, rejects invalid outcomes and scope violations, produces replayable verification evidence, survives its documented lifecycle, and can only ship after those properties pass on the release commit.

## Verification and limits

Observed passing checks on the review target:

- `go test -race ./...`: all nine Go packages passed.
- `go vet ./...`: passed.
- `shellcheck scripts/*.sh scripts/hooks/*.sh tests/bats/helpers/setup.bash`: passed.
- `golangci-lint run ./...`: 0 issues using locally installed 2.13.2; CI pins 2.8.0.
- Freshly built binary with `CODEX_DISPATCH_BIN=... bats tests/bats`: all 113 tests passed.
- Disposable Go probes confirmed unauthenticated cross-origin handler dispatch and hidden dirty-file restoration. Disposable shell probes confirmed missing-patch success, relative-patch failure, and partial fan-in. CTO probes confirmed runner false success/scope acceptance and reviewer-harness false success.
- `codex app-server generate-json-schema`: installed 0.153.4 schema confirms resume overrides exist. Go toolchain was 1.25.12.

The probes deliberately assert the presence of the current defect; a passing probe means the defect was demonstrated. They are not fixed-code regression tests.

No paid Claude/Codex model turns, full live exploit, native Windows/macOS runtime checks, remote GitHub protection inspection, dependency vulnerability audit, or crash/soak campaign was performed. The existing real-Codex smoke covers a fresh happy path and is opt-in; it does not resolve those gaps. Cross-compilation in CI cannot prove native lifecycle behavior. No claim is made that this review found every defect or that customers have experienced these failures.

The hard product question: if the wrapper cannot prove stronger bounds and more reliable acceptance than direct execution, what justifies the second model, extra latency, and additional failure modes?
