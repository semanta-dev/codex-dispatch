#!/usr/bin/env bash
# Claude Code SessionStart hook → codex-dispatch hook session-start.
set -euo pipefail

if [ -n "${CODEX_DISPATCH_BIN:-}" ] && [ -x "${CODEX_DISPATCH_BIN}" ]; then
  exec "${CODEX_DISPATCH_BIN}" hook session-start
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
version_file="${script_dir}/../../VERSION"
if [ ! -f "$version_file" ]; then
  printf '{"continue":true}\n'
  exit 0
fi
PINNED_VERSION="$(cat "$version_file")"
cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/codex-dispatch/v${PINNED_VERSION}"
if [ -x "${cache_dir}/codex-dispatch" ]; then
  exec "${cache_dir}/codex-dispatch" hook session-start
fi

# Best-effort: if the binary isn't found, just continue silently.
printf '{"continue":true}\n'
