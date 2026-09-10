import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import uuid

SPEC = importlib.util.spec_from_file_location('codex_expansion', Path(__file__).resolve().parents[2] / 'scripts/hooks/codex-expansion.py')
HOOK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HOOK)


class ExpansionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        subprocess.run(['git', 'init', '-q', str(self.repo)], check=True)
        self.event = dict(command_name='codex-dispatch:codex', expansion_type='slash_command', command_source='plugin',
                          session_id=str(uuid.uuid4()), prompt_id=str(uuid.uuid4()), cwd=str(self.repo),
                          command_args='--no-tests --acceptance "literal $(touch SENTINEL)" "task with\na newline"')
        self.bundle = dict(complete=True, run_dir=str(self.repo / '.codex-dispatch/runs/one'), result=dict(session_id='codex-one', exit_code=0))

    def test_untrusted_text_is_environment_data_not_shell_source(self):
        captured = []
        def invoke(argv, env, cwd, deadline=None):
            captured.append((argv, env, cwd))
            return json.dumps(self.bundle)
        with patch.object(HOOK, 'invoke', side_effect=invoke):
            result = HOOK.expand(self.event)
        argv, env, cwd = captured[0]
        self.assertEqual(argv[1], str(HOOK.ROOT / 'review-evidence.py'))
        self.assertEqual(env['CODEX_ACCEPTANCE'], 'literal $(touch SENTINEL)')
        self.assertEqual(env['CODEX_TASK'], 'task with\na newline')
        self.assertFalse((self.repo / 'SENTINEL').exists())
        self.assertIn('CODEX_EXPANSION_RECEIPT', result['hookSpecificOutput']['additionalContext'])

    def test_duplicate_reuses_receipt_new_prompt_dispatches(self):
        with patch.object(HOOK, 'invoke', return_value=json.dumps(self.bundle)) as invoke:
            first = HOOK.expand(self.event)
            self.assertEqual(first, HOOK.expand(self.event))
            self.assertEqual(invoke.call_count, 1)
            self.event['prompt_id'] = str(uuid.uuid4())
            HOOK.expand(self.event)
            self.assertEqual(invoke.call_count, 2)

    def test_changed_args_cannot_reuse_identity(self):
        with patch.object(HOOK, 'invoke', return_value=json.dumps(self.bundle)) as invoke:
            HOOK.expand(self.event)
            self.event['command_args'] = 'a different task'
            with self.assertRaisesRegex(ValueError, 'identity changed'):
                HOOK.expand(self.event)
            self.assertEqual(invoke.call_count, 1)

    def test_failed_dispatch_never_reexecutes_on_duplicate(self):
        with patch.object(HOOK, 'invoke', side_effect=ValueError('capture failed')) as invoke:
            with self.assertRaisesRegex(ValueError, 'capture failed'):
                HOOK.expand(self.event)
            with self.assertRaisesRegex(ValueError, 'pending/failed'):
                HOOK.expand(self.event)
            self.assertEqual(invoke.call_count, 1)

    def test_wrong_source_and_invalid_args_do_not_dispatch(self):
        with patch.object(HOOK, 'invoke') as invoke:
            self.event['command_source'] = 'project'
            with self.assertRaises(ValueError):
                HOOK.expand(self.event)
            self.event['command_source'] = 'plugin'
            for args in ['--unknown task', '--max-iter 0 task', '--acceptance "unclosed', '--no-tests']:
                self.event['command_args'] = args
                with self.assertRaises(ValueError):
                    HOOK.expand(self.event)
            invoke.assert_not_called()

    def test_inherited_session_and_feedback_cannot_contaminate_first_run(self):
        with patch.dict(os.environ, {'CODEX_SESSION_ID': 'stale', 'CODEX_FEEDBACK': 'stale', 'CODEX_RESULT_DIR': 'stale'}):
            env = HOOK.environment(HOOK.parse(self.event['command_args']), self.repo)
        for key in ['CODEX_SESSION_ID', 'CODEX_FEEDBACK', 'CODEX_RESULT_DIR']:
            self.assertNotIn(key, env)

    def test_background_status_is_argv_data(self):
        self.event['command_args'] = '--status "literal; $(echo bad)"'
        with patch.object(HOOK, 'invoke', return_value='status output') as invoke:
            output = HOOK.expand(self.event)
        self.assertEqual(invoke.call_args.args[0][-2:], ['--status', 'literal; $(echo bad)'])
        self.assertIn('status output', output['hookSpecificOutput']['additionalContext'])


if __name__ == '__main__':
    unittest.main()
