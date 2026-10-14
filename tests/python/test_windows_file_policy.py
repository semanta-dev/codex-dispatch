"""Actual Win32 HANDLE tests; host skips are not native qualification evidence."""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest

SPEC = importlib.util.spec_from_file_location('windows_files', Path(__file__).resolve().parents[2] / 'scripts/windows_file_policy.py')
FILES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FILES)


@unittest.skipUnless(os.name == 'nt', 'requires native Windows HANDLE implementation')
class WindowsFileTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.directory = self.root / 'nested'
        self.directory.mkdir()
        self.file = self.directory / 'file'
        self.file.write_bytes(b'actual bytes')

    def read(self, root=None, name='nested/file', limit=1024):
        with FILES.parent_handle(root or self.root, name) as parent:
            return b''.join(FILES.regular_bytes(parent, Path(name).name, time.monotonic() + 5, limit))

    def test_real_handle_reads_bytes_and_releases_locks(self):
        self.assertEqual(self.read(), b'actual bytes')
        self.file.rename(self.directory / 'renamed')

    def test_parent_rename_and_reparse_replacement_are_blocked_while_held(self):
        with FILES.parent_handle(self.root, 'nested/file') as parent:
            with self.assertRaises(OSError):
                self.directory.rename(self.root / 'moved')
            with self.assertRaises(OSError):
                self.root.rename(self.root.with_name(self.root.name + '-moved'))
            self.assertEqual(b''.join(FILES.regular_bytes(parent, 'file', time.monotonic() + 5)), b'actual bytes')

    def test_leaf_write_and_rename_blocked_during_handle_read(self):
        self.file.write_bytes(b'x' * (2 * 1024 * 1024))
        with FILES.parent_handle(self.root, 'nested/file') as parent:
            chunks = FILES.regular_bytes(parent, 'file', time.monotonic() + 5)
            try:
                self.assertTrue(next(chunks))
                with self.assertRaises(OSError):
                    self.file.write_bytes(b'corruption')
                with self.assertRaises(OSError):
                    self.file.rename(self.directory / 'moved')
            finally:
                chunks.close()

    def test_junction_parent_and_root_are_rejected_without_following(self):
        alias = self.root / 'alias'
        subprocess.run(['cmd', '/c', 'mklink', '/J', str(alias), str(self.directory)], check=True, capture_output=True)
        self.addCleanup(lambda: os.rmdir(alias))
        for root, name in [(self.root, 'alias/file'), (alias, 'file')]:
            with self.subTest(root=root, name=name), self.assertRaises((OSError, ValueError)):
                self.read(root, name)

    def test_leaf_symlink_rejected(self):
        link = self.directory / 'link'
        # Requires Developer Mode/admin, configured by native qualification.
        link.symlink_to(self.file)
        with self.assertRaises((OSError, ValueError)):
            self.read(name='nested/link')

    def test_aliases_and_streams_rejected(self):
        for name in ['../outside', 'nested/file:stream', 'nested/CON', 'nested/file.', 'nested/file ']:
            with self.subTest(name=name), self.assertRaises(ValueError):
                self.read(name=name)

    def test_quota_and_deadline_fail_without_leaking_handles(self):
        with self.assertRaises(ValueError):
            self.read(limit=1)
        with FILES.parent_handle(self.root, 'nested/file') as parent:
            with self.assertRaises(ValueError):
                list(FILES.regular_bytes(parent, 'file', time.monotonic() - 1))
        self.file.unlink()
