"""Validate a private receipt produced by the actual pinned sibling code host."""
import copy
import importlib.util
import json
import pathlib
import unittest

HERE = pathlib.Path(__file__).parent
SPEC = importlib.util.spec_from_file_location("codehost_fixture_observation", HERE / "codex-fixture.py")
FIXTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURE)


class CodeHostReceiptTests(unittest.TestCase):
    def test_actual_pinned_producer_and_hostile_mutations(self):
        captured = json.loads((HERE / "testdata/actual-codehost-broker.json").read_text())
        self.assertTrue(FIXTURE.code_host_observation_passed(captured))
        for field, value in [("version", True), ("version", 2), ("passed", False), ("probe_passed", False),
                             ("fake_only", False), ("production_boundary_qualified", True)]:
            mutated = copy.deepcopy(captured)
            mutated[field] = value
            self.assertFalse(FIXTURE.code_host_observation_passed(mutated), field)
        for direction in ["input", "output"]:
            for field, value in [("bytes", 0), ("bytes", True), ("bytes", (8 << 20) + 1),
                                 ("synthetic_secret_seen", True), ("capped", True),
                                 ("sha256", "f" * 63), ("sha256", "g" * 64)]:
                mutated = copy.deepcopy(captured)
                mutated[direction][field] = value
                self.assertFalse(FIXTURE.code_host_observation_passed(mutated), (direction, field))
            mutated = copy.deepcopy(captured)
            mutated[direction] = []
            self.assertFalse(FIXTURE.code_host_observation_passed(mutated))
        for malformed in [None, [], {}, {"version": 1}]:
            self.assertFalse(FIXTURE.code_host_observation_passed(malformed))


if __name__ == "__main__":
    unittest.main()
