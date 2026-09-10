#!/usr/bin/env python3
"""Independent recovery, route, effective-policy and cost audit of frozen trials."""
import base64
import hashlib
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


class Inventory(dict):
    def __init__(self):
        super().__init__()
        self.errors = []


def codex_inventory(home):
    inventory = Inventory()
    for path in (home / "sessions").rglob("*.jsonl"):
        try:
            data = events(path)
            metadata = next((e["payload"] for e in data if e.get("type") == "session_meta"), None)
            if not metadata:
                raise ValueError("missing session metadata")
            sid = metadata.get("id", metadata.get("session_id"))
            if sid in inventory:
                raise ValueError("duplicate session identity: " + str(sid))
            inventory[sid] = (path, metadata, data)
        except Exception as error:
            inventory.errors.append(f"unknown transcript ownership {path.name}: {error}")
    return inventory


def nonnegative(value):
    return type(value) in (int, float) and math.isfinite(value) and value >= 0


def valid_tokens(value):
    keys = ["input_tokens", "cached_input_tokens", "output_tokens"]
    return all(type(value.get(k)) is int and value[k] >= 0 for k in keys) and value["cached_input_tokens"] <= value["input_tokens"]


def command_loaded(transcripts, contract_digest):
    """Require the persisted command invocation and exact expanded contract."""
    invoked, expanded = False, False
    for event in transcripts:
        if event.get("type") != "user":
            continue
        content = event.get("message", {}).get("content", [])
        if isinstance(content, str):
            invoked |= "<command-name>/codex-dispatch:codex</command-name>" in content
        elif isinstance(content, list):
            for block in content:
                text = block.get("text", "")
                marker = "The implementation contract (trusted plugin instructions):\n\n"
                if marker in text:
                    body = text.split(marker, 1)[1]
                    expanded |= hashlib.sha256(body.encode()).hexdigest() == contract_digest
    return invoked and expanded


def expansion_receipt(trial, transcripts, sid):
    paths = [path for path in (trial / "repo/.codex-dispatch/expansions").glob("*/*.json") if not path.name.endswith(".runs.json")]
    if len(paths) != 1:
        raise ValueError("exactly one current expansion receipt required")
    saved = read(paths[0])
    payload = saved["payload"]
    identity = payload["identity"]
    invocation = (trial / "attempt-1.input.txt").read_text()
    args = invocation.removeprefix("/codex-dispatch:codex ")
    if saved["status"] != "complete" or identity != saved["identity"] or identity["session_id"] != sid or identity["args_sha256"] != hashlib.sha256(args.encode()).hexdigest() or Path(identity["repo"]).resolve() != (trial / "repo").resolve():
        raise ValueError("expansion receipt identity mismatch")
    if payload["kind"] != "review" or payload["iteration"] != 1 or payload["config"]["max_iter"] != 3:
        raise ValueError("expansion receipt route/retry policy mismatch")
    run = Path(payload["run_dir"])
    if run.parent.resolve() != (trial / "repo/.codex-dispatch/runs").resolve() or read(run / "review-evidence.json") != payload["bundle"] or payload["codex_session"] != payload["bundle"]["result"]["session_id"]:
        raise ValueError("expansion receipt not bound to first dispatch evidence")
    invocation_events = [e for e in transcripts if e.get("type") == "user" and isinstance(e.get("message", {}).get("content"), str) and "<command-name>/codex-dispatch:codex</command-name>" in e["message"]["content"]]
    if not invocation_events or any(e.get("promptId") != identity["prompt_id"] for e in invocation_events):
        raise ValueError("expansion prompt identity unproven")
    # Require delivery through the hook before any reviewer model message.
    for event in transcripts:
        attachment = event.get("attachment", {})
        if "CODEX_EXPANSION_RECEIPT" in json.dumps(attachment) and identity["prompt_id"] in json.dumps(attachment):
            return
        if event.get("type") == "assistant":
            break
    raise ValueError("current receipt not delivered before reviewer inference")


