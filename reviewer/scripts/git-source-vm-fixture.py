#!/usr/bin/env python3
"""Run one source-only guest against trusted synthetic Git in a bounded unit.

No credential, subscription client, provider endpoint or App is used. This
qualifies the guest data path only, not a production host supervisor or recovery.
"""
import argparse
import importlib.util
import io
import json
import os
from pathlib import Path
import secrets
import subprocess
import tarfile
import tempfile


def sibling(name):
    spec = importlib.util.spec_from_file_location("source_fixture_"+name.replace("-", "_"),
                                                 Path(__file__).with_name(name+".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["image", "output", "module-cache"]:
        parser.add_argument("--"+name, type=Path, required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--case", choices=["normal", "export-close", "control-close", "read-close", "foreign-request"], default="normal")
    args = parser.parse_args()
    common = sibling("claude-fixture")
    lifecycle = sibling("fixture-unit")
    module = Path(__file__).resolve().parent.parent
    source = common.source_manifest(module)
    image = args.image.resolve(strict=True)
    metadata = json.loads((image/"manifest.json").read_bytes())
    if (metadata.get("purpose") != "source-vm-image" or metadata.get("source_commit") != source["source_commit"]
            or metadata.get("source_tree") != source["source_tree"] or metadata.get("source_dirty") is not False):
        raise RuntimeError("guest image must match this clean committed source")
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    raw = subprocess.check_output(["git", "-C", str(module.parent), "archive", "--format=tar",
                                   source["source_commit"], "reviewer"], timeout=15)
    if len(raw) > 16 << 20:
        raise RuntimeError("source archive exceeds build bound")
    binary = output/"source-guest.test"
    with tempfile.TemporaryDirectory(prefix="sfl-source-fixture-build-") as directory:
        root = Path(directory)
        with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
            archive.extractall(root, filter="data")
        environment = common.offline_environment(root/"cache", args.module_cache)
        environment["GOMAXPROCS"] = "2"
        with (output/"build.log").open("wb") as log:
            subprocess.run([args.go, "-C", str(root/"reviewer"), "mod", "verify"],
                           env=environment, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=60)
            subprocess.run([args.go, "-C", str(root/"reviewer"), "test", "-p", "2", "-c",
                            "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", str(binary), "."],
                           env=environment, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=120)
    if common.source_manifest(module) != source:
        raise RuntimeError("source changed during fixture build")
    runtime = "/run/user/"+str(os.getuid())
    environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "HOME": str(Path.home()),
                   "XDG_RUNTIME_DIR": runtime, "DBUS_SESSION_BUS_ADDRESS": "unix:path="+runtime+"/bus"}
    runtime_directory = output/"runtime"
    runtime_directory.mkdir(mode=0o700)
    nonce = secrets.token_hex(8)
    unit_name = "sfl-source-guest-fixture-"+nonce+".service"
    unit = lifecycle.FixtureUnit(unit_name, "SFL credential-free source guest fixture "+nonce, output, environment)
    command = ["/usr/bin/systemd-run", "--user", "--wait", "--pipe", "--collect", "--unit="+unit_name,
               "--property=MemoryMax=2G", "--property=MemorySwapMax=0", "--property=TasksMax=96",
               "--property=CPUQuota=100%", "--property=RuntimeMaxSec=140", "--setenv=SFL_SOURCE_GUEST_IMAGE="+str(image),
               "--setenv=SFL_SOURCE_GUEST_RUN_DIR="+str(runtime_directory),
               "--setenv=SFL_SOURCE_GUEST_CASE="+args.case, "--", str(binary), "-test.run=^TestGitVMGuestContainedActualGit$", "-test.v", "-test.timeout=130s"]
    code, stopped = None, False
    try:
        with (output/"unit.log").open("wb") as log:
            process = unit.start(command, stdout=log, stderr=subprocess.STDOUT)
            code = process.wait(timeout=150)
        unit.stop()
        stopped = unit.stopped
    finally:
        try:
            if unit.captured and not unit.stopped:
                unit.stop()
                stopped = unit.stopped
        finally:
            unit.close()
            result = {"version": 1, "purpose": "source-vm-guest-fixture", **source,
                      "image_manifest_sha256": common.digest(image/"manifest.json"), "unit": unit_name,
                      "exit_code": code, "original_unit_stopped": stopped,
                      "case": args.case, "fixture_passed": code == 0 and stopped,
                      "guest_data_path_passed": args.case == "normal" and code == 0 and stopped, "production_host_qualified": False,
                      "auth_used": False, "provider_used": False, "trusted_release_promoted": False}
            (output/"result.json").write_text(json.dumps(result, indent=2)+"\n")
    print(json.dumps(result))
    if not result["fixture_passed"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
