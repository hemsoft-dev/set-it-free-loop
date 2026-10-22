#!/usr/bin/env python3
"""Qualify one owned native supervisor SIGKILL with synthetic auth and local TLS."""
import argparse
import importlib.util
import io
import json
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-fake-only", action="store_true", required=True)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--provider", choices=["codex", "claude"], required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--module-cache", type=Path, required=True)
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
    timed_out, code = False, None
    with (output/"runner.log").open("xb") as log:
        try:
            code = subprocess.run([str(program), "--run-fake-only", str(configuration)],
                                  env=environment, stdout=log, stderr=subprocess.STDOUT, timeout=190).returncode
        except subprocess.TimeoutExpired:
            timed_out = True
    receipt = {**source, "image_source_commit": manifest["source_commit"], "state_directory": str(state),
               "program_sha256": common.digest(program), "exit_code": code, "timed_out": timed_out,
               "elapsed_seconds": round(time.monotonic()-start, 3), "fake_only": True,
               "runtime_qualified": False, "provider_eligibility_qualified": False}
    with (output/"receipt.json").open("x") as stream:
        json.dump(receipt, stream, indent=2)
        stream.write("\n")
    print(json.dumps(receipt))
    raise SystemExit(code if code is not None else 1)


if __name__ == "__main__":
    main()
