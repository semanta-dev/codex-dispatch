import importlib.util
import hashlib
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("benchmark_audit", Path(__file__).resolve().parents[1] / "benchmark/audit.py")
AUDIT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(AUDIT)


class BenchmarkGateTests(unittest.TestCase):
    def cohort(self):
        return {f"C{case:02}-{rep}-{arm}": {"accepted": True, "failures": [], "safety_violations": [], "seconds": 10,
                "cost_complete": True, "total_cost_equivalent_usd": 1, "known_cost_equivalent_usd": 1}
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


if __name__ == "__main__":
    unittest.main()
