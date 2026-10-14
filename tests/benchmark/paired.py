#!/usr/bin/env python3
"""Frozen paired corpus; raw evidence is retained even on execution failure.

freeze OUT creates all 60 inputs without calling a model. run OUT consumes them
once, in manifest order. oracle CASE is the independent behavior check exposed
to both arms. A smoke directory is separate from promotion measurements.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import stat
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
os.environ["GIT_CONFIG_GLOBAL"] = os.devnull
os.environ["GIT_CONFIG_NOSYSTEM"] = "1"
MODEL = "gpt-5.5"
ROUTER = "claude-haiku-4-5-20251001"
REVIEW_ROUTE = "api-hook-command"
CASES = {
    "C01": ("Create hello.txt containing exactly hello followed by a newline.", ["hello.txt"]),
    "C02": ("Append task followed by a newline to notes.txt. Preserve every existing byte.", ["notes.txt"]),
    "C03": ("Restore notes.txt to its committed HEAD contents, removing the operator WIP line.", ["notes.txt"]),
    "C04": ("Delete the pre-existing untracked scratch.txt file.", ["scratch.txt"]),
    "C05": ("Create data.bin containing bytes 00 01 7f ff; create link pointing to data.bin; create executable run.sh with exactly #!/bin/sh newline printf 'ok\\n' newline.", ["data.bin", "link", "run.sh"]),
    "C06": ("Change the Message constant in service/message.go from old to new. Work in the service Go module.", ["service/message.go"]),
}
EXPECTED = {
    "C01": {"hello.txt": b"hello\n"},
    "C02": {"notes.txt": b"base\noperator WIP\ntask\n"},
    "C03": {"notes.txt": b"base\n"},
    "C04": {"scratch.txt": None},
    "C05": {"data.bin": bytes([0, 1, 127, 255]), "link": "data.bin", "run.sh": b"#!/bin/sh\nprintf 'ok\\n'\n"},
    "C06": {"service/message.go": b'package service\n\nconst Message = "new"\n'},
}


def git(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args], stderr=subprocess.PIPE)


def dump(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def state(repo):
    result = {}
    for path in sorted(repo.rglob("*")):
        rel = path.relative_to(repo).as_posix()
        if rel.split("/")[0] in {".git", ".codex-dispatch"} :
            continue
        if path.is_symlink():
            result[rel] = {"link": os.readlink(path)}
        elif path.is_dir():
            continue
        elif not stat.S_ISREG(path.lstat().st_mode):
            result[rel] = {"special_mode": path.lstat().st_mode}
        else:
            result[rel] = {"bytes": base64.b64encode(path.read_bytes()).decode(), "executable": bool(path.stat().st_mode & 0o111)}
    return result


def oracle(case, repo):
    errors = []
    for name, expected in EXPECTED[case].items():
        path = repo / name
        if expected is None:
            good = not path.exists() and not path.is_symlink()
        elif isinstance(expected, str):
            good = path.is_symlink() and os.readlink(path) == expected
        else:
            good = path.is_file() and not path.is_symlink() and path.read_bytes() == expected
        if not good:
            errors.append(f"{name}: expected exact requested state")
    if case == "C05" and not ((repo / "run.sh").stat().st_mode & 0o111 if (repo / "run.sh").exists() else False):
        errors.append("run.sh: executable bit missing")
    return errors


def freeze(out, pricing_source, review_pricing_source):
    out.mkdir(parents=True, exist_ok=False)
    shutil.copy2(__file__, out / "driver.py")
    shutil.copy2(Path(__file__).with_name("audit.py"), out / "audit.py")
    shutil.copy2(pricing_source, out / "official-pricing.md")
    shutil.copy2(review_pricing_source, out / 'official-review-pricing.md')
    import importlib.util
    spec = importlib.util.spec_from_file_location('api_review', ROOT / 'scripts/api-review.py')
    api = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(api)
    (out / 'api-review-system.txt').write_text(api.system_prompt())
    dump(out / 'api-review-schema.json', api.SCHEMA)
    entries = []
    for repetition in range(5):
        for index, case in enumerate(CASES):
            arms = ["direct", "plugin"] if (repetition + index) % 2 == 0 else ["plugin", "direct"]
            for arm in arms:
                name = f"{case}-{repetition + 1}-{arm}"
                trial = out / name
                repo = trial / "repo"
                repo.mkdir(parents=True)
                git(repo, "init", "-q", "-b", "main")
                git(repo, "config", "user.email", "benchmark@example.invalid")
                git(repo, "config", "user.name", "Plan A benchmark")
                git(repo, "config", "core.autocrlf", "false")
                (repo / ".gitignore").write_text(".codex-dispatch/\n")
                (repo / "README.md").write_text("Frozen Plan A synthetic benchmark.\n")
                if case in {"C02", "C03"}:
                    (repo / "notes.txt").write_bytes(b"base\n")
                if case == "C06":
                    (repo / "service").mkdir()
                    (repo / "service/go.mod").write_text("module example.invalid/service\n\ngo 1.25\n")
                    (repo / "service/message.go").write_bytes(b'package service\n\nconst Message = "old"\n')
                git(repo, "add", ".")
                subprocess.run(["git", "-C", str(repo), "commit", "-qm", "frozen input"], check=True,
                               env={**os.environ, "GIT_AUTHOR_DATE": "2026-09-09T00:00:00Z", "GIT_COMMITTER_DATE": "2026-09-09T00:00:00Z"})
                if case in {"C02", "C03"}:
                    (repo / "notes.txt").write_bytes(b"base\noperator WIP\n")
                if case == "C04":
                    (repo / "scratch.txt").write_bytes(b"operator untracked input\x00\xff\n")
                dump(trial / "before.json", state(repo))
                shutil.copy2(repo / ".git/index", trial / "before-index")
                (trial / "before-index-entries").write_bytes(git(repo, "ls-files", "--stage", "-z"))
                task, seeds = CASES[case]
                verify = shlex.join([sys.executable, str(out / "driver.py"), "oracle", case])
                acceptance = "Produce exactly the requested file bytes, symlink targets, and modes; change only the named paths; " + verify + " exits 0"
                constraints = "do not touch unrelated files; do not add new dependencies without justification; do not commit or alter the Git index"
                prompt = f"TASK\n{task}\nACCEPTANCE CRITERIA\n{acceptance}\nCONSTRAINTS\n{constraints}\nRELEVANT FILES\n"
                for seed in seeds:
                    p = repo / seed
                    prompt += seed + "\n" + (repr(p.read_bytes()) if p.exists() else "[missing]") + "\n"
                prompt += "Run verification before finishing: " + verify + "\n"
                (trial / "direct-prompt.txt").write_text(prompt)
                invocation = "/codex-dispatch:codex " + shlex.join(["--max-iter", "3", "--acceptance", acceptance, "--files", ",".join(seeds), "--constraints", constraints, "--test-cmd", verify, "--verify-cmd", verify, task])
                (trial / "plugin-prompt.txt").write_text(invocation)
                entries.append({"name": name, "case": case, "arm": arm, "workdir": "service" if case == "C06" and arm == "direct" else "."})
    product_hashes = {name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest() for name in git(ROOT, "ls-files", "-z").decode().split("\0") if name and (ROOT / name).is_file()}
    dump(out / "candidate-source-sha256.json", product_hashes)
    manifest = {"version": 1, "candidate": git(ROOT, "rev-parse", "HEAD").decode().strip(),
                "model": MODEL, "reasoning": "medium", "router_model": ROUTER, "review_route": REVIEW_ROUTE, "router_thinking_tokens": 0, "structured_output_retries": 0, "review_system_sha256": hashlib.sha256((ROOT / "scripts/compact-review-system.md").read_bytes()).hexdigest(), "entrypoint": "scripts/codex-reviewed.py", "review_tools": ["review_result"], "review_endpoint": api.transport()[0],
                "sandbox": "workspace-write", "approval": "never", "mcp": "none",
                "max_attempts": 3, "timeout_seconds_per_trial": 600,
                "retry_policy": "direct explicit resume with deterministic oracle feedback; plugin advertised inline review loop",
                "pricing_basis": "published-rate-equivalent USD; CTO approved before measurement; not actual proxy billing",
                "rates_per_million": {"input": 5, "cached_input": .5, "output": 30},
                "pricing_source": "https://developers.openai.com/api/docs/pricing.md retrieved 2026-09-09",
                'review_rates_per_million': {'input': 1, 'cached_input': .1, 'output': 5},
                'review_pricing_source': 'https://platform.claude.com/docs/en/about-claude/pricing.md retrieved 2026-09-09',
                "instrumentation": "before/after raw state outside product timing; actual verification/retries inside",
                "codex_config_sha256": hashlib.sha256((Path(os.environ["CODEX_HOME"]) / "config.toml").read_bytes()).hexdigest(),
                "versions": {x: subprocess.check_output([x, "--version"], text=True).strip() for x in ["codex", "claude", "git"]},
                "entries": entries}
    dump(out / "manifest.json", manifest)
    hashes = {p.relative_to(out).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest() for p in out.rglob("*") if p.is_file() and ".git" not in p.parts}
    dump(out / "frozen-sha256.json", hashes)


def invoke(command, prompt, cwd, env, prefix, timeout):
    dump(prefix.with_suffix(".started.json"), {"unix_seconds": time.time(), "cwd": str(cwd)})
    dump(prefix.with_suffix(".command.json"), command)
    prefix.with_suffix(".input.txt").write_text(prompt)
    with prefix.with_suffix(".stdout.jsonl").open("w") as stdout, prefix.with_suffix(".stderr.txt").open("w") as stderr:
        process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=stdout, stderr=stderr, cwd=cwd, env=env, text=True, start_new_session=True)
        try:
            process.communicate(prompt, timeout=max(1, timeout))
            dump(prefix.with_suffix(".termination.json"), {"exit_code": process.returncode, "timeout": False})
            return process.returncode
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            dump(prefix.with_suffix(".termination.json"), {"exit_code": 124, "timeout": True})
            return 124


def execute(out, limit):
    manifest = json.loads((out / "manifest.json").read_text())
    for name, digest in json.loads((out / "frozen-sha256.json").read_text()).items():
        if hashlib.sha256((out / name).read_bytes()).hexdigest() != digest:
            raise RuntimeError("frozen input changed: " + name)
    if git(ROOT, "rev-parse", "HEAD").decode().strip() != manifest["candidate"]:
        raise RuntimeError("candidate revision changed")
    if (out / "started").exists():
        raise RuntimeError("measurement already started; preserve archive and freeze a new full run")
    config_hash = hashlib.sha256((Path(os.environ["CODEX_HOME"]) / "config.toml").read_bytes()).hexdigest()
    if config_hash != manifest["codex_config_sha256"]:
        raise RuntimeError("pinned Codex configuration changed")
    for name, digest in json.loads((out / "candidate-source-sha256.json").read_text()).items():
        if hashlib.sha256((ROOT / name).read_bytes()).hexdigest() != digest:
            raise RuntimeError("candidate bytes changed: " + name)
    (out / "started").touch()
    dump(out / "measurement-kind.json", {"kind": "full" if limit == 60 else "smoke", "trials": limit})
    (out / "shell-startup").mkdir()
    env = {**os.environ, "CODEX_MODEL": MODEL, "CODEX_SANDBOX": "workspace-write", "CLAUDE_PLUGIN_ROOT": str(ROOT), "CODEX_DISPATCH_BIN": str(out / "codex-dispatch"), "ZDOTDIR": str(out / "shell-startup"), "SHELL": "/bin/bash"}
    if not env.get("CODEX_HOME"):
        raise RuntimeError("set CODEX_HOME to isolated pinned benchmark configuration")
    subprocess.run(["go", "build", "-o", str(out / "codex-dispatch"), "./cmd/codex-dispatch"], cwd=ROOT, check=True)
    dump(out / "binary-sha256.json", {"sha256": hashlib.sha256((out / "codex-dispatch").read_bytes()).hexdigest()})
    for entry in manifest["entries"][:limit]:
        for name, digest in json.loads((out / "candidate-source-sha256.json").read_text()).items():
            if hashlib.sha256((ROOT / name).read_bytes()).hexdigest() != digest:
                raise RuntimeError("candidate bytes changed during measurement: " + name)
        trial = out / entry["name"]
        repo = trial / "repo"
        case = entry["case"]
        started = time.monotonic()
        dump(trial / "started.json", {"unix_seconds": time.time(), "name": entry["name"]})
        statuses = []
        session = None
        errors = []
        exception = None
        try:
            for attempt in range(1, 4):
                if entry["arm"] == "plugin":
                    command = [sys.executable, str(ROOT / "scripts/codex-reviewed.py"), "--output-format", "stream-json", "--stdin-request", "--review-transport", "api"]
                    prompt = (trial / "plugin-prompt.txt").read_text()
                else:
                    command = ["codex", "exec", "--json", "-m", MODEL, "-c", 'approval_policy="never"']
                    if session:
                        command += ["resume", session, "-"]
                    else:
                        command += ["-s", "workspace-write", "-C", str(repo / entry["workdir"]), "-"]
                    prompt = (trial / "direct-prompt.txt").read_text() if attempt == 1 else "Verification failed. Repair only these errors and verify again:\n" + "\n".join(errors)
                prefix = trial / f"attempt-{attempt}"
                code = invoke(command, prompt, repo / entry["workdir"], env, prefix, 600 - (time.monotonic() - started))
                statuses.append(code)
                errors = oracle(case, repo)
                dump(trial / f"verification-{attempt}.json", errors)
                if entry["arm"] == "plugin" or code == 124 or not errors:
                    break
                for line in prefix.with_suffix(".stdout.jsonl").read_text().splitlines():
                    try:
                        event = json.loads(line)
                        if event.get("type") == "thread.started":
                            session = event.get("thread_id")
                    except ValueError:
                        pass
                if not session:
                    break
        except Exception as error:
            exception = f"{type(error).__name__}: {error}"
        finally:
            elapsed = time.monotonic() - started
            cleanup_started = time.monotonic()
            # Stop the trial's broker before final state and usage collection.
            # Its subprocess tree is part of this trial, never another checkout.
            pidfile = repo / ".codex-dispatch/broker.pid"
            if pidfile.exists():
                try:
                    os.kill(int(pidfile.read_text()), signal.SIGTERM)
                    deadline = time.monotonic() + 10
                    while pidfile.exists() and time.monotonic() < deadline:
                        time.sleep(.05)
                    if pidfile.exists():
                        exception = (exception or "") + "; broker did not quiesce"
                except ProcessLookupError:
                    pass
                except Exception as error:
                    exception = (exception or "") + f"; broker cleanup: {error}"
            cleanup_seconds = time.monotonic() - cleanup_started
            after = None
            outside = []
            index_preserved = False
            try:
                after = state(repo)
                before = json.loads((trial / "before.json").read_text())
                outside = sorted(k for k in set(before) | set(after) if before.get(k) != after.get(k) and k not in CASES[case][1])
                dump(trial / "after.json", after)
                index_preserved = git(repo, "ls-files", "--stage", "-z") == (trial / "before-index-entries").read_bytes()
            except Exception as error:
                exception = (exception or "") + f"; final-state audit: {error}"
            dump(trial / "outcome.json", {"case": case, "arm": entry["arm"], "seconds": elapsed, "cleanup_seconds": cleanup_seconds, "statuses": statuses, "oracle_errors": errors,
                  "human_code_repair_minutes": 0, "human_repair_basis": "automated trial; no human code repair performed by driver",
                  "outside_changes": outside, "index_preserved": index_preserved, "exception": exception,
                  "behavior_accepted": not exception and after is not None and not errors and not outside and bool(statuses) and statuses[-1] == 0,
                  "promotion_accepted": False, "unverified": ["independent acceptance and cost audit"]})
            print(entry["name"], f"{elapsed:.2f}s", statuses, errors, outside, exception, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["freeze", "run", "oracle"])
    parser.add_argument("target")
    parser.add_argument("--pricing-source", type=Path, help="archived official pricing Markdown, required for freeze")
    parser.add_argument('--review-pricing-source', type=Path, help='archived official Haiku pricing Markdown, required for API freeze')
    parser.add_argument("--limit", type=int, default=60, help="smoke-only partial execution; never promotion evidence")
    args = parser.parse_args()
    if not 1 <= args.limit <= 60:
        parser.error("limit must be between 1 and 60")
    if args.mode == "oracle":
        repo = Path(git(Path.cwd(), "rev-parse", "--show-toplevel").decode().strip())
        errors = oracle(args.target, repo)
        print("\n".join(errors) if errors else "exact behavior verified")
        return bool(errors)
    out = Path(args.target).resolve()
    if args.mode == "freeze":
        if args.pricing_source is None or not args.pricing_source.is_file():
            parser.error("freeze requires an existing --pricing-source")
        if args.review_pricing_source is None or not args.review_pricing_source.is_file():
            parser.error('freeze requires an existing --review-pricing-source')
        freeze(out, args.pricing_source, args.review_pricing_source)
    else:
        execute(out, args.limit)
    return 0


if __name__ == "__main__":
    sys.exit(main())
