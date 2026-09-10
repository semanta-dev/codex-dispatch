"""Exercise rollback at the actual filesystem mutation boundary."""
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import json
import os

spec = importlib.util.spec_from_file_location('fanin', Path(__file__).resolve().parents[2] / 'scripts/graphrag-fanin.py')
fanin = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fanin)


class FaninTests(unittest.TestCase):
    def test_second_install_failure_rolls_back_first_and_retains_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            parent, work = root / 'parent', root / 'work'
            parent.mkdir(); work.mkdir()
            records = {}
            for name in ('a', 'b'):
                (parent / name).write_text('operator original')
                (work / name).write_text('candidate')
                records[name] = {'state': fanin.state(parent / name), 'wip': False}
            manifest, changed = root / 'manifest', root / 'changed'
            manifest.write_text(json.dumps(records))
            changed.write_bytes(b' M\0a\0 M\0b\0')
            real_replace = os.replace
            def fail_second(src, dst):
                if Path(src).name == '1.install':
                    raise OSError('injected second install failure')
                return real_replace(src, dst)
            recovery = root / 'recovery'
            with patch.object(fanin.os, 'replace', side_effect=fail_second):
                with self.assertRaisesRegex(OSError, 'injected'):
                    fanin.apply(parent, work, manifest, changed, recovery)
            for name in ('a', 'b'):
                self.assertEqual((parent / name).read_text(), 'operator original')
            self.assertEqual((recovery / 'status').read_text(), 'rolled back\n')
            self.assertEqual((recovery / '0.after').read_text(), 'candidate')

    def test_symlink_mode_and_deletion(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); parent, work = root/'parent', root/'work'
            parent.mkdir(); work.mkdir()
            (parent/'link').symlink_to('old-target')
            (work/'link').symlink_to('new-target')
            (parent/'deleted').write_text('delete me')
            (work/'executable').write_text('script'); (work/'executable').chmod(0o755)
            names = ('link', 'deleted', 'executable')
            manifest, changed = root/'manifest', root/'changed'
            manifest.write_text(json.dumps({n: {'state': fanin.state(parent/n), 'wip': False} for n in names}))
            changed.write_bytes(b''.join(b' M\0'+n.encode()+b'\0' for n in names))
            fanin.apply(parent, work, manifest, changed, root/'recovery')
            self.assertEqual(os.readlink(parent/'link'), 'new-target')
            self.assertFalse((parent/'deleted').exists())
            # Windows chmod does not synthesize POSIX execute bits. Fan-in
            # must preserve the mode the source filesystem actually reports.
            self.assertEqual(fanin.state(parent/'executable'), fanin.state(work/'executable'))
            if os.name != 'nt':
                self.assertEqual((parent/'executable').stat().st_mode & 0o777, 0o755)


if __name__ == '__main__':
    unittest.main()
