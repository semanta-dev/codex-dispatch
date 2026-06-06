#!/usr/bin/env bash
# Run one GraphRAG packet dispatch in an isolated git worktree, audit the
# packet's allowed files there, then fan in only those allowed paths.
#
# Required input:
#   GRAPHRAG_ALLOWED_FILES  newline-separated allowed paths, or
#   --allowed-file PATH     file containing newline-separated allowed paths
#
# Dispatch input is the normal codex-dispatch environment:
#   CODEX_TASK, CODEX_ACCEPTANCE, CODEX_CONSTRAINTS, CODEX_FILES, ...
#
# Optional:
#   GRAPHRAG_DISPATCH_COMMAND  command to run instead of dispatch-codex.sh
#   GRAPHRAG_WORKTREE_KEEP=1   keep the temporary worktree for debugging
#   GRAPHRAG_WORKTREE_PARENT   checkout to fan changes into; default cwd repo
#   GRAPHRAG_WORKTREE_BASE     git revision to check out; default HEAD
#   CODEX_FILES                comma-separated input files to seed from parent

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
plugin_root="$(cd "$script_dir/.." && pwd)"

allowed_file=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --allowed-file)
      [ "$#" -ge 2 ] || { printf 'graphrag-worktree-dispatch: --allowed-file needs a path\n' >&2; exit 2; }
      allowed_file="$2"
      shift 2
      ;;
    -*)
      printf 'graphrag-worktree-dispatch: unknown flag: %s\n' "$1" >&2
      exit 2
      ;;
    *)
      printf 'graphrag-worktree-dispatch: unexpected arg: %s\n' "$1" >&2
      exit 2
      ;;
  esac
done

parent="${GRAPHRAG_WORKTREE_PARENT:-$(pwd)}"
if ! parent_root="$(git -C "$parent" rev-parse --show-toplevel 2>/dev/null)"; then
  printf 'graphrag-worktree-dispatch: parent is not inside a git repository\n' >&2
  exit 2
fi
parent="$parent_root"
base="${GRAPHRAG_WORKTREE_BASE:-HEAD}"

work_root="$parent/.codex-dispatch/graphrag-worktrees"
run_root="$parent/.codex-dispatch/graphrag-worktree-runs"
mkdir -p "$work_root" "$run_root"

pid_alive() {
  # Returns success when the given numeric PID names a live process.
  local pid="$1"
  case "$pid" in
    ''|*[!0-9]*) return 1 ;;
  esac
  kill -0 "$pid" 2>/dev/null
}

# Startup GC: drop git worktree registrations whose dirs are gone, then remove
# leftover worktree dirs whose owning dispatch PID is dead so a crashed run does
# not leak detached worktrees forever. Worktree dirs are named
# "<timestamp>-<pid>-a<attempt>"; the PID is the field before the final "-aN".
git -C "$parent" worktree prune >/dev/null 2>&1 || true
for stale in "$work_root"/*; do
  [ -d "$stale" ] || continue
  base_name="${stale##*/}"
  case "$base_name" in
    *-a[0-9]*)
      owner_pid="${base_name%-a*}"
      owner_pid="${owner_pid##*-}"
      ;;
    *) owner_pid="" ;;
  esac
  if [ -n "$owner_pid" ] && pid_alive "$owner_pid"; then
    continue
  fi
  git -C "$parent" worktree remove --force "$stale" >/dev/null 2>&1 || rm -rf "$stale"
done
git -C "$parent" worktree prune >/dev/null 2>&1 || true

run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
worktree=""
worktrees=()
run_dir="$run_root/$run_id"
mkdir -p "$run_dir"

allowed_tmp="$run_dir/allowed-files.txt"
if [ -n "$allowed_file" ]; then
  [ -f "$allowed_file" ] || { printf 'graphrag-worktree-dispatch: allowed file not found: %s\n' "$allowed_file" >&2; exit 2; }
  # shellcheck disable=SC2016 # Literal backticks are Markdown path cleanup.
  sed 's/^`//; s/`$//; s#^\./##; /^[[:space:]]*$/d' "$allowed_file" > "$allowed_tmp"
