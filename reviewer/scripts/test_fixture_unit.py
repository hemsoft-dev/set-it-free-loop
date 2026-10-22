"""Descriptor routing and birth gates. No systemd unit or host policy change."""
import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

fixture = runpy.run_path(str(Path(__file__).with_name("fixture-unit.py")))
FixtureUnit = fixture["FixtureUnit"]


class OriginalUnitTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def owner(self, populated):
        old = self.root/"original"
        old.mkdir()
        (old/"cgroup.events").write_text("populated "+str(int(populated))+"\n")
        (old/"cgroup.kill").write_text("0")
        owner = FixtureUnit("sfl-routing-test.service", "owned test", self.root, {})
        owner.group = os.open(old, os.O_RDONLY | os.O_DIRECTORY)
        owner.hierarchy = fixture["cgroup_root"]()
        owner.group_identity = os.fstat(owner.group)
        owner.hierarchy_identity = os.fstat(owner.hierarchy)
        owner.namespace_identity = fixture["namespaces"]()
        owner.birth = {"boot": fixture["boot"]()}
        process = subprocess.Popen(["/usr/bin/sleep", ".01"])
        owner.pidfd = os.pidfd_open(process.pid)
        process.wait(timeout=2)
        owner.captured = True
        self.addCleanup(owner.close)
        replacement = self.root/"replacement"
        replacement.mkdir()
        (replacement/"cgroup.events").write_text("populated "+str(int(not populated))+"\n")
        (replacement/"cgroup.kill").write_text("0")
        return owner, old, replacement

    def test_original_populated_replacement_empty_does_not_prove_stop(self):
        owner, old, replacement = self.owner(True)
        with self.assertRaisesRegex(RuntimeError, "physical stop remains unknown"):
            owner.stop(timeout=.03)
        self.assertEqual((old/"cgroup.kill").read_text(), "1")
        self.assertEqual((replacement/"cgroup.kill").read_text(), "0")
        self.assertFalse((self.root/"unit-stopped.json").exists())

    def test_original_empty_replacement_populated_is_independent(self):
        owner, old, replacement = self.owner(False)
        with patch.object(subprocess, "run", side_effect=AssertionError("unexpected named manager operation")):
            owner.stop()
        self.assertTrue(owner.stopped)
        self.assertEqual((old/"cgroup.kill").read_text(), "0")
        self.assertEqual((replacement/"cgroup.kill").read_text(), "0")
        self.assertTrue(json.loads((self.root/"unit-stopped.json").read_text())["original_root_dead"])

    def test_symlinked_control_does_not_mutate_target(self):
        owner, old, replacement = self.owner(True)
        (old/"cgroup.kill").unlink()
        (old/"cgroup.kill").symlink_to(replacement/"cgroup.kill")
        with self.assertRaises(OSError):
            owner.stop(timeout=.03)
        self.assertEqual((replacement/"cgroup.kill").read_text(), "0")
        self.assertFalse(owner.stopped)

    def test_no_capture_never_performs_named_cleanup(self):
        owner = FixtureUnit("sfl-routing-test.service", "owned test", self.root, {})
        with patch.object(subprocess, "run", side_effect=AssertionError("unexpected manager mutation")):
            with self.assertRaisesRegex(RuntimeError, "never captured"):
                owner.stop()

    def test_closed_capture_never_reuses_stale_handles(self):
        owner, _, _ = self.owner(False)
        owner.close()
        with self.assertRaisesRegex(RuntimeError, "never captured"):
            owner.stop()

    def test_dead_root_alone_does_not_override_populated_original(self):
        owner, _, _ = self.owner(True)
        self.assertTrue(owner.root_dead())
        with self.assertRaises(RuntimeError):
            owner.stop(timeout=.03)

    def test_boot_change_refuses_all_control_operations(self):
        owner, _, _ = self.owner(True)
        owner.birth["boot"] = "different-boot"
        with patch.object(owner, "control", side_effect=AssertionError("untrusted hierarchy used")):
            with self.assertRaisesRegex(RuntimeError, "hierarchy changed"):
                owner.stop()

    def test_empty_original_with_live_root_is_unknown(self):
        owner, _, _ = self.owner(False)
        with patch.object(owner, "root_dead", return_value=False):
            with self.assertRaises(RuntimeError):
                owner.stop(timeout=.03)

    def test_missing_events_without_deleted_original_is_unknown(self):
        owner, old, _ = self.owner(False)
        (old/"cgroup.events").unlink()
        self.assertFalse(owner.empty())

    def test_removed_original_is_proved_from_retained_descriptor(self):
        owner, old, _ = self.owner(False)
        (old/"cgroup.events").unlink()
        (old/"cgroup.kill").unlink()
        old.rmdir()
        owner.stop()
        self.assertTrue(owner.stopped)

    def test_birth_validation_precedes_manager_queries(self):
        owner = FixtureUnit("sfl-routing-test.service", "owned test", self.root, {})
        birth = {"pid": os.getpid(), "start": 1, "invocation": "a"*32,
                 "boot": fixture["boot"](), "group": "/user.slice/sfl-routing-test.service"}
        for values in [{"pid": True}, {"invocation": ""}, {"group": "/user.slice/../sfl-routing-test.service"}, {"extra": 1}]:
            with self.subTest(values=values), patch.object(owner, "observe", side_effect=AssertionError("premature manager operation")):
                with self.assertRaises(RuntimeError):
                    owner.capture({**birth, **values})

    def test_records_publish_atomically_without_replacing_existing_identity(self):
        write = fixture["write_new"]
        record = self.root/"record.json"
        write(record, {"identity": "original"})
        with self.assertRaises(FileExistsError):
            write(record, {"identity": "replacement"})
        self.assertEqual(json.loads(record.read_text()), {"identity": "original"})
        self.assertEqual(list(self.root.glob(".unit-record-*")), [])


if __name__ == "__main__":
    unittest.main()
