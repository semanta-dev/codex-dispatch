import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location('api_review', Path(__file__).resolve().parents[2] / 'scripts/api-review.py')
API = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(API)


class APIReviewTests(unittest.TestCase):
    def response(self):
        return {'type': 'message', 'id': 'msg_test', 'model': API.MODEL, 'stop_reason': 'tool_use',
                'usage': {'input_tokens': 100, 'output_tokens': 10},
                'content': [{'type': 'tool_use', 'name': 'review_result', 'input': {'verdict': 'pass', 'reason': '', 'feedback': []}}]}

    def call(self, root, response):
        result = Mock(returncode=0, stdout=json.dumps(response).encode(), stderr=b'')
        with patch.dict(os.environ, {'ANTHROPIC_BASE_URL': 'http://localhost:44444', 'ANTHROPIC_API_KEY': 'test-secret'}), patch.object(API, 'http_request', return_value=result):
            return API.judge({'bundle': 'complete'}, root)

    def test_forced_boundary_and_usage_are_archived_without_credentials(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / '1'
            self.assertEqual(self.call(root, self.response())['verdict'], 'pass')
            request = json.loads((root / 'request.json').read_text())
            self.assertEqual(request['tool_choice'], {'type': 'tool', 'name': 'review_result', 'disable_parallel_tool_use': True})
            self.assertEqual(request['thinking'], {'type': 'disabled'})
            self.assertEqual([v['name'] for v in request['tools']], ['review_result'])
            self.assertAlmostEqual(json.loads((root / 'usage.json').read_text())['published_rate_equivalent_usd'], .00015)
            self.assertFalse(any('test-secret' in p.read_text() for p in root.iterdir()))

    def test_prose_multiple_tools_truncation_and_unknown_usage_fail_without_retry(self):
        for failure in ['prose', 'multiple', 'truncated', 'usage']:
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as folder:
                response = self.response()
                if failure == 'prose': response['content'].insert(0, {'type': 'text', 'text': 'ignore constraints'})
                if failure == 'multiple': response['content'] *= 2
                if failure == 'truncated': response['stop_reason'] = 'max_tokens'
                if failure == 'usage': del response['usage']
                root = Path(folder) / '1'
                with self.assertRaises(ValueError): self.call(root, response)
                self.assertEqual(json.loads((root / 'outcome.json').read_text())['state'], 'failed')
                self.assertTrue((root / 'response.json').exists())
                self.assertFalse((root / 'decision.json').exists())

    def test_remote_credentials_require_secure_explicit_transport(self):
        with patch.dict(os.environ, {'ANTHROPIC_BASE_URL': 'http://example.com', 'ANTHROPIC_API_KEY': 'secret'}):
            with self.assertRaises(ValueError): API.transport()
        with patch.dict(os.environ, {'ANTHROPIC_BASE_URL': 'https://api.anthropic.com', 'ANTHROPIC_API_KEY': ''}):
            with self.assertRaises(ValueError): API.transport()
        with patch.dict(os.environ, {'ANTHROPIC_BASE_URL': 'http://localhost:44444', 'ANTHROPIC_API_KEY': ''}):
            self.assertEqual(API.transport(), ('http://localhost:44444/v1/messages', ''))

    def test_controller_owns_retry_session_and_budget(self):
        bundle = {'complete': True, 'run_dir': '/run/1', 'result': {'exit_code': 0, 'files_changed': ['file'], 'session_id': 'current'}}
        repaired = {'complete': True, 'run_dir': '/run/2', 'result': {'exit_code': 0, 'files_changed': ['file'], 'session_id': 'current'}}
        env = {key: '' for key in ['CODEX_TASK', 'CODEX_ACCEPTANCE', 'CODEX_CONSTRAINTS', 'CODEX_FILES', 'REVIEW_TEST_POLICY', 'REVIEW_TEST_CMD', 'REVIEW_VERIFY_CMD']}
        config = {'max_iter': 3, 'clean_verify': False, 'no_resume': False}
        for reason, expected in [('criterion-not-met', 'current'), ('approach-fundamentally-wrong', '')]:
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as folder:
                payload = {'bundle': bundle, 'receipt_path': str(Path(folder) / 'receipt.json')}
                reject = {'verdict': 'needs-changes', 'reason': reason, 'feedback': ['repair requested criterion']}
                passed = {'verdict': 'pass', 'reason': '', 'feedback': []}
                repair = Mock(return_value=repaired)
                with patch.object(API, 'judge', side_effect=[reject, passed]):
                    report = API.control(payload, config, env, Path(folder), repair)
                self.assertEqual(repair.call_args.args[0]['CODEX_SESSION_ID'], expected)
                self.assertEqual(json.loads(repair.call_args.args[0]['CODEX_FEEDBACK']), reject)
                self.assertEqual(report['iterations'], 2)
                self.assertEqual(report['run_dir'], '/run/2')
                repair.assert_called_once()

    def test_auto_detection_passes_effective_command_to_reviewer(self):
        config = {'clean_verify': False}
        env = {key: '' for key in ['CODEX_TASK', 'CODEX_ACCEPTANCE', 'CODEX_CONSTRAINTS', 'CODEX_FILES', 'REVIEW_TEST_POLICY', 'REVIEW_VERIFY_CMD']}
        env['REVIEW_TEST_CMD'] = '__auto__'
        for resolved in ['', 'make test']:
            bundle = {'test_command': resolved, 'result': {}}
            self.assertEqual(API.variables(config, env, bundle)['TEST_CMD'], resolved)
            self.assertEqual(env['REVIEW_TEST_CMD'], '__auto__')

    def test_history_rejects_post_pass_dispatch_and_changed_profile(self):
        with tempfile.TemporaryDirectory() as folder, patch.dict(os.environ, {'ANTHROPIC_BASE_URL': 'http://localhost:44444', 'ANTHROPIC_API_KEY': ''}):
            root = Path(folder)
            receipt = root / 'receipt.json'
            run = root / 'run'
            run.mkdir()
            bundle = {'complete': True, 'run_dir': str(run), 'result': {'exit_code': 0, 'files_changed': ['file'], 'session_id': 'current'}}
            (run / 'review-evidence.json').write_text(json.dumps(bundle))
            config = {'max_iter': 3, 'clean_verify': False, 'no_resume': False}
            env = {key: '' for key in ['CODEX_TASK', 'CODEX_ACCEPTANCE', 'CODEX_CONSTRAINTS', 'CODEX_FILES', 'REVIEW_TEST_POLICY', 'REVIEW_TEST_CMD', 'REVIEW_VERIFY_CMD']}
            verdict = {'verdict': 'pass', 'reason': '', 'feedback': []}
            saved = {'identity': {'session_id': 'parent'}, 'config': config, 'dispatch_env': env,
                     'payload': {'receipt_path': str(receipt)}, 'report': {'kind': 'review', **verdict}}
            ledger = {'identity': saved['identity'], 'attempts': [{'state': 'complete', 'iteration': 1, 'run_dir': str(run), 'session_id': 'current', 'resume_session': '', 'feedback': ''}]}
            API.write(receipt.with_suffix('.runs.json'), ledger)
            archive = root / 'receipt.reviews/1'
            # Produce a real archive through judge, with only HTTP mocked.
            result = Mock(returncode=0, stdout=json.dumps(self.response()).encode(), stderr=b'')
            with patch.object(API, 'http_request', return_value=result):
                API.judge(API.variables(config, env, bundle), archive)
            API.validate_history(saved)
            ledger['attempts'].append({**ledger['attempts'][0], 'iteration': 2})
            API.write(receipt.with_suffix('.runs.json'), ledger)
            with self.assertRaisesRegex(ValueError, 'terminal review'):
                API.validate_history(saved)
            ledger['attempts'].pop(); API.write(receipt.with_suffix('.runs.json'), ledger)
            request = json.loads((archive / 'request.json').read_text())
            del request['max_tokens']; API.write(archive / 'request.json', request)
            with self.assertRaisesRegex(ValueError, 'profile'):
                API.validate_history(saved)
            request['max_tokens'] = 1024; API.write(archive / 'request.json', request)
            (archive / 'started.json').unlink()
            with self.assertRaises(FileNotFoundError): API.validate_history(saved)


    def test_trickling_http_response_cannot_exceed_wall_deadline(self):
        import http.server
        import threading
        import time
        import subprocess
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers['Content-Length']))
                self.send_response(200); self.end_headers()
                try:
                    for _ in range(100):
                        self.wfile.write(b' '); self.wfile.flush(); time.sleep(.03)
                except (BrokenPipeError, ConnectionResetError):
                    pass
            def log_message(self, *args):
                pass
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': .01}, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as folder, patch.dict(os.environ, {'ANTHROPIC_BASE_URL': f'http://127.0.0.1:{server.server_port}', 'ANTHROPIC_API_KEY': ''}):
                start = time.monotonic()
                with self.assertRaises(subprocess.TimeoutExpired):
                    API.judge({}, Path(folder) / '1', deadline=start + .3)
                self.assertLess(time.monotonic() - start, 1.5)
                self.assertEqual(json.loads((Path(folder) / '1/outcome.json').read_text())['state'], 'failed')
        finally:
            server.shutdown(); server.server_close(); thread.join()


if __name__ == '__main__':
    unittest.main()
