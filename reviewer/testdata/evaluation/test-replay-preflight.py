#!/usr/bin/env python3
"""Unsupported Git must stop before any object read or provider-free probe."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class ReplayPreflight(unittest.TestCase):
    def test_unsupported_no_lazy_fetch_has_clear_failure_without_fallback(self):
        with tempfile.TemporaryDirectory(prefix='sfl-old-git-probe-') as temp:
            directory = Path(temp)
            log = directory / 'calls.jsonl'
            git = directory / 'git'
            git.write_text('#!' + sys.executable + '\nimport json,sys\n'
                           + 'with open(' + repr(str(log)) + ',"a") as f: f.write(json.dumps(sys.argv[1:])+"\\n")\n'
                           + 'sys.stderr.write("unknown option: --no-lazy-fetch\\n")\nsys.exit(129)\n')
            git.chmod(0o700)
            env = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ['PATH'])
            run = subprocess.run([sys.executable, str(Path(__file__).with_name('replay-defects.py'))],
                                 capture_output=True, text=True, env=env, timeout=20)
            self.assertNotEqual(run.returncode, 0)
            self.assertIn('requires Git 2.46+', run.stderr)
            self.assertNotIn('Traceback', run.stderr)
            self.assertEqual(run.stdout, '')
            calls = [json.loads(line) for line in log.read_text().splitlines()]
            self.assertEqual(calls, [['--no-replace-objects', '--no-lazy-fetch', '--version']])


if __name__ == '__main__':
    unittest.main()
