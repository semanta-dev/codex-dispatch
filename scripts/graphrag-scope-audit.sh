#!/usr/bin/env bash
# Verify a GraphRAG packet only changed paths listed in its Allowed files.
#
# Input:
#   GRAPHRAG_ALLOWED_FILES  newline-separated allowed paths, or
#   --allowed-file PATH     file containing newline-separated allowed paths
#
# Runtime artifacts are ignored:
#   .codex-dispatch/
#   __pycache__/
#   *.pyc
#   common untracked dependency/cache/temp outputs
#
# Extra newline-separated untracked ignore globs may be supplied with:
#   GRAPHRAG_SCOPE_AUDIT_IGNORE

set -euo pipefail

allowed_file=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --allowed-file)
      [ "$#" -ge 2 ] || { printf 'graphrag-scope-audit: --allowed-file needs a path\n' >&2; exit 2; }
      allowed_file="$2"
      shift 2
      ;;
    -*)
      printf 'graphrag-scope-audit: unknown flag: %s\n' "$1" >&2
      exit 2
      ;;
    *)
      printf 'graphrag-scope-audit: unexpected arg: %s\n' "$1" >&2
      exit 2
      ;;
  esac
done

if ! git rev-parse --show-toplevel >/dev/null 2>&1; then
  printf 'graphrag-scope-audit: cwd is not inside a git repository\n' >&2
  exit 2
fi

declare -A allowed
load_allowed() {
  local line
  while IFS= read -r line; do
    line="${line#\`}"
    line="${line%\`}"
    line="${line#./}"
    [ -z "$line" ] && continue
    case "$line" in
      /*|../*|*/../*|*/..)
        printf 'graphrag-scope-audit: unsafe allowed path: %s\n' "$line" >&2
        exit 2
        ;;
    esac
    allowed["$line"]=1
  done
}

if [ -n "$allowed_file" ]; then
  [ -f "$allowed_file" ] || { printf 'graphrag-scope-audit: allowed file not found: %s\n' "$allowed_file" >&2; exit 2; }
  load_allowed < "$allowed_file"
elif [ -n "${GRAPHRAG_ALLOWED_FILES:-}" ]; then
  load_allowed <<< "$GRAPHRAG_ALLOWED_FILES"
else
  printf 'graphrag-scope-audit: no allowed files supplied\n' >&2
  exit 2
fi

if [ "${#allowed[@]}" -eq 0 ]; then
  printf 'graphrag-scope-audit: allowed file list is empty\n' >&2
  exit 2
fi

is_ignored_untracked_artifact() {
	local path="$1"
	if git check-ignore --quiet -- "$path"; then
	  return 0
	fi
	case "$path" in
    .codex-dispatch/*|\
node_modules/*|*/node_modules/*|\
vendor/bundle/*|.bundle/*|\
.venv/*|venv/*|env/*|.tox/*|\
__pycache__/*|*/__pycache__/*|*.pyc|*.pyo|\
.pytest_cache/*|.mypy_cache/*|.ruff_cache/*|.coverage|coverage.xml|htmlcov/*|\
.cache/*|*/.cache/*|\
	tmp/*|temp/*|.tmp/*|logs/*|*.log|\
	.next/*|.nuxt/*|.svelte-kit/*|.turbo/*|\
	coverage/*)
	      return 0
	      ;;
    *) return 1 ;;
  esac
}

is_extra_ignored_untracked_artifact() {
  local path="$1"
  local pattern
  while IFS= read -r pattern; do
    [ -z "$pattern" ] && continue
    # GRAPHRAG_SCOPE_AUDIT_IGNORE intentionally accepts shell globs.
    # shellcheck disable=SC2254
    case "$path" in
      $pattern) return 0 ;;
    esac
  done <<< "${GRAPHRAG_SCOPE_AUDIT_IGNORE:-}"
  return 1
}

status_entries() {
  git status --porcelain=v1 -z --untracked-files=all | while IFS= read -r -d '' entry; do
    [ -z "$entry" ] && continue
    # Porcelain v1 -z format is two status columns, a space, then a
    # NUL-terminated path. Rename/copy records include a second NUL field
    # for the source path; the first path is the destination.
    local status="${entry:0:2}"
    local path="${entry:3}"
    path="${path#./}"
    printf '%s\0%s\0' "$status" "$path"
    # A rename/copy also touches the source path: a rename deletes it.
    # Audit the source against Allowed files too, so a rename whose
    # SOURCE is out of scope cannot smuggle an out-of-scope deletion.
    case "$status" in
      *R*|*C*)
        local old_path=""
        IFS= read -r -d '' old_path || true
        old_path="${old_path#./}"
        [ -n "$old_path" ] && printf '%s\0%s\0' "$status" "$old_path"
        ;;
    esac
  done
}

violations=()
audited=()
while IFS= read -r -d '' status && IFS= read -r -d '' path; do
  [ -z "$path" ] && continue
  if [ "$status" = "??" ] && { is_ignored_untracked_artifact "$path" || is_extra_ignored_untracked_artifact "$path"; }; then
    continue
  fi
  audited+=("$path")
  if [ -z "${allowed[$path]+x}" ]; then
    violations+=("$path")
  fi
done < <(status_entries)

if [ "${#violations[@]}" -gt 0 ]; then
  printf 'violation\n'
  printf 'Out-of-scope changed files:\n'
  printf -- '- %s\n' "${violations[@]}"
  exit 1
fi

printf 'clean\n'
if [ "${#audited[@]}" -eq 0 ]; then
  printf 'No non-ignored changed files.\n'
else
  printf 'Changed files within allowed scope:\n'
  printf -- '- %s\n' "${audited[@]}"
fi
