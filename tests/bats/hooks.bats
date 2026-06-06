#!/usr/bin/env bats

load "helpers/setup.bash"

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  cddx_setup_binary "$REPO_ROOT"

  TEST_REPO="$(mktemp -d)"
  cd "$TEST_REPO" || return 1
  git init -q -b main
  git config user.email t@t
  git config user.name t
  echo x > README.md && git add README.md && git commit -q -m init
}

teardown() {
  cd /
  rm -rf "$TEST_REPO"
}

@test "session-start hook continues when broker unreachable" {
  out="$(printf '{"session_id":"s","cwd":"%s","hook_event_name":"SessionStart"}' "$TEST_REPO" | "$REPO_ROOT/scripts/hooks/session-start.sh")"
  [[ "$out" == *'"continue":true'* ]]
}

@test "stop hook continues when broker unreachable" {
  out="$(printf '{"session_id":"s","cwd":"%s","hook_event_name":"Stop"}' "$TEST_REPO" | "$REPO_ROOT/scripts/hooks/stop.sh")"
  [[ "$out" == *'"continue":true'* ]]
}

@test "session-end hook continues when broker unreachable" {
  out="$(printf '{"session_id":"s","cwd":"%s","hook_event_name":"SessionEnd"}' "$TEST_REPO" | "$REPO_ROOT/scripts/hooks/session-end.sh")"
  [[ "$out" == *'"continue":true'* ]]
}

@test "CODEX_DISPATCH_DISABLE_HOOKS short-circuits stop hook" {
  payload='{"session_id":"s","cwd":"'"$TEST_REPO"'","hook_event_name":"Stop"}'
  out="$(printf '%s' "$payload" | CODEX_DISPATCH_DISABLE_HOOKS=1 "$REPO_ROOT/scripts/hooks/stop.sh")"
  [[ "$out" == *'"continue":true'* ]]
}

@test "hooks emit valid JSON even with empty stdin" {
  out="$("$REPO_ROOT/scripts/hooks/session-start.sh" < /dev/null)"
  echo "$out" | jq -e . > /dev/null
}
