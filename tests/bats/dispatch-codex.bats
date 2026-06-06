#!/usr/bin/env bats

# Tests for scripts/dispatch-codex.sh — the shared dispatch core.
#
# Each test runs in a throwaway git repo with a fake `codex` binary on PATH.
# The fake is the fake-appserver binary (speaks codex app-server JSON-RPC),
# configured per-test via FAKE_APPSERVER_EDIT / FAKE_APPSERVER_SESSION /
# FAKE_APPSERVER_EXIT.

bats_require_minimum_version 1.5.0

load "helpers/setup.bash"

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  cddx_setup_binary "$REPO_ROOT"
  DISPATCH="$REPO_ROOT/scripts/dispatch-codex.sh"

  FAKE_BIN="$(mktemp -d)"
  cddx_build_fake_appserver "$FAKE_BIN" "$REPO_ROOT"
  ORIG_PATH="$PATH"
  export PATH="$FAKE_BIN:$PATH"

  # Always report a satisfying version to internal/codex/appserver's check.
  export FAKE_CODEX_VERSION="0.130.0"

  TEST_REPO="$(mktemp -d)"
  cd "$TEST_REPO" || return 1
  git init -q -b main
  git config user.email test@test
  git config user.name test
  echo ".codex-dispatch/" > .gitignore
  echo "initial" > README.md
  git add .gitignore README.md
  git commit -q -m "init"

  export CODEX_TASK="add hello.txt with greeting"
  export CODEX_ACCEPTANCE="hello.txt exists"
  unset CODEX_RESULT_DIR
  unset CODEX_FILES CODEX_CONSTRAINTS CODEX_CONVENTIONS_FILE
  unset CODEX_SESSION_ID CODEX_FEEDBACK

  # The CLAUDE_SESSION_ID env is what internal/codex.deriveSessionID reads;
  # set a stable value so tasks land under a known session in the broker.
  export CLAUDE_SESSION_ID="bats-dispatch-codex"
}

teardown() {
  cd /
  # Clean up the broker process this test spawned (if any). The launcher
  # auto-spawned a broker via os.Executable() when CODEX_DISPATCH_BIN ran the
  # `dispatch` subcommand. The broker writes its PID file inside the
  # throwaway TEST_REPO/.codex-dispatch/ — kill it cleanly so the next test
  # gets a fresh broker.
  if [ -f "$TEST_REPO/.codex-dispatch/broker.pid" ]; then
    kill "$(cat "$TEST_REPO/.codex-dispatch/broker.pid")" 2>/dev/null || true
  fi
  rm -rf "$TEST_REPO" "$FAKE_BIN" 2>/dev/null || true
  export PATH="$ORIG_PATH"
}

# --- validation -------------------------------------------------------------

@test "exits 2 when not in a git repo" {
  cd "$(mktemp -d)"
  run "$DISPATCH"
  [ "$status" -eq 2 ]
  [[ "$output" == *"git"* ]]
}

@test "exits 64 when CODEX_TASK is missing" {
  unset CODEX_TASK
  run "$DISPATCH"
  [ "$status" -eq 64 ]
  [[ "$output" == *"CODEX_TASK"* ]]
}

@test "exits 64 when CODEX_ACCEPTANCE is missing" {
  unset CODEX_ACCEPTANCE
  run "$DISPATCH"
  [ "$status" -eq 64 ]
  [[ "$output" == *"CODEX_ACCEPTANCE"* ]]
}

@test "exits 3 when codex binary missing from PATH" {
  PATH="/usr/bin:/bin" run "$DISPATCH"
  [ "$status" -eq 3 ]
  [[ "$output" == *"codex"* ]]
}

# --- happy path --------------------------------------------------------------

@test "happy path: result.json contains all spec fields" {
  export FAKE_APPSERVER_EDIT="hello.txt:Hello world"
  export FAKE_APPSERVER_SESSION="sess-123"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  local run_dir
  run_dir="$(echo "$output" | tail -n 1)"
  [ -d "$run_dir" ]
  [ -f "$run_dir/result.json" ]
  jq -e 'has("exit_code") and has("session_id") and has("files_changed") and has("lines_added") and has("lines_removed") and has("stdout_path") and has("diff_path") and has("fell_back_to_fresh")' "$run_dir/result.json" >/dev/null
}

