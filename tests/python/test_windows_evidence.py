"""Native integration and actual direct-worker timeout tests."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('windows_evidence', ROOT / 'scripts/windows_evidence.py')
EVIDENCE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(EVIDENCE)


class WorkerLifetimeTests(unittest.TestCase):
    def test_stalled_direct_worker_is_killed_and_reaped(self):
        real = subprocess.Popen
        processes = []
        def launch(*args, **kwargs):
            proc = real(*args, **kwargs)
            processes.append(proc)
            return proc
        with patch.object(EVIDENCE, '_command', return_value=[sys.executable, '-I', '-c', 'import time; time.sleep(30)']), patch.object(EVIDENCE.subprocess, 'Popen', side_effect=launch):
            start = time.monotonic()
            with self.assertRaises(subprocess.TimeoutExpired):
                # Exceeds Windows anonymous-pipe capacity by orders of magnitude.
                # The child never reads it; timeout must still kill/reap promptly.
                EVIDENCE._run_worker({'large_request': 'x' * (1024 * 1024)}, .25)
            self.assertLess(time.monotonic() - start, 5)
        self.assertEqual(len(processes), 1)
        self.assertIsNotNone(processes[0].poll())

    def test_actual_worker_child_environment_excludes_api_credentials(self):
        code = 'import json,os; print(json.dumps({"files":{},"secret":os.environ.get("ANTHROPIC_API_KEY")}))'
        with patch.dict(os.environ, {'ANTHROPIC_API_KEY': 'must-not-leak'}), patch.object(EVIDENCE, '_command', return_value=[sys.executable, '-I', '-c', code]):
            self.assertIsNone(EVIDENCE._run_worker({}, 5)['secret'])


@unittest.skipUnless(os.name == 'nt', 'requires actual Windows HANDLE evidence worker')
class NativeWindowsEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        subprocess.run(['git', 'init', '-q', str(self.repo)], check=True)
        self.run = self.repo / '.codex-dispatch/runs/test'
        self.run.mkdir(parents=True)
        (self.repo / 'tracked.txt').write_bytes(b'first')
        subprocess.run(['git', '-C', str(self.repo), 'add', 'tracked.txt'], check=True)

    def test_actual_capture_and_public_state_preserve_schema_detect_drift(self):
        (self.repo / 'untracked').write_bytes(b'untracked')
        (self.run / 'ignored-runtime').write_bytes(b'runtime')
        first = EVIDENCE.capture(self.repo, self.run)
        self.assertEqual(set(first['files']), {'tracked.txt', 'untracked'})
        self.assertEqual(first['files']['tracked.txt'], {'kind':'file', 'mode':stat.S_IMODE((self.repo/'tracked.txt').stat().st_mode), 'sha256':hashlib.sha256(b'first').hexdigest()})
        spec = importlib.util.spec_from_file_location('collector_native', ROOT / 'scripts/review-evidence.py')
        collector = importlib.util.module_from_spec(spec); spec.loader.exec_module(collector)
        self.assertEqual(collector.state(self.repo, self.run), first['files'])
        self.assertEqual(collector.live_fingerprint(self.repo, self.run), first['fingerprint'])
        (self.repo / 'tracked.txt').write_bytes(b'changed')
        second = EVIDENCE.capture(self.repo, self.run)
        self.assertNotEqual(first['fingerprint'], second['fingerprint'])
        subprocess.run(['git', '-C', str(self.repo), 'add', 'tracked.txt'], check=True)
        third = EVIDENCE.capture(self.repo, self.run)
        self.assertNotEqual(second['fingerprint'], third['fingerprint'])
        (self.repo / 'tracked.txt').unlink()
        self.assertNotIn('tracked.txt', EVIDENCE.capture(self.repo,self.run)['files'])

    def test_symlink_target_matches_native_readlink_without_reading_target(self):
        outside = self.repo.parent / (self.repo.name + '-absent')
        link = self.repo / 'link'
        link.symlink_to(outside)
        value = EVIDENCE.capture(self.repo,self.run)['files']['link']
        self.assertEqual(value['kind'], 'symlink')
        self.assertEqual(value['target'], os.readlink(link))
        self.assertEqual(value['sha256'], hashlib.sha256(os.fsencode(os.readlink(link))).hexdigest())

    def test_junction_substitution_of_tracked_parent_rejected(self):
        directory = self.repo / 'nested'; directory.mkdir()
        (directory/'file').write_bytes(b'owned')
        subprocess.run(['git','-C',str(self.repo),'add','nested/file'],check=True)
        directory.rename(self.repo/'original')
        subprocess.run(['cmd','/c','mklink','/J',str(directory),str(self.repo/'original')],check=True,capture_output=True)
        self.addCleanup(lambda: os.rmdir(directory))
        with self.assertRaises(ValueError):
            EVIDENCE.capture(self.repo,self.run)

    def test_static_no_tests_collector_completes_through_native_state(self):
        spec = importlib.util.spec_from_file_location('collector_static', ROOT / 'scripts/review-evidence.py')
        collector = importlib.util.module_from_spec(spec); spec.loader.exec_module(collector)
        scripts = self.run / 'fake-scripts'; scripts.mkdir()
        (scripts/'dispatch-codex.sh').write_text('printf "%s\\n" "$FAKE_RUN"\n')
        result = {'exit_code':0,'session_id':'native-static','files_changed':['tracked.txt'],'lines_added':1,'lines_removed':0}
        (self.run/'result.json').write_text(json.dumps(result))
        (self.run/'diff.patch').write_text('diff --git a/tracked.txt b/tracked.txt\n+first\n')
        (self.run/'effective-workdir.txt').write_text(str(self.repo))
        before = Path.cwd()
        try:
            os.chdir(self.repo)
            with patch.object(collector,'ROOT',scripts), patch.dict(os.environ,{'FAKE_RUN':str(self.run),'REVIEW_TEST_POLICY':'skip','REVIEW_TEST_CMD':'','REVIEW_VERIFY_CMD':''}):
                bundle = collector.collect()
            self.assertTrue(bundle['complete'])
            self.assertEqual(bundle['live_fingerprint'],collector.live_fingerprint(self.repo,self.run))
            self.assertEqual(bundle['verification_mutations'],[])
        finally:
            os.chdir(before)
