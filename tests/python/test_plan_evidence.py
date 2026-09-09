import importlib.util
from pathlib import Path
import sys
import unittest

spec = importlib.util.spec_from_file_location('plan_runner', Path(__file__).resolve().parents[2] / 'scripts/graphrag-plan-runner.py')
runner = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = runner
spec.loader.exec_module(runner)


class EvidenceTests(unittest.TestCase):
    def test_verification_created_out_of_scope_edit_fails(self):
        evidence = runner.evaluate_evidence(0, {'exit_code': 0, 'files_changed': ['allowed']},
                                           ['allowed', 'outside'], ['allowed'], ['allowed'], {'exit_code': 0})
        self.assertFalse(evidence['accepted'])
        self.assertEqual(evidence['scope_state'], 'failed')

    def test_invalid_result_types_and_paths_fail(self):
        for result in (None, [], {}, {'exit_code': True, 'files_changed': []},
                       {'exit_code': 0, 'files_changed': ['../outside']},
                       {'exit_code': 0, 'files_changed': 'allowed'}):
            with self.subTest(result=result):
                evidence = runner.evaluate_evidence(0, result, ['allowed'], ['allowed'], ['allowed'], {'exit_code': 0})
                self.assertFalse(evidence['accepted'])

    def test_verification_failure_and_skip_fail(self):
        for verification in (None, {'exit_code': 1}):
            evidence = runner.evaluate_evidence(0, {'exit_code': 0, 'files_changed': ['allowed']},
                                               ['allowed'], ['allowed'], ['allowed'], verification)
            self.assertFalse(evidence['accepted'])

    def test_verification_cannot_erase_scope_violation(self):
        evidence = runner.evaluate_evidence(0, {'exit_code': 0, 'files_changed': ['allowed', 'outside']},
                                           ['allowed'], ['allowed', 'outside'], ['allowed'], {'exit_code': 0})
        self.assertFalse(evidence['accepted'])

    def test_windows_paths_are_rejected_on_every_host(self):
        for path in ('C:/outside', r'C:\outside', r'..\outside', '//server/share', '/absolute'):
            self.assertFalse(runner.safe_path(path), path)

    def test_invalid_progress_cannot_resume_from_acceptance_evidence(self):
        import tempfile
        import json
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            packet = runner.Packet('001', 'test', 'test', 'body', {}, ['progress.done.md'], [], 'true', 'progress.done.md')
            progress = repo / packet.progress_record
            progress.write_text('invalid record')
            target = runner.evidence_path(repo, packet)
            target.parent.mkdir(parents=True)
            target.write_text(json.dumps({'version': 2, 'accepted': True, 'verification_state': 'passed',
                                          'files': {'progress.done.md': runner.fingerprint(progress)}}))
            self.assertFalse(runner.is_done(repo, packet))