elif [ -n "${GRAPHRAG_ALLOWED_FILES:-}" ]; then
  # shellcheck disable=SC2016 # Literal backticks are Markdown path cleanup.
  printf '%s\n' "$GRAPHRAG_ALLOWED_FILES" | sed 's/^`//; s/`$//; s#^\./##; /^[[:space:]]*$/d' > "$allowed_tmp"
else
  printf 'graphrag-worktree-dispatch: no allowed files supplied\n' >&2
  exit 2
fi
if [ ! -s "$allowed_tmp" ]; then
  printf 'graphrag-worktree-dispatch: allowed file list is empty\n' >&2
  exit 2
fi

# Hash of an allowed path as it exists at the worktree's starting point ($base).
# The worktree branches from $base, so this is the version any edit is layered
# on top of. Empty output means the path does not exist at $base.
base_hash() {
  git -C "$parent" rev-parse -q --verify "$base:$1" 2>/dev/null || true
}

declare -A allowed
declare -A parent_allowed_hash
declare -A parent_wip
while IFS= read -r line; do
  line="${line#./}"
  [ -z "$line" ] && continue
  case "$line" in
    /*|../*|*/../*|*/..)
      printf 'graphrag-worktree-dispatch: unsafe allowed path: %s\n' "$line" >&2
      exit 2
      ;;
  esac
  allowed["$line"]=1
  if [ -e "$parent/$line" ] || [ -L "$parent/$line" ]; then
    parent_allowed_hash["$line"]="$(git -C "$parent" hash-object -- "$line")"
  else
    parent_allowed_hash["$line"]="__missing__"
  fi
  # A worktree edit is layered on the $base version, so if the parent's
  # working-tree copy already diverges from $base it carries uncommitted WIP
  # that a blind fan-in would silently overwrite. Record it now and refuse
  # fan-in for such paths below.
  parent_wip["$line"]=0
  base_blob="$(base_hash "$line")"
  if [ "${parent_allowed_hash[$line]}" != "${base_blob:-__missing__}" ]; then
    parent_wip["$line"]=1
  fi
done < "$allowed_tmp"

is_allowed_path() {
  local key="$1"
  [[ -v "allowed[$key]" ]]
}

# Seed/base consistency precondition: the worktree branches from $base, so a
# CODEX_FILES input that is tracked at $base but dirty in the parent working
# tree would be seeded as the stale $base version (the working-tree edits are
# silently dropped). Fail fast and tell the operator to commit those inputs.
if [ -n "${CODEX_FILES:-}" ]; then
  IFS=',' read -r -a precheck_seeds <<< "$CODEX_FILES"
  for precheck in "${precheck_seeds[@]}"; do
    precheck="${precheck#"${precheck%%[![:space:]]*}"}"
    precheck="${precheck%"${precheck##*[![:space:]]}"}"
    precheck="${precheck#./}"
    [ -z "$precheck" ] && continue
    case "$precheck" in
      /*|../*|*/../*|*/..) continue ;;
    esac
    # Only tracked-at-base inputs are seeded from $base; untracked inputs are
    # copied from the parent working tree below, so they are already current.
    if ! git -C "$parent" cat-file -e "$base:$precheck" 2>/dev/null; then
      continue
    fi
    seed_base_blob="$(base_hash "$precheck")"
    seed_parent_blob="__missing__"
    if [ -e "$parent/$precheck" ] || [ -L "$parent/$precheck" ]; then
      seed_parent_blob="$(git -C "$parent" hash-object -- "$precheck")"
    fi
    if [ "$seed_parent_blob" != "${seed_base_blob:-__missing__}" ]; then
      printf 'graphrag-worktree-dispatch: input %s has uncommitted changes; commit your inputs before an isolated worktree run\n' "$precheck" >&2
      exit 2
    fi
  done
