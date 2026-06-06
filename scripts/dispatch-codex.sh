#!/usr/bin/env bash
# dispatch-codex launcher — locate or download the codex-dispatch binary and
# exec the dispatch subcommand. The Go binary is the source of truth; this
# script handles only the trust boundary (download + checksum verification).
#
# Resolution order:
#   1. $CODEX_DISPATCH_BIN (escape hatch for local dev and CI tests).
#   2. ${cache}/v${PINNED_VERSION}/codex-dispatch (already extracted).
#   3. ${cache}/v${PINNED_VERSION}/manual/codex-dispatch (offline-install slot).
#   4. Download tarball + checksums.txt from GitHub Releases; verify; extract.
#
# Exit codes (launcher-only, distinct from the Go binary's 0/2/3/64):
#   5 checksum mismatch
#   6 required tool missing
#   7 network unreachable
#   8 tarball corrupt despite matching checksum

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
version_file="${script_dir}/../VERSION"
if [ ! -f "$version_file" ]; then
  printf 'codex-dispatch: VERSION file missing at %s\n' "$version_file" >&2
  exit 6
fi
PINNED_VERSION="$(cat "$version_file")"

err() { printf 'codex-dispatch: %s\n' "$*" >&2; }

# --- platform detection -----------------------------------------------------
detect_platform() {
  local os arch
  case "$(uname -s)" in
    Linux)  os="linux"  ;;
    Darwin) os="darwin" ;;
    *)      err "unsupported OS: $(uname -s)"; exit 6 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) err "unsupported arch: $(uname -m)"; exit 6 ;;
  esac
  printf '%s-%s\n' "$os" "$arch"
}

# --- tool detection ---------------------------------------------------------
require_tools() {
  command -v tar >/dev/null 2>&1 || {
    err "tar required (install with: apt-get install tar, dnf install tar, or brew install gnu-tar)"
    exit 6
  }
  if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
    err "curl or wget required (install with: apt-get install curl or brew install curl)"
    exit 6
  fi
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    err "sha256sum or shasum required (provided by coreutils on Linux; preinstalled on macOS)"
    exit 6
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

fetch_url() {
  local url="$1" dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl --fail --silent --show-error --location --output "$dest" "$url" || return 7
  else
    wget --quiet --output-document="$dest" "$url" || return 7
  fi
}

# --- locking ----------------------------------------------------------------
with_lock() {
  local lockdir="$1"; shift
  mkdir -p "$lockdir"
  if command -v flock >/dev/null 2>&1; then
    local fd
    exec {fd}>"$lockdir/.lock"
    flock "$fd"
    "$@"
    local rc=$?
    trap - RETURN
    eval "exec $fd>&-"
    return "$rc"
  else
    local sentinel="$lockdir/.lock.d"
    local tries=0
    while ! mkdir "$sentinel" 2>/dev/null; do
      tries=$((tries + 1))
      [ "$tries" -gt 60 ] && { err "lock contention on $lockdir"; return 1; }
      sleep 1
    done
    trap 'rmdir "$sentinel" 2>/dev/null || true' RETURN EXIT
    "$@"
  fi
}

download_and_verify() {
  local version="$1" platform="$2" cache="$3"
  local base="${CODEX_DISPATCH_RELEASE_URL:-https://github.com/semanta-dev/codex-dispatch/releases/download/v${version}}"
  local tarball="codex-dispatch_${platform}.tar.gz"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  if ! fetch_url "${base}/${tarball}" "$tmp/$tarball"; then
    err "cannot reach ${base} to download v${version} binary."
    err "For offline install, download codex-dispatch_${platform}.tar.gz and"
    err "checksums.txt from"
    err "https://github.com/semanta-dev/codex-dispatch/releases/tag/v${version}"
    err "and place them in ${cache}/manual/, then re-run."
    exit 7
  fi
  if ! fetch_url "${base}/checksums.txt" "$tmp/checksums.txt"; then
    err "fetched tarball but cannot reach checksums.txt"
    exit 7
  fi

  local expected actual
  expected="$(grep " ${tarball}\$" "$tmp/checksums.txt" | awk '{print $1}')"
  if [ -z "$expected" ]; then
    err "checksums.txt has no entry for ${tarball}"; exit 5
  fi
  actual="$(sha256_of "$tmp/$tarball")"
  if [ "$expected" != "$actual" ]; then
    err "checksum mismatch: expected $expected got $actual"; exit 5
  fi

  mkdir -p "$cache"
  if ! tar -xzf "$tmp/$tarball" -C "$cache" codex-dispatch 2>/dev/null; then
    if ! tar -xzf "$tmp/$tarball" -C "$cache"; then
      err "tarball is corrupt"
      exit 8
    fi
  fi
  if [ ! -x "$cache/codex-dispatch" ]; then
    err "extracted archive missing codex-dispatch binary"
    exit 8
  fi
  chmod +x "$cache/codex-dispatch"
}

# --- main -------------------------------------------------------------------

if [ -n "${CODEX_DISPATCH_BIN:-}" ] && [ -x "${CODEX_DISPATCH_BIN}" ]; then
  exec "${CODEX_DISPATCH_BIN}" dispatch "$@"
fi

require_tools
PLATFORM="$(detect_platform)"
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/codex-dispatch/v${PINNED_VERSION}"
BIN="${CACHE_DIR}/codex-dispatch"

if [ ! -x "$BIN" ]; then
  if [ -x "${CACHE_DIR}/manual/codex-dispatch" ]; then
    cp "${CACHE_DIR}/manual/codex-dispatch" "$BIN"
    chmod +x "$BIN"
  else
    with_lock "$CACHE_DIR" download_and_verify "$PINNED_VERSION" "$PLATFORM" "$CACHE_DIR"
  fi
fi

exec "$BIN" dispatch "$@"