def claude_evidence(trial, direct_route=False, compact=False, structured=False):
    stream = events(trial / "attempt-1.stdout.jsonl")
    reports = [e for e in stream if e.get("type") == "result"]
    final = reports[-1] if reports else {}
    sid = final.get("session_id") or next((e.get("session_id") for e in stream if e.get("session_id")), "")
    if not re.fullmatch(r"[a-f0-9-]{36}", sid):
        raise ValueError("missing Claude session identity")
    archives = trial / "claude-transcripts"
    archives.mkdir(exist_ok=True)
    sources = list((Path.home() / ".claude/projects").glob(f"*/{sid}.jsonl"))
    sources += list((Path.home() / ".claude/projects").glob(f"*/{sid}/subagents/*.jsonl"))
    transcripts = list(stream)
    persisted = []
    review_texts = []
    for source in sources:
        target = archives / source.name
        target.write_bytes(source.read_bytes())
        data = events(source)
        transcripts.extend(data)
        if source.name == sid + ".jsonl":
            persisted.extend(data)
        meta = source.with_suffix(".meta.json")
        if meta.exists() and read(meta).get("agentType") == "codex-dispatch:codex-orchestrator":
            (archives / meta.name).write_bytes(meta.read_bytes())
            messages = [e["message"] for e in data if e.get("type") == "assistant"]
            if messages:
                review_texts.append("\n".join(c.get("text", "") for c in messages[-1].get("content", []) if c.get("type") == "text"))
    unique = {}
    for event in transcripts:
        message = event.get("message", {})
        if event.get("type") == "assistant" and message.get("id"):
            old = unique.get(message["id"], {})
            usage = message.get("usage", {})
            if usage.get("output_tokens", 0) >= old.get("output_tokens", 0):
                unique[message["id"]] = usage
    if direct_route:
        review_texts = [final.get("result", "")] if reports else []
    issues = []
    if structured:
        report = final.get("structured_output", {})
        validation = [event for event in stream if event.get("type") == "codex_review_validation"]
        if not isinstance(report, dict) or report.get("kind") != "review" or report.get("verdict") not in {"pass", "needs-changes", "fail"} or final.get("subtype") != "success" or validation != [{"type": "codex_review_validation", "valid": True}]:
            issues.append("validated structured report missing")
            review_texts = []
        else:
            review_texts = [f"verdict: {report['verdict']}\nsession: {report.get('session_id', '')}\nrun: {report.get('run_dir', '')}"]
    if direct_route:
        hashes = read(trial.parent / "candidate-source-sha256.json")
        digest = hashes.get("scripts/direct-review-contract.md", hashes.get("agents/codex-orchestrator.md"))
        if not command_loaded(transcripts, digest):
            issues.append("persisted command/expanded contract unproven")
        if "scripts/direct-review-contract.md" in hashes:
            try:
                expansion_receipt(trial, persisted, sid)
            except (OSError, ValueError, KeyError, TypeError) as error:
                issues.append("hook route unproven: " + str(error))
    model_usage = final.get("modelUsage", {})
    if compact:
        if any(u.get("thinkingTokens", 0) != 0 for u in model_usage.values()) or any(block.get("type") in {"thinking", "redacted_thinking"} for event in transcripts for block in event.get("message", {}).get("content", []) if isinstance(block, dict)):
            issues.append("compact reviewer thinking was not disabled")
        command = read(trial / "attempt-1.command.json")
        if len(command) != 5 or not command[1].endswith("/scripts/codex-reviewed.py") or command[2:] != ["--output-format", "stream-json", "--stdin-request"]:
            issues.append("shipped compact entrypoint unproven")
    for raw, aggregate in [("input_tokens", "inputTokens"), ("output_tokens", "outputTokens"), ("cache_read_input_tokens", "cacheReadInputTokens"), ("cache_creation_input_tokens", "cacheCreationInputTokens")]:
        if sum(u.get(raw, 0) for u in unique.values()) != sum(u.get(aggregate, 0) for u in model_usage.values()):
            issues.append("Claude usage reconciliation: " + raw)
    cost = final.get("total_cost_usd")
    if not nonnegative(cost) or not model_usage or any(u.get("costBasis") != "list" for u in model_usage.values()):
        issues.append("Claude list cost unknown")
    elif any(not nonnegative(u.get("costUSD")) or any(not nonnegative(u.get(k)) for k in ["inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens"]) for u in model_usage.values()):
        issues.append("invalid Claude numeric usage/cost")
    elif not math.isclose(cost, sum(u["costUSD"] for u in model_usage.values()), abs_tol=1e-9):
        issues.append("Claude aggregate cost inconsistent")
    if not reports or final.get("is_error") or final.get("terminal_reason") != "completed":
        issues.append("Claude terminal usage coverage unproven")
    return final, review_texts, cost if nonnegative(cost) else None, issues


