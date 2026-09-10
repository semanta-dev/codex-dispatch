"""Synthetic validator unit tests. These fixtures are never release evidence."""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("promotion_gate", ROOT / "scripts/promotion-gate.py")
GATE = importlib.util.module_from_spec(spec)
spec.loader.exec_module(GATE)


class PromotionGateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.policy = ROOT / "config/promotion-policy.json"
        self.record_path = self.root / "promotion-record.json"
        self.artifacts = {}
        self.add_artifact("logs/test.log", "SYNTHETIC UNIT TEST, NOT PROMOTION PROOF\n")
        assets = {}
        for platform in GATE.PLATFORMS:
            name = f"assets/codex-dispatch_{platform}.{'zip' if platform.startswith('windows-') else 'tar.gz'}"
            assets[platform] = name
            self.add_artifact(name, "SYNTHETIC ARCHIVE " + platform)
        self.add_artifact("assets/checksums.txt", "\n".join(f"{self.artifacts[name]}  {Path(name).name}" for name in assets.values()) + "\n")
        self.rows = {name: {"accepted": True, "failures": [], "safety_violations": [], "seconds": 10,
                           "cost_complete": True, "total_cost_equivalent_usd": 1, "known_cost_equivalent_usd": 1,
                           "human_code_repair_minutes": 0} for name in GATE.ROWS}
        self.add_artifact("benchmark/rows.json", json.dumps(self.rows))
        proof = {"evidence": ["logs/test.log"]}
        self.record = {"schema_version": 1, "candidate_commit": "test-candidate", "product_source_tree": "test-tree",
                       "policy_sha256": GATE.digest(self.policy), "trusted_ci_run_id": "123", "implementer": "test-builder",
                       "artifacts": self.artifacts,
                       "platforms": {platform: {"candidate_commit": "test-candidate", "execution": "native", "artifact": assets[platform],
                                                "checks": {check: {"status": "pass", **proof} for check in GATE.CHECKS}} for platform in GATE.PLATFORMS},
                       "reviews": {role: {"reviewer": "test-" + role, "verdict": "GO", "blockers": [], "candidate_commit": "test-candidate", **proof} for role in ("cto", "10x-engineer")},
                       "fixtures": {name: {"status": "pass", "score": 8, **proof} for name in GATE.FIXTURES},
                       "benchmark": {"candidate_commit": "test-candidate", "rows_artifact": "benchmark/rows.json", **proof}}

        reviewer = GATE.reviewer_contract()
        binding = GATE.profile_binding(reviewer)
        self.record["deployed_reviewer"] = binding
        for name, fixture in self.record["fixtures"].items():
            expected_text = (ROOT / "tests/fixtures/reviewer" / name / "expected_verdict.txt").read_text()
            expected = GATE.re.search(r"(?m)^VERDICT: (pass|needs-changes|fail)$", expected_text).group(1)
            expected_reason = GATE.re.search(r"(?m)^REASON:[ \t]*(.*)$", expected_text).group(1).strip()
            fixture.update(runs=10, observations=[])
            for number in range(1, 11):
                request_name, response_name = f"fixtures/{name}/{number}/request.json", f"fixtures/{name}/{number}/response.json"
                self.add_artifact(request_name, json.dumps({"model": reviewer.MODEL, "system": reviewer.system_prompt(),
                    "messages": [{"role": "user", "content": json.dumps(GATE.canonical_fixture_payload(name))}],
                    "tools": [{"name": "review_result", "description": "Submit the independent code review decision.", "input_schema": reviewer.SCHEMA}],
                    "max_tokens": 1024, "thinking": {"type": "disabled"},
                    "tool_choice": {"type": "tool", "name": "review_result", "disable_parallel_tool_use": True}}))
                verdict = expected if number <= 8 else ("fail" if expected == "pass" else "pass")
                self.add_artifact(response_name, json.dumps({"type": "message", "model": reviewer.MODEL, "id": f"synthetic-{name}-{number}", "stop_reason": "tool_use",
                    "content": [{"type": "tool_use", "name": "review_result", "input": {"verdict": verdict, "reason": expected_reason if number <= 8 else ("" if verdict == "pass" else "synthetic reason"), "feedback": []}}]}))
                fixture["observations"].append({"run": number, "reviewer": binding, "status": "complete", "request_artifact": request_name,
                                              "response_artifact": response_name, "evidence": [request_name, response_name]})

    def add_artifact(self, name, contents):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(contents)
        self.artifacts[name] = GATE.digest(path)

    def validate(self, approval=True):
        self.record_path.write_text(json.dumps(self.record))
        return GATE.validate(self.record_path, self.policy, "test-candidate", "test-tree",
                             GATE.digest(self.record_path) if approval else None, "123")

    def reject(self):
        with self.assertRaises((ValueError, KeyError)):
            self.validate()

    def test_complete_synthetic_contract_validates_only_with_external_digest(self):
        self.assertEqual(self.validate()["decision"], "GO")
        with self.assertRaisesRegex(ValueError, "independent record approval"):
            self.validate(approval=False)

    def test_fixture_requires_integer_count_and_exactly_ten_runs(self):
        fixture = self.record["fixtures"]["fail-tests"]
        for score, runs in [(8.5, 10), (8, 1), (8, None), (True, 10)]:
            with self.subTest(score=score, runs=runs):
                fixture.update(score=score, runs=runs)
                self.reject()

    def test_dropped_failures_and_inflated_count_rejected(self):
        fixture = self.record["fixtures"]["fail-tests"]
        fixture["score"] = 10
        self.reject()
        fixture["score"] = 8
        fixture["observations"].pop()
        self.reject()

    def test_wrong_deployed_model_system_or_schema_rejected(self):
        observation = self.record["fixtures"]["fail-tests"]["observations"][0]
        name = observation["request_artifact"]
        original = json.loads((self.root / name).read_text())
        for field in ("model", "system", "tools"):
            with self.subTest(field=field):
                request = copy.deepcopy(original)
                request[field] = "wrong"
                self.add_artifact(name, json.dumps(request))
                self.reject()

    def test_error_observation_is_retained_as_nonmatch(self):
        observation = self.record["fixtures"]["fail-tests"]["observations"][-1]
        observation.update(status="error", response_artifact=None)
        self.assertEqual(self.validate()["decision"], "GO")

    def test_unrelated_messages_fail_even_with_new_artifact_and_approval_digests(self):
        for fixture in self.record["fixtures"].values():
            for observation in fixture["observations"]:
                path = observation["request_artifact"]
                request = json.loads((self.root / path).read_text())
                request["messages"] = [{"role": "user", "content": "unrelated content"}]
                self.add_artifact(path, json.dumps(request))
        self.reject()

    def test_each_fixture_input_component_is_bound(self):
        path = self.record["fixtures"]["fail-tests"]["observations"][0]["request_artifact"]
        original = json.loads((self.root / path).read_text())
        for component in ("TASK", "ACCEPTANCE", "CONSTRAINTS", "result", "bundle", "TEST_CMD", "TEST_POLICY", "CLEAN_VERIFY"):
            with self.subTest(component=component):
                request = copy.deepcopy(original)
                payload = json.loads(request["messages"][0]["content"])
                payload[component] = "substituted"
                request["messages"][0]["content"] = json.dumps(payload)
                self.add_artifact(path, json.dumps(request))
                self.reject()
        for component in ("diff", "test", "result", "changed_file_facts", "verification"):
            with self.subTest(bundle_component=component):
                request = copy.deepcopy(original)
                payload = json.loads(request["messages"][0]["content"])
                payload["bundle"][component] = "substituted"
                request["messages"][0]["content"] = json.dumps(payload)
                self.add_artifact(path, json.dumps(request))
                self.reject()

    def test_promotion_payload_validation_never_executes_fixture_commands(self):
        with patch("subprocess.run", side_effect=AssertionError("host execution forbidden")):
            for name in GATE.FIXTURES:
                GATE.canonical_fixture_payload(name)
            self.assertEqual(self.validate()["decision"], "GO")

    def test_canonical_payload_matches_actual_committed_fixture_harness(self):
        spec = importlib.util.spec_from_file_location("fixture_harness", ROOT / "tests/reviewer/direct-fixtures.py")
        harness = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(harness)
        for name in GATE.FIXTURES:
            self.assertEqual(GATE.canonical_fixture_payload(name), harness.fixture_input(ROOT / "tests/fixtures/reviewer" / name, self.root))

    def test_extra_failed_attempt_cannot_be_dropped_from_inventory(self):
        self.add_artifact("fixtures/fail-tests/11/error.log", "extra failed attempt")
        self.reject()

    def test_reused_observation_artifacts_rejected(self):
        observations = self.record["fixtures"]["fail-tests"]["observations"]
        observations[1]["request_artifact"] = observations[0]["request_artifact"]
        self.reject()

    def test_missing_native_platform_rejected(self):
        del self.record["platforms"]["windows-arm64"]
        self.reject()

    def test_every_nonpass_status_rejected(self):
        for status in ("fail", "unsupported", "error", "skipped", None):
            with self.subTest(status=status):
                self.record["platforms"]["linux-amd64"]["checks"]["F01"]["status"] = status
                self.reject()

    def test_missing_positive_control_rejected(self):
        del self.record["platforms"]["darwin-amd64"]["checks"]["positive-control"]
        self.reject()

    def test_emulated_native_lifecycle_rejected(self):
        self.record["platforms"]["windows-arm64"]["execution"] = "cross-compiled"
        self.reject()

    def test_stale_candidate_source_policy_or_run_rejected(self):
        original = copy.deepcopy(self.record)
        for field in ("candidate_commit", "product_source_tree", "policy_sha256", "trusted_ci_run_id"):
            with self.subTest(field=field):
                self.record = copy.deepcopy(original)
                self.record[field] = "stale"
                self.reject()

    def test_artifact_tampering_rejected(self):
        (self.root / "logs/test.log").write_text("tampered")
        self.reject()

    def test_missing_proof_rejected(self):
        self.record["reviews"]["cto"]["evidence"] = []
        self.reject()

    def test_self_review_or_shared_reviewer_rejected(self):
        self.record["reviews"]["cto"]["reviewer"] = "test-builder"
        self.reject()
        self.record["reviews"]["cto"]["reviewer"] = "test-10x-engineer"
        self.reject()

    def test_cto_blocker_vetoes_all_passes(self):
        self.record["reviews"]["cto"]["blockers"] = ["still unsafe"]
        self.reject()

    def test_missing_fixture_and_low_score_rejected(self):
        self.record["fixtures"]["fail-tests"]["score"] = 7
        self.reject()
        del self.record["fixtures"]["fail-tests"]
        self.reject()

    def test_incomplete_paired_cohort_rejected(self):
        self.rows.pop(next(iter(self.rows)))
        self.add_artifact("benchmark/rows.json", json.dumps(self.rows))
        self.reject()

    def test_existing_benchmark_critical_gate_reused(self):
        self.rows["C01-1-plugin"]["safety_violations"] = ["false_completion"]
        self.add_artifact("benchmark/rows.json", json.dumps(self.rows))
        self.reject()

    def test_cost_ratio_and_nonfinite_values_rejected(self):
        for value in (1.26, float("nan"), float("inf"), True):
            with self.subTest(value=value):
                for name, row in self.rows.items():
                    if name.endswith("plugin"):
                        row["total_cost_equivalent_usd"] = value
                self.add_artifact("benchmark/rows.json", json.dumps(self.rows))
                self.reject()

    def test_path_escape_rejected(self):
        self.artifacts["../outside"] = "bad"
        self.reject()

    def test_checksums_must_match_published_archives(self):
        self.add_artifact("assets/checksums.txt", "wrong\n")
        self.reject()

    def test_weakened_policy_inventory_rejected(self):
        policy = json.loads(self.policy.read_text())
        policy["platforms"].remove("windows-arm64")
        self.policy = self.root / "policy.json"
        self.policy.write_text(json.dumps(policy))
        self.record["policy_sha256"] = GATE.digest(self.policy)
        self.reject()

    def test_duplicate_json_keys_rejected(self):
        self.record_path.write_text('{"schema_version": 1, "schema_version": 1}')
        with self.assertRaisesRegex(ValueError, "duplicate JSON key"):
            GATE.validate(self.record_path, self.policy, "test-candidate", "test-tree", GATE.digest(self.record_path), "123")


if __name__ == "__main__":
    unittest.main()
