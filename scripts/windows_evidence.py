"""Finite Windows live evidence: a direct, no-child HANDLE-reading worker.

The parent performs fixed Git plumbing with a timeout and stripped environment.
The isolated worker imports only the trusted file reader, spawns no processes,
and is terminated/reaped directly on timeout. This is not a process-tree sandbox.
"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parent
MAX_REQUEST = 8 * 1024 * 1024
MAX_RESPONSE = 32 * 1024 * 1024
MAX_TREE = 512 * 1024 * 1024


def environment():
    values = {key: value for key in ['SystemRoot', 'WINDIR', 'TEMP', 'TMP', 'PATH']
              if (value := os.environ.get(key)) is not None}
    return {**values, 'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_SYSTEM': os.devnull,
            'GIT_CONFIG_GLOBAL': os.devnull, 'GIT_NO_REPLACE_OBJECTS': '1',
            'GIT_OPTIONAL_LOCKS': '0', 'GIT_NO_LAZY_FETCH': '1',
            'GIT_ALLOW_PROTOCOL': '', 'GIT_TERMINAL_PROMPT': '0'}


def _command():
    return [sys.executable, '-I', str(Path(__file__).resolve()), '--worker']


def _run_worker(packet, timeout):
    data = json.dumps(packet).encode()
    if len(data) > MAX_REQUEST or timeout <= 0:
        raise ValueError('Windows evidence request exceeds budget')
    # Windows communicate(input=...) writes synchronously before timed waits.
    # A stalled reader can fill that pipe forever. A prewritten input file makes
    # the only wait the explicitly timed process wait, even for large requests.
    with tempfile.TemporaryFile() as source, tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        source.write(data)
        source.seek(0)
        proc = subprocess.Popen(_command(), stdin=source, stdout=out, stderr=err, env=environment())
        try:
            proc.wait(timeout=timeout)
        except BaseException:
            proc.kill()
            proc.wait(timeout=5)
            raise
        if out.tell() > MAX_RESPONSE or err.tell() > MAX_RESPONSE:
            raise ValueError('Windows evidence response exceeds quota')
        out.seek(0); err.seek(0)
        response = json.loads(out.read(MAX_RESPONSE + 1))
        if proc.returncode or 'error' in response:
            raise ValueError(response.get('error', 'Windows evidence reader failed'))
        return response


def _git(repo, args, deadline):
    # Resolve outside the repository cwd; Windows CreateProcess must not search
    # the untrusted checkout for a git.exe ahead of PATH.
    git = None
    for directory in os.get_exec_path():
        if not Path(directory).is_absolute():
            continue
        candidate = (Path(directory) / ('git.exe' if os.name == 'nt' else 'git')).resolve()
        if candidate.is_file() and not candidate.is_relative_to(Path(repo).resolve()):
            git = candidate
            break
    if not git:
        raise OSError('Git executable required')
    command = [str(Path(git).resolve()), '--no-replace-objects', '-c', 'core.fsmonitor=false',
               '-c', 'core.hooksPath=' + os.devnull, '-c', 'protocol.allow=never', *args]
    with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
        result = subprocess.run(command, cwd=repo, env=environment(), stdout=output, stderr=errors,
                                timeout=max(.001, deadline-time.monotonic()), check=False)
        if result.returncode:
            raise ValueError('Windows evidence Git inventory failed')
        if output.tell() > MAX_REQUEST or errors.tell() > MAX_REQUEST:
            raise ValueError('Windows evidence Git inventory exceeds quota')
        output.seek(0)
        return output.read(MAX_REQUEST + 1)


def capture(repo, run, timeout=30):
    deadline = time.monotonic() + timeout
    repo, run = Path(repo).absolute(), Path(run).absolute()
    names = _git(repo, ['ls-files', '--cached', '--others', '--exclude-standard', '-z'], deadline)
    index = _git(repo, ['ls-files', '--stage', '-z'], deadline)
    packet = {'repo': str(repo), 'run': str(run), 'names': [os.fsdecode(v) for v in names.split(b'\0') if v]}
    response = _run_worker(packet, deadline-time.monotonic())
    data = {'files': response['files'], 'index_sha256': hashlib.sha256(index).hexdigest()}
    return {**data, 'fingerprint': {'version': 1, 'sha256': hashlib.sha256(json.dumps(data, sort_keys=True).encode()).hexdigest()}}


def worker(packet):
    spec = importlib.util.spec_from_file_location('windows_file_policy', ROOT / 'windows_file_policy.py')
    files = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(files)
    repo, run = Path(packet['repo']), Path(packet['run'])
    names = set(packet['names'])
    if len(names) > 100000:
        raise ValueError('Windows evidence file count exceeds quota')
    state, total = {}, 0
    for name in names:
        path = repo / name
        if name.startswith('.codex-dispatch/') or path == run or run in path.parents:
            continue
        try:
            with files.parent_handle(repo, name) as parent:
                value, size = files.entry_state(parent, Path(name).name, time.monotonic() + 30)
        except FileNotFoundError:
            continue
        total += size
        if total > MAX_TREE:
            raise ValueError('Windows evidence tree exceeds quota')
        state[name] = value
    return {'files': state}


def main():
    if sys.argv[1:] != ['--worker'] or os.name != 'nt':
        return 64
    try:
        raw = sys.stdin.buffer.read(MAX_REQUEST + 1)
        if len(raw) > MAX_REQUEST:
            raise ValueError('Windows evidence request exceeds quota')
        result = json.dumps(worker(json.loads(raw)))
        if len(result.encode()) > MAX_RESPONSE:
            raise ValueError('Windows evidence response exceeds quota')
        print(result)
        return 0
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(json.dumps({'error': str(error)}))
        return 1


if __name__ == '__main__':
    sys.exit(main())
