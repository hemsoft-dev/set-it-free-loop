import base64
import importlib.util
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

DIRECTORY = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("git_vm_prerequisite", DIRECTORY/"run.py")
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
NONCE = "883c661671d20c825627c9d854192bae"


class GuestRecordTests(unittest.TestCase):
    def actual(self):
        return probe.decode_guest_result(base64.b64decode((DIRECTORY/"testdata/actual-guest-result.b64").read_bytes().strip(), validate=True), NONCE)

    def frame(self, value):
        return ("SFL_GIT_VM_PREREQ_"+NONCE+" "+json.dumps(value)+"\n").encode()

    def test_actual_pinned_producer_record(self):
        self.assertEqual(hashlib.sha256((DIRECTORY/"probe.c").read_bytes()).hexdigest(),
                         "32535a29ad3bcc9227ca53cc1633e8c7eac9404422fb0ea9227bb95353c33902")
        self.assertEqual(hashlib.sha256(base64.b64decode((DIRECTORY/"testdata/actual-guest-result.b64").read_bytes().strip(), validate=True)).hexdigest(),
                         "42bd090440c0adbb8db6e74d3bb347b385dca8dde19103a3214983dd8c215085")
        value = self.actual()
        self.assertTrue(probe.prerequisites_passed(value))
        self.assertEqual(value["kernel"], "7.0.0-38-generic")
        self.assertFalse(value["production_qualified"])

    def test_schema_framing_and_provenance_refuse(self):
        actual = base64.b64decode((DIRECTORY/"testdata/actual-guest-result.b64").read_bytes().strip(), validate=True)
        malformed = [actual+actual, b"x"*(4*1024*1024+1), actual.replace(NONCE.encode(), b"0"*32),
                     actual.replace(b'"version":1', b'"version":1,"version":1'), actual.rstrip()+b" {}\n"]
        for change in [{"version": 2}, {"size_bytes": True}, {"sealed_proc_exec": 1},
                       {"production_qualified": True}, {"unexpected": True}]:
            malformed.append(self.frame(dict(self.actual(), **change)))
        missing = self.actual()
        del missing["noswap_reported"]
        malformed.append(self.frame(missing))
        for raw in malformed:
            with self.subTest(raw=raw[:80]):
                with self.assertRaises((RuntimeError, ValueError)):
                    probe.decode_guest_result(raw, NONCE)

    def test_observed_prerequisite_failures_never_pass(self):
        for change in [{"mount_succeeded": False}, {"mount_errno": 1}, {"noswap_reported": False},
                       {"size_bytes": 268431360}, {"inode_limit": 65535}, {"filesystem_type": 0},
                       {"mount_flags": 0}, {"sealed_proc_exec": False}, {"sealed_execveat": False}]:
            with self.subTest(change=change):
                value = probe.decode_guest_result(self.frame(dict(self.actual(), **change)), NONCE)
                self.assertFalse(probe.prerequisites_passed(value))

    def test_compiled_guest_refuses_ordinary_host_execution(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-guard-") as temporary:
            binary = Path(temporary)/"probe"
            environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}
            subprocess.run(["/usr/bin/cc", "-static", "-O2", "-Wall", "-Wextra", "-Werror",
                            "-o", str(binary), str(DIRECTORY/"probe.c")],
                           env=environment, check=True, timeout=30)
            for arguments in [[], ["--guest-init"]]:
                result = subprocess.run([str(binary), *arguments], env=environment,
                                        capture_output=True, timeout=3)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, b"")
                self.assertEqual(result.stderr, b"")


if __name__ == "__main__":
    unittest.main()
