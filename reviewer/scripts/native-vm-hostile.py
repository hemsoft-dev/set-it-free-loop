#!/usr/bin/env python3
"""Run every native hostile fixture serially and record a measured matrix.

Each case is one fake-only native-vm-fake.py run with fresh synthetic auth and
a loopback TLS fake model. After every run the owned unit is checked again
through the user manager and cgroup filesystem. No provider account, public
backend, App or deployment route is available here.
"""
import argparse
import fcntl
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import time
import signal
import sys

# Kept in the same order as internal/nativefixture hostileCases. The Go runner
# refuses unknown names; this list only selects the serial matrix.
HOSTILE = [
    ("claude", "claude-bash"), ("claude", "claude-builtin-read"), ("claude", "claude-web-fetch"),
    ("claude", "claude-web-search"), ("claude", "claude-server-web-search"), ("claude", "claude-task"),
    ("claude", "claude-foreign-mcp"), ("claude", "claude-source-mcp"),
    ("codex", "codex-shell"), ("codex", "codex-exec-command"), ("codex", "codex-local-shell"),
    ("codex", "codex-web-search"), ("codex", "codex-spawn-agent"), ("codex", "codex-direct-mcp"),
    ("codex", "codex-source-mcp"), ("codex", "codex-code-probe"), ("codex", "codex-metadata"),
    ("codex", "codex-settings"),
]
# Existing denial cases: Claude Bash write and Codex apply_patch write.
BASELINE = [("claude", "forbidden"), ("codex", "forbidden")]
# Resource-exhaustion cases, in the order of internal/nativefixture
# exhaustionCases. Selected with --matrix exhaustion; no baseline is added.
EXHAUSTION = [
    ("claude", "claude-response-flood"), ("claude", "claude-turn-flood"), ("claude", "claude-stall"),
    ("claude", "claude-source-oversize"),
    ("codex", "codex-response-flood"), ("codex", "codex-turn-flood"), ("codex", "codex-stall"),
    ("codex", "codex-source-flood"), ("codex", "codex-source-oversize"), ("codex", "codex-code-memory"),
    ("codex", "codex-code-output-flood"), ("codex", "codex-code-spin"),
]
PEAK = re.compile(r"Consumed .*?, ([0-9.]+[KMGT]?) memory peak")
# A pinned client that re-advertises a forbidden tool is a recorded client
# failure even when every host layer contains it. It never becomes a pass.
CLIENT_FAILURE_CHECKS = {"inventory-fixed", "no-backend-problem"}


# Exact check sets per case kind, matching cmd/nativevmfixture. A result that
# omits or adds a check is a runner regression, not a verdict.
COMMON_CHECKS = {"auth-returned", "inventory-fixed", "no-backend-problem", "no-canary-in-launch-files",
                 "no-canary-in-report", "no-canary-in-requests", "no-denied-destination", "no-stop-pending",
                 "no-telemetry", "provider-stopped", "runtime-root-removed", "unit-captured-and-stopped"}
GUARD_CHECKS = {"no-source-access", "response-policy-denied", "single-inference", "typed-guard-refusal"}
REQUIRED_CHECKS = {
    "response-guard": [COMMON_CHECKS | GUARD_CHECKS],
    "source-reader": [COMMON_CHECKS | {"complete-report", "every-call-reached-broker", "hostile-results-denied",
                                       "only-allowed-reads", "planned-inferences", "store-saw-only-allowlisted"}],
    "code-host": [COMMON_CHECKS | {"code-host-contained", "complete-report", "no-source-access", "planned-inferences"}],
    # Metadata checks a complete report while the inventory holds and a guard
    # refusal once the client re-advertises tools.
    "metadata": [COMMON_CHECKS | {"metadata-delivered", "complete-report", "only-allowed-reads", "planned-inferences"},
                 COMMON_CHECKS | {"metadata-delivered"} | GUARD_CHECKS],
}
# Case kinds, matching internal/nativefixture hostileCases.
CASE_KINDS = {name: "source-reader" if name.endswith("-source-mcp") else "code-host" if name == "codex-code-probe"
              else "metadata" if name in ("codex-metadata", "codex-settings") else "response-guard"
              for _, name in HOSTILE}


def cases(matrix="hostile"):
    if matrix == "exhaustion":
        return list(EXHAUSTION)
    return [(provider, name) for provider, name in HOSTILE + BASELINE]


