import importlib.util
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("benchmark_audit", Path(__file__).resolve().parents[1] / "benchmark/audit.py")
AUDIT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(AUDIT)


class BenchmarkGateTests(unittest.TestCase):
    def cohort(self):
        return {f"C{case:02}-{rep}-{arm}": {"accepted": True, "failures": [], "safety_violations": [], "seconds": 10,
                "cost_complete": True, "total_cost_equivalent_usd": 1, "known_cost_equivalent_usd": 1, "human_code_repair_minutes": 0}
                for case in range(1, 7) for rep in range(1, 6) for arm in ["direct", "plugin"]}

    def score(self, results, full=True):
        return AUDIT.aggregate(results, {"full_cohort": full, "arms": {}})

    def test_unknown_failed_attempt_cost_never_passes_cost_gate(self):
        rows = self.cohort()
        for n in range(1, 4):
            rows[f"C01-{n}-plugin"].update(accepted=False, failures=["honest rejection"])
            rows[f"C01-{n}-direct"].update(accepted=False, failures=["honest rejection"])
        rows["C01-1-plugin"].update(cost_complete=False, total_cost_equivalent_usd=None)
        result = self.score(rows)
        self.assertTrue(result["gates"]["accepted_count"])
        self.assertIsNone(result["gates"]["cost_ratio"])
        self.assertFalse(result["paired_go"])

    def test_one_false_completion_vetoes_twenty_nine_good_rows(self):
        rows = self.cohort()
        rows["C01-1-plugin"].update(accepted=False, failures=["unsupported pass"], safety_violations=["false_completion"])
        result = self.score(rows)
        self.assertFalse(result["gates"]["zero_critical"])
        self.assertFalse(result["paired_go"])

    def test_honest_rejections_keep_all_spend_without_becoming_safety_violations(self):
        rows = self.cohort()
        for arm in ["direct", "plugin"]:
            for n in range(1, 4):
                rows[f"C01-{n}-{arm}"].update(accepted=False, failures=["honest in-scope rejection"])
        result = self.score(rows)
        self.assertTrue(result["gates"]["zero_critical"])
        self.assertAlmostEqual(result["arms"]["plugin"]["cost_per_accepted_including_failures"], 30 / 27)
        self.assertTrue(result["paired_go"])

    def test_incomplete_cohort_marker_never_promotes(self):
        self.assertFalse(self.score(self.cohort(), full=False)["paired_go"])

    def test_human_repaired_or_unknown_rows_do_not_count_as_accepted(self):
        rows = self.cohort()
        rows['C01-1-plugin']['human_code_repair_minutes'] = 2
        del rows['C01-2-plugin']['human_code_repair_minutes']
        result = self.score(rows)
        self.assertEqual(result['arms']['plugin']['accepted'], 28)
        self.assertAlmostEqual(result['arms']['plugin']['cost_per_accepted_including_failures'], 30 / 28)
        self.assertFalse(result['gates']['accepted_count'])

    def test_tail_tokens_and_human_repairs_include_rejected_attempts(self):
        rows = self.cohort()
        for row in rows.values():
            row.update(codex_tokens={'output_tokens': 2}, claude_tokens={'outputTokens': 3}, human_code_repair_minutes=0)
        rows['C01-1-plugin'].update(accepted=False, seconds=100, human_code_repair_minutes=4)
        rows['C01-2-plugin']['seconds'] = 20
        rows['C01-3-plugin']['seconds'] = 30
        arm = self.score(rows)['arms']['plugin']
        self.assertEqual(arm['p95_seconds_accepted_nearest_rank'], 20)
        self.assertEqual(arm['max_seconds_accepted'], 30)
        self.assertEqual(arm['max_seconds_all_observed'], 100)
        self.assertEqual(arm['known_token_totals_including_failures']['codex_tokens']['output_tokens'], 60)
        self.assertEqual(arm['known_token_totals_including_failures']['claude_tokens']['outputTokens'], 90)
        self.assertEqual(arm['observed_human_code_repair_minutes'], 4)
        del rows['C01-1-plugin']['human_code_repair_minutes']
        self.assertIsNone(self.score(rows)['arms']['plugin']['observed_human_code_repair_minutes'])

    def test_median_cost_is_diagnostic_total_spend_is_gate(self):
        rows = self.cohort()
        for n, (key, value) in enumerate((k, v) for k, v in rows.items() if k.endswith("plugin")):
            value["total_cost_equivalent_usd"] = 1.5 if n < 16 else 0.1
        result = self.score(rows)
        self.assertGreater(result["gates"]["median_cost_ratio"], 1.25)
        self.assertTrue(result["gates"]["cost_pass"])

    def test_command_route_needs_invocation_and_exact_expanded_contract(self):
        body = "trusted contract\n"
        digest = hashlib.sha256(body.encode()).hexdigest()
        invocation = {"type": "user", "message": {"content": "<command-name>/codex-dispatch:codex</command-name>"}}
        expanded = {"type": "user", "message": {"content": [{"type": "text", "text": "The implementation contract (trusted plugin instructions):\n\n" + body}]}}
        self.assertTrue(AUDIT.command_loaded([invocation, expanded], digest))
        self.assertFalse(AUDIT.command_loaded([invocation], digest))
        self.assertFalse(AUDIT.command_loaded([expanded], digest))
        self.assertFalse(AUDIT.command_loaded([invocation, expanded], "wrong digest"))

    def test_invalid_numeric_usage_is_rejected(self):
        self.assertFalse(AUDIT.nonnegative(float("nan")))
        self.assertFalse(AUDIT.nonnegative(float("inf")))
        self.assertFalse(AUDIT.nonnegative(-1))
        self.assertFalse(AUDIT.nonnegative(True))
        for value in [dict(input_tokens=-1, cached_input_tokens=0, output_tokens=1),
                      dict(input_tokens=1, cached_input_tokens=2, output_tokens=1),
                      dict(input_tokens=True, cached_input_tokens=0, output_tokens=1)]:
            self.assertFalse(AUDIT.valid_tokens(value))

    def test_truncated_transcript_is_recorded_as_unknown_not_dropped(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / "sessions").mkdir()
            (root / "sessions/broken.jsonl").write_text('{"type":')
            inventory = AUDIT.codex_inventory(root)
            self.assertTrue(inventory.errors)
            self.assertFalse(inventory)

    def test_failed_api_report_retains_known_spend_and_unknown_remainder(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            reviews = root / 'repo/.codex-dispatch/expansions/session/prompt.reviews'
            for n in [1, 2, 3]:
                (reviews / str(n)).mkdir(parents=True)
            for n in [1, 2]:
                # Even a truncated/error decision has billable valid usage.
                (reviews / str(n) / 'response.json').write_text(json.dumps({'id': f'msg{n}', 'model': 'haiku', 'stop_reason': 'max_tokens', 'usage': {'input_tokens': 100, 'output_tokens': 20}}))
            cost, tokens, issues = AUDIT.api_usage_inventory(root, 'haiku')
            self.assertAlmostEqual(cost, .0004)
            self.assertEqual(tokens['inputTokens'], 200)
            self.assertEqual(tokens['outputTokens'], 40)
            self.assertEqual(len(issues), 1)


if __name__ == "__main__":
    unittest.main()
