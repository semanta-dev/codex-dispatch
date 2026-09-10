import importlib.util
import os
from pathlib import Path
import subprocess
import socket
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('policy', ROOT / 'scripts/execution_policy.py')
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


class ConfinementTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / 'repo'
        self.repo.mkdir()
        subprocess.run(['git', 'init', '-q', str(self.repo)], check=True)
        (self.repo / 'input').write_text('expected')
        (self.repo / '.gitignore').write_text('.env\n')
        (self.repo / '.env').write_text('FAKE_IGNORED_SECRET')
        self.home = self.root / 'home'
        self.home.mkdir()
        (self.home / 'secret').write_text('FAKE_HOME_SECRET')

    def run_check(self, command, timeout=3):
        return policy.verify(command, self.repo, self.repo, self.root / 'stdout', self.root / 'stderr', timeout)

    def require_linux(self):
        if sys.platform != 'linux':
            self.skipTest('Linux confinement probe; required native backend coverage remains a promotion gate')
        result = self.run_check('test "$(cat input)" = expected; printf positive-control')
        self.assertEqual(result['exit_code'], 0, result)
        self.assertEqual(result['stdout'], 'positive-control')

    def test_unavailable_backend_never_executes(self):
        with patch.object(policy.sys, 'platform', 'unsupported'):
            result = self.run_check('touch bypass')
        self.assertEqual(result['failure_kind'], 'sandbox-unavailable')
        self.assertNotEqual(result['exit_code'], 0)
        self.assertFalse((self.repo / 'bypass').exists())

    def test_positive_control_and_secrets_filesystem_network_boundary(self):
        self.require_linux()
        with patch.dict(os.environ, {'ANTHROPIC_API_KEY': 'FAKE_REVIEW_KEY', 'HOME': str(self.home)}):
            command = '''python3 - <<'PROBE'
import os,pathlib,socket
assert 'ANTHROPIC_API_KEY' not in os.environ
assert not pathlib.Path('.env').exists()
assert not pathlib.Path(os.environ['HOME'],'secret').exists()
try:
 pathlib.Path('/usr/outside').write_text('escape')
except OSError: pass
else: raise AssertionError('outside write succeeded')
s=socket.socket();s.settimeout(.2)
try: s.connect(('1.1.1.1',443))
except OSError: pass
else: raise AssertionError('network access succeeded')
print('boundaries enforced')
PROBE'''
            result = self.run_check(command)
        self.assertEqual(result['exit_code'], 0, result)
        self.assertNotIn('FAKE_', result['stdout'] + result['stderr'])
        self.assertFalse((self.root / 'outside').exists())

    def test_writes_are_disposable_and_reported(self):
        self.require_linux()
        result = self.run_check('printf changed > input')
        self.assertEqual(result['exit_code'], 0, result)
        self.assertEqual(result['mutations'], ['input'])
        self.assertEqual((self.repo / 'input').read_text(), 'expected')

    def test_reachable_host_listener_is_unreachable_inside_verification(self):
        self.require_linux()
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            listener.listen(2)
            port = listener.getsockname()[1]
            # Prove the destination works on this host before testing denial.
            with socket.create_connection(('127.0.0.1', port), timeout=1):
                accepted, _ = listener.accept()
                accepted.close()
            result = self.run_check("python3 - <<'PROBE'\nimport socket\ns=socket.socket();s.settimeout(.3)\ntry: s.connect(('127.0.0.1', " + str(port) + "))\nexcept OSError: print('host listener denied')\nelse: raise AssertionError('host network reachable')\nPROBE")
            self.assertEqual(result['exit_code'], 0, result)
            self.assertEqual(result['stdout'].strip(), 'host listener denied')
            listener.settimeout(.1)
            with self.assertRaises(socket.timeout):
                listener.accept()

    def test_hang_and_noisy_output_are_bounded(self):
        self.require_linux()
        for command, reason in [('sleep 10', 'timeout'), ('yes noisy', 'output-limit')]:
            started = time.monotonic()
            result = self.run_check(command, timeout=1)
            self.assertEqual(result['failure_kind'], reason, result)
            self.assertLess(time.monotonic() - started, 3)
            self.assertLessEqual(len(result['stdout']), policy.TAIL)
            self.assertLessEqual((self.root / 'stdout').stat().st_size, policy.MAX_OUTPUT)

    def test_special_and_oversized_output_fail_without_unbounded_read(self):
        self.require_linux()
        for command in ['mkfifo fifo', 'truncate -s 1000000000 huge']:
            started = time.monotonic()
            with self.assertRaisesRegex(ValueError, 'special|oversized'):
                self.run_check(command, timeout=1)
            self.assertLess(time.monotonic() - started, 2)

    def test_launch_failure_restores_signal_handlers(self):
        import signal
        before = {number: signal.getsignal(number) for number in [signal.SIGINT, signal.SIGTERM]}
        with patch.object(policy.subprocess, 'Popen', side_effect=FileNotFoundError('missing')), self.assertRaises(FileNotFoundError):
            policy.supervised(['/missing-executable'], self.repo, {}, self.root / 'out', self.root / 'err', 1)
        self.assertEqual(before, {number: signal.getsignal(number) for number in before})

    def test_precancelled_command_never_launches(self):
        import threading
        cancelled = threading.Event()
        cancelled.set()
        with patch.object(policy.subprocess, 'Popen') as spawn:
            result = policy.supervised(['must-not-launch'], self.repo, {}, self.root / 'out', self.root / 'err', 1, cancel=cancelled)
        spawn.assert_not_called()
        self.assertEqual(result['exit_code'], 130)

    def test_completed_command_cannot_pass_after_deadline(self):
        real = subprocess.Popen
        def delayed(*args, **kwargs):
            proc = real(*args, **kwargs)
            time.sleep(.2)
            return proc
        with patch.object(policy.subprocess, 'Popen', side_effect=delayed):
            result = policy.supervised([sys.executable, '-c', 'pass'], self.repo, {}, self.root / 'out', self.root / 'err', .05)
        self.assertEqual(result['exit_code'], 124)

    def test_double_fork_cannot_write_after_owned_command_exits(self):
        if sys.platform != 'linux':
            self.skipTest('Linux subreaper lifecycle; other native owners remain required')
        sentinel = self.root / 'escaped'
        code = "import os,time,pathlib; p=os.fork(); os._exit(0) if p else None; os.setsid(); p=os.fork(); os._exit(0) if p else None; time.sleep(.4); pathlib.Path('" + str(sentinel) + "').write_text('escaped')"
        result = policy.supervised([sys.executable, '-c', code], self.repo, {}, self.root / 'out', self.root / 'err', 2)
        self.assertEqual(result['exit_code'], 0, result)
        time.sleep(.5)
        self.assertFalse(sentinel.exists())

    def test_daemon_cannot_outlive_namespace(self):
        self.require_linux()
        result = self.run_check("python3 -c 'import os,time; p=os.fork(); os._exit(0) if p else None; os.setsid(); time.sleep(10)'", timeout=.5)
        self.assertLess(result['wall_s'], 2)


if __name__ == '__main__':
    unittest.main()