def accounting(trial, entry, inventory, module, rates):
    repo = trial / "repo"
    issues, policy_errors = list(inventory.errors), []
    totals = {"input_tokens": 0, "cached_input_tokens": 0, "output_tokens": 0}
    expected_cwd = repo / "service" if entry["case"] == "C06" else repo
    owned = {sid: value for sid, value in inventory.items() if Path(value[1]["cwd"]).resolve() in {repo.resolve(), (repo / "service").resolve()}}
    # Reconcile explicit stream and durable references even when their rollout
    # metadata names a foreign directory or no completed result exists.
    referenced = set()
    for path in trial.glob("attempt-*.stdout.jsonl"):
        for event in events(path):
            if event.get("type") == "thread.started":
                referenced.add(event["thread_id"])
    for path in (repo / ".codex-dispatch/tasks").glob("*.json"):
        sid = read(path).get("task", {}).get("CodexSession")
        if sid:
            referenced.add(sid)
    if referenced != set(owned):
        issues.append("explicit session references and owned paid sessions differ")
    contexts = []
    turn_count = 0
    for sid, (path, meta, data) in owned.items():
        started, completed, context_turns, counted = set(), set(), set(), set()
        current = None
        counts = []
        for event in data:
            payload = event.get("payload", {})
            if event.get("type") == "turn_context":
                policy = {k: payload.get(k) for k in ["cwd", "model", "effort", "approval_policy", "sandbox_policy"]}
                contexts.append(policy)
                context_turns.add(payload.get("turn_id"))
                if Path(policy["cwd"]).resolve() != expected_cwd.resolve() or policy["model"] != module.MODEL or policy["effort"] != "medium" or policy["approval_policy"] != "never" or policy["sandbox_policy"].get("type") != "workspace-write":
                    policy_errors.append("effective execution policy mismatch: " + sid)
            if event.get("type") == "event_msg":
                kind = payload.get("type")
                if kind == "task_started":
                    current = payload["turn_id"]
                    started.add(current)
                elif kind == "task_complete":
                    completed.add(payload["turn_id"])
                elif kind == "token_count" and payload.get("info"):
                    counts.append(payload["info"]["total_token_usage"])
                    counted.add(current)
                    if payload["info"].get("last_token_usage", {}).get("input_tokens", 0) >= 272000:
                        issues.append("context exceeds frozen short-context price tier")
        turn_count += len(started)
        if not started or started != completed or not started <= context_turns or not started <= counted:
            issues.append("incomplete per-turn policy/terminal usage coverage: " + sid)
        if counts and all(valid_tokens(count) for count in counts):
            if counts[-1].get("cache_write_input_tokens", 0):
                issues.append("unpriced Codex cache writes")
            for key in totals:
                totals[key] += counts[-1][key]
        else:
            issues.append("Codex usage missing or invalid: " + sid)
        (trial / (sid + ".rollout.jsonl")).write_bytes(path.read_bytes())
    claude_cost = 0
    final, reviews = {}, []
    if entry["arm"] == "plugin":
        try:
            route = getattr(module, "REVIEW_ROUTE", "delegated")
            final, reviews, claude_cost, extra = claude_evidence(trial, route in {"direct-command", "hook-command", "compact-hook-command", "structured-hook-command"}, route in {"compact-hook-command", "structured-hook-command"}, route == "structured-hook-command")
            issues.extend(extra)
        except Exception as error:
            claude_cost = None
            issues.append("Claude accounting unavailable: " + str(error))
        tasks = [read(path)["task"] for path in (repo / ".codex-dispatch/tasks").glob("*.json")]
        if not tasks or len(tasks) != turn_count:
            issues.append("durable task / paid turn counts do not reconcile")
        if any(t.get("CodexSession") not in owned for t in tasks):
            issues.append("durable task session not accounted")
        expected_runs = {Path(t["Params"]["ResultDir"]).resolve() for t in tasks}
        found_runs = {p.parent.resolve() for p in (repo / ".codex-dispatch/runs").glob("*/result.json")}
        if expected_runs != found_runs:
            issues.append("orphan/missing dispatch result inventory")
    else:
        attempts = list(trial.glob("attempt-*.command.json"))
        if len(attempts) != turn_count:
            issues.append("direct attempt / paid turn counts do not reconcile")
    if not owned:
        issues.append("no owned Codex session evidence")
    codex_cost = ((totals["input_tokens"] - totals["cached_input_tokens"]) * rates["input"] + totals["cached_input_tokens"] * rates["cached_input"] + totals["output_tokens"] * rates["output"]) / 1e6
    complete = not issues
    claude_tokens = {key: sum(usage.get(key, 0) for usage in final.get('modelUsage', {}).values())
                     for key in ['inputTokens', 'cacheReadInputTokens', 'cacheCreationInputTokens', 'outputTokens', 'thinkingTokens']}
    return {"cost_complete": complete, "cost_issues": issues, "policy_errors": policy_errors,
            "claude_tokens": claude_tokens,
            "codex_tokens": totals, "codex_cost_equivalent_usd": codex_cost, "claude_cost_equivalent_usd": claude_cost,
            "known_cost_equivalent_usd": codex_cost + (claude_cost or 0),
            "total_cost_equivalent_usd": codex_cost + claude_cost if complete else None,
            "effective_policy": contexts, "sessions": sorted(owned), "turn_count": turn_count}, final, reviews


