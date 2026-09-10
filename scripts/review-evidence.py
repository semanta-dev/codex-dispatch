#!/usr/bin/env python3
"""Collect one dispatch iteration's complete evidence for independent review."""
import importlib.util
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import shutil
import sys
import time

ROOT = Path(__file__).resolve().parent
LIMIT = 64000
_POLICY_SPEC = importlib.util.spec_from_file_location("execution_policy", ROOT / "execution_policy.py")
POLICY = importlib.util.module_from_spec(_POLICY_SPEC)
_POLICY_SPEC.loader.exec_module(POLICY)


def detect_test(repo):
    pyproject = repo / "pyproject.toml"
    if (repo / "pytest.ini").exists() or (pyproject.exists() and "[tool.pytest" in pyproject.read_text()):
        return "pytest"
    package = repo / "package.json"
    if package.exists() and json.loads(package.read_text()).get("scripts", {}).get("test"):
        return "npm test"
    for name, command in [("Cargo.toml", "cargo test"), ("go.mod", "go test ./...")]:
        if (repo / name).exists():
            return command
    make = repo / "Makefile"
    if make.exists() and any(line.startswith("test:") for line in make.read_text().splitlines()):
        return "make test"
    return ""


def bash_executable() -> str:
    if os.name != "nt":
        return "bash"
    # Windows CreateProcess searches System32 before PATH for bare bash,
    # selecting the WSL launcher even when running inside Git Bash.
    cygpath = shutil.which("cygpath")
    if not cygpath:
        raise OSError("Git Bash is required for native Windows shell execution")
    shell = Path(cygpath).with_name("bash.exe")
    if not shell.is_file():
        raise OSError("Git Bash executable not found beside cygpath")
    return str(shell)


