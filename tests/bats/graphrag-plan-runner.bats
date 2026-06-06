#!/usr/bin/env bats

bats_require_minimum_version 1.5.0

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  RUNNER="$REPO_ROOT/scripts/graphrag-plan-runner.py"
  TMP_REPO="$(mktemp -d)"
  export GIT_CONFIG_GLOBAL=/dev/null
  cd "$TMP_REPO" || return 1
  git init -q -b main
  git config user.email t@t
  git config user.name t
  mkdir -p docs/graphrag/plans tests
  printf 'root\n' > README.md
  cat > tests/verify.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
test -f docs/one.md
test -f docs/two.md
grep -q 'Status: done' docs/graphrag/progress/001-one.done.md
grep -q 'Status: done' docs/graphrag/progress/002-two.done.md
EOF
  chmod +x tests/verify.sh
  git add README.md tests/verify.sh
  git commit -q -m init

  FAKE_DISPATCH="$TMP_REPO/fake-dispatch.sh"
  cat > "$FAKE_DISPATCH" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p docs/graphrag/progress .codex-dispatch/runs/fake
case "$CODEX_TASK" in
  *"Packet 001"*)
    printf 'one\n' > docs/one.md
    files='["docs/one.md"]'
    ;;
  *"Packet 002"*)
    printf 'two\n' > docs/two.md
    files='["docs/two.md"]'
    ;;
  *)
    files='[]'
    ;;
esac
printf '{"exit_code":0,"session_id":"fake","files_changed":%s,"lines_added":2,"lines_removed":0,"stdout_path":"%s/.codex-dispatch/runs/fake/stdout.log","diff_path":"%s/.codex-dispatch/runs/fake/diff.patch","fell_back_to_fresh":false}\n' "$files" "$PWD" "$PWD" > .codex-dispatch/runs/fake/result.json
printf '{"method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":10,"cachedInputTokens":5,"outputTokens":3,"reasoningOutputTokens":1}}}}\n' > .codex-dispatch/runs/fake/stdout.log
printf '{"method":"turn/completed","params":{"turn":{"durationMs":100}}}\n' >> .codex-dispatch/runs/fake/stdout.log
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$FAKE_DISPATCH"

  cat > docs/graphrag/plans/demo.plan.md <<'EOF'
# Plan: Demo

Goal:
Exercise packet orchestration.

---

## Packet 001: One

Objective:
Create one.

Inputs:
- `tests/verify.sh`

Allowed files:
- `docs/one.md`
- `docs/graphrag/progress/001-one.done.md`
- `docs/graphrag/progress/001-one.blocked.md`

Disallowed changes:
- Do not edit README.md.

Acceptance criteria:
- docs/one.md exists.
- progress record is done.

Implementation notes:
- Keep it small.

GraphRAG context:
- None.

Verification:
```bash
test -f docs/one.md
```

Progress record:
`docs/graphrag/progress/001-one.done.md`

Rollback:
Delete created files.

## Packet 002: Two

Objective:
Create two.

Allowed files:
- `docs/two.md`
- `docs/graphrag/progress/002-two.done.md`
- `docs/graphrag/progress/002-two.blocked.md`

Disallowed changes:
- Do not edit README.md.

Acceptance criteria:
- docs/two.md exists.
- progress record is done.

Implementation notes:
- Keep it small.

GraphRAG context:
- None.

Verification:
```bash
test -f docs/two.md
```

Progress record:
`docs/graphrag/progress/002-two.done.md`

Rollback:
Delete created files.
EOF
}

teardown() {
  cd /
  rm -rf "$TMP_REPO"
}

