#!/usr/bin/env bash
# clean-verify — run a verification command against HEAD + a dispatch run's diff
# in a throwaway git worktree, so verification cannot falsely pass by reading
# dirty-tree, gitignored, or otherwise-uncommitted state. Used by the
# codex-orchestrator for `--clean-verify`.
#
# Usage:
#   clean-verify.sh <run_dir> <verify-cmd> [args...]
#
#   <run_dir>     a dispatch run directory containing diff.patch (codex's edits).
#   <verify-cmd>  the command (and args) to run inside the isolated worktree.
#
# Behavior:
#   - Creates a detached worktree at HEAD under a temp dir, applies
#     <run_dir>/diff.patch onto it, runs the verify command there, then removes
#     the worktree (always — even on failure; set CLEAN_VERIFY_KEEP=1 to keep it).
#   - A diff that will not apply cleanly to HEAD is itself a signal that the
#     change depends on uncommitted state; that exits 65 (distinct from a normal
#     verify failure) so the caller can surface it specifically.
#
# Exit codes:
#   <verify-cmd's exit code>  normal pass/fail of the verification
#   2   usage error (missing run_dir or verify command)
#   6   required tool missing (git) or run_dir/diff.patch not found
#   65  codex's diff did not apply cleanly to HEAD (depends on uncommitted state)

set -euo pipefail

command -v git >/dev/null 2>&1 || { printf 'clean-verify: git not found on PATH\n' >&2; exit 6; }

run_dir="${1:-}"
[ -n "$run_dir" ] || { printf 'clean-verify: usage: clean-verify.sh <run_dir> <verify-cmd> [args...]\n' >&2; exit 2; }
shift
[ "$#" -ge 1 ] || { printf 'clean-verify: no verify command given\n' >&2; exit 2; }

diff_path="$run_dir/diff.patch"
[ -d "$run_dir" ] || { printf 'clean-verify: run_dir not found: %s\n' "$run_dir" >&2; exit 6; }

repo="$(git rev-parse --show-toplevel 2>/dev/null)" || { printf 'clean-verify: not inside a git repository\n' >&2; exit 6; }

wt="$(mktemp -d "${TMPDIR:-/tmp}/codex-clean-verify.XXXXXX")"
# shellcheck disable=SC2329  # invoked indirectly via 'trap cleanup EXIT' below
cleanup() {
  if [ -n "${CLEAN_VERIFY_KEEP:-}" ]; then
    printf 'clean-verify: keeping worktree %s (CLEAN_VERIFY_KEEP set)\n' "$wt" >&2
    return
  fi
  git -C "$repo" worktree remove --force "$wt" >/dev/null 2>&1 || rm -rf "$wt"
}
trap cleanup EXIT

git -C "$repo" worktree add --quiet --detach "$wt" HEAD

if [ -s "$diff_path" ]; then
  if ! git -C "$wt" apply --whitespace=nowarn "$diff_path" 2>/dev/null; then
    printf "clean-verify: codex's diff did not apply cleanly to HEAD — the change likely depends on uncommitted/gitignored state\n" >&2
    exit 65
  fi
fi

# Run the verification in the isolated tree; propagate its exit code verbatim.
set +e
( cd "$wt" && "$@" )
rc=$?
set -e
exit "$rc"
