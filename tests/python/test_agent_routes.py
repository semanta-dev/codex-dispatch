import importlib.util
import json
from pathlib import Path
import shlex
import unittest
from unittest.mock import Mock, patch
import io

ROOT = Path(__file__).resolve().parents[2]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


agent = load('agent_adapter', ROOT / 'scripts/codex-agent.py')
hook = load('agent_hook', ROOT / 'scripts/hooks/codex-expansion.py')


class RouteTests(unittest.TestCase):
    def test_equivalent_requests_share_parser_configuration(self):
        data = {'TASK': 'literal $(touch sentinel) and "quotes"\nnext line',
                'ACCEPTANCE CRITERIA': 'preserve all bytes', 'FILES': 'module/a.py',
                'WORKDIR': 'module', 'TEST CMD': 'python3 -m unittest', 'MAX ITER': 4,
                'NO RESUME': True, 'CLEAN VERIFY': True, 'VERIFY CMD': 'printf success'}
        raw = agent.request(data).removeprefix('/codex-dispatch:codex ')
        direct = shlex.join(['--acceptance', data['ACCEPTANCE CRITERIA'], '--files', data['FILES'],
                             '--workdir', data['WORKDIR'], '--test-cmd', data['TEST CMD'],
                             '--verify-cmd', data['VERIFY CMD'], '--max-iter', '4', '--no-resume',
                             '--clean-verify', '--', data['TASK']])
        self.assertEqual(hook.parse(raw), hook.parse(direct))
        self.assertEqual(hook.parse(raw)['task'], data['TASK'])

    def test_defaults_and_rejected_requests_do_not_dispatch(self):
        raw = agent.request({'TASK': 'task', 'ACCEPTANCE CRITERIA': 'criterion'})
        self.assertEqual(hook.parse(raw.removeprefix('/codex-dispatch:codex '))['max_iter'], 3)
        for data in [{}, {'TASK': 'task'}, {'TASK': 'task', 'ACCEPTANCE CRITERIA': 'criterion', 'MAX ITER': 11}]:
            with patch.object(agent.sys, 'stdin', io.StringIO(json.dumps(data))), patch.object(agent.subprocess, 'run') as run:
                self.assertEqual(agent.main(), 64)
                run.assert_not_called()

    def test_adapter_passes_data_and_propagates_canonical_failure(self):
        data = {'TASK': 'touch sentinel', 'ACCEPTANCE CRITERIA': 'do task'}
        with patch.object(agent.sys, 'stdin', io.StringIO(json.dumps(data))), patch.object(agent.subprocess, 'run', return_value=Mock(returncode=65)) as run:
            self.assertEqual(agent.main(), 65)
        self.assertEqual(run.call_args.kwargs['input'], agent.request(data))
        self.assertNotIn(data['TASK'], run.call_args.args[0])
        self.assertIn('--stdin-request', run.call_args.args[0])
