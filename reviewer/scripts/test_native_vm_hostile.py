import importlib.util
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("native_vm_hostile", Path(__file__).with_name("native-vm-hostile.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def result(name, **changes):
    value = {"Case": name, "FakeOnly": True, "RuntimeQualified": False, "ProviderEligibilityQualified": False,
             "Passed": True, "HostContained": True, "Checks": {"unit-captured-and-stopped": True, "inventory-fixed": True}}
    value.update(changes)
    return value


class HostileMatrixTests(unittest.TestCase):
    def test_case_list_matches_the_go_fixture(self):
        source = (Path(__file__).resolve().parent.parent/"internal"/"nativefixture"/"hostile_linux.go").read_text()
        declared = re.findall(r'\{"((?:claude|codex)-[a-z-]+)", r\.(Claude|Codex), Hostile', source)
        self.assertEqual([(provider.lower(), name) for name, provider in declared], module.HOSTILE)
        self.assertEqual(len(module.cases()), len(module.HOSTILE)+2)

    def test_only_complete_contained_results_pass(self):
        self.assertEqual(module.verdict("codex-shell", result("codex-shell")), "pass")
        for bad in [result("codex-shell", Passed=False), result("codex-shell", HostContained=False),
                    result("other-case"), result("codex-shell", Checks={}),
                    result("codex-shell", RuntimeQualified=True), result("codex-shell", FakeOnly=False),
                    result("codex-shell", Checks={"unit-captured-and-stopped": False}, Passed=False)]:
            self.assertEqual(module.verdict("codex-shell", bad), "fail", bad)

    def test_client_failure_requires_contained_host_and_only_inventory_checks(self):
        failed = result("codex-metadata", Passed=False, Checks={"inventory-fixed": False, "no-backend-problem": False, "typed-guard-refusal": True})
        self.assertEqual(module.verdict("codex-metadata", failed), "client-failure")
        self.assertEqual(module.verdict("codex-metadata", dict(failed, HostContained=False)), "fail")
        self.assertEqual(module.verdict("codex-metadata", dict(failed, Passed=True)), "fail")
        widened = dict(failed, Checks={"inventory-fixed": False, "no-canary-in-requests": False})
        self.assertEqual(module.verdict("codex-metadata", widened), "fail")

    def test_baseline_denial_requires_negative_pass(self):
        base = {"FakeOnly": True, "RuntimeQualified": False, "ProviderEligibilityQualified": False, "Passed": True, "Negative": True}
        self.assertEqual(module.verdict("forbidden", base), "pass")
        self.assertEqual(module.verdict("forbidden", dict(base, Negative=False)), "fail")
        self.assertEqual(module.verdict("forbidden", dict(base, Passed=False)), "fail")

    def test_independent_stop_requires_inactive_manager_and_empty_original_group(self):
        with tempfile.TemporaryDirectory(prefix="sfl-hostile-cgroup-") as directory:
            anchor = Path(directory)
            group = anchor/"app.slice"/"sfl-alpha-x.service"
            unit = {"Captured": True, "Stopped": True, "PayloadKind": "native-vm", "Identity": {"invocation_id": "abc"},
                    "Cgroup": {"Path": "/app.slice/sfl-alpha-x.service", "Inode": 0}}
            gone = {"LoadState": "not-found", "MainPID": "0", "ActiveState": "inactive", "InvocationID": ""}
            self.assertTrue(module.unit_stopped(unit, gone, anchor))
            self.assertFalse(module.unit_stopped(unit, dict(gone, MainPID="42"), anchor))
            self.assertFalse(module.unit_stopped(unit, dict(gone, ActiveState="active"), anchor))
            self.assertFalse(module.unit_stopped(unit, dict(gone, LoadState="loaded", InvocationID="other"), anchor))
            self.assertFalse(module.unit_stopped(dict(unit, Stopped=False), gone, anchor))
            group.mkdir(parents=True)
            (group/"cgroup.events").write_text("populated 0\n")
            (group/"cgroup.procs").write_text("")
            self.assertFalse(module.unit_stopped(unit, gone, anchor), "replaced group inode accepted")
            unit["Cgroup"]["Inode"] = group.stat().st_ino
            self.assertTrue(module.unit_stopped(unit, gone, anchor))
            (group/"cgroup.procs").write_text("123\n")
            self.assertFalse(module.unit_stopped(unit, gone, anchor))

    def test_fake_runner_refuses_mixed_or_unnamed_hostile_selection(self):
        script = Path(__file__).with_name("native-vm-fake.py")
        common = ["--run-fake-only", "--image", "/nonexistent", "--output", "/nonexistent/out", "--provider", "codex", "--go", "go", "--module-cache", "/nonexistent"]
        for extra in [["--case", "hostile"], ["--case", "source", "--hostile", "codex-shell"],
                      ["--case", "hostile", "--hostile", "codex-shell;rm"], ["--case", "hostile", "--hostile", "other-shell"]]:
            run = subprocess.run([sys.executable, "-B", str(script), *common, *extra], capture_output=True, timeout=30)
            self.assertEqual(run.returncode, 2, extra)


if __name__ == "__main__":
    unittest.main()
