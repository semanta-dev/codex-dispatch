"""Confined verification with finite execution and output budgets.

Verification sees a disposable snapshot of nonignored repository files, no host
HOME, reviewer environment, Git credentials, runtime receipts, or network.
Unsupported backends fail closed. There is deliberately no host fallback.
"""
import ctypes
import contextlib
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import select
import stat
import subprocess
import sys
import tempfile
import threading
import time

GIT = ['git', '--no-replace-objects', '-c', 'core.fsmonitor=false', '-c', 'core.hooksPath=' + os.devnull, '-c', 'protocol.allow=never']

MAX_OUTPUT = 8 * 1024 * 1024
TAIL = 8000


def minimal_environment():
    return {'PATH': '/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin',
            'HOME': '/tmp/home', 'TMPDIR': '/tmp', 'LANG': 'C.UTF-8',
            'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null',
            'GIT_TERMINAL_PROMPT': '0', 'PYTHONDONTWRITEBYTECODE': '1',
            'GOCACHE': '/tmp/go-cache', 'GOPATH': '/tmp/go'}


def tail(path, limit=TAIL):
    with Path(path).open('rb') as stream:
        stream.seek(max(0, os.fstat(stream.fileno()).st_size - limit))
        return stream.read(limit).decode(errors='replace')


def protect_process():
    # Prevent an unconfined same-UID launch child from reopening anonymous
    # control pipes through /proc/<parent>/fd or reading controller memory.
    # Keep this protection for the controller lifetime, including handoff.
    if sys.platform == 'linux':
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.prctl(4, 0, 0, 0, 0):  # PR_SET_DUMPABLE
            raise OSError(ctypes.get_errno(), 'cannot protect process control channels')


