import importlib.util
from pathlib import Path
import re
import time
import os
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


def running(pid):
    """A process that exists and is not a zombie. Orphaned children may stay
    zombies where PID 1 does not reap them; they can no longer execute."""
    try:
        state = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[0]
    except (FileNotFoundError, IndexError):
        return False
    return state != "Z"


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

    def test_group_removed_during_the_stop_check_is_gone(self):
        with tempfile.TemporaryDirectory(prefix="sfl-hostile-cgroup-") as directory:
            anchor = Path(directory)
            group = anchor/"app.slice"/"sfl-alpha-x.service"
            group.mkdir(parents=True)
            unit = {"Captured": True, "Stopped": True, "PayloadKind": "native-vm", "Identity": {"invocation_id": "abc"},
                    "Cgroup": {"Path": "/app.slice/sfl-alpha-x.service", "Inode": group.stat().st_ino}}
            gone = {"LoadState": "not-found", "MainPID": "0", "ActiveState": "inactive", "InvocationID": ""}
            # The original group exists at stat but its files vanish before read.
            self.assertTrue(module.unit_stopped(unit, gone, anchor))

    def test_timed_out_case_kills_its_process_group_and_aborts(self):
        with tempfile.TemporaryDirectory(prefix="sfl-hostile-timeout-") as directory:
            root = Path(directory)
            marker = root/"child.pid"
            script = root/"slow.py"
            script.write_text("import os,subprocess,sys,time\n"
                              "child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'])\n"
                              f"open({str(marker)!r},'w').write(str(child.pid))\n"
                              "time.sleep(60)\n")
            args = type("Args", (), {"image": root, "go": "go", "module_cache": root})()
            saved = (module.CASE_TIMEOUT, module.native_units, module.stop_new_units)
            stopped = []
            module.CASE_TIMEOUT = 2
            module.native_units = lambda: set()
            module.stop_new_units = lambda before: stopped.append(before) or True
            try:
                row = module.run_case(script, args, "codex", "codex-shell", root/"case")
            finally:
                module.CASE_TIMEOUT, module.native_units, module.stop_new_units = saved
            self.assertEqual((row["verdict"], row["timed_out"], row["abort"], row["unit_stopped_independently"]), ("fail", True, True, True))
            self.assertEqual(stopped, [set()])
            child = int(marker.read_text())
            for _ in range(50):
                if not running(child):
                    break
                time.sleep(0.05)
            else:
                self.fail("timed-out case left its child running")

    def test_missing_or_timed_out_receipt_cleans_up_and_aborts(self):
        for kind in ["missing", "malformed", "inner-timeout"]:
            with tempfile.TemporaryDirectory(prefix="sfl-hostile-receipt-") as directory:
                root = Path(directory)
                output = root/"case"
                script = root/"runner.py"
                if kind == "missing":
                    body = "pass\n"
                elif kind == "malformed":
                    body = f"import os; os.makedirs({str(output)!r}, exist_ok=True); open({str(output/'receipt.json')!r},'w').write('[1]')\n"
                else:
                    body = (f"import json,os; os.makedirs({str(output)!r}, exist_ok=True); "
                            f"json.dump({{'state_directory':'/nonexistent','timed_out':True,'source_commit':'x'}}, open({str(output/'receipt.json')!r},'w'))\n")
                script.write_text(body)
                args = type("Args", (), {"image": root, "go": "go", "module_cache": root})()
                saved = (module.native_units, module.stop_new_units)
                stopped = []
                module.native_units = lambda: {"sfl-alpha-before.service"}
                module.stop_new_units = lambda before: stopped.append(before) or True
                try:
                    row = module.run_case(script, args, "codex", "codex-shell", output)
                finally:
                    module.native_units, module.stop_new_units = saved
                self.assertEqual((row["verdict"], row["abort"], row["unit_stopped_independently"]), ("fail", True, True), kind)
                self.assertEqual(stopped, [{"sfl-alpha-before.service"}], kind)

    def test_fake_runner_refuses_mixed_or_unnamed_hostile_selection(self):
        script = Path(__file__).with_name("native-vm-fake.py")
        common = ["--run-fake-only", "--image", "/nonexistent", "--output", "/nonexistent/out", "--provider", "codex", "--go", "go", "--module-cache", "/nonexistent"]
        for extra in [["--case", "hostile"], ["--case", "source", "--hostile", "codex-shell"],
                      ["--case", "hostile", "--hostile", "codex-shell;rm"], ["--case", "hostile", "--hostile", "other-shell"]]:
            run = subprocess.run([sys.executable, "-B", str(script), *common, *extra], capture_output=True, timeout=30)
            self.assertEqual(run.returncode, 2, extra)


if __name__ == "__main__":
    unittest.main()
