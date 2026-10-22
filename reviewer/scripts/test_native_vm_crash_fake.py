import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("native_crash_fake", Path(__file__).with_name("native-vm-crash-fake.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class OwnedDriverTimeoutTests(unittest.TestCase):
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
 print(json.dumps({'Status':'stopped-owned-unit'}))
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
            self.assertNotEqual(code, 0)
            self.assertTrue(cleanup["owned_process_group_stopped"])
            self.assertTrue(cleanup["forced_point_observed"])
            self.assertEqual(cleanup["status"], "known")
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
