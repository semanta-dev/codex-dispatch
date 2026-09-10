#!/usr/bin/env python3
"""Public compact-review entrypoint. Does not modify persistent Claude settings."""
import argparse
import os
from pathlib import Path
import shlex
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
MODEL = 'claude-haiku-4-5-20251001'


def command(output_format):
    argv = ['claude', '--print', '--model', MODEL, '--plugin-dir', str(ROOT),
            '--system-prompt-file', str(ROOT / 'scripts/compact-review-system.md'),
            '--tools', 'Skill,Bash,Read,Grep,Glob', '--strict-mcp-config', '--setting-sources', '',
            '--permission-mode', 'dontAsk', '--allowedTools', 'Skill', 'Bash', 'Read', 'Grep', 'Glob',
            '--output-format', output_format]
    if output_format == 'stream-json':
        argv.append('--verbose')
    return argv


def environment():
    return {**os.environ, 'MAX_THINKING_TOKENS': '0'}


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
    return subprocess.run(command(args.output_format), input=prompt, text=True, env=environment()).returncode


if __name__ == '__main__':
    sys.exit(main())
