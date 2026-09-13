# Remediation qualification matrix — 2026-09-13

This matrix is the current release gate. A development check passing does not
qualify promotion until the corresponding end-to-end evidence exists.

| Area | Evidence | Status |
|---|---|---|
| Artifact confinement and lifecycle | Go race suite, focused symlink tests, ten remediation gates | PASS (development) |
| Baseline and deletion semantics | Authenticated v2 snapshot tests, clean-verify Bats, Python materialization tests | PASS (development) |
| Archive extraction | Bounded gzip expansion, tar/PAX inventory and ZIP/ZIP64 preflight regressions; valid tar/zip launcher controls; checked-in fixture applied | PASS (development) |
| Private authority store | Sparse/staged/split-index recovery, bounded Git closure and exact-byte Go→Python clean replay pass; inherited temporary-root overlap rejected | OPEN: actual Codex read denial, other child writable roots, second-account/native proof and reviewer consumer migration |
| Native execution | Six-platform positive/negative/lifecycle/toolchain controls | OPEN; checkpoint records blockers |
| Public routes | Direct/API/CLI/delegated/orchestration/GraphRAG plus resume/failure/cancel/no-op | OPEN |
| Frozen candidate | Single immutable candidate with F01-F12 and nine-fixture observation set | OPEN |
| Independent review | Security and CTO qualification on the frozen candidate | OPEN |

The release decision remains **NO-GO** while any `OPEN` row remains. No release,
merge, or deployment is authorized by this artifact.

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
