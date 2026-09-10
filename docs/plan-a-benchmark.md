# Plan A (Repair): fixed promotion benchmark

This protocol is fixed before collecting live results. It does not claim that
benchmarks, hosted release validation, or native-platform tests have run.
Changes to this corpus require a new version and a new full comparison.

## Corpus v1

Use disposable repositories and the same input bytes for direct Codex and
codex-dispatch. Never benchmark destructive cases in an operator checkout.
Pin the repaired checkout revision, Go toolchain, Codex CLI, requested model,
sandbox, prompts, acceptance commands and seed contents in each run manifest.
The repository's regression fixtures supply executable negative-case oracles.

| ID | Task and fixture | Required outcome |
|---|---|---|
| C01 | Add one explicitly allowed text file in a clean repository. | Exact requested contents, no outside edits, successful verification. |
| C02 | Append one line to a tracked file already containing operator WIP. | Task delta contains only the appended line; WIP remains recoverable. |
| C03 | Restore a tracked file containing operator WIP to committed contents. | Restoration is attributed as an edit; exact original bytes remain recoverable. |
| C04 | Delete a pre-existing nonignored untracked input. | Deletion is attributed; the deleted bytes remain recoverable. |
| C05 | Add binary content, a symlink and an executable file in allowed paths. | Binary replay, target bytes and executable mode match the request. |
| C06 | Edit a seeded file under an auto-selected Go module. | Prompt includes the original caller-relative seed; execution CWD is the module. |
| C07 | Return a failed turn, missing result, malformed result, or boolean exit code. | No acceptance, completion record, or dependent execution. |
| C08 | Write outside allowed scope, including reverting dirty WIP or undoing that write during verification. | Independent audit fails; progress is not accepted. |
| C09 | Produce two allowed worktree edits while the second parent destination has WIP; inject a second-install failure separately. | Preflight changes neither destination; injected apply failure restores the first; rejected output survives. |
| C10 | Restart the broker after completion and during a queued/running task; inject readable-archive/write-failure plus eviction. | Completed outcome survives; unfinished outcome is unknown/errored; stale running state never resurrects. |

C01-C06 are the paired live task corpus, five repetitions per task and arm.
C07-C10 are deterministic failure-injection gates and run on every supported
native platform before promotion. Run all seven existing reviewer fixtures ten
times each using the same pinned reviewer model. Archive every response, failure,
retry, artifact and verification output; do not rerun only failures out of the
reported denominator. A skip or unavailable credential is unverified, not a pass.

## Targets fixed before measurement

- Zero lost WIP bytes, outside-scope acceptances, false completion records,
  unauthorized RPC executions, or stale-status resurrection in any run.
- All deterministic regression gates pass, including Linux, Windows and macOS
  native broker authentication/status tests. Cross-compilation is insufficient.
- At least 27 of 30 live tasks accepted without human code repair, and no lower
  accepted-without-repair count than direct execution on the same inputs.
- At least 8 of 10 correct verdict/reason pairs for every reviewer fixture;
  invalid/missing model output counts as an incorrect judgment.
- Median end-to-end time and total model cost per accepted task are each no more
  than 1.25 times the direct-execution arm. Include retries, verification and
  reviewer calls. Report tail latency, token totals and human repair minutes.
- Report all numerators/denominators and uncertainty; this small corpus is a
  promotion screen, not a claim about general autonomous coding reliability.

Measure raw snapshot overhead separately on fixed 1,000-file and 10,000-file
repositories with text and binary inputs. Record file counts, total bytes and
median/p95 baseline-plus-capture wall time. Flag overhead above ten percent of
median live task time for optimization before expansion. Regular-file hashes are batched through Git; symlink hashing uses individual
processes. Do not hide snapshot cost in model latency.

Promotion requires CTO review of the complete manifest and evidence, with every
unverified native/vendor boundary explicitly resolved. No new automation surface
or weaker scope concurrency is justified by unit-test success alone.

