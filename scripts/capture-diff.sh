#!/usr/bin/env bash
# capture-diff launcher — see scripts/dispatch-codex.sh for the cache flow.

set -euo pipefail

if [ -n "${CODEX_DISPATCH_BIN:-}" ] && [ -x "${CODEX_DISPATCH_BIN}" ]; then
  exec "${CODEX_DISPATCH_BIN}" capture-diff "$@"
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
version_file="${script_dir}/../VERSION"
if [ ! -f "$version_file" ]; then
  printf 'capture-diff: VERSION file missing at %s\n' "$version_file" >&2
  exit 6
fi
PINNED_VERSION="$(cat "$version_file")"
cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/codex-dispatch/v${PINNED_VERSION}"
if [ -x "${cache_dir}/codex-dispatch" ]; then
  exec "${cache_dir}/codex-dispatch" capture-diff "$@"
fi

printf 'capture-diff: codex-dispatch binary not found; set CODEX_DISPATCH_BIN or run /codex once to populate the cache\n' >&2
exit 6
