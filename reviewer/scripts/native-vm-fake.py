#!/usr/bin/env python3
"""Opt in to one ephemeral KVM review with fresh synthetic auth and fake TLS.

This is runtime qualification, not activation. No provider account/profile,
public backend, App, spending or deployment route is available here.
"""
import argparse
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import secrets
import subprocess
import tarfile
import tempfile
import time


def sibling(name):
    spec = importlib.util.spec_from_file_location("sfl_"+name.replace("-", "_"),
                                                 Path(__file__).with_name(name+".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def asset(path, expected=None):
    if path.is_symlink() or not path.is_file():
        raise RuntimeError("fake boot input must be a regular snapshot")
    value = hashlib.sha256(path.read_bytes()).hexdigest()
    if expected is not None and value != expected:
        raise RuntimeError("fake boot input changed")
    return {"Path": str(path), "SHA256": value, "Bytes": path.stat().st_size}


def runtime_spec(image, manifest):
    captured = manifest["host_runtime"]
    if (captured["purpose"] != "native-qemu-runtime" or captured["state"] != "built-only"
            or captured["source_commit"] != manifest["source_commit"]
            or captured["manifest"]["Program"] != {key: manifest["qemu"][key] for key in ["SHA256", "Bytes"]}):
        raise RuntimeError("captured QEMU runtime does not belong to this image")
    runtime = sibling("native-vm-runtime")
    helper = captured["namespace_helper"]
    helper_data = runtime.read_owned(Path(helper["Path"]), helper["SHA256"])
    if len(helper_data) != helper["Bytes"]:
        raise RuntimeError("namespace helper length changed")
    for library in captured["bootstrap_dependencies"]:
        path = library["Path"]
        if not path.startswith("/") or not runtime.allowed(path[1:]) or path.startswith("/firmware/"):
            raise RuntimeError("invalid bootstrap dependency")
        data = runtime.read_owned(Path(path), library["SHA256"])
        if len(data) != library["Bytes"]:
            raise RuntimeError("bootstrap dependency length changed")
    artifact = asset(image/"host-runtime"/"native-qemu-runtime.bin", captured["artifact"]["SHA256"])
    if artifact["Bytes"] != captured["artifact"]["Bytes"]:
        raise RuntimeError("captured runtime length changed")
    return {"Artifact": artifact, "NamespaceHelper": helper,
            "BootstrapLibraries": captured["bootstrap_dependencies"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-fake-only", action="store_true", required=True)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--provider", choices=["codex", "claude", "both"], required=True)
    parser.add_argument("--case", choices=["source", "forbidden", "adversarial", "self-critique", "refresh", "refresh-uncertain", "cancel-provider", "cancel-reader", "cancel-refresh"], required=True)
    parser.add_argument("--peer-rounds", type=int, choices=[1, 2], default=1)
    parser.add_argument("--go", required=True)
    parser.add_argument("--module-cache", type=Path, required=True)
    args = parser.parse_args()
    if (args.provider == "both") != (args.case == "adversarial"):
        parser.error("adversarial requires both providers; every other case requires one")
    if args.case != "adversarial" and args.peer_rounds != 1:
        parser.error("extra peer rounds require the adversarial case")
    common = sibling("claude-fixture")
    module = Path(__file__).resolve().parent.parent
    source = common.source_manifest(module)
    image = args.image.absolute()
    with (image/"manifest.json").open("rb") as stream:
        raw = stream.read(65537)
    if len(raw) > 65536:
        raise RuntimeError("image manifest exceeds bound")
    manifest = json.loads(raw)
    if manifest.get("purpose") != "native-review" or manifest.get("state") != "built-only":
        raise RuntimeError("explicit native build artifact required")
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    program = output/"nativevmfixture"
    archive = subprocess.check_output(["git", "-C", str(module.parent), "archive", "--format=tar",
                                       source["source_commit"], "reviewer"])
    if len(archive) > 16 << 20:
        raise RuntimeError("fake runner source archive exceeds bound")
    with tempfile.TemporaryDirectory(prefix="sfl-native-fake-build-") as directory:
        root = Path(directory)
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:") as tar:
            tar.extractall(root, filter="data")
        environment = common.offline_environment(root/"cache", args.module_cache)
        subprocess.run([args.go, "-C", str(root/"reviewer"), "mod", "verify"],
                       env=environment, check=True, timeout=120)
        flags = "-X=main.sourceHead="+source["source_commit"]+" -X=main.sourceTree="+source["source_tree"]
        subprocess.run([args.go, "-C", str(root/"reviewer"), "build", "-mod=readonly", "-trimpath",
                        "-buildvcs=false", "-ldflags", flags, "-o", str(program), "./cmd/nativevmfixture"],
                       env=environment, check=True, timeout=120)
    program.chmod(0o500)
    if common.source_manifest(module) != source:
        raise RuntimeError("source changed during fake runner build")
    # Short private state paths preserve sockaddr_un bounds. Existing roots are
    # refused by the runner. Retain journals if stop/auth recovery is uncertain.
    state = Path("/tmp")/("sfl-nvm-"+secrets.token_hex(8))
    config = {"Version": 1, "Purpose": "native-control-fake-only" if args.case == "self-critique" else "native-fake-only", "ImageSourceHead": manifest["source_commit"],
              "Root": str(state), "Provider": "" if args.provider == "both" else args.provider,
              "Negative": args.case == "forbidden", "Refresh": args.case == "refresh", "UncertainRefresh": args.case == "refresh-uncertain",
              "Cancellation": args.case.removeprefix("cancel-") if args.case.startswith("cancel-") else "",
              "ProtocolRounds": args.peer_rounds if args.case == "adversarial" else 0,
              "QEMU": asset(Path(manifest["qemu"]["Path"]), manifest["qemu"]["SHA256"]),
              "Firmware": asset(image/"bios.bin", manifest["bios_sha256"]),
              "Runtime": runtime_spec(image, manifest),
              "Kernel": asset(image/"vmlinuz", manifest["kernel_sha256"]),
              "Initrd": asset(image/"native.cpio.gz", manifest["initrd_sha256"]),
              "Stub": asset(image/"nativevmstub", manifest["programs"]["nativevmstub"])}
    configuration = output/"configuration.json"
    with configuration.open("x") as stream:
        json.dump(config, stream, separators=(",", ":"))
    configuration.chmod(0o600)
    start = time.monotonic()
    # No inherited proxy, provider auth, profile paths or GitHub token.
    environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "SFL_NATIVE_VM_FAKE_ONLY": "1"}
    timed_out = False
    exit_code = None
    with (output/"runner.log").open("xb") as log:
        try:
            result = subprocess.run([str(program), "--run-fake-only", str(configuration)],
                                    env=environment, stdout=log, stderr=subprocess.STDOUT,
                                    timeout=425 if args.case == "self-critique" else 225 if args.case == "refresh" else 125 if args.case != "adversarial" else 2*(1+args.peer_rounds)*100+25)
            exit_code = result.returncode
        except subprocess.TimeoutExpired:
            timed_out = True
    receipt = {**source, "image_source_commit": manifest["source_commit"], "state_directory": str(state),
               "program_sha256": common.digest(program), "exit_code": exit_code, "timed_out": timed_out,
               "elapsed_seconds": round(time.monotonic()-start, 3), "fake_only": True,
               "runtime_qualified": False, "provider_eligibility_qualified": False}
    with (output/"receipt.json").open("x") as stream:
        json.dump(receipt, stream, indent=2)
        stream.write("\n")
    print(json.dumps(receipt))
    raise SystemExit(exit_code if exit_code is not None else 1)


if __name__ == "__main__":
    main()