## Local screening observation, 2026-09-09

Linux amd64, AMD EPYC 9R14, Go 1.25.12; one observation per size using
`go test ./internal/diff -run '^$' -bench BenchmarkSnapshotRoundTrip -benchtime=1x`.
The generated inputs use 112-byte text files and one 1,024-byte binary file per ten
paths. Timings include baseline plus post-run capture, excluding fixture setup.

| Files | Per-file Git hashing | Batched regular-file hashing |
|---|---:|---:|
| 1,000 | 10.287 seconds | 0.377 seconds |
| 10,000 | 103.110 seconds | 2.080 seconds |

These are single observations, not median/p95 promotion results. The filesystem
rejected an invalid-UTF-8 filename before hashing; octal quoting of that byte is
unit-tested, while newline/tab/quote/backslash/Unicode names receive real patch
replay tests. Native filesystem portability remains unverified.

## Repeated snapshot observation, 2026-09-09

Ten fresh fixture repetitions per size, same Linux host and toolchain, using
`go test ./internal/diff -run '^$' -bench BenchmarkSnapshotRoundTrip -benchtime=1x -count=10`:

| Files | Median | p95 (nearest rank) |
|---|---:|---:|
| 1,000 | 0.353 seconds | 0.409 seconds |
| 10,000 | 1.977 seconds | 2.082 seconds |

Raw output: `/tmp/plan-a-snapshot-benchmark-10.txt`. The ten-percent live-task
comparison remains pending the full paired corpus.

## Paired executable protocol

`tests/benchmark/paired.py` freezes separate identical repositories for all
30 cases per arm before running any model. The actual plugin invocation uses
`/codex-dispatch:codex`, its Haiku orchestrator, inline review, and a three-attempt
cap. Direct Codex receives the same task facts and an explicit-session retry
policy driven by the independent oracle. Both use gpt-5.5, medium reasoning,
workspace-write, approval never, and no MCP servers in isolated configuration.

CTO approved the previously unspecified dollar metric as **published-rate-equivalent
USD before paired measurement**. This is not actual proxy billing or promised
bill savings. The 1.25x threshold is unchanged. Archive the dated official source
https://developers.openai.com/api/docs/pricing.md alongside the manifest. At the
2026-09-09 retrieval, gpt-5.5 short-context rates per million tokens were $5 input,
$0.50 cached input, and $30 output. Claude list-basis aggregate cost is counted
once only after reconciling router and subagent usage. Missing usage or
unreconciled aggregation leaves the cost gate unverified.

Smoke runs are instrumentation checks, excluded from promotion denominators.
The first C01 smoke in `/tmp/plan-a-paired-smoke-v1` is invalid for matched
comparison: a local `.zshenv` overrode CODEX_SANDBOX before broker admission.
No broker policy enforcement defect was established. Isolating ZDOTDIR and
shell configuration for both arms corrected actual rollout policies.

The second C01 smoke in `/tmp/plan-a-paired-smoke-v2` passed independent exact
behavior, staging, recovery/patch replay, route, policy and usage audits. Direct
execution took 10.64s at $0.042351 equivalent; reviewed execution took 59.99s at
$0.186675 equivalent. These excluded smoke results indicate a material cost and
latency risk, not a completed corpus result. `tests/benchmark/audit.py` performs
the independent audit and leaves missing evidence as failures.


The lean reviewed-route candidate retains the advertised slash command and
independent Haiku review. It bundles full evidence into one helper call,
deduplicates identical test/verification commands, and launches Claude with
`--tools Agent,Skill,Bash,Read,Grep,Glob --effort low`. These remove unused tool
schemas and redundant execution; all model calls and review costs still count.
The executable manifest pins these settings before measurement. Freeze with:

