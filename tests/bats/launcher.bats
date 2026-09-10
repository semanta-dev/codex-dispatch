#!/usr/bin/env bats
# Bats intentionally isolates each test in a subshell.
# shellcheck disable=SC2030,SC2031

load "helpers/setup.bash"

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  DISPATCH="$REPO_ROOT/scripts/dispatch-codex.sh"
  PLATFORM="$(cddx_detect_platform)"
  BIN_NAME="codex-dispatch"
  case "$PLATFORM" in windows-*) BIN_NAME="codex-dispatch.exe" ;; esac
  VERSION="0.99.0-test"
  RELEASE_DIR="$(mktemp -d)"
  CACHE_HOME="$(mktemp -d)"
  CACHE_VER_DIR="$CACHE_HOME/codex-dispatch/v${VERSION}"

  # Build a synthetic repo layout pointing the launcher at a controlled VERSION.
  TMP_REPO="$(mktemp -d)"
  mkdir -p "$TMP_REPO/scripts"
  cp "$DISPATCH" "$TMP_REPO/scripts/dispatch-codex.sh"
  printf '%s\n' "$VERSION" > "$TMP_REPO/VERSION"
  DISPATCH="$TMP_REPO/scripts/dispatch-codex.sh"

  export XDG_CACHE_HOME="$CACHE_HOME"
  unset CODEX_DISPATCH_BIN

  # The dispatch subcommand needs to run inside a git repo with CODEX_TASK
  # and CODEX_ACCEPTANCE set; the test binary in our fixture echoes a fixed
  # string and ignores those, but the launcher still passes them through.
  TEST_CWD="$(mktemp -d)"
  cd "$TEST_CWD" || return
  git init -q -b main
  git config user.email t@t
  git config user.name t
  echo x > README.md
  git add README.md
  git commit -q -m init

  export CODEX_TASK=x
  export CODEX_ACCEPTANCE=y
  export TMPDIR="$TMP_REPO/tmp"
  mkdir -p "$TMPDIR"
}

teardown() {
  cd /
  rm -rf "$TMP_REPO" "$RELEASE_DIR" "$CACHE_HOME" "$TEST_CWD"
}

@test "happy path: downloads from file:// URL, verifies checksum, dispatches" {
  RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" "$PLATFORM" "stub-ok")"
  export CODEX_DISPATCH_RELEASE_URL="$RELEASE_URL"

  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [[ "$output" == *"stub-ok"* ]]
  [ -x "$CACHE_VER_DIR/$BIN_NAME" ]
}

@test "tampered checksum exits 5 and does not extract" {
  RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" "$PLATFORM")"
  # Corrupt the checksum after building.
  sed -i.bak 's/^[a-f0-9]*/0000000000000000000000000000000000000000000000000000000000000000/' "$RELEASE_DIR/checksums.txt"
  rm -f "$RELEASE_DIR/checksums.txt.bak"
  export CODEX_DISPATCH_RELEASE_URL="$RELEASE_URL"

  run "$DISPATCH"
  [ "$status" -eq 5 ]
  [[ "$output" == *"checksum"* ]]
  [ ! -x "$CACHE_VER_DIR/$BIN_NAME" ]
}

@test "offline-install slot is used when download URL is unreachable" {
  mkdir -p "$CACHE_VER_DIR/manual"
  cat > "$CACHE_VER_DIR/manual/$BIN_NAME" <<'EOF'
#!/usr/bin/env bash
echo "from-manual-slot"
EOF
  chmod +x "$CACHE_VER_DIR/manual/$BIN_NAME"
  # Point at a 404-ish file URL to prove we never hit it.
  export CODEX_DISPATCH_RELEASE_URL="file:///nonexistent/release"

  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [[ "$output" == *"from-manual-slot"* ]]
  [ -x "$CACHE_VER_DIR/$BIN_NAME" ]
}

@test "subsequent invocations use the cached binary (no second download)" {
  RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" "$PLATFORM" "v1")"
  export CODEX_DISPATCH_RELEASE_URL="$RELEASE_URL"

  run "$DISPATCH"
  [ "$status" -eq 0 ]
  # Move the release away — second call must succeed using the cache.
  rm -rf "$RELEASE_DIR"
  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [[ "$output" == *"v1"* ]]
}

