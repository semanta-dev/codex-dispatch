import importlib.util
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('plan_runner_cancel', Path(__file__).resolve().parents[2] / 'scripts/graphrag-plan-runner.py')
RUNNER = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = RUNNER
SPEC.loader.exec_module(RUNNER)


class ProcessCancellationTests(unittest.TestCase):
    def setUp(self):
        RUNNER.CANCEL_REQUESTED.clear()
        self.addCleanup(RUNNER.CANCEL_REQUESTED.clear)
        self.addCleanup(RUNNER.cancel_processes)

    def test_cancellation_drains_descendants_and_releases_output_handles(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            ready, output = root / 'ready', root / 'output'
            code = "import subprocess,sys,pathlib; child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)']); pathlib.Path(sys.argv[1]).write_text(str(child.pid)); child.wait()"
            results = []
            def work():
                with output.open('w') as log:
                    results.append(RUNNER.run_process([sys.executable, '-c', code, str(ready)], stdout=log, stderr=log, text=True))
            worker = threading.Thread(target=work)
            worker.start()
            try:
                deadline = time.monotonic() + 10
                while not ready.exists() and worker.is_alive() and time.monotonic() < deadline:
                    time.sleep(.01)
                self.assertTrue(ready.exists(), 'descendant never became ready')
                RUNNER.cancel_processes()
                worker.join(10)
                self.assertFalse(worker.is_alive(), 'cancellation did not drain worker')
                self.assertNotEqual(results[0].returncode, 0)
                output.unlink()  # Windows denies this while a descendant holds it.
                self.assertFalse(RUNNER.ACTIVE_PROCESSES)
            finally:
                RUNNER.cancel_processes()
                worker.join(10)

    def test_cancel_serializes_with_launch_and_blocks_later_launches(self):
        spawning, release = threading.Event(), threading.Event()
        real = subprocess.Popen
        def spawn(*args, **kwargs):
            spawning.set()
            self.assertTrue(release.wait(5))
            return real(*args, **kwargs)
        results = []
        with patch.object(RUNNER.subprocess, 'Popen', side_effect=spawn):
            worker = threading.Thread(target=lambda: results.append(RUNNER.run_process([sys.executable, '-c', 'import time; time.sleep(60)'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)))
            worker.start()
            self.assertTrue(spawning.wait(5))
            cancel = threading.Thread(target=RUNNER.cancel_processes)
            cancel.start()
            release.set()
            cancel.join(10)
            worker.join(10)
            self.assertFalse(cancel.is_alive())
            self.assertFalse(worker.is_alive())
            self.assertNotEqual(results[0].returncode, 0)
        with patch.object(RUNNER.subprocess, 'Popen') as spawn:
            self.assertEqual(RUNNER.run_process(['must-not-launch']).returncode, 130)
            spawn.assert_not_called()


if __name__ == '__main__':
    unittest.main()
