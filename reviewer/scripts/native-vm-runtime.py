#!/usr/bin/env python3
"""Build a captured QEMU runtime artifact without starting QEMU or a VM.

Only named ELF dependencies and three fixed ROM files enter the artifact.
The original namespace helper and its host runtime remain explicit bootstrap
prerequisites. No auth/profile, provider, App or deployment path is available.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import struct
import tempfile

MAGIC = b"SFL-QEMU-RUNTIME-1\n"
MAX_MANIFEST = 32 << 10
MAX_DATA = 128 << 20
ROM_NAMES = ("kvmvapic.bin", "linuxboot.bin", "linuxboot_dma.bin")
LIBRARY = re.compile(r"lib[A-Za-z0-9_+.-]+\.so(?:\.[A-Za-z0-9_-]+)*")


def sibling(name):
    spec = importlib.util.spec_from_file_location("sfl_runtime_"+name.replace("-", "_"),
                                                 Path(__file__).with_name(name+".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def canonical(value):
    return sibling("native-vm-image").canonical(value)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def allowed(path):
    if path in ["lib64/ld-linux-x86-64.so.2", *["firmware/"+name for name in ROM_NAMES]]:
        return True
    for prefix in ["lib/x86_64-linux-gnu/", "usr/lib/x86_64-linux-gnu/"]:
        if path.startswith(prefix):
            return LIBRARY.fullmatch(path[len(prefix):]) is not None
    return False


def read_owned(path, expected=None):
    # Package library names can be symlinks. Capture the resolved regular file
    # through one descriptor and store regular bytes, never the symlink itself.
    source = path.resolve(strict=True)
    fd = os.open(source, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_nlink != 1
                or info.st_mode & 0o022 or not 0 < info.st_size <= 64 << 20):
            raise RuntimeError("runtime input must be a bounded root-owned regular file")
        data = stream.read((64 << 20)+1)
    if len(data) != info.st_size or (expected is not None and digest(data) != expected):
        raise RuntimeError("runtime input changed or differs from its pin")
    return data


def encode(program, bodies):
    if (set(program) != {"SHA256", "Bytes"} or not re.fullmatch("[a-f0-9]{64}", program["SHA256"])
            or type(program["Bytes"]) is not int or not 0 < program["Bytes"] <= 64 << 20):
        raise RuntimeError("invalid QEMU pin")
    required = {"lib64/ld-linux-x86-64.so.2", *["firmware/"+name for name in ROM_NAMES]}
    if not required.issubset(bodies) or not any(name in bodies for name in [
            "lib/x86_64-linux-gnu/libc.so.6", "usr/lib/x86_64-linux-gnu/libc.so.6"]):
        raise RuntimeError("runtime interpreter, libc or firmware missing")
    if not 5 <= len(bodies) <= 128 or sum(len(data) for data in bodies.values()) > MAX_DATA:
        raise RuntimeError("runtime artifact exceeds file or byte budget")
    files = []
    for path, data in sorted(bodies.items()):
        if not allowed(path) or type(data) is not bytes or not 0 < len(data) <= 64 << 20:
            raise RuntimeError("invalid runtime entry")
        files.append({"Path": path, "SHA256": digest(data), "Bytes": len(data)})
    manifest = {"Version": 1, "Purpose": "native-qemu-runtime", "Program": program, "Files": files}
    header = canonical(manifest)
    if len(header) > MAX_MANIFEST:
        raise RuntimeError("runtime manifest exceeds budget")
    return manifest, MAGIC+struct.pack(">I", len(header))+header+b"".join(bodies[path] for path in sorted(bodies))


def capture_runtime(data, directory):
    with tempfile.TemporaryDirectory(prefix="elf-", dir=directory) as temporary:
        snapshot = Path(temporary)/"input"
        snapshot.write_bytes(data)
        snapshot.chmod(0o500)
        paths = sibling("native-vm-image").runtime_paths(snapshot)
        result = {}
        for source in paths:
            path = source.lstrip("/")
            if not allowed(path):
                raise RuntimeError("unsupported ELF dependency path")
            result[path] = read_owned(Path(source))
        return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--qemu", type=Path, required=True)
    parser.add_argument("--qemu-sha256", required=True)
    parser.add_argument("--namespace-helper", type=Path, required=True)
    parser.add_argument("--namespace-helper-sha256", required=True)
    parser.add_argument("--firmware-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    common = sibling("claude-fixture")
    module = Path(__file__).resolve().parent.parent
    source = common.source_manifest(module)
    qemu = read_owned(args.qemu, args.qemu_sha256)
    helper = read_owned(args.namespace_helper, args.namespace_helper_sha256)
    with tempfile.TemporaryDirectory(prefix="sfl-qemu-runtime-") as temporary:
        directory = Path(temporary)
        bodies = capture_runtime(qemu, directory)
        helper_bodies = capture_runtime(helper, directory)
    for name in ROM_NAMES:
        bodies["firmware/"+name] = read_owned(args.firmware_dir/name)
    manifest, frame = encode({"SHA256": digest(qemu), "Bytes": len(qemu)}, bodies)
    if common.source_manifest(module) != source:
        raise RuntimeError("source changed during artifact build")
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    artifact = output/"native-qemu-runtime.bin"
    with artifact.open("xb") as stream:
        stream.write(frame)
    artifact.chmod(0o444)
    result = {"version": 1, "purpose": "native-qemu-runtime", **source, "manifest": manifest,
              "artifact": {"Path": str(artifact), "SHA256": digest(frame), "Bytes": len(frame)},
              "namespace_helper": {"Path": str(args.namespace_helper.absolute()), "SHA256": digest(helper), "Bytes": len(helper)},
              "bootstrap_dependencies": [{"Path": "/"+path, "SHA256": digest(data), "Bytes": len(data)}
                                         for path, data in sorted(helper_bodies.items())],
              "bootstrap_note": "Retain the original opened helper inode; its host interpreter/libraries remain trusted host prerequisites, not sealed payload files.",
              "state": "built-only", "kvm_opened": False, "vm_started": False,
              "client_started": False, "runtime_qualified": False}
    (output/"manifest.json").write_text(json.dumps(result, indent=2)+"\n")
    print(json.dumps({"manifest": str(output/"manifest.json"), "state": "built-only",
                      "files": len(manifest["Files"]), "artifact_bytes": len(frame)}))


if __name__ == "__main__":
    main()
