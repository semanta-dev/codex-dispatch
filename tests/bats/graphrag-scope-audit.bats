#!/usr/bin/env bats

bats_require_minimum_version 1.5.0

setup() {
  REPO_ROOT="$(cd "${BATS_TEST_DIRNAME}/../.." && pwd)"
  SCRIPT="$REPO_ROOT/scripts/graphrag-scope-audit.sh"
  TMP_REPO="$(mktemp -d)"
  export GIT_CONFIG_GLOBAL=/dev/null
  cd "$TMP_REPO"
  git init -q -b main
  git config user.email t@t
  git config user.name t
  printf 'root\n' > README.md
  git add README.md
  git commit -q -m init
}

teardown() {
  rm -rf "$TMP_REPO"
}

@test "passes when tracked and untracked changes are in allowed files" {
  mkdir -p src docs/graphrag/progress
  printf 'print("ok")\n' > src/app.py
  printf 'done\n' > docs/graphrag/progress/001.done.md

  export GRAPHRAG_ALLOWED_FILES=$'src/app.py\ndocs/graphrag/progress/001.done.md'
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"clean"* ]]
  [[ "$output" == *"src/app.py"* ]]
  [[ "$output" == *"docs/graphrag/progress/001.done.md"* ]]
}

@test "fails when an untracked changed file is outside allowed files" {
  mkdir -p src
  printf 'print("ok")\n' > src/app.py
  printf 'secret\n' > src/extra.py

  export GRAPHRAG_ALLOWED_FILES=$'src/app.py'
  run "$SCRIPT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"violation"* ]]
  [[ "$output" == *"src/extra.py"* ]]
}

@test "ignores dispatch and Python runtime artifacts" {
  mkdir -p .codex-dispatch/runs/1 src/__pycache__
  printf '{}\n' > .codex-dispatch/runs/1/result.json
  printf 'bytecode\n' > src/__pycache__/app.cpython-313.pyc

  export GRAPHRAG_ALLOWED_FILES=$'src/app.py'
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"No non-ignored changed files"* ]]
}

@test "ignores common untracked dependency cache and temp outputs" {
  mkdir -p node_modules/pkg tmp temp .pytest_cache .next coverage
  printf 'x\n' > node_modules/pkg/index.js
  printf 'x\n' > tmp/file
  printf 'x\n' > temp/file
  printf 'x\n' > .pytest_cache/cache
  printf 'x\n' > .next/server.js
  printf 'x\n' > coverage/lcov.info

  export GRAPHRAG_ALLOWED_FILES=$'src/app.py'
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"No non-ignored changed files"* ]]
}

@test "does not silently ignore broad build directories unless gitignored" {
  mkdir -p dist build target bin obj
  printf 'x\n' > dist/app.js
  printf 'x\n' > build/app.o
  printf 'x\n' > target/app
  printf 'x\n' > bin/tool
  printf 'x\n' > obj/file.o

  export GRAPHRAG_ALLOWED_FILES=$'src/app.py'
  run "$SCRIPT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"dist/app.js"* ]]
  [[ "$output" == *"bin/tool"* ]]

  printf 'dist/\nbuild/\ntarget/\nbin/\nobj/\n' > .gitignore
  git add .gitignore
  git commit -q -m 'ignore build outputs'
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"No non-ignored changed files"* ]]
}

@test "does not ignore tracked changes just because path looks generated" {
  mkdir -p bin
  printf 'original\n' > bin/tool
  git add bin/tool
  git commit -q -m 'track bin tool'
  printf 'changed\n' > bin/tool

  export GRAPHRAG_ALLOWED_FILES=$'src/app.py'
  run "$SCRIPT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"bin/tool"* ]]
}

@test "supports extra untracked ignore globs from environment" {
  mkdir -p custom-cache src
  printf 'x\n' > custom-cache/output
  printf 'print("ok")\n' > src/app.py

  export GRAPHRAG_ALLOWED_FILES=$'src/app.py'
  run "$SCRIPT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"custom-cache/output"* ]]

  export GRAPHRAG_SCOPE_AUDIT_IGNORE=$'custom-cache/*'
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"src/app.py"* ]]
}

@test "accepts allowed paths from a file" {
  mkdir -p src
  printf 'print("ok")\n' > src/app.py
  printf 'src/app.py\n' > allowed.txt

  run "$SCRIPT" --allowed-file allowed.txt
  [ "$status" -eq 1 ]
  [[ "$output" == *"allowed.txt"* ]]

  printf 'src/app.py\nallowed.txt\n' > allowed.txt
  run "$SCRIPT" --allowed-file allowed.txt
  [ "$status" -eq 0 ]
}

@test "handles porcelain-quoted paths with spaces and backslashes" {
  mkdir -p "src/space dir"
  path='src/space dir/app\name.py'
  printf 'print("ok")\n' > "$path"

  export GRAPHRAG_ALLOWED_FILES="$path"
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"$path"* ]]
}

@test "fails when a rename deletes an out-of-scope source file" {
  mkdir -p src
  printf 'print("ok")\n' > src/old.py
  git add src/old.py
  git commit -q -m 'track old'
  git mv src/old.py src/new.py

  # Only the rename destination is allowed; the source deletion is not.
  export GRAPHRAG_ALLOWED_FILES=$'src/new.py'
  run "$SCRIPT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"violation"* ]]
  [[ "$output" == *"src/old.py"* ]]
}

@test "passes when both rename endpoints are in allowed files" {
  mkdir -p src
  printf 'print("ok")\n' > src/old.py
  git add src/old.py
  git commit -q -m 'track old'
  git mv src/old.py src/new.py

  export GRAPHRAG_ALLOWED_FILES=$'src/old.py\nsrc/new.py'
  run "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"clean"* ]]
  [[ "$output" == *"src/new.py"* ]]
  [[ "$output" == *"src/old.py"* ]]
}
