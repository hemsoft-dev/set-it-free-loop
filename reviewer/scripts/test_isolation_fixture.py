"""Qualification receipt regressions; no kernel, service or provider is run."""
import base64
import copy
import hashlib
import json
from pathlib import Path
import runpy
import unittest
from unittest.mock import patch

fixture = runpy.run_path(str(Path(__file__).with_name("isolation-fixture.py")))
parse = fixture["parse_guest_result"]


def receipt():
    contents = [b"worker-only evidence\n", b"original source\n", b"worker-only evidence\n"]
    paths = ["worker.go", "source.go", "worker.go"]
    reads = [{"Revision": "a"*40, "Path": path, "Bytes": len(data),
              "Hash": hashlib.sha256(json.dumps(base64.b64encode(data).decode()).encode()).hexdigest()}
             for path, data in zip(paths, contents)]
    return {"version": 1, "nonce": "b"*32, "passed": True, "fake_clients": True,
            "production_boundary_qualified": False,
            "source_access": {"tool_calls": 2, "reader_calls": 3, "reads": reads,
                              "bytes": sum(map(len, contents))},
            "roles": [{"uid": uid, "stopped": True, "memory_max": "100663296", "pids_max": "32",
                       "cpu_max": "100000 100000", "descendants_killed": 1} for uid in [1201, 1202]],
            "reaped_orphans": 1}


class ReceiptTests(unittest.TestCase):
    def test_current_bound_receipt_and_rejections(self):
        good = receipt()
        self.assertEqual(parse(json.dumps(good), "b"*32, "a"*40), good)
        mutations = [lambda value: value.update(nonce="c"*32),
                     lambda value: value.update(passed="false"),
                     lambda value: value.update(passed=1),
                     lambda value: value.update(fake_clients=1),
                     lambda value: value.update(production_boundary_qualified=0),
                     lambda value: value.update(reaped_orphans=True),
                     lambda value: value["roles"][1].update(descendants_killed=0),
                     lambda value: value["roles"][0].update(stopped="true"),
                     lambda value: value["roles"][1].update(memory_max="max"),
                     lambda value: value["source_access"]["reads"][0].update(Revision="c"*40),
                     lambda value: value["source_access"]["reads"][0].update(Hash="0"*64),
                     lambda value: value["source_access"].update(bytes=0)]
        for mutate in mutations:
            bad = copy.deepcopy(good)
            mutate(bad)
            with self.subTest(value=bad), self.assertRaises(RuntimeError):
                parse(json.dumps(bad), "b"*32, "a"*40)

    def test_ambiguous_and_wrong_root_results_never_qualify(self):
        raw = json.dumps(receipt())
        bad_values = [raw.replace('"passed": true', '"passed": false, "passed": true'), "null", "false", "[]"]
        for value in bad_values:
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                parse(value, "b"*32, "a"*40)

    def test_captured_actual_kernel_receipt(self):
        # This is actual guest output from the pinned executable, not a
        # second spelling of the verifier's expected field layout.
        captured = json.loads(Path(__file__).with_name("testdata").joinpath("actual-uid-guest.json").read_text())
        value = captured["guest"]
        self.assertEqual(parse(json.dumps(value), value["nonce"], captured["synthetic_commit"]), value)


class AcceleratorTests(unittest.TestCase):
    def test_explicit_accelerator_has_no_fallback(self):
        options = fixture["accelerator_options"]
        self.assertEqual(options("tcg"), ["-accel", "tcg,thread=single", "-cpu", "max"])
        self.assertEqual(options("kvm"), ["-accel", "kvm", "-cpu", "host"])
        for bad in ["kvm:tcg", "auto", "", None]:
            with self.subTest(value=bad), self.assertRaises(RuntimeError):
                options(bad)

    def test_tcg_does_not_open_hardware(self):
        with patch("os.open", side_effect=AssertionError("unexpected device access")):
            self.assertEqual(fixture["probe_accelerator"]("tcg"),
                             {"accelerator": "tcg", "kvm_device_opened": False})

    def test_kvm_open_api_and_failure_are_explicit(self):
        probe = fixture["probe_accelerator"]
        with patch("os.open", return_value=73) as opened, patch("fcntl.ioctl", return_value=12) as ioctl, patch("os.close") as closed:
            self.assertEqual(probe("kvm"), {"accelerator": "kvm", "kvm_device_opened": True, "kvm_api_version": 12})
            self.assertEqual(opened.call_args.args[0], "/dev/kvm")
            ioctl.assert_called_once_with(73, 0xAE00, 0)
            closed.assert_called_once_with(73)
        with patch("os.open", side_effect=PermissionError("denied")), self.assertRaises(RuntimeError):
            probe("kvm")
        for answer in [11, OSError("ioctl failed")]:
            with self.subTest(answer=answer), patch("os.open", return_value=73), patch("os.close") as closed:
                with patch("fcntl.ioctl", side_effect=answer if isinstance(answer, OSError) else None,
                           return_value=answer), self.assertRaises(RuntimeError):
                    probe("kvm")
                closed.assert_called_once_with(73)


class SealedBootReceiptTests(unittest.TestCase):
    def test_exact_execution_receipt_and_rejections(self):
        expected = {"SourceHead": "a"*40, "BindingHash": "b"*64, "BootSpecHash": "c"*64}
        good = {"Version": 1, **expected, "VerifiedExecutable": True, "QEMUExited": True,
                "FakeOnly": True, "ProductionBoundaryQualified": False, "Error": ""}
        parse = fixture["parse_sealed_boot_receipt"]
        self.assertEqual(parse(json.dumps(good), expected), good)
        for key, value in [("Version", True), ("VerifiedExecutable", 1), ("QEMUExited", False),
                           ("FakeOnly", 1), ("ProductionBoundaryQualified", 0), ("Error", "failed"),
                           ("BindingHash", "d"*64), ("SourceHead", "d"*40), ("BootSpecHash", "d"*64)]:
            bad = {**good, key: value}
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                parse(json.dumps(bad), expected)
        with self.assertRaises(RuntimeError):
            parse(json.dumps(good).replace('"Version": 1', '"Version": 1, "Version": 1'), expected)


if __name__ == "__main__":
    unittest.main()