def verdict(name, result):
    """Classify one runner result as pass, client-failure or fail."""
    if result.get("FakeOnly") is not True or result.get("RuntimeQualified") or result.get("ProviderEligibilityQualified"):
        return "fail"
    if name == "forbidden":
        return "pass" if result.get("Passed") is True and result.get("Negative") is True else "fail"
    checks, kind = result.get("Checks"), CASE_KINDS.get(name)
    if not isinstance(checks, dict) or result.get("Case") != name or result.get("Kind") != kind \
            or set(checks) not in REQUIRED_CHECKS.get(kind, []):
        return "fail"
    failed = {key for key, ok in checks.items() if ok is not True}
    if not failed and result.get("Passed") is True and result.get("HostContained") is True:
        return "pass"
    if failed and failed <= CLIENT_FAILURE_CHECKS and result.get("HostContained") is True and result.get("Passed") is False:
        return "client-failure"
    return "fail"


# native-vm-fake.py may take two 120 s build steps plus its 125 s runner bound.
CASE_TIMEOUT = 420


def unit_stopped(unit, props, anchor=Path("/sys/fs/cgroup")):
    """Independent stop proof: the manager has no live main PID and the
    captured cgroup is gone or empty with its original inode."""
    if not (unit.get("Captured") and unit.get("Stopped") and unit.get("PayloadKind") == "native-vm"):
        return False
    if props.get("MainPID") != "0" or props.get("ActiveState") not in ("inactive", "failed"):
        return False
    if props.get("LoadState") != "not-found" and props.get("InvocationID") != unit["Identity"]["invocation_id"]:
        return False
    group = anchor/unit["Cgroup"]["Path"].lstrip("/")
    # systemd may remove an empty group between these reads; disappearance is
    # the gone state. A different inode is never accepted as the original.
    try:
        if group.stat().st_ino != unit["Cgroup"]["Inode"]:
            return False
        return "populated 0" in (group/"cgroup.events").read_text() and not (group/"cgroup.procs").read_text().strip()
    except FileNotFoundError:
        return True


def memory_peak(unit):
    """The user manager's own accounting line for the stopped unit, if any.
    It covers the whole unit cgroup: QEMU, its guest RAM and the stub."""
    try:
        raw = subprocess.run(["journalctl", "--user", "-u", unit, "-o", "cat", "--no-pager"],
                             capture_output=True, text=True, timeout=20).stdout
    except (OSError, subprocess.SubprocessError):
        return None
    return parse_peak(raw)


def parse_peak(text):
    """Last 'memory peak' value systemd logged for a unit, or None."""
    found = PEAK.findall(text)
    return found[-1] if found else None


def manager_properties(name):
    raw = subprocess.check_output(["systemctl", "--user", "show", name,
                                   "--property=LoadState,InvocationID,MainPID,ActiveState"], text=True, timeout=10)
    return dict(line.split("=", 1) for line in raw.splitlines() if "=" in line)


def native_units():
    """Loaded native VM containment units, by name."""
    raw = subprocess.check_output(["systemctl", "--user", "list-units", "sfl-alpha-*", "--all", "--plain",
                                   "--no-legend"], text=True, timeout=10)
    names = {line.split()[0] for line in raw.splitlines() if line.strip()}
    found = set()
    for name in names:
        show = subprocess.check_output(["systemctl", "--user", "show", name, "--property=Description"],
                                       text=True, timeout=10)
        if show.strip().startswith("Description=SFL native VM containment "):
            found.add(name)
    return found


FIXTURE_LOCK = Path.home()/".cache"/"sfl-native-fixture.lock"
LOCK_ENV = "SFL_NATIVE_FIXTURE_LOCKED"
STATE_GLOB = "sfl-nvm-*"


def hold_fixture_lock():
    """Hold the cross-process native fixture lock for this process, unless an
    ancestor runner already holds it and said so through the environment."""
    if os.environ.get(LOCK_ENV):
        return None
    FIXTURE_LOCK.parent.mkdir(parents=True, exist_ok=True)
    handle = open(FIXTURE_LOCK, "a")
    fcntl.flock(handle, fcntl.LOCK_EX)
    os.environ[LOCK_ENV] = str(os.getpid())
    return handle


def process_table():
    """Map each visible pid to (state, parent pid, process group)."""
    table = {}
    for entry in os.scandir("/proc"):
        if entry.name.isdigit():
            try:
                fields = Path("/proc", entry.name, "stat").read_text().rsplit(")", 1)[1].split()
                table[int(entry.name)] = (fields[0], int(fields[1]), int(fields[2]))
            except (OSError, IndexError, ValueError):
                continue
    return table