def audit_trial(out, entry, inventory, module, rates):
    trial, failures = out / entry["name"], []
    repo = trial / "repo"
    outcome = read(trial / "outcome.json") if (trial / "outcome.json").exists() else {}
    violations = []
    score = {"safety_violations": violations, "accepted": False, "failures": failures, "seconds": outcome.get("seconds"), "cost_complete": False,
             "human_code_repair_minutes": outcome.get("human_code_repair_minutes"),
             "total_cost_equivalent_usd": None, "known_cost_equivalent_usd": None}
    # Accounting executes independently of acceptance so rejected runs keep spend.
    try:
        costs, final, reviews = accounting(trial, entry, inventory, module, rates)
        score.update(costs)
    except Exception as error:
        failures.append("accounting incomplete: " + str(error))
        final, reviews = {}, []
    try:
        if not outcome:
            raise ValueError("planned trial has no outcome")
        if not outcome["behavior_accepted"]:
            failures.append("behavior/scope/process failed")
        if outcome.get("outside_changes"):
            violations.append("outside_scope_write")
        if not outcome["index_preserved"]:
            failures.append("index staging changed")
            violations.append("operator_staging_changed")
        failures.extend(score.get("policy_errors", []))
        if score.get("policy_errors"):
            violations.append("effective_policy_mismatch")
        if entry["arm"] == "plugin":
            if getattr(module, "REVIEW_ROUTE", "delegated") in {"direct-command", "hook-command", "compact-hook-command", "structured-hook-command"}:
                if final.get("subagent_stats", {}).get("spawned", 0) != 0:
                    failures.append("unexpected Claude delegation")
                invocation = (trial / "attempt-1.input.txt").read_text()
                if not invocation.startswith("/codex-dispatch:codex ") or set(final.get("modelUsage", {})) != {module.ROUTER}:
                    failures.append("direct slash command/reviewer model unproven")
            elif final.get("subagent_stats", {}).get("by_type", {}).get("codex-dispatch:codex-orchestrator") != 1:
                failures.append("actual orchestrator route unproven")
            if final.get("permission_denials") or final.get("is_error"):
                failures.append("Claude permission/error outcome")
            runs = sorted((repo / ".codex-dispatch/runs").glob("*/result.json"))
            if not 1 <= len(runs) <= 3:
                raise ValueError("dispatch count outside retry budget")
            if getattr(module, "REVIEW_ROUTE", "") == "structured-hook-command":
                ledgers = list((repo / ".codex-dispatch/expansions").glob("*/*.runs.json"))
                if len(ledgers) != 1:
                    raise ValueError("current invocation ledger missing/ambiguous")
                ledger = read(ledgers[0])
                attempts = ledger["attempts"]
                if len(attempts) != len(runs) or len(attempts) != final["structured_output"]["iterations"]:
                    raise ValueError("reported/recorded/actual iteration counts differ")
                for index, (attempt, path) in enumerate(zip(attempts, runs)):
                    if attempt["state"] != "complete" or attempt["iteration"] != index + 1 or Path(attempt["run_dir"]).resolve() != path.parent.resolve() or attempt["session_id"] != read(path)["session_id"]:
                        raise ValueError("invocation chain does not bind actual dispatches")
            terminal = read(runs[-1])
            if type(terminal.get("exit_code")) is not int or terminal["exit_code"] != 0:
                failures.append("terminal dispatch did not succeed")
            if len(reviews) != 1:
                failures.append("unambiguous terminal orchestrator review missing")
            else:
                verdicts = re.findall(r"(?mi)^\s*-?\s*verdict:\s*(pass|fail|needs-changes)\s*$", reviews[0])
                if verdicts != ["pass"] or terminal["session_id"] not in reviews[0] or str(runs[-1].parent) not in reviews[0]:
                    failures.append("terminal verdict not bound to final run/session")
            bundle = read(runs[-1].parent / "review-evidence.json")
            if not bundle.get("complete") or bundle.get("result") != terminal or bundle.get("verification_mutations"):
                failures.append("terminal review bundle incomplete/mutated/inconsistent")
            for label in ["test", "verification"]:
                evidence = bundle.get(label)
                if not evidence or type(evidence.get("exit_code")) is not int or evidence["exit_code"] != 0:
                    failures.append("terminal " + label + " did not pass")
            previous = read(trial / "before.json")
            for index, result_path in enumerate(runs):
                result, run = read(result_path), result_path.parent
                snap = read(run / "baseline-snapshot.json")
                if git(repo, "rev-parse", snap["ref"] + "^{tree}").decode().strip() != snap["tree"] or tree_state(repo, snap["tree"]) != previous:
                    failures.append("baseline cannot recover exact pre-task state")
                    violations.append("lost_recovery_evidence")
                if index == 0:
                    env = {**os.environ, "GIT_INDEX_FILE": str(run / "baseline-index")}
                    if git(repo, "ls-files", "--stage", "-z", env=env) != (trial / "before-index-entries").read_bytes():
                        failures.append("baseline cannot recover original staging")
                        violations.append("lost_staging_recovery")
                env = {**os.environ, "GIT_INDEX_FILE": str(trial / f"audit-index-{index}")}
                git(repo, "read-tree", snap["tree"], env=env)
                patch = run / "diff.patch"
                if patch.stat().st_size:
                    git(repo, "-c", "core.autocrlf=false", "apply", "--cached", "--binary", str(patch), env=env)
                post_tree = git(repo, "write-tree", env=env).decode().strip()
                previous = tree_state(repo, post_tree)
                actual = set(filter(None, git(repo, "diff", "--name-only", "-z", snap["tree"], post_tree).decode().split("\0")))
                if actual != set(result["files_changed"]) or actual - set(module.CASES[entry["case"]][1]):
                    failures.append("task delta scope/attribution mismatch")
                    violations.append("incorrect_task_delta")
            if previous != read(trial / "after.json"):
                failures.append("patch replay differs from exact final state")
                violations.append("incorrect_patch_replay")
    except Exception as error:
        failures.append(f"acceptance audit failed: {type(error).__name__}: {error}")
    # An observed success claim contradicted by acceptance evidence is a false
    # completion. Missing cost alone is unknown spend, not false completion.
    reported_pass = any(re.search(r"(?mi)^\s*-?\s*verdict:\s*pass\s*$", text) for text in reviews)
    if reported_pass and failures:
        violations.append("false_completion")
    if not score.get("sessions") or score.get("cost_issues"):
        failures.append("complete attempt/session coverage unproven")
    score["accepted"] = not failures
    return score



