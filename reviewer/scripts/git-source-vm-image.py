#!/usr/bin/env python3
"""Build a source-only guest image from committed code and explicit pinned inputs.

Build only. No VM, provider, credential, Git remote or App is opened. The output
is an unqualified candidate, never an activation or trusted-release receipt.
"""
import argparse
import io
import json
from pathlib import Path
import stat
import subprocess
import sys
import tarfile
import tempfile
import importlib.util


def sibling(name):
    spec = importlib.util.spec_from_file_location("source_"+name.replace("-", "_"),
                                                 Path(__file__).with_name(name+".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def build(module, args, source, output, common):
    raw = subprocess.check_output(["git", "-C", str(module.parent), "archive", "--format=tar",
                                   source["source_commit"], "reviewer"], timeout=15)
    if len(raw) > 16 << 20:
        raise RuntimeError("source archive exceeds build bound")
    with tempfile.TemporaryDirectory(prefix="sfl-source-build-") as directory:
        root = Path(directory)
        with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
            archive.extractall(root, filter="data")
        environment = common.offline_environment(root/"cache", args.module_cache)
        environment["GOMAXPROCS"] = "2"
        subprocess.run([args.go, "-C", str(root/"reviewer"), "mod", "verify"],
                       env=environment, check=True, timeout=60)
        for name, package in [("source-init", "./cmd/gitsourcevmguest"),
                              ("gitruntime", "./cmd/gitruntime")]:
            subprocess.run([args.go, "-C", str(root/"reviewer"), "build", "-p", "2",
                            "-mod=readonly", "-trimpath", "-buildvcs=false", "-o",
                            str(output/name), package], env=environment, check=True, timeout=120)
            (output/name).chmod(0o500)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["kernel", "kernel-config", "qemu", "namespace-helper", "firmware-dir",
                 "bios", "git", "http-helper", "output", "module-cache"]:
        parser.add_argument("--"+name, type=Path, required=True)
    for name in ["kernel", "kernel-config", "qemu", "namespace-helper", "bios", "git", "http-helper"]:
        parser.add_argument("--"+name+"-sha256", required=True)
    parser.add_argument("--go", required=True)
    args = parser.parse_args()
    image_tools = sibling("native-vm-image")
    common, archive_tools = sibling("claude-fixture"), sibling("isolation-fixture")
    module = Path(__file__).resolve().parent.parent
    source = common.source_manifest(module)
    config = image_tools.pinned_bytes(args.kernel_config, args.kernel_config_sha256, 1 << 20)
    image_tools.kernel_config(config)
    required = ["CONFIG_USER_NS", "CONFIG_UTS_NS", "CONFIG_SECCOMP", "CONFIG_SECCOMP_FILTER",
                "CONFIG_CGROUP_SCHED", "CONFIG_FAIR_GROUP_SCHED"]
    lines = set(config.decode("ascii").splitlines())
    if any(key+"=y" not in lines for key in required):
        raise RuntimeError("source worker requires built-in namespace, filter and CPU facilities")
    captured = {}
    for name, maximum in [("kernel", 64 << 20), ("bios", 64 << 20),
                          ("namespace_helper", 64 << 20), ("git", 64 << 20), ("http_helper", 64 << 20)]:
        captured[name] = image_tools.pinned_bytes(getattr(args, name), getattr(args, name+"_sha256"), maximum)
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    build(module, args, source, output, common)
    subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("native-vm-runtime.py")),
                    "--qemu", str(args.qemu), "--qemu-sha256", args.qemu_sha256,
                    "--namespace-helper", str(args.namespace_helper),
                    "--namespace-helper-sha256", args.namespace_helper_sha256,
                    "--firmware-dir", str(args.firmware_dir), "--output", str(output/"host-runtime")],
                   env=image_tools.BUILD_ENV, check=True, timeout=60)
    host = json.loads((output/"host-runtime/manifest.json").read_bytes())
    subprocess.run([str(output/"gitruntime"), "--git", str(args.git.absolute()),
                    "--git-sha256", args.git_sha256, "--git-bytes", str(len(captured["git"])),
                    "--http-helper", str(args.http_helper.absolute()), "--http-sha256", args.http_helper_sha256,
                    "--http-bytes", str(len(captured["http_helper"])), "--output-dir", str(output/"git-runtime")],
                   env=image_tools.BUILD_ENV, check=True, timeout=120)
    runtime = json.loads((output/"git-runtime/manifest.json").read_bytes())
    guest = image_tools.Image(source["source_commit"])
    for directory in ["proc", "sys", "run", "dev", "tmp"]:
        guest.entry(directory, mode=stat.S_IFDIR | 0o755)
    guest.entry("dev/console", mode=stat.S_IFCHR | 0o600, major=5, minor=1)
    guest.entry("dev/null", mode=stat.S_IFCHR | 0o666, major=1, minor=3)
    worker = (output/"source-init").read_bytes()
    guest.entry("source-init", worker, stat.S_IFREG | 0o555)
    guest.entry("source-bwrap", captured["namespace_helper"], stat.S_IFREG | 0o555)
    runtime_bytes = (output/"git-runtime/git-runtime.bin").read_bytes()
    if len(runtime_bytes) != runtime["pins"]["Artifact"]["Bytes"] or image_tools.digest(runtime_bytes) != runtime["pins"]["Artifact"]["SHA256"]:
        raise RuntimeError("runtime artifact changed")
    guest.entry("source-runtime.bin", runtime_bytes)
    libraries = []
    for pin in host["bootstrap_dependencies"]:
        data = image_tools.pinned_bytes(Path(pin["Path"]).resolve(strict=True), pin["SHA256"], 64 << 20)
        if len(data) != pin["Bytes"]:
            raise RuntimeError("bootstrap size differs")
        guest.entry(pin["Path"].lstrip("/"), data, stat.S_IFREG | 0o555)
        libraries.append({"Path": pin["Path"].lstrip("/"), "SHA256": pin["SHA256"], "Bytes": len(data)})
    def pin(data):
        return {"SHA256": image_tools.digest(data), "Bytes": len(data)}
    release = {"Version": 1, "Purpose": "source-vm-guest", "SourceHead": source["source_commit"],
               "Worker": pin(worker), "NamespaceHelper": pin(captured["namespace_helper"]),
               "Runtime": runtime["pins"], "BootstrapLibraries": libraries, "Modules": []}
    guest.entry("source-release.json", image_tools.canonical(release))
    archive_tools.archive(guest.entries, output/"source.cpio.gz", compresslevel=1)
    for name, data in [("vmlinuz", captured["kernel"]), ("bios.bin", captured["bios"]), ("kernel.config", config)]:
        with (output/name).open("xb") as stream:
            stream.write(data)
        (output/name).chmod(0o444)
    if common.source_manifest(module) != source:
        raise RuntimeError("source changed during image build")
    result = {"version": 1, "purpose": "source-vm-image", **source, "release": release,
              "kernel": pin(captured["kernel"]), "kernel_config": pin(config), "bios": pin(captured["bios"]),
              "initrd": pin((output/"source.cpio.gz").read_bytes()), "host_runtime": host,
              "state": "built-only", "vm_started": False, "runtime_qualified": False,
              "trusted_release_promoted": False}
    (output/"manifest.json").write_text(json.dumps(result, indent=2)+"\n")
    print(json.dumps({"manifest": str(output/"manifest.json"), "state": "built-only"}))


if __name__ == "__main__":
    main()
