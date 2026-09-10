#!/usr/bin/env bash
# dispatch-codex launcher — locate or download the codex-dispatch binary and
# exec the dispatch subcommand. The Go binary is the source of truth; this
# script handles only the trust boundary (download + checksum verification).
#
# Resolution order:
#   1. $CODEX_DISPATCH_BIN (escape hatch for local dev and CI tests).
#   2. ${cache}/v${PINNED_VERSION}/codex-dispatch (already extracted).
#   3. ${cache}/v${PINNED_VERSION}/manual/codex-dispatch (offline-install slot).
#   4. Download the archive (tar.gz on Linux/macOS, zip on Windows) +
#      checksums.txt from GitHub Releases; verify; extract.
#
# Exit codes (launcher-only, distinct from the Go binary's 0/2/3/64):
#   5 checksum mismatch
#   6 required tool missing
#   7 network unreachable
#   8 archive corrupt despite matching checksum

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
    MINGW*|MSYS*|CYGWIN*|Windows_NT) os="windows" ;; # Git Bash / MSYS2 / Cygwin
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
  # The archive tool depends on the platform's archive format: zip on Windows,
  # tar.gz elsewhere. ARCHIVE_EXT is set in main before this runs.
  if [ "${ARCHIVE_EXT:-tar.gz}" = "zip" ]; then
    command -v unzip >/dev/null 2>&1 || {
      err "unzip required (install with: pacman -S unzip in MSYS2, or use a shell that provides it)"
      exit 6
    }
  else
    command -v tar >/dev/null 2>&1 || {
      err "tar required (install with: apt-get install tar, dnf install tar, or brew install gnu-tar)"
      exit 6
    }
  fi
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
# Subshells scope EXIT/signal traps and descriptors to their resource owner.
# A nested download must never replace the lock owner's cleanup trap.
with_lock() (
  local lockdir="$1"; shift
  mkdir -p "$lockdir"
  if command -v flock >/dev/null 2>&1; then
    local fd
    exec {fd}>"$lockdir/.lock"
    flock "$fd"
  else
    local sentinel="$lockdir/.lock.d"
    local tries=0
    while ! mkdir "$sentinel" 2>/dev/null; do
      tries=$((tries + 1))
      [ "$tries" -gt 60 ] && { err "lock contention on $lockdir"; return 1; }
      sleep 1
    done
    trap 'rmdir "$sentinel" 2>/dev/null || true' EXIT
  fi
  trap 'exit 130' INT
  trap 'exit 143' TERM
  "$@"
)

# extract_archive pulls the binary out of a downloaded archive into cache. It
# tries the single-member fast path first, then the whole archive, honoring the
# platform archive format (zip on Windows, tar.gz elsewhere).
extract_archive() {
  local archive="$1" cache="$2" bin="$3"
  if [ "$ARCHIVE_EXT" = "zip" ]; then
    unzip -o -q "$archive" "$bin" -d "$cache" 2>/dev/null && return 0
    unzip -o -q "$archive" -d "$cache" 2>/dev/null && return 0
    return 1
  fi
  tar -xzf "$archive" -C "$cache" "$bin" 2>/dev/null && return 0
  tar -xzf "$archive" -C "$cache" 2>/dev/null && return 0
  return 1
}

download_and_verify() (
  local version="$1" platform="$2" cache="$3"
  # Another launcher may have installed while this process waited for the lock.
  [ -x "$cache/$BIN_NAME" ] && return 0
  local base="${CODEX_DISPATCH_RELEASE_URL:-https://github.com/semanta-dev/codex-dispatch/releases/download/v${version}}"
  local archive="codex-dispatch_${platform}.${ARCHIVE_EXT}"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"; rm -f "$cache/.${BIN_NAME}.install"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  if ! fetch_url "${base}/${archive}" "$tmp/$archive"; then
    err "cannot reach ${base} to download v${version} binary."
    err "For an unreleased checkout, build ./cmd/codex-dispatch and set CODEX_DISPATCH_BIN to its absolute path."
    err "For offline install, verify and extract the matching release, then place ${BIN_NAME} in ${cache}/manual/."
    exit 7
  fi
  if ! fetch_url "${base}/checksums.txt" "$tmp/checksums.txt"; then
    err "fetched archive but cannot reach checksums.txt"
    exit 7
  fi

  local expected actual
  expected="$(awk -v name="$archive" '$2 == name {print $1}' "$tmp/checksums.txt")"
  if [ -z "$expected" ]; then
    err "checksums.txt has no entry for ${archive}"; exit 5
  fi
  actual="$(sha256_of "$tmp/$archive")"
  if [ "$expected" != "$actual" ]; then
    err "checksum mismatch: expected $expected got $actual"; exit 5
  fi

  mkdir -p "$tmp/extracted"
  if ! extract_archive "$tmp/$archive" "$tmp/extracted" "$BIN_NAME"; then
    err "archive is corrupt"
    exit 8
  fi
  if [ ! -f "$tmp/extracted/$BIN_NAME" ] || [ -L "$tmp/extracted/$BIN_NAME" ]; then
    err "extracted archive missing $BIN_NAME binary"
    exit 8
  fi
  chmod +x "$tmp/extracted/$BIN_NAME"
  # Stage on the cache filesystem so publication is an atomic rename.
  cp "$tmp/extracted/$BIN_NAME" "$cache/.${BIN_NAME}.install"
  mv -f "$cache/.${BIN_NAME}.install" "$cache/$BIN_NAME"
)

# --- main -------------------------------------------------------------------

if [ -n "${CODEX_DISPATCH_BIN:-}" ]; then
  if [ ! -x "$CODEX_DISPATCH_BIN" ]; then
    err "CODEX_DISPATCH_BIN is not executable: $CODEX_DISPATCH_BIN"
    exit 6
  fi
  exec "${CODEX_DISPATCH_BIN}" dispatch "$@"
fi

# Detect the platform first so the archive format (zip on Windows, tar.gz
# elsewhere) and binary name (codex-dispatch.exe on Windows) are known before we
# check for the matching archive tool.
PLATFORM="$(detect_platform)"
PLATFORM_OS="${PLATFORM%%-*}"
if [ "$PLATFORM_OS" = "windows" ]; then
  BIN_NAME="codex-dispatch.exe"
  ARCHIVE_EXT="zip"
else
  BIN_NAME="codex-dispatch"
  ARCHIVE_EXT="tar.gz"
fi

CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/codex-dispatch/v${PINNED_VERSION}"
BIN="${CACHE_DIR}/${BIN_NAME}"

if [ ! -x "$BIN" ]; then
  if [ -x "${CACHE_DIR}/manual/${BIN_NAME}" ]; then
    cp "${CACHE_DIR}/manual/${BIN_NAME}" "$BIN"
    chmod +x "$BIN" 2>/dev/null || true
  else
    require_tools
    with_lock "$CACHE_DIR" download_and_verify "$PINNED_VERSION" "$PLATFORM" "$CACHE_DIR"
  fi
fi

exec "$BIN" dispatch "$@"
