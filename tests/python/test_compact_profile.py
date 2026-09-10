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
        with patch.object(sys, 'argv', ['codex-reviewed.py', *args]), patch.dict(os.environ, {'MAX_THINKING_TOKENS': '1234'}), patch.object(PROFILE.subprocess, 'run', return_value=Mock(returncode=0)) as run:
            self.assertEqual(PROFILE.main(), 0)
            call = run.call_args
            self.assertEqual(shlex.split(call.kwargs['input'].removeprefix('/codex-dispatch:codex ')), args)
            self.assertEqual(call.kwargs['env']['MAX_THINKING_TOKENS'], '0')
            self.assertEqual(os.environ['MAX_THINKING_TOKENS'], '1234')
            self.assertIn('--system-prompt-file', call.args[0])
            self.assertIn('--plugin-dir', call.args[0])
            self.assertNotIn('--bare', call.args[0])

    def test_stream_output_is_verbose_and_mcp_is_explicitly_isolated(self):
        command = PROFILE.command('stream-json')
        self.assertIn('--verbose', command)
        self.assertIn('--strict-mcp-config', command)
        self.assertEqual(command[command.index('--model') + 1], PROFILE.MODEL)


if __name__ == '__main__':
    unittest.main()
