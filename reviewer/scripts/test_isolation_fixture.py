"""Qualification receipt regressions; no kernel, service or provider is run."""
import base64
import copy
import hashlib
import json
from pathlib import Path
import runpy
import unittest

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


if __name__ == "__main__":
    unittest.main()
