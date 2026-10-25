import importlib.util
import copy
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


def result(name, variant=0, **changes):
    kind = module.CASE_KINDS.get(name, "response-guard")
    value = {"Case": name, "Kind": kind, "FakeOnly": True, "RuntimeQualified": False, "ProviderEligibilityQualified": False,
             "Passed": True, "HostContained": True, "Checks": dict.fromkeys((module.required_checks(name) or module.REQUIRED_CHECKS[kind])[variant], True)}
    if name == "codex-settings":
        binding = {"SessionHash": "a"*64, "LaunchID": "b"*32, "ContainmentID": "c"*64}
        value.update(HelperBinding=binding, HelperRequestHash="d"*64, DenyHelperSHA256="f"*64,
                     ImageSourceHead="e"*40, HelperOutcome="no-observed-attempt",
                     ExecObservation={"Version": 1, "Binding": binding.copy(), "RequestHash": "d"*64,
                                      "ImageSourceHead": "e"*40, "HelperSHA256": "f"*64, "HelperEntry": "sh-deny-v1", "UID": 1202,
                                      "Attempts": 0, "Denied": 0, "Failed": False, "Complete": True})
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


def nested_case(script, marker):
    """Write a case that starts a child in its own session, as native-vm-fake.py
    starts nativevmfixture, and a grandchild in a further session. The marker
    lists both pids once both exist."""
    script.write_text("import os,subprocess,sys,time\n"
                      "grand=['import subprocess,sys,time',\n"
                      "       'g=subprocess.Popen([sys.executable,\"-c\",\"import time; time.sleep(120)\"], start_new_session=True)',\n"
                      "       'print(g.pid, flush=True)', 'time.sleep(120)']\n"
                      "child=subprocess.Popen([sys.executable,'-c','\\n'.join(grand)], start_new_session=True, stdout=subprocess.PIPE)\n"
                      "grandchild=child.stdout.readline().strip().decode()\n"
                      f"open({str(marker)!r}+'.tmp','w').write(f'{{child.pid}} {{grandchild}}')\n"
                      f"os.rename({str(marker)!r}+'.tmp',{str(marker)!r})\n"
                      "time.sleep(120)\n")


def all_stopped(marker):
    pids = [int(pid) for pid in marker.read_text().split()]
    for _ in range(100):
        if not any(running(pid) for pid in pids):
            return len(pids) == 2
        time.sleep(0.05)
    return False