@test "concurrent invocations don't race the download" {
  RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" "$PLATFORM" "concurrent-ok")"
  export CODEX_DISPATCH_RELEASE_URL="$RELEASE_URL"

  # Fire 4 in parallel; collect exit codes.
  local pids=()
  for _ in 1 2 3 4; do
    "$DISPATCH" >/dev/null 2>&1 &
    pids+=($!)
  done
  wait_for_children "${pids[@]}"
  [ -x "$CACHE_VER_DIR/$BIN_NAME" ]
  out="$("$CACHE_VER_DIR/$BIN_NAME")"
  [[ "$out" == *"concurrent-ok"* ]]
}

@test "missing platform archive extractor exits 6 with a clear message" {
  # Build a PATH that lacks tar but has every other tool we need.
  STUB_PATH="$(mktemp -d)"
  for tool in bash sh awk grep sed mktemp cat chmod printf find git curl wget sha256sum shasum flock dirname uname; do
    if command -v "$tool" >/dev/null 2>&1; then
      ln -s "$(command -v "$tool")" "$STUB_PATH/$tool"
    fi
  done
  PATH="$STUB_PATH" run "$DISPATCH"
  [ "$status" -eq 6 ]
  [[ "$output" == *"required"* ]]
}

@test "windows platform downloads .zip, extracts codex-dispatch.exe, dispatches" {
  command -v zip >/dev/null 2>&1 && command -v unzip >/dev/null 2>&1 || skip "zip/unzip not available"

  # Shim uname so the launcher detects Windows (Git Bash / MSYS2) on this host.
  local shim; shim="$(mktemp -d)"
  cat > "$shim/uname" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  -s) echo "MINGW64_NT-10.0" ;;
  -m) echo "x86_64" ;;
  *)  exec /usr/bin/uname "$@" ;;
esac
EOF
  chmod +x "$shim/uname"

  # Exercise the same fixture builder used on native Windows runners.
  RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" windows-amd64 "win-stub-ok")"
  export CODEX_DISPATCH_RELEASE_URL="$RELEASE_URL"

  PATH="$shim:$PATH" run "$DISPATCH"
  [ "$status" -eq 0 ]
  [[ "$output" == *"win-stub-ok"* ]]
  [ -x "$CACHE_VER_DIR/codex-dispatch.exe" ]
  rm -rf "$shim"
}

# Retain the original wait status and drain all children, even after a failure.
wait_for_children() {
  local child rc=0 child_rc
  for child in "$@"; do
    if wait "$child"; then
      :
    else
      child_rc=$?
      printf 'launcher child %s failed: %s\n' "$child" "$child_rc" >&2
      rc=$child_rc
    fi
  done
  return "$rc"
}

without_flock() {
  NO_FLOCK_PATH="$TMP_REPO/no-flock"
  mkdir -p "$NO_FLOCK_PATH"
  local tool
  for tool in bash sh awk grep mktemp cat chmod dirname uname mkdir rmdir sleep rm cp mv tar gzip unzip curl sha256sum shasum; do
    if command -v "$tool" >/dev/null 2>&1; then
      ln -s "$(command -v "$tool")" "$NO_FLOCK_PATH/$tool"
    fi
  done
}

@test "concurrent wait gate fails on injected child failure and drains peers" {
  bash -c 'exit 17' &
  local failed=$!
  bash -c 'sleep 0.1; echo drained > "$1"' _ "$TMP_REPO/drained" &
  local peer=$! rc=0
  wait_for_children "$failed" "$peer" || rc=$?
  [ "$rc" -eq 17 ]
  [ -f "$TMP_REPO/drained" ]
}

@test "without flock: valid cold-cache install cleans download and lock" {
  CODEX_DISPATCH_RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" "$PLATFORM")"
  export CODEX_DISPATCH_RELEASE_URL
  without_flock
  PATH="$NO_FLOCK_PATH" run "$DISPATCH"
  [ "$status" -eq 0 ]
  [[ "$output" == *stub-ok* ]]
  [ ! -d "$CACHE_VER_DIR/.lock.d" ]
  [ -z "$(ls -A "$TMPDIR")" ]
}

@test "without flock: failed download cleans resources and permits retry" {
  without_flock
  export CODEX_DISPATCH_RELEASE_URL="file:///nonexistent/release"
  PATH="$NO_FLOCK_PATH" run "$DISPATCH"
  [ "$status" -eq 7 ]
  [ ! -d "$CACHE_VER_DIR/.lock.d" ]
  [ -z "$(ls -A "$TMPDIR")" ]
  CODEX_DISPATCH_RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" "$PLATFORM")"
  PATH="$NO_FLOCK_PATH" run "$DISPATCH"
  [ "$status" -eq 0 ]
}

