#!/usr/bin/env bash
# tests/reviewer/run-fixtures.sh — best-effort harness for codex-reviewer fixtures.
#
# For each tests/fixtures/reviewer/<name>/, build an input prompt from the
# fixture files, invoke `claude` headlessly with the codex-reviewer body
# appended to the system prompt, parse VERDICT/REASON from the response, and
# compare to expected_verdict.txt.
#
# The agent's verdict is a model judgment and is inherently variable. The
# fixture spec calls for >=80% match per fixture over 10 runs; this script
# accepts a REVIEWER_FIXTURE_RUNS count (default 1) and reports pass/fail
# tallies per fixture so a human can decide whether the rate is acceptable.
#
# Skip behavior: if `claude` is not on PATH or REVIEWER_FIXTURES_SKIP=1, the
# script prints a clear skip message and exits 0. CI without a Claude API key
# can rely on the skip path.
#
# Coverage note (inlined orchestrator reviewer path + exit_code==4):
#   The codex-orchestrator inlines this same review logic (orchestrator step 5c
#   == codex-reviewer sections 1-6), so a fixture that exercises codex-reviewer
#   also validates the orchestrator's inline reviewer. The two short-circuit
#   contracts must agree: `exit_code == 4` is the dispatch core's "completed
#   without meaningful edits" sentinel and MUST map to REASON=no-changes, not
#   codex-error. To cover that path, drop a fixture under
#   tests/fixtures/reviewer/<name>/ containing an empty diff.patch (no
#   `diff --git` headers) plus a result.json with {"exit_code": 4, ...} and an
#   expected_verdict.txt of `VERDICT: fail` / `REASON: no-changes`. When a
#   fixture provides result.json, this harness passes it to the reviewer as
#   RESULT PATH (see build_prompt) so the exit_code==4 short-circuit is actually
#   exercised; without it the reviewer still detects the empty diff as
#   no-changes.
#
# Flags:
#   --dry-run    Print the prompt for each fixture without calling claude.
#   --fixture N  Run only the fixture named N.

set -euo pipefail

err() { printf 'run-fixtures: %s\n' "$*" >&2; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
AGENT="$REPO_ROOT/agents/codex-reviewer.md"
FIXTURES_DIR="$REPO_ROOT/tests/fixtures/reviewer"
MODEL="${REVIEWER_MODEL:-claude-sonnet-4-6}"
RUNS="${REVIEWER_FIXTURE_RUNS:-1}"

dry_run=0
only_fixture=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry_run=1 ;;
    --fixture) shift; only_fixture="${1:-}" ;;
    -h|--help)
      cat <<USAGE
Usage: $0 [--dry-run] [--fixture <name>]

Env:
  REVIEWER_MODEL          model id (default claude-sonnet-4-6)
  REVIEWER_FIXTURE_RUNS   runs per fixture (default 1)
  REVIEWER_FIXTURES_SKIP  if set, skip without running claude
USAGE
      exit 0 ;;
    *) err "unknown arg: $1"; exit 64 ;;
  esac
  shift
done

if [ ! -f "$AGENT" ]; then
  err "agent file not found: $AGENT"
  exit 1
fi

if [ ! -d "$FIXTURES_DIR" ]; then
  err "fixtures dir not found: $FIXTURES_DIR"
  exit 1
fi

if [ -n "${REVIEWER_FIXTURES_SKIP:-}" ]; then
  err "REVIEWER_FIXTURES_SKIP is set; skipping reviewer fixture run."
  exit 0
fi

if [ "$dry_run" -eq 0 ] && ! command -v claude >/dev/null 2>&1; then
  err "claude CLI not on PATH; skipping reviewer fixture run."
  err "(set REVIEWER_FIXTURES_SKIP=1 to suppress this message in CI)"
  exit 0
fi

agent_body="$(awk '/^---$/{c++; next} c>=2' "$AGENT")"