@test "happy path: files_changed lists codex's edit and lines_added > 0" {
  export FAKE_APPSERVER_EDIT="hello.txt:Hello"
  export FAKE_APPSERVER_SESSION="sess-1"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  local run_dir
  run_dir="$(echo "$output" | tail -n 1)"
  [ "$(jq -r '.files_changed[0]' "$run_dir/result.json")" = "hello.txt" ]
  [ "$(jq -r '.lines_added' "$run_dir/result.json")" -ge 1 ]
}

@test "no-change result: exit_code marks missing meaningful edits" {
  export FAKE_APPSERVER_SESSION="sess-no-edit"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  local run_dir
  run_dir="$(echo "$output" | tail -n 1)"
  [ "$(jq -r '.exit_code' "$run_dir/result.json")" -eq 4 ]
  [ "$(jq -r '.error_message' "$run_dir/result.json")" = "codex completed without meaningful repository edits" ]
  [ "$(jq -r '.lines_added' "$run_dir/result.json")" -eq 0 ]
  [ "$(jq -r '.lines_removed' "$run_dir/result.json")" -eq 0 ]
  [ "$(jq -r '.files_changed | length' "$run_dir/result.json")" -eq 0 ]
}

# --- detached vs synchronous no-edit contract (P008 integration) -------------
#
# End-to-end check that the detached contract DIVERGES from the synchronous one
# on a no-edit turn, exactly as README documents:
#   - synchronous: a no-edit run computes a diff, finds nothing meaningful, and
#     records result.json.exit_code == 4 (the no-edits gate).
#   - detached: NO diff is computed, so the no-edits gate cannot fire — the run
#     completes done with --status.exit_code == 0 and writes NO result.json.
# This is the lifecycle/contract behaviour the broker's detached path (P008)
# guarantees; asserting both sides in one test pins the divergence.

# _codex_wait_terminal <task_id> — poll --detach --status to a terminal state,
# leaving the final --status JSON in $output.
_codex_wait_terminal() {
  local task_id="$1" i state
  for i in $(seq 1 100); do
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

@test "no-edit run: synchronous gates exit_code=4, detached completes exit 0 with no result.json" {
  export FAKE_APPSERVER_SESSION="sess-noedit-sync"
  # No FAKE_APPSERVER_EDIT — codex makes no repository changes.

  # Synchronous: the no-edits gate fires in result.json.
  export CODEX_RESULT_DIR="$TEST_REPO/run-sync-noedit"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(jq -r '.exit_code' "$TEST_REPO/run-sync-noedit/result.json")" -eq 4 ]

  # Detached: same no-edit turn, but no diff is computed so the gate cannot fire.
  unset CODEX_RESULT_DIR
  export FAKE_APPSERVER_SESSION="sess-noedit-detach"
  export CODEX_RESULT_DIR="$TEST_REPO/run-detach-noedit"
  task_id="$("$DISPATCH" --detach)"
  [[ "$task_id" =~ ^t_[a-f0-9]+$ ]]
  _codex_wait_terminal "$task_id"
  [ "$(echo "$output" | jq -r '.state')" = "done" ]
  [ "$(echo "$output" | jq -r '.exit_code')" -eq 0 ]
  # Detached writes the streaming log, never a result.json (reduced shape).
  [ -f "$TEST_REPO/run-detach-noedit/stdout.log" ]
  [ ! -f "$TEST_REPO/run-detach-noedit/result.json" ]
}

# --- result dir handling -----------------------------------------------------

@test "default result dir is created under .codex-dispatch/runs/" {
  export FAKE_APPSERVER_SESSION="sess-dd"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ -d ".codex-dispatch/runs" ]
  [ "$(find .codex-dispatch/runs -mindepth 1 -maxdepth 1 -type d | wc -l)" -gt 0 ]
}

@test "respects CODEX_RESULT_DIR override" {
  export CODEX_RESULT_DIR="$TEST_REPO/custom-run-dir"
  export FAKE_APPSERVER_SESSION="sess-override"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ -f "$TEST_REPO/custom-run-dir/result.json" ]
}

