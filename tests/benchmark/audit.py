#!/usr/bin/env python3
"""Independent recovery, route, effective-policy and cost audit of frozen trials."""
import base64
import importlib.util
import json
import math
import os
from pathlib import Path
import re
import statistics
import subprocess
import sys


def read(path):
    return json.loads(path.read_text())


def events(path):
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def git(repo, *args, env=None):
    return subprocess.check_output(["git", "-C", str(repo), *args], env=env, stderr=subprocess.PIPE)


def tree_state(repo, tree):
    result = {}
    for record in git(repo, "ls-tree", "-r", "-z", tree).split(b"\0"):
        if not record:
            continue
        header, name = record.split(b"\t", 1)
        mode, kind, oid = header.split()
        assert kind == b"blob", "special tree entry"
        data = git(repo, "cat-file", "blob", oid.decode())
        result[name.decode()] = {"link": data.decode()} if mode == b"120000" else {"bytes": base64.b64encode(data).decode(), "executable": mode == b"100755"}
    return result


def audit_trial(out, entry, home, module):
    trial = out / entry["name"]
    repo = trial / "repo"
    outcome = read(trial / "outcome.json")
    failures = []
    if not outcome["behavior_accepted"]:
        failures.append("behavior/scope/process failed")
    if not outcome["index_preserved"]:
        failures.append("index staging changed")
    sessions = set()
    claude_cost = 0
    if entry["arm"] == "plugin":
        stream = events(trial / "attempt-1.stdout.jsonl")
        reports = [e for e in stream if e.get("type") == "result"]
        final = reports[-1] if reports else {}
        route = final.get("subagent_stats", {}).get("by_type", {})
        if route.get("codex-dispatch:codex-orchestrator") != 1:
            failures.append("actual orchestrator route unproven")
        if not re.search(r"verdict\s*:\s*pass\b", final.get("result", ""), re.I):
            failures.append("review did not report pass")
        if final.get("permission_denials") or final.get("is_error"):
            failures.append("Claude permission/error outcome")
        usage = final.get("modelUsage", {})
        unique = {e["message"]["id"]: e["message"].get("usage", {}) for e in stream if e.get("type") == "assistant" and e.get("message", {}).get("id")}
        for raw, aggregate in [("input_tokens", "inputTokens"), ("cache_read_input_tokens", "cacheReadInputTokens"), ("cache_creation_input_tokens", "cacheCreationInputTokens")]:
            if sum(u.get(raw, 0) for u in unique.values()) != sum(u.get(aggregate, 0) for u in usage.values()):
                failures.append("Claude usage reconciliation: " + raw)
        claude_cost = final.get("total_cost_usd")
        if claude_cost is None or not usage or any(u.get("costBasis") != "list" for u in usage.values()):
            failures.append("Claude list cost unproven")
            claude_cost = 0
        elif not math.isclose(claude_cost, sum(u["costUSD"] for u in usage.values()), abs_tol=1e-9):
            failures.append("Claude aggregate cost inconsistent")
        runs = sorted((repo / ".codex-dispatch/runs").glob("*/result.json"))
        if not 1 <= len(runs) <= 3:
            failures.append("dispatch count outside retry budget")
        previous = read(trial / "before.json")
        for index, result_path in enumerate(runs):
            result = read(result_path)
            sessions.add(result["session_id"])
            run = result_path.parent
            snap = read(run / "baseline-snapshot.json")
            if git(repo, "rev-parse", snap["ref"] + "^{tree}").decode().strip() != snap["tree"]:
                failures.append("baseline recovery ref mismatch")
            if tree_state(repo, snap["tree"]) != previous:
                failures.append("baseline cannot recover exact pre-task state")
            if index == 0 and (run / "baseline-index").read_bytes() != (trial / "before-index").read_bytes():
                # Keep raw evidence, compare semantic staging when stat metadata changed.
                env = {**os.environ, "GIT_INDEX_FILE": str(run / "baseline-index")}
                if git(repo, "ls-files", "--stage", "-z", env=env) != (trial / "before-index-entries").read_bytes():
                    failures.append("baseline cannot recover original staging")
            temp_index = trial / f"audit-index-{index}"
            env = {**os.environ, "GIT_INDEX_FILE": str(temp_index)}
            git(repo, "read-tree", snap["tree"], env=env)
            patch = run / "diff.patch"
            if patch.stat().st_size:
                git(repo, "-c", "core.autocrlf=false", "apply", "--cached", "--binary", str(patch), env=env)
            post_tree = git(repo, "write-tree", env=env).decode().strip()
            previous = tree_state(repo, post_tree)
            actual = set(filter(None, git(repo, "diff", "--name-only", "-z", snap["tree"], post_tree).decode().split("\0")))
            if actual != set(result["files_changed"]) or actual - set(module.CASES[entry["case"]][1]):
                failures.append("task delta scope/attribution mismatch")
        if previous != read(trial / "after.json"):
            failures.append("patch replay differs from exact final state")
    else:
        for path in trial.glob("attempt-*.stdout.jsonl"):
            for event in events(path):
                if event.get("type") == "thread.started":
                    sessions.add(event["thread_id"])
    totals = {"input_tokens": 0, "cached_input_tokens": 0, "output_tokens": 0}
    effective = []
    for session in sessions:
        paths = list((home / "sessions").rglob("*" + session + ".jsonl"))
        if len(paths) != 1:
            failures.append("missing/ambiguous effective Codex session evidence")
            continue
        transcript = events(paths[0])
        counts = []
        for event in transcript:
            payload = event.get("payload", {})
            if event.get("type") == "turn_context":
                policy = {k: payload.get(k) for k in ["cwd", "model", "effort", "approval_policy", "sandbox_policy"]}
                effective.append(policy)
                expected_cwd = repo / "service" if entry["case"] == "C06" else repo
                if Path(policy["cwd"]).resolve() != expected_cwd.resolve() or policy["model"] != module.MODEL or policy["effort"] != "medium" or policy["approval_policy"] != "never" or policy["sandbox_policy"].get("type") != "workspace-write":
                    failures.append("effective execution policy mismatch")
            if event.get("type") == "event_msg" and payload.get("type") == "token_count" and payload.get("info"):
                counts.append(payload["info"]["total_token_usage"])
                if payload["info"].get("last_token_usage", {}).get("input_tokens", 0) >= 272000:
                    failures.append("context exceeds frozen short-context price tier")
        if not counts:
            failures.append("Codex usage missing")
        else:
            for key in totals:
                totals[key] += counts[-1][key]
        # Preserve the synthetic transcript after scoring, outside product timing.
        (trial / (session + ".rollout.jsonl")).write_bytes(paths[0].read_bytes())
    if not sessions or not effective:
        failures.append("effective session policy unproven")
    codex_cost = ((totals["input_tokens"] - totals["cached_input_tokens"]) * 5 + totals["cached_input_tokens"] * .5 + totals["output_tokens"] * 30) / 1e6
    return {"accepted": not failures, "failures": failures, "effective_policy": effective, "codex_tokens": totals, "codex_cost_equivalent_usd": codex_cost, "claude_cost_equivalent_usd": claude_cost, "total_cost_equivalent_usd": codex_cost + claude_cost, "seconds": outcome["seconds"]}


