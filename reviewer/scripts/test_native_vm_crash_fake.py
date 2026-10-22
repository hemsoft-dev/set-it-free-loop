import importlib.util
import json
import os
import signal
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("native_crash_fake", Path(__file__).with_name("native-vm-crash-fake.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class OwnedDriverTimeoutTests(unittest.TestCase):
    def test_driver_exit_before_signal_cannot_qualify_forced_loss(self):
        with tempfile.TemporaryDirectory(prefix="sfl-exited-driver-test-") as directory:
            root = Path(directory)
            program = root/"fixture"
            config = root/"configuration"
            config.write_text(str(root))
            recovery = {"Status": "stopped-owned-unit", "SourceHead": "a"*40,
                        "SourceTree": "b"*40, "OriginalRootVerified": True,
                        "PhysicalStop": True, "RuntimeRemoved": True,
                        "AuthRecoveryFenced": True, "AuthUnchanged": True}
            program.write_text('''#!/usr/bin/python3
import json, pathlib, sys, time
root=pathlib.Path(pathlib.Path(sys.argv[2]).read_text())
if sys.argv[1]=='--recover-owned':
 print('''+repr(json.dumps(recovery))+''')
else:
 (root/'point').write_text('ready')
 while not (root/'exit-now').exists(): time.sleep(0.01)
 sys.exit(1)
''')
            program.chmod(0o500)
            original_kill = module.kill_owned_group

            def kill_after_real_exit(process):
                (root/"exit-now").write_text("exit")
                self.assertEqual(process.wait(timeout=3), 1)
                return original_kill(process)

            with mock.patch.object(module, "kill_owned_group", side_effect=kill_after_real_exit):
                with (root/"log").open("wb") as log:
                    code, timed_out, cleanup = module.run_owned(program, config,
                        {"PATH": "/usr/bin:/bin"}, log, timeout=5, force_point=root/"point")
            self.assertTrue(cleanup["forced_point_observed"])
            self.assertEqual(cleanup["status"], "known")
            self.assertFalse(cleanup["driver_alive_before_signal"])
            self.assertFalse(cleanup["kill_signal_sent"])
            self.assertFalse(cleanup["driver_sigkill_observed"])
            self.assertFalse(module.forced_loss_passed(
                {"source_commit": "a"*40, "source_tree": "b"*40}, code, timed_out, cleanup))
            claimed = dict(cleanup, driver_alive_before_signal=True,
                           kill_signal_sent=True, driver_sigkill_observed=True)
            self.assertFalse(module.forced_loss_passed(
                {"source_commit": "a"*40, "source_tree": "b"*40}, code, timed_out, claimed))

    def test_timeout_kills_owned_child_and_runs_separate_recovery(self):
        with tempfile.TemporaryDirectory(prefix="sfl-timeout-test-") as directory:
            root = Path(directory)
            program = root/"fixture"
            config = root/"configuration"
            config.write_text(str(root))
            program.write_text('''#!/usr/bin/python3
import json, pathlib, subprocess, sys, time
root=pathlib.Path(pathlib.Path(sys.argv[2]).read_text())
if sys.argv[1]=='--recover-owned':
 (root/'recovered').write_text('yes')
 print(json.dumps({'Status':'stopped-owned-unit','SourceHead':'a'*40,'SourceTree':'b'*40,
  'OriginalRootVerified':True,'PhysicalStop':True,'RuntimeRemoved':True,
  'AuthRecoveryFenced':True,'AuthUnchanged':True}))
else:
 child=subprocess.Popen(['/usr/bin/sleep','60'])
 (root/'child-pid').write_text(str(child.pid))
 (root/'point').write_text('ready')
 time.sleep(60)
''')
            program.chmod(0o500)
            with (root/"log").open("wb") as log:
                code, timed_out, cleanup = module.run_owned(program, config,
                    {"PATH":"/usr/bin:/bin"}, log, timeout=5, force_point=root/"point")
            self.assertTrue(timed_out)
            self.assertEqual(code, -signal.SIGKILL)
            self.assertTrue(cleanup["owned_process_group_stopped"])
            self.assertTrue(cleanup["forced_point_observed"])
            self.assertTrue(cleanup["driver_alive_before_signal"])
            self.assertTrue(cleanup["kill_signal_sent"])
            self.assertTrue(cleanup["driver_sigkill_observed"])
            self.assertEqual(cleanup["status"], "known")
            source = {"source_commit": "a"*40, "source_tree": "b"*40}
            self.assertTrue(module.forced_loss_passed(source, code, timed_out, cleanup))
            for key in ["driver_alive_before_signal", "kill_signal_sent", "driver_sigkill_observed"]:
                self.assertFalse(module.forced_loss_passed(
                    source, code, timed_out, dict(cleanup, **{key: False})))
            self.assertEqual((root/"recovered").read_text(), "yes")
            child = Path('/proc')/(root/"child-pid").read_text()/"stat"
            if child.exists():
                self.assertEqual(child.read_text().split(')')[-1].split()[0], 'Z')

    def test_deadline_preserves_unknown_recovery_as_failure(self):
        with tempfile.TemporaryDirectory(prefix="sfl-deadline-test-") as directory:
            root = Path(directory)
            program = root/"fixture"
            program.write_text("#!/bin/sh\nif [ \"$1\" = --recover-owned ]; then echo '{\"Status\":\"unknown\"}'; exit 1; fi\nsleep 60\n")
            program.chmod(0o500)
            with (root/"log").open("wb") as log:
                code, timed_out, cleanup = module.run_owned(program, root/"unused",
                    {"PATH":"/usr/bin:/bin"}, log, timeout=0.1)
            self.assertTrue(timed_out)
            self.assertNotEqual(code, 0)
            self.assertEqual(cleanup["trigger"], "deadline")
            self.assertEqual(cleanup["status"], "unknown")
            self.assertFalse(cleanup["forced_point_observed"])

    def test_success_does_not_invoke_recovery(self):
        with tempfile.TemporaryDirectory(prefix="sfl-normal-test-") as directory:
            root = Path(directory)
            program = root/"fixture"
            program.write_text('#!/bin/sh\nexit 0\n')
            program.chmod(0o500)
            with (root/"log").open("wb") as log:
                code, timed_out, cleanup = module.run_owned(program, root/"unused",
                    {"PATH":"/usr/bin:/bin"}, log, timeout=3)
            self.assertEqual(code, 0)
            self.assertFalse(timed_out)
            self.assertEqual(cleanup["status"], "not-needed")


if __name__ == "__main__":
    unittest.main()
