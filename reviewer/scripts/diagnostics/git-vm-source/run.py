"""Inert VM prerequisite assessment, not production acquisition qualification."""
import argparse
from contextlib import contextmanager
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import stat
import struct
import sys
import subprocess
import time


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


@contextmanager
def regular_input(path, limit):
    descriptor = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NONBLOCK | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_size <= limit, "bounded regular input required")
        yield stream


def read_bounded(path, limit):
    with regular_input(path, limit) as stream:
        raw = stream.read(limit+1)
    require(len(raw) <= limit, "input grew beyond bound")
    return raw


def hash_file(path, limit=64 << 20):
    checksum, total = hashlib.sha256(), 0
    with regular_input(path, limit) as stream:
        while True:
            chunk = stream.read(min(1 << 20, limit+1-total))
            if not chunk:
                return checksum.hexdigest()
            total += len(chunk)
            require(total <= limit, "hashed input grew beyond bound")
            checksum.update(chunk)


def decode_guest_result(serial, nonce):
    require(isinstance(serial, bytes) and len(serial) <= 4 << 20, "bounded serial bytes required")
    require(isinstance(nonce, str) and re.fullmatch(r"[0-9a-f]{32}", nonce), "fresh nonce required")
    rows = [line for line in serial.splitlines() if line.startswith(b"SFL_GIT_VM_PREREQ_")]
    marker = ("SFL_GIT_VM_PREREQ_"+nonce+" ").encode()
    require(len(rows) == 1 and rows[0].startswith(marker), "one current-invocation guest record required")
    raw = rows[0][len(marker):]
    require(len(raw) <= 8192, "bounded guest record required")

    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "duplicate guest field")
            result[key] = value
        return result

    value = json.loads(raw, object_pairs_hook=unique)
    integers = ["version", "mount_errno", "filesystem_type", "size_bytes", "inode_limit", "mount_flags", "work_mode", "work_uid", "work_gid"]
    booleans = ["mount_succeeded", "noswap_reported", "sealed_proc_exec", "sealed_execveat", "work_writable", "fake_only", "production_qualified"]
    require(isinstance(value, dict) and set(value) == set(integers+booleans+["nonce", "kernel"]), "exact guest schema required")
    require(all(type(value[key]) is int and value[key] >= 0 for key in integers), "integer guest observations required")
    require(all(type(value[key]) is bool for key in booleans), "boolean guest observations required")
    require(value["version"] == 2 and value["nonce"] == nonce, "guest provenance differs")
    require(isinstance(value["kernel"], str) and re.fullmatch(r"[a-z0-9.+-]{1,128}", value["kernel"]), "bounded observed kernel required")
    require(value["fake_only"] and not value["production_qualified"], "inert purpose required")
    return value


def prerequisites_passed(guest):
    return (guest["mount_succeeded"] and guest["mount_errno"] == 0 and guest["noswap_reported"]
            and guest["size_bytes"] == 268435456 and guest["inode_limit"] == 65536
            and guest["filesystem_type"] == 0x1021994 and guest["mount_flags"] & 14 == 14
            and not guest["mount_flags"] & 1 and guest["work_mode"] == 0o700
            and guest["work_uid"] == 1203 and guest["work_gid"] == 1203 and guest["work_writable"]
            and guest["sealed_proc_exec"] and guest["sealed_execveat"])



def unique_object(pairs):
    value = {}
    for key, item in pairs:
        require(key not in value, "duplicate metadata field")
        value[key] = item
    return value


def pin_bytes(raw, pin):
    require(set(pin) == {"SHA256", "Bytes"} and type(pin["Bytes"]) is int
            and 0 < pin["Bytes"] <= 64 << 20
            and isinstance(pin["SHA256"], str) and re.fullmatch(r"[0-9a-f]{64}", pin["SHA256"]), "bounded asset pin required")
    require(len(raw) == pin["Bytes"] and hashlib.sha256(raw).hexdigest() == pin["SHA256"], "asset differs from measured pin")
    return raw


