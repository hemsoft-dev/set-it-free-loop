"""Validate a private receipt produced by the actual pinned sibling code host."""
import copy
import importlib.util
import json
import pathlib
import unittest
from unittest import mock
import os
import tempfile

HERE = pathlib.Path(__file__).parent
SPEC = importlib.util.spec_from_file_location("codehost_fixture_observation", HERE / "codex-fixture.py")
FIXTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURE)


class CodeHostReceiptTests(unittest.TestCase):
    def test_actual_pinned_producer_and_hostile_mutations(self):
        captured = json.loads((HERE / "testdata/actual-codehost-broker.json").read_text())
        expected = captured["bwrap_sha256"]
        self.assertTrue(FIXTURE.code_host_observation_passed(captured, expected))
        self.assertFalse(FIXTURE.code_host_observation_passed(captured, "0" * 64))
        for field, value in [("version", True), ("version", 1), ("passed", False), ("probe_passed", False),
                             ("relay_failure", True), ("bwrap_sha256", "f" * 64), ("fake_only", False), ("production_boundary_qualified", True)]:
            mutated = copy.deepcopy(captured)
            mutated[field] = value
            self.assertFalse(FIXTURE.code_host_observation_passed(mutated, expected), field)
        for direction in ["input", "output"]:
            for field, value in [("bytes", 0), ("bytes", True), ("bytes", (8 << 20) + 1),
                                 ("synthetic_secret_seen", True), ("capped", True),
                                 ("sha256", "f" * 63), ("sha256", "g" * 64)]:
                mutated = copy.deepcopy(captured)
                mutated[direction][field] = value
                self.assertFalse(FIXTURE.code_host_observation_passed(mutated, expected), (direction, field))
            mutated = copy.deepcopy(captured)
            mutated[direction] = []
            self.assertFalse(FIXTURE.code_host_observation_passed(mutated, expected))
        for malformed in [None, [], {}, {"version": 1}]:
            self.assertFalse(FIXTURE.code_host_observation_passed(malformed, expected))

    def test_path_override_cannot_change_the_recorded_namespace_executable(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "bwrap"
            path.write_text("#!/bin/sh\nexit 99\n")
            path.chmod(0o700)
            with mock.patch.dict(os.environ, {"PATH": directory}):
                self.assertEqual(FIXTURE.bwrap_binary(), "/usr/bin/bwrap")


if __name__ == "__main__":
    unittest.main()
