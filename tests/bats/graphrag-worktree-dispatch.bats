#!/usr/bin/env bats

bats_require_minimum_version 1.5.0

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  SCRIPT="$REPO_ROOT/scripts/graphrag-worktree-dispatch.sh"
  TMP_REPO="$(mktemp -d)"
  export GIT_CONFIG_GLOBAL=/dev/null
  cd "$TMP_REPO" || return 1
  git init -q -b main
  git config user.email t@t
  git config user.name t
  printf 'root\n' > README.md
  git add README.md
  git commit -q -m init

  FAKE_DISPATCH="$TMP_REPO/fake-dispatch.sh"
  cat > "$FAKE_DISPATCH" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$(dirname "${FAKE_WRITE_PATH}")"
printf '%s\n' "${FAKE_WRITE_CONTENT:-written}" > "${FAKE_WRITE_PATH}"
if [ -n "${FAKE_EXTRA_PATH:-}" ]; then
  mkdir -p "$(dirname "${FAKE_EXTRA_PATH}")"
  printf '%s\n' "${FAKE_EXTRA_CONTENT:-extra}" > "${FAKE_EXTRA_PATH}"
fi
run_dir="${PWD}/.codex-dispatch/runs/fake"
mkdir -p "$run_dir"
printf 'fake stdout\n' > "$run_dir/stdout.log"
printf '{"exit_code":%s,"session_id":"fake","files_changed":[],"lines_added":1,"lines_removed":0,"stdout_path":"%s/stdout.log","diff_path":"%s/diff.patch","fell_back_to_fresh":false}\n' "${FAKE_RESULT_EXIT_CODE:-0}" "$run_dir" "$run_dir" > "$run_dir/result.json"
printf '%s\n' "$run_dir"
EOF
  chmod +x "$FAKE_DISPATCH"
}

teardown() {
  cd /
  rm -rf "$TMP_REPO"
}

@test "fans in allowed worktree changes to parent checkout" {
  export GRAPHRAG_DISPATCH_COMMAND="$FAKE_DISPATCH"
  export GRAPHRAG_ALLOWED_FILES=$'docs/packet.md'
  export FAKE_WRITE_PATH='docs/packet.md'
  export FAKE_WRITE_CONTENT='from isolated worktree'

  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ -f docs/packet.md ]
  [ "$(cat docs/packet.md)" = "from isolated worktree" ]
  [[ "$output" == *"changed_files=docs/packet.md"* ]]
}

@test "does not fan in out-of-scope worktree changes" {
  export GRAPHRAG_DISPATCH_COMMAND="$FAKE_DISPATCH"
  export GRAPHRAG_ALLOWED_FILES=$'docs/packet.md'
  export FAKE_WRITE_PATH='docs/packet.md'
  export FAKE_EXTRA_PATH='docs/outside.md'

  run "$SCRIPT"
  [ "$status" -eq 1 ]
  [ ! -e docs/packet.md ]
  [ ! -e docs/outside.md ]
  [[ "$output" == *"scope audit failed"* || "$output" == *"Out-of-scope changed files"* ]]
}

@test "does not fan in when dispatch result exit_code is nonzero" {
  export GRAPHRAG_DISPATCH_COMMAND="$FAKE_DISPATCH"
  export GRAPHRAG_ALLOWED_FILES=$'docs/packet.md'
  export FAKE_WRITE_PATH='docs/packet.md'
  export FAKE_RESULT_EXIT_CODE=64

  run "$SCRIPT"
  [ "$status" -eq 1 ]
  [ ! -e docs/packet.md ]
  [[ "$output" == *"dispatch result failed with exit_code=64"* ]]
}

@test "retries failed dispatch result in a fresh worktree before fan-in" {
  retry_dispatch="$TMP_REPO/fake-retry.sh"
  retry_count="$TMP_REPO/retry-count"
  cat > "$retry_dispatch" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
count=0
if [ -f "$FAKE_RETRY_COUNT" ]; then
  count="$(cat "$FAKE_RETRY_COUNT")"
fi
count=$((count + 1))
printf '%s\n' "$count" > "$FAKE_RETRY_COUNT"
mkdir -p docs .codex-dispatch/runs/fake
printf 'attempt %s\n' "$count" > docs/packet.md
if [ "$count" -eq 1 ]; then
  exit_code=64
else
  exit_code=0
fi
printf '{"exit_code":%s,"session_id":"fake","files_changed":["docs/packet.md"],"lines_added":1,"lines_removed":0,"stdout_path":"%s/.codex-dispatch/runs/fake/stdout.log","diff_path":"%s/.codex-dispatch/runs/fake/diff.patch","fell_back_to_fresh":false}\n' "$exit_code" "$PWD" "$PWD" > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$retry_dispatch"

  export GRAPHRAG_DISPATCH_COMMAND="$retry_dispatch"
  export GRAPHRAG_ALLOWED_FILES=$'docs/packet.md'
  export FAKE_RETRY_COUNT="$retry_count"
  export GRAPHRAG_DISPATCH_ATTEMPTS=2

  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(cat docs/packet.md)" = "attempt 2" ]
  [ "$(cat "$retry_count")" = "2" ]
}

