"""Grant receipt boundary tests. No host unit, KVM device or provider is used."""
import copy
import json
from pathlib import Path
import runpy
import unittest
from unittest.mock import patch

fixture = runpy.run_path(str(Path(__file__).with_name("vm-grant-fixture.py")))
parse = fixture["parse_grant_result"]


def receipt():
    expected = {"SourceHead": "a"*40, "SourceTree": "b"*40, "StubHash": "c"*64,
                "Binding": {"session_hash": "d"*64, "launch_id": "e"*32, "containment_id": "f"*64},
                "BindingHash": "1"*64, "ManifestHash": "2"*64, "Unit": "sfl-alpha-"+"e"*32+".service"}
    execution = {"Version": 1, "BindingHash": expected["BindingHash"], "ManifestHash": expected["ManifestHash"],
                 "VerifiedExecutable": True, "QEMUExited": True, "FakeOnly": True, "ProductionBoundaryQualified": False, "Error": ""}
    journal = {"Binding": expected["Binding"], "Unit": expected["Unit"], "StubHash": expected["StubHash"],
               "ManifestHash": expected["ManifestHash"], "PayloadKind": "vm-fixture",
               **{key: True for key in ["Started", "StartSettled", "Captured", "Aborted", "Stopped"]}}
    value = {"Version": 1, "Root": "/tmp/sfl-vm-unit-synthetic", "Execution": execution, "Journal": journal, "Error": "",
             **{key: expected[key] for key in ["SourceHead", "SourceTree", "StubHash", "BindingHash", "ManifestHash", "Unit"]},
             **{key: True for key in ["Started", "BeforeGrantInert", "Granted", "Stopped", "Recovered", "Passed", "FakeOnly"]},
             "ProductionBoundaryQualified": False}
    return value, expected


class GrantReceiptTests(unittest.TestCase):
    def test_final_probe_uses_fixed_local_user_bus_and_rejects_live_unit(self):
        raw = b"LoadState=not-found\nActiveState=inactive\nMainPID=0\nControlGroup=\n"
        with patch.object(fixture["_shared"], "execute", return_value=raw) as execute:
            self.assertEqual(fixture["terminal_unit"]("fixed-unit")[0], raw)
            environment = execute.call_args.kwargs["env"]
            self.assertEqual(environment["XDG_RUNTIME_DIR"], "/run/user/"+str(__import__("os").geteuid()))
            self.assertNotIn("DBUS_SESSION_BUS_ADDRESS", environment)
        with patch.object(fixture["_shared"], "execute", return_value=raw.replace(b"MainPID=0", b"MainPID=42")):
            with self.assertRaises(RuntimeError):
                fixture["terminal_unit"]("fixed-unit")

    def test_bound_receipt(self):
        value, expected = receipt()
        self.assertEqual(parse(json.dumps(value), expected), value)

    def test_failure_identity_boolean_and_cleanup_mutations(self):
        value, expected = receipt()
        changes = [lambda v: v.update(Version=True), lambda v: v.update(Passed=1),
                   lambda v: v.update(BeforeGrantInert=False), lambda v: v.update(Recovered=False),
                   lambda v: v.update(ProductionBoundaryQualified=0), lambda v: v.update(SourceHead="f"*40),
                   lambda v: v.update(ManifestHash="0"*64), lambda v: v.update(Error="cleanup failed"),
                   lambda v: v["Execution"].update(VerifiedExecutable=False), lambda v: v["Execution"].update(QEMUExited=1),
                   lambda v: v["Journal"].update(Stopped=False), lambda v: v["Journal"].update(PayloadKind=""),
                   lambda v: v.update(Root=None), lambda v: v.update(unexpected=True)]
        for change in changes:
            bad = copy.deepcopy(value)
            change(bad)
            with self.subTest(value=bad), self.assertRaises(RuntimeError):
                parse(json.dumps(bad), expected)

    def test_duplicate_and_missing_documents(self):
        value, expected = receipt()
        for raw in ["null", "[]", "{}", json.dumps(value).replace('"Passed": true', '"Passed": false, "Passed": true')]:
            with self.subTest(raw=raw), self.assertRaises(RuntimeError):
                parse(raw, expected)
