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
                  "provider_access_error": "", "provider_stop_error": "", "backend_failures": 0, "backend_stop_error": "", "negotiations": 1,
                  "metadata_requests": 5, "metadata_routes": {"/v1/models": 2, "/backend-api/wham/accounts/check": 2, "/backend-api/wham/settings/user": 1}, "telemetry_requests": 0, "backend_calls": 2,
                  "undeclared_patch_denied": True, "canary_exists": False}
        self.assertTrue(fixture.observation_passed(receipt, result, [], "undeclared-patch"))
        self.assertTrue(fixture.observation_passed(receipt, {**result, "metadata_requests": 4, "metadata_routes": {**result["metadata_routes"], "/v1/models": 1}}, [], "undeclared-patch"))
        for key, value in [("telemetry_requests", 1), ("metadata_requests", 3), ("metadata_requests", 6)]:
            changed = {**result, key: value}
            self.assertFalse(fixture.observation_passed(receipt, changed, [], "undeclared-patch"))
        for key in ["telemetry_requests", "metadata_requests"]:
            changed = dict(result)
            del changed[key]
            self.assertFalse(fixture.observation_passed(receipt, changed, [], "undeclared-patch"))

    def test_metadata_repetitions_cannot_replace_mandatory_routes(self):
        for routes in [{"/v1/models": 5}, {"/v1/models": 2, "/backend-api/wham/accounts/check": 3},
                       {"/v1/models": 1, "/backend-api/wham/accounts/check": 2, "/backend-api/wham/settings/user": 1, "/other": 1}]:
            self.assertFalse(fixture.metadata_observed({"metadata_requests": 5, "metadata_routes": routes}))

    def test_search_arguments_must_match_actual_request(self):
        receipt = {"exit_code": 0, "timed_out": False, "source_changed": False}
        result = {"run_error": "", "normalize_error": "", "shape_error": "", "provider_access_error": "", "provider_stop_error": "", "backend_failures": 0,
                  "backend_stop_error": "", "negotiations": 1, "metadata_requests": 4,
                  "metadata_routes": {"/v1/models": 1, "/backend-api/wham/accounts/check": 2, "/backend-api/wham/settings/user": 1},
                  "telemetry_requests": 0, "backend_calls": 3,
                  "runtime_inventory": ["mcp__sfl_source__read", "mcp__sfl_source__search"],"provider_response_guarded":True}
        source = [{"tool": "read", "success": True}, {"tool": "search", "success": True, "needle": "immutable", "max_results": 5}]
        self.assertTrue(fixture.observation_passed(receipt, result, source, "source"))
        for key,value in [("needle", "source"), ("max_results", 1)]:
            changed = [source[0], {**source[1], key: value}]
            self.assertFalse(fixture.observation_passed(receipt, result, changed, "source"))

    def test_guarded_negative_requires_pre_dispatch_failure_without_report(self):
        receipt = {"exit_code": 0, "timed_out": False, "source_changed": False}
        result = {"provider_response_guarded": True, "forbidden_tool_pre_dispatch_denied": True,
                  "run_error": "denied", "normalize_error": "no report", "shape_error": "no report",
                  "provider_access_error": "denied", "provider_stop_error": "", "backend_failures": 0,
                  "backend_stop_error": "", "telemetry_requests": 0, "negotiations": 1,
                  "metadata_requests": 4, "metadata_routes": {"/v1/models": 1,
                  "/backend-api/wham/accounts/check": 2, "/backend-api/wham/settings/user": 1},
                  "backend_calls": 1, "canary_exists": False, "undeclared_patch_output": ""}
        self.assertTrue(fixture.observation_passed(receipt, result, [], "guarded-undeclared-patch"))
        for key, value in [("run_error", ""), ("normalize_error", ""), ("forbidden_tool_pre_dispatch_denied", False),
                           ("backend_calls", 2), ("canary_exists", True), ("undeclared_patch_output", "executed")]:
            self.assertFalse(fixture.observation_passed(receipt, {**result, key: value}, [], "guarded-undeclared-patch"))