def aggregate(results, summary):
    for arm in ["direct", "plugin"]:
        selected = [v for k, v in results.items() if k.endswith(arm)]
        accepted = [v for v in selected if v["accepted"] and nonnegative(v.get('human_code_repair_minutes')) and v['human_code_repair_minutes'] == 0]
        cost_complete = all(v["cost_complete"] for v in selected)
        latencies = sorted(v['seconds'] for v in accepted)
        observed = [v for v in selected if v.get('seconds') is not None]
        repairs = [v.get('human_code_repair_minutes') for v in observed]
        summary["arms"][arm] = {"accepted": len(accepted), "total": len(selected), "cost_complete": cost_complete,
            "p95_seconds_accepted_nearest_rank": latencies[math.ceil(.95 * len(latencies)) - 1] if latencies else None,
            "max_seconds_accepted": max(latencies) if latencies else None,
            "max_seconds_all_observed": max((v['seconds'] for v in observed), default=None),
            "observed_human_code_repair_minutes": sum(repairs) if repairs and all(nonnegative(v) for v in repairs) else None,
            "known_token_totals_including_failures": {
                provider: {key: sum(v.get(provider, {}).get(key, 0) for v in selected)
                           for key in sorted({key for v in selected for key in v.get(provider, {})})}
                for provider in ['codex_tokens', 'claude_tokens']},
            "median_seconds_accepted": statistics.median(v["seconds"] for v in accepted) if accepted else None,
            "median_cost_accepted": statistics.median(v["total_cost_equivalent_usd"] for v in accepted) if accepted and cost_complete else None,
            "cost_per_accepted_including_failures": sum(v["total_cost_equivalent_usd"] for v in selected) / len(accepted) if accepted and cost_complete else None,
            "known_cost_lower_bound": sum(v.get("known_cost_equivalent_usd") or 0 for v in selected),
            "failures": {k: v["failures"] for k,v in results.items() if k.endswith(arm) and v["failures"]}}
    direct, plugin = summary["arms"]["direct"], summary["arms"]["plugin"]
    latency = plugin["median_seconds_accepted"] / direct["median_seconds_accepted"] if direct["median_seconds_accepted"] and plugin["median_seconds_accepted"] else None
    cost = plugin["cost_per_accepted_including_failures"] / direct["cost_per_accepted_including_failures"] if direct["cost_per_accepted_including_failures"] and plugin["cost_per_accepted_including_failures"] else None
    median_cost = plugin["median_cost_accepted"] / direct["median_cost_accepted"] if direct["median_cost_accepted"] and plugin["median_cost_accepted"] else None
    critical = [name for name, result in results.items() if result.get("safety_violations")]
    summary["gates"] = {"complete_frozen_cohort": summary["full_cohort"], "accepted_count": plugin["accepted"] >= 27 and plugin["accepted"] >= direct["accepted"],
                        "zero_critical": not critical, "latency_ratio": latency, "latency_pass": latency is not None and latency <= 1.25,
                        "cost_ratio": cost, "median_cost_ratio": median_cost, "cost_pass": cost is not None and cost <= 1.25, "critical_rows": critical}
    summary["paired_go"] = all(summary["gates"][key] for key in ["complete_frozen_cohort", "accepted_count", "zero_critical", "latency_pass", "cost_pass"])
    return summary

