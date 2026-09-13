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