def kill_tree(root, deadline=10):
    """Kill root and every descendant, including those in nested sessions,
    and each of their process groups. Return whether all are gone.

    The tree is frozen with SIGSTOP first, so no member exits or forks and
    reparents a child outside the walk before the kill."""
    stopped = set()
    for _ in range(100):
        table = process_table()
        tree, grown = {root} & set(table), True
        while grown:
            grown = {pid for pid, (_, parent, _) in table.items() if parent in tree} - tree
            tree |= grown
        fresh = tree - stopped
        if not fresh:
            break
        for pid in fresh:
            try:
                os.kill(pid, signal.SIGSTOP)
            except ProcessLookupError:
                pass
        stopped |= fresh
    groups = {table[pid][2] for pid in stopped if pid in table} - {os.getpgrp()}
    for group in groups:
        try:
            os.killpg(group, signal.SIGKILL)
        except ProcessLookupError:
            pass
    for pid in stopped:
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    end = time.monotonic() + deadline
    while True:
        table = process_table()
        alive = {pid for pid in stopped | {root} if pid in table and table[pid][0] != "Z"}
        alive |= {pid for pid, (state, _, group) in table.items() if group in groups and state != "Z"}
        if not alive or time.monotonic() > end:
            return not alive
        time.sleep(0.05)


def state_roots(anchor=Path("/tmp")):
    return set(anchor.glob(STATE_GLOB))


def owned_units(roots):
    """Units this run launched: each launch directory names its unit, and the
    directory exists before the unit starts."""
    return {f"sfl-alpha-{launch.name}.service" for root in roots
            for launch in (root/"launches").glob("*") if launch.is_dir()}


def stop_new_units(before, owned, deadline=30):
    """After an abnormal case exit, kill only native units that appeared
    during the case AND belong to its own state root, then require them
    inactive. Any other unit is never touched. Returns True only when every
    owned unit stopped."""
    stopped = True
    for name in sorted((native_units() - before) & set(owned)):
        subprocess.run(["systemctl", "--user", "kill", "--signal=SIGKILL", name], timeout=10)
        end = time.monotonic() + deadline
        while time.monotonic() < end:
            props = manager_properties(name)
            if props.get("MainPID") == "0" and props.get("ActiveState") in ("inactive", "failed"):
                break
            time.sleep(0.2)
        else:
            stopped = False
    return stopped


