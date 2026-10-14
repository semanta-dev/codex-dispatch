#!/usr/bin/env python3
"""Fail-closed promotion evidence validator, not a test or review generator.

The record must be independently reviewed and its SHA256 approved outside the
candidate checkout (release-promotion environment secret). Its artifacts must
come from the successful same-candidate trusted CI run checked by release.yml.
Local synthetic records exercise validation only and do not authorize release.
"""
import argparse
import hashlib
import importlib.util
import json
import math
import os
import re
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
PLATFORMS = {f"{os_name}-{arch}" for os_name in ("linux", "darwin", "windows") for arch in ("amd64", "arm64")}
CHECKS = {f"F{n:02}" for n in range(1, 13)} | {"positive-control", "native-lifecycle"}
FIXTURES = {"fail-approach", "fail-criterion", "fail-quality", "fail-scope", "fail-tests", "pass-binary-deletion", "pass-mode-only", "pass-no-tests-flag", "pass-simple"}
ROWS = {f"C{case:02}-{rep}-{arm}" for case in range(1, 7) for rep in range(1, 6) for arm in ("direct", "plugin")}


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


def finite(value):
    return type(value) in (int, float) and math.isfinite(value) and value >= 0


def parse_json(text):
    def unique(pairs):
        value = {}
        for key, item in pairs:
            require(key not in value, f"duplicate JSON key: {key}")
            value[key] = item
        return value
    return json.loads(text, object_pairs_hook=unique)


def read_json(path):
    return parse_json(path.read_text())


def reviewer_contract():
    spec = importlib.util.spec_from_file_location("promotion_reviewer", ROOT / "scripts/api-review.py")
    reviewer = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(reviewer)
    return reviewer


def profile_binding(reviewer):
    return {"model": reviewer.MODEL,
            "system_sha256": hashlib.sha256(reviewer.system_prompt().encode()).hexdigest(),
            "schema_sha256": hashlib.sha256(json.dumps(reviewer.SCHEMA, sort_keys=True, separators=(",", ":")).encode()).hexdigest()}


def canonical_fixture_payload(name):
    """Use the fixture harness pure payload builder without executing commands.

    The committed corpus has one executable test case: `false` with no output.
    Unknown future fixture commands fail closed until their expected evidence is
    specified here. The promotion validator never runs repository test commands.
    """
    fixture = ROOT / "tests/fixtures/reviewer" / name
    def read(filename, default=""):
        path = fixture / filename
        return path.read_text().strip() if path.exists() else default
    test_policy, command = read("test-policy.txt", "skip"), read("test-cmd.txt")
    test = None
    if test_policy != "skip" and command:
        require(command == "false", "fixture test evidence has no frozen expectation")
        test = {"command": "false", "exit_code": 1, "stdout": "", "stderr": ""}
    spec = importlib.util.spec_from_file_location("promotion_fixture_harness", ROOT / "tests/reviewer/direct-fixtures.py")
    harness = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(harness)
    return harness.fixture_payload(fixture, test)