@test "runs independent GraphRAG packets single-tree by default (no git worktree)" {
  worktrees_before="$(git worktree list)"
  run "$RUNNER" docs/graphrag/plans/demo.plan.md --out runner-out --jobs 2 --dispatch-command "$FAKE_DISPATCH"
  [ "$status" -eq 0 ]
  [ -f docs/one.md ]
  [ -f docs/two.md ]
  [ -f runner-out/ledger.json ]
  # Default is single-tree: no worktrees created, worktree list unchanged.
  [ "$(git worktree list)" = "$worktrees_before" ]
  [ ! -d .git/worktrees ]
  [ ! -d .codex-dispatch/graphrag-worktrees ]
  python3 - <<'PY'
import json
ledger = json.load(open("runner-out/ledger.json"))
assert ledger["status"] == "pass", ledger
assert ledger["isolation"] == "none", ledger
assert ledger["packets_run"] == 2, ledger
assert ledger["packets_passed"] == 2, ledger
assert all(record["run_dir"] for record in ledger["records"]), ledger
assert all(
    not any(path.endswith((".done.md", ".blocked.md")) for path in record["dispatch_allowed_files"])
    for record in ledger["records"]
), ledger
PY
}

@test "--isolation worktree routes through graphrag-worktree-dispatch.sh" {
  worktrees_before="$(git worktree list)"
  run "$RUNNER" docs/graphrag/plans/demo.plan.md --out wt-out --jobs 2 \
    --isolation worktree --dispatch-command "$FAKE_DISPATCH"
  [ "$status" -eq 0 ]
  [ -f docs/one.md ]
  [ -f docs/two.md ]
  [ -f wt-out/ledger.json ]
  # The worktree wrapper leaves its run-record directory behind even after it
  # prunes the temporary worktrees, proving the worktree path was taken.
  [ -d .codex-dispatch/graphrag-worktree-runs ]
  python3 - <<'PY'
import json
ledger = json.load(open("wt-out/ledger.json"))
assert ledger["status"] == "pass", ledger
assert ledger["isolation"] == "worktree", ledger
assert ledger["packets_passed"] == 2, ledger
PY
  # Temporary worktrees are pruned on success: list is back to baseline.
  [ "$(git worktree list)" = "$worktrees_before" ]
}

@test "fails fast on missing packet contract sections" {
  cat > docs/graphrag/plans/bad.plan.md <<'EOF'
# Plan: Bad

## Packet 001: Bad

Objective:
Missing required sections.
EOF

  run "$RUNNER" docs/graphrag/plans/bad.plan.md --out bad-out --dispatch-command "$FAKE_DISPATCH"
  [ "$status" -eq 2 ]
  [[ "$output" == *"missing allowed files"* ]]
  [[ "$output" == *"missing acceptance criteria"* ]]
}

@test "writes missing progress records after successful dispatch and verification" {
  no_progress="$TMP_REPO/fake-no-progress.sh"
  cat > "$no_progress" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p docs .codex-dispatch/runs/fake
case "$CODEX_TASK" in
  *"Packet 001"*) printf 'one\n' > docs/one.md; files='["docs/one.md"]' ;;
  *"Packet 002"*) printf 'two\n' > docs/two.md; files='["docs/two.md"]' ;;
  *) files='[]' ;;
esac
printf '{"exit_code":0,"session_id":"fake","files_changed":%s,"lines_added":1,"lines_removed":0}\n' "$files" > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$no_progress"

  run "$RUNNER" docs/graphrag/plans/demo.plan.md --out progress-out --jobs 2 --dispatch-command "$no_progress"
  [ "$status" -eq 0 ]
  grep -q 'Status: done' docs/graphrag/progress/001-one.done.md
  grep -q 'written by graphrag-plan-runner.py' docs/graphrag/progress/001-one.done.md
  grep -q 'Status: done' docs/graphrag/progress/002-two.done.md
}

