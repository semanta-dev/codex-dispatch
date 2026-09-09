#!/usr/bin/env python3
"""Collect one dispatch iteration's complete evidence for independent review."""
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import shutil
import sys

ROOT = Path(__file__).resolve().parent
LIMIT = 64000


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
    names = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=repo)
    result = {}
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
            data = path.read_bytes()
            kind = "file"
        else:
            raise ValueError("cannot review special file: " + name)
        result[name] = {"kind": kind, "mode": stat.S_IMODE(info.st_mode), "sha256": hashlib.sha256(data).hexdigest()}
        if kind == "symlink":
            result[name]["target"] = os.fsdecode(data)
    return result


def check(command, repo, run, label, clean=False):
    argv = [bash_executable(), (ROOT / "clean-verify.sh").as_posix(), run.as_posix(), bash_executable(), "-c", command] if clean else [bash_executable(), "-c", command]
    stdout, stderr = run / (label + ".stdout"), run / (label + ".stderr")
    with stdout.open("w") as out, stderr.open("w") as err:
        proc = subprocess.run(argv, cwd=repo, stdout=out, stderr=err)
    output = stdout.read_text(errors="replace")
    errors = stderr.read_text(errors="replace")
    return {"command": command, "exit_code": proc.returncode, "stdout": output[-8000:], "stderr": errors[-8000:],
            "output_truncated": len(output) > 8000 or len(errors) > 8000, "stdout_path": str(stdout), "stderr_path": str(stderr), "clean": clean}


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
        return bundle
    diff = (run / "diff.patch").read_text(errors="strict")
    if len(diff) > LIMIT:
        raise ValueError("diff exceeds review bundle limit; inspect full artifact before any acceptance: " + str(run / "diff.patch"))
    bundle["diff"] = diff
    repo = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip())
    before = state(repo, run)
    original_index = subprocess.check_output(["git", "ls-files", "--stage", "-z"], cwd=repo)
    test = os.environ.get("REVIEW_TEST_CMD", "") if os.environ.get("REVIEW_TEST_POLICY", "run") != "skip" else ""
    verify = os.environ.get("REVIEW_VERIFY_CMD", "")
    clean = os.environ.get("REVIEW_CLEAN_VERIFY", "false") == "true"
    mutations = set()
    if test:
        bundle["test"] = check(test, repo, run, "unit-test")
        after = state(repo, run)
        mutations.update(p for p in before.keys() | after.keys() if before.get(p) != after.get(p))
        if subprocess.check_output(["git", "ls-files", "--stage", "-z"], cwd=repo) != original_index:
            mutations.add(".git/index")
    if verify:
        if verify == test and not clean:
            bundle["verification"] = {**bundle["test"], "reused_test_execution": True}
        else:
            bundle["verification"] = check(verify, repo, run, "behavioral-verification", clean)
            after = state(repo, run)
            mutations.update(p for p in before.keys() | after.keys() if before.get(p) != after.get(p))
    if subprocess.check_output(["git", "ls-files", "--stage", "-z"], cwd=repo) != original_index:
        mutations.add(".git/index")
    bundle["verification_mutations"] = sorted(mutations)
    bundle["changed_file_facts"] = {p: before.get(p, {"kind": "deleted"}) for p in changed}
    bundle["complete"] = True
    (run / "review-evidence.json").write_text(json.dumps(bundle, indent=2) + "\n")
    return bundle


def main():
    try:
        print(json.dumps(collect()))
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(json.dumps({"complete": False, "error": str(error)}))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
