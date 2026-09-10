"""Real hostile Git hooks/config must never execute during clean materialization."""
import importlib.util
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("clean_verify", ROOT / "scripts/clean_verify.py")
CLEAN = importlib.util.module_from_spec(spec)
spec.loader.exec_module(CLEAN)


class CleanMaterializationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.git("init", "-q")
        self.git("config", "user.email", "test@example.com")
        self.git("config", "user.name", "test")
        (self.repo / "module").mkdir()
        (self.repo / "module/value.txt").write_text("baseline\n")
        (self.repo / ".gitattributes").write_text("module/value.txt filter=hostile export-ignore\n")
        self.git("add", ".")
        self.git("commit", "-qm", "baseline")
        self.baseline = self.git("rev-parse", "HEAD").strip()
        self.run_dir = self.repo / "run"
        self.run_dir.mkdir()
        (self.run_dir / "baseline-head.txt").write_text(self.baseline + "\n")
        (self.run_dir / "diff.patch").write_text("")
        self.sentinel = self.root / "outside-secret"

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repo), *args], text=True, stderr=subprocess.PIPE)

    def hostile_config(self):
        hooks = self.root / "hooks"
        hooks.mkdir()
        executable = hooks / "post-checkout"
        executable.write_text('#!/bin/sh\nprintf "%s" "$ANTHROPIC_API_KEY" > ' + shlex.quote(str(self.sentinel)) + '\ncat\n')
        executable.chmod(0o755)
        self.git("config", "core.hooksPath", str(hooks))
        self.git("config", "core.fsmonitor", str(executable))
        self.git("config", "filter.hostile.smudge", str(executable))
        self.git("config", "filter.hostile.clean", str(executable))
        self.git("config", "filter.hostile.required", "true")

    def materialize(self):
        checkout, logs = self.root / "checkout", self.root / "logs"
        checkout.mkdir()
        logs.mkdir()
        with patch.dict(os.environ, {"ANTHROPIC_API_KEY": "FAKE_REVIEW_CREDENTIAL_ONLY"}):
            CLEAN.materialize(self.repo, self.baseline, checkout, logs, time.monotonic() + 20)
        return checkout

    def test_raw_materialization_never_runs_hooks_filters_or_fsmonitor(self):
        self.hostile_config()
        checkout = self.materialize()
        self.assertFalse(self.sentinel.exists())
        # export-ignore must not silently drop reviewed baseline source.
        self.assertEqual((checkout / "module/value.txt").read_text(), "baseline\n")
        self.assertNotIn("hostile", (checkout / ".git/config").read_text())

    def test_global_filter_configuration_and_git_env_are_not_inherited(self):
        self.hostile_config()
        with patch.dict(os.environ, {"GIT_CONFIG_GLOBAL": str(self.repo / ".git/config"),
                                     "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.fsmonitor",
                                     "GIT_CONFIG_VALUE_0": str(self.root / "hooks/post-checkout")}):
            self.materialize()
        self.assertFalse(self.sentinel.exists())

    def test_symlink_preserved_without_following_host_target(self):
        (self.repo / "link").symlink_to(self.sentinel)
        self.git("add", "link")
        self.git("commit", "-qm", "link")
        self.baseline = self.git("rev-parse", "HEAD").strip()
        checkout = self.materialize()
        self.assertTrue((checkout / "link").is_symlink())
        self.assertFalse(self.sentinel.exists())

    @unittest.skipUnless(sys.platform == "linux" and Path("/usr/bin/bwrap").exists(), "requires native Linux confinement")
    def test_public_clean_route_confines_and_preserves_module_cwd(self):
        self.hostile_config()
        result = subprocess.run(["bash", str(ROOT / "scripts/clean-verify.sh"), str(self.run_dir), "bash", "-c",
                                 'test "$(cat value.txt)" = baseline && test -z "${ANTHROPIC_API_KEY:-}"'],
                                cwd=self.repo / "module", env={**os.environ, "ANTHROPIC_API_KEY": "FAKE_REVIEW_CREDENTIAL_ONLY"},
                                capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.sentinel.exists())

    @unittest.skipUnless(sys.platform == "linux" and Path("/usr/bin/bwrap").exists(), "requires native Linux confinement")
    def test_public_clean_route_reports_verifier_source_mutation(self):
        result = subprocess.run(["bash", str(ROOT / "scripts/clean-verify.sh"), str(self.run_dir), "bash", "-c",
                                 'echo changed > module/value.txt'], cwd=self.repo, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 66, result.stderr)
        self.assertEqual((self.repo / "module/value.txt").read_text(), "baseline\n")

    def test_patch_cannot_write_repository_configuration(self):
        (self.run_dir / "diff.patch").write_text("diff --git a/.git/config b/.git/config\nnew file mode 100644\n--- /dev/null\n+++ b/.git/config\n@@ -0,0 +1 @@\n+hostile\n")
        result = subprocess.run(["bash", str(ROOT / "scripts/clean-verify.sh"), str(self.run_dir), "true"],
                                cwd=self.repo, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 65, result.stderr)


if __name__ == "__main__":
    unittest.main()