class HostileMatrixTests(unittest.TestCase):
    def test_settings_without_hook_observation_cannot_pass(self):
        old = result("codex-settings")
        old["Checks"].pop("hook-observation-bound", None)
        old["Checks"].pop("hook-command-denied", None)
        self.assertEqual(module.verdict("codex-settings", old), "fail")

    def test_helper_observation_distinguishes_denial_unknown_and_unsafe(self):
        zero = result("codex-settings")
        self.assertEqual(module.verdict("codex-settings", zero), "pass")
        denied = copy.deepcopy(zero)
        denied["ExecObservation"].update(Attempts=1, Denied=1)
        denied["HelperOutcome"] = "attempted-and-denied"
        self.assertEqual(module.verdict("codex-settings", denied), "pass")
        for key, wrong in [("Attempts", 2), ("Denied", 0), ("Complete", False), ("Failed", True),
                           ("UID", 1203), ("RequestHash", "0"*64), ("HelperSHA256", "0"*64),
                           ("ImageSourceHead", "0"*40), ("Binding", {}), ("Version", 0), ("Version", True),
                           ("UID", 1202.0), ("HelperEntry", "interpreter"), ("unexpected", "forged")]:
            bad = copy.deepcopy(denied)
            bad["ExecObservation"][key] = wrong
            self.assertEqual(module.verdict("codex-settings", bad), "fail", key)
        for observation in [None, {}, "truncated", {"Version": 1}]:
            self.assertEqual(module.verdict("codex-settings", dict(zero, ExecObservation=observation)), "fail")
        for claim in ["ignored-hook", "no-execution", "unsafe", "unknown"]:
            self.assertEqual(module.verdict("codex-settings", dict(zero, HelperOutcome=claim)), "fail")

    def test_case_list_matches_the_go_fixture(self):
        source = (Path(__file__).resolve().parent.parent/"internal"/"nativefixture"/"hostile_linux.go").read_text()
        declared = re.findall(r'\{"((?:claude|codex)-[a-z-]+)", r\.(Claude|Codex), Hostile', source)
        self.assertEqual([(provider.lower(), name) for name, provider in declared], module.HOSTILE)
        self.assertEqual(len(module.cases()), len(module.HOSTILE)+2)
        kinds = dict(re.findall(r'(Hostile[A-Za-z]+)\s*= "([a-z-]+)"', source))
        table = re.findall(r'\{"((?:claude|codex)-[a-z-]+)", r\.(?:Claude|Codex), (Hostile[A-Za-z]+)\}', source)
        hostile = {name: kind for name, kind in module.CASE_KINDS.items() if (name.split("-")[0], name) in module.HOSTILE}
        self.assertEqual({name: kinds[kind] for name, kind in table}, hostile)
        self.assertEqual(set(module.REQUIRED_CHECKS), set(kinds.values()))

    def test_required_checks_match_the_go_result(self):
        source = (Path(__file__).resolve().parent.parent/"cmd"/"nativevmfixture"/"hostile_linux.go").read_text()
        common = source[source.index("checks := map[string]bool{"):]
        common = common[:common.index("\n\t}\n")]
        exhaustion = (Path(__file__).resolve().parent.parent/"cmd"/"nativevmfixture"/"exhaustion_linux.go").read_text()
        declared = set(re.findall(r'checks\["([a-z-]+)"\] =', source+exhaustion)) | set(re.findall(r'\n\t\t"([a-z-]+)":', common))
        # The unknown-case sentinel is always false and never required.
        declared.discard("known-exhaustion-case")
        required = set().union(*(variant for name in module.CASE_KINDS for variant in module.required_checks(name)))
        self.assertEqual(required, declared)
        # Each exhaustion case carries exactly the checks its Go branch sets.
        switch = exhaustion[exhaustion.index("\tswitch in.Name {"):exhaustion.index("\tdefault:")]
        for branch in switch.split("\n\tcase ")[1:]:
            names = re.findall(r'"((?:claude|codex)-[a-z-]+)"', branch.split(":\n", 1)[0])
            keys = set(re.findall(r'checks\["([a-z-]+)"\] =', branch))
            for name in names:
                self.assertEqual(module.EXHAUSTION_CHECKS[name], keys, name)
        self.assertEqual(set(module.EXHAUSTION_CHECKS), {name for _, name in module.EXHAUSTION})

    def test_verdict_requires_the_exact_check_set_and_kind(self):
        for name, kind in module.CASE_KINDS.items():
            for variant in range(len(module.required_checks(name))):
                complete = result(name, variant)
                self.assertEqual(module.verdict(name, complete), "pass", name)
                for key in complete["Checks"]:
                    missing = dict(complete, Checks={k: v for k, v in complete["Checks"].items() if k != key})
                    self.assertEqual(module.verdict(name, missing), "fail", (name, key))
                extra = dict(complete, Checks=dict(complete["Checks"], **{"unexpected": True}))
                self.assertEqual(module.verdict(name, extra), "fail", name)
                self.assertEqual(module.verdict(name, dict(complete, Kind="other")), "fail", name)
        # A client failure needs the full set too, not only the allowed failing keys.
        partial = result("codex-metadata", 1, Passed=False, Checks={"inventory-fixed": False})
        self.assertEqual(module.verdict("codex-metadata", partial), "fail")

    def test_exhaustion_list_matches_the_go_fixture(self):
        source = (Path(__file__).resolve().parent.parent/"internal"/"nativefixture"/"exhaustion_linux.go").read_text()
        declared = re.findall(r'\{"((?:claude|codex)-[a-z-]+)", r\.(Claude|Codex), HostileExhaust\}', source)
        self.assertEqual([(provider.lower(), name) for name, provider in declared], module.EXHAUSTION)
        self.assertEqual(module.cases("exhaustion"), module.EXHAUSTION)
        self.assertFalse(set(module.EXHAUSTION) & set(module.cases()))
        self.assertIn('HostileExhaust = "exhaustion"', source)
        self.assertEqual({name: module.CASE_KINDS[name] for _, name in module.EXHAUSTION}, dict.fromkeys((n for _, n in module.EXHAUSTION), "exhaustion"))

    def test_returning_exhaustion_cases_are_judged_strictly(self):
        typed = {"claude-stall": "expired-phase", "codex-stall": "expired-phase",
                 "codex-code-memory": "typed-phase-failure", "codex-request-flood": "typed-phase-failure"}
        self.assertIn(("codex", "codex-request-flood"), module.EXHAUSTION)
        self.assertEqual({name for name, checks in module.EXHAUSTION_CHECKS.items() if module.RETURNED_CHECKS <= checks},
                         set(typed))
        for name, phase in typed.items():
            [checks] = module.required_checks(name)
            self.assertTrue(module.RETURNED_CHECKS | {phase, "auth-returned-or-fenced"} <= checks, name)
            self.assertNotIn("typed-failure", checks, name)
            passing = result(name)
            self.assertEqual(module.verdict(name, passing), "pass", name)
            # Fenced auth, a lost audit or an untyped class fails the case even
            # when every host containment check held. It is never reported as
            # a contained client failure.
            for check in sorted(module.RETURNED_CHECKS | {phase}):
                for contained in (True, False):
                    failed = dict(passing, Passed=False, HostContained=contained,
                                  Checks=dict(passing["Checks"], **{check: False}))
                    self.assertEqual(module.verdict(name, failed), "fail", (name, check, contained))
        # The pre-change results, which accepted fenced auth and a lost audit,
        # no longer match any check set.
        legacy = {
            "codex-stall": {"typed-failure", "guard-cancelled-stall", "unaccepted-upstream-recorded", "deadline-bounded",
                            "single-inference", "no-source-access"},
            "codex-code-memory": {"typed-failure", "allocation-dispatched", "memory-bound-ended-cell", "no-source-access"},
        }
        for name, checks in legacy.items():
            old = result(name, Checks=dict.fromkeys(module.EXHAUSTION_COMMON | checks, True))
            self.assertEqual(module.verdict(name, old), "fail", name)

    def test_other_exhaustion_cases_are_unchanged(self):
        flood = {"typed-failure", "flood-passed-bound", "egress-exhausted", "single-inference", "no-source-access"}
        turn = {"typed-failure", "turn-bound-reached", "every-turn-used-source"}
        oversize = {"first-read-delivered", "second-read-denied", "byte-bound", "honest-outcome"}
        unchanged = {
            "claude-response-flood": flood, "codex-response-flood": flood, "claude-turn-flood": turn, "codex-turn-flood": turn,
            "claude-source-oversize": oversize, "codex-source-oversize": oversize,
            "codex-source-flood": {"tool-call-bound", "client-saw-bound", "honest-outcome"},
            "codex-code-spin": {"cell-yielded", "code-host-stopped", "ended-before-deadline", "typed-or-validated", "no-source-access"},
            "codex-code-output-flood": {"typed-failure", "relay-limit-reached", "no-source-access"},
        }
        for name, checks in unchanged.items():
            self.assertEqual(module.EXHAUSTION_CHECKS[name], checks, name)
            self.assertEqual(module.required_checks(name), [module.EXHAUSTION_COMMON | checks], name)
        self.assertEqual(set(unchanged) | {"claude-stall", "codex-stall", "codex-code-memory", "codex-request-flood"},
                         {name for _, name in module.EXHAUSTION})

    def test_memory_peak_is_the_last_accounting_line(self):
        log = ("Started x.\nx.service: Consumed 4.790s CPU time over 5.090s wall clock time, 1.1G memory peak.\n"
               "x.service: Consumed 1.000s CPU time, 812.5M memory peak.\n")
        self.assertEqual(module.parse_peak(log), "812.5M")
        self.assertIsNone(module.parse_peak("Started x.\n"))

    def test_only_complete_contained_results_pass(self):
        self.assertEqual(module.verdict("codex-shell", result("codex-shell")), "pass")
        for bad in [result("codex-shell", Passed=False), result("codex-shell", HostContained=False),
                    result("other-case"), result("codex-shell", Checks={}),
                    result("codex-shell", RuntimeQualified=True), result("codex-shell", FakeOnly=False),
                    result("codex-shell", Kind="metadata"),
                    result("codex-shell", Checks=dict(result("codex-shell")["Checks"], **{"unit-captured-and-stopped": False}), Passed=False)]:
            self.assertEqual(module.verdict("codex-shell", bad), "fail", bad)

    def test_client_failure_requires_contained_host_and_only_inventory_checks(self):
        failed = result("claude-source-mcp", Passed=False)
        failed["Checks"].update({"inventory-fixed": False, "no-backend-problem": False})
        self.assertEqual(module.verdict("claude-source-mcp", failed), "client-failure")
        self.assertEqual(module.verdict("claude-source-mcp", dict(failed, HostContained=False)), "fail")
        self.assertEqual(module.verdict("claude-source-mcp", dict(failed, Passed=True)), "fail")
        widened = dict(failed, Checks=dict(failed["Checks"], **{"no-canary-in-requests": False}))
        self.assertEqual(module.verdict("claude-source-mcp", widened), "fail")

    def test_codex_inventory_change_is_a_host_failure(self):
        # The Codex request policy owns the inventory. A changed inventory that
        # reached the model is never a contained client failure.
        for name in ["codex-source-mcp", "codex-shell", "codex-code-probe"]:
            failed = result(name, Passed=False)
            failed["Checks"].update({"inventory-fixed": False, "no-backend-problem": False})
            self.assertEqual(module.verdict(name, failed), "fail", name)
        # A Codex backend problem alone may still be a contained client failure.
        problem = result("codex-source-mcp", Passed=False)
        problem["Checks"]["no-backend-problem"] = False
        self.assertEqual(module.verdict("codex-source-mcp", problem), "client-failure")

    def test_codex_metadata_requires_the_request_refusal(self):
        refused = result("codex-metadata", 1)
        self.assertEqual(set(refused["Checks"]), module.COMMON_CHECKS - {"inventory-fixed"} | module.REQUEST_REFUSAL_CHECKS)
        self.assertEqual(module.verdict("codex-metadata", refused), "pass")
        for check in ["request-policy-denied", "no-inference-forwarded", "auth-returned", "unit-captured-and-stopped"]:
            failed = dict(refused, Passed=False, HostContained=False, Checks=dict(refused["Checks"], **{check: False}))
            self.assertEqual(module.verdict("codex-metadata", failed), "fail", check)
        # The measured pre-#276 result: the widened request reached the model
        # and only the response policy refused its tool calls. Even with every
        # host check true it no longer matches a metadata check set.
        forwarded = result("codex-metadata", Passed=False, HostContained=True,
                           Checks=dict.fromkeys(module.COMMON_CHECKS | {"metadata-delivered"} | module.GUARD_CHECKS, True))
        forwarded["Checks"].update({"inventory-fixed": False, "no-backend-problem": False})
        self.assertEqual(module.verdict("codex-metadata", forwarded), "fail")
        # A widened request that reached the model under the new check set.
        leaked = dict(refused, Passed=False, HostContained=False,
                      Checks=dict(refused["Checks"], **{"request-policy-denied": False, "no-inference-forwarded": False,
                                                        "no-backend-problem": False}))
        self.assertEqual(module.verdict("codex-metadata", leaked), "fail")

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
            nested_case(script, marker)
            args = type("Args", (), {"image": root, "go": "go", "module_cache": root})()
            saved = (module.CASE_TIMEOUT, module.native_units, module.stop_new_units)
            stopped = []
            module.CASE_TIMEOUT = 15
            module.native_units = lambda: set()
            module.stop_new_units = lambda before, owned: stopped.append(before) or True
            start = time.monotonic()
            try:
                row = module.run_case(script, args, "codex", "codex-shell", root/"case")
            finally:
                module.CASE_TIMEOUT, module.native_units, module.stop_new_units = saved
            self.assertGreaterEqual(time.monotonic()-start, 14, "case returned before its timeout")
            self.assertEqual((row["verdict"], row["timed_out"], row["abort"], row["unit_stopped_independently"], row["process_tree_killed"]),
                             ("fail", True, True, True, True))
            self.assertEqual(stopped, [set()])
            if not marker.exists():
                self.fail("setup: case did not start its child within the patched timeout")
            if not all_stopped(marker):
                self.fail("timed-out case left its child running")

    def _receipt_case(self, root, state, unit_props=None, exit_code=0):
        """Run a case whose runner leaves a completed receipt for state."""
        output = root/"case"
        script = root/"runner.py"
        script.write_text(f"import json,os; os.makedirs({str(output)!r}, exist_ok=True); "
                          f"json.dump({{'state_directory':{str(state)!r},'timed_out':False,'source_commit':'x',"
                          f"'image_source_commit':'y','elapsed_seconds':1}}, open({str(output/'receipt.json')!r},'w'))\n"
                          f"raise SystemExit({exit_code})\n")
        args = type("Args", (), {"image": root, "go": "go", "module_cache": root})()
        saved = (module.native_units, module.stop_new_units, module.manager_properties)
        stopped = []
        module.native_units = lambda: {"sfl-alpha-before.service"}
        module.stop_new_units = lambda before, owned: stopped.append(before) or True
        module.manager_properties = lambda name: unit_props or {}
        try:
            row = module.run_case(script, args, "codex", "codex-shell", output)
        finally:
            module.native_units, module.stop_new_units, module.manager_properties = saved
        return row, stopped

    def test_passing_result_from_a_nonzero_exit_fails(self):
        unit = {"Unit": "sfl-alpha-x.service", "Captured": True, "Stopped": True, "PayloadKind": "native-vm",
                "Identity": {"invocation_id": "abc"}, "Cgroup": {"Path": "/nonexistent-sfl-test.slice/x.service", "Inode": 0}}
        gone = {"LoadState": "not-found", "MainPID": "0", "ActiveState": "inactive", "InvocationID": ""}
        for code in [0, 1, 137]:
            with tempfile.TemporaryDirectory(prefix="sfl-hostile-exit-") as directory:
                root = Path(directory)
                state = root/"state"
                launch = state/"launches"/"l1"
                launch.mkdir(parents=True)
                (launch/"unit.json").write_text(json.dumps(unit))
                (state/"result.json").write_text(json.dumps(result("codex-shell")))
                row, _ = self._receipt_case(root, state, gone, exit_code=code)
                self.assertEqual((row["verdict"], row["exit_code"]), ("pass" if code == 0 else "fail", code), code)

    def _main(self, only, heads):
        """Run main with fake cases that report the given runner and image heads."""
        saved = (module.run_case, module.expected_heads, module.hold_fixture_lock, sys.argv)
        ran = []
        def run_case(script, args, provider, name, output):
            ran.append(name)
            runner, image = heads[len(ran)-1]
            return {"provider": provider, "case": name, "verdict": "pass", "runner_head": runner, "image_head": image}
        with tempfile.TemporaryDirectory(prefix="sfl-hostile-main-") as directory:
            module.run_case, module.expected_heads, module.hold_fixture_lock = run_case, lambda script, image: ("r", "i"), lambda: None
            sys.argv = ["native-vm-hostile.py", "--run-fake-only", "--image", directory, "--output", directory+"/out",
                        "--go", "go", "--module-cache", directory] + [f"--only={name}" for name in only]
            try:
                with self.assertRaises(SystemExit) as exit:
                    module.main()
                rows = json.loads(Path(directory, "out", "matrix.json").read_text()) if ran else []
            finally:
                module.run_case, module.expected_heads, module.hold_fixture_lock, sys.argv = saved
        return exit.exception.code, ran, rows

    def test_every_only_selector_must_name_a_case(self):
        stderr = sys.stderr
        sys.stderr = open(os.devnull, "w")
        try:
            for only in [["codex-shell", "codex-code-proeb"], ["nope"]]:
                code, ran, _ = self._main(only, [("r", "i")])
                self.assertEqual((code, ran), (2, []), only)
        finally:
            sys.stderr.close()
            sys.stderr = stderr
        self.assertEqual(self._main(["codex-shell"], [("r", "i")])[:2], (0, ["codex-shell"]))

    def test_matrix_aborts_when_a_row_reports_another_revision(self):
        cases = ["codex-shell", "codex-exec-command", "codex-local-shell"]
        for heads in [[("r", "i"), ("r2", "i")], [("r", "i"), ("r", "i2")], [("r2", "i2")]]:
            code, ran, rows = self._main(cases, heads + [("r", "i")]*3)
            self.assertEqual((code, len(ran)), (1, len(heads)), heads)
            self.assertEqual((rows[-1]["verdict"], rows[-1]["abort"], rows[-1]["expected_heads"]), ("fail", True, ["r", "i"]))
        code, ran, rows = self._main(cases, [("r", "i")]*3)
        self.assertEqual((code, ran), (0, cases))

    def test_expected_heads_reads_the_committed_runner_and_image_manifest(self):
        with tempfile.TemporaryDirectory(prefix="sfl-hostile-heads-") as directory:
            Path(directory, "manifest.json").write_text(json.dumps({"source_commit": "abc"}))
            script = Path(module.__file__)
            head = subprocess.check_output(["git", "-C", str(script.parent), "rev-parse", "HEAD"], text=True).strip()
            self.assertEqual(module.expected_heads(script, Path(directory)), (head, "abc"))

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
            nested_case(script, marker)
            calls = []
            units = type("Units", (), {"native_units": staticmethod(lambda: {"sfl-alpha-before.service"}),
                                       "stop_new_units": staticmethod(lambda before, owned: calls.append(before) or True)})
            with (root/"log").open("xb") as log:
                exit_code, timed_out, stopped = fake.run_fixture([sys.executable, str(script)], dict(os.environ), log, 15, units)
            self.assertEqual((exit_code, timed_out, stopped), (None, True, True))
            self.assertEqual(calls, [{"sfl-alpha-before.service"}])
            if not marker.exists():
                self.fail("setup: fixture did not start its child within the timeout")
            if not all_stopped(marker):
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
                module.stop_new_units = lambda before, owned: stopped.append(before) or True
                try:
                    row = module.run_case(script, args, "codex", "codex-shell", output)
                finally:
                    module.native_units, module.stop_new_units = saved
                self.assertEqual((row["verdict"], row["abort"], row["unit_stopped_independently"]), ("fail", True, True), kind)
                self.assertEqual(stopped, [{"sfl-alpha-before.service"}], kind)

    def test_standalone_fake_runner_cleans_up_after_a_nonzero_exit_only(self):
        fake_spec = importlib.util.spec_from_file_location("native_vm_fake_exit", Path(__file__).with_name("native-vm-fake.py"))
        fake = importlib.util.module_from_spec(fake_spec)
        fake_spec.loader.exec_module(fake)
        with tempfile.TemporaryDirectory(prefix="sfl-hostile-exit-") as directory:
            root = Path(directory)
            state = root/"sfl-nvm-cccc"
            (state/"launches"/("f"*32)).mkdir(parents=True)
            for code, expected in [(0, None), (3, True)]:
                calls = []
                units = type("Units", (), {"native_units": staticmethod(lambda: set()),
                                           "owned_units": staticmethod(module.owned_units),
                                           "stop_new_units": staticmethod(lambda before, owned: calls.append(set(owned)) or True)})
                with (root/f"log-{code}").open("xb") as log:
                    result = fake.run_fixture([sys.executable, "-c", f"raise SystemExit({code})"], dict(os.environ), log, 30, units, state)
                self.assertEqual(result, (code, False, expected), code)
                self.assertEqual(calls, [] if code == 0 else [{"sfl-alpha-" + "f"*32 + ".service"}], code)

    def test_cleanup_kills_only_units_owned_by_the_case_state_root(self):
        with tempfile.TemporaryDirectory(prefix="sfl-hostile-owned-") as directory:
            anchor = Path(directory)
            mine = anchor/"sfl-nvm-aaaa"
            (mine/"launches"/"0123456789abcdef0123456789abcdef").mkdir(parents=True)
            (anchor/"sfl-nvm-bbbb"/"launches").mkdir(parents=True)
            owned = module.owned_units({mine})
            self.assertEqual(owned, {"sfl-alpha-0123456789abcdef0123456789abcdef.service"})
            killed = []
            saved = (module.native_units, module.manager_properties, module.subprocess.run)
            module.native_units = lambda: {"sfl-alpha-old.service", "sfl-alpha-foreign.service", *owned}
            module.manager_properties = lambda name: {"MainPID": "0", "ActiveState": "inactive"}
            module.subprocess.run = lambda command, **kwargs: killed.append(command[-1])
            try:
                self.assertTrue(module.stop_new_units({"sfl-alpha-old.service"}, owned))
            finally:
                module.native_units, module.manager_properties, module.subprocess.run = saved
            self.assertEqual(killed, sorted(owned), "a foreign or pre-existing unit was signalled")

    def test_fake_runner_refuses_mixed_or_unnamed_hostile_selection(self):
        script = Path(__file__).with_name("native-vm-fake.py")
        common = ["--run-fake-only", "--image", "/nonexistent", "--output", "/nonexistent/out", "--provider", "codex", "--go", "go", "--module-cache", "/nonexistent"]
        for extra in [["--case", "hostile"], ["--case", "source", "--hostile", "codex-shell"],
                      ["--case", "hostile", "--hostile", "codex-shell;rm"], ["--case", "hostile", "--hostile", "other-shell"]]:
            run = subprocess.run([sys.executable, "-B", str(script), *common, *extra], capture_output=True, timeout=30)
            self.assertEqual(run.returncode, 2, extra)


if __name__ == "__main__":
    unittest.main()