# --- artifacts ---------------------------------------------------------------

@test "stdout.log is written" {
  export CODEX_RESULT_DIR="$TEST_REPO/run-stdout"
  export FAKE_APPSERVER_SESSION="sess-out"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ -f "$TEST_REPO/run-stdout/stdout.log" ]
}

@test "prompt.txt is written and contains task and acceptance" {
  export CODEX_RESULT_DIR="$TEST_REPO/run-prompt"
  export FAKE_APPSERVER_SESSION="sess-prompt"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ -f "$TEST_REPO/run-prompt/prompt.txt" ]
  grep -qF "$CODEX_TASK" "$TEST_REPO/run-prompt/prompt.txt"
  grep -qF "$CODEX_ACCEPTANCE" "$TEST_REPO/run-prompt/prompt.txt"
}

@test "session_id from fake codex is parsed into result.json" {
  export CODEX_RESULT_DIR="$TEST_REPO/run-sid"
  export FAKE_APPSERVER_SESSION="my-custom-session-id"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(jq -r '.session_id' "$TEST_REPO/run-sid/result.json")" = "my-custom-session-id" ]
}

@test "baseline-head.txt records the pre-run HEAD" {
  export CODEX_RESULT_DIR="$TEST_REPO/run-baseline"
  export FAKE_APPSERVER_SESSION="sess-base"
  local before_head
  before_head="$(git rev-parse HEAD)"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ -f "$TEST_REPO/run-baseline/baseline-head.txt" ]
  [ "$(cat "$TEST_REPO/run-baseline/baseline-head.txt")" = "$before_head" ]
}

# --- WIP separation ----------------------------------------------------------

@test "pre-existing WIP is captured separately and not attributed to codex" {
  echo "WIP change" >> README.md
  export CODEX_RESULT_DIR="$TEST_REPO/run-wip"
  export FAKE_APPSERVER_EDIT="codex-only.txt:from codex"
  export FAKE_APPSERVER_SESSION="sess-wip"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ -s "$TEST_REPO/run-wip/baseline-pre.patch" ]
  local files_changed
  files_changed="$(jq -r '.files_changed | join(" ")' "$TEST_REPO/run-wip/result.json")"
  [[ "$files_changed" == *"codex-only.txt"* ]]
  [[ "$files_changed" != *"README.md"* ]]
}

# --- codex exit code ---------------------------------------------------------

@test "failed turn status is preserved in result.json without failing dispatch" {
  # FAKE_APPSERVER_EXIT controls Turn.Status. Real codex's app-server protocol
  # has a closed enum: completed | failed | cancelled. The broker maps these
  # to exit codes 0 / 2 / 64. dispatch itself always returns 0 when result.json
  # is written successfully — the codex outcome is data in the file.
  export CODEX_RESULT_DIR="$TEST_REPO/run-fail"
  export FAKE_APPSERVER_EXIT="failed"
  export FAKE_APPSERVER_SESSION="sess-fail"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(jq -r '.exit_code' "$TEST_REPO/run-fail/result.json")" -eq 2 ]
}

# --- prompt assembly (Task 3) ------------------------------------------------

@test "prompt includes conventions content when CODEX_CONVENTIONS_FILE is set" {
  echo "USE TABS NOT SPACES" > "$TEST_REPO/CONVENTIONS.md"
  export CODEX_CONVENTIONS_FILE="$TEST_REPO/CONVENTIONS.md"
  export CODEX_RESULT_DIR="$TEST_REPO/run-conv"
  export FAKE_APPSERVER_SESSION="sess-conv"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qF "USE TABS NOT SPACES" "$TEST_REPO/run-conv/prompt.txt"
  grep -qE '^CONVENTIONS' "$TEST_REPO/run-conv/prompt.txt"
}

