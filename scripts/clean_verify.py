#!/usr/bin/env python3
"""Read raw Git objects; never checkout host-controlled hooks, filters or config.

Fresh index entries retain ignored baseline files without invoking git add's
attribute conversions. Patch application occurs in a separate, controlled Git
repository. The actual verifier always uses execution_policy.verify.
"""
import importlib.util
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import time

spec = importlib.util.spec_from_file_location("execution_policy", Path(__file__).with_name("execution_policy.py"))
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)
LIMIT = 8 * 1024 * 1024
TREE_LIMIT = 512 * 1024 * 1024


class InvalidBaseline(ValueError):
    pass


def environment():
    env = policy.minimal_environment()
    env.update(GIT_CONFIG_SYSTEM=os.devnull, GIT_CONFIG_GLOBAL=os.devnull,
               GIT_NO_REPLACE_OBJECTS="1", GIT_OPTIONAL_LOCKS="0", GIT_NO_LAZY_FETCH="1", GIT_ALLOW_PROTOCOL="")
    return env


def git(repo, args, logs, deadline, output=None):
    result = policy.supervised(
        ["git", "--no-replace-objects", "-c", "core.fsmonitor=false", "-c", f"core.hooksPath={os.devnull}",
         "-c", f"core.attributesFile={os.devnull}", "-c", "protocol.allow=never", "-C", str(repo), *args],
        repo, environment(), output or logs / "git.stdout", logs / "git.stderr", max(.01, deadline - time.monotonic()))
    if result["exit_code"] != 0 or result.get("failure_kind"):
        raise ValueError(result["stderr"] or "Git materialization exceeded execution budget")
    policy.check_deadline(deadline)
    return (output or logs / "git.stdout").read_bytes()


def materialize(repo, baseline, destination, logs, deadline):
    # ls-tree and cat-file read objects directly. Unlike archive/checkout they
    # apply neither export-ignore/export-subst nor smudge/clean filters.
    tree = git(repo, ["ls-tree", "-r", "-z", baseline], logs, deadline)
    if len(tree) > LIMIT:
        raise ValueError("baseline inventory exceeds quota")
    entries = []
    for row in tree.split(b"\0"):
        if not row:
            continue
        header, raw_name = row.split(b"\t", 1)
        mode, kind, oid = header.split()
        name = os.fsdecode(raw_name)
        relative = Path(name)
        if relative.is_absolute() or any(part in {"..", ".git"} for part in relative.parts) or not relative.parts:
            raise ValueError("unsafe baseline path")
        if kind != b"blob" or mode not in (b"100644", b"100755", b"120000"):
            raise ValueError("unsupported baseline entry")
        entries.append((relative, mode, oid, raw_name))
    if len(entries) > 100000:
        raise ValueError("baseline file count exceeds quota")
    total = 0
    for relative, mode, oid, _ in entries:
        # Reject symlink ancestors before creating directories or opening files.
        for parent in relative.parents:
            if (destination / parent).is_symlink():
                raise ValueError("symlink ancestor in baseline")
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        blob = logs / "blob"
        data = git(repo, ["cat-file", "blob", oid.decode("ascii")], logs, deadline, blob)
        total += len(data)
        if len(data) > LIMIT or total > TREE_LIMIT:
            raise ValueError("baseline bytes exceed quota")
        if mode == b"120000":
            target.symlink_to(os.fsdecode(data))
        else:
            with target.open("xb") as stream:
                stream.write(data)
            target.chmod(0o755 if mode == b"100755" else 0o644)
    git(destination, ["init", "--quiet", "--template=", "--object-format=" + ("sha256" if len(baseline) == 64 else "sha1")], logs, deadline)
    # update-index consumes raw entries and invokes no content filters. Objects
    # need not be copied: verification snapshots use ls-files, not Git history.
    index_input = b"".join(mode + b" " + oid + b"\t" + name + b"\0" for _, mode, oid, name in entries)
    index_file = logs / "index-input"
    index_file.write_bytes(index_input)
    # Pass a fixed shell command and input file path via argv, never interpolate
    # repository-controlled filenames or source into executable shell text.
    result = policy.supervised(["/bin/bash", "--noprofile", "--norc", "-c",
                               'git -c core.fsmonitor=false -c core.hooksPath=/dev/null update-index -z --index-info < "$1"',
                               "clean-index", str(index_file)], destination, environment(), logs / "index.stdout", logs / "index.stderr",
                              max(.01, deadline-time.monotonic()))
    if result["exit_code"] != 0:
        raise ValueError(result["stderr"])