@test "without flock: concurrent cold-cache installs all succeed" {
  CODEX_DISPATCH_RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" "$PLATFORM" "concurrent-ok")"
  export CODEX_DISPATCH_RELEASE_URL
  without_flock
  local pids=() i rc=0
  for i in 1 2 3 4; do
    PATH="$NO_FLOCK_PATH" "$DISPATCH" >"$TMP_REPO/child-$i.log" 2>&1 &
    pids+=("$!")
  done
  wait_for_children "${pids[@]}" || rc=$?
  if [ "$rc" -ne 0 ]; then cat "$TMP_REPO"/child-*.log; fi
  [ "$rc" -eq 0 ]
  for i in 1 2 3 4; do
    [[ "$(cat "$TMP_REPO/child-$i.log")" == *concurrent-ok* ]]
  done
  [ ! -d "$CACHE_VER_DIR/.lock.d" ]
  [ -z "$(ls -A "$TMPDIR")" ]
}

@test "missing checksum entry rejects with exit 5 and cleans temporary state" {
  CODEX_DISPATCH_RELEASE_URL="$(cddx_build_release_fixture "$RELEASE_DIR" "$VERSION" "$PLATFORM")"
  printf 'unrelated checksum\n' > "$RELEASE_DIR/checksums.txt"
  export CODEX_DISPATCH_RELEASE_URL
  without_flock
  PATH="$NO_FLOCK_PATH" run "$DISPATCH"
  [ "$status" -eq 5 ]
  [ ! -e "$CACHE_VER_DIR/$BIN_NAME" ]
  [ ! -d "$CACHE_VER_DIR/.lock.d" ]
  [ -z "$(ls -A "$TMPDIR")" ]
}

@test "invalid explicit source binary fails without downloading" {
  export CODEX_DISPATCH_BIN="$TMP_REPO/missing"
  run "$DISPATCH"
  [ "$status" -eq 6 ]
  [[ "$output" == *CODEX_DISPATCH_BIN* ]]
  [ ! -e "$CACHE_VER_DIR" ]
}

@test "without flock: interrupted process group cleans lock and download" {
  command -v python3 >/dev/null 2>&1 || skip "python3 unavailable"
  case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) skip "POSIX process-group signal test" ;; esac
  without_flock
  rm "$NO_FLOCK_PATH/curl"
  cat > "$NO_FLOCK_PATH/curl" <<'EOF'
#!/usr/bin/env bash
touch "$LAUNCHER_READY"
sleep 30
EOF
  ln -s "$(command -v touch)" "$NO_FLOCK_PATH/touch"
  # Make nested cleanup ordering deterministic: the download owner can finish
  # before the lock owner. Test cleanup must not SIGKILL that pending trap.
  export LAUNCHER_REAL_RMDIR="$(command -v rmdir)"
  rm "$NO_FLOCK_PATH/rmdir"
  cat > "$NO_FLOCK_PATH/rmdir" <<'EOF'
#!/usr/bin/env bash
sleep .2
exec "$LAUNCHER_REAL_RMDIR" "$@"
EOF
  chmod +x "$NO_FLOCK_PATH/rmdir"
  chmod +x "$NO_FLOCK_PATH/curl"
  export LAUNCHER_READY="$TMP_REPO/ready"
  export CODEX_DISPATCH_RELEASE_URL="file:///unused"
  run python3 - "$DISPATCH" "$NO_FLOCK_PATH" "$CACHE_VER_DIR/.lock.d" <<'PYCODE'
import os, signal, subprocess, sys, time
p = subprocess.Popen([sys.argv[1]], env={**os.environ, "PATH": sys.argv[2]}, start_new_session=True,
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
try:
    deadline = time.monotonic() + 5
    while not os.path.exists(os.environ["LAUNCHER_READY"]):
        assert p.poll() is None, "launcher exited before download"
        assert time.monotonic() < deadline, "download never started"
        time.sleep(.02)
    os.killpg(p.pid, signal.SIGTERM)
    assert p.wait(timeout=5) != 0
    # The parent shell may exit before its resource-owning children finish traps.
    deadline = time.monotonic() + 5
    while os.listdir(os.environ["TMPDIR"]) or os.path.exists(sys.argv[3]):
        assert time.monotonic() < deadline, "download directory or lock leaked"
        time.sleep(.02)
finally:
    try: os.killpg(p.pid, signal.SIGKILL)
    except ProcessLookupError: pass
    p.wait()
PYCODE
  [ "$status" -eq 0 ]
  [ ! -d "$CACHE_VER_DIR/.lock.d" ]
  [ -z "$(ls -A "$TMPDIR")" ]
}