build_prompt() {
  local fix="$1"
  printf 'TASK\n'
  cat "$fix/task.txt"
  printf '\nACCEPTANCE CRITERIA\n'
  cat "$fix/acceptance.txt"
  if [ -f "$fix/constraints.txt" ]; then
    printf '\nCONSTRAINTS\n'
    cat "$fix/constraints.txt"
  fi
  printf '\nDIFF PATH\n%s\n' "$fix/diff.patch"
  printf '\nDIFF CONTENT\n```\n'
  cat "$fix/diff.patch"
  printf '```\n'
  # Pass result.json when the fixture provides one so the reviewer's
  # exit_code==4 (no-changes) short-circuit can be exercised. Inline the
  # content too, since the headless model may not read the path off disk.
  if [ -f "$fix/result.json" ]; then
    printf '\nRESULT PATH\n%s\n' "$fix/result.json"
    printf '\nRESULT CONTENT\n```json\n'
    cat "$fix/result.json"
    printf '```\n'
  fi
  if [ -f "$fix/test-policy.txt" ]; then
    printf '\nTEST POLICY\n%s\n' "$(cat "$fix/test-policy.txt")"
  else
    printf '\nTEST POLICY\nskip\n'
  fi
  if [ -f "$fix/test-cmd.txt" ]; then
    printf '\nTEST CMD\n%s\n' "$(cat "$fix/test-cmd.txt")"
  fi
}

parse_field() {
  local field="$1" output="$2"
  printf '%s\n' "$output" \
    | awk -v field="^${field}:" '$0 ~ field {sub(field, ""); sub(/^[ \t]+/, ""); print; exit}'
}

run_one_fixture() {
  local fix="$1"
  local name expected_verdict expected_reason prompt
  name="$(basename "$fix")"
  expected_verdict="$(awk -F': *' '/^VERDICT:/ {print $2; exit}' "$fix/expected_verdict.txt")"
  expected_reason="$(awk -F': *' '/^REASON:/ {sub(/^REASON:[ ]*/, ""); print; exit}' "$fix/expected_verdict.txt")"
  prompt="$(build_prompt "$fix")"

  if [ "$dry_run" -eq 1 ]; then
    printf '\n=== %s (expected VERDICT=%s REASON=%s) ===\n' \
      "$name" "$expected_verdict" "$expected_reason"
    printf '%s\n' "$prompt"
    return 0
  fi

  local pass=0 fail=0 i output got_verdict got_reason
  for ((i = 1; i <= RUNS; i++)); do
    # See pick-iterations.sh: --bare requires ANTHROPIC_API_KEY; OAuth users
    # need plain --print so the keychain is read.
    local bare_flag=""
    [ -n "${ANTHROPIC_API_KEY:-}" ] && bare_flag="--bare"
    # shellcheck disable=SC2086
    output="$(claude $bare_flag --print --model "$MODEL" \
      --append-system-prompt "$agent_body" \
      "$prompt" </dev/null 2>/dev/null || true)"
    got_verdict="$(parse_field VERDICT "$output")"
    got_reason="$(parse_field REASON "$output")"
    if [ "$got_verdict" = "$expected_verdict" ] && [ "$got_reason" = "$expected_reason" ]; then
      pass=$((pass + 1))
    else
      fail=$((fail + 1))
      err "  [$name run $i] mismatch — got VERDICT=$got_verdict REASON=$got_reason"
    fi
  done

  printf '%-26s  expected=%-13s/%-30s  pass=%d  fail=%d\n' \
    "$name" "$expected_verdict" "$expected_reason" "$pass" "$fail"
}

mapfile -t fixtures < <(find "$FIXTURES_DIR" -mindepth 1 -maxdepth 1 -type d | sort)

if [ "${#fixtures[@]}" -eq 0 ]; then
  err "no fixtures found under $FIXTURES_DIR"
  exit 1
fi

for fix in "${fixtures[@]}"; do
  if [ -n "$only_fixture" ] && [ "$(basename "$fix")" != "$only_fixture" ]; then
    continue
  fi
  run_one_fixture "$fix"
done

if [ "$dry_run" -eq 0 ]; then
  err "fixture run complete (model judgment is variable; expect >=80% match per fixture over 10 runs)"
fi
