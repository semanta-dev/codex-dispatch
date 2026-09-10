#!/usr/bin/env python3
"""Freeze and evaluate the deployed Haiku decision rubric, independent of retries."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
MODEL = 'claude-haiku-4-5-20251001'
def dump(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def judgment(text):
    pairs = re.findall(r'^(VERDICT|REASON):[ \t]*([^\n]*)', text, re.M)
    if len(pairs) != 2 or {key for key, _ in pairs} != {'VERDICT', 'REASON'}:
        return {}
    return {key: value.strip() for key, value in pairs}


def freeze(out):
    out.mkdir(parents=True, exist_ok=False)
    shutil.copytree(ROOT / 'tests/fixtures/reviewer', out / 'fixtures')
    contract = (ROOT / 'scripts/direct-review-contract.md').read_text()
    rubric = contract.split('Apply these checks yourself.', 1)[1].split('## Repair only', 1)[0]
    (out / 'rubric.md').write_text('Apply these checks yourself.' + rubric)
    shutil.copy2(ROOT / 'scripts/compact-review-system.md', out / 'system.md')
    shutil.copy2(ROOT / 'scripts/codex-reviewed.py', out / 'profile.py')
    shutil.copy2(ROOT / 'scripts/api-review.py', out / 'api-review.py')
    spec = importlib.util.spec_from_file_location('api_review', out / 'api-review.py')
    api = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(api)
    api.ROOT = ROOT
    (out / 'api-system.md').write_text(api.system_prompt())
    shutil.copy2(__file__, out / 'direct-fixtures.py')
    dump(out / 'manifest.json', {'model': MODEL, 'thinking_tokens': 0, 'structured_output_retries': 0, 'review_transport': 'api', 'review_endpoint': api.transport()[0], 'runs_per_fixture': 10, 'threshold': 8, 'source_root': str(ROOT),
                               'contract_sha256': hashlib.sha256(contract.encode()).hexdigest(),
                               'candidate': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
                               'fixtures': sorted(p.name for p in (out / 'fixtures').iterdir())})
    dump(out / 'frozen-sha256.json', {p.relative_to(out).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest() for p in out.rglob('*') if p.is_file()})


def fixture_payload(fixture, test):
    """Build exact reviewer input from frozen files and explicit test evidence."""
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
    bundle = {'complete': True, 'result': result, 'diff': diff, 'test': test, 'verification': None,
              'verification_mutations': [], 'changed_file_facts': {}}
    return {'TASK': read('task.txt'), 'ACCEPTANCE': read('acceptance.txt'), 'CONSTRAINTS': read('constraints.txt'),
            'FILES': '', 'TEST_POLICY': policy, 'TEST_CMD': command, 'VERIFY_CMD': '', 'CLEAN_VERIFY': False,
            'bundle': bundle, 'result': result, 'previous_bundle': None}


def fixture_input(fixture, work):
    """Execute the fixture test once, then use the pure canonical payload."""
    def read(name, default=""):
        path = fixture / name
        return path.read_text().strip() if path.exists() else default
    policy, command = read("test-policy.txt", "skip"), read("test-cmd.txt")
    test = None
    if policy != "skip" and command:
        proc = subprocess.run(["bash", "-c", command], cwd=work, capture_output=True, text=True, timeout=30)
        test = {"command": command, "exit_code": proc.returncode, "stdout": proc.stdout, "stderr": proc.stderr}
    return fixture_payload(fixture, test)


def run(out):
    for name, digest in json.loads((out / 'frozen-sha256.json').read_text()).items():
        if hashlib.sha256((out / name).read_bytes()).hexdigest() != digest:
            raise ValueError('frozen fixture input changed: ' + name)
    (out / 'started').open('x').close()
    spec = importlib.util.spec_from_file_location('api_review', out / 'api-review.py')
    api = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(api)
    manifest = json.loads((out / 'manifest.json').read_text())
    if api.transport()[0] != manifest['review_endpoint']:
        raise ValueError('frozen API transport changed')
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
            error = None
            got = {}
            try:
                report = api.judge(payload, prefix, system=(out / 'api-system.md').read_text())
                got = {'VERDICT': report['verdict'], 'REASON': report['reason']}
                okay = got == expected
            except Exception as failure:
                okay, error = False, str(failure)
            dump(archive / (str(iteration) + '.judgment.json'), {'expected': expected, 'got': got, 'match': okay, 'error': error})
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
