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


def _open(api, path, directory):
    # GENERIC_READ; FILE_SHARE_READ only; OPEN_EXISTING; inspect link itself.
    handle = api.CreateFileW(str(path), 0x80000000, 1, None, 3, 0x02200000, None)
    if handle == _INVALID:
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        info = _Info()
        if not api.GetFileInformationByHandle(handle, ctypes.byref(info)):
            raise ctypes.WinError(ctypes.get_last_error())
        if api.GetFileType(handle) != 1 or info.attributes & _REPARSE:
            raise ValueError('reparse or nondisk evidence entry')
        if bool(info.attributes & _DIRECTORY) != directory:
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
    finally:
        api.CloseHandle(handle)