def read_regular(path, limit):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise ValueError("special or oversized materialization input")
        data = stream.read(limit + 1)
        if len(data) > limit:
            raise ValueError("materialization input grew beyond quota")
        return data


def run(run_dir, command, invocation):
    run_dir = Path(run_dir).resolve()
    patch = run_dir / "diff.patch"
    baseline_file = run_dir / "baseline-head.txt"
    if not patch.is_file() or not baseline_file.is_file():
        raise InvalidBaseline("diff.patch or baseline-head.txt missing")
    if sys.platform != "linux":
        print("clean-verify: native confined verification is unsupported on this platform", file=sys.stderr)
        return 126
    if baseline_file.stat().st_size > 65:
        raise InvalidBaseline("invalid baseline commit")
    try:
        baseline = read_regular(baseline_file, 65).decode("ascii").strip()
    except (ValueError, OSError) as error:
        raise InvalidBaseline("invalid baseline commit") from error
    if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", baseline):
        raise InvalidBaseline("invalid baseline commit")
    deadline = time.monotonic() + 120
    with tempfile.TemporaryDirectory(prefix="codex-clean-verify-") as temporary:
        temporary = Path(temporary)
        logs, checkout = temporary / "logs", temporary / "checkout"
        logs.mkdir()
        checkout.mkdir()
        try:
            repo = Path(os.fsdecode(git(invocation, ["rev-parse", "--show-toplevel"], logs, deadline)).strip()).resolve()
            relative = invocation.resolve().relative_to(repo)
            resolved = git(repo, ["rev-parse", "--verify", baseline + "^{commit}"], logs, deadline).decode().strip()
            if resolved != baseline:
                raise InvalidBaseline("baseline must identify a commit")
        except ValueError as error:
            raise InvalidBaseline(str(error)) from error
        materialize(repo, baseline, checkout, logs, deadline)
        if patch.stat().st_size > LIMIT:
            raise ValueError("patch exceeds quota")
        if patch.stat().st_size:
            # Copy once into the controlled directory to prevent path substitution
            # while git applies the patch; never permit --unsafe-paths.
            patch_copy = logs / "diff.patch"
            patch_copy.write_bytes(read_regular(patch, LIMIT))
            git(checkout, ["apply", "--whitespace=nowarn", str(patch_copy)], logs, deadline)
        result = policy.verify(shlex.join(command), checkout, checkout / relative,
                               logs / "verify.stdout", logs / "verify.stderr", max(.01, deadline-time.monotonic()))
        print(result["stdout"], end="")
        print(result["stderr"], end="", file=sys.stderr)
        if result.get("mutations"):
            print("clean-verify: verification changed reviewed source or staging: " + ", ".join(result["mutations"]), file=sys.stderr)
            return 66
        rc = result["exit_code"]
        return rc if rc >= 0 else 128 - rc


def main():
    if len(sys.argv) < 3:
        print("clean-verify: usage: clean-verify.sh <run_dir> <verify-cmd> [args...]", file=sys.stderr)
        return 2
    if not shutil.which("git"):
        print("clean-verify: git not found", file=sys.stderr)
        return 6
    try:
        return run(sys.argv[1], sys.argv[2:], Path.cwd())
    except InvalidBaseline as error:
        print("clean-verify: " + str(error), file=sys.stderr)
        return 6
    except (ValueError, OSError) as error:
        print("clean-verify: " + str(error), file=sys.stderr)
        return 65
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    sys.exit(main())
