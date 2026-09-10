#!/usr/bin/env python3
"""Freeze and evaluate the deployed Haiku decision rubric, independent of retries."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
MODEL = 'claude-haiku-4-5-20251001'
ADAPTER = '''Evaluate exactly one reviewer decision using the supplied variables and bundle.
Apply the trusted review-check section without dispatching, repairing, or running implementation tools.
This is a decision evaluation; retry limits and final orchestration reports do not apply.
Call StructuredOutput with verdict and reason. Empty reason for pass.
Emit no prose. Do not call any other tool.
'''


def dump(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def judgment(text):
    pairs = re.findall(r'^(VERDICT|REASON):[ \t]*([^\n]*)', text, re.M)
    if len(pairs) != 2 or {key for key, _ in pairs} != {'VERDICT', 'REASON'}:
        return {}
    return {key: value.strip() for key, value in pairs}


def thinking_off(parsed, final, prefix):
    sid = final.get('session_id', '')
    if not re.fullmatch(r'[a-f0-9-]{36}', sid):
        return False
    sources = list((Path.home() / '.claude/projects').glob(f'*/{sid}.jsonl'))
    if len(sources) != 1:
        return False
    source = sources[0]
    prefix.with_suffix('.transcript.jsonl').write_bytes(source.read_bytes())
    try:
        persisted = [json.loads(line) for line in source.read_text().splitlines() if line.strip()]
    except ValueError:
        return False
    blocks = [block for event in [*parsed, *persisted] for block in event.get('message', {}).get('content', []) if isinstance(block, dict)]
    return not any(block.get('type') in {'thinking', 'redacted_thinking'} for block in blocks) and all(value.get('thinkingTokens', 0) == 0 for value in final.get('modelUsage', {}).values())


def freeze(out):
    out.mkdir(parents=True, exist_ok=False)
    shutil.copytree(ROOT / 'tests/fixtures/reviewer', out / 'fixtures')
    contract = (ROOT / 'scripts/direct-review-contract.md').read_text()
    rubric = contract.split('Apply these checks yourself.', 1)[1].split('## Repair only', 1)[0]
    (out / 'rubric.md').write_text('Apply these checks yourself.' + rubric)
    (out / 'adapter.md').write_text(ADAPTER)
    shutil.copy2(ROOT / 'scripts/compact-review-system.md', out / 'system.md')
    shutil.copy2(ROOT / 'scripts/codex-reviewed.py', out / 'profile.py')
    shutil.copy2(__file__, out / 'direct-fixtures.py')
    dump(out / 'manifest.json', {'model': MODEL, 'thinking_tokens': 0, 'structured_output_retries': 1, 'runs_per_fixture': 10, 'threshold': 8, 'source_root': str(ROOT),
                               'contract_sha256': hashlib.sha256(contract.encode()).hexdigest(),
                               'candidate': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
                               'fixtures': sorted(p.name for p in (out / 'fixtures').iterdir())})
    dump(out / 'frozen-sha256.json', {p.relative_to(out).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest() for p in out.rglob('*') if p.is_file()})


def fixture_input(fixture, work):
    def read(name, default=''):
        path = fixture / name
        return path.read_text().strip() if path.exists() else default
    diff = (fixture / 'diff.patch').read_text()
    changed = re.findall(r'^diff --git a/\S+ b/(\S+)$', diff, re.M)
    result = json.loads(read('result.json')) if (fixture / 'result.json').exists() else {
        'exit_code': 0, 'files_changed': changed,
        'lines_added': sum(l.startswith('+') and not l.startswith('+++') for l in diff.splitlines()),
        'lines_removed': sum(l.startswith('-') and not l.startswith('---') for l in diff.splitlines())}
    policy, command = read('test-policy.txt', 'skip'), read('test-cmd.txt')
    test = None
    if policy != 'skip' and command:
        proc = subprocess.run(['bash', '-c', command], cwd=work, capture_output=True, text=True, timeout=30)
        test = {'command': command, 'exit_code': proc.returncode, 'stdout': proc.stdout, 'stderr': proc.stderr}
    bundle = {'complete': True, 'result': result, 'diff': diff, 'test': test, 'verification': None,
              'verification_mutations': [], 'changed_file_facts': {}}
    return {'TASK': read('task.txt'), 'ACCEPTANCE': read('acceptance.txt'), 'CONSTRAINTS': read('constraints.txt'),
            'FILES': '', 'TEST_POLICY': policy, 'TEST_CMD': command, 'VERIFY_CMD': '', 'CLEAN_VERIFY': False,
            'bundle': bundle, 'result': result}


def run(out):
    for name, digest in json.loads((out / 'frozen-sha256.json').read_text()).items():
        if hashlib.sha256((out / name).read_bytes()).hexdigest() != digest:
            raise ValueError('frozen fixture input changed: ' + name)
    (out / 'started').open('x').close()
    spec = importlib.util.spec_from_file_location('profile', out / 'profile.py')
    profile = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(profile)
    # Use the exact shipped system role and launch profile; only the decision
    # adapter replaces slash-command orchestration for this isolated rubric gate.
    manifest = json.loads((out / 'manifest.json').read_text())
    profile.ROOT = Path(manifest['source_root'])
    command = profile.command('stream-json')
    schema = {'type': 'object', 'properties': {'verdict': {'enum': ['pass', 'needs-changes', 'fail']}, 'reason': {'type': 'string', 'maxLength': 100}}, 'required': ['verdict', 'reason'], 'additionalProperties': False}
    command[command.index('--json-schema') + 1] = json.dumps(schema)
    command[command.index('--system-prompt-file') + 1] = str(out / 'system.md')
    command += ['--append-system-prompt', (out / 'rubric.md').read_text() + '\n' + (out / 'adapter.md').read_text()]
    dump(out / 'command.json', command)
    results = {}
    work = out / 'work'
    work.mkdir()
    for name in manifest['fixtures']:
        fixture = out / 'fixtures' / name
        archive = out / 'responses' / name
        archive.mkdir(parents=True)
        payload = fixture_input(fixture, work)
        prompt = json.dumps(payload)
        (archive / 'prompt.json').write_text(prompt)
        expected = judgment((fixture / 'expected_verdict.txt').read_text())
        if not expected:
            raise ValueError('invalid frozen expected judgment')
        matched = 0
        for iteration in range(1, 11):
            prefix = archive / str(iteration)
            with prefix.with_suffix('.stdout.jsonl').open('w') as stdout, prefix.with_suffix('.stderr.txt').open('w') as stderr:
                try:
                    proc = subprocess.run(command, input=prompt, text=True, stdout=stdout, stderr=stderr, env=profile.environment(), cwd=work, timeout=120)
                    code = proc.returncode
                except subprocess.TimeoutExpired:
                    code = 124
            parsed = []
            try:
                parsed = [json.loads(line) for line in prefix.with_suffix('.stdout.jsonl').read_text().splitlines() if line.strip()]
            except ValueError:
                pass
            finals = [event for event in parsed if event.get('type') == 'result']
            final = finals[-1] if finals else {}
            report = final.get('structured_output')
            got = {key.upper(): value for key, value in report.items()} if isinstance(report, dict) and set(report) == {'verdict', 'reason'} and all(isinstance(value, str) for value in report.values()) else {}
            tools = [b for e in parsed for b in e.get('message', {}).get('content', []) if isinstance(b, dict) and b.get('type') == 'tool_use']
            usage = final.get('modelUsage', {})
            no_thinking = thinking_off(parsed, final, prefix)
            okay = bool(code == 0 and final.get('subtype') == 'success' and not final.get('is_error') and final.get('terminal_reason') == 'completed' and got == expected and set(usage) == {MODEL} and tools and all(tool.get('name') == 'StructuredOutput' for tool in tools) and no_thinking)
            dump(prefix.with_suffix('.judgment.json'), {'exit_code': code, 'expected': expected, 'got': got, 'match': okay, 'model_usage': usage})
            matched += okay
        results[name] = {'matched': matched, 'runs': 10, 'pass': matched >= 8}
        print(name, matched, '/ 10', flush=True)
        dump(out / 'results.json', results)
    return 0 if all(value['pass'] for value in results.values()) else 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['freeze', 'run'])
    parser.add_argument('out', type=Path)
    args = parser.parse_args()
    out = args.out.resolve()
    return freeze(out) if args.action == 'freeze' else run(out)


if __name__ == '__main__':
    sys.exit(main())
