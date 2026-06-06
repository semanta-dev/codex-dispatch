#!/usr/bin/env bash
# pick-iterations launcher — resolves the codex-dispatch binary and execs the
# subcommand. The Go binary is the source of truth; this script only locates
# it. See scripts/dispatch-codex.sh for the full download/verify flow.

set -euo pipefail

if [ -n "${CODEX_DISPATCH_BIN:-}" ] && [ -x "${CODEX_DISPATCH_BIN}" ]; then
  exec "${CODEX_DISPATCH_BIN}" pick-iterations
fi

# Fall through to the same cache that scripts/dispatch-codex.sh populates on
# first run. We do not attempt the download here; if the cache is empty, the
# user should run /codex once (which goes through the dispatch launcher) to
# trigger the download, then this script will find the binary on its next call.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
version_file="${script_dir}/../VERSION"
if [ ! -f "$version_file" ]; then
  printf 'pick-iterations: VERSION file missing at %s\n' "$version_file" >&2
  exit 6
fi
PINNED_VERSION="$(cat "$version_file")"
cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/codex-dispatch/v${PINNED_VERSION}"
if [ -x "${cache_dir}/codex-dispatch" ]; then
  exec "${cache_dir}/codex-dispatch" pick-iterations
fi

printf 'pick-iterations: codex-dispatch binary not found; set CODEX_DISPATCH_BIN or run /codex once to populate the cache\n' >&2
exit 6
