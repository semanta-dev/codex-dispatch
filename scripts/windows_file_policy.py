"""Native Windows evidence reads through pinned, non-reparse HANDLEs.

No path resolution follows links. Every directory from the local drive root is
held without write/delete sharing until the leaf has been read. UNC/device
namespaces are deliberately unsupported. Native qualification is required.
"""
import contextlib
import ctypes
from ctypes import wintypes
import os
from pathlib import PureWindowsPath
import re
import hashlib
import stat
import struct
import time

MAX_FILE_BYTES = 64 * 1024 * 1024
_INVALID = ctypes.c_void_p(-1).value
_DIRECTORY = 0x10
_REPARSE = 0x400


class _Info(ctypes.Structure):
    _fields_ = [('attributes', wintypes.DWORD), ('creation', wintypes.FILETIME),
                ('access', wintypes.FILETIME), ('write', wintypes.FILETIME),
                ('volume', wintypes.DWORD), ('size_high', wintypes.DWORD),
                ('size_low', wintypes.DWORD), ('links', wintypes.DWORD),
                ('index_high', wintypes.DWORD), ('index_low', wintypes.DWORD)]


def _api():
    if os.name != 'nt':
        raise OSError('Windows HANDLE evidence reader requires native Windows')
    api = ctypes.WinDLL('kernel32', use_last_error=True)
    api.CreateFileW.argtypes = [wintypes.LPCWSTR, wintypes.DWORD, wintypes.DWORD,
                               ctypes.c_void_p, wintypes.DWORD, wintypes.DWORD, wintypes.HANDLE]
    api.CreateFileW.restype = wintypes.HANDLE
    api.GetFileInformationByHandle.argtypes = [wintypes.HANDLE, ctypes.POINTER(_Info)]
    api.GetFileInformationByHandle.restype = wintypes.BOOL
    api.GetFileType.argtypes = [wintypes.HANDLE]
    api.GetFileType.restype = wintypes.DWORD
    api.GetDriveTypeW.argtypes = [wintypes.LPCWSTR]
    api.GetDriveTypeW.restype = wintypes.UINT
    api.ReadFile.argtypes = [wintypes.HANDLE, ctypes.c_void_p, wintypes.DWORD,
                            ctypes.POINTER(wintypes.DWORD), ctypes.c_void_p]
    api.ReadFile.restype = wintypes.BOOL
    api.CloseHandle.argtypes = [wintypes.HANDLE]
    api.CloseHandle.restype = wintypes.BOOL
    return api


def _component(name):
    if (not name or name in {'.', '..'} or name[-1:] in {' ', '.'}
            or any(ord(c) < 32 or c in '<>:"/\\|?*' for c in name)
            or re.fullmatch(r'(?i)(CON|PRN|AUX|NUL|COM[1-9¹²³]|LPT[1-9¹²³])(?:\..*)?', name)):
        raise ValueError('unsafe Windows evidence path component')
    return name


def _open(api, path, directory, allow_reparse=False):
    # GENERIC_READ; FILE_SHARE_READ only; OPEN_EXISTING; inspect link itself.
    handle = api.CreateFileW(str(path), 0x80000000, 1, None, 3, 0x02200000, None)
    if handle == _INVALID:
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        info = _Info()
        if not api.GetFileInformationByHandle(handle, ctypes.byref(info)):
            raise ctypes.WinError(ctypes.get_last_error())
        if api.GetFileType(handle) != 1 or (info.attributes & _REPARSE and not allow_reparse):
            raise ValueError('reparse or nondisk evidence entry')
        if directory is not None and bool(info.attributes & _DIRECTORY) != directory:
            raise ValueError('unexpected Windows evidence entry type')
        return handle, info
    except BaseException:
        api.CloseHandle(handle)
        raise