@test "does not overwrite parent changes made while isolated packet runs" {
  blocker="$TMP_REPO/blocker"
  conflict_dispatch="$TMP_REPO/fake-conflict.sh"
  cat > "$conflict_dispatch" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [ -e "${FAKE_BLOCKER}" ]; do sleep 0.05; done
mkdir -p docs .codex-dispatch/runs/fake
printf 'from worktree\n' > docs/packet.md
printf '{"exit_code":0,"session_id":"fake","files_changed":["docs/packet.md"],"lines_added":1,"lines_removed":0,"stdout_path":"%s/.codex-dispatch/runs/fake/stdout.log","diff_path":"%s/.codex-dispatch/runs/fake/diff.patch","fell_back_to_fresh":false}\n' "$PWD" "$PWD" > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$conflict_dispatch"
  mkdir -p docs
  printf 'original\n' > docs/packet.md
  git add docs/packet.md
  git commit -q -m packet
  touch "$blocker"

  (
    GRAPHRAG_DISPATCH_COMMAND="$conflict_dispatch" \
    GRAPHRAG_ALLOWED_FILES=$'docs/packet.md' \
    FAKE_BLOCKER="$blocker" \
    "$SCRIPT" > conflict.out 2> conflict.err
  ) &
  pid=$!
  for _ in $(seq 1 100); do
    if [ -n "$(find .codex-dispatch/graphrag-worktrees -mindepth 1 -maxdepth 1 -type d -print -quit 2>/dev/null)" ]; then
      break
    fi
    sleep 0.05
  done
  printf 'parent changed\n' > docs/packet.md
  rm -f "$blocker"
  wait "$pid" || rc=$?
  rc="${rc:-0}"

  [ "$rc" -eq 1 ]
  [ "$(cat docs/packet.md)" = "parent changed" ]
  [[ "$(cat conflict.err)" == *"fan-in conflict for docs/packet.md"* ]]
}

@test "parallel isolated packets fan in disjoint files cleanly" {
  first="$TMP_REPO/fake-first.sh"
  second="$TMP_REPO/fake-second.sh"
  cat > "$first" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p docs .codex-dispatch/runs/fake
printf 'one\n' > docs/one.md
printf '{"exit_code":0}\n' > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  cat > "$second" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p docs .codex-dispatch/runs/fake
printf 'two\n' > docs/two.md
printf '{"exit_code":0}\n' > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$first" "$second"

  (
    GRAPHRAG_DISPATCH_COMMAND="$first" \
    GRAPHRAG_ALLOWED_FILES=$'docs/one.md' \
    "$SCRIPT" > first.out 2> first.err
  ) &
  pid1=$!
  (
    GRAPHRAG_DISPATCH_COMMAND="$second" \
    GRAPHRAG_ALLOWED_FILES=$'docs/two.md' \
    "$SCRIPT" > second.out 2> second.err
  ) &
  pid2=$!
  wait "$pid1"
  wait "$pid2"

  [ "$(cat docs/one.md)" = "one" ]
  [ "$(cat docs/two.md)" = "two" ]
  [[ "$(cat first.out)" == *"changed_files=docs/one.md"* ]]
  [[ "$(cat second.out)" == *"changed_files=docs/two.md"* ]]
}

@test "seeds CODEX_FILES inputs without treating unchanged inputs as edits" {
  mkdir -p docs/parts
  printf 'source material\n' > docs/parts/input.md
  reader="$TMP_REPO/fake-reader.sh"
  cat > "$reader" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
test -f docs/parts/input.md
mkdir -p docs .codex-dispatch/runs/fake
printf 'final from %s\n' "$(cat docs/parts/input.md)" > docs/final.md
printf '{"exit_code":0}\n' > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$reader"

  export GRAPHRAG_DISPATCH_COMMAND="$reader"
  export GRAPHRAG_ALLOWED_FILES=$'docs/final.md'
  export CODEX_FILES='docs/parts/input.md'

  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(cat docs/final.md)" = "final from source material" ]
  [ "$(cat docs/parts/input.md)" = "source material" ]
  [[ "$output" == *"changed_files=docs/final.md"* ]]
}