@test "serializes two packets that share an Allowed file (no shared wave), deterministically" {
  # Both packets write the SAME allowed file. A concurrency-detecting dispatch
  # records its own pid in a lockfile and fails if a peer is mid-dispatch; if
  # the overlap partition never co-schedules them, every run succeeds.
  overlap_dispatch="$TMP_REPO/fake-overlap.sh"
  cat > "$overlap_dispatch" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p .codex-dispatch/runs/fake
busy=".codex-dispatch/overlap.busy"
if [ -e "$busy" ]; then
  printf 'overlap: concurrent dispatch detected\n' >&2
  exit 7
fi
: > "$busy"
case "$CODEX_TASK" in
  *"Packet 001"*) printf 'one\n' >> docs/shared.md ;;
  *"Packet 002"*) printf 'two\n' >> docs/shared.md ;;
esac
rm -f "$busy"
printf '{"exit_code":0,"session_id":"fake","files_changed":["docs/shared.md"],"lines_added":1,"lines_removed":0}\n' > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$overlap_dispatch"

  cat > docs/graphrag/plans/overlap.plan.md <<'EOF'
# Plan: Overlap

## Packet 001: One

Objective:
Append to the shared file.

Allowed files:
- `docs/shared.md`
- `docs/graphrag/progress/001-one.done.md`

Acceptance criteria:
- docs/shared.md changed.

Implementation notes:
- small.

Verification:
```bash
test -f docs/shared.md
```

Progress record:
`docs/graphrag/progress/001-one.done.md`

## Packet 002: Two

Objective:
Append to the shared file.

Allowed files:
- `docs/shared.md`
- `docs/graphrag/progress/002-two.done.md`

Acceptance criteria:
- docs/shared.md changed.

Implementation notes:
- small.

Verification:
```bash
test -f docs/shared.md
```

Progress record:
`docs/graphrag/progress/002-two.done.md`
EOF

  # Repeat to assert determinism: the same partition holds every run.
  for i in 1 2 3; do
    rm -f docs/shared.md docs/graphrag/progress/00*-*.done.md
    run "$RUNNER" docs/graphrag/plans/overlap.plan.md --out "overlap-out-$i" --jobs 2 \
      --dispatch-command "$overlap_dispatch"
    [ "$status" -eq 0 ]
    python3 - "overlap-out-$i/ledger.json" <<'PY'
import json, sys
ledger = json.load(open(sys.argv[1]))
assert ledger["status"] == "pass", ledger
assert ledger["packets_passed"] == 2, ledger
# The shared allowed file is reported as a co-write conflict pre-flight.
conflicts = ledger["overlap_conflicts"]
assert any(
    set(c["packets"]) == {"001", "002"} and c["path"] == "docs/shared.md"
    for c in conflicts
), conflicts
PY
  done
}

@test "schedules dependent packets in order and blocks a branch whose dependency fails" {
  # 001 succeeds; 002 fails its dispatch; 003 depends on 002 and must be blocked
  # (never dispatched) once 002 fails.
  dep_dispatch="$TMP_REPO/fake-dep.sh"
  cat > "$dep_dispatch" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p docs .codex-dispatch/runs/fake
case "$CODEX_TASK" in
  *"Packet 001"*)
    # Record that 001 ran before 003 would be eligible.
    printf 'one\n' > docs/one.md
    printf '{"exit_code":0,"session_id":"fake","files_changed":["docs/one.md"],"lines_added":1,"lines_removed":0}\n' > .codex-dispatch/runs/fake/result.json
    printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
    ;;
  *"Packet 002"*)
    # Dispatch fails: nonzero exit, no result dir.
    printf 'two failed\n' >&2
    exit 1
    ;;
  *"Packet 003"*)
    # Should never run because 002 (its dep) failed.
    printf 'three\n' > docs/three-should-not-exist.md
    printf '{"exit_code":0,"session_id":"fake","files_changed":["docs/three.md"],"lines_added":1,"lines_removed":0}\n' > .codex-dispatch/runs/fake/result.json
    printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
    ;;
esac
EOF
  chmod +x "$dep_dispatch"

  cat > docs/graphrag/plans/dep.plan.md <<'EOF'
