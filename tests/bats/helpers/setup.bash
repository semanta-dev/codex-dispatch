# tests/bats/helpers/setup.bash — shared setup for Bats suites running against
# the Go binary. Sources are expected to call `cddx_setup_binary` from setup().
#
# Sets:
#   CODEX_DISPATCH_BIN — abs path to the built ./dist/codex-dispatch
# Side effects:
#   PATH is *not* modified by this helper (the per-test setup may shim PATH
#   for fake codex/claude). The launchers honor CODEX_DISPATCH_BIN directly.

cddx_setup_binary() {
  local repo_root="$1"
  # Honor a caller-provided binary path (CI sets this from goreleaser snapshot
  # output; manual dev runs may set it from a local build).
  if [ -n "${CODEX_DISPATCH_BIN:-}" ] && [ -x "${CODEX_DISPATCH_BIN}" ]; then
    return 0
  fi
  local bin="$repo_root/dist/codex-dispatch"
  if [ ! -x "$bin" ]; then
    if ! ( cd "$repo_root" && go build -o "$bin" ./cmd/codex-dispatch ) >&2; then
      printf 'cddx_setup_binary: go build failed\n' >&2
      return 1
    fi
  fi
  export CODEX_DISPATCH_BIN="$bin"
}

# cddx_build_release_fixture <out_dir> <version> <platform> [content="ok"]
#
# Populates out_dir with:
#   codex-dispatch_<platform>.tar.gz   (containing a single executable named
#                                       codex-dispatch that echoes <content>)
#   checksums.txt                      (sha256 entry for the tarball)
# Returns the URL to use as CODEX_DISPATCH_RELEASE_URL (file://...).
cddx_build_release_fixture() {
  local out="$1" _version="$2" platform="$3" content="${4:-stub-ok}"
  mkdir -p "$out"
  local stage
  stage="$(mktemp -d)"
  cat > "$stage/codex-dispatch" <<EOF
#!/usr/bin/env bash
echo "${content}"
EOF
  chmod +x "$stage/codex-dispatch"
  ( cd "$stage" && tar -czf "${out}/codex-dispatch_${platform}.tar.gz" codex-dispatch )
  local sum
  if command -v sha256sum >/dev/null 2>&1; then
    sum="$(sha256sum "${out}/codex-dispatch_${platform}.tar.gz" | awk '{print $1}')"
  else
    sum="$(shasum -a 256 "${out}/codex-dispatch_${platform}.tar.gz" | awk '{print $1}')"
  fi
  printf '%s  codex-dispatch_%s.tar.gz\n' "$sum" "$platform" > "${out}/checksums.txt"
  rm -rf "$stage"
  printf 'file://%s\n' "$out"
}

cddx_detect_platform() {
  local os arch
  case "$(uname -s)" in
    Linux)  os="linux"  ;;
    Darwin) os="darwin" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
  esac
  printf '%s-%s\n' "$os" "$arch"
}

# cddx_build_fake_appserver <out_dir> <repo_root>
# Builds the fake-appserver into $out_dir/fake-appserver and symlinks it as
# $out_dir/codex so PATH-search resolves "codex" to the fake.
cddx_build_fake_appserver() {
  local out_dir="$1"
  local repo_root="$2"
  mkdir -p "$out_dir"
  if [ ! -x "$out_dir/fake-appserver" ]; then
    if ! ( cd "$repo_root/tests/fixtures/fake-appserver" && go build -o "$out_dir/fake-appserver" . ) >&2; then
      printf 'cddx_build_fake_appserver: go build failed\n' >&2
      return 1
    fi
  fi
  if [ ! -L "$out_dir/codex" ] && [ ! -x "$out_dir/codex" ]; then
    ln -s "$out_dir/fake-appserver" "$out_dir/codex"
  fi
}