```sh
CODEX_HOME=/path/to/private-pinned-config python3 tests/benchmark/paired.py freeze /path/to/new-archive --pricing-source /path/to/official-pricing.md
CODEX_HOME=/path/to/private-pinned-config python3 tests/benchmark/paired.py run /path/to/new-archive
python3 /path/to/new-archive/audit.py /path/to/new-archive /path/to/private-pinned-config
```

`--limit` marks a partial smoke, never a promotion cohort. The original seven
reviewer fixtures remain mandatory; binary-deletion and mode-only fixtures add
coverage without replacing any original denominator.

## Compact review configuration, approved before measurement

The CTO accepted `python3 scripts/codex-reviewed.py` as the Plan A candidate
entrypoint before its first measurement. It invokes the actual plugin command
and UserPromptExpansion hook, pins Haiku 4.5 with `MAX_THINKING_TOKENS=0`, and
replaces the general coding-agent system role with the shipped independent
review role in `scripts/compact-review-system.md`. It does not use `--bare`,
which would disable plugin hooks. No global settings are changed. Qualification
covers this documented profile; it does not make a latency/cost claim for an
existing Claude session using its own thinking settings.

All original gates and the fixed corpus remain unchanged. The benchmark calls
the shipped entrypoint, freezes profile/contract/system hashes and records its
configuration. Receipt auditing proves current prompt/session/arguments,
pre-inference evidence delivery, first-run binding, and all final patch/run
attribution. Thinking-token usage must be zero in the compact configuration.

`tests/reviewer/direct-fixtures.py` freezes the exact deployed review-check
section, compact system role, model/configuration and a single-decision adapter.
It evaluates all nine original fixtures ten times, retaining every response and
requiring at least eight matches per fixture. Expected verdict/reason pairs are
unchanged. This isolates review classification from retry-control outcomes;
the actual paired slash route separately validates orchestration. Standalone
Sonnet scores do not substitute for this Haiku decision-quality gate.

Hook smoke v5 accepted all six cases per arm. Its median latency was 39.885s
versus 22.599s direct; total model spend was $0.9191848 versus $0.732622. It
failed economics and remains excluded from promotion. Complete evidence:
`/tmp/plan-a-paired-smoke-v5`.

Compact text smoke v6 accepted all twelve trials with thinking disabled, but
still failed economics: median plugin 35.249s versus direct 20.868s; total spend
$0.7168757 versus $0.494803. Haiku emitted long visible review narratives. This
archive remains excluded: `/tmp/plan-a-paired-smoke-v6`.

The next bounded candidate uses Claude's StructuredOutput tool and validates
its report locally. The installed-CLI controlled-endpoint probe showed a valid
tool response can finish in one API request; a prose-first response incurs an
extra request. This is not an assumed performance win. The shipped profile pins
`MAX_STRUCTURED_OUTPUT_RETRIES=1`; all formatting retries remain charged. The
fixture adapter uses the same mechanism with verdict/reason fields only. It
rejects implementation tools, missing structured output, and thinking blocks.

Structured acceptance additionally requires an invocation ledger: every attempt
is recorded before dispatch under the receipt identity, original request values
remain fixed, resumes stay within that chain, and the final report's count and
run/session must match its completed tail. An old successful run cannot satisfy
a claimed retry. The independent paired audit reconciles the ledger with the
actual run/session inventory, rather than trusting the model's iteration count.

### Diagnostic reporting additions

Future freezes include nearest-rank p95 and maximum accepted latency, maximum
observed latency including rejected attempts, and aggregate known Codex/Claude
token totals including failures. Unknown accounting still vetoes promotion.
Observed human code-repair minutes are recorded explicitly for automated trials
(zero interventions); absent older measurements remain unknown. These are
diagnostics, not new or relaxed promotion thresholds.

The excluded v8 two-trial transport probe accepted both C01 results using the
real API and receipt-bound StructuredOutput validation. Direct: 13.95 seconds
and $0.06121; plugin: 29.73 seconds and $0.0834432. The schema compatibility
repair worked, but this probe does not establish economics or promotion.