fi

cleanup() {
  if [ "${GRAPHRAG_WORKTREE_KEEP:-0}" != "1" ]; then
    for wt in "${worktrees[@]:-}"; do
      [ -d "$wt" ] && git -C "$parent" worktree remove --force "$wt" >/dev/null 2>&1 || true
    done
  fi
}
trap cleanup EXIT

dispatch_cmd="${GRAPHRAG_DISPATCH_COMMAND:-$plugin_root/scripts/dispatch-codex.sh}"
dispatch_attempts="${GRAPHRAG_DISPATCH_ATTEMPTS:-2}"
case "$dispatch_attempts" in
  ''|*[!0-9]*) dispatch_attempts=2 ;;
esac
[ "$dispatch_attempts" -lt 1 ] && dispatch_attempts=1

last_dispatch_dir=""
dispatch_rc=1
result_rc=64
selected_attempt=0
for attempt in $(seq 1 "$dispatch_attempts"); do
  attempt_worktree="$work_root/$run_id-a$attempt"
  worktrees+=("$attempt_worktree")
  git -C "$parent" worktree add --detach "$attempt_worktree" "$base" > "$run_dir/worktree-add-$attempt.out" 2> "$run_dir/worktree-add-$attempt.err"

  seed_file="$run_dir/seeded-inputs-$attempt.tsv"
  : > "$seed_file"
  if [ -n "${CODEX_FILES:-}" ]; then
    IFS=',' read -r -a seed_paths <<< "$CODEX_FILES"
    for seed in "${seed_paths[@]}"; do
      seed="${seed#"${seed%%[![:space:]]*}"}"
      seed="${seed%"${seed##*[![:space:]]}"}"
      seed="${seed#./}"
      [ -z "$seed" ] && continue
      case "$seed" in
        /*|../*|*/../*|*/..) continue ;;
      esac
      if git -C "$attempt_worktree" ls-files --error-unmatch -- "$seed" >/dev/null 2>&1; then
        continue
      fi
      if [ -f "$parent/$seed" ]; then
        mkdir -p "$attempt_worktree/$(dirname "$seed")"
        cp -p "$parent/$seed" "$attempt_worktree/$seed"
        hash="$(git -C "$attempt_worktree" hash-object -- "$seed")"
        printf '%s\t%s\n' "$seed" "$hash" >> "$seed_file"
      fi
    done
  fi

  set +e
  (
    cd "$attempt_worktree" || exit 2
    "$dispatch_cmd"
  ) > "$run_dir/dispatch-$attempt.stdout" 2> "$run_dir/dispatch-$attempt.stderr"
  dispatch_rc=$?
  set -e

  cp "$run_dir/dispatch-$attempt.stdout" "$run_dir/dispatch.stdout"
  cp "$run_dir/dispatch-$attempt.stderr" "$run_dir/dispatch.stderr"
  last_dispatch_dir="$(tail -n 1 "$run_dir/dispatch-$attempt.stdout" 2>/dev/null || true)"
  result_rc=64
  if [ -n "$last_dispatch_dir" ] && [ -f "$last_dispatch_dir/result.json" ]; then
    result_rc="$(jq -r '.exit_code // 64' "$last_dispatch_dir/result.json")"
  fi
  printf '%s\t%s\t%s\t%s\n' "$attempt" "$dispatch_rc" "$result_rc" "$last_dispatch_dir" >> "$run_dir/dispatch-attempts.tsv"

  while IFS=$'\t' read -r seed hash; do
    [ -z "$seed" ] && continue
    is_allowed_path "$seed" && continue
    if [ -f "$attempt_worktree/$seed" ]; then
      current_hash="$(git -C "$attempt_worktree" hash-object -- "$seed")"
      if [ "$current_hash" = "$hash" ]; then
        rm -f "$attempt_worktree/$seed"
      fi
    fi
  done < "$seed_file"

	  if [ "$dispatch_rc" -eq 0 ] && [ "$result_rc" -eq 0 ]; then
	    worktree="$attempt_worktree"
	    selected_attempt="$attempt"
	    break
	  fi
