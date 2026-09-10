"""Development evidence must capture every source edit, including untracked files."""
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('development_gate', ROOT / 'scripts/remediation-gates.py')
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class DevelopmentGateTests(unittest.TestCase):
    def test_output_cannot_hide_source_paths(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            repo = base / 'repo'
            repo.mkdir()
            alias = base / 'alias'
            alias.symlink_to(repo, target_is_directory=True)
            with patch.object(gate, 'ROOT', repo):
                for output in [repo, base, repo / 'new-output', alias / 'output']:
                    with self.subTest(output=output), self.assertRaisesRegex(ValueError, 'outside'):
                        gate.validate_output(output)
                self.assertEqual(gate.validate_output(base / 'evidence'), base / 'evidence')

    def test_worktree_changes_invalidate_manifest_without_index_change(self):
        with tempfile.TemporaryDirectory() as temporary:
            repo = Path(temporary)
            subprocess.run(['git', 'init', '-q', str(repo)], check=True)
            source = repo / 'source.py'
            source.write_text('before')
            subprocess.run(['git', 'add', '.'], cwd=repo, check=True)
            subprocess.run(['git', '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
                            '-c', 'commit.gpgsign=false', 'commit', '-qm', 'fixture'], cwd=repo, check=True)
            with patch.object(gate, 'ROOT', repo):
                before = gate.source_manifest()
                source.write_text('after')
                changed = gate.source_manifest()
                self.assertEqual(before['commit'], changed['commit'])
                self.assertEqual(before['index_sha256'], changed['index_sha256'])
                self.assertNotEqual(before['files'], changed['files'])
                (repo / 'new.py').write_text('untracked')
                self.assertNotEqual(changed, gate.source_manifest())
