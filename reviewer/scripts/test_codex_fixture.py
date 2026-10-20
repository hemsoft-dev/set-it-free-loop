import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location("codex_fixture", pathlib.Path(__file__).with_name("codex-fixture.py"))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class CodexObservationTest(unittest.TestCase):
    def test_telemetry_and_metadata_are_independent_completion_gates(self):
        receipt = {"exit_code": 0, "timed_out": False, "source_changed": False}
        result = {"run_error": "", "normalize_error": "", "shape_error": "",
                  "backend_failures": 0, "backend_stop_error": "", "negotiations": 1,
                  "metadata_requests": 5, "telemetry_requests": 0, "backend_calls": 2,
                  "undeclared_patch_denied": True, "canary_exists": False}
        self.assertTrue(fixture.observation_passed(receipt, result, [], "undeclared-patch"))
        self.assertTrue(fixture.observation_passed(receipt, {**result, "metadata_requests": 4}, [], "undeclared-patch"))
        for key, value in [("telemetry_requests", 1), ("metadata_requests", 3), ("metadata_requests", 6)]:
            changed = {**result, key: value}
            self.assertFalse(fixture.observation_passed(receipt, changed, [], "undeclared-patch"))
        for key in ["telemetry_requests", "metadata_requests"]:
            changed = dict(result)
            del changed[key]
            self.assertFalse(fixture.observation_passed(receipt, changed, [], "undeclared-patch"))
