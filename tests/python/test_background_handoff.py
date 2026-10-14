"""Only a successful launch and private caller acknowledgment may detach."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('handoff_policy', ROOT / 'scripts/execution_policy.py')
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


@unittest.skipUnless(sys.platform == 'linux', 'Linux process owner; other native handoff backends remain required')
class BackgroundHandoffTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fake = self.root / 'dispatch'
        self.sentinel = self.root / 'late-write'
        self.record = self.root / 'transfer.json'
        self.env = {**os.environ, 'CODEX_DISPATCH_BIN': str(self.fake)}
        self.argv = ['bash', str(ROOT / 'scripts/dispatch-codex.sh'), '--detach']

    def program(self, exit_code=0, forge=False):
        self.fake.write_text('#!' + sys.executable + '\n' + '''import json,os,pathlib,time
p=os.fork()
if p==0:
 os.setsid()
 time.sleep(.5)
 pathlib.Path(''' + repr(str(self.sentinel)) + ''').write_text('survived')
 os._exit(0)
''' + ("pathlib.Path(" + repr(str(self.record)) + ").write_text(json.dumps({'state':'transferred','task_id':'t_forged'}))\n" if forge else '') + "print('t_abcd',flush=True)\nos._exit(" + str(exit_code) + ')\n')
        self.fake.chmod(0o755)

    def invoke(self, **kwargs):
        return policy.supervised(self.argv, self.root, self.env, self.root / 'stdout', self.root / 'stderr',
                                 2, background_handoff=True, handoff_record=self.record, **kwargs)

    def test_workspace_record_cannot_override_failed_child(self):
        self.program(exit_code=17, forge=True)
        result = self.invoke()
        self.assertEqual(result['exit_code'], 17, result)
        self.assertIsNone(result['background_handoff'])
        time.sleep(.6)
        self.assertFalse(self.sentinel.exists())

    def test_precommit_cancel_during_parent_pause_drains(self):
        self.program()
        cancelled = threading.Event()
        real = subprocess.Popen
        def paused(*args, **kwargs):
            child = real(*args, **kwargs)
            time.sleep(.15)
            cancelled.set()
            return child
        with patch.object(policy.subprocess, 'Popen', side_effect=paused):
            result = self.invoke(cancel=cancelled)
        self.assertEqual(result['exit_code'], 130, result)
        self.assertIsNone(result['background_handoff'])
        time.sleep(.6)
        self.assertFalse(self.sentinel.exists())

    def test_same_uid_child_cannot_reopen_either_control_pipe_holder(self):
        attack_log = self.root / 'attack.json'
        self.fake.write_text('#!' + sys.executable + '\n' + r"""import errno,json,os,pathlib,time
owner=os.getppid()
args=pathlib.Path(f'/proc/{owner}/cmdline').read_bytes().split(b'\0')
index=args.index(b'--handoff-background')
commit=int(args[index+2])
supervisor=int(args[index-1])
results=[]
for pid,fd in [(owner,commit),(supervisor,commit+1)]:
 try:
  stolen=os.open(f'/proc/{pid}/fd/{fd}',os.O_WRONLY)
  os.write(stolen,b't_abcd\n')
  os.close(stolen)
  results.append('forged')
 except OSError as error:
  results.append(error.errno)
pathlib.Path(""" + repr(str(attack_log)) + """).write_text(json.dumps(results))
p=os.fork()
if p==0:
 os.setsid();time.sleep(.5)
 pathlib.Path(""" + repr(str(self.sentinel)) + """).write_text('escaped')
 os._exit(0)
print('t_abcd',flush=True)
""")
        self.fake.chmod(0o755)
        cancelled = threading.Event()
        real = subprocess.Popen
        def paused(*args, **kwargs):
            process = real(*args, **kwargs)
            time.sleep(.2)
            cancelled.set()
            return process
        with patch.object(policy.subprocess, 'Popen', side_effect=paused):
            result = self.invoke(cancel=cancelled)
        import errno
        self.assertTrue(attack_log.exists(), {**result, 'stderr': (self.root / 'stderr').read_text()})
        self.assertEqual(json.loads(attack_log.read_text()), [errno.EACCES, errno.EACCES])
        self.assertEqual(result['exit_code'], 130, result)
        time.sleep(.6)
        self.assertFalse(self.sentinel.exists())

    def test_postcommit_cancel_reports_started_identity(self):
        self.program()
        cancelled = threading.Event()
        real = os.write
        def cancel_after_ack(fd, data):
            result = real(fd, data)
            if data == b't_abcd\n':
                cancelled.set()
            return result
        with patch.object(policy.os, 'write', side_effect=cancel_after_ack):
            result = self.invoke(cancel=cancelled)
        self.assertEqual(result['exit_code'], 0, result)
        self.assertEqual(result['background_handoff']['task_id'], 't_abcd')
        self.assertEqual(json.loads(self.record.read_text())['state'], 'transferred')
        time.sleep(.6)
        self.assertTrue(self.sentinel.exists())