done

printf '%s\n' "$last_dispatch_dir" > "$run_dir/dispatch-run-dir.txt"
printf '%s\n' "$dispatch_rc" > "$run_dir/dispatch.rc"
printf '%s\n' "$result_rc" > "$run_dir/dispatch-result.rc"
printf '%s\n' "$selected_attempt" > "$run_dir/dispatch-selected-attempt.txt"

if [ -z "$worktree" ]; then
  if [ "$dispatch_rc" -ne 0 ]; then
    printf 'graphrag-worktree-dispatch: dispatch command failed with rc=%s\n' "$dispatch_rc" >&2
    printf '%s\n' "$run_dir"
    exit "$dispatch_rc"
  fi
  printf 'graphrag-worktree-dispatch: dispatch result failed with exit_code=%s\n' "$result_rc" >&2
  printf '%s\n' "$run_dir"
  exit 1
fi

if [ -n "$last_dispatch_dir" ] && [ -f "$last_dispatch_dir/result.json" ]; then
  cp -p "$last_dispatch_dir/result.json" "$run_dir/dispatch-result.json"
fi
if [ -n "$last_dispatch_dir" ] && [ -f "$last_dispatch_dir/stdout.log" ]; then
  cp -p "$last_dispatch_dir/stdout.log" "$run_dir/dispatch-stdout.log"
fi

set +e
(
  cd "$worktree" || exit 2
  "$plugin_root/scripts/graphrag-scope-audit.sh" --allowed-file "$allowed_tmp"
) > "$run_dir/scope-audit.out" 2> "$run_dir/scope-audit.err"
audit_rc=$?
set -e
printf '%s\n' "$audit_rc" > "$run_dir/scope-audit.rc"

if [ "$audit_rc" -ne 0 ]; then
  printf 'graphrag-worktree-dispatch: scope audit failed\n' >&2
  printf '%s\n' "$run_dir"
  exit 1
fi

changed_file="$run_dir/changed-files.txt"
changed_status="$run_dir/changed-status.z"
: > "$changed_file"
: > "$changed_status"

while IFS= read -r -d '' entry; do
  [ -z "$entry" ] && continue
  status="${entry:0:2}"
  path="${entry:3}"
  path="${path#./}"
  old_path=""
  case "$status" in
    *R*|*C*)
      IFS= read -r -d '' old_path || true
      old_path="${old_path#./}"
      ;;
  esac
  if is_allowed_path "$path"; then
    printf '%s\0%s\0' "$status" "$path" >> "$changed_status"
    printf '%s\n' "$path" >> "$changed_file"
  fi
  # A rename moves content off the OLD path. If the source is also allowed and
  # was not itself recreated in the worktree, schedule it for deletion at
  # fan-in so the parent gets the renamed file once, not a stale duplicate.
  case "$status" in
    *R*)
      if [ -n "$old_path" ] && is_allowed_path "$old_path" \
        && [ ! -e "$worktree/$old_path" ] && [ ! -L "$worktree/$old_path" ]; then
        printf 'DR\0%s\0' "$old_path" >> "$changed_status"
        printf '%s\n' "$old_path" >> "$changed_file"
      fi
      ;;
  esac
done < <(git -C "$worktree" status --porcelain=v1 -z --untracked-files=all)