# Plan: Dep

## Packet 001: One

Objective:
First.

Allowed files:
- `docs/one.md`
- `docs/graphrag/progress/001-one.done.md`

Acceptance criteria:
- one exists.

Implementation notes:
- small.

Verification:
```bash
test -f docs/one.md
```

Progress record:
`docs/graphrag/progress/001-one.done.md`

## Packet 002: Two

Objective:
Second.

Allowed files:
- `docs/two.md`
- `docs/graphrag/progress/002-two.done.md`

Acceptance criteria:
- two exists.

Implementation notes:
- small.

Verification:
```bash
test -f docs/two.md
```

Progress record:
`docs/graphrag/progress/002-two.done.md`

## Packet 003: Three

Objective:
Third, depends on 002.

Allowed files:
- `docs/three.md`
- `docs/graphrag/progress/003-three.done.md`

Acceptance criteria:
- three exists.

Implementation notes:
- small.

Depends on:
- 002

Verification:
```bash
test -f docs/three.md
```

Progress record:
`docs/graphrag/progress/003-three.done.md`
EOF

  run "$RUNNER" docs/graphrag/plans/dep.plan.md --out dep-out --jobs 1 \
    --dispatch-command "$dep_dispatch"
  [ "$status" -eq 1 ]
  [ -f docs/one.md ]
  [ ! -f docs/three-should-not-exist.md ]
  python3 - <<'PY'
import json
ledger = json.load(open("dep-out/ledger.json"))
assert ledger["status"] == "fail", ledger
recs = {r.get("packet"): r for r in ledger["records"] if "packet" in r}
assert recs["001"]["status"] == "pass", recs["001"]
assert recs["002"]["status"] == "fail", recs["002"]
# 003 was never dispatched (its dependency failed) and is reported blocked.
assert "003" not in recs, ledger
blocked = [r for r in ledger["records"] if r.get("status") == "blocked"]
assert blocked and "003" in blocked[0]["packets"], ledger
PY
}

@test "SIGINT after one wave leaves a valid partial ledger.json with the fanned-in packet" {
  # 001 succeeds in wave 1; 002 (depends on 001) sends SIGINT to the runner
  # instead of completing, so the runner unwinds after wave 1 fanned in.
  sigint_dispatch="$TMP_REPO/fake-sigint.sh"
  cat > "$sigint_dispatch" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p docs .codex-dispatch/runs/fake
case "$CODEX_TASK" in
  *"Packet 001"*)
    printf 'one\n' > docs/one.md
    printf '{"exit_code":0,"session_id":"fake","files_changed":["docs/one.md"],"lines_added":1,"lines_removed":0}\n' > .codex-dispatch/runs/fake/result.json
    printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
    ;;
  *"Packet 002"*)
    # Interrupt the plan-runner (our parent) mid-run, then exit so the worker
    # thread does not hang the executor shutdown.
    kill -INT "$PPID" 2>/dev/null || true
    sleep 0.2
    exit 1
    ;;
esac
EOF
  chmod +x "$sigint_dispatch"

  cat > docs/graphrag/plans/sigint.plan.md <<'EOF'
# Plan: Sigint

## Packet 001: One

Objective:
First wave.

Allowed files:
- `docs/one.md`
- `docs/graphrag/progress/001-one.done.md`

Acceptance criteria:
- one exists.

Implementation notes:
- small.

Verification:
```bash
test -f docs/one.md
```

Progress record:
`docs/graphrag/progress/001-one.done.md`

## Packet 002: Two

Objective:
Second wave; interrupts the runner.

Allowed files:
- `docs/two.md`
- `docs/graphrag/progress/002-two.done.md`

Acceptance criteria:
- two exists.

Implementation notes:
- small.

Depends on:
- 001

Verification:
```bash
test -f docs/two.md
```

