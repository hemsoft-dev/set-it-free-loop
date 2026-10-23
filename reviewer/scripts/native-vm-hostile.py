#!/usr/bin/env python3
"""Run every native hostile fixture serially and record a measured matrix.

Each case is one fake-only native-vm-fake.py run with fresh synthetic auth and
a loopback TLS fake model. After every run the owned unit is checked again
through the user manager and cgroup filesystem. No provider account, public
backend, App or deployment route is available here.
"""
import argparse
import hashlib
import json
import os
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
# A pinned client that re-advertises a forbidden tool is a recorded client
# failure even when every host layer contains it. It never becomes a pass.
CLIENT_FAILURE_CHECKS = {"inventory-fixed", "no-backend-problem"}


def cases():
    return [(provider, name) for provider, name in HOSTILE + BASELINE]


def verdict(name, result):
    """Classify one runner result as pass, client-failure or fail."""
    if result.get("FakeOnly") is not True or result.get("RuntimeQualified") or result.get("ProviderEligibilityQualified"):
        return "fail"
    if name == "forbidden":
        return "pass" if result.get("Passed") is True and result.get("Negative") is True else "fail"
    checks = result.get("Checks")
    if not isinstance(checks, dict) or not checks or result.get("Case") != name:
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


def stop_new_units(before, deadline=30):
    """After a timed-out case, kill native units it started through their
    cgroup and require them inactive. Returns True only when all stopped."""
    stopped = True
    for name in sorted(native_units() - before):
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
    before = native_units()
    with output.with_suffix(".log").open("xb") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            returncode = process.wait(timeout=CASE_TIMEOUT)
        except subprocess.TimeoutExpired:
            # Kill the whole case, not only the wrapper, then prove its units
            # stopped before the matrix reports and stops.
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            return {"provider": provider, "case": name, "verdict": "fail", "timed_out": True,
                    "exit_code": None, "unit_stopped_independently": stop_new_units(before), "abort": True}
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
                "unit_stopped_independently": stop_new_units(before), "abort": True}
    if receipt.get("timed_out"):
        # The inner runner killed only its direct fixture process; its unit
        # may still run. Stop it as for an outer timeout and end the matrix.
        return {"provider": provider, "case": name, "verdict": "fail", "timed_out": True,
                "exit_code": returncode, "runner_head": receipt.get("source_commit"),
                "unit_stopped_independently": stop_new_units(before), "abort": True}
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
                "unit_stopped_independently": stop_new_units(before), "abort": True}
    stopped = False
    if unit is not None:
        try:
            stopped = unit_stopped(unit, manager_properties(unit["Unit"])) and not (units[0].parent/"qemu-runtime").exists()
        except (KeyError, TypeError, AttributeError, subprocess.SubprocessError):
            stopped = False
    if not stopped:
        # The case returned but its stop is not independently proven, or its
        # runtime root remains. Clean up like a timeout and end the matrix.
        return {"provider": provider, "case": name, "kind": result.get("Kind", "baseline-denial"), "verdict": "fail",
                "timed_out": False, "exit_code": returncode, "runner_head": receipt.get("source_commit"),
                "state": result.get("State"), "error_class": result.get("ErrorClass"),
                "unit_stopped_independently": False, "cleanup_stopped_units": stop_new_units(before),
                "result": str(result_path), "abort": True}
    outcome = verdict(name, result)
    return {"provider": provider, "case": name, "kind": result.get("Kind", "baseline-denial"),
            "verdict": outcome, "exit_code": completed.returncode, "timed_out": receipt["timed_out"],
            "runner_head": receipt["source_commit"], "image_head": receipt["image_source_commit"],
            "state": result.get("State"), "error_class": result.get("ErrorClass"),
            "guest_stage": result.get("GuestStage"), "failed_checks": sorted(k for k, v in (result.get("Checks") or {}).items() if v is not True),
            "elapsed_seconds": receipt["elapsed_seconds"], "unit_stopped_independently": stopped,
            "result": str(result_path),
            "result_sha256": hashlib.sha256(result_path.read_bytes()).hexdigest() if result_path.exists() else None}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-fake-only", action="store_true", required=True)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--module-cache", type=Path, required=True)
    parser.add_argument("--only", action="append", default=[])
    args = parser.parse_args()
    selected = [c for c in cases() if not args.only or c[1] in args.only]
    if not selected:
        parser.error("no matching case")
    args.output.mkdir(mode=0o700, parents=False, exist_ok=False)
    script = Path(__file__).with_name("native-vm-fake.py")
    rows = []
    for provider, name in selected:
        rows.append(run_case(script, args, provider, name, args.output/f"{provider}-{name}"))
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