def state(repo, run):
    names = subprocess.check_output(POLICY.GIT + ["ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=repo, env=POLICY.minimal_environment())
    result = {}
    deadline = time.monotonic() + 30
    total = 0
    for raw in names.split(b"\0"):
        if not raw:
            continue
        name = os.fsdecode(raw)
        path = repo / name
        if name.startswith(".codex-dispatch/") or path == run or run in path.parents:
            continue
        try:
            info = path.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(info.st_mode):
            data = os.fsencode(os.readlink(path))
            kind = "symlink"
        elif stat.S_ISREG(info.st_mode):
            digest = hashlib.sha256()
            with POLICY.parent_handle(repo, Path(name)) as handle:
                for chunk in POLICY.regular_bytes(handle, Path(name).name, deadline):
                    total += len(chunk)
                    if total > POLICY.MAX_TREE_BYTES:
                        raise ValueError("reviewed tree exceeds evidence quota")
                    digest.update(chunk)
            data = None
            kind = "file"
        else:
            raise ValueError("cannot review special file: " + name)
        result[name] = {"kind": kind, "mode": stat.S_IMODE(info.st_mode), "sha256": digest.hexdigest() if data is None else hashlib.sha256(data).hexdigest()}
        if kind == "symlink":
            result[name]["target"] = os.fsdecode(data)
    return result


def check(command, repo, run, label, clean=False):
    stdout, stderr = run / (label + ".stdout"), run / (label + ".stderr")
    timeout = min(120, int(os.environ.get("CODEX_DISPATCH_TIMEOUT_MS", "120000")) / 1000)
    if not clean:
        root = Path(subprocess.check_output(POLICY.GIT + ["rev-parse", "--show-toplevel"], cwd=repo, text=True, env=POLICY.minimal_environment()).strip())
        return {**POLICY.verify(command, root, repo, stdout, stderr, timeout), "clean": False}
    argv = [bash_executable(), (ROOT / "clean-verify.sh").as_posix(), run.as_posix(), bash_executable(), "-c", command]
    return {**POLICY.supervised(argv, repo, dict(os.environ), stdout, stderr, timeout), "command": command, "clean": True}


def live_fingerprint(repo, run):
    index = subprocess.check_output(POLICY.GIT + ["ls-files", "--stage", "-z"], cwd=repo, env=POLICY.minimal_environment())
    data = {"files": state(repo, run), "index_sha256": hashlib.sha256(index).hexdigest()}
    return {"version": 1, "sha256": hashlib.sha256(json.dumps(data, sort_keys=True).encode()).hexdigest()}


def collect():
    proc = subprocess.run([bash_executable(), (ROOT / "dispatch-codex.sh").as_posix()], text=True, capture_output=True)
    if proc.returncode:
        raise ValueError(f"dispatch launcher failed ({proc.returncode}): {proc.stderr[-8000:]}")
    lines = proc.stdout.splitlines()
    if not lines:
        raise ValueError("dispatch did not return a run directory")
    raw_path = lines[-1]
    if os.name == "nt" and raw_path.startswith("/"):
        raw_path = subprocess.check_output(["cygpath", "-w", raw_path], text=True).strip()
    run = Path(raw_path).resolve()
    result = json.loads((run / "result.json").read_text())
    if not isinstance(result, dict) or type(result.get("exit_code")) is not int:
        raise ValueError("missing or invalid dispatch exit_code")
    changed = result.get("files_changed")
    if not isinstance(changed, list) or any(not isinstance(p, str) for p in changed):
        raise ValueError("invalid files_changed evidence")
    for key in ["lines_added", "lines_removed"]:
        if type(result.get(key)) is not int or result[key] < 0:
            raise ValueError("invalid " + key + " evidence")
    bundle = {"complete": True, "run_dir": str(run), "result": result, "test": None, "verification": None}
    if result["exit_code"] != 0:
        bundle["codex_stdout_tail"] = (run / "stdout.log").read_text(errors="replace")[-8000:]
        (run / "review-evidence.json").write_text(json.dumps(bundle, indent=2) + "\n")
        return bundle
    diff = (run / "diff.patch").read_text(errors="strict")
    if len(diff) > LIMIT:
        raise ValueError("diff exceeds review bundle limit; inspect full artifact before any acceptance: " + str(run / "diff.patch"))
    bundle["diff"] = diff
    repo = Path(subprocess.check_output(POLICY.GIT + ["rev-parse", "--show-toplevel"], text=True, env=POLICY.minimal_environment()).strip())
    effective = Path((run / "effective-workdir.txt").read_text().strip()).resolve()
    if not effective.is_dir() or not effective.is_relative_to(repo.resolve()):
        raise ValueError("effective verification directory escapes repository or is missing")
    bundle["effective_workdir"] = str(effective)
    captured_fingerprint = live_fingerprint(repo, run)
    before = state(repo, run)
    original_index = subprocess.check_output(POLICY.GIT + ["ls-files", "--stage", "-z"], cwd=repo, env=POLICY.minimal_environment())
    test = os.environ.get("REVIEW_TEST_CMD", "") if os.environ.get("REVIEW_TEST_POLICY", "run") != "skip" else ""
    if test == "__auto__":
        test = detect_test(effective)
    bundle["test_command"] = test
    verify = os.environ.get("REVIEW_VERIFY_CMD", "")
    clean = os.environ.get("REVIEW_CLEAN_VERIFY", "false") == "true"
    mutations = set()
    if test:
        bundle["test"] = check(test, effective, run, "unit-test")
        mutations.update(bundle["test"].get("mutations", []))
        after = state(repo, run)
        mutations.update(p for p in before.keys() | after.keys() if before.get(p) != after.get(p))
        if subprocess.check_output(POLICY.GIT + ["ls-files", "--stage", "-z"], cwd=repo, env=POLICY.minimal_environment()) != original_index:
            mutations.add(".git/index")
    if verify:
        if verify == test and not clean:
            bundle["verification"] = {**bundle["test"], "reused_test_execution": True}
        else:
            bundle["verification"] = check(verify, effective, run, "behavioral-verification", clean)
            mutations.update(bundle["verification"].get("mutations", []))
            after = state(repo, run)
            mutations.update(p for p in before.keys() | after.keys() if before.get(p) != after.get(p))
    if subprocess.check_output(POLICY.GIT + ["ls-files", "--stage", "-z"], cwd=repo, env=POLICY.minimal_environment()) != original_index:
        mutations.add(".git/index")
    bundle["verification_mutations"] = sorted(mutations)
    bundle["changed_file_facts"] = {p: before.get(p, {"kind": "deleted"}) for p in changed}
    bundle["live_fingerprint"] = captured_fingerprint
    if live_fingerprint(repo, run) != captured_fingerprint:
        bundle["verification_mutations"] = sorted(set(bundle["verification_mutations"]) | {"live-tree-drift"})
    bundle["complete"] = True
    (run / "review-evidence.json").write_text(json.dumps(bundle, indent=2) + "\n")
    return bundle


def collect_with_receipt():
    raw = os.environ.get("REVIEW_RECEIPT_PATH")
    if not raw:
        return collect()  # Explicit legacy agent routes use the same collector.
    receipt = Path(raw)
    lock = receipt.with_suffix(".lock")
    lock.mkdir()  # Concurrent/abandoned invocations fail closed.
    try:
        saved = json.loads(receipt.read_text())
        identity = json.loads(os.environ["REVIEW_INVOCATION_ID"])
        if saved["identity"] != identity or saved["status"] not in {"running", "complete"}:
            raise ValueError("review invocation identity/status changed")
        if any(os.environ.get(key, "") != value for key, value in saved["dispatch_env"].items()) or os.environ.get("CODEX_RESULT_DIR"):
            raise ValueError("review request parameters changed")
        ledger = receipt.with_suffix(".runs.json")
        data = json.loads(ledger.read_text()) if ledger.exists() else {"identity": identity, "attempts": []}
        attempts = data["attempts"]
        if data["identity"] != identity or any(row["state"] != "complete" for row in attempts):
            raise ValueError("review chain is incomplete or belongs to another invocation")
        if len(attempts) >= saved["config"]["max_iter"]:
            raise ValueError("review iteration budget exhausted")
        session = os.environ.get("CODEX_SESSION_ID", "")
        permitted = {""} if not attempts or saved["config"]["no_resume"] else {"", attempts[-1]["session_id"]}
        if session not in permitted:
            raise ValueError("review resumed a session outside the current chain")
        def persist():
            temp = ledger.with_suffix(".tmp")
            temp.write_text(json.dumps(data, indent=2) + "\n")
            os.replace(temp, ledger)
        row = {"iteration": len(attempts) + 1, "state": "running", "resume_session": session,
               'feedback': os.environ.get('CODEX_FEEDBACK', '')}
        attempts.append(row)
        persist()  # Count the attempt before any dispatch or model work.
        try:
            bundle = collect()
            row.update(state="complete", run_dir=bundle["run_dir"], session_id=bundle["result"]["session_id"])
            persist()
            return bundle
        except BaseException as error:
            row.update(state="failed", error=str(error))
            persist()
            raise
    finally:
        lock.rmdir()


def main():
    try:
        print(json.dumps(collect_with_receipt()))
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print(json.dumps({"complete": False, "error": str(error)}))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
