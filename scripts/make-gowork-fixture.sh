#!/usr/bin/env bash
# make-gowork-fixture.sh — scaffold a go.work monorepo fixture used by the
# codex-dispatch cwd harness (tests/bats/dispatch-gowork-cwd.bats and
# tests/sdk/gowork-ultracode-e2e.sh).
#
# Layout produced under <dest> (a single git repo at the parent, go.work at the
# top, four sibling modules):
#
#   <dest>/
#     .git/                       (git init -b main, one commit)
#     .gitignore                  (ignores .codex-dispatch/)
#     go.work                     (use ./shared ./models ./server ./ui)
#     shared/   go.mod + shared.go   + shared_test.go   (target test)
#     models/   go.mod + models.go   + models_test.go
#     server/   go.mod + server.go   + server_test.go
#     ui/       go.mod + ui.go       + ui_test.go
#
# Each module is its own Go module (example.com/gowork/<name>) with a trivial
# package and a passing target test, so a dispatch scoped to one module has a
# real, module-local check to satisfy. This reproduces the user's preferred
# style: a parent dir with go.work and modules beneath it.
#
# Usage: make-gowork-fixture.sh <dest>
#   <dest> must not already exist (refuses to clobber).
set -euo pipefail

err() { printf 'make-gowork-fixture: %s\n' "$*" >&2; }

DEST="${1:-}"
if [ -z "$DEST" ]; then
  err "usage: make-gowork-fixture.sh <dest>"
  exit 64
fi
if [ -e "$DEST" ]; then
  err "destination already exists: $DEST"
  exit 1
fi

# go.work's `go` line must not exceed the local toolchain; pin to the repo's
# floor (1.22) which every supported toolchain satisfies.
GOWORK_GO_VERSION="1.22"

MODULES=(shared models server ui)

mkdir -p "$DEST"
cd "$DEST"

git init -q -b main
git config user.email "fixture@codex-dispatch.test"
git config user.name "gowork fixture"

printf '.codex-dispatch/\n' > .gitignore

# go.work tying the modules together.
{
  printf 'go %s\n\n' "$GOWORK_GO_VERSION"
  printf 'use (\n'
  for m in "${MODULES[@]}"; do
    printf '\t./%s\n' "$m"
  done
  printf ')\n'
} > go.work

for m in "${MODULES[@]}"; do
  mkdir -p "$m"
  printf 'module example.com/gowork/%s\n\ngo %s\n' "$m" "$GOWORK_GO_VERSION" > "$m/go.mod"
  # Package source: a single exported identity-ish function per module so a
  # dispatch has something real to edit, and the module name is observable.
  cat > "$m/$m.go" <<EOF
// Package $m is a fixture module in the go.work monorepo cwd harness.
package $m

// Name reports this module's name. A dispatch scoped to ./$m operates here.
func Name() string { return "$m" }
EOF
  # Target test: passes as-is so a correctly-scoped dispatch can run
  # \`go test ./...\` from the module and see green.
  cat > "$m/${m}_test.go" <<EOF
package $m

import "testing"

func TestName(t *testing.T) {
	if got := Name(); got != "$m" {
		t.Fatalf("Name() = %q, want %q", got, "$m")
	}
}
EOF
done

git add -A
git commit -q -m "init gowork monorepo fixture"

printf '%s\n' "$DEST"
