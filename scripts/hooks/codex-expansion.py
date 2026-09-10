#!/usr/bin/env python3
"""Dispatch user-invoked /codex before inference; arguments arrive as JSON data."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("review_evidence", ROOT / "review-evidence.py")
EVIDENCE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(EVIDENCE)
HELPER_DEADLINE = 450


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise ValueError(message)


def parse(raw):
    parser = Parser(add_help=False, allow_abbrev=False)
    for flag in ["acceptance", "files", "workdir", "constraints", "test-cmd", "verify-cmd"]:
        parser.add_argument("--" + flag, default="")
    parser.add_argument("--max-iter", type=int, default=3)
    for flag in ["no-tests", "clean-verify", "no-resume", "detach", "list"]:
        parser.add_argument("--" + flag, action="store_true")
    for flag in ["status", "cancel"]:
        parser.add_argument("--" + flag)
    parser.add_argument("task", nargs=argparse.REMAINDER)
    args = parser.parse_args(shlex.split(raw))
    if args.task[:1] == ["--"]:
        args.task = args.task[1:]
    args.task = " ".join(args.task)
    if not 1 <= args.max_iter <= 10:
        raise ValueError("--max-iter must be between 1 and 10")
    if sum(bool(v) for v in [args.detach, args.list, args.status, args.cancel]) > 1:
        raise ValueError("background operation flags are mutually exclusive")
    if not args.task.strip() and not (args.list or args.status or args.cancel):
        raise ValueError("/codex needs a task description")
    return vars(args)


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


def environment(config, repo):
    env = dict(os.environ)
    env.setdefault("CODEX_REVIEW_TRANSPORT", "api")
    for key in ["CODEX_RESULT_DIR", "CODEX_SESSION_ID", "CODEX_FEEDBACK"]:
        env.pop(key, None)
    env.update({"CODEX_TASK": config["task"], "CODEX_ACCEPTANCE": config["acceptance"] or config["task"],
                "CODEX_FILES": config["files"], "CODEX_WORKDIR": config["workdir"],
                "CODEX_CONSTRAINTS": "do not touch unrelated files; do not add new dependencies without justification; " + config["constraints"],
                "REVIEW_TEST_POLICY": "skip" if config["no_tests"] else "run",
                "REVIEW_TEST_CMD": config["test_cmd"] or ("" if config["no_tests"] else "__auto__"),
                "REVIEW_VERIFY_CMD": config["verify_cmd"], "REVIEW_CLEAN_VERIFY": str(config["clean_verify"]).lower()})
    # Preserve a stricter operator timeout; reserve time to record failures before
    # the enclosing hook's 480-second deadline.
    timeout = int(env.get("CODEX_DISPATCH_TIMEOUT_MS", "0"))
    env["CODEX_DISPATCH_TIMEOUT_MS"] = str(min(timeout, 400000) if timeout > 0 else 400000)
    return env


def invoke(argv, env, cwd, deadline=None, background_handoff=False, on_handoff=None):
    remaining = min(HELPER_DEADLINE, deadline - time.monotonic()) if deadline is not None else HELPER_DEADLINE
    if remaining <= 10:
        raise ValueError('command controller deadline exhausted')
    env = {key: value for key, value in env.items() if key not in {'ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN'}}
    env = {**env, 'CODEX_DISPATCH_TIMEOUT_MS': str(min(int(env.get('CODEX_DISPATCH_TIMEOUT_MS', '400000')), int((remaining - 10) * 1000)))}
    import tempfile
    with tempfile.TemporaryDirectory(prefix="codex-helper-") as directory:
        result = EVIDENCE.POLICY.supervised(argv, cwd, env, Path(directory) / "stdout", Path(directory) / "stderr", remaining, background_handoff=background_handoff, handoff_record=(Path(env["REVIEW_RECEIPT_PATH"]).parent / ".handoffs" / (Path(env["REVIEW_RECEIPT_PATH"]).stem + ".json")) if background_handoff and env.get("REVIEW_RECEIPT_PATH") else None, on_handoff=on_handoff)
        if result['exit_code']:
            raise ValueError(f"dispatch/evidence failed ({result['exit_code']}): {result['stderr'][-2000:]} {result['stdout'][-2000:]}")
        if result['output_truncated']:
            # Controller JSON must be complete; helper output is capped on disk.
            return Path(result['stdout_path']).read_text()
        return result['stdout']


def write(path, record):
    temp = path.with_suffix(".tmp")
    temp.write_text(json.dumps(record, indent=2) + "\n")
    os.replace(temp, path)


def validate_api_completion(saved, repo, raw):
    spec = importlib.util.spec_from_file_location('api_review', ROOT / 'api-review.py')
    api = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(api)
    api.validate_history(saved)
    spec = importlib.util.spec_from_file_location('compact_profile', ROOT / 'codex-reviewed.py')
    profile = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(profile)
    return profile.validate_result({'subtype': 'success', 'terminal_reason': 'completed',
        'session_id': saved['identity']['session_id'], 'structured_output': saved['report']},
        '/codex-dispatch:codex ' + raw, repo=repo,
        receipt_path=repo / '.codex-dispatch/expansions' / saved['identity']['session_id'] / (saved['identity']['prompt_id'] + '.json'))


def expand(event):
    deadline = time.monotonic() + 450  # Reserve 30s for receipt/error and child cleanup.
    if event.get("command_name") != "codex-dispatch:codex":
        return {}
    if event.get("expansion_type") != "slash_command" or event.get("command_source") != "plugin":
        raise ValueError("untrusted command expansion source")
    sid, pid = (str(uuid.UUID(event[key])) for key in ["session_id", "prompt_id"])
    raw = event["command_args"]
    config = parse(raw)
    cwd = Path(event["cwd"]).resolve()
    repo = Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], cwd=cwd, text=True).strip()).resolve()
    identity = {"session_id": sid, "prompt_id": pid, "args_sha256": hashlib.sha256(raw.encode()).hexdigest(), "repo": str(repo)}
    directory = repo / ".codex-dispatch/expansions" / sid
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    path = directory / (pid + ".json")
    env = environment(config, repo)
    env["REVIEW_RECEIPT_PATH"] = str(path)
    env["REVIEW_INVOCATION_ID"] = json.dumps(identity, sort_keys=True)
    dispatch_env = {k: env[k] for k in ["CODEX_TASK", "CODEX_ACCEPTANCE", "CODEX_FILES", "CODEX_WORKDIR", "CODEX_CONSTRAINTS", "REVIEW_TEST_POLICY", "REVIEW_TEST_CMD", "REVIEW_VERIFY_CMD", "REVIEW_CLEAN_VERIFY", "REVIEW_RECEIPT_PATH", "REVIEW_INVOCATION_ID"]}
    header = {"identity": identity, "config": config, "dispatch_env": dispatch_env,
              'review_contract_sha256': hashlib.sha256((ROOT / 'direct-review-contract.md').read_bytes()).hexdigest(),
              'hook_event': {key: event[key] for key in ['command_name', 'command_source', 'expansion_type', 'session_id', 'prompt_id', 'command_args', 'cwd']}}
    # Exclusive create precedes execution. A crash or concurrent duplicate stays
    # blocked; it cannot silently dispatch again under the same prompt identity.
    try:
        with path.open("x") as output:
            json.dump({**header, "status": "running"}, output)
    except FileExistsError:
        saved = json.loads(path.read_text())
        if saved.get("identity") != identity or saved.get("status") != "complete":
            raise ValueError("duplicate command is pending/failed or identity changed")
        if saved.get("review_transport") == "api":
            if saved.get("terminal_validation") != "passed":
                raise ValueError("terminal validation pending or failed")
            validate_api_completion(saved, repo, raw)
        return saved["output"]
    background_transfer = None
    def remember_handoff(record):
        nonlocal background_transfer
        background_transfer = record
    try:
        api = None
        if env.get('CODEX_REVIEW_TRANSPORT') == 'api':
            spec = importlib.util.spec_from_file_location('api_review', ROOT / 'api-review.py')
            api = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(api)
        background = ["--detach"] if config["detach"] else ["--list"] if config["list"] else ["--status", config["status"]] if config["status"] else ["--cancel", config["cancel"]] if config["cancel"] else []
        if not background and api is not None:
            api.transport()  # Only synchronous review needs a reviewer credential.
        if background:
            output = invoke([EVIDENCE.bash_executable(), (ROOT / "dispatch-codex.sh").as_posix(), *background], env, cwd, deadline, background_handoff=config["detach"], on_handoff=remember_handoff)
            payload = {"identity": identity, "kind": "background", "output": output}
        else:
            bundle = json.loads(invoke([sys.executable, str(ROOT / "review-evidence.py")], env, cwd, deadline))
            if bundle.get("complete") is not True or bundle.get("error"):
                raise ValueError("incomplete dispatch evidence")
            payload = {"identity": identity, "kind": "review", "iteration": 1, "config": config,
                       "dispatch_env": dispatch_env,
                       "bundle": bundle, "run_dir": bundle["run_dir"], "codex_session": bundle["result"]["session_id"]}
        payload["receipt_path"] = str(path)
        if api is not None:
            if payload['kind'] == 'background':
                report = {'kind': 'background', 'output': payload['output']}
            else:
                def repair(repair_env, repair_cwd):
                    return json.loads(invoke([sys.executable, str(ROOT / 'review-evidence.py')], repair_env, repair_cwd, deadline))
                report = api.control(payload, config, env, cwd, repair, deadline)
            display = report['output'] if report['kind'] == 'background' else '\n'.join([f"Codex {report['verdict']}: {report['reason']}", f"iterations: {report['iterations']} / {report['max_iterations']}", 'files: ' + ', '.join(report['files_changed']), *report['feedback']])
            output = {'continue': False, 'stopReason': 'Codex API review controller completed', 'systemMessage': display}
            saved = {**header, 'status': 'complete', 'payload': payload, 'output': output,
                     'review_transport': 'api', 'report': report, 'terminal_validation': 'pending'}
            write(path, saved)
            validate_api_completion(saved, repo, raw)
            write(path, {**saved, 'terminal_validation': 'passed'})
            return output
        context = "CODEX_EXPANSION_RECEIPT\n" + json.dumps(payload) + "\nEND_CODEX_EXPANSION_RECEIPT"
        output = {"hookSpecificOutput": {"hookEventName": "UserPromptExpansion", "additionalContext": context}}
        write(path, {**header, "status": "complete", "payload": payload, "output": output})
        return output
    except BaseException as error:
        if background_transfer is not None:
            write(path, {**header, "status": "background-started", "handoff": background_transfer, "error": str(error)})
            raise ValueError("background task started before interruption: " + background_transfer['task_id']) from error
        write(path, {**header, "status": "failed", "error": str(error)})
        raise


def main():
    try:
        output = expand(json.load(sys.stdin))
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        output = {"decision": "block", "reason": "Codex command blocked: " + str(error)}
    print(json.dumps(output))


if __name__ == "__main__":
    main()