lockdir="$parent/.codex-dispatch/graphrag-fanin.lock"
lock_owner="$lockdir/owner"
lock_acquired=0
# Reclaim a lock whose owner PID is dead so a crashed peer cannot wedge fan-in
# for the full retry window; only reclaim once the lock has aged past the grace
# period to avoid racing a freshly created lock before its owner file lands.
lock_stale_secs=5
reclaim_stale_lock() {
  [ -d "$lockdir" ] || return 1
  local owner_pid="" owner_age=0 now mtime
  if [ -f "$lock_owner" ]; then
    owner_pid="$(head -n 1 "$lock_owner" 2>/dev/null || true)"
  fi
  if pid_alive "$owner_pid"; then
    return 1
  fi
  now="$(date +%s)"
  mtime="$(stat -c %Y "$lockdir" 2>/dev/null || stat -f %m "$lockdir" 2>/dev/null || echo "$now")"
  owner_age=$(( now - mtime ))
  if [ "$owner_age" -lt "$lock_stale_secs" ]; then
    return 1
  fi
  # Owner is dead and the lock is old: take it over.
  rmdir "$lockdir" 2>/dev/null || rm -rf "$lockdir" 2>/dev/null || true
  return 0
}
for _ in $(seq 1 600); do
  if mkdir "$lockdir" 2>/dev/null; then
    printf '%s\n%s\n' "$$" "$run_id" > "$lock_owner"
    lock_acquired=1
    break
  fi
  reclaim_stale_lock || true
  sleep 0.1
done
if [ "$lock_acquired" -ne 1 ]; then
  printf 'graphrag-worktree-dispatch: could not acquire fan-in lock\n' >&2
  printf '%s\n' "$run_dir"
  exit 1
fi
trap 'rm -rf "$lockdir" >/dev/null 2>&1 || true; cleanup' EXIT

while IFS= read -r -d '' status && IFS= read -r -d '' path; do
  [ -z "$path" ] && continue
  src="$worktree/$path"
  dst="$parent/$path"
  current_hash="__missing__"
  if [ -e "$dst" ] || [ -L "$dst" ]; then
    current_hash="$(git -C "$parent" hash-object -- "$path")"
  fi
  # Refuse fan-in if the parent path carried uncommitted WIP at startup: the
  # worktree edited the $base version, so writing over the parent here would
  # silently destroy those local changes. Ask the operator to commit/stash.
  if [ "${parent_wip[$path]:-0}" = "1" ]; then
    printf 'graphrag-worktree-dispatch: parent %s has uncommitted changes; commit or stash before an isolated worktree run\n' "$path" >&2
    printf '%s\n' "$run_dir"
    exit 1
  fi
  if [ "$current_hash" != "${parent_allowed_hash[$path]:-__missing__}" ]; then
    printf 'graphrag-worktree-dispatch: fan-in conflict for %s\n' "$path" >&2
    printf '%s\n' "$run_dir"
    exit 1
  fi
  if [ "$status" = "DR" ]; then
    # Rename source: delete the stale original from the parent.
    rm -f "$dst"
  elif [ -e "$src" ] || [ -L "$src" ]; then
    mkdir -p "$(dirname "$dst")"
    cp -p "$src" "$dst"
  else
    rm -f "$dst"
  fi
done < "$changed_status"

rm -rf "$lockdir" >/dev/null 2>&1 || true
trap cleanup EXIT

printf 'graphrag-worktree-dispatch: run_dir=%s\n' "$run_dir"
printf 'graphrag-worktree-dispatch: worktree=%s\n' "$worktree"
printf 'graphrag-worktree-dispatch: dispatch_run_dir=%s\n' "$last_dispatch_dir"
printf 'graphrag-worktree-dispatch: dispatch_result_json=%s\n' "$run_dir/dispatch-result.json"
printf 'graphrag-worktree-dispatch: dispatch_stdout_log=%s\n' "$run_dir/dispatch-stdout.log"
printf 'graphrag-worktree-dispatch: changed_files=%s\n' "$(paste -sd, "$changed_file")"
printf '%s\n' "$run_dir"