Progress record:
`docs/graphrag/progress/002-two.done.md`
EOF

  run "$RUNNER" docs/graphrag/plans/sigint.plan.md --out sigint-out --jobs 1 \
    --dispatch-command "$sigint_dispatch"
  # Interrupted runs return 130.
  [ "$status" -eq 130 ]
  [ -f sigint-out/ledger.json ]
  python3 - <<'PY'
import json
ledger = json.load(open("sigint-out/ledger.json"))  # must be valid JSON
assert ledger["interrupted"] is True, ledger
recs = {r.get("packet"): r for r in ledger["records"] if "packet" in r}
# Packet 001 fanned in before the interrupt and is recorded.
assert "001" in recs, ledger
assert recs["001"]["status"] == "pass", recs["001"]
PY
}

@test "disjoint single-tree packets dispatch in parallel (per-file lock, not a global mutex)" {
  # Two packets claim disjoint files, so they ride the same wave AND must hold
  # disjoint per-file lock sets -> they dispatch concurrently. Each dispatch
  # marks itself in-flight with its own marker file, then waits for the peer's
  # marker to appear; if both markers coexist the dispatches overlapped. A
  # single global lock would serialize them and the wait would time out.
  # Each packet uses a private run dir and result.json so the fixture itself
  # never races on shared scratch state.
  parallel_dispatch="$TMP_REPO/fake-parallel.sh"
  cat > "$parallel_dispatch" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$CODEX_TASK" in
  *"Packet 001"*) me=001; out=docs/one.md; peer=002 ;;
  *"Packet 002"*) me=002; out=docs/two.md; peer=001 ;;
  *) me=other; out=docs/other.md; peer=none ;;
esac
mkdir -p docs ".codex-dispatch/runs/$me" .codex-dispatch/markers
# Atomic per-packet in-flight marker (no shared read-modify-write).
: > ".codex-dispatch/markers/$me"
# Wait (bounded) for the peer to be in-flight at the same time.
for _ in $(seq 1 100); do
  if [ -e ".codex-dispatch/markers/$peer" ]; then
    : > .codex-dispatch/overlap.flag
    break
  fi
  sleep 0.02
done
printf '%s\n' "$me" > "$out"
rm -f ".codex-dispatch/markers/$me"
printf '{"exit_code":0,"session_id":"fake","files_changed":["%s"],"lines_added":1,"lines_removed":0}\n' "$out" > ".codex-dispatch/runs/$me/result.json"
printf '%s\n' "$PWD/.codex-dispatch/runs/$me"
EOF
  chmod +x "$parallel_dispatch"

  run "$RUNNER" docs/graphrag/plans/demo.plan.md --out par-out --jobs 2 \
    --dispatch-command "$parallel_dispatch"
  [ "$status" -eq 0 ]
  [ -f docs/one.md ]
  [ -f docs/two.md ]
  # The two disjoint packets were in-flight simultaneously: not globally serialized.
  [ -f .codex-dispatch/overlap.flag ]
  python3 - <<'PY'
import json
ledger = json.load(open("par-out/ledger.json"))
assert ledger["status"] == "pass", ledger
assert ledger["packets_passed"] == 2, ledger
PY
}

@test "on-disk ledger.json matches the ledger printed to stdout (single build_ledger)" {
  run "$RUNNER" docs/graphrag/plans/demo.plan.md --out match-out --jobs 2 \
    --dispatch-command "$FAKE_DISPATCH"
  [ "$status" -eq 0 ]
  # The stdout JSON and the on-disk ledger must be the same object (same wall_s,
  # same records); a second build_ledger() call would re-snapshot wall_s and
  # diverge.
  printf '%s\n' "$output" > "$TMP_REPO/stdout.json"
  python3 - "$TMP_REPO/stdout.json" match-out/ledger.json <<'PY'
import json, sys
printed = json.load(open(sys.argv[1]))
ondisk = json.load(open(sys.argv[2]))
assert printed == ondisk, (
    "stdout ledger diverges from ledger.json:\n"
    f"  stdout wall_s={printed.get('wall_s')} ondisk wall_s={ondisk.get('wall_s')}"
)
PY
}