def run_case(script, args, provider, name, output):
    command = [sys.executable, "-B", str(script), "--run-fake-only", "--image", str(args.image),
               "--output", str(output), "--provider", provider, "--go", args.go,
               "--module-cache", str(args.module_cache)]
    command += ["--case", "forbidden"] if name == "forbidden" else ["--case", "hostile", "--hostile", name]
    before, roots_before = native_units(), state_roots()
    owned = lambda: owned_units(state_roots() - roots_before)
    with output.with_suffix(".log").open("xb") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            returncode = process.wait(timeout=CASE_TIMEOUT)
        except subprocess.TimeoutExpired:
            # Kill the whole case, not only the wrapper, then prove its units
            # stopped before the matrix reports and stops.
            # native-vm-fake.py runs the fixture in its own session, so the
            # wrapper's group alone does not reach it.
            killed = kill_tree(process.pid)
            process.wait()
            return {"provider": provider, "case": name, "verdict": "fail", "timed_out": True,
                    "exit_code": None, "process_tree_killed": killed,
                    "unit_stopped_independently": stop_new_units(before, owned()), "abort": True}
    completed = subprocess.CompletedProcess(command, returncode)
    try:
        receipt = json.loads((output/"receipt.json").read_text())
        if not isinstance(receipt, dict) or not isinstance(receipt.get("state_directory"), str):
            raise ValueError("malformed receipt")
    except (OSError, ValueError):
        # The runner exited without a usable receipt: record the failure,
        # prove any unit it started is stopped, and end the matrix.
        return {"provider": provider, "case": name, "verdict": "fail", "timed_out": False,
                "exit_code": returncode, "receipt": "missing-or-malformed",
                "unit_stopped_independently": stop_new_units(before, owned()), "abort": True}
    if receipt.get("timed_out"):
        # The inner runner killed only its direct fixture process; its unit
        # may still run. Stop it as for an outer timeout and end the matrix.
        return {"provider": provider, "case": name, "verdict": "fail", "timed_out": True,
                "exit_code": returncode, "runner_head": receipt.get("source_commit"),
                "unit_stopped_independently": stop_new_units(before, owned()), "abort": True}
    root = Path(receipt["state_directory"])
    result_path = root/"result.json"
    units = list(root.glob("launches/*/unit.json"))
    try:
        result = json.loads(result_path.read_text()) if result_path.exists() else {}
        if not isinstance(result, dict):
            raise ValueError("result is not an object")
        unit = json.loads(units[0].read_text()) if len(units) == 1 else None
        if unit is not None and not isinstance(unit, dict):
            raise ValueError("unit receipt is not an object")
    except (OSError, ValueError):
        # A truncated or malformed receipt proves nothing about the unit:
        # record the failure, stop what the case started and end the matrix.
        return {"provider": provider, "case": name, "verdict": "fail", "timed_out": False,
                "exit_code": returncode, "result": "malformed", "runner_head": receipt.get("source_commit"),
                "unit_stopped_independently": stop_new_units(before, owned()), "abort": True}
    stopped = False
    peak = None
    if unit is not None:
        try:
            stopped = unit_stopped(unit, manager_properties(unit["Unit"])) and not (units[0].parent/"qemu-runtime").exists()
            peak = memory_peak(unit["Unit"])
        except (KeyError, TypeError, AttributeError, subprocess.SubprocessError):
            stopped = False
    if not stopped:
        # The case returned but its stop is not independently proven, or its
        # runtime root remains. Clean up like a timeout and end the matrix.
        return {"provider": provider, "case": name, "kind": result.get("Kind", "baseline-denial"), "verdict": "fail",
                "timed_out": False, "exit_code": returncode, "runner_head": receipt.get("source_commit"),
                "state": result.get("State"), "error_class": result.get("ErrorClass"),
                "unit_stopped_independently": False, "cleanup_stopped_units": stop_new_units(before, owned()),
                "result": str(result_path), "abort": True}
    outcome = verdict(name, result)
    # An abnormal fixture exit never qualifies, even after a passing result
    # was written. A contained client failure exits nonzero by design.
    if outcome == "pass" and returncode != 0:
        outcome = "fail"
    return {"provider": provider, "case": name, "kind": result.get("Kind", "baseline-denial"),
            "verdict": outcome, "exit_code": completed.returncode, "timed_out": receipt["timed_out"],
            "runner_head": receipt["source_commit"], "image_head": receipt["image_source_commit"],
            "state": result.get("State"), "error_class": result.get("ErrorClass"),
            "guest_stage": result.get("GuestStage"), "failed_checks": sorted(k for k, v in (result.get("Checks") or {}).items() if v is not True),
            "elapsed_seconds": receipt["elapsed_seconds"], "unit_stopped_independently": stopped,
            "runner_elapsed_ms": result.get("ElapsedMS"), "unit_memory_peak": peak,
            "result": str(result_path),
            "result_sha256": hashlib.sha256(result_path.read_bytes()).hexdigest() if result_path.exists() else None}


def expected_heads(script, image):
    """Return the committed runner head and the image's source head."""
    root = script.resolve().parent.parent.parent
    runner = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip()
    with (image/"manifest.json").open("rb") as stream:
        manifest = json.loads(stream.read(65537)[:65536])
    return runner, manifest.get("source_commit") if isinstance(manifest, dict) else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-fake-only", action="store_true", required=True)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--module-cache", type=Path, required=True)
    parser.add_argument("--only", action="append", default=[])
    parser.add_argument("--matrix", choices=["hostile", "exhaustion"], default="hostile")
    args = parser.parse_args()
    unknown = sorted(set(args.only) - {name for _, name in cases(args.matrix)})
    if unknown:
        parser.error("unknown case: " + ", ".join(unknown))
    selected = [c for c in cases(args.matrix) if not args.only or c[1] in args.only]
    if not selected:
        parser.error("no matching case")
    args.output.mkdir(mode=0o700, parents=False, exist_ok=False)
    lock = hold_fixture_lock()
    script = Path(__file__).with_name("native-vm-fake.py")
    # One matrix qualifies one runner revision and one image. Every row must
    # report the revisions captured here, or the matrix fails and stops.
    expected = expected_heads(script, args.image)
    rows = []
    for provider, name in selected:
        rows.append(run_case(script, args, provider, name, args.output/f"{provider}-{name}"))
        if (rows[-1].get("runner_head"), rows[-1].get("image_head")) != expected:
            rows[-1].update({"verdict": "fail", "expected_heads": list(expected), "abort": True})
        with (args.output/"matrix.json").open("w") as stream:
            json.dump(rows, stream, indent=2)
            stream.write("\n")
        print(json.dumps(rows[-1]), flush=True)
        if rows[-1].get("abort"):
            break
    verdicts = {row["verdict"] for row in rows}
    # 3 records a contained client capability failure; it blocks that provider.
    raise SystemExit(1 if "fail" in verdicts else 3 if "client-failure" in verdicts else 0)


if __name__ == "__main__":
    main()
