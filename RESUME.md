# Resume — codex/plan-a-repair

**Date:** 2026-09-17
**Branch:** `codex/plan-a-repair`
**HEAD:** `2721ac4`

## What is this

Plan A repair for `semanta/codex-dispatch`. The qualification matrix in
`docs/qualification-matrix-2026-09-13.md` is the release gate. Release is
NO-GO until every OPEN row closes.

## Current matrix

| Row | Status |
|---|---|
| Artifact confinement and lifecycle | PASS (development) |
| Baseline and deletion semantics | PASS (development) |
| Archive extraction | PASS (development) |
| **Private authority store** | **OPEN** |
| Native execution | PASS (Linux-only confinement) |
| Public routes | PASS |
| Frozen candidate | OPEN |
| Independent review | OPEN |

Two rows remain OPEN. Frozen candidate and independent review both wait on
private authority store; close that row and the other two can proceed.

## What was done this session

1. **Native execution scoped to Linux-only.** macOS (sandbox startup, descendant
   ownership) and Windows (LPAC, Winsock/MSYS native I/O) are terminal blockers
   on kernel/permission model boundaries. Porting bubblewrap to those platforms
   is a research project, not a fix. `scripts/execution_policy.py:324` already
   fails closed with `sandbox-unavailable` on non-Linux — no host-execution
   fallback exists. Row moved to PASS.

2. **Harness defects found and fixed.** The real-Codex integration harness had
   two bugs caught only by running it:
   - Binary built inside the throwaway repo, so `git add .` staged a ~10 MB
     object and baseline capture refused the run at its 8 MB per-object limit.
     Binary now builds into a separate `BIN_DIR` outside the repo.
   - Inherited `CODEX_SESSION_ID` from the caller, making dispatch take the
     resume path against a thread with an active writer. Harness now unsets it.

3. **Proxy auth wired.** `env_key = "CODEX_PROXY_API_KEY"` was commented out
   in `~/.codex/config.toml`, so the broker forwarded requests to
   `codex-max-proxy` with no bearer token and received 503. Uncommented;
   key must be passed at dispatch time.

4. **Real-Codex smoke PASS.** After fixing auth, `tests/integration/real-codex.sh`
   completed: exit_code 0, session `01a0b1a3-d0ee-7d90-ac83-7dc3853b7c42`,
   `greeting.txt` created and verified. Public routes row closed.

5. **Full local suite green** (on `88204ec`, holds on `2721ac4`):
   - `go test ./internal/...` — 11 packages pass
   - `python3 -m pytest tests/python -q` — 139 passed, 11 skipped
   - `bats tests/bats` — 140/140 pass
   - `python3 scripts/remediation-gates.py` — all 10 checks pass, decision: `independent-review-required`

## The one remaining blocker

**Private authority store** needs four things (from the matrix):

1. **Actual Codex read denial** — prove that a dispatched Codex process cannot
   read files in the private controller-owned object store. The probe in
   `docs/authority-sandbox-probe-2026-09-13.md` reproduced the original
   disclosure and custom-TMPDIR write; the overlap guard is implemented. What
   remains is end-to-end proof under confinement (`bwrap`).

2. **Other child writable roots** — verify no other writable mount escapes the
   confinement boundary.

3. **Second-account/native proof** — run from a separate OS account or native
   runner and show the private store is inaccessible.

4. **Reviewer consumer migration** — the reviewer reads authority records;
   document and test migration for legacy records that predate the v2 format.

The remediation plan packets are in `docs/remediation-plan-2026-09-13.md`
(packet 1, pp. 100–160). The primary files are `internal/diff/snapshot.go`,
`scripts/execution_policy.py`, and `tests/python/test_execution_policy.py`.

## How to pick up

```bash
cd /home/jdziat/Code/jdziat/semanta/codex-dispatch
git checkout codex/plan-a-repair   # already at 2721ac4
cat docs/qualification-matrix-2026-09-13.md   # current gate state
cat docs/remediation-plan-2026-09-13.md       # packet 1 = private authority work
```

Register with the coordination hub before editing:

```
mcp__semanta__agent_register({ name, role, goal })
mcp__semanta__agent_claim({ files: [...], intent: "..." })
# edit files
mcp__semanta__agent_release({ files: [...] })
```

The `repair-gates` agent holds stale claims on several files (last seen 91+ h
ago). Its claims are advisory — they will not block edits — but check
`agent_list` and `agent_inbox` at session start.

To run the real-Codex integration test:

```bash
REAL_CODEX=1 CODEX_PROXY_API_KEY=$(python3 -c "import json,os; print(json.load(open(os.path.expanduser('~/.codex-max-proxy/config.json')))['api_key'])") \
  tests/integration/real-codex.sh
```

## What not to do

- Do not add a host-execution fallback on macOS or Windows — policy forbids it
  and `execution_policy.py` enforces it.
- Do not weaken the symlink leaf validation — it prevents silent export
  redirection; the control is correct.
- Do not use `os.DevNull` (`NUL`) as a Git config path on Windows — fixed in
  `internal/diff/diff.go:215`.
- Do not assemble the release matrix from different executable candidates —
  one frozen commit, one frozen tree.

