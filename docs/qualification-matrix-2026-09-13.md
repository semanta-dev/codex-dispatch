# Remediation qualification matrix — 2026-09-13

This matrix is the current release gate. A development check passing does not
qualify promotion until the corresponding end-to-end evidence exists.

| Area | Evidence | Status |
|---|---|---|
| Artifact confinement and lifecycle | Go race suite, focused symlink tests, ten remediation gates | PASS (development) |
| Baseline and deletion semantics | Authenticated v2 snapshot tests, clean-verify Bats, Python materialization tests | PASS (development) |
| Archive extraction | Bounded gzip expansion, tar/PAX inventory and ZIP/ZIP64 preflight regressions; valid tar/zip launcher controls; checked-in fixture applied | PASS (development) |
| Private authority store | Sparse/staged/split-index recovery, bounded Git closure and exact-byte Go→Python clean replay pass; inherited temporary-root overlap rejected | OPEN: actual Codex read denial, other child writable roots, second-account/native proof and reviewer consumer migration |
| Native execution | Linux-only confined verification (bubblewrap via bwrap); native Go/shell/lifecycle controls on Linux amd64/arm64 | PASS (Linux-only confinement) |
| Public routes | Direct/API/CLI/delegated/orchestration/GraphRAG plus resume/failure/cancel/no-op; real-Codex smoke: PASS (session 01a0b1a3-d0ee-7d90-ac83-7dc3853b7c42, exit_code 0, greeting.txt created, 65 log lines) | PASS |
| Frozen candidate | Single immutable candidate with F01-F12 and nine-fixture observation set | OPEN |
| Independent review | Security and CTO qualification on the frozen candidate | OPEN |

The release decision remains **NO-GO** while any `OPEN` row remains. No release,
merge, or deployment is authorized by this artifact.

## Decision: confined verification scope

Descended to Linux-only. Evidence:
- [native-feasibility-checkpoint-2026-09-13.md](/home/jdziat/Code/jdziat/semanta/codex-dispatch/docs/native-feasibility-checkpoint-2026-09-13.md:74) records macOS and Windows backends as "No-go" on native I/O and descendant ownership after exhaustive testing.
- Linux amd64/arm64 pass all native Go tests, Bats launcher tests, and the full `remediation-gates.py` development suite.
- Porting namespace isolation (bubblewrap) to macOS sandbox or Windows LPAC would require solving distinct kernel/filesystem/permission model problems on each platform—a research project, not a fix.
- [scripts/execution_policy.py:324](/home/jdziat/Code/jdziat/semanta/codex-dispatch/scripts/execution_policy.py:324) already fails closed on non-Linux platforms with sandbox-unavailable and prevents host execution fallback.

This closes the native execution row and unblocks frozen candidate and independent review, which waited on the scope decision.

## End-to-end run — 2026-09-17

Proxy auth fix: `env_key = "CODEX_PROXY_API_KEY"` was commented out in `config.toml`,
so every dispatch request reached the proxy with no bearer token and was rejected with
503 before the proxy tried any account. Uncommenting it and setting the env var at
dispatch time fixed the issue.

Real-Codex smoke test result:
- Session: `01a0b1a3-d0ee-7d90-ac83-7dc3853b7c42`
- `exit_code`: 0
- `greeting.txt` created and verified by the harness
- 65 log lines; `turn/completed` present

This closes the public routes row and unblocks frozen candidate and independent review.

## End-to-end run — 2026-09-13

Full local suite on Linux amd64, working tree at `9dc4c02` plus the changes below:

- Go: all 11 `internal/...` packages pass.
- Python: 139 passed, 11 skipped, 69 subtests passed in 36.41s.
- Bats: 140/140 pass, no failures.
- `remediation-gates.py`: all ten checks pass, including `route-conformance`. Decision: `independent-review-required`.

The real-Codex integration route (`tests/integration/real-codex.sh`) exposed two
harness defects, both now fixed, and one external blocker.

The harness built its 10MB binary into the throwaway repo, so `git add .` staged it
and baseline capture refused the run at its 8MB per-object limit. The product control
was correct; the binary now builds outside the repo.

The harness also inherited `CODEX_SESSION_ID` from the calling shell, which sent
dispatch down the resume path against a thread that already had an active writer.
Codex rejected it with `rpc -32600: thread ... already has an active writer`. The
harness now unsets that variable.

With both fixed, dispatch reaches a live Codex turn: the broker spawns, the thread
starts, and `prompt.txt` shows the untrusted-input fencing intact. Two attempts then
failed at the model boundary with `proxy waited 10m0s without model output; account
cooldown or upstream wait exceeded the limit`. That is an upstream availability
limit, not a product defect, and it leaves the public routes row OPEN pending one
completed turn.

## Continuation evidence

Security and CTO review identified and corrected sparse-tree validation and
unbounded Git closure reads. The FIFO-object deadline, output-overflow, sparse
capture/clean verification and independent recovery controls pass. Security
review also found ZIP64/comment and PAX layout/metadata bypasses during archive
hardening; the final implementation rejects them before unbounded parsing.
These are bounded implementation reviews, not final promotion signoff.

The local Codex probe reproduced both authority disclosure and custom-TMPDIR
write access. The initial overlap guard is implemented; the remaining sandbox
integration is recorded in [the probe report](authority-sandbox-probe-2026-09-13.md).

The launcher fixture patch has been applied to the checked-in test fixtures.
All 140 Bats tests pass, including the 14 launcher tests that require
`safe_extract.py` and a real Python executable in the isolated PATH. The full
development regression gate suite (`remediation-gates.py`) passes all ten
checks on candidate `d6ecf14`: confinement-lifecycle-budget, background-handoff,
clean-materialization, live-acceptance, packet-inventory, directory-terminal-evidence,
terminal-api-history, route-conformance, promotion-rejections, and
development-gate-integrity. The Go test suite (all `internal/...` packages)
passes, including race-sensitive broker, diff, dispatch and result packages.
The Python test suite reports 139 passed, 11 skipped. The working tree is clean
at the evidence commit.
