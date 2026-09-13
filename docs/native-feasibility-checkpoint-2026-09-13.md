# Native feasibility checkpoint — 2026-09-13

This checkpoint records the bounded feasibility result for the reviewed native
execution backends. It is diagnostic evidence only; it does not qualify a
backend for promotion.

| Platform/backend | Result | Blocking observation |
|---|---|---|
| Linux arm64 | Feasible in existing launcher tests | Positive confinement and lifecycle controls pass; frozen-candidate evidence is still absent. |
| Linux amd64 | Unresolved | The prior noisy-output/timing failure remains a qualification risk even though it was not reproduced in local retries. |
| Windows amd64 LPAC | No-go | Raw controls pass, but genuine Go file I/O after WSA startup and MSYS native-object access fail under the constrained token. |
| Windows arm64 LPAC | No-go | The same native I/O limitation prevents a production backend despite passing raw boundary probes. |
| macOS amd64 sandbox | No-go | Shell/Go startup and descendant ownership are not reliable under the required filesystem policy. |
| macOS arm64 sandbox | No-go | The same startup and descendant ownership blocker remains unresolved. |

No platform fallback may claim confinement when its native backend is not
qualified. The release route therefore remains fail-closed until the private
authority store, native controls, route matrix, and independent qualification
are complete.

## Fifth observation: symlinked-ancestor path handling

Candidates `8f9611d` through `bdaf2ed`, CI runs
[34785904353](https://github.com/semanta-dev/codex-dispatch/actions/runs/34785904353)
and [34786672344](https://github.com/semanta-dev/codex-dispatch/actions/runs/34786672344).

The macOS and Windows `native Go tests` failures in these runs were not sandbox
or LPAC blockers. They were a defect in the new authority/result-directory code:
paths were compared and validated without resolving symlinked ancestors. On macOS
every temporary directory is reached through `/var` -> `/private/var`; on Windows
the runner temporary path appears as the 8.3 short name `RUNNER~1`.

Two distinct problems shared that cause. `rejectTemporaryOverlap` resolved only
the temporary side, so `filepath.Rel` compared unrelated namespaces and **accepted
a genuinely overlapping TMPDIR**. That was a real weakness rather than a test
artifact: the added regression fails against the previous implementation on Linux.
Separately, `ensureResultDir` rejected a symlink anywhere in the ancestor chain,
which refused every macOS temporary path and failed 18 native Go tests.

Both are fixed with portable regressions that reproduce the condition on Linux.
macOS native Go failures went from 18 to zero. A symlinked result-directory leaf
is still refused so the export destination cannot be silently redirected.

This corrects the diagnosis only. The LPAC/Winsock/MSYS and macOS sandbox
startup blockers recorded above are unchanged, and native execution remains
unqualified and NO-GO.

### Per-platform Go test counts across these runs

| Platform | `8f9611d` (before) | `bdaf2ed` (after) |
|---|---|---|
| Linux amd64 / arm64 | pass | pass |
| macOS amd64 / arm64 | 18 failures | pass |
| Windows amd64 | 1 failure | pending in-flight run |
| Windows arm64 | 54 failures | 53 failures |

Windows arm64 is dominated by a separate pre-existing defect that is not path
resolution: Git reports `fatal: unable to access 'NUL': Invalid argument` from
`git rev-parse --show-toplevel`, failing 36 `internal/diff` and 15
`internal/dispatch` tests. That count was 54 before this work and 53 after, so it
is untouched by the resolution fix and remains open native debt.