@test "does not remove tracked CODEX_FILES read inputs" {
  mkdir -p tests docs
  printf 'tracked test\n' > tests/quality.py
  git add tests/quality.py
  git commit -q -m tests
  reader="$TMP_REPO/fake-tracked-reader.sh"
  cat > "$reader" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
test -f tests/quality.py
mkdir -p docs .codex-dispatch/runs/fake
printf 'used %s\n' "$(cat tests/quality.py)" > docs/out.md
printf '{"exit_code":0}\n' > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$reader"

  export GRAPHRAG_DISPATCH_COMMAND="$reader"
  export GRAPHRAG_ALLOWED_FILES=$'docs/out.md'
  export CODEX_FILES='tests/quality.py'

  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(cat tests/quality.py)" = "tracked test" ]
  [ "$(cat docs/out.md)" = "used tracked test" ]
  [[ "$output" == *"changed_files=docs/out.md"* ]]
}

@test "persists selected dispatch result and stdout outside temporary worktree" {
  export GRAPHRAG_DISPATCH_COMMAND="$FAKE_DISPATCH"
  export GRAPHRAG_ALLOWED_FILES=$'docs/packet.md'
  export FAKE_WRITE_PATH='docs/packet.md'

  run "$SCRIPT"
  [ "$status" -eq 0 ]
  run_dir="${lines[-1]}"
  [ -f "$run_dir/dispatch-result.json" ]
  [ -f "$run_dir/dispatch-stdout.log" ]
  [ "$(jq -r '.exit_code' "$run_dir/dispatch-result.json")" -eq 0 ]
  [ "$(cat "$run_dir/dispatch-selected-attempt.txt")" = "1" ]
  [[ "$output" == *"dispatch_result_json=$run_dir/dispatch-result.json"* ]]
  [[ "$output" == *"dispatch_stdout_log=$run_dir/dispatch-stdout.log"* ]]
}

@test "fans in a rename of allowed a.go to b.go without leaving the source behind" {
  printf 'package main\n' > a.go
  git add a.go
  git commit -q -m a

  rename_dispatch="$TMP_REPO/fake-rename.sh"
  cat > "$rename_dispatch" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
git mv a.go b.go
mkdir -p .codex-dispatch/runs/fake
printf '{"exit_code":0}\n' > .codex-dispatch/runs/fake/result.json
printf '%s\n' "$PWD/.codex-dispatch/runs/fake"
EOF
  chmod +x "$rename_dispatch"

  export GRAPHRAG_DISPATCH_COMMAND="$rename_dispatch"
  export GRAPHRAG_ALLOWED_FILES=$'a.go\nb.go'

  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ -f b.go ]
  [ ! -e a.go ]
  [ "$(cat b.go)" = "package main" ]
}

@test "refuses fan-in when parent allowed path has uncommitted WIP" {
  mkdir -p docs
  printf 'committed\n' > docs/packet.md
  git add docs/packet.md
  git commit -q -m packet
  # Uncommitted WIP on the allowed path that the isolated run must not clobber.
  printf 'operator WIP\n' > docs/packet.md

  export GRAPHRAG_DISPATCH_COMMAND="$FAKE_DISPATCH"
  export GRAPHRAG_ALLOWED_FILES=$'docs/packet.md'
  export FAKE_WRITE_PATH='docs/packet.md'
  export FAKE_WRITE_CONTENT='from isolated worktree'

  run "$SCRIPT"
  [ "$status" -eq 1 ]
  [ "$(cat docs/packet.md)" = "operator WIP" ]
  [[ "$output" == *"docs/packet.md has uncommitted changes"* ]]
}

@test "reclaims a stale fan-in lock owned by a dead PID instead of blocking" {
  lockdir="$TMP_REPO/.codex-dispatch/graphrag-fanin.lock"
  mkdir -p "$lockdir"
  # An impossible PID stands in for a crashed peer that never released the lock.
  printf '%s\n%s\n' '2147483646' 'dead-run' > "$lockdir/owner"
  # Age the lock past the staleness grace period.
  touch -d '1 hour ago' "$lockdir" 2>/dev/null || touch -t 200001010000 "$lockdir"

  export GRAPHRAG_DISPATCH_COMMAND="$FAKE_DISPATCH"
  export GRAPHRAG_ALLOWED_FILES=$'docs/packet.md'
  export FAKE_WRITE_PATH='docs/packet.md'
  export FAKE_WRITE_CONTENT='reclaimed'

  start="$(date +%s)"
  run "$SCRIPT"
  end="$(date +%s)"
  [ "$status" -eq 0 ]
  [ "$(cat docs/packet.md)" = "reclaimed" ]
  # Reclaim must happen well under the 60s blocking window.
  [ "$(( end - start ))" -lt 30 ]
}
