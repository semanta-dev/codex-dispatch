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
  baseline="$(git rev-parse HEAD)"
  tree="$(git rev-parse HEAD^{tree})"
  controller_home="$(getent passwd "$(id -u)" | cut -d: -f6)"
  mkdir -p "$controller_home/.codex-dispatch-authority"
  chmod 700 "$controller_home/.codex-dispatch-authority"
  authority="$controller_home/.codex-dispatch-authority/$(printf %s "$BATS_TEST_TMPDIR" | sha256sum | cut -c1-32)"
  mkdir -m 700 "$authority"
  git init --bare --quiet "$authority"
  git push --quiet "$authority" HEAD:refs/authority/baseline
  cp .git/index "$authority/baseline-index"
  printf '%s\n' "$baseline" > run/baseline-head.txt
  : > run/diff.patch
  python3 - "$authority" "$repo" "$baseline" "$tree" <<'PYFIXTURE'
import hashlib, json, pathlib, sys
store, repo = map(pathlib.Path, sys.argv[1:3])
head, tree = sys.argv[3:5]
record = dict(version=2, run_id=store.name, head=head, baseline_tree=tree,
              index_digest=hashlib.sha256((store / "baseline-index").read_bytes()).hexdigest(),
              index_present=True, candidate_hash="c" * 64, repository=str(repo),
              workdir=str(repo), export_dir=str(repo / "run"), terminal_state="BASELINE_CAPTURED")
(store / "index-objects.json").write_text(json.dumps(dict(version=1, objects=[]), separators=(",", ":")))
# Recompute after creating the manifest.
record["index_objects_digest"] = hashlib.sha256((store / "index-objects.json").read_bytes()).hexdigest()
(store / "authority.json").write_text(json.dumps(record))
(repo / "run/baseline-snapshot.json").write_text(json.dumps(dict(version=2, head=head, tree=tree,
    run_id=store.name, authority_dir=str(store), object_dir=str(store / "objects"))))
PYFIXTURE
  seal_patch

}

# Fixture controller seals the selected bytes before launching the verifier.
seal_patch() {
  python3 - "$authority" "$repo/run/diff.patch" <<'PYFIXTURE'
import hashlib, json, pathlib, sys
store, patch = map(pathlib.Path, sys.argv[1:])
record = json.loads((store / "authority.json").read_text())
record.update(terminal_state="DIFF_CAPTURED", patch_digest=hashlib.sha256(patch.read_bytes()).hexdigest())
(store / "capture.json").write_text(json.dumps(record))
(store / "diff.patch").write_bytes(patch.read_bytes())
PYFIXTURE
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
  seal_patch
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
  seal_patch
  run bash "$script" run bash -c '[ "$(wc -c < file.txt)" -eq 3 ]'
  [ "$status" -eq 0 ]
  run bash "$script" run bash -c 'exit 42'
  [ "$status" -eq 42 ]
}

@test "invalid patch preserves apply diagnostic and never verifies" {
  printf 'not a patch\n' > run/diff.patch
  seal_patch
  run bash "$script" run touch "$repo/verified"
  [ "$status" -eq 65 ]
  [[ "$output" == *"No valid patches"* ]]
  [ ! -e "$repo/verified" ]
}
