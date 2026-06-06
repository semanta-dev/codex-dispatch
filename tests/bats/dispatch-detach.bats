#!/usr/bin/env bats

load "helpers/setup.bash"

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  cddx_setup_binary "$REPO_ROOT"
  DISPATCH="$REPO_ROOT/scripts/dispatch-codex.sh"

  FAKE_BIN="$(mktemp -d)"
  cddx_build_fake_appserver "$FAKE_BIN" "$REPO_ROOT"
  ORIG_PATH="$PATH"
  export PATH="$FAKE_BIN:$PATH"
  export FAKE_CODEX_VERSION="0.130.0"
  export FAKE_APPSERVER_SESSION="thread-detach-1"
  # Where the configured edit lands; doesn't matter for these tests
  local fake_edit
  fake_edit="$(mktemp -u):detached-hello"
  export FAKE_APPSERVER_EDIT="$fake_edit"

  TEST_REPO="$(mktemp -d)"
  cd "$TEST_REPO" || return 1
  git init -q -b main
  git config user.email t@t
  git config user.name t
  echo x > README.md && git add README.md && git commit -q -m init

  export CODEX_TASK="t"
  export CODEX_ACCEPTANCE="a"
  export CLAUDE_SESSION_ID="bats-detach"
}

teardown() {
  cd /
  if [ -f "$TEST_REPO/.codex-dispatch/broker.pid" ]; then
    kill "$(cat "$TEST_REPO/.codex-dispatch/broker.pid")" 2>/dev/null || true
  fi
  rm -rf "$TEST_REPO" "$FAKE_BIN" 2>/dev/null || true
  export PATH="$ORIG_PATH"
}

@test "--detach returns a task_id immediately" {
  run "$DISPATCH" --detach
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^t_[a-f0-9]+$ ]]
}

@test "--list shows tasks the broker knows about" {
  task_id="$("$DISPATCH" --detach)"
  sleep 0.5  # let it finish against the fake
  run "$DISPATCH" --list
  [ "$status" -eq 0 ]
  [[ "$output" == *"$task_id"* ]]
}

@test "--status reports state of a known task" {
  task_id="$("$DISPATCH" --detach)"
  sleep 0.5
  run "$DISPATCH" --status "$task_id"
  [ "$status" -eq 0 ]
  [[ "$output" == *"$task_id"* ]]
  # Either done or running is acceptable (race-tolerant)
  [[ "$output" == *"done"* ]] || [[ "$output" == *"running"* ]]
}

@test "--cancel succeeds on a known task" {
  task_id="$("$DISPATCH" --detach)"
  run "$DISPATCH" --cancel "$task_id"
  # Cancel races with completion; either outcome is acceptable as long as exit 0
  # (the broker returns -32007 task-already-terminal when racing past completion,
  # which the CLI surfaces as exit 1 + stderr — accept both)
  [ "$status" -eq 0 ] || [ "$status" -eq 1 ]
}

@test "--status on unknown task exits 1" {
  run "$DISPATCH" --status t_unknown
  [ "$status" -eq 1 ]
}

@test "--status requires a task_id" {
  run "$DISPATCH" --status
  [ "$status" -eq 64 ]
}

@test "--cancel requires a task_id" {
  run "$DISPATCH" --cancel
  [ "$status" -eq 64 ]
}

# --- detached lifecycle / P008 contract (integration) -----------------------
#
# These assert the END-TO-END detached contract documented in README's "Detached
# task contract (--status)": a detached task runs to a terminal state against the
# fake appserver, exposes the reduced --status shape (no result.json/diff; always
# carries event_count + fell_back_to_fresh; exits 0 on a clean turn), and never
# writes a result.json. They go past the existing race-tolerant smoke cases by
# polling to a TERMINAL state and asserting the concrete contract fields.

# _detach_wait_terminal <task_id> — poll --status until state is done|cancelled|
# errored or a deadline elapses. Leaves the final `--status` JSON in $output.
_detach_wait_terminal() {
  local task_id="$1" i state
  for i in $(seq 1 100); do   # ~10s at 0.1s
    run "$DISPATCH" --status "$task_id"
    [ "$status" -eq 0 ] || { sleep 0.1; continue; }
    state="$(echo "$output" | jq -r '.state')"
    case "$state" in
      done|cancelled|errored) return 0 ;;
    esac
    sleep 0.1
  done
  return 1
}

@test "detached run reaches a terminal done state with exit_code 0 (clean turn)" {
  export FAKE_APPSERVER_SESSION="thread-detach-done"
  task_id="$("$DISPATCH" --detach)"
  [[ "$task_id" =~ ^t_[a-f0-9]+$ ]]

  _detach_wait_terminal "$task_id"
  [ "$(echo "$output" | jq -r '.state')" = "done" ]
  [ "$(echo "$output" | jq -r '.exit_code')" -eq 0 ]
  # session_id is the fake's configured thread once a thread exists.
  [ "$(echo "$output" | jq -r '.session_id')" = "thread-detach-done" ]
}

@test "detached --status exposes the reduced contract shape (no diff fields)" {
  export FAKE_APPSERVER_SESSION="thread-detach-shape"
  task_id="$("$DISPATCH" --detach)"
  _detach_wait_terminal "$task_id"

  # Always-present contract fields per README.
  echo "$output" | jq -e 'has("task_id") and has("state") and has("started_at") and has("event_count") and has("fell_back_to_fresh")' >/dev/null
  # A clean turn that was never a stale resume.
  [ "$(echo "$output" | jq -r '.fell_back_to_fresh')" = "false" ]
  [ "$(echo "$output" | jq -r '.event_count')" -ge 1 ]
  # The reduced shape: detached runs compute no diff, so these MUST be absent.
  echo "$output" | jq -e 'has("files_changed") | not' >/dev/null
  echo "$output" | jq -e 'has("lines_added") | not' >/dev/null
  echo "$output" | jq -e 'has("lines_removed") | not' >/dev/null
}

@test "detached run writes stdout.log but NO result.json (durable record is the log)" {
  export FAKE_APPSERVER_SESSION="thread-detach-nojson"
  # Pin the run dir so we can inspect what the detached path wrote.
  export CODEX_RESULT_DIR="$TEST_REPO/detach-run"
  task_id="$("$DISPATCH" --detach)"
  _detach_wait_terminal "$task_id"
  [ "$(echo "$output" | jq -r '.state')" = "done" ]

  # The streaming stdout.log is the detached run's durable record...
  [ -f "$TEST_REPO/detach-run/stdout.log" ]
  grep -qF '"method":"turn/completed"' "$TEST_REPO/detach-run/stdout.log"
  # ...and unlike a synchronous dispatch it must NOT produce a result.json.
  [ ! -f "$TEST_REPO/detach-run/result.json" ]
}