@test "overlap partition never co-schedules co-writers and is deterministic" {
  RUNNER="$RUNNER" python3 - <<'PY'
import importlib.util, os, sys
spec = importlib.util.spec_from_file_location("plr", os.environ["RUNNER"])
plr = importlib.util.module_from_spec(spec)
sys.modules["plr"] = plr
spec.loader.exec_module(plr)

def pkt(num, files):
    return plr.Packet(
        number=num, title="", heading=f"## Packet {num}", body="", sections={},
        allowed_files=files, input_files=[], verification="", progress_record="",
        dependencies=[],
    )

# 001 & 002 share docs/a.md; 003 is independent.
packets = [
    pkt("001", ["docs/a.md", "docs/graphrag/progress/001.done.md"]),
    pkt("002", ["docs/a.md", "docs/graphrag/progress/002.done.md"]),
    pkt("003", ["docs/c.md", "docs/graphrag/progress/003.done.md"]),
]
by_number = {p.number: p for p in packets}

# Determinism: the same wave selection every time, across 5 calls.
waves = [plr.select_wave(["001", "002", "003"], by_number, jobs=3) for _ in range(5)]
assert all(w == waves[0] for w in waves), waves
# 001 and 002 share docs/a.md -> never in the same wave.
assert not ({"001", "002"} <= set(waves[0])), waves[0]
# The independent packet rides along with the first co-writer.
assert waves[0] == ["001", "003"], waves[0]

# Conflicts are reported deterministically.
conflicts = plr.overlap_conflicts(packets)
assert conflicts == [("001", "002", "docs/a.md")], conflicts

# Progress records are per-packet and never count as a conflict.
shared_progress = [
    pkt("004", ["docs/x.md", "docs/graphrag/progress/shared.done.md"]),
    pkt("005", ["docs/y.md", "docs/graphrag/progress/shared.done.md"]),
]
assert plr.overlap_conflicts(shared_progress) == [], plr.overlap_conflicts(shared_progress)
PY
}

@test "parse fixes: PACKET_RE ignores prose headings; deps do not zero-pad non-numeric ids" {
  RUNNER="$RUNNER" python3 - <<'PY'
import importlib.util, os, sys
spec = importlib.util.spec_from_file_location("plr", os.environ["RUNNER"])
plr = importlib.util.module_from_spec(spec)
sys.modules["plr"] = plr
spec.loader.exec_module(plr)

import pathlib, tempfile

# A "## Packet Naming" prose heading must NOT be parsed as a packet; only the
# digit-bearing packet id is.
plan = """# Plan

## Packet Naming conventions

This section is prose about how packets are named, not a packet.

## Packet 001: Real

Allowed files:
- `docs/a.md`
- `docs/graphrag/progress/001.done.md`

Acceptance criteria:
- ok.

Verification:
```bash
true
```

Progress record:
`docs/graphrag/progress/001.done.md`
"""
with tempfile.TemporaryDirectory() as d:
    p = pathlib.Path(d) / "plan.md"
    p.write_text(plan)
    packets = plr.parse_plan(p)
    numbers = [pk.number for pk in packets]
    assert numbers == ["001"], numbers

# parse_dependencies must zero-pad pure-numeric ids but leave non-numeric ids
# (and digit substrings inside them) verbatim.
assert plr.parse_dependencies("1") == ["001"], plr.parse_dependencies("1")
assert plr.parse_dependencies("001, 2") == ["001", "002"], plr.parse_dependencies("001, 2")
assert plr.parse_dependencies("07a") == ["07a"], plr.parse_dependencies("07a")
assert plr.parse_dependencies("phase-b") == ["phase-b"], plr.parse_dependencies("phase-b")
PY
}
