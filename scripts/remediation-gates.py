#!/usr/bin/env python3
"""Run development regression gates and retain evidence for adversarial review.

This generates test evidence, never reviewer approval or a promotion record.
Unsupported confinement remains a failed qualification gate.
"""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
SUITES = {
    'confinement-lifecycle-budget': ('F01', 'F02', 'F07', 'test_execution_policy.py'),
    'background-handoff': ('F02', 'test_background_handoff.py'),
    'clean-materialization': ('F01', 'F05', 'test_clean_materialization.py'),
    'live-acceptance': ('F03', 'test_compact_profile.py'),
    'packet-inventory': ('F04', 'test_plan_evidence.py'),
    'directory-terminal-evidence': ('F05', 'F06', 'test_review_evidence.py'),
    'terminal-api-history': ('F06', 'test_api_review.py'),
    'route-conformance': ('F10', 'test_agent_routes.py'),
    'promotion-rejections': ('test_promotion_gate.py',),
    'development-gate-integrity': ('test_remediation_gates.py',),
}


def validate_output(output):
    output, source = output.resolve(), ROOT.resolve()
    if output.is_relative_to(source) or source.is_relative_to(output):
        raise ValueError('evidence output must be outside the repository and cannot be its ancestor')
    return output


def source_manifest():
    paths = subprocess.check_output(['git', '-c', 'core.fsmonitor=false', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'], cwd=ROOT)
    files = {}
    for raw in sorted(set(paths.split(b'\0')) - {b''}):
        relative = Path(raw.decode())
        path = ROOT / relative
        if not path.exists() and not path.is_symlink():
            files[str(relative)] = {'kind': 'deleted'}
            continue
        data = str(path.readlink()).encode() if path.is_symlink() else path.read_bytes()
        files[str(relative)] = {'mode': path.lstat().st_mode, 'sha256': hashlib.sha256(data).hexdigest()}
    return {'commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
            'index_sha256': hashlib.sha256(subprocess.check_output(['git', 'ls-files', '--stage', '-z'], cwd=ROOT)).hexdigest(),
            'files': files}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    try:
        args.out = validate_output(args.out)
    except ValueError as error:
        parser.error(str(error))
    args.out.mkdir(parents=True, exist_ok=True)
    source_before = source_manifest()
    checks = []
    for name, contract in SUITES.items():
        log = args.out / (name + '.log')
        argv = [sys.executable, '-m', 'unittest', 'discover', '-s', 'tests/python', '-p', contract[-1], '-v']
        started = time.monotonic()
        with log.open('wb') as output:
            try:
                result = subprocess.run(argv, cwd=ROOT, stdout=output, stderr=subprocess.STDOUT, timeout=180)
                code = result.returncode
            except subprocess.TimeoutExpired:
                code = 124
        content = log.read_bytes()
        # unittest exits zero if the pattern matches nothing; that is not proof.
        status = 'pass' if code == 0 and b'Ran 0 tests' not in content and b'... ok' in content and b'... skipped' not in content else 'fail'
        if name in {'confinement-lifecycle-budget', 'clean-materialization'} and sys.platform != 'linux':
            status = 'unsupported'
        checks.append({'id': name, 'finding_ids': list(contract[:-1]), 'command': argv, 'exit_code': code,
                       'status': status, 'artifact': log.name, 'artifact_sha256': hashlib.sha256(content).hexdigest(),
                       'wall_s': round(time.monotonic()-started, 3)})
    source_after = source_manifest()
    source_unchanged = source_before == source_after
    (args.out / 'source-manifest.json').write_text(json.dumps(source_before, sort_keys=True, indent=2) + '\n')
    record = {'source_unchanged': source_unchanged, 'source_manifest_sha256': hashlib.sha256((args.out / 'source-manifest.json').read_bytes()).hexdigest(), 'schema_version': 1, 'kind': 'development-regression-evidence',
              'candidate_commit': source_before['commit'],
              'working_tree_dirty': bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT)),
              'platform': platform.platform(), 'checks': checks,
              'decision': 'independent-review-required' if source_unchanged else 'invalid-source-drift',
              'unfulfilled_gates': ['six-platform native qualification', 'selected published artifact smoke',
                                   'nine reviewer fixtures', 'fresh paired 30+30 cohort', 'independent CTO and 10x GO']}
    (args.out / 'development-gates.json').write_text(json.dumps(record, indent=2) + '\n')
    print(json.dumps({'decision': record['decision'], 'checks': {row['id']: row['status'] for row in checks}}))
    return 0 if source_unchanged and all(row['status'] == 'pass' for row in checks) else 1


if __name__ == '__main__':
    sys.exit(main())