def runtime_path(path):
    if path in ["lib64/ld-linux-x86-64.so.2", "firmware/kvmvapic.bin", "firmware/linuxboot.bin", "firmware/linuxboot_dma.bin"]:
        return True
    return isinstance(path, str) and re.fullmatch(r"(?:usr/)?lib/x86_64-linux-gnu/lib[A-Za-z0-9_+.-]+\.so(?:\.[A-Za-z0-9_-]+)*", path) is not None


def decode_runtime(raw, program):
    magic = b"SFL-QEMU-RUNTIME-1\n"
    require(type(raw) is bytes and len(raw) <= (128 << 20)+(32 << 10)+len(magic)+4
            and raw.startswith(magic) and len(raw) >= len(magic)+4, "bounded captured runtime required")
    size = struct.unpack(">I", raw[len(magic):len(magic)+4])[0]
    require(0 < size <= 32 << 10, "bounded runtime header required")
    at = len(magic)+4+size
    header = raw[len(magic)+4:at]
    manifest = json.loads(header, object_pairs_hook=unique_object)
    require(set(manifest) == {"Version", "Purpose", "Program", "Files"} and type(manifest["Version"]) is int
            and manifest["Version"] == 1 and manifest["Purpose"] == "native-qemu-runtime"
            and manifest["Program"] == program, "measured runtime purpose/program differs")
    require(type(manifest["Files"]) is list and 5 <= len(manifest["Files"]) <= 128
            and type(manifest["Program"]) is dict and set(manifest["Program"]) == {"SHA256", "Bytes"}, "bounded runtime manifest required")
    canonical = {"Version": manifest["Version"], "Purpose": manifest["Purpose"],
                 "Program": {key: manifest["Program"][key] for key in ["SHA256", "Bytes"]}, "Files": []}
    for item in manifest["Files"]:
        require(type(item) is dict and set(item) == {"Path", "SHA256", "Bytes"}, "exact runtime entry required")
        canonical["Files"].append({key: item[key] for key in ["Path", "SHA256", "Bytes"]})
    require(json.dumps(canonical, separators=(",", ":")).encode() == header, "canonical runtime header required")
    files = manifest["Files"]
    require(type(files) is list and 5 <= len(files) <= 128, "bounded runtime entries required")
    bodies, previous = {}, ""
    for entry in files:
        require(type(entry) is dict and set(entry) == {"Path", "SHA256", "Bytes"}, "exact runtime entry required")
        path = entry["Path"]
        require(runtime_path(path) and path > previous, "unique sorted allowed runtime paths required")
        pin = {key: entry[key] for key in ["SHA256", "Bytes"]}
        require(type(pin["Bytes"]) is int and 0 < pin["Bytes"] <= 64 << 20, "bounded runtime body required")
        bodies[path] = pin_bytes(raw[at:at+pin["Bytes"]], pin)
        at += pin["Bytes"]
        previous = path
    require(at == len(raw) and sum(map(len, bodies.values())) <= 128 << 20, "runtime framing or budget differs")
    require({"lib64/ld-linux-x86-64.so.2", "firmware/kvmvapic.bin", "firmware/linuxboot.bin", "firmware/linuxboot_dma.bin"}.issubset(bodies)
            and any(path in bodies for path in ["lib/x86_64-linux-gnu/libc.so.6", "usr/lib/x86_64-linux-gnu/libc.so.6"]), "complete captured runtime required")
    return bodies


def snapshot_file(path, raw, mode):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with path.open("xb") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    path.chmod(mode)
    return {"SHA256": hashlib.sha256(raw).hexdigest(), "Bytes": len(raw)}