@test "prompt auto-detects CLAUDE.md when CODEX_CONVENTIONS_FILE is unset" {
  echo "PROJECT RULE: NO MAGIC NUMBERS" > "$TEST_REPO/CLAUDE.md"
  git -C "$TEST_REPO" add CLAUDE.md
  git -C "$TEST_REPO" commit -q -m "add CLAUDE.md"
  export CODEX_RESULT_DIR="$TEST_REPO/run-auto"
  export FAKE_APPSERVER_SESSION="sess-auto"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qF "PROJECT RULE: NO MAGIC NUMBERS" "$TEST_REPO/run-auto/prompt.txt"
}

@test "prompt auto-detects AGENTS.md when CLAUDE.md is absent" {
  echo "AGENT RULE: PREFER FUNCTIONS" > "$TEST_REPO/AGENTS.md"
  git -C "$TEST_REPO" add AGENTS.md
  git -C "$TEST_REPO" commit -q -m "add AGENTS.md"
  export CODEX_RESULT_DIR="$TEST_REPO/run-agents"
  export FAKE_APPSERVER_SESSION="sess-agents"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qF "AGENT RULE: PREFER FUNCTIONS" "$TEST_REPO/run-agents/prompt.txt"
}

@test "prompt warns when CODEX_CONVENTIONS_FILE points to a missing path" {
  export CODEX_CONVENTIONS_FILE="$TEST_REPO/does-not-exist.md"
  export CODEX_RESULT_DIR="$TEST_REPO/run-missing-conv"
  export FAKE_APPSERVER_SESSION="sess-mc"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qF "not found" "$TEST_REPO/run-missing-conv/prompt.txt"
  [[ "$output" == *"warning"* ]]
}

@test "prompt includes file snippets from CODEX_FILES" {
  echo "FOO_CONTENT" > "$TEST_REPO/foo.txt"
  echo "BAR_CONTENT" > "$TEST_REPO/bar.txt"
  export CODEX_FILES="foo.txt,bar.txt"
  export CODEX_RESULT_DIR="$TEST_REPO/run-files"
  export FAKE_APPSERVER_SESSION="sess-files"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qF "FOO_CONTENT" "$TEST_REPO/run-files/prompt.txt"
  grep -qF "BAR_CONTENT" "$TEST_REPO/run-files/prompt.txt"
  grep -qF "=== foo.txt ===" "$TEST_REPO/run-files/prompt.txt"
  grep -qF "=== bar.txt ===" "$TEST_REPO/run-files/prompt.txt"
}

@test "prompt warns about missing files in CODEX_FILES but still dispatches" {
  echo "REAL_CONTENT" > "$TEST_REPO/real.txt"
  export CODEX_FILES="real.txt,ghost.txt"
  export CODEX_RESULT_DIR="$TEST_REPO/run-miss-files"
  export FAKE_APPSERVER_SESSION="sess-mf"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qF "REAL_CONTENT" "$TEST_REPO/run-miss-files/prompt.txt"
  grep -qF "[file not found: ghost.txt]" "$TEST_REPO/run-miss-files/prompt.txt"
  [[ "$output" == *"ghost.txt"* ]]
}

@test "prompt includes CODEX_CONSTRAINTS when set" {
  export CODEX_CONSTRAINTS="DO NOT TOUCH tests/"
  export CODEX_RESULT_DIR="$TEST_REPO/run-cons"
  export FAKE_APPSERVER_SESSION="sess-cons"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qF "DO NOT TOUCH tests/" "$TEST_REPO/run-cons/prompt.txt"
  grep -qE '^CONSTRAINTS' "$TEST_REPO/run-cons/prompt.txt"
}

@test "prompt includes CODEX_FEEDBACK when set" {
  export CODEX_FEEDBACK="last iteration: test foo failed"
  export CODEX_RESULT_DIR="$TEST_REPO/run-fb"
  export FAKE_APPSERVER_SESSION="sess-fb"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qF "last iteration: test foo failed" "$TEST_REPO/run-fb/prompt.txt"
  grep -qE '^PRIOR FEEDBACK' "$TEST_REPO/run-fb/prompt.txt"
}

# --- codex resume + fallback (Task 3) ---------------------------------------
#
# The broker now drives `codex app-server` JSON-RPC v2: initialize →
# thread/start or thread/resume → turn/start. Assertions use
# FAKE_APPSERVER_RPC_LOG, where each line is "<method>\t<params-json>".