def main():
    out, home = map(lambda p: Path(p).resolve(), sys.argv[1:3])
    spec = importlib.util.spec_from_file_location("frozen_driver", out / "driver.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    manifest = read(out / "manifest.json")
    results = {}
    for entry in manifest["entries"]:
        trial = out / entry["name"]
        if not (trial / "outcome.json").exists():
            continue
        try:
            score = audit_trial(out, entry, home, module)
        except Exception as error:
            score = {"accepted": False, "failures": [f"audit failed: {type(error).__name__}: {error}"]}
        (trial / "audit.json").write_text(json.dumps(score, indent=2) + "\n")
        results[entry["name"]] = score
    summary = {"scored": len(results), "full_cohort": len(results) == 60, "arms": {}}
    for arm in ["direct", "plugin"]:
        selected = [v for k, v in results.items() if k.endswith(arm)]
        accepted = [v for v in selected if v["accepted"]]
        summary["arms"][arm] = {"accepted": len(accepted), "total": len(selected),
                                  "median_seconds_accepted": statistics.median(v["seconds"] for v in accepted) if accepted else None,
                                  "cost_per_accepted": sum(v.get("total_cost_equivalent_usd", 0) for v in selected) / len(accepted) if accepted else None,
                                  "failures": {k: v["failures"] for k,v in results.items() if k.endswith(arm) and v["failures"]}}
    (out / "audit-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
