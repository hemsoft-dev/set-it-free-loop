import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location("claude_observation", pathlib.Path(__file__).with_name("claude-fixture.py"))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class ClaudeObservationTest(unittest.TestCase):
    def test_private_stop_and_actual_source_arguments_are_required(self):
        receipt = {"exit_code": 0, "timed_out": False, "source_changed": False}
        result = {"run_error": "", "report_error": "", "backend_stop_error": "",
                  "provider_stop_error": "", "provider_access_error": "", "backend_calls": 3,
                  "backend_requests": 3, "backend_failures": 0, "provider_response_guarded": True}
        source = [{"tool": "read", "revision": "a" * 40, "path": "source.go", "success": True},
                  {"tool": "search", "revision": "a" * 40, "needle": "immutable", "max_results": 5, "success": True}]
        self.assertTrue(fixture.observation_passed(receipt, result, source))
        for field in ["backend_stop_error", "provider_stop_error", "provider_access_error"]:
            self.assertFalse(fixture.observation_passed(receipt, {**result, field: "failed"}, source))
            changed = dict(result)
            del changed[field]
            self.assertFalse(fixture.observation_passed(receipt, changed, source))
        for index, field, value in [(0, "path", "other.go"), (1, "needle", "source"),
                                    (1, "max_results", 1), (1, "revision", "b" * 40), (1, "success", "true")]:
            changed = [dict(item) for item in source]
            changed[index][field] = value
            self.assertFalse(fixture.observation_passed(receipt, result, changed))

    def test_guarded_bash_requires_failure_before_dispatch(self):
        receipt = {"exit_code": 0, "timed_out": False, "source_changed": False}
        result = {"provider_response_guarded": True, "forbidden_tool_pre_dispatch_denied": True,
                  "run_error": "denied", "report_error": "no report", "provider_access_error": "denied",
                  "provider_stop_error": "", "backend_stop_error": "", "backend_calls": 1,
                  "backend_requests": 1, "backend_failures": 0, "canary_exists": False}
        self.assertTrue(fixture.observation_passed(receipt, result, [], "guarded-bash"))
        for key, value in [("run_error", ""), ("report_error", ""), ("provider_access_error", ""),
                           ("forbidden_tool_pre_dispatch_denied", False), ("canary_exists", True), ("backend_calls", 2)]:
            self.assertFalse(fixture.observation_passed(receipt, {**result, key: value}, [], "guarded-bash"))
