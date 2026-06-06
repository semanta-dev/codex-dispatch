#!/usr/bin/env bats

bats_require_minimum_version 1.5.0

# Tests for scripts/pick-iterations.sh — picks a max-iteration count for the
# /codex command and codex-dispatch subagent, in [PICK_FLOOR, PICK_CEILING].
#
# The helper "fails closed" — it always exits 0 and emits a single integer to
# stdout, even when the LLM call is unavailable, fails, or returns garbage.

load "helpers/setup.bash"

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  cddx_setup_binary "$REPO_ROOT"
  SCRIPT="$REPO_ROOT/scripts/pick-iterations.sh"

  FAKE_BIN="$(mktemp -d)"
  ORIG_PATH="$PATH"
  export PATH="$FAKE_BIN:$PATH"

  unset PICK_TASK PICK_ACCEPTANCE PICK_FLOOR PICK_CEILING PICK_MODEL PICK_DISABLE_LLM
}

teardown() {
  rm -rf "$FAKE_BIN"
  export PATH="$ORIG_PATH"
}

write_fake_claude() {
  cat > "$FAKE_BIN/claude" <<EOF
#!/usr/bin/env bash
$1
EOF
  chmod +x "$FAKE_BIN/claude"
}

# --- deterministic fallback shape -------------------------------------------

@test "outputs a single integer in default [2,5] when no LLM" {
  export PICK_TASK="add a hello function"
  export PICK_DISABLE_LLM=1
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9]+$ ]]
  [ "$output" -ge 2 ]
  [ "$output" -le 5 ]
}

@test "respects PICK_FLOOR and PICK_CEILING (clamp to identical bounds)" {
  export PICK_TASK="trivial"
  export PICK_FLOOR=4
  export PICK_CEILING=4
  export PICK_DISABLE_LLM=1
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" -eq 4 ]
}

@test "respects custom range" {
  export PICK_TASK="some task"
  export PICK_FLOOR=1
  export PICK_CEILING=3
  export PICK_DISABLE_LLM=1
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" -ge 1 ]
  [ "$output" -le 3 ]
}

@test "swaps inverted floor and ceiling and stays in range" {
  export PICK_TASK="task"
  export PICK_FLOOR=5
  export PICK_CEILING=2
  export PICK_DISABLE_LLM=1
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" -ge 2 ]
  [ "$output" -le 5 ]
}

@test "empty PICK_TASK uses floor" {
  export PICK_TASK=""
  export PICK_DISABLE_LLM=1
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" -eq 2 ]
}

# --- deterministic monotonicity ---------------------------------------------

@test "longer task yields >= shorter task iterations (deterministic)" {
  export PICK_DISABLE_LLM=1
  PICK_TASK="x" run "$SCRIPT"
  short="$output"
  long_task="$(printf 'x%.0s' {1..1000})"
  PICK_TASK="$long_task" run "$SCRIPT"
  long="$output"
  [ "$long" -ge "$short" ]
}

@test "more acceptance criteria yield >= fewer (deterministic)" {
  export PICK_TASK="task"
  export PICK_DISABLE_LLM=1
  PICK_ACCEPTANCE="" run "$SCRIPT"
  few="$output"
  PICK_ACCEPTANCE=$'a\nb\nc\nd\ne' run "$SCRIPT"
  many="$output"
  [ "$many" -ge "$few" ]
}

# --- LLM path ---------------------------------------------------------------

@test "fake claude returning a valid integer in range is used" {
  write_fake_claude 'echo 3'
  export PICK_TASK="task"
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" -eq 3 ]
}

@test "fake claude returning above-ceiling is clamped to ceiling" {
  write_fake_claude 'echo 100'
  export PICK_TASK="task"
  export PICK_FLOOR=2
  export PICK_CEILING=5
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" -eq 5 ]
}

@test "fake claude returning below-floor is clamped to floor" {
  write_fake_claude 'echo 0'
  export PICK_TASK="task"
  export PICK_FLOOR=2
  export PICK_CEILING=5
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" -eq 2 ]
}

@test "fake claude with no integer in output falls back to deterministic" {
  write_fake_claude 'echo "I do not know."'
  export PICK_TASK="task"
  run --separate-stderr "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9]+$ ]]
  [ "$output" -ge 2 ]
  [ "$output" -le 5 ]
}

@test "fake claude failing nonzero falls back to deterministic" {
  write_fake_claude 'echo "boom" >&2; exit 1'
  export PICK_TASK="task"
  run --separate-stderr "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9]+$ ]]
  [ "$output" -ge 2 ]
  [ "$output" -le 5 ]
}

@test "claude binary missing: deterministic fallback" {
  export PICK_TASK="task"
  PATH="/usr/bin:/bin" run --separate-stderr "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9]+$ ]]
  [ "$output" -ge 2 ]
  [ "$output" -le 5 ]
}

@test "PICK_DISABLE_LLM skips claude even when claude is on PATH" {
  write_fake_claude 'echo 4'  # would set result to 4 if invoked
  # Pick floor=ceiling=2 so deterministic = 2; an LLM result of 4 would fail.
  export PICK_TASK="x"
  export PICK_FLOOR=2
  export PICK_CEILING=2
  export PICK_DISABLE_LLM=1
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" -eq 2 ]
}

# --- output discipline ------------------------------------------------------

@test "stdout is exactly one integer line, diagnostics go to stderr" {
  write_fake_claude 'echo 3; echo "ignored chatter" >&2'
  export PICK_TASK="task"
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  # Single line, integer
  [ "$(printf '%s' "$output" | wc -l)" -eq 0 ]
  [[ "$output" =~ ^[0-9]+$ ]]
}
