import importlib.util
import json
from pathlib import Path
import re
import time
import os
import subprocess
import sys
import tempfile
import unittest

def load(filename, name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


module = load("native-vm-hostile.py", "native_vm_hostile")


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
    except (OSError, IndexError):
        # FileNotFoundError or ProcessLookupError: the process exited,
        # possibly between opening and reading its stat file.
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
            # The case publishes its child before the patched timeout can
            # matter: the marker is renamed into place after the child exists,
            # and the timeout leaves ample room for interpreter startup under
            # load. A missing marker is reported as setup failure only after
            # the group is known to be gone.
            script.write_text("import os,subprocess,sys,time\n"
                              "child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(120)'])\n"
                              f"open({str(marker)!r}+'.tmp','w').write(str(child.pid))\n"
                              f"os.rename({str(marker)!r}+'.tmp',{str(marker)!r})\n"
                              "time.sleep(120)\n")
            args = type("Args", (), {"image": root, "go": "go", "module_cache": root})()
            saved = (module.CASE_TIMEOUT, module.native_units, module.stop_new_units)
            stopped = []
            module.CASE_TIMEOUT = 15
            module.native_units = lambda: set()
            module.stop_new_units = lambda before: stopped.append(before) or True
            start = time.monotonic()
            try:
                row = module.run_case(script, args, "codex", "codex-shell", root/"case")
            finally:
                module.CASE_TIMEOUT, module.native_units, module.stop_new_units = saved
            self.assertGreaterEqual(time.monotonic()-start, 14, "case returned before its timeout")
            self.assertEqual((row["verdict"], row["timed_out"], row["abort"], row["unit_stopped_independently"]), ("fail", True, True, True))
            self.assertEqual(stopped, [set()])
            if not marker.exists():
                self.fail("setup: case did not start its child within the patched timeout")
            child = int(marker.read_text())
            for _ in range(100):
                if not running(child):
                    break
                time.sleep(0.05)
            else:
                self.fail("timed-out case left its child running")

    def _receipt_case(self, root, state, unit_props=None):
        """Run a case whose runner leaves a completed receipt for state."""
        output = root/"case"
        script = root/"runner.py"
        script.write_text(f"import json,os; os.makedirs({str(output)!r}, exist_ok=True); "
                          f"json.dump({{'state_directory':{str(state)!r},'timed_out':False,'source_commit':'x',"
                          f"'image_source_commit':'y','elapsed_seconds':1}}, open({str(output/'receipt.json')!r},'w'))\n")
        args = type("Args", (), {"image": root, "go": "go", "module_cache": root})()
        saved = (module.native_units, module.stop_new_units, module.manager_properties)
        stopped = []
        module.native_units = lambda: {"sfl-alpha-before.service"}
        module.stop_new_units = lambda before: stopped.append(before) or True
        module.manager_properties = lambda name: unit_props or {}
        try:
            row = module.run_case(script, args, "codex", "codex-shell", output)
        finally:
            module.native_units, module.stop_new_units, module.manager_properties = saved
        return row, stopped

    def test_malformed_result_cleans_up_and_aborts(self):
        for body in ['{"Case": "codex-sh', "[1]", "", '"text"', "\xff"]:
            with tempfile.TemporaryDirectory(prefix="sfl-hostile-result-") as directory:
                root = Path(directory)
                state = root/"state"
                state.mkdir()
                (state/"result.json").write_bytes(body.encode("latin-1"))
                row, stopped = self._receipt_case(root, state)
                self.assertEqual((row["verdict"], row["abort"], row["result"]), ("fail", True, "malformed"), body)
                self.assertEqual(stopped, [{"sfl-alpha-before.service"}], body)

    def test_unproven_stop_after_normal_return_cleans_up_and_aborts(self):
        unit = {"Unit": "sfl-alpha-x.service", "Captured": True, "Stopped": True, "PayloadKind": "native-vm",
                "Identity": {"invocation_id": "abc"}, "Cgroup": {"Path": "/nonexistent-sfl-test.slice/x.service", "Inode": 0}}
        gone = {"LoadState": "not-found", "MainPID": "0", "ActiveState": "inactive", "InvocationID": ""}
        for kind in ["live-main-pid", "runtime-root-remains", "no-unit", "control"]:
            with tempfile.TemporaryDirectory(prefix="sfl-hostile-stop-") as directory:
                root = Path(directory)
                state = root/"state"
                launch = state/"launches"/"l1"
                launch.mkdir(parents=True)
                (state/"result.json").write_text(json.dumps(result("codex-shell")))
                props = gone
                if kind != "no-unit":
                    (launch/"unit.json").write_text(json.dumps(unit))
                if kind == "live-main-pid":
                    props = dict(gone, MainPID="42", ActiveState="active")
                if kind == "runtime-root-remains":
                    (launch/"qemu-runtime").mkdir()
                row, stopped = self._receipt_case(root, state, props)
                if kind == "control":
                    self.assertEqual((row["verdict"], row.get("abort"), stopped), ("pass", None, []), kind)
                    continue
                self.assertEqual((row["verdict"], row["abort"], row["unit_stopped_independently"]), ("fail", True, False), kind)
                self.assertEqual(stopped, [{"sfl-alpha-before.service"}], kind)

    def test_standalone_fake_runner_timeout_stops_new_native_units(self):
        fake = load("native-vm-fake.py", "native_vm_fake")
        with tempfile.TemporaryDirectory(prefix="sfl-fake-timeout-") as directory:
            root = Path(directory)
            marker = root/"child.pid"
            script = root/"slow.py"
            script.write_text("import os,subprocess,sys,time\n"
                              "child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(120)'])\n"
                              f"open({str(marker)!r}+'.tmp','w').write(str(child.pid))\n"
                              f"os.rename({str(marker)!r}+'.tmp',{str(marker)!r})\n"
                              "time.sleep(120)\n")
            calls = []
            units = type("Units", (), {"native_units": staticmethod(lambda: {"sfl-alpha-before.service"}),
                                       "stop_new_units": staticmethod(lambda before: calls.append(before) or True)})
            with (root/"log").open("xb") as log:
                exit_code, timed_out, stopped = fake.run_fixture([sys.executable, str(script)], dict(os.environ), log, 15, units)
            self.assertEqual((exit_code, timed_out, stopped), (None, True, True))
            self.assertEqual(calls, [{"sfl-alpha-before.service"}])
            if not marker.exists():
                self.fail("setup: fixture did not start its child within the timeout")
            child = int(marker.read_text())
            for _ in range(100):
                if not running(child):
                    break
                time.sleep(0.05)
            else:
                self.fail("standalone timeout left the fixture's child running")
            with (root/"log2").open("xb") as log:
                self.assertEqual(fake.run_fixture([sys.executable, "-c", "pass"], dict(os.environ), log, 15, units), (0, False, None))
            self.assertEqual(len(calls), 1, "normal exit must not stop units")

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