def prepare_runtime(root, bodies):
    root.mkdir(mode=0o700)
    pins = {}
    for name, raw in sorted(bodies.items()):
        require(name in ["qemu", "kernel", "initrd", "firmware/bios.bin"] or runtime_path(name), "fixed runtime destination required")
        pins[name] = snapshot_file(root/name, raw, 0o555 if name in ["qemu", "lib64/ld-linux-x86-64.so.2"] else 0o444)
    for name in ["proc", "dev", "tmp"]:
        (root/name).mkdir(mode=0o700)
    for directory, _, _ in os.walk(root, topdown=False):
        Path(directory).chmod(0o555)
    info = root.lstat()
    return {"device": info.st_dev, "inode": info.st_ino, "pins": pins}


def verify_runtime(root, identity):
    info = root.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_dev == identity["device"] and info.st_ino == identity["inode"], "original runtime directory required")
    expected = set(identity["pins"])
    directories = {".", "proc", "dev", "tmp"}
    for name in expected:
        require(name in ["qemu", "kernel", "initrd", "firmware/bios.bin"] or runtime_path(name), "fixed runtime path required")
        directories.update(str(parent) for parent in Path(name).parents)
    found = set()
    for directory, children, files in os.walk(root, followlinks=False):
        path = Path(directory)
        info = path.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o555
                and str(path.relative_to(root)) in directories, "private readonly runtime directory required")
        for name in children:
            require(not (path/name).is_symlink(), "runtime symlink refused")
        for name in files:
            item = path/name
            relative = str(item.relative_to(root))
            require(relative in expected, "unexpected runtime file")
            info = item.lstat()
            mode = 0o555 if relative in ["qemu", "lib64/ld-linux-x86-64.so.2"] else 0o444
            require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1
                    and stat.S_IMODE(info.st_mode) == mode, "private readonly runtime file required")
            pin_bytes(read_bounded(item, 64 << 20), identity["pins"][relative])
            found.add(relative)
    require(found == expected, "runtime file missing")


def cleanup_action(current, observed, description):
    # Description alone never grants authority to stop a live unit.
    if current.get("MainPID") == "0" and current.get("ActiveState") in ["inactive", "failed"]:
        return "already-stopped"
    require(observed is not None and re.fullmatch(r"[0-9a-f]{32}", observed.get("InvocationID", "")), "no captured live invocation; cleanup unknown")
    require(current.get("Description") == description and current.get("InvocationID") == observed["InvocationID"], "replacement unit refused")
    return "stop-original"



def verify_bootstrap(asset):
    path = Path(asset["Path"])
    require(path.is_absolute() and runtime_path(str(path).lstrip("/")) and not str(path).startswith("/firmware/"), "named host bootstrap dependency required")
    # Package aliases are expected here. Their resolved root-owned bytes remain
    # trusted host prerequisites, unlike the private captured QEMU payload.
    resolved = path.resolve(strict=True)
    with regular_input(resolved, 64 << 20) as stream:
        info = os.fstat(stream.fileno())
        require(info.st_uid == 0 and info.st_nlink == 1 and not info.st_mode & 0o022, "root-owned bootstrap dependency required")
        pin_bytes(stream.read((64 << 20)+1), {key: asset[key] for key in ["SHA256", "Bytes"]})


