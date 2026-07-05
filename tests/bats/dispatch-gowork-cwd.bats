#!/usr/bin/env bats

# Regression harness for the go.work monorepo cwd bug.
#
# Layout: one git repo at the parent with go.work and modules ./shared ./models
# ./server ./ui beneath it (scripts/make-gowork-fixture.sh). A dispatch invoked
# from inside a module must run codex with THAT module as its cwd — not collapse
# every dispatch to the repo root (the bug). The fake app-server records the
# thread/start cwd it received via FAKE_APPSERVER_RECORD_CWD; we assert it equals
# the module we dispatched from.
#
# This drives the real launcher (scripts/dispatch-codex.sh -> Go binary -> broker
# -> fake app-server), complementing the in-process Go test
# (internal/dispatch.TestRunThreadsSubdirCwdToCodex).

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
  export FAKE_CODEX_VERSION="0.130.0"

  # Scaffold the go.work monorepo fixture (its own throwaway git repo).
  FIXTURE_PARENT="$(mktemp -d)"
  FIXTURE="$FIXTURE_PARENT/gowork"
  "$REPO_ROOT/scripts/make-gowork-fixture.sh" "$FIXTURE" >/dev/null

  # The fake records each thread/start cwd here; the broker/app-server inherit
  # this env, and each dispatch overwrites the file (tests run sequentially).
  RECORD_CWD="$(mktemp)"
  export FAKE_APPSERVER_RECORD_CWD="$RECORD_CWD"

  export CODEX_TASK="noop task"
  export CODEX_ACCEPTANCE="noop acceptance"
  export CLAUDE_SESSION_ID="bats-gowork-cwd"
  unset CODEX_RESULT_DIR CODEX_FILES CODEX_CONSTRAINTS CODEX_CONVENTIONS_FILE \
        CODEX_SESSION_ID CODEX_FEEDBACK CODEX_WORKDIR
}

teardown() {
  cd /
  if [ -f "$FIXTURE/.codex-dispatch/broker.pid" ]; then
    kill "$(cat "$FIXTURE/.codex-dispatch/broker.pid")" 2>/dev/null || true
  fi
  rm -rf "$FIXTURE_PARENT" "$FAKE_BIN" "$RECORD_CWD" 2>/dev/null || true
  export PATH="$ORIG_PATH"
}

# Dispatch from <module> and assert codex received that module dir as its cwd.
assert_module_cwd() {
  local module="$1"
  local moddir
  moddir="$(cd "$FIXTURE/$module" && pwd -P)"
  cd "$moddir" || return 1
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  local got
  got="$(cat "$RECORD_CWD")"
  # The launcher resolves WorkDir via os.Getwd() (physical path); compare against
  # the physical module path so /tmp symlinks don't cause spurious mismatches.
  if [ "$got" != "$moddir" ]; then
    echo "dispatch from ./$module ran codex in: $got" >&2
    echo "expected module dir:                  $moddir" >&2
    echo "(repo root collapse is the bug this guards against)" >&2
    return 1
  fi
}

@test "dispatch from ./shared runs codex in ./shared" {
  assert_module_cwd shared
}

@test "dispatch from ./models runs codex in ./models" {
  assert_module_cwd models
}

@test "dispatch from ./server runs codex in ./server" {
  assert_module_cwd server
}

@test "dispatch from ./ui runs codex in ./ui" {
  assert_module_cwd ui
}

@test "dispatch from the repo root still runs codex at the root" {
  local root
  root="$(cd "$FIXTURE" && pwd -P)"
  cd "$root" || return 1
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(cat "$RECORD_CWD")" = "$root" ]
}

@test "auto-derives the module from CODEX_FILES at the repo root (no CODEX_WORKDIR)" {
  local root moddir
  root="$(cd "$FIXTURE" && pwd -P)"
  moddir="$(cd "$FIXTURE/server" && pwd -P)"
  cd "$root" || return 1
  CODEX_FILES="server/server_hello.go" run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(cat "$RECORD_CWD")" = "$moddir" ]
}

@test "does not auto-scope when CODEX_FILES span two modules" {
  local root
  root="$(cd "$FIXTURE" && pwd -P)"
  cd "$root" || return 1
  CODEX_FILES="server/server_hello.go,shared/shared_hello.go" run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(cat "$RECORD_CWD")" = "$root" ]
}

@test "worktree-isolated dispatch also runs codex in the module" {
  # The worktree fallback creates its tree under <repo>/.codex-dispatch, so the
  # same auto-derivation applies: codex runs in <worktree>/<module>, not the
  # worktree root. We assert the recorded thread cwd ends in the module dir.
  # (The fan-in result may be exit 4 here — the fake codex writes relative to the
  # broker's app-server cwd, not the thread cwd — so we assert only the recorded
  # cwd, which is the scoping signal.)
  local allowed
  allowed="$(mktemp)"
  echo "server/probe.go" > "$allowed"
  cd "$FIXTURE" || return 1
  CODEX_FILES="server/server.go" \
  FAKE_APPSERVER_EDIT="server/probe.go:package server" \
    run "$REPO_ROOT/scripts/graphrag-worktree-dispatch.sh" --allowed-file "$allowed"
  rm -f "$allowed"
  local got
  got="$(cat "$RECORD_CWD")"
  # cwd is inside a temp worktree but must end with the module path.
  [[ "$got" == *"/.codex-dispatch/graphrag-worktrees/"*"/server" ]]
}

@test "CODEX_WORKDIR pins the dispatch to a module from the repo root" {
  local root moddir
  root="$(cd "$FIXTURE" && pwd -P)"
  moddir="$(cd "$FIXTURE/server" && pwd -P)"
  cd "$root" || return 1
  CODEX_WORKDIR="server" run "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(cat "$RECORD_CWD")" = "$moddir" ]
}