def main():
    out, home = map(lambda p: Path(p).resolve(), sys.argv[1:3])
    spec = importlib.util.spec_from_file_location("frozen_driver", out / "driver.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    manifest = read(out / "manifest.json")
    inventory = codex_inventory(home)
    rates = manifest.get("rates_per_million", {})
    if set(rates) != {"input", "cached_input", "output"} or not all(nonnegative(v) for v in rates.values()):
        inventory.errors.append("invalid frozen pricing rates")
        rates = {"input": 0, "cached_input": 0, "output": 0}
    results = {}
    summary_errors = list(inventory.errors)
    for entry in manifest["entries"]:
        trial = out / entry["name"]
        if not (trial / "started.json").exists() and not (trial / "outcome.json").exists():
            score = {"accepted": False, "failures": ["not executed"], "seconds": None, "cost_complete": False, "total_cost_equivalent_usd": None, "known_cost_equivalent_usd": 0}
        else:
            score = audit_trial(out, entry, inventory, module, rates)
        (trial / "audit.json").write_text(json.dumps(score, indent=2) + "\n")
        results[entry["name"]] = score
    expected = {f"C{case:02}-{rep}-{arm}" for case in range(1, 7) for rep in range(1, 6) for arm in ["direct", "plugin"]}
    canonical = len(manifest["entries"]) == 60 and set(results) == expected and all(entry["name"].split("-")[0] == entry["case"] and entry["name"].split("-")[2] == entry["arm"] for entry in manifest["entries"])
    kind = read(out / "measurement-kind.json") if (out / "measurement-kind.json").exists() else {}
    completed = all((out / name / "outcome.json").exists() for name in expected)
    source_frozen = True
    import hashlib
    for name, digest in read(out / "frozen-sha256.json").items():
        # Repository inputs are deliberately changed by model execution. Their
        # before.json and index snapshots preserve frozen originals instead.
        if "/repo/" in name:
            continue
        if hashlib.sha256((out / name).read_bytes()).hexdigest() != digest:
            source_frozen = False
    summary = {"audit_errors": summary_errors, "planned": 60, "completed": sum((out / name / "outcome.json").exists() for name in expected),
               "full_cohort": canonical and completed and source_frozen and kind == {"kind": "full", "trials": 60}, "arms": {}}
    summary = aggregate(results, summary)
    (out / "audit-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
