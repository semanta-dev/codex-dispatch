#!/usr/bin/env python3
"""Preflight every packet destination, then apply with recoverable rollback."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys


def checked(root, name):
    parts = name.split('/')
    if not name or any(p in ('', '.', '..', '.git', '.codex-dispatch') for p in parts):
        raise ValueError(f'unsafe allowed path: {name}')
    target = root.joinpath(*parts)
    for directory in target.parents:
        if directory == root:
            break
        if directory.is_symlink() or (directory.exists() and not directory.is_dir()):
            raise ValueError(f'unsafe parent directory for {name}')
    return target


def state(path):
    try:
        info = path.lstat()
    except FileNotFoundError:
        return None
    if stat.S_ISLNK(info.st_mode):
        content = os.fsencode(os.readlink(path))
        mode = '120000'
    elif stat.S_ISREG(info.st_mode):
        content = path.read_bytes()
        mode = '100755' if info.st_mode & 0o111 else '100644'
    else:
        raise ValueError(f'unsupported destination type: {path}')
    return {'mode': mode, 'hash': hashlib.sha256(content).hexdigest()}


def git(repo, *args):
    return subprocess.check_output(['git', '--literal-pathspecs', '-C', str(repo), *args])


def capture(parent, base, allowed, manifest):
    records = {}
    for name in allowed.read_text().splitlines():
        if not name:
            continue
        current = state(checked(parent, name))
        entry = git(parent, 'ls-tree', '-z', base, '--', name)
        if entry and entry.split(b'\t', 1)[0].split()[1] != b'blob':
            raise ValueError(f'allowed path is not a file: {name}')
        # Git decides WIP using its normalization rules (e.g. CRLF/filters).
        # Raw states independently protect bytes and modes during fan-in.
        check = subprocess.run(['git', '--literal-pathspecs', '-C', str(parent),
                                'diff', '--quiet', '--no-ext-diff', base, '--', name])
        if check.returncode not in (0, 1):
            raise ValueError(f'cannot inspect parent changes: {name}')
        records[name] = {'state': current, 'wip': check.returncode == 1 or (not entry and current is not None)}
    manifest.write_text(json.dumps(records))


def copy_entry(src, dst):
    if src.is_symlink():
        dst.symlink_to(os.readlink(src))
    else:
        shutil.copy2(src, dst, follow_symlinks=False)


def apply(parent, worktree, manifest, changed, recovery):
    records = json.loads(manifest.read_text())
    tokens = changed.read_bytes().split(b'\0')
    if tokens[-1] != b'' or (len(tokens) - 1) % 2:
        raise ValueError('invalid changed-status evidence')
    names = list(dict.fromkeys(os.fsdecode(tokens[i]) for i in range(1, len(tokens) - 1, 2)))
    operations = []
    # No parent file is touched until ALL destinations and sources pass.
    for name in names:
        if name not in records:
            raise ValueError(f'path outside allowed files: {name}')
        dst, src = checked(parent, name), checked(worktree, name)
        record = records[name]
        if record['wip']:
            raise ValueError(f'parent {name} has uncommitted changes')
        if state(dst) != record['state']:
            raise ValueError(f'fan-in conflict for {name}')
        operations.append((name, state(src)))
    recovery.mkdir()
    (recovery / 'plan.json').write_text(json.dumps(operations))
    # Preserve both candidate output and original destinations before writes.
    for i, (name, after) in enumerate(operations):
        if records[name]['state'] is not None:
            copy_entry(checked(parent, name), recovery / f'{i}.before')
        if after is not None:
            copy_entry(checked(worktree, name), recovery / f'{i}.after')
            if state(recovery / f'{i}.after') != after:
                raise ValueError(f'source changed during fan-in: {name}')
    applied, created = [], []
    try:
        for i, (name, after) in enumerate(operations):
            dst = checked(parent, name)
            if state(dst) != records[name]['state']:
                raise ValueError(f'fan-in conflict for {name}')
            missing = []
            directory = dst.parent
            while not directory.exists():
                missing.append(directory)
                directory = directory.parent
            for directory in reversed(missing):
                directory.mkdir()
                created.append(directory)
            applied.append(i) # Record intent before the filesystem mutation.
            if after is None:
                if dst.exists() or dst.is_symlink():
                    dst.unlink()
            else:
                prepared = recovery / f'{i}.install'
                copy_entry(recovery / f'{i}.after', prepared)
                os.replace(prepared, dst)
        (recovery / 'status').write_text('complete\n')
    except BaseException:
        failed = []
        for i in reversed(applied):
            name, after = operations[i]
            try:
                dst = checked(parent, name)
                actual = state(dst)
                if actual == records[name]['state']:
                    continue
                if actual != after:
                    raise ValueError('destination changed after installation')
                if records[name]['state'] is None:
                    dst.unlink()
                else:
                    prepared = recovery / f'{i}.restore'
                    copy_entry(recovery / f'{i}.before', prepared)
                    os.replace(prepared, dst)
            except (OSError, ValueError) as exc:
                failed.append(f'{name}: {exc}')
        for directory in reversed(created):
            try:
                directory.rmdir()
            except OSError:
                pass
        (recovery / 'status').write_text('rollback incomplete: ' + repr(failed) if failed else 'rolled back\n')
        raise


def main():
    mode, *args = sys.argv[1:]
    try:
        if mode == 'capture':
            parent, base, allowed, manifest = args
            capture(Path(parent), base, Path(allowed), Path(manifest))
        elif mode == 'apply':
            apply(*(Path(arg) for arg in args))
        else:
            raise ValueError('unknown fan-in operation')
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        print(f'graphrag-worktree-dispatch: {exc}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