@contextlib.contextmanager
def parent_handle(root, relative):
    """Pin all ancestors; yield an opaque parent usable by regular_bytes."""
    root = PureWindowsPath(str(root))
    raw = str(relative).replace('/', '\\')
    parts = raw.split('\\')
    if not root.is_absolute() or not re.fullmatch(r'[A-Za-z]:', root.drive):
        raise ValueError('absolute local-drive evidence root required')
    for part in [*root.parts[1:], *parts]:
        _component(part)
    api, held = _api(), []
    if api.GetDriveTypeW(root.anchor) not in {3, 6}:  # Fixed local disk or RAM disk.
        raise ValueError('remote or removable evidence drives are unsupported')
    try:
        current = PureWindowsPath(root.anchor)
        handle, _ = _open(api, current, True)
        held.append(handle)
        for part in [*root.parts[1:], *parts[:-1]]:
            current /= part
            handle, _ = _open(api, current, True)
            held.append(handle)
        yield (api, current)
    finally:
        for handle in reversed(held):
            api.CloseHandle(handle)


def regular_bytes(parent, name, deadline, max_bytes=MAX_FILE_BYTES):
    """Yield bounded bytes from one actual HANDLE, never reopen by path."""
    api, directory = parent
    _component(name)
    if time.monotonic() >= deadline:
        raise ValueError('Windows evidence deadline exhausted')
    handle, info = _open(api, directory / name, False)
    try:
        yield from _handle_bytes(api, handle, info, deadline, max_bytes)
    finally:
        api.CloseHandle(handle)


def _handle_bytes(api, handle, info, deadline, max_bytes):
    if (info.size_high << 32) | info.size_low > max_bytes:
        raise ValueError('Windows evidence file exceeds quota')
    buffer = ctypes.create_string_buffer(min(1024 * 1024, max_bytes + 1))
    consumed = 0
    while True:
        if time.monotonic() >= deadline:
            raise ValueError('Windows evidence deadline exhausted')
        count = wintypes.DWORD()
        if not api.ReadFile(handle, buffer, len(buffer), ctypes.byref(count), None):
            raise ctypes.WinError(ctypes.get_last_error())
        consumed += count.value
        if consumed > max_bytes:
            raise ValueError('Windows evidence file grew beyond quota')
        if not count.value:
            return
        yield buffer.raw[:count.value]


def entry_state(parent, name, deadline):
    """Hash a regular file or symlink; symlink data comes from its own HANDLE."""
    api, directory = parent
    _component(name)
    handle, info = _open(api, directory / name, None, allow_reparse=True)
    try:
        # Both ancestors and leaf are pinned against replacement during lstat.
        mode = stat.S_IMODE(os.lstat(directory / name).st_mode)
        if info.attributes & _REPARSE:
            api.DeviceIoControl.argtypes = [wintypes.HANDLE, wintypes.DWORD, ctypes.c_void_p,
                wintypes.DWORD, ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(wintypes.DWORD), ctypes.c_void_p]
            api.DeviceIoControl.restype = wintypes.BOOL
            buffer, count = ctypes.create_string_buffer(16384), wintypes.DWORD()
            if not api.DeviceIoControl(handle, 0x000900A8, None, 0, buffer, len(buffer), ctypes.byref(count), None):
                raise ctypes.WinError(ctypes.get_last_error())
            raw = buffer.raw[:count.value]
            if len(raw) < 20 or struct.unpack_from('<I', raw)[0] != 0xA000000C:
                raise ValueError('only symlink reparse leaves are supported')
            offset, length = struct.unpack_from('<HH', raw, 8)
            if offset % 2 or length % 2 or 20 + offset + length > len(raw):
                raise ValueError('invalid symlink reparse data')
            target = raw[20 + offset:20 + offset + length].decode('utf-16-le')
            # Match native Python readlink's substitute-name presentation.
            if target.startswith('\\??\\'):
                target = '\\\\?\\' + target[4:]
            data = os.fsencode(target)
            return {'kind': 'symlink', 'mode': mode, 'sha256': hashlib.sha256(data).hexdigest(), 'target': target}, len(data)
        if info.attributes & _DIRECTORY:
            raise ValueError('cannot review a directory/submodule')
        digest, size = hashlib.sha256(), 0
        for chunk in _handle_bytes(api, handle, info, deadline, MAX_FILE_BYTES):
            size += len(chunk)
            digest.update(chunk)
        return {'kind': 'file', 'mode': mode, 'sha256': digest.hexdigest()}, size
    finally:
        api.CloseHandle(handle)