def owned_unit(config_path):
    # This entry has no arbitrary command or provider inputs. Refuse ordinary
    # host invocation before touching a runtime, helper or KVM descriptor.
    invocation = os.environ.get("INVOCATION_ID", "")
    require(re.fullmatch(r"[0-9a-f]{32}", invocation), "owned systemd invocation required")
    config_path = Path(config_path)
    require(config_path.is_absolute() and config_path.name == "unit.json", "fixed private unit configuration required")
    parent_info = config_path.parent.lstat()
    require(stat.S_ISDIR(parent_info.st_mode) and parent_info.st_uid == os.getuid()
            and stat.S_IMODE(parent_info.st_mode) == 0o700, "private unit parent required")
    info = config_path.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1
            and stat.S_IMODE(info.st_mode) == 0o600, "private unit configuration required")
    config = json.loads(read_bounded(config_path, 65536), object_pairs_hook=unique_object)
    require(set(config) == {"nonce", "runtime", "runner_sha256", "helper", "bootstrap_dependencies"}, "exact inert unit schema required")
    nonce = config["nonce"]
    require(isinstance(nonce, str) and re.fullmatch(r"[0-9a-f]{32}", nonce), "unit nonce required")
    unit = "sfl-git-vm-probe-"+nonce+".service"
    with open("/proc/self/cgroup", "rb") as stream:
        groups = stream.read(8193)
    require(len(groups) <= 8192 and len(groups.splitlines()) == 1
            and groups.startswith(b"0::/user.slice/") and groups.rstrip().endswith(("/"+unit).encode()), "current inert unit cgroup required")
    require(hash_file(Path(__file__), 65536) == config["runner_sha256"], "owned runner changed")
    with (config_path.parent/"unit.used").open("xb") as marker:
        marker.write(invocation.encode())
    root = config_path.parent/"runtime"
    verify_runtime(root, config["runtime"])
    root_fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    root_info = os.fstat(root_fd)
    require(root_info.st_dev == config["runtime"]["device"] and root_info.st_ino == config["runtime"]["inode"], "runtime replaced")
    helper = config["helper"]
    require(helper["Path"] == "/usr/bin/bwrap", "original namespace helper required")
    helper_fd = os.open(helper["Path"], os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC)
    with os.fdopen(os.dup(helper_fd), "rb") as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and info.st_nlink == 1 and not info.st_mode & 0o022
                and info.st_size <= 64 << 20, "trusted namespace helper required")
        pin_bytes(stream.read((64 << 20)+1), {key: helper[key] for key in ["SHA256", "Bytes"]})
    for asset in config["bootstrap_dependencies"]:
        verify_bootstrap(asset)
    kvm_fd = os.open("/dev/kvm", os.O_RDWR)
    os.set_inheritable(kvm_fd, True)
    os.set_inheritable(root_fd, True)
    serial_fd = os.open(config_path.parent/"serial.log", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    os.dup2(serial_fd, 1)
    os.close(serial_fd)
    helper_args = ["bwrap", "--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session",
        "--clearenv", "--setenv", "LANG", "C.UTF-8", "--ro-bind-fd", str(root_fd), "/", "--proc", "/proc",
        "--dev", "/dev", "--size", "16777216", "--tmpfs", "/tmp", "--chdir", "/", "--argv0", "sfl-git-vm-probe", "--", "/qemu"]
    qemu_args = ["-no-user-config", "-nodefaults", "-machine", "q35,dump-guest-core=off,mem-merge=off",
        "-add-fd", "fd="+str(kvm_fd)+",set=1,opaque=sfl-kvm", "-accel", "kvm,device=/dev/fdset/1",
        "-cpu", "host", "-m", "512M", "-smp", "1", "-display", "none", "-monitor", "none", "-nic", "none", "-no-reboot",
        "-L", "/firmware", "-bios", "/firmware/bios.bin", "-kernel", "/kernel", "-initrd", "/initrd",
        "-append", "console=ttyS0 rdinit=/init panic=1 sfl-fixture="+nonce+" -- --guest-init", "-serial", "stdio",
        "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny"]
    os.execve(helper_fd, helper_args+qemu_args, {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})


def run(args):
    root = Path(args.output).absolute()
    root.mkdir(mode=0o700, parents=False, exist_ok=False)
    repo = Path(__file__).resolve().parents[4]
    source = Path(__file__).with_name("probe.c")
    require(not subprocess.check_output(["git", "-C", str(repo), "status", "--porcelain"]), "commit clean fixture source before qualification")
    head = subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True).strip()
    tree = subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD^{tree}"], text=True).strip()
    require(source.is_file() and not source.is_symlink() and source.stat().st_size <= 65536, "fixed regular guest source required")
    image = Path(args.image).absolute()
    manifest_bytes = read_bounded(image/"manifest.json", 65536)
    manifest = json.loads(manifest_bytes)
    require(manifest.get("purpose") == "native-review" and manifest.get("state") == "built-only", "explicit measured native image required")
    nonce = secrets.token_hex(16)
    unit = "sfl-git-vm-probe-"+nonce+".service"
    description = "SFL inert Git VM prerequisite "+nonce
    environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8",
                   "XDG_RUNTIME_DIR": "/run/user/"+str(os.getuid()),
                   "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/"+str(os.getuid())+"/bus"}


    def digest(path):
        return hash_file(path)


    def show():
        raw = subprocess.check_output(["systemctl", "--user", "show", unit,
            "--property=LoadState,Description,ActiveState,MainPID,InvocationID,ControlGroup,MemoryMax,MemorySwapMax,TasksMax,CPUQuotaPerSecUSec,RuntimeMaxUSec,KillMode,NoNewPrivileges,Restart"],
            env=environment, timeout=3, text=True)
        return dict(line.split("=", 1) for line in raw.splitlines())


    spec = importlib.util.spec_from_file_location("inert_vm_archive", repo/"reviewer/scripts/isolation-fixture.py")
    shared = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(shared)
    program = root/"probe"
    compiler = Path("/usr/bin/cc").resolve()
    compiler_stat = compiler.stat()
    require(stat.S_ISREG(compiler_stat.st_mode) and compiler_stat.st_uid == 0 and compiler_stat.st_mode & 0o022 == 0, "trusted compiler required")
    source_hash, compiler_hash = digest(source), digest(compiler)
    subprocess.run([str(compiler), "-static", "-O2", "-Wall", "-Wextra", "-Werror", "-o", str(program), str(source)], env=environment, check=True, timeout=30)
    require(digest(source) == source_hash and digest(compiler) == compiler_hash, "source or compiler changed during build")
    require(subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True).strip() == head
            and not subprocess.check_output(["git", "-C", str(repo), "status", "--porcelain"]), "source changed during build")
    program.chmod(0o500)
    refusal = subprocess.run([str(program)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=3)
    require(refusal.returncode == 2 and not refusal.stdout and not refusal.stderr, "inert VM prerequisite proof refused")
    initrd = root/"probe.cpio.gz"
    shared.archive({"init": (stat.S_IFREG | 0o500, read_bounded(program, 4 << 20), 0, 0),
                    "dev": (stat.S_IFDIR | 0o555, b"", 0, 0),
                    "dev/console": (stat.S_IFCHR | 0o600, b"", 5, 1)}, initrd)
    qemu = Path(manifest["qemu"]["Path"])
    require(qemu == Path("/usr/bin/qemu-system-x86_64"), "fixed installed QEMU required")
    qemu_stat = qemu.stat()
    require(stat.S_ISREG(qemu_stat.st_mode) and qemu_stat.st_uid == 0 and qemu_stat.st_mode & 0o022 == 0, "trusted QEMU required")
    kernel, firmware = image/"vmlinuz", image/"bios.bin"
    require(digest(qemu) == manifest["qemu"]["SHA256"], "inert VM prerequisite proof refused")
    require(digest(kernel) == manifest["kernel_sha256"], "inert VM prerequisite proof refused")
    require(digest(firmware) == manifest["bios_sha256"], "inert VM prerequisite proof refused")
    runtime_metadata = manifest["host_runtime"]
    artifact = runtime_metadata["artifact"]
    raw = read_bounded(Path(artifact["Path"]), (128 << 20)+(32 << 10)+64)
    require(len(raw) == artifact["Bytes"] and hashlib.sha256(raw).hexdigest() == artifact["SHA256"], "captured runtime pin differs")
    program_pin = {key: manifest["qemu"][key] for key in ["SHA256", "Bytes"]}
    bodies = decode_runtime(raw, program_pin)
    bodies.update({"qemu": pin_bytes(read_bounded(qemu, 64 << 20), program_pin),
        "kernel": pin_bytes(read_bounded(kernel, 64 << 20), {"SHA256": manifest["kernel_sha256"], "Bytes": kernel.stat().st_size}),
        "firmware/bios.bin": pin_bytes(read_bounded(firmware, 64 << 20), {"SHA256": manifest["bios_sha256"], "Bytes": firmware.stat().st_size}),
        "initrd": read_bounded(initrd, 4 << 20)})
    runtime_identity = prepare_runtime(root/"runtime", bodies)
    verify_runtime(root/"runtime", runtime_identity)
    runner_pin = snapshot_file(root/"runner.py", read_bounded(Path(__file__), 65536), 0o400)
    config = {"nonce": nonce, "runtime": runtime_identity, "runner_sha256": runner_pin["SHA256"],
              "helper": runtime_metadata["namespace_helper"], "bootstrap_dependencies": runtime_metadata["bootstrap_dependencies"]}
    snapshot_file(root/"unit.json", (json.dumps(config, sort_keys=True)+"\n").encode(), 0o600)
    serial = root/"serial.log"
    command = ["systemd-run", "--user", "--unit="+unit, "--description="+description,
        "--collect", "--wait", "--pipe", "--service-type=exec",
        "--property=MemoryMax=1G", "--property=MemorySwapMax=0", "--property=TasksMax=64",
        "--property=CPUQuota=100%", "--property=RuntimeMaxSec=60s", "--property=KillMode=control-group",
        "--property=TimeoutStopSec=5s", "--property=NoNewPrivileges=yes", "--property=Restart=no", "--",
        "/usr/bin/python3", "-I", "-S", str(root/"runner.py"), "--owned-unit", str(root/"unit.json")]
    receipt = {"version": 2, "purpose": "inert-git-vm-prerequisites", "nonce": nonce,
        "unit": unit, "description": description, "command": command,
        "runtime": runtime_identity, "runtime_artifact_sha256": artifact["SHA256"],
        "namespace_helper": runtime_metadata["namespace_helper"],
        "bootstrap_note": "Original helper and root-owned host interpreter/libraries remain trusted host prerequisites; VM payload uses private readonly captured bytes.",
        "host_source": head, "host_tree": tree,
        "compiler": {"path": str(compiler), "sha256": compiler_hash},
        "guest_probe_source_sha256": source_hash, "guest_program_sha256": digest(program),
        "initrd_sha256": digest(initrd), "kernel_sha256": digest(kernel), "qemu_sha256": digest(qemu),
        "firmware_sha256": digest(firmware), "ordinary_host_invocation_refused": True,
        "fake_only": True, "model_inference": False, "production_qualified": False,
        "candidate_prerequisites_passed": False, "unit_stopped": False}
    (root/"prepared.json").write_text(json.dumps(receipt, indent=2)+"\n")
    process = None
    observed = None
    failure = None
    started = time.monotonic()
    try:
        with (root/"unit.log").open("xb") as log:
            process = subprocess.Popen(command, env=environment, stdout=log, stderr=subprocess.STDOUT)
            while process.poll() is None:
                if time.monotonic()-started > 70:
                    raise RuntimeError("inert VM deadline elapsed")
                if serial.exists() and serial.stat().st_size > 4 << 20:
                    raise RuntimeError("guest output bound exceeded")
                if observed is None:
                    current = show()
                    if current.get("Description") == description and current.get("ActiveState") == "active" and int(current.get("MainPID", "0")) > 0:
                        require(current["InvocationID"], "inert VM prerequisite proof refused")
                        group = current["ControlGroup"]
                        require(group.startswith("/user.slice/") and group.endswith("/"+unit) and ".." not in Path(group).parts, "inert VM prerequisite proof refused")
                        directory = Path("/sys/fs/cgroup")/group.lstrip("/")
                        current["cgroup_inode"] = directory.stat().st_ino
                        current["kernel_limits"] = {k: (directory/k).read_text().strip() for k in ["memory.max", "memory.swap.max", "pids.max", "cpu.max"]}
                        observed = current
                        (root/"observed.json").write_text(json.dumps(observed, indent=2)+"\n")
                time.sleep(0.1)
        require(process.returncode == 0 and observed is not None, "inert VM prerequisite proof refused")
        require(observed["kernel_limits"]["memory.max"] == str(1 << 30), "inert VM prerequisite proof refused")
        require(observed["kernel_limits"]["memory.swap.max"] == "0" and observed["kernel_limits"]["pids.max"] == "64", "inert VM prerequisite proof refused")
        require(observed["KillMode"] == "control-group", "inert VM prerequisite proof refused")
        cpu = observed["kernel_limits"]["cpu.max"].split()
        require(len(cpu) == 2 and all(part.isdigit() for part in cpu) and int(cpu[0]) == int(cpu[1]) > 0, "one CPU quota required")
        require(observed["RuntimeMaxUSec"] == "1min" and observed["NoNewPrivileges"] == "yes"
                and observed["Restart"] == "no", "bounded nonprivileged host unit required")
        guest = decode_guest_result(read_bounded(serial, 4 << 20), nonce)
        receipt["guest"] = guest
        receipt["candidate_prerequisites_passed"] = prerequisites_passed(guest)
    except BaseException as error:
        failure = repr(error)
    finally:
        try:
            current = show()
            action = cleanup_action(current, observed, description)
            if action == "stop-original":
                subprocess.run(["systemctl", "--user", "stop", unit], env=environment, check=True, timeout=10)
            if process is not None:
                process.wait(timeout=5)
            stopped = show()
            require(stopped.get("MainPID") == "0" and stopped.get("ActiveState") in ["inactive", "failed"], "inert VM prerequisite proof refused")
            require(observed is not None, "original unit identity was never captured")
            directory = Path("/sys/fs/cgroup")/observed["ControlGroup"].lstrip("/")
            removed = not directory.exists()
            require(removed or directory.stat().st_ino == observed["cgroup_inode"] and not (directory/"cgroup.procs").read_text().strip()
                    and "populated 0" in (directory/"cgroup.events").read_text(), "inert VM prerequisite proof refused")
            verify_runtime(root/"runtime", runtime_identity)
            require(read_bounded(root/"unit.used", 32).decode() == observed["InvocationID"], "owned bootstrap invocation differs")
            for asset in runtime_metadata["bootstrap_dependencies"]:
                verify_bootstrap(asset)
            require(digest(Path(runtime_metadata["namespace_helper"]["Path"])) == runtime_metadata["namespace_helper"]["SHA256"], "host helper changed")
            receipt.update({"unit_stopped": True, "final_unit": stopped, "original_cgroup_removed": removed,
                            "original_cgroup_empty": True, "runtime_verified_after_stop": True})
        except BaseException as error:
            failure = (failure+"; " if failure else "")+"cleanup: "+repr(error)
        receipt.update({"elapsed_seconds": round(time.monotonic()-started, 3), "error": failure})
        (root/"result.json").write_text(json.dumps(receipt, indent=2)+"\n")
    print(json.dumps({k: receipt[k] for k in ["candidate_prerequisites_passed", "unit_stopped", "elapsed_seconds", "error"]}))
    return 0 if failure is None and receipt["candidate_prerequisites_passed"] and receipt["unit_stopped"] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-inert-only", required=True, action="store_true")
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    return run(parser.parse_args())


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--owned-unit":
        owned_unit(sys.argv[2])
    else:
        raise SystemExit(main())
