#!/usr/bin/env python3
"""Assemble a native-review image from committed helpers and pinned clients.

Build only. This command never opens KVM, starts a unit or client, reads an auth
profile, installs an App or selects a provider endpoint. An image is not evidence
of a qualified runtime or permission to use a subscription.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import subprocess


PINNED = {
    "codex": "399ff69c431c066e40de318fba45c2b07f45a76b19939d247ee4828fc70c4215",
    "claude": "375988f457756524234695cf2d8f64ace99c5f68682e47ea24862a843c191450",
    "codehost": "8724d73ec190536233ab4628d24a071bfbcf27e5124f08fe35129f26fe48b916",
}
ROLES = ["reader", "codex", "claude", "codehost"]
BUILD_ENV = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}


def sibling(name):
    spec = importlib.util.spec_from_file_location("sfl_"+name.replace("-", "_"),
                                                 Path(__file__).with_name(name+".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def canonical(value):
    raw = json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    for character, escaped in [("&", "\\u0026"), ("<", "\\u003c"), (">", "\\u003e"),
                               ("\u2028", "\\u2028"), ("\u2029", "\\u2029")]:
        raw = raw.replace(character, escaped)
    return raw.encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def pinned_bytes(path, expected, maximum):
    if not re.fullmatch("[a-f0-9]{64}", expected):
        raise RuntimeError("invalid immutable digest")
    # The bytes subsequently archived are the same bytes read through this FD.
    fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= maximum:
            raise RuntimeError("image input is not a bounded regular file")
        data = stream.read(maximum+1)
    if len(data) != info.st_size or digest(data) != expected:
        raise RuntimeError("image input changed or digest differs")
    return data


def kernel_config(raw):
    required = {"CONFIG_CGROUPS", "CONFIG_MEMCG", "CONFIG_CGROUP_PIDS", "CONFIG_IPC_NS",
                "CONFIG_PID_NS", "CONFIG_NET_NS", "CONFIG_DEVTMPFS", "CONFIG_VIRTIO",
                "CONFIG_VIRTIO_PCI", "CONFIG_VIRTIO_CONSOLE", "CONFIG_TMPFS", "CONFIG_PROC_FS",
                "CONFIG_SYSFS", "CONFIG_BINFMT_ELF", "CONFIG_BLK_DEV_INITRD"}
    values = {}
    for line in raw.decode("ascii").splitlines():
        if "=" in line and not line.startswith("#"):
            key, value = line.split("=", 1)
            if key in values:
                raise RuntimeError("ambiguous kernel configuration")
            values[key] = value
    if any(values.get(key) != "y" for key in required):
        raise RuntimeError("image requires the captured kernel's built-in guest facilities")


def runtime_paths(binary):
    # Only pinned official images or the fixed /usr/bin/env reach ldd. Include
    # the ELF interpreter, which a =>-only dependency parser would miss.
    result = subprocess.run(["/usr/bin/ldd", str(binary)], capture_output=True,
                            text=True, check=False, timeout=10, env=BUILD_ENV)
    if "not found" in result.stdout:
        raise RuntimeError("missing pinned client runtime dependency")
    if result.returncode and "not a dynamic executable" not in result.stderr+result.stdout:
        raise RuntimeError("cannot capture pinned client runtime")
    paths = sorted(set(re.findall(r"(?:=>\s+)?(/[^\s()]+)", result.stdout)))
    if any(not path.startswith(("/lib/", "/lib64/", "/usr/lib/", "/usr/lib64/"))
           or ".." in Path(path).parts for path in paths):
        raise RuntimeError("unexpected executable runtime dependency")
    return paths


class Image:
    def __init__(self, head):
        self.entries = {}
        self.files = []
        self.head = head
        self.targets = set()
        self.total = 0

    def entry(self, name, data=b"", mode=stat.S_IFREG | 0o444, major=0, minor=0):
        if name.startswith("/") or any(part in ("", ".", "..") for part in name.split("/")):
            raise RuntimeError("invalid image path")
        for end in range(1, len(name.split("/"))):
            parent = "/".join(name.split("/")[:end])
            previous = self.entries.setdefault(parent, (stat.S_IFDIR | 0o755, b"", 0, 0))
            if not stat.S_ISDIR(previous[0]):
                raise RuntimeError("image parent collides with file")
        if name in self.entries:
            raise RuntimeError("duplicate image entry")
        self.entries[name] = (mode, data, major, minor)

    def runtime(self, source, target, data, roles):
        if source != "/native-init" and not source.startswith("/runtime/"):
            raise RuntimeError("invalid runtime source")
        fixed = {"/native-init", "/sourceproxy", "/usr/bin/env", "/client/codex", "/client/claude",
                 "/client/codex-code-mode-host", "/codehost"}
        if ((target not in fixed and not target.startswith(("/lib/", "/lib64/", "/usr/lib/")))
                or ".." in Path(source).parts or ".." in Path(target).parts):
            raise RuntimeError("invalid runtime target")
        if any(role not in ROLES for role in roles) or not roles or len(set(roles)) != len(roles):
            raise RuntimeError("invalid image role")
        for role in roles:
            key = (role, target)
            if key in self.targets:
                raise RuntimeError("duplicate role target")
            self.targets.add(key)
        self.total += len(data)
        if not 0 < len(data) <= 512 << 20 or self.total > 768 << 20:
            raise RuntimeError("native runtime byte budget exceeded")
        name = source.lstrip("/")
        if name not in self.entries:
            self.entry(name, data, stat.S_IFREG | 0o555)
        elif self.entries[name][1] != data:
            raise RuntimeError("conflicting runtime source")
        self.files.append({"Source": source, "Target": target, "SHA256": digest(data),
                           "Bytes": len(data), "Roles": roles})

    def release(self):
        if len(self.files) > 128:
            raise RuntimeError("native runtime file limit exceeded")
        return {"Version": 1, "Purpose": "native-review", "SourceHead": self.head,
                "Files": self.files, "Modules": []}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["kernel", "kernel-config", "codex", "claude", "codehost", "output"]:
        parser.add_argument("--"+name, type=Path, required=True)
    parser.add_argument("--kernel-sha256", required=True)
    parser.add_argument("--kernel-config-sha256", required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--module-cache", type=Path, required=True)
    args = parser.parse_args()
    common, archive = sibling("claude-fixture"), sibling("isolation-fixture")
    module = Path(__file__).resolve().parent.parent
    source = common.source_manifest(module)
    config = pinned_bytes(args.kernel_config, args.kernel_config_sha256, 1 << 20)
    kernel_config(config)
    kernel = pinned_bytes(args.kernel, args.kernel_sha256, 64 << 20)
    clients = {name: pinned_bytes(getattr(args, name), expected, 512 << 20)
               for name, expected in PINNED.items()}
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    snapshots = {}
    for name, data in clients.items():
        snapshots[name] = output / (name+"-snapshot")
        with snapshots[name].open("xb") as stream:
            stream.write(data)
        snapshots[name].chmod(0o500)
    programs = {}
    for name, package in [("native-init", "./cmd/nativevmguest"), ("sourceproxy", "./cmd/sourceproxy"),
                          ("nativevmstub", "./cmd/nativevmstub")]:
        target = output / name
        programs[name] = common.build_offline(module, args.go, target, package, source, args.module_cache)
        target.chmod(0o500)
    image = Image(source["source_commit"])
    for name in ["proc", "sys", "run", "dev"]:
        image.entry(name, mode=stat.S_IFDIR | 0o755)
    image.entry("dev/console", mode=stat.S_IFCHR | 0o600, major=5, minor=1)
    image.entry("dev/null", mode=stat.S_IFCHR | 0o666, major=1, minor=3)
    init = (output / "native-init").read_bytes()
    image.runtime("/native-init", "/native-init", init, ROLES)
    image.runtime("/native-init", "/client/codex-code-mode-host", init, ["codex"])
    image.runtime("/runtime/sourceproxy", "/sourceproxy", (output / "sourceproxy").read_bytes(), ["codex", "claude"])
    for name, data in clients.items():
        target = "/codehost" if name == "codehost" else "/client/"+name
        image.runtime("/runtime/"+name, target, data, [name])
    env = Path("/usr/bin/env")
    image.runtime("/runtime/env", "/usr/bin/env", env.read_bytes(), ["codex", "claude"])
    libraries = {}
    for name in ["codex", "claude", "codehost", "env"]:
        path = env if name == "env" else snapshots[name]
        roles = ["codex", "claude"] if name == "env" else [name]
        for dependency in runtime_paths(path):
            libraries.setdefault(dependency, set()).update(roles)
        if name != "env" and common.digest(path) != PINNED[name]:
            raise RuntimeError("pinned client changed during runtime capture")
    for path, roles in sorted(libraries.items()):
        # Read only the named ELF dependencies. No home, profile, repository
        # content, shell, Git, NSS configuration or ambient certificate store.
        data = Path(path).read_bytes()
        image.runtime("/runtime/libs"+path, path, data, sorted(roles))
    release = image.release()
    image.entry("native-release.json", canonical(release))
    initrd = output / "native.cpio.gz"
    archive.archive(image.entries, initrd)
    initrd.chmod(0o444)
    (output / "vmlinuz").write_bytes(kernel)
    (output / "vmlinuz").chmod(0o444)
    if common.source_manifest(module) != source:
        raise RuntimeError("source changed during image build")
    manifest = {"version": 1, "purpose": "native-review", **source,
                "kernel_sha256": args.kernel_sha256, "kernel_config_sha256": args.kernel_config_sha256,
                "initrd_sha256": common.digest(initrd), "initrd_bytes": initrd.stat().st_size,
                "programs": programs, "release": release, "state": "built-only",
                "runtime_qualified": False, "provider_eligibility_qualified": False}
    with (output / "manifest.json").open("x") as stream:
        json.dump(manifest, stream, indent=2)
        stream.write("\n")
    print(json.dumps({"manifest": str(output / "manifest.json"), "state": "built-only"}))


if __name__ == "__main__":
    main()
