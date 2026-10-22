#!/usr/bin/env python3
"""Qualify one owned native supervisor SIGKILL with synthetic auth and local TLS."""
import argparse
import importlib.util
import io
import json
import os
import signal
from pathlib import Path
import secrets
import subprocess
import tarfile
import tempfile
import time


def sibling(name):
    spec = importlib.util.spec_from_file_location("sfl_crash_"+name.replace("-", "_"),
                                                 Path(__file__).with_name(name+".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module



def group_running(group):
    # Zombies cannot keep work or auth locks live. Bound observation rather than
    # assuming killpg or a reaped parent proves that every child exited.
    entries = list(Path("/proc").iterdir())
    if len(entries) > 32768:
        raise RuntimeError("process observation exceeds fixture bound")
    for entry in entries:
        if not entry.name.isdigit():
            continue
        try:
            raw = (entry/"stat").read_text()
            fields = raw[raw.rfind(")")+2:].split()
            if len(fields) >= 3 and int(fields[2]) == group and fields[0] not in ("Z", "X"):
                return True
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
    return False


def kill_owned_group(process):
    observed = {"stopped": False, "driver_alive_before_signal": process.poll() is None,
                "signal_sent": False, "driver_sigkill_observed": False}
    try:
        os.killpg(process.pid, signal.SIGKILL)
        observed["signal_sent"] = True
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        return observed
    observed["driver_sigkill_observed"] = process.returncode == -signal.SIGKILL
    deadline = time.monotonic()+5
    while time.monotonic() < deadline:
        if not group_running(process.pid):
            observed["stopped"] = True
            return observed
        time.sleep(0.02)
    return observed


def run_owned(program, configuration, environment, log, timeout=190, force_point=None):
    process = subprocess.Popen([str(program), "--run-fake-only", str(configuration)],
                               env=environment, stdout=log, stderr=subprocess.STDOUT,
                               start_new_session=True)
    deadline = time.monotonic()+timeout
    observed_point = False
    while True:
        if force_point is not None and force_point.is_file():
            observed_point = True
            trigger = "forced-timeout-point"
            break
        remaining = deadline-time.monotonic()
        if remaining <= 0:
            trigger = "deadline"
            break
        try:
            process.wait(timeout=min(0.1, remaining))
            if process.returncode == 0:
                return 0, False, {"status": "not-needed"}
            trigger = "driver-error"
            break
        except subprocess.TimeoutExpired:
            continue
    try:
        killed = kill_owned_group(process)
    except (OSError, RuntimeError, ValueError):
        killed = {}
    group_stopped = killed.get("stopped") is True
    record = {"Status": "unknown"}
    recovery = None
    try:
        recovery = subprocess.Popen([str(program), "--recover-owned", str(configuration)],
                                    env=environment, stdout=subprocess.PIPE, stderr=log,
                                    start_new_session=True)
        raw, _ = recovery.communicate(timeout=45)
        if len(raw) <= 8192:
            decoded = json.loads(raw)
            if isinstance(decoded, dict):
                record = decoded
    except (OSError, subprocess.TimeoutExpired, ValueError):
        if recovery is not None:
            kill_owned_group(recovery)
    known = (group_stopped and recovery is not None and recovery.returncode == 0
             and record.get("Status") == "stopped-owned-unit")
    return process.returncode, trigger != "driver-error", {
            "status": "known" if known else "unknown", "trigger": trigger,
            "owned_process_group_stopped": group_stopped,
            "driver_alive_before_signal": killed.get("driver_alive_before_signal") is True,
            "kill_signal_sent": killed.get("signal_sent") is True,
            "driver_sigkill_observed": killed.get("driver_sigkill_observed") is True,
            "forced_point_observed": observed_point, "recovery": record}


def forced_loss_passed(source, code, timed_out, cleanup):
    recovery = cleanup.get("recovery", {})
    return (code == -signal.SIGKILL and timed_out and cleanup.get("status") == "known"
            and cleanup.get("trigger") == "forced-timeout-point"
            and cleanup.get("forced_point_observed") is True
            and all(cleanup.get(key) is True for key in
                ["owned_process_group_stopped", "driver_alive_before_signal",
                 "kill_signal_sent", "driver_sigkill_observed"])
            and recovery.get("Status") == "stopped-owned-unit"
            and recovery.get("SourceHead") == source["source_commit"]
            and recovery.get("SourceTree") == source["source_tree"]
            and all(recovery.get(key) is True for key in
                ["OriginalRootVerified", "PhysicalStop", "RuntimeRemoved", "AuthRecoveryFenced", "AuthUnchanged"]))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-fake-only", action="store_true", required=True)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--provider", choices=["codex", "claude"], required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--module-cache", type=Path, required=True)
    parser.add_argument("--force-driver-timeout", action="store_true")
    args = parser.parse_args()
    common = sibling("claude-fixture")
    native = sibling("native-vm-fake")
    module = Path(__file__).resolve().parent.parent
    source = common.source_manifest(module)
    image = args.image.absolute()
    with (image/"manifest.json").open("rb") as stream:
        raw = stream.read(65537)
    if len(raw) > 65536:
        raise RuntimeError("bounded native image manifest required")
    manifest = json.loads(raw)
    if manifest.get("purpose") != "native-review" or manifest.get("state") != "built-only":
        raise RuntimeError("explicit native image required")
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    program = output/"nativevmcrashfixture"
    archive = subprocess.check_output(["git", "-C", str(module.parent), "archive", "--format=tar",
                                       source["source_commit"], "reviewer"])
    if len(archive) > 16 << 20:
        raise RuntimeError("bounded committed source required")
    with tempfile.TemporaryDirectory(prefix="sfl-native-crash-build-") as directory:
        root = Path(directory)
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:") as tar:
            tar.extractall(root, filter="data")
        environment = common.offline_environment(root/"cache", args.module_cache)
        subprocess.run([args.go, "-C", str(root/"reviewer"), "mod", "verify"],
                       env=environment, check=True, timeout=120)
        flags = "-X=main.sourceHead="+source["source_commit"]+" -X=main.sourceTree="+source["source_tree"]
        subprocess.run([args.go, "-C", str(root/"reviewer"), "build", "-mod=readonly", "-trimpath",
                        "-buildvcs=false", "-ldflags", flags, "-o", str(program), "./cmd/nativevmcrashfixture"],
                       env=environment, check=True, timeout=120)
    program.chmod(0o500)
    if common.source_manifest(module) != source:
        raise RuntimeError("source changed during build")
    state = Path("/tmp")/("sfl-nvc-"+secrets.token_hex(8))
    config = {"Version": 1, "Purpose": "native-process-loss-fake-only",
              "InstanceNonce": secrets.token_hex(16), "ForceDriverTimeout": args.force_driver_timeout,
              "ImageSourceHead": manifest["source_commit"], "Root": str(state), "Provider": args.provider,
              "QEMU": native.asset(Path(manifest["qemu"]["Path"]), manifest["qemu"]["SHA256"]),
              "Firmware": native.asset(image/"bios.bin", manifest["bios_sha256"]),
              "Runtime": native.runtime_spec(image, manifest),
              "Kernel": native.asset(image/"vmlinuz", manifest["kernel_sha256"]),
              "Initrd": native.asset(image/"native.cpio.gz", manifest["initrd_sha256"]),
              "Stub": native.asset(image/"nativevmstub", manifest["programs"]["nativevmstub"])}
    configuration = output/"configuration.json"
    with configuration.open("x") as stream:
        json.dump(config, stream, separators=(",", ":"))
    configuration.chmod(0o600)
    environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "SFL_NATIVE_VM_CRASH_FAKE_ONLY": "1"}
    start = time.monotonic()
    with (output/"runner.log").open("xb") as log:
        code, timed_out, cleanup = run_owned(program, configuration, environment, log,
                    force_point=state/"driver-timeout-point.json" if args.force_driver_timeout else None)
    forced_passed = args.force_driver_timeout and forced_loss_passed(source, code, timed_out, cleanup)
    receipt = {**source, "image_source_commit": manifest["source_commit"], "state_directory": str(state),
               "program_sha256": common.digest(program), "exit_code": code, "timed_out": timed_out,
               "elapsed_seconds": round(time.monotonic()-start, 3), "fake_only": True,
               "runtime_qualified": False, "provider_eligibility_qualified": False,
               "forced_driver_timeout": args.force_driver_timeout, "timeout_cleanup": cleanup,
               "qualification_passed": forced_passed if args.force_driver_timeout else code == 0 and not timed_out}
    with (output/"receipt.json").open("x") as stream:
        json.dump(receipt, stream, indent=2)
        stream.write("\n")
    print(json.dumps(receipt))
    raise SystemExit(0 if receipt["qualification_passed"] else 1)


if __name__ == "__main__":
    main()
