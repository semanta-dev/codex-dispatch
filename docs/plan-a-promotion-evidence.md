# Plan A (Repair): promotion evidence

Measured product candidate: `cedf3ad`. Date: 2026-09-09 (America/Los_Angeles).
**CTO final decision: GO for promotion of the qualified profile below.**
The CTO independently reran the exact frozen audit; the entire summary matched,
with no errors and every agreed gate passing. All 60 row accounting results,
30 API histories/unique responses, zero parent usage, frozen hashes, 90 fixture
judgments and same-commit CI were independently checked. Documentation-only
closeout does not change the measured product candidate.
See [recorded CTO decision](plan-a-evidence/cto-decision-cedf3ad.md).

## Qualified profile

The candidate is the public `scripts/codex-reviewed.py --review-transport api`
profile, invoking the actual `/codex-dispatch:codex` command and its validated
UserPromptExpansion hook. Python controls dispatch, evidence and bounded repairs;
Haiku 4.5 returns one forced independent review decision with thinking disabled.
The parent Claude CLI (2.1.266) stops at the hook without model inference.
The review receives the exact frozen rubric directly; the parent does not receive
an expanded review contract. Receipt, prompt, repository, request, session and
run identities are reconciled independently.

Measured transport: explicitly configured `http://localhost:44444/v1/messages`,
with published Anthropic rate equivalents, not a claim about proxy billing.
Codex CLI 0.153.4 uses GPT-5.5, medium reasoning, workspace-write, approval never,
no MCP, and isolated configuration/shell startup. Default existing-session slash
commands and the separate CLI reviewer profile have no corresponding economics
qualification. No release, deployment or merge was performed.

## Full frozen cohort

The first promotion cohort contains all six cases, five repetitions per arm,
with alternating arm order. All 60 planned trials are retained. No smoke rows
were pooled into it; no failed individual trial was replaced. There were zero
observed human code-repair minutes in either arm.

| Metric | Direct Codex | API-reviewed plugin | Gate |
|---|---:|---:|---|
| Independently accepted | 30/30 | 30/30 | At least 27/30 and no worse than direct: pass |
| Critical safety violations | 0 | 0 | Zero: pass |
| Median end-to-end latency | 18.1097 s | 21.7923 s | 1.20335x, limit 1.25x: pass |
| p95 accepted latency (nearest rank) | 32.0421 s | 36.3485 s | Diagnostic |
| Maximum observed latency | 33.3129 s | 44.1707 s | Diagnostic |
| Total model spend | $2.242747 | $2.412180 | Includes every attempt/review |
| Spend per accepted task | $0.0747582 | $0.0804060 | 1.07555x, limit 1.25x: pass |
| Cost accounting | Complete | Complete | No unknown usage: pass |
| Human code repair | 0 minutes | 0 minutes | Accepted without human repair |

The plugin is 20.3% slower at the median and 7.6% more expensive per accepted
task in this cohort. It passes the agreed overhead limits; it is not a speedup.
The small synthetic corpus is a promotion screen, not a general reliability
claim or a guarantee for large repositories, long tasks or other transports.

Codex token totals (input includes cached input): direct 1,297,457 input,
1,024,384 cached input, 12,173 output; plugin 1,327,373 input, 1,044,224 cached
input, 12,132 output. The plugin's Haiku reviewer used 101,963 input and 1,680
output tokens, with zero cached, cache-write or thinking tokens. Thinking is
not added again to output totals.

## Reviewer and native gates

The exact frozen API profile/rubric scored 90/90 judgments: 10/10 on each of
fail-approach, fail-criterion, fail-quality, fail-scope, fail-tests,
pass-binary-deletion, pass-mode-only, pass-no-tests-flag and pass-simple. Each
fixture required at least 8/10. All raw responses, usage and expected judgments
are retained; prior Sonnet/CLI fixture runs were not substituted.

[CI 34426597509](https://github.com/semanta-dev/codex-dispatch/actions/runs/34426597509)
passed all nine jobs at `cedf3ad`: Go/race/lint, shell, native Linux x64/ARM,
macOS Intel/ARM, Windows x64/ARM, and snapshot/Bats. The native failure-injection
suite covers C07-C10, including rejection/completion, scope/WIP, recoverable
fan-in, durable state and interruption cleanup. The API slice's 56 local Python
tests include actual trickling-response wall-deadline enforcement, forbidden
retry-after-pass, and retained failed-attempt spend.

Ten independent snapshot measurements produced median 0.353 s for 1,000 files
and 1.977 s for 10,000 files (p95 0.409 s and 2.082 s). Relative to the measured
plugin median these are about 1.6% and 9.1%, below the 10% expansion flag.

## Artifacts and reproduction

Committed compact evidence:

- [Full cohort summary](plan-a-evidence/paired-summary-cedf3ad.json)
- [Frozen manifest and published pricing](plan-a-evidence/manifest-cedf3ad.json)
- [Reviewer fixture results](plan-a-evidence/fixture-results-cedf3ad.json)
- [Exact-commit CI results](plan-a-evidence/ci-cedf3ad.json)

Retained raw evidence on the measurement host:

- `/tmp/plan-a-paired-full-v1`: all 60 input repositories, receipts, API requests,
  responses, Codex rollouts, process results, patch replay and per-row audits.
- `/tmp/plan-a-paired-full-v1.driver.log`: complete driver output.
- `/tmp/plan-a-haiku-api-fixtures-v1`: immutable rubric/profile and all 90 judgments.
- `/tmp/plan-a-snapshot-benchmark-10.txt`: raw snapshot repetitions.
- Official pricing is archived in the cohort's `official-pricing.md` and
  `official-review-pricing.md`, covered by its frozen SHA-256 manifest.

Reproduce the independent audit on that host without rerunning model trials:

```sh
python3 /tmp/plan-a-paired-full-v1/audit.py \
  /tmp/plan-a-paired-full-v1 /tmp/plan-a-benchmark-private-codex
```

Earlier smoke archives v1-v11 remain excluded and retained, including vendor
schema failure v7 and the slower CLI profiles. The changes that followed those
failures are documented in [progress](plan-a-progress.md).
