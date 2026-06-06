#!/usr/bin/env bats

load "helpers/setup.bash"

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  DISPATCH="$REPO_ROOT/scripts/dispatch-codex.sh"
  PLATFORM="$(cddx_detect_platform)"
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
  cd "$TEST_CWD"
  git init -q -b main
  git config user.email t@t
  git config user.name t
  echo x > README.md
  git add README.md
  git commit -q -m init

  export CODEX_TASK=x
  export CODEX_ACCEPTANCE=y
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
  [ -x "$CACHE_VER_DIR/codex-dispatch" ]
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
  [ ! -x "$CACHE_VER_DIR/codex-dispatch" ]
}

@test "offline-install slot is used when download URL is unreachable" {
  mkdir -p "$CACHE_VER_DIR/manual"
  cat > "$CACHE_VER_DIR/manual/codex-dispatch" <<'EOF'
#!/usr/bin/env bash
echo "from-manual-slot"
EOF
  chmod +x "$CACHE_VER_DIR/manual/codex-dispatch"
  # Point at a 404-ish file URL to prove we never hit it.
  export CODEX_DISPATCH_RELEASE_URL="file:///nonexistent/release"

  run "$DISPATCH"
  [ "$status" -eq 0 ]
  [[ "$output" == *"from-manual-slot"* ]]
  [ -x "$CACHE_VER_DIR/codex-dispatch" ]
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
  local status=0
  for pid in "${pids[@]}"; do
    if ! wait "$pid"; then status=$?; fi
  done
  [ "$status" -eq 0 ]
  [ -x "$CACHE_VER_DIR/codex-dispatch" ]
  out="$("$CACHE_VER_DIR/codex-dispatch")"
  [[ "$out" == *"concurrent-ok"* ]]
}

@test "missing tar exits 6 with a clear message" {
  # Build a PATH that lacks tar but has every other tool we need.
  STUB_PATH="$(mktemp -d)"
  for tool in bash sh awk grep sed mktemp cat chmod printf find git curl wget sha256sum shasum flock dirname uname; do
    if command -v "$tool" >/dev/null 2>&1; then
      ln -s "$(command -v "$tool")" "$STUB_PATH/$tool"
    fi
  done
  PATH="$STUB_PATH" run "$DISPATCH"
  [ "$status" -eq 6 ]
  [[ "$output" == *"tar"* ]] || [[ "$output" == *"required"* ]]
}
