import importlib.util
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("review_evidence", Path(__file__).resolve().parents[2] / "scripts/review-evidence.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class ReviewEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name).resolve()
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        self.run = self.repo / ".codex-dispatch/runs/test"
        self.run.mkdir(parents=True)
        (self.repo / "hello.txt").write_text("hello\n")
        self.result = {"exit_code": 0, "session_id": "test", "files_changed": ["hello.txt"], "lines_added": 1, "lines_removed": 0}
        (self.run / "result.json").write_text(json.dumps(self.result))
        (self.run / "diff.patch").write_text("diff --git a/hello.txt b/hello.txt\n+hello\n")
        (self.run / "stdout.log").write_text("codex evidence\n")
        self.scripts = self.run / "scripts"
        self.scripts.mkdir()
        (self.scripts / "dispatch-codex.sh").write_text("#!/bin/sh\nprintf '%s\\n' " + shlex.quote(self.run.as_posix()) + "\n")
        self.addCleanup(patch.stopall)
        patch.object(MODULE, "ROOT", self.scripts).start()
        patch.dict(os.environ, {"REVIEW_TEST_CMD": "", "REVIEW_VERIFY_CMD": "", "REVIEW_TEST_POLICY": "run", "REVIEW_CLEAN_VERIFY": "false"}).start()
        previous = Path.cwd()
        os.chdir(self.repo)
        self.addCleanup(os.chdir, previous)

    def test_identical_checks_execute_once_and_preserve_complete_diff(self):
        command = "printf x >> .codex-dispatch/runs/test/count"
        os.environ.update(REVIEW_TEST_CMD=command, REVIEW_VERIFY_CMD=command)
        bundle = MODULE.collect()
        self.assertEqual((self.run / "count").read_text(), "x")
        self.assertTrue(bundle["verification"]["reused_test_execution"])
        self.assertEqual(bundle["diff"], (self.run / "diff.patch").read_text())
        self.assertEqual(bundle["changed_file_facts"]["hello.txt"]["kind"], "file")

    def test_mutation_then_restoration_is_still_detected(self):
        os.environ.update(REVIEW_TEST_CMD="printf changed > hello.txt", REVIEW_VERIFY_CMD="printf 'hello\\n' > hello.txt")
        bundle = MODULE.collect()
        self.assertEqual(bundle["verification_mutations"], ["hello.txt"])
        self.assertEqual((self.repo / "hello.txt").read_text(), "hello\n")

    def test_staging_then_unstaging_is_still_detected(self):
        os.environ.update(REVIEW_TEST_CMD="git add hello.txt", REVIEW_VERIFY_CMD="git rm --cached hello.txt")
        bundle = MODULE.collect()
        self.assertIn(".git/index", bundle["verification_mutations"])

    def test_failing_check_is_not_success(self):
        os.environ["REVIEW_VERIFY_CMD"] = "printf diagnostic >&2; exit 7"
        bundle = MODULE.collect()
        self.assertEqual(bundle["verification"]["exit_code"], 7)
        self.assertIn("diagnostic", bundle["verification"]["stderr"])

    def test_clean_verification_preserves_arguments_and_shell_semantics(self):
        source = Path(__file__).resolve().parents[2] / "scripts/clean-verify.sh"
        shutil.copy2(source, self.scripts / "clean-verify.sh")
        (self.repo / "README.md").write_text("base\n")
        subprocess.run(["git", "add", "README.md"], check=True)
        subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-qm", "base"], check=True)
        (self.run / "baseline-head.txt").write_bytes(subprocess.check_output(["git", "rev-parse", "HEAD"]))
        (self.run / "diff.patch").write_text("diff --git a/hello.txt b/hello.txt\nnew file mode 100644\n--- /dev/null\n+++ b/hello.txt\n@@ -0,0 +1 @@\n+hello\n")
        command = "printf '%s\\n' 'two words' | grep -q '^two words$'; test \"$(cat hello.txt)\" = hello"
        os.environ.update(REVIEW_TEST_CMD=command, REVIEW_VERIFY_CMD=command, REVIEW_CLEAN_VERIFY="true")
        bundle = MODULE.collect()
        self.assertEqual(bundle["verification"]["exit_code"], 0, bundle["verification"])
        self.assertTrue(bundle["verification"]["clean"])
        self.assertNotIn("reused_test_execution", bundle["verification"])
        self.assertFalse(bundle["verification_mutations"])
        os.environ.update(REVIEW_TEST_CMD="", REVIEW_VERIFY_CMD="printf repaired > hello.txt; test $(cat hello.txt) = repaired")
        rejected = MODULE.collect()
        self.assertEqual(rejected["verification"]["exit_code"], 66, rejected["verification"])
        self.assertIn("hello.txt", rejected["verification"]["stderr"])

    def test_zero_text_lines_preserve_binary_deletion_and_mode_evidence(self):
        self.result.update(lines_added=0, lines_removed=0)
        (self.run / "result.json").write_text(json.dumps(self.result))
        (self.repo / "hello.txt").unlink()
        (self.run / "diff.patch").write_text("diff --git a/hello.txt b/hello.txt\ndeleted file mode 100644\nBinary files a/hello.txt and /dev/null differ\n")
        deleted = MODULE.collect()
        self.assertTrue(deleted["complete"])
        self.assertEqual(deleted["changed_file_facts"]["hello.txt"]["kind"], "deleted")
        (self.repo / "hello.txt").write_text("hello\n")
        (self.repo / "hello.txt").chmod(0o755)
        (self.run / "diff.patch").write_text("diff --git a/hello.txt b/hello.txt\nold mode 100644\nnew mode 100755\n")
        mode = MODULE.collect()
        self.assertTrue(mode["complete"])
        self.assertEqual(mode["result"]["lines_added"], 0)
        self.assertIn("new mode 100755", mode["diff"])

    def test_boolean_exit_code_is_invalid(self):
        self.result["exit_code"] = False
        (self.run / "result.json").write_text(json.dumps(self.result))
        with self.assertRaisesRegex(ValueError, "exit_code"):
            MODULE.collect()

    def test_oversized_diff_cannot_silently_pass(self):
        (self.run / "diff.patch").write_text("x" * (MODULE.LIMIT + 1))
        with self.assertRaisesRegex(ValueError, "exceeds"):
            MODULE.collect()

    def test_failed_dispatch_does_not_run_verification(self):
        self.result["exit_code"] = 1
        (self.run / "result.json").write_text(json.dumps(self.result))
        os.environ["REVIEW_VERIFY_CMD"] = "touch must-not-exist"
        bundle = MODULE.collect()
        self.assertEqual(bundle["result"]["exit_code"], 1)
        self.assertFalse((self.repo / "must-not-exist").exists())


if __name__ == "__main__":
    unittest.main()
