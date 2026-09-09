#!/usr/bin/env bats

setup() {
  script="$BATS_TEST_DIRNAME/../../scripts/clean-verify.sh"
  repo="$BATS_TEST_TMPDIR/repo"
  mkdir -p "$repo"
  cd "$repo"
  git init -q
  git config user.email test@example.com
  git config user.name test
  printf 'base\n' > file.txt
  git add file.txt
  git commit -qm base
  mkdir run
  git rev-parse HEAD > run/baseline-head.txt
  : > run/diff.patch
}

@test "missing patch fails before verification" {
  rm run/diff.patch
  run bash "$script" run touch "$repo/verified"
  [ "$status" -eq 6 ]
  [ ! -e "$repo/verified" ]
}

@test "relative and absolute run paths apply the same patch" {
  printf 'changed\n' > file.txt
  git diff --binary > run/diff.patch
  for path in run "$repo/run"; do
    run bash "$script" "$path" bash -c '[ "$(cat file.txt)" = changed ]'
    [ "$status" -eq 0 ]
  done
}

@test "verification uses recorded commit after HEAD moves" {
  printf 'later\n' > file.txt
  git add file.txt
  git commit -qm later
  run bash "$script" run bash -c '[ "$(cat file.txt)" = base ]'
  [ "$status" -eq 0 ]
}

@test "missing malformed and unknown baseline fail" {
  rm run/baseline-head.txt
  run bash "$script" run true
  [ "$status" -eq 6 ]
  printf 'HEAD\n' > run/baseline-head.txt
  run bash "$script" run true
  [ "$status" -eq 6 ]
  printf '%040d\n' 0 > run/baseline-head.txt
  run bash "$script" run true
  [ "$status" -eq 6 ]
}

@test "binary patch replays and verification exit is preserved" {
  printf '\0\1\2' > file.txt
  git diff --binary > run/diff.patch
  run bash "$script" run bash -c '[ "$(wc -c < file.txt)" -eq 3 ]'
  [ "$status" -eq 0 ]
  run bash "$script" run bash -c 'exit 42'
  [ "$status" -eq 42 ]
}

@test "invalid patch preserves apply diagnostic and never verifies" {
  printf 'not a patch\n' > run/diff.patch
  run bash "$script" run touch "$repo/verified"
  [ "$status" -eq 65 ]
  [[ "$output" == *"No valid patches"* ]]
  [ ! -e "$repo/verified" ]
}
