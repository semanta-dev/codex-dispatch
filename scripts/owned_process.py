#!/usr/bin/env python3
"""Linux process owner. Used only by execution_policy, never as a sandbox.

A dedicated subreaper owns one command and adopted double-forked descendants.
Parent death requests cleanup. Confinement remains execution_policy's job.
"""
import ctypes
import os
import re
import select
from pathlib import Path
import signal
import subprocess
import sys
import time


def descendants(pid):
    found = set()
    pending = [pid]
    while pending:
        parent = pending.pop()
        for task in Path(f'/proc/{parent}/task').glob('*'):
            try:
                children = map(int, (task / 'children').read_text().split())
                for child in children:
                    if child not in found:
                        found.add(child)
                        pending.append(child)
            except (FileNotFoundError, ProcessLookupError):
                pass
    return found


def drain():
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        children = descendants(os.getpid())
        if not children:
            return True
        for child in children:
            try:
                os.kill(child, signal.SIGKILL)
            except ProcessLookupError:
                pass
        while True:
            try:
                child, _ = os.waitpid(-1, os.WNOHANG)
                if child == 0:
                    break
            except ChildProcessError:
                break
        time.sleep(.01)
    remaining = descendants(os.getpid())
    if remaining:
        # PID/state diagnostics identify a failed drain without exposing argv,
        # environment, or repository data in captured verification logs.
        states = []
        for child in sorted(remaining):
            try:
                status = Path(f'/proc/{child}/status').read_text()
                fields = [line for line in status.splitlines() if line.startswith(('State:', 'PPid:', 'NSpid:'))]
                states.append(f'{child}: ' + ', '.join(fields))
            except (FileNotFoundError, ProcessLookupError):
                continue
        print('owned-process: descendant drain deadline: ' + '; '.join(states), file=sys.stderr, flush=True)
    return not remaining


def main():
    expected_parent = int(sys.argv[1])
    command = sys.argv[2:]
    handoff = None
    ready_fd = commit_fd = None
    if command[:1] == ['--handoff-background']:
        ready_fd, commit_fd = int(command[1]), int(command[2])
        handoff = Path(command[3])
        command = command[4:]
        if len(command) != 3 or Path(command[1]).resolve() != Path(__file__).with_name('dispatch-codex.sh').resolve() or command[2] != '--detach':
            return 125
    task_id = None
    transferred = False
    def acknowledge(timeout):
        if task_id is None or commit_fd is None:
            return False
        readable, _, _ = select.select([commit_fd], [], [], timeout)
        if readable:
            return os.read(commit_fd, 256) == (task_id + '\n').encode()
        return False
    def interrupted(signum, frame):
        raise KeyboardInterrupt()
    for signum in [signal.SIGTERM, signal.SIGINT, signal.SIGHUP]:
        signal.signal(signum, interrupted)
    libc = ctypes.CDLL(None, use_errno=True)
    # PR_SET_CHILD_SUBREAPER and PR_SET_PDEATHSIG. Check parent again to close
    # the launch-to-registration race rather than orphaning a command.
    if libc.prctl(4, 0, 0, 0, 0) or libc.prctl(36, 1, 0, 0, 0) or libc.prctl(1, signal.SIGTERM, 0, 0, 0):
        return 125
    if os.getppid() != expected_parent:
        return 130
    code = 125
    try:
        child = subprocess.Popen(command)
        code = child.wait()
        if handoff is not None and code == 0:
            with handoff.open('rb') as log:
                log.seek(max(0, handoff.stat().st_size - 256))
                task_id = log.read(256).decode().strip()
            if re.fullmatch(r't_[0-9a-f]+', task_id) is None:
                task_id = None
                code = 125
            else:
                os.write(ready_fd, (task_id + '\n').encode())
                transferred = acknowledge(5)
                if not transferred:
                    code = 125
    except KeyboardInterrupt:
        code = 130
    except OSError as error:
        print(str(error), file=sys.stderr)
        code = 127
    finally:
        for signum in [signal.SIGTERM, signal.SIGINT, signal.SIGHUP]:
            signal.signal(signum, signal.SIG_IGN)
        # The command never inherits these private descriptors (Popen defaults
        # close_fds=True). Workspace records do not authorize ownership transfer.
        transferred = transferred or acknowledge(0)
        if transferred:
            code = 0
        elif not drain():
            code = 125
        for descriptor in [ready_fd, commit_fd]:
            if descriptor is not None:
                os.close(descriptor)
    return code if code >= 0 else 128 - code


if __name__ == '__main__':
    sys.exit(main())
