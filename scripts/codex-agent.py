#!/usr/bin/env python3
"""Adapt a labeled delegated task to the canonical reviewed command as JSON data."""
import importlib.util
import json
from pathlib import Path
import shlex
import subprocess
import sys

ROOT = Path(__file__).resolve().parent
FIELDS = {'TASK', 'ACCEPTANCE CRITERIA', 'FILES', 'WORKDIR', 'CONSTRAINTS',
          'TEST POLICY', 'TEST CMD', 'VERIFY CMD', 'MAX ITER', 'NO RESUME', 'CLEAN VERIFY'}


def request(data):
    if not isinstance(data, dict) or set(data) - FIELDS:
        raise ValueError('unknown or invalid delegated request fields')
    for key in ['TASK', 'ACCEPTANCE CRITERIA']:
        if not isinstance(data.get(key), str) or not data[key].strip():
            raise ValueError('missing ' + key)
    argv = []
    for key, flag in [('ACCEPTANCE CRITERIA', 'acceptance'), ('FILES', 'files'), ('WORKDIR', 'workdir'),
                      ('CONSTRAINTS', 'constraints'), ('TEST CMD', 'test-cmd'), ('VERIFY CMD', 'verify-cmd')]:
        if key in data:
            if not isinstance(data[key], str):
                raise ValueError('invalid ' + key)
            argv.extend(['--' + flag, data[key]])
    if 'MAX ITER' in data:
        if type(data['MAX ITER']) is not int:
            raise ValueError('invalid MAX ITER')
        argv.extend(['--max-iter', str(data['MAX ITER'])])
    if data.get('TEST POLICY', 'run') not in {'run', 'skip'}:
        raise ValueError('invalid TEST POLICY')
    if data.get('TEST POLICY') == 'skip':
        argv.append('--no-tests')
    for key, flag in [('NO RESUME', 'no-resume'), ('CLEAN VERIFY', 'clean-verify')]:
        if key in data and type(data[key]) is not bool:
            raise ValueError('invalid ' + key)
        if data.get(key):
            argv.append('--' + flag)
    argv.extend(['--', data['TASK']])
    raw = shlex.join(argv)
    spec = importlib.util.spec_from_file_location('expansion', ROOT / 'hooks/codex-expansion.py')
    hook = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(hook)
    hook.parse(raw)  # The canonical parser owns defaults and limits.
    return '/codex-dispatch:codex ' + raw


def main():
    try:
        prompt = request(json.load(sys.stdin))
    except (ValueError, TypeError) as error:
        print(json.dumps({'error': str(error)}), file=sys.stderr)
        return 64
    return subprocess.run([sys.executable, str(ROOT / 'codex-reviewed.py'), '--stdin-request', '--output-format', 'json'],
                          input=prompt, text=True).returncode


if __name__ == '__main__':
    sys.exit(main())
