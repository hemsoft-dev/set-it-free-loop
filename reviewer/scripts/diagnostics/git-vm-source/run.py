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
    integers = ["version", "mount_errno", "filesystem_type", "size_bytes", "inode_limit", "mount_flags"]
    booleans = ["mount_succeeded", "noswap_reported", "sealed_proc_exec", "sealed_execveat", "fake_only", "production_qualified"]
    require(isinstance(value, dict) and set(value) == set(integers+booleans+["nonce", "kernel"]), "exact guest schema required")
    require(all(type(value[key]) is int and value[key] >= 0 for key in integers), "integer guest observations required")
    require(all(type(value[key]) is bool for key in booleans), "boolean guest observations required")
    require(value["version"] == 1 and value["nonce"] == nonce, "guest provenance differs")
    require(isinstance(value["kernel"], str) and re.fullmatch(r"[a-z0-9.+-]{1,128}", value["kernel"]), "bounded observed kernel required")
    require(value["fake_only"] and not value["production_qualified"], "inert purpose required")
    return value


def prerequisites_passed(guest):
    return (guest["mount_succeeded"] and guest["mount_errno"] == 0 and guest["noswap_reported"]
            and guest["size_bytes"] == 268435456 and guest["inode_limit"] == 65536
            and guest["filesystem_type"] == 0x1021994 and guest["mount_flags"] & 14 == 14
            and guest["sealed_proc_exec"] and guest["sealed_execveat"])


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
    serial = root/"serial.log"
    command = ["systemd-run", "--user", "--unit="+unit, "--description="+description,
        "--collect", "--wait", "--pipe", "--service-type=exec",
        "--property=MemoryMax=1G", "--property=MemorySwapMax=0", "--property=TasksMax=64",
        "--property=CPUQuota=100%", "--property=RuntimeMaxSec=60s", "--property=KillMode=control-group",
        "--property=TimeoutStopSec=5s", "--property=NoNewPrivileges=yes", "--property=Restart=no", "--",
        "/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", str(qemu),
        "-no-user-config", "-nodefaults", "-machine", "q35,dump-guest-core=off,mem-merge=off",
        "-accel", "kvm", "-cpu", "host", "-m", "512M", "-smp", "1",
        "-display", "none", "-monitor", "none", "-nic", "none", "-no-reboot",
        "-bios", str(firmware), "-kernel", str(kernel), "-initrd", str(initrd),
        "-append", "console=ttyS0 rdinit=/init panic=1 sfl-fixture="+nonce+" -- --guest-init",
        "-serial", "file:"+str(serial),
        "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny"]
    receipt = {"version": 1, "purpose": "inert-git-vm-prerequisites", "nonce": nonce,
        "unit": unit, "description": description, "command": command,
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
        current = show()
        if current.get("Description") == description:
            require(observed is None or not current.get("InvocationID") or current["InvocationID"] == observed["InvocationID"], "inert VM prerequisite proof refused")
            subprocess.run(["systemctl", "--user", "stop", unit], env=environment, check=True, timeout=10)
        else:
            require(current.get("LoadState") == "not-found" or current.get("ActiveState") == "inactive", "inert VM prerequisite proof refused")
        if process is not None:
            process.wait(timeout=5)
        stopped = show()
        require(stopped.get("MainPID") == "0" and stopped.get("ActiveState") in ["inactive", "failed"], "inert VM prerequisite proof refused")
        if observed:
            directory = Path("/sys/fs/cgroup")/observed["ControlGroup"].lstrip("/")
            require(not directory.exists() or directory.stat().st_ino == observed["cgroup_inode"] and not (directory/"cgroup.procs").read_text().strip() and "populated 0" in (directory/"cgroup.events").read_text(), "inert VM prerequisite proof refused")
        receipt.update({"unit_stopped": True, "final_unit": stopped, "elapsed_seconds": round(time.monotonic()-started, 3), "error": failure})
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
    raise SystemExit(main())
