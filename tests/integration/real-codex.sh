#!/usr/bin/env bash
# Opt-in: drives the v0.3.0 broker against real `codex app-server`.
#
# Costs a few cents in OpenAI charges per run (one Codex turn). Not in CI.
# Use this to validate that the broker still speaks codex's actual JSON-RPC
# protocol before tagging a release.
#
# Run: REAL_CODEX=1 tests/integration/real-codex.sh

set -euo pipefail

if [ "${REAL_CODEX:-}" != "1" ]; then
  echo "skip: set REAL_CODEX=1 to enable (~few cents per run)"
  exit 0
fi

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d)"
cleanup() {
  if [ -f "$WORK/.codex-dispatch/broker.pid" ]; then
    kill "$(cat "$WORK/.codex-dispatch/broker.pid")" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> build codex-dispatch"
go build -o "$WORK/codex-dispatch" "$REPO_ROOT/cmd/codex-dispatch"

cd "$WORK"
echo "==> init throwaway repo at $WORK"
git init -q -b main
git config user.email t@t
git config user.name t
echo "init" > README.md
git add . && git commit -q -m init

export CODEX_TASK="create a file named greeting.txt containing the single word Hello"
export CODEX_ACCEPTANCE="greeting.txt exists and contains 'Hello'"
export CODEX_SANDBOX="workspace-write"
export CLAUDE_SESSION_ID="real-codex-smoke-$$"

echo "==> dispatch (broker auto-spawned)"
"$WORK/codex-dispatch" dispatch

# Locate the most recent run dir. find handles non-alphanumeric paths and is
# what shellcheck prefers; the ts-pid run-dir scheme sorts identically to mtime.
RESULT_DIR="$(find .codex-dispatch/runs -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' 2>/dev/null | sort -nr | head -n1 | cut -d' ' -f2-)"
if [ -z "$RESULT_DIR" ]; then
  echo "FAIL: no run dir under .codex-dispatch/runs/"
  exit 1
fi
echo "==> result dir: $RESULT_DIR"

# Assertions.
if [ ! -f "$RESULT_DIR/result.json" ]; then
  echo "FAIL: no result.json"
  exit 1
fi
EXIT_CODE="$(jq -r .exit_code "$RESULT_DIR/result.json")"
SESSION_ID="$(jq -r .session_id "$RESULT_DIR/result.json")"
if [ "$EXIT_CODE" != "0" ]; then
  echo "FAIL: exit_code=$EXIT_CODE"
  jq . "$RESULT_DIR/result.json"
  exit 1
fi
if [ -z "$SESSION_ID" ] || [ "$SESSION_ID" = "null" ]; then
  echo "FAIL: empty session_id"
  exit 1
fi
if [ ! -f greeting.txt ]; then
  echo "FAIL: greeting.txt not created"
  ls -la
  exit 1
fi
if ! grep -q "Hello" greeting.txt; then
  echo "FAIL: greeting.txt missing 'Hello'"
  cat greeting.txt
  exit 1
fi
if ! grep -q '"method":"turn/completed"' "$RESULT_DIR/stdout.log"; then
  echo "FAIL: no turn/completed in stdout.log"
  tail "$RESULT_DIR/stdout.log"
  exit 1
fi

echo "real-codex smoke: PASS"
echo "  exit_code:  $EXIT_CODE"
echo "  session_id: $SESSION_ID"
echo "  log lines:  $(wc -l <"$RESULT_DIR/stdout.log")"