@test "resume mode: fake codex receives thread/resume with the prev session id" {
  local rpc_log="$FAKE_BIN/rpc-resume.log"
  export CODEX_SESSION_ID="prev-sess-abc"
  export CODEX_RESULT_DIR="$TEST_REPO/run-resume"
  export FAKE_APPSERVER_RPC_LOG="$rpc_log"
  export FAKE_APPSERVER_SESSION="sess-resume-1"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qE $'^thread/resume\t.*"threadId":"prev-sess-abc"' "$rpc_log"
  [ "$(jq -r '.fell_back_to_fresh' "$TEST_REPO/run-resume/result.json")" = "false" ]
}

@test "fresh mode: fake codex receives thread/start (no thread/resume) when CODEX_SESSION_ID is empty" {
  local rpc_log="$FAKE_BIN/rpc-fresh.log"
  export CODEX_RESULT_DIR="$TEST_REPO/run-fresh"
  export FAKE_APPSERVER_RPC_LOG="$rpc_log"
  export FAKE_APPSERVER_SESSION="sess-fresh-1"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  run ! grep -qE $'^thread/resume\t' "$rpc_log"
  grep -qE $'^thread/start\t' "$rpc_log"
}

@test "CODEX_SANDBOX flows into thread/start params (default danger-full-access)" {
  local rpc_log="$FAKE_BIN/rpc-sandbox.log"
  export CODEX_RESULT_DIR="$TEST_REPO/run-sandbox-default"
  export FAKE_APPSERVER_RPC_LOG="$rpc_log"
  export FAKE_APPSERVER_SESSION="sess-sandbox-default"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qE $'^thread/start\t.*"sandbox":"danger-full-access"' "$rpc_log"
}

@test "CODEX_SANDBOX=workspace-write overrides the default" {
  local rpc_log="$FAKE_BIN/rpc-sandbox-workspace.log"
  export CODEX_RESULT_DIR="$TEST_REPO/run-sandbox-workspace"
  export FAKE_APPSERVER_RPC_LOG="$rpc_log"
  export FAKE_APPSERVER_SESSION="sess-sandbox-workspace"
  export CODEX_SANDBOX="workspace-write"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  grep -qE $'^thread/start\t.*"sandbox":"workspace-write"' "$rpc_log"
  run ! grep -qE $'^thread/start\t.*"sandbox":"danger-full-access"' "$rpc_log"
}

@test "CODEX_SANDBOX with invalid value exits 64" {
  export CODEX_SANDBOX="totally-bogus"
  run "$DISPATCH"
  [ "$status" -eq 64 ]
  [[ "$output" == *"CODEX_SANDBOX"* ]]
}

@test "stale resume falls back to fresh and sets fell_back_to_fresh=true" {
  local rpc_log="$FAKE_BIN/rpc-stale.log"
  export CODEX_SESSION_ID="00000000-0000-0000-0000-000000000000"
  export CODEX_RESULT_DIR="$TEST_REPO/run-stale"
  export FAKE_APPSERVER_STALE_RESUME="$CODEX_SESSION_ID"
  export FAKE_APPSERVER_RPC_LOG="$rpc_log"
  export FAKE_APPSERVER_EDIT="from-fresh.txt:from fresh retry"
  export FAKE_APPSERVER_SESSION="fresh-thread-id"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(jq -r '.fell_back_to_fresh' "$TEST_REPO/run-stale/result.json")" = "true" ]
  # Broker tried resume first, then fell back to thread/start after -32004.
  grep -qE $'^thread/resume\t' "$rpc_log"
  grep -qE $'^thread/start\t' "$rpc_log"
  # Fresh retry's edits made it into the diff
  [ "$(jq -r '.files_changed[0]' "$TEST_REPO/run-stale/result.json")" = "from-fresh.txt" ]
  # Session id is from the successful fresh attempt
  [ "$(jq -r '.session_id' "$TEST_REPO/run-stale/result.json")" = "fresh-thread-id" ]
  # Stdout log preserves the fallback marker
  grep -qF "fell back to fresh dispatch" "$TEST_REPO/run-stale/stdout.log"
}