def validate(record_path, policy_path, candidate, source_tree, approved_digest, trusted_run):
    require(bool(approved_digest) and digest(record_path) == approved_digest, "missing or mismatched independent record approval")
    policy, record = read_json(policy_path), read_json(record_path)
    require(policy.get("schema_version") == 1 and record.get("schema_version") == 1, "unsupported schema")
    require(set(policy["platforms"]) == PLATFORMS and len(policy["platforms"]) == 6, "policy platform inventory weakened")
    require(set(policy["checks"]) == CHECKS, "policy check inventory weakened")
    require(set(policy["fixtures"]) == FIXTURES and len(policy["fixtures"]) == 9, "policy fixture inventory weakened")
    require(policy["minimum_fixture_score"] == 8 and policy.get("fixture_observations") == 10 and policy["paired_trials_per_arm"] == 30 and policy["minimum_plugin_accepted"] == 27
            and policy["maximum_latency_ratio"] == 1.25 and policy["maximum_cost_ratio"] == 1.25, "policy benchmark thresholds changed")
    require(record.get("candidate_commit") == candidate and record.get("product_source_tree") == source_tree, "candidate/source mismatch")
    require(record.get("policy_sha256") == digest(policy_path), "policy digest mismatch")
    require(bool(trusted_run) and str(record.get("trusted_ci_run_id")) == str(trusted_run), "trusted CI origin mismatch")
    artifacts = record.get("artifacts", {})
    require(bool(artifacts), "missing artifact inventory")
    root = record_path.parent.resolve()
    for name, expected in artifacts.items():
        path = root / name
        require(not Path(name).is_absolute() and ".." not in Path(name).parts and path.resolve().is_relative_to(root), "unsafe artifact path")
        require(path.is_file() and not path.is_symlink() and path.stat().st_size > 0, f"missing/empty artifact: {name}")
        require(digest(path) == expected, f"artifact digest mismatch: {name}")

    # Publish only these six approved archives and their verified checksum file.
    assets = {platform: f"assets/codex-dispatch_{platform}.{'zip' if platform.startswith('windows-') else 'tar.gz'}" for platform in PLATFORMS}
    require(set(assets.values()) | {"assets/checksums.txt"} <= set(artifacts), "release asset inventory incomplete")
    checksums = (root / "assets/checksums.txt").read_text().splitlines()
    require(len(checksums) == 6, "release checksums inventory incomplete")
    expected_checksums = {f"{artifacts[name]}  {Path(name).name}" for name in assets.values()}
    require(set(checksums) == expected_checksums, "release checksums mismatch")

    def evidence(item, label):
        refs = item.get("evidence", [])
        require(isinstance(refs, list) and bool(refs) and all(ref in artifacts for ref in refs), f"missing artifact references: {label}")

    matrix = record.get("platforms", {})
    require(set(matrix) == PLATFORMS, "incomplete platform inventory")
    for platform, entry in matrix.items():
        require(entry.get("execution") == "native", f"non-native platform: {platform}")
        require(entry.get("candidate_commit") == candidate, f"stale platform: {platform}")
        require(entry.get("artifact") == assets[platform], f"missing product artifact: {platform}")
        require(set(entry.get("checks", {})) == CHECKS, f"incomplete checks: {platform}")
        for check, result in entry["checks"].items():
            require(result.get("status") == "pass", f"{platform}/{check} did not pass")
            evidence(result, f"{platform}/{check}")
    reviewers = record.get("reviews", {})
    require(set(reviewers) == {"cto", "10x-engineer"}, "missing independent reviewers")
    identities = set()
    for role, review in reviewers.items():
        require(review.get("verdict") == "GO" and review.get("blockers") == [] and review.get("candidate_commit") == candidate, f"{role} did not approve candidate")
        require(review.get("reviewer") and review.get("reviewer") != record.get("implementer"), "reviewer is implementer or absent")
        identities.add(review["reviewer"])
        evidence(review, role)
    require(record.get("implementer") and len(identities) == 2, "reviewers not independent")
    fixtures = record.get("fixtures", {})
    require(set(fixtures) == FIXTURES, "incomplete reviewer fixtures")
    reviewer = reviewer_contract()
    binding = profile_binding(reviewer)
    require(record.get("deployed_reviewer") == binding, "deployed reviewer binding mismatch")
    request_inventory, response_inventory, response_ids, fixture_refs = set(), set(), set(), set()
    for name, fixture in fixtures.items():
        require(fixture.get("status") == "pass" and type(fixture.get("score")) is int and 8 <= fixture["score"] <= 10, f"fixture failed: {name}")
        require(type(fixture.get("runs")) is int and fixture["runs"] == 10, f"fixture must retain exactly ten runs: {name}")
        observations = fixture.get("observations", [])
        require(isinstance(observations, list) and len(observations) == 10, f"fixture observations missing: {name}")
        require(all(type(item.get("run")) is int for item in observations) and {item["run"] for item in observations} == set(range(1, 11)), f"fixture run inventory invalid: {name}")
        expected_text = (ROOT / "tests/fixtures/reviewer" / name / "expected_verdict.txt").read_text()
        expected = re.search(r"(?m)^VERDICT: (pass|needs-changes|fail)$", expected_text).group(1)
        expected_reason = re.search(r"(?m)^REASON:[ \t]*(.*)$", expected_text).group(1).strip()
        matches = 0
        for observation in observations:
            require(observation.get("reviewer") == binding, f"fixture reviewer profile mismatch: {name}")
            prefix = f"fixtures/{name}/{observation['run']}/"
            request_name = observation.get("request_artifact")
            require(request_name == prefix + "request.json", f"fixture request attribution mismatch: {name}")
            require(request_name in artifacts and request_name not in request_inventory, f"fixture request missing or reused: {name}")
            request_inventory.add(request_name)
            fixture_refs.add(request_name)
            request = read_json(root / request_name)
            require(set(request) == {"model", "max_tokens", "thinking", "system", "messages", "tools", "tool_choice"}, f"fixture request contract fields differ: {name}")
            messages = request.get("messages")
            require(isinstance(messages, list) and len(messages) == 1 and isinstance(messages[0], dict)
                    and set(messages[0]) == {"role", "content"} and messages[0]["role"] == "user"
                    and isinstance(messages[0]["content"], str), f"fixture request messages invalid: {name}")
            actual_payload = parse_json(messages[0]["content"])
            require(json.dumps(actual_payload, sort_keys=True, allow_nan=False) == json.dumps(canonical_fixture_payload(name), sort_keys=True, allow_nan=False),
                    f"fixture request input differs from frozen task/evidence: {name}")
            require(request.get("model") == reviewer.MODEL and request.get("system") == reviewer.system_prompt(), f"fixture request model/system mismatch: {name}")
            require(request.get("tools") == [{"name": "review_result", "description": "Submit the independent code review decision.", "input_schema": reviewer.SCHEMA}], f"fixture schema mismatch: {name}")
            require(request.get("max_tokens") == 1024 and request.get("thinking") == {"type": "disabled"} and request.get("tool_choice") == {"type": "tool", "name": "review_result", "disable_parallel_tool_use": True}, f"fixture request settings mismatch: {name}")
            evidence(observation, f"{name}/{observation['run']}")
            require(all(ref.startswith(prefix) for ref in observation["evidence"]), f"fixture evidence attribution mismatch: {name}")
            fixture_refs.update(observation["evidence"])
            require(observation.get("status") in {"complete", "error"}, f"fixture outcome missing: {name}")
            if observation["status"] == "error":
                require(observation.get("response_artifact") is None, f"failed fixture response must be retained as diagnostic evidence: {name}")
                continue
            response_name = observation.get("response_artifact")
            require(response_name == prefix + "response.json", f"fixture response attribution mismatch: {name}")
            require(response_name in artifacts and response_name not in response_inventory, f"fixture response missing or reused: {name}")
            response_inventory.add(response_name)
            fixture_refs.add(response_name)
            response = read_json(root / response_name)
            # Malformed/error decisions count as observations but never matches.
            try:
                require(response.get("type") == "message" and response.get("model") == reviewer.MODEL and response.get("id") and response.get("stop_reason") == "tool_use", "incomplete reviewer response")
                require(response["id"] not in response_ids, "reused API response id")
                response_ids.add(response["id"])
                content = response["content"]
                require(isinstance(content, list) and len(content) == 1 and content[0].get("type") == "tool_use" and content[0].get("name") == "review_result", "invalid reviewer tool")
                decision = reviewer.decision(content[0].get("input"))
                matches += int(decision["verdict"] == expected and decision["reason"] == expected_reason)
            except (ValueError, TypeError, KeyError):
                pass
        require(matches == fixture["score"], f"fixture matched count differs from retained observations: {name}")
        evidence(fixture, name)
    require({name for name in artifacts if name.startswith("fixtures/")} <= fixture_refs, "unaccounted fixture evidence or dropped attempts")
    benchmark = record.get("benchmark", {})
    require(benchmark.get("candidate_commit") == candidate, "stale benchmark")
    require(benchmark.get("rows_artifact") in artifacts, "benchmark raw audit rows missing")
    evidence(benchmark, "benchmark source transcripts and audit")
    rows = read_json(root / benchmark["rows_artifact"])
    require(set(rows) == ROWS, "incomplete paired 30+30 cohort")
    for name, row in rows.items():
        require(type(row.get("accepted")) is bool and row.get("cost_complete") is True, f"incomplete trial: {name}")
        require(isinstance(row.get("failures"), list) and isinstance(row.get("safety_violations"), list), f"missing trial failure inventory: {name}")
        require(not row["accepted"] or not row["failures"], f"accepted trial contains audit failures: {name}")
        for key in ("seconds", "total_cost_equivalent_usd", "human_code_repair_minutes"):
            require(finite(row.get(key)), f"invalid {key}: {name}")
    spec = importlib.util.spec_from_file_location("promotion_benchmark_audit", ROOT / "tests/benchmark/audit.py")
    audit = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(audit)
    summary = audit.aggregate(rows, {"full_cohort": True, "arms": {}})
    require(summary["paired_go"], "paired benchmark gate rejected")
    return {"decision": "GO", "candidate_commit": candidate, "policy_sha256": digest(policy_path), "record_sha256": digest(record_path)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("record", type=Path)
    parser.add_argument("--policy", type=Path, default=ROOT / "config/promotion-policy.json")
    args = parser.parse_args()
    try:
        candidate = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        tree = subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], cwd=ROOT, text=True).strip()
        require(not subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=no"], cwd=ROOT), "tracked checkout differs from candidate")
        print(json.dumps(validate(args.record.resolve(), args.policy.resolve(), candidate, tree,
                                  os.environ.get("APPROVED_PROMOTION_RECORD_SHA256"), os.environ.get("TRUSTED_PROMOTION_RUN_ID"))))
        return 0
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
        print(json.dumps({"decision": "NO-GO", "error": str(error)}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