def kill_tree(proc):
    if os.name == 'nt':
        subprocess.run(['taskkill', '/PID', str(proc.pid), '/T', '/F'],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
    elif sys.platform == 'linux':
        # The dedicated owner drains even descendants that escape its group.
        if proc.poll() is None:
            proc.send_signal(signal.SIGTERM)
            try:
                proc.wait(timeout=4)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
    else:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    proc.wait(timeout=10)


def supervised(argv, cwd, env, stdout, stderr, timeout=120, cancel=None, admission_lock=None, background_handoff=False, handoff_record=None, on_handoff=None):
    """Disk-backed capture; interruption always drains the owned process group.

    Confined commands additionally live in a PID namespace whose init dies with
    the supervisor, covering descendants that create their own process groups.
    """
    protect_process()
    if timeout <= 0:
        raise ValueError('execution deadline exhausted')
    options = {'creationflags': subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == 'nt' else {'start_new_session': True}
    started = time.monotonic()
    reason = ''
    transferred = None
    handoff = Path(stdout).parent if background_handoff else None
    ready_read = ready_write = commit_read = commit_write = None
    if handoff is not None and sys.platform != 'linux':
        raise ValueError('native background ownership handoff is not yet supported')
    task_ready = None
    if handoff is not None and Path(stdout).name != 'stdout':
        raise ValueError('background handoff requires owned stdout capture')
    previous = {}
    proc = None
    cleanup_seconds = 0
    def interrupted(signum, frame):
        if transferred is None:
            raise KeyboardInterrupt(f"execution interrupted by signal {signum}")
    try:
        if threading.current_thread() is threading.main_thread():
            for signum in [signal.SIGINT, signal.SIGTERM]:
                previous[signum] = signal.signal(signum, interrupted)
        with Path(stdout).open('wb') as out, Path(stderr).open('wb') as err:
            with admission_lock or contextlib.nullcontext():
                if cancel is not None and cancel.is_set():
                    reason = 'cancelled'
                elif time.monotonic() - started >= timeout:
                    reason = 'timeout'
                else:
                    handoff_args = []
                    if background_handoff:
                        ready_read, ready_write = os.pipe()
                        commit_read, commit_write = os.pipe()
                        handoff_args = ['--handoff-background', str(ready_write), str(commit_read), str(stdout)]
                        options['pass_fds'] = (ready_write, commit_read)
                    owned_argv = [sys.executable, '-I', str(Path(__file__).with_name('owned_process.py')), str(os.getpid()), *handoff_args, *argv] if sys.platform == 'linux' else argv
                    proc = subprocess.Popen(owned_argv, cwd=cwd, env=env, stdout=out, stderr=err, **options)
                    for descriptor in [ready_write, commit_read]:
                        if descriptor is not None:
                            os.close(descriptor)
                    ready_write = commit_read = None
            while proc is not None and proc.poll() is None:
                if time.monotonic() - started >= timeout:
                    reason = 'timeout'
                    break
                if cancel is not None and cancel.is_set():
                    reason = 'cancelled'
                    break
                if ready_read is not None and task_ready is None and select.select([ready_read], [], [], 0)[0]:
                    task_ready = os.read(ready_read, 256).decode().strip()
                if task_ready and transferred is None:
                    with admission_lock or contextlib.nullcontext():
                        if cancel is not None and cancel.is_set():
                            reason = 'cancelled'
                            break
                        if time.monotonic() - started >= timeout:
                            reason = 'timeout'
                            break
                        if not __import__('re').fullmatch(r't_[0-9a-f]+', task_ready):
                            reason = 'invalid-handoff'
                            break
                        blocked = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT, signal.SIGTERM})
                        try:
                            record = {'state': 'handoff-prepared', 'task_id': task_ready}
                            target = Path(handoff_record) if handoff_record is not None else None
                            if target is not None:
                                target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                                temporary = target.with_suffix('.pending')
                                with temporary.open('x') as saved:
                                    json.dump(record, saved)
                                    saved.flush()
                                    os.fsync(saved.fileno())
                                os.replace(temporary, target)
                            os.write(commit_write, (task_ready + '\n').encode())
                            transferred = {**record, 'state': 'transferred'}
                            if on_handoff is not None:
                                on_handoff(transferred)
                            if target is not None:
                                # This file records the outcome; only the private
                                # pipe above authorizes release by the owner.
                                temporary.write_text(json.dumps(transferred))
                                os.replace(temporary, target)
                        finally:
                            signal.pthread_sigmask(signal.SIG_SETMASK, blocked)
                if os.fstat(out.fileno()).st_size + os.fstat(err.fileno()).st_size > MAX_OUTPUT:
                    reason = 'output-limit'
                    break
                time.sleep(.02)
    finally:
        for signum in previous:
            signal.signal(signum, signal.SIG_IGN)
        try:
            if proc is not None:
                cleanup_started = time.monotonic()
                kill_tree(proc)
                cleanup_seconds = time.monotonic() - cleanup_started
        finally:
            for descriptor in [ready_read, ready_write, commit_read, commit_write]:
                if descriptor is not None:
                    os.close(descriptor)
            for signum, handler in previous.items():
                signal.signal(signum, handler)
    if transferred is not None:
        reason = ''
    elif cancel is not None and cancel.is_set():
        reason = 'cancelled'
    elif time.monotonic() - started >= timeout:
        reason = 'timeout'
    if Path(stdout).stat().st_size + Path(stderr).stat().st_size > MAX_OUTPUT:
        reason = 'output-limit'
        for path in [stdout, stderr]:
            with Path(path).open('r+b') as stream:
                stream.truncate(min(os.fstat(stream.fileno()).st_size, MAX_OUTPUT // 2))
    return {'exit_code': 0 if transferred is not None else 124 if reason == 'timeout' else 130 if reason == 'cancelled' else 125 if reason else proc.returncode,
            'cleanup_s': round(cleanup_seconds, 3), 'owned_exit_code': proc.returncode if proc is not None else None,
            'failure_kind': reason, 'background_handoff': transferred, 'stdout': tail(stdout), 'stderr': tail(stderr),
            'stdout_path': str(stdout), 'stderr_path': str(stderr),
            'output_truncated': Path(stdout).stat().st_size > TAIL or Path(stderr).stat().st_size > TAIL,
            'wall_s': round(time.monotonic() - started, 3)}


MAX_FILE_BYTES = 128 * 1024 * 1024
MAX_TREE_BYTES = 512 * 1024 * 1024
MAX_FILES = 100000


def check_deadline(deadline):
    if time.monotonic() >= deadline:
        raise ValueError('verification snapshot/audit deadline exhausted')


@contextlib.contextmanager
def parent_handle(root, relative):
    handle = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in relative.parts[:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=handle)
            os.close(handle)
            handle = child
        yield handle
    finally:
        os.close(handle)


def regular_bytes(handle, name, deadline):
    descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=handle)
    with os.fdopen(descriptor, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > MAX_FILE_BYTES:
            raise ValueError('special or oversized verification input/output: ' + name)
        consumed = 0
        while True:
            check_deadline(deadline)
            data = stream.read(1024 * 1024)
            if not data:
                break
            consumed += len(data)
            if consumed > MAX_FILE_BYTES:
                raise ValueError('verification file grew beyond quota')
            yield data


def snapshot(repo, destination, deadline):
    names = subprocess.check_output(GIT + ['ls-files', '--cached', '--others', '--exclude-standard', '-z'], cwd=repo, env=minimal_environment(), timeout=max(.01, deadline-time.monotonic()))
    total = 0
    paths = set(names.split(b'\0')) - {b''}
    if len(paths) > MAX_FILES:
        raise ValueError('verification input file count exceeds quota')
    for raw in paths:
        check_deadline(deadline)
        name = os.fsdecode(raw)
        relative = Path(name)
        if relative.is_absolute() or '..' in relative.parts:
            raise ValueError('unsafe repository path')
        if relative.parts[0] in {'.codex-dispatch', '.git'}:
            continue
        target = destination / relative
        with parent_handle(repo, relative) as handle:
            try:
                info = os.stat(relative.name, dir_fd=handle, follow_symlinks=False)
            except FileNotFoundError:
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            if stat.S_ISLNK(info.st_mode):
                target.symlink_to(os.readlink(relative.name, dir_fd=handle))
            elif stat.S_ISREG(info.st_mode):
                with target.open('xb') as output:
                    for data in regular_bytes(handle, relative.name, deadline):
                        total += len(data)
                        if total > MAX_TREE_BYTES:
                            raise ValueError('verification input bytes exceed quota')
                        output.write(data)
                target.chmod(stat.S_IMODE(info.st_mode) & 0o777)
            else:
                raise ValueError('special repository file')


def tree_state(root, deadline):
    result = {}
    total = 0
    for directory, dirs, files in os.walk(root, followlinks=False):
        for name in dirs + files:
            check_deadline(deadline)
            path = Path(directory) / name
            relative = path.relative_to(root)
            with parent_handle(root, relative) as handle:
                info = os.stat(name, dir_fd=handle, follow_symlinks=False)
                if stat.S_ISDIR(info.st_mode):
                    continue
                digest = hashlib.sha256()
                if stat.S_ISLNK(info.st_mode):
                    digest.update(os.fsencode(os.readlink(name, dir_fd=handle)))
                elif stat.S_ISREG(info.st_mode):
                    for data in regular_bytes(handle, name, deadline):
                        total += len(data)
                        if total > MAX_TREE_BYTES:
                            raise ValueError('verification output bytes exceed quota')
                        digest.update(data)
                else:
                    raise ValueError('special verification output: ' + str(relative))
                result[str(relative)] = [info.st_mode, digest.hexdigest()]
                if len(result) > MAX_FILES:
                    raise ValueError('verification output file count exceeds quota')
    return result


def verify(command, repo, cwd, stdout, stderr, timeout=120, cancel=None, admission_lock=None):
    deadline = time.monotonic() + timeout
    repo, cwd = Path(repo).resolve(), Path(cwd).resolve()
    relative = cwd.relative_to(repo)
    base = {'command': command, 'cwd': str(cwd), 'confinement': 'unsupported', 'mutations': []}
    if sys.platform != 'linux' or not Path('/usr/bin/bwrap').is_file():
        return {**base, 'exit_code': 126, 'failure_kind': 'sandbox-unavailable',
                'stdout': '', 'stderr': 'Confined verification requires a supported sandbox backend; host execution is disabled.'}
    with tempfile.TemporaryDirectory(prefix='codex-verification-') as temporary:
        workspace = Path(temporary) / 'workspace'
        workspace.mkdir()
        snapshot(repo, workspace, deadline)
        (workspace / relative).mkdir(parents=True, exist_ok=True)
        before = tree_state(workspace, deadline)
        argv = ['/usr/bin/bwrap', '--unshare-all', '--die-with-parent', '--new-session',
                '--ro-bind', '/usr', '/usr', '--proc', '/proc', '--dev', '/dev', '--tmpfs', '/tmp',
                '--dir', '/tmp/home']
        for name in ['bin', 'sbin', 'lib', 'lib64']:
            path = Path('/') / name
            if path.is_symlink():
                argv += ['--symlink', os.readlink(path), str(path)]
            elif path.exists():
                argv += ['--ro-bind', str(path), str(path)]
        argv += ['--bind', str(workspace), str(repo), '--chdir', str(cwd), '--remount-ro', '/', '--clearenv']
        for name, value in minimal_environment().items():
            argv += ['--setenv', name, value]
        argv += ['/bin/bash', '--noprofile', '--norc', '-c', command]
        result = supervised(argv, repo, minimal_environment(), stdout, stderr, max(.01, deadline-time.monotonic()), cancel=cancel, admission_lock=admission_lock)
        if result['exit_code'] != 0 and result['stderr'].startswith('bwrap:'):
            result['failure_kind'] = 'sandbox-initialization-failed'
            result['stderr'] += 'Sandbox initialization failed. Check bubblewrap and host user-namespace/AppArmor prerequisites; verification remains unprivileged and host execution is disabled.\n'
        if result["failure_kind"]:
            return {**base, **result, "confinement": "linux-bwrap-v1"}
        after = tree_state(workspace, deadline)
        changed = sorted(name for name in before.keys() | after.keys() if before.get(name) != after.get(name))
        return {**base, **result, 'confinement': 'linux-bwrap-v1', 'mutations': changed}
