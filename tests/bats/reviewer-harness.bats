#!/usr/bin/env bats
setup() {
  script="$BATS_TEST_DIRNAME/../reviewer/run-fixtures.sh"
  mkdir -p "$BATS_TEST_TMPDIR/bin"
  export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
  export REVIEWER_FIXTURE_RUNS=1
  unset REVIEWER_FIXTURES_SKIP
  cat > "$BATS_TEST_TMPDIR/bin/claude" <<'MOCK'
#!/usr/bin/env bash
printf 'VERDICT: %s\nREASON:\n' "${MOCK_VERDICT:-pass}"
exit "${MOCK_RC:-0}"
MOCK
  chmod +x "$BATS_TEST_TMPDIR/bin/claude"
}
@test "correct verdict passes and incorrect verdict fails" {
  run bash "$script" --fixture pass-simple
  [ "$status" -eq 0 ]
  export MOCK_VERDICT=fail
  run bash "$script" --fixture pass-simple
  [ "$status" -eq 1 ]
}
@test "failed invocation cannot pass using its output" {
  export MOCK_RC=1
  run bash "$script" --fixture pass-simple
  [ "$status" -eq 1 ]
}
@test "invalid counts and empty fixture selection fail" {
  for count in 0 invalid -1; do
    export REVIEWER_FIXTURE_RUNS="$count"
    run bash "$script" --fixture pass-simple
    [ "$status" -eq 64 ]
  done
  export REVIEWER_FIXTURE_RUNS=1
  run bash "$script" --fixture does-not-exist
  [ "$status" -eq 64 ]
}
@test "explicit skip is distinct from success" {
  export REVIEWER_FIXTURES_SKIP=1
  run bash "$script"
  [ "$status" -eq 77 ]
}

@test "per-fixture threshold accepts eight of ten and rejects seven" {
  cat > "$BATS_TEST_TMPDIR/bin/claude" <<'MOCK'
#!/usr/bin/env bash
count=0
[ ! -f "$MOCK_COUNT" ] || read -r count < "$MOCK_COUNT"
count=$((count + 1))
printf '%s\n' "$count" > "$MOCK_COUNT"
if [ "$count" -le "$MOCK_PASSES" ]; then
  printf 'VERDICT: pass\nREASON:\n'
else
  printf 'VERDICT: fail\nREASON: wrong\n'
fi
MOCK
  export REVIEWER_FIXTURE_RUNS=10 MOCK_COUNT="$BATS_TEST_TMPDIR/count" MOCK_PASSES=8
  run bash "$script" --fixture pass-simple
  [ "$status" -eq 0 ]
  rm "$MOCK_COUNT"
  export MOCK_PASSES=7
  run bash "$script" --fixture pass-simple
  [ "$status" -eq 1 ]
}
