#!/usr/bin/env python3
"""Public compact-review entrypoint. Does not modify persistent Claude settings."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
MODEL = 'claude-haiku-4-5-20251001'


REPORT_FIELDS = {
    'kind': {'const': 'review'},
    'verdict': {'enum': ['pass', 'needs-changes', 'fail']},
    'reason': {'type': 'string', 'maxLength': 100},
    'iterations': {'type': 'integer', 'minimum': 1, 'maximum': 10},
    'max_iterations': {'type': 'integer', 'minimum': 1, 'maximum': 10},
    'files_changed': {'type': 'array', 'items': {'type': 'string'}},
    'session_id': {'type': 'string'},
    'run_dir': {'type': 'string'},
    'fell_back_to_fresh': {'type': 'boolean'},
    'feedback': {'type': 'array', 'maxItems': 8, 'items': {'type': 'string', 'maxLength': 1000}},
}
REPORT_SCHEMA = {'type': 'object', 'oneOf': [
    {'properties': REPORT_FIELDS, 'required': list(REPORT_FIELDS), 'additionalProperties': False},
    {'properties': {'kind': {'const': 'background'}, 'output': {'type': 'string'}},
     'required': ['kind', 'output'], 'additionalProperties': False},
]}


def validate_shape(report):
    if not isinstance(report, dict):
        raise ValueError('missing structured review report')
    if report.get('kind') == 'background':
        if set(report) != {'kind', 'output'} or not isinstance(report['output'], str):
            raise ValueError('invalid background report')
        return
    if set(report) != set(REPORT_FIELDS) or report.get('kind') != 'review' or report.get('verdict') not in REPORT_FIELDS['verdict']['enum']:
        raise ValueError('invalid structured review fields')
    for key in ['reason', 'session_id', 'run_dir']:
        if not isinstance(report[key], str):
            raise ValueError('invalid report ' + key)
    if len(report['reason']) > 100 or type(report['fell_back_to_fresh']) is not bool:
        raise ValueError('invalid report reason/fallback')
    for key in ['iterations', 'max_iterations']:
        if type(report[key]) is not int or not 1 <= report[key] <= 10:
            raise ValueError('invalid report iteration budget')
    if report['iterations'] > report['max_iterations']:
        raise ValueError('report exceeds iteration budget')
    for key in ['files_changed', 'feedback']:
        if not isinstance(report[key], list) or any(not isinstance(value, str) for value in report[key]):
            raise ValueError('invalid report ' + key)
    if len(report['feedback']) > 8 or any(len(value) > 1000 for value in report['feedback']):
        raise ValueError('report feedback exceeds schema')
    if report['verdict'] == 'pass' and (report['reason'] or report['feedback']):
        raise ValueError('pass report must have empty reason and feedback')


def validate_result(final, prompt):
    if final.get('subtype') != 'success' or final.get('is_error') or final.get('terminal_reason') != 'completed':
        raise ValueError('Claude did not complete structured review')
    report = final.get('structured_output')
    validate_shape(report)
    sid = final.get('session_id', '')
    import uuid
    sid = str(uuid.UUID(sid))
    repo = Path(subprocess.check_output(['git', 'rev-parse', '--show-toplevel'], text=True).strip()).resolve()
    receipts = [path for path in (repo / '.codex-dispatch/expansions' / sid).glob('*.json') if not path.name.endswith('.runs.json')]
    if len(receipts) != 1:
        raise ValueError('current command receipt missing or ambiguous')
    saved = json.loads(receipts[0].read_text())
    payload = saved['payload']
    identity = payload['identity']
    raw = prompt.removeprefix('/codex-dispatch:codex ')
    if saved['status'] != 'complete' or identity != saved['identity'] or identity['session_id'] != sid or identity['args_sha256'] != hashlib.sha256(raw.encode()).hexdigest() or Path(identity['repo']).resolve() != repo:
        raise ValueError('current command receipt identity mismatch')
    if report['kind'] != payload['kind']:
        raise ValueError('report does not match command route')
    if report['kind'] == 'background':
        if report['output'] != payload['output']:
            raise ValueError('background output changed')
        return report
    if report['max_iterations'] != payload['config']['max_iter']:
        raise ValueError('review budget changed')
    ledger = json.loads(receipts[0].with_suffix('.runs.json').read_text())
    attempts = ledger['attempts']
    if ledger['identity'] != identity or len(attempts) != report['iterations'] or any(row['state'] != 'complete' or row['iteration'] != index + 1 for index, row in enumerate(attempts)):
        raise ValueError('review count does not match completed invocation chain')
    if not attempts or Path(attempts[0]['run_dir']).resolve() != Path(payload['run_dir']).resolve():
        raise ValueError('review chain does not start at current receipt')
    run = Path(report['run_dir']).resolve()
    if Path(attempts[-1]['run_dir']).resolve() != run or attempts[-1]['session_id'] != report['session_id']:
        raise ValueError('reported run is not current invocation chain tail')
    if run.parent != (repo / '.codex-dispatch/runs').resolve():
        raise ValueError('review run outside current repository')
    result = json.loads((run / 'result.json').read_text())
    if report['session_id'] != result['session_id'] or report['files_changed'] != result['files_changed'] or report['fell_back_to_fresh'] != result.get('fell_back_to_fresh', False):
        raise ValueError('report not bound to dispatch result')
    if report['verdict'] == 'pass':
        bundle = json.loads((run / 'review-evidence.json').read_text())
        if type(result['exit_code']) is not int or result['exit_code'] != 0 or bundle.get('complete') is not True or bundle.get('result') != result or bundle.get('verification_mutations'):
            raise ValueError('pass lacks complete successful evidence')
        if not result['files_changed']:
            raise ValueError('pass has no meaningful changes')
        config = payload['config']
        for label, required in [('test', payload['dispatch_env']['REVIEW_TEST_POLICY'] != 'skip' and bool(payload['dispatch_env']['REVIEW_TEST_CMD'])), ('verification', bool(config['verify_cmd']))]:
            evidence = bundle.get(label)
            if required and (not isinstance(evidence, dict) or type(evidence.get('exit_code')) is not int or evidence['exit_code'] != 0):
                raise ValueError('pass lacks successful ' + label)
    return report


def render(report):
    if report['kind'] == 'background':
        return report['output']
    return '\n'.join(['codex dispatch finished',
        f"- iterations: {report['iterations']} / {report['max_iterations']}",
        f"- verdict: {report['verdict']}", f"- reason: {report['reason']}",
        '- files changed: ' + ', '.join(report['files_changed']),
        f"- session id: {report['session_id']}",
        '- fell back to fresh: ' + str(report['fell_back_to_fresh']).lower(),
        f"- run artifacts: {report['run_dir']}", *report['feedback']])


def command(output_format):
    argv = ['claude', '--print', '--model', MODEL, '--plugin-dir', str(ROOT),
            '--system-prompt-file', str(ROOT / 'scripts/compact-review-system.md'),
            '--tools', 'Skill,Bash,Read,Grep,Glob', '--strict-mcp-config', '--setting-sources', '',
            '--permission-mode', 'dontAsk', '--allowedTools', 'Skill', 'Bash', 'Read', 'Grep', 'Glob',
            '--output-format', output_format, '--json-schema', json.dumps(REPORT_SCHEMA)]
    if output_format == 'stream-json':
        argv.append('--verbose')
    return argv


def environment():
    return {**os.environ, 'MAX_THINKING_TOKENS': '0', 'MAX_STRUCTURED_OUTPUT_RETRIES': '1'}


def main():
    parser = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument('--output-format', choices=['text', 'json', 'stream-json'], default='text')
    parser.add_argument('--stdin-request', action='store_true', help='read one complete /codex-dispatch:codex invocation as data from stdin')
    args, task_args = parser.parse_known_args()
    if args.stdin_request:
        if task_args:
            parser.error('--stdin-request cannot be combined with task arguments')
        prompt = sys.stdin.read()
        if not prompt.startswith('/codex-dispatch:codex '):
            parser.error('stdin must contain one /codex-dispatch:codex invocation')
    else:
        if not task_args:
            parser.error('provide /codex flags and a task, or --stdin-request')
        prompt = '/codex-dispatch:codex ' + shlex.join(task_args)
    # Claude's structured result is validated before human-readable success.
    # Streaming mode preserves the raw vendor evidence; callers must also check
    # this process's exit status and the final local validation event.
    proc = subprocess.Popen(command('stream-json'), stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, env=environment())
    proc.stdin.write(prompt)
    proc.stdin.close()
    final = {}
    try:
        for line in proc.stdout:
            if args.output_format == 'stream-json':
                print(line, end='', flush=True)
            try:
                event = json.loads(line)
                if event.get('type') == 'result':
                    final = event
            except ValueError:
                pass
        code = proc.wait()
        if code:
            raise ValueError(f'Claude exited {code}')
        report = validate_result(final, prompt)
    except (OSError, ValueError, KeyError, TypeError) as error:
        print('codex review failed: ' + str(error), file=sys.stderr)
        if args.output_format == 'stream-json':
            print(json.dumps({'type': 'codex_review_validation', 'valid': False, 'error': str(error)}))
        return 65
    if args.output_format == 'stream-json':
        print(json.dumps({'type': 'codex_review_validation', 'valid': True}))
    elif args.output_format == 'json':
        print(json.dumps(report))
    else:
        print(render(report))
    return 0 if report['kind'] == 'background' or report['verdict'] == 'pass' else 1


if __name__ == '__main__':
    sys.exit(main())
