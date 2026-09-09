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
