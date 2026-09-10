import importlib.util
import os
from pathlib import Path
import shlex
import sys
import unittest
from unittest.mock import patch, Mock

SPEC = importlib.util.spec_from_file_location('compact_profile', Path(__file__).resolve().parents[2] / 'scripts/codex-reviewed.py')
PROFILE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROFILE)


class CompactProfileTests(unittest.TestCase):
    def test_task_roundtrips_as_data_and_profile_is_child_local(self):
        args = ['--acceptance', 'literal $(touch sentinel)', 'keep quotes: \' " and\nnewline']
        process = Mock(stdout=iter([]))
        process.wait.return_value = 0
        with patch.object(sys, 'argv', ['codex-reviewed.py', *args]), patch.dict(os.environ, {'MAX_THINKING_TOKENS': '1234'}), patch.object(PROFILE.subprocess, 'Popen', return_value=process) as run, patch.object(PROFILE, 'validate_result', return_value={'kind': 'background', 'output': ''}):
            self.assertEqual(PROFILE.main(), 0)
            call = run.call_args
            self.assertEqual(shlex.split(process.stdin.write.call_args.args[0].removeprefix('/codex-dispatch:codex ')), args)
            self.assertEqual(call.kwargs['env']['MAX_THINKING_TOKENS'], '0')
            self.assertEqual(os.environ['MAX_THINKING_TOKENS'], '1234')
            self.assertIn('--system-prompt-file', call.args[0])
            self.assertIn('--plugin-dir', call.args[0])
            self.assertNotIn('--bare', call.args[0])
            self.assertEqual(call.kwargs['env']['MAX_STRUCTURED_OUTPUT_RETRIES'], '1')

    def test_stream_output_is_verbose_and_mcp_is_explicitly_isolated(self):
        command = PROFILE.command('stream-json')
        self.assertIn('--verbose', command)
        self.assertIn('--strict-mcp-config', command)
        self.assertEqual(command[command.index('--model') + 1], PROFILE.MODEL)

class StructuredReportTests(unittest.TestCase):
    def setUp(self):
        import hashlib
        import json
        import tempfile
        import uuid
        self.json = json
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name).resolve()
        self.run = self.repo / '.codex-dispatch/runs/current'
        self.run.mkdir(parents=True)
        sid = str(uuid.uuid4())
        self.prompt = '/codex-dispatch:codex task'
        self.result = {'exit_code': 0, 'session_id': 'codex-current', 'files_changed': ['hello.txt'], 'fell_back_to_fresh': False}
        self.bundle = {'complete': True, 'result': self.result, 'verification_mutations': [], 'test': {'exit_code': 0}, 'verification': {'exit_code': 0}}
        self.report = {'kind': 'review', 'verdict': 'pass', 'reason': '', 'iterations': 1, 'max_iterations': 3,
                       'files_changed': ['hello.txt'], 'session_id': 'codex-current', 'run_dir': str(self.run), 'fell_back_to_fresh': False, 'feedback': []}
        self.final = {'subtype': 'success', 'terminal_reason': 'completed', 'session_id': sid, 'structured_output': self.report}
        identity = {'session_id': sid, 'prompt_id': str(uuid.uuid4()), 'args_sha256': hashlib.sha256(b'task').hexdigest(), 'repo': str(self.repo)}
        saved = {'status': 'complete', 'identity': identity, 'payload': {'identity': identity, 'kind': 'review', 'run_dir': str(self.run), 'config': {'max_iter': 3, 'verify_cmd': 'verify'}, 'dispatch_env': {'REVIEW_TEST_POLICY': 'run', 'REVIEW_TEST_CMD': 'test'}}}
        path = self.repo / '.codex-dispatch/expansions' / sid / 'receipt.json'
        path.parent.mkdir(parents=True)
        path.write_text(json.dumps(saved))
        self.ledger = path.with_suffix('.runs.json')
        self.ledger.write_text(json.dumps({'identity': identity, 'attempts': [{'state': 'complete', 'iteration': 1, 'run_dir': str(self.run), 'session_id': 'codex-current'}]}))
        self.persist()
        self.git = patch.object(PROFILE.subprocess, 'check_output', return_value=str(self.repo) + '\n')
        self.git.start()
        self.addCleanup(self.git.stop)

    def persist(self):
        (self.run / 'result.json').write_text(self.json.dumps(self.result))
        (self.run / 'review-evidence.json').write_text(self.json.dumps(self.bundle))

    def test_pass_requires_current_receipt_and_exact_successful_run(self):
        self.assertEqual(PROFILE.validate_result(self.final, self.prompt), self.report)
        self.report['session_id'] = 'stale'
        with self.assertRaisesRegex(ValueError, 'chain tail'):
            PROFILE.validate_result(self.final, self.prompt)

    def test_success_without_structured_output_never_passes(self):
        del self.final['structured_output']
        with self.assertRaises(ValueError):
            PROFILE.validate_result(self.final, self.prompt)

    def test_claiming_second_iteration_cannot_reuse_old_success(self):
        self.report['iterations'] = 2
        with self.assertRaisesRegex(ValueError, 'chain'):
            PROFILE.validate_result(self.final, self.prompt)

    def test_only_recorded_final_run_can_be_accepted(self):
        ledger = self.json.loads(self.ledger.read_text())
        ledger['attempts'].append({'state': 'complete', 'iteration': 2, 'run_dir': str(self.run.parent / 'newer'), 'session_id': 'newer-session'})
        self.ledger.write_text(self.json.dumps(ledger))
        self.report['iterations'] = 2
        with self.assertRaisesRegex(ValueError, 'chain tail'):
            PROFILE.validate_result(self.final, self.prompt)

    def test_nonzero_or_boolean_exit_and_failed_verification_never_pass(self):
        for code in [64, False]:
            self.result['exit_code'] = code
            self.persist()
            with self.assertRaises(ValueError):
                PROFILE.validate_result(self.final, self.prompt)
        self.result['exit_code'] = 0
        self.bundle['verification']['exit_code'] = 1
        self.persist()
        with self.assertRaisesRegex(ValueError, 'verification'):
            PROFILE.validate_result(self.final, self.prompt)

    def test_extra_fields_boolean_iterations_and_wrong_prompt_are_rejected(self):
        with self.assertRaises(ValueError):
            PROFILE.validate_shape({**self.report, 'extra': 'field'})
        with self.assertRaises(ValueError):
            PROFILE.validate_shape({**self.report, 'iterations': True})
        with self.assertRaisesRegex(ValueError, 'identity'):
            PROFILE.validate_result(self.final, self.prompt + ' changed')


if __name__ == '__main__':
    unittest.main()
