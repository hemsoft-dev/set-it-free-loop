#!/usr/bin/env python3
"""Original-instance cleanup for explicitly invoked, credential-free diagnostics.

This is not a production launcher or an auth-release proof. The trusted child
waits before exec so the parent can retain its original cgroup and PID handles.
No systemctl stop/kill operation is used, including on failed capture.
"""
import ctypes
import json
import os
from pathlib import Path
import re
import secrets
import select
import subprocess
import sys
import time


def unique(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise RuntimeError("duplicate fixture birth field")
        value[key] = item
    return value


def read_bounded(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK)
    try:
        data = os.read(fd, 4097)
        if len(data) > 4096:
            raise RuntimeError("fixture metadata exceeds bound")
        return data
    finally:
        os.close(fd)


def write_new(path, value):
    path = Path(path)
    raw = (json.dumps(value, sort_keys=True, separators=(",", ":"))+"\n").encode()
    temporary = path.parent/(".unit-record-"+secrets.token_hex(16))
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
    try:
        if os.write(fd, raw) != len(raw):
            raise RuntimeError("short fixture metadata write")
        os.fsync(fd)
    finally:
        os.close(fd)
    try:
        os.link(temporary, path, follow_symlinks=False)
    finally:
        temporary.unlink()
    parent = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        os.fsync(parent)
    finally:
        os.close(parent)


def boot():
    return read_bounded("/proc/sys/kernel/random/boot_id").decode().strip()


def namespaces():
    return [os.stat("/proc/self/ns/"+kind).st_ino for kind in ["mnt", "cgroup"]]


def process_start(pid):
    raw = read_bounded("/proc/"+str(pid)+"/stat").decode()
    return int(raw[raw.rindex(")")+2:].split()[19])


def process_group(pid):
    rows = read_bounded("/proc/"+str(pid)+"/cgroup").decode().splitlines()
    if len(rows) != 1 or not rows[0].startswith("0::/"):
        raise RuntimeError("unified fixture cgroup missing")
    return rows[0][3:]


def directory(parent, name):
    return os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW, dir_fd=parent)


def cgroup_root():
    fd = os.open("/sys/fs/cgroup", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW)
    try:
        buf = ctypes.create_string_buffer(256)
        if ctypes.CDLL(None, use_errno=True).fstatfs(fd, buf) != 0 or ctypes.c_long.from_buffer(buf).value != 0x63677270:
            raise RuntimeError("fixture hierarchy is not cgroup-v2")
        return fd
    except BaseException:
        os.close(fd)
        raise


class FixtureUnit:
    def __init__(self, unit, description, output, environment):
        if not re.fullmatch(r"sfl-[a-z0-9-]+\.service", unit) or not description:
            raise RuntimeError("invalid fixed fixture identity")
        self.unit, self.description = unit, description
        self.output, self.environment = Path(output), environment
        self.process = None
        self.group = self.hierarchy = self.pidfd = None
        self.captured = self.stopped = False

    def observe(self):
        result = subprocess.run(["/usr/bin/systemctl", "--user", "show", self.unit,
                                 "--property=Description,InvocationID,ControlGroup,MainPID,ActiveState,LoadState"],
                                env=self.environment, capture_output=True, timeout=3, check=True)
        if len(result.stdout) > 4096:
            raise RuntimeError("fixture unit metadata exceeds bound")
        return dict(line.split("=", 1) for line in result.stdout.decode().splitlines() if "=" in line)

    def start(self, command, **kwargs):
        # Diagnostic callers supply a fixed bounded unit and inert payload.
        boundary = command.index("--")
        wrapped = command[:boundary] + ["--description="+self.description,
                   "--property=ExitType=cgroup", "--property=RemainAfterExit=no"]
        wrapped += ["--", "/usr/bin/python3", "-I", "-S", str(Path(__file__).resolve()),
                    "--child", str(self.output), *command[boundary+1:]]
        self.process = subprocess.Popen(wrapped, env=self.environment, **kwargs)
        deadline = time.monotonic()+5
        while not (self.output/"unit-birth.json").exists():
            if self.process.poll() is not None or time.monotonic() >= deadline:
                raise RuntimeError("fixture birth capture missing; no name-based cleanup")
            time.sleep(.01)
        birth = json.loads(read_bounded(self.output/"unit-birth.json"), object_pairs_hook=unique)
        self.capture(birth)
        write_new(self.output/"unit-go.json", birth)
        return self.process

    def capture(self, birth):
        if (set(birth) != {"pid", "start", "invocation", "boot", "group"}
                or type(birth["pid"]) is not int or birth["pid"] <= 0
                or type(birth["start"]) is not int or birth["start"] <= 0
                or not isinstance(birth["invocation"], str)
                or not re.fullmatch("[0-9a-f]{32}", birth["invocation"])
                or birth["boot"] != boot() or not isinstance(birth["group"], str)):
            raise RuntimeError("invalid fixture birth identity")
        parts = birth["group"].split("/")
        if (len(parts) < 3 or parts[0] != "" or parts[1] != "user.slice"
                or parts[-1] != self.unit or any(p in ["", ".", ".."] for p in parts[1:])):
            raise RuntimeError("invalid original fixture cgroup")
        current = self.observe()
        if (current.get("Description") != self.description
                or current.get("InvocationID") != birth["invocation"]
                or current.get("ControlGroup") != birth["group"]
                or current.get("MainPID") != str(birth["pid"])):
            raise RuntimeError("fixture manager identity differs from birth")
        try:
            self.hierarchy = cgroup_root()
            self.hierarchy_identity = os.fstat(self.hierarchy)
            self.namespace_identity = namespaces()
            fd = os.dup(self.hierarchy)
            try:
                for part in parts[1:]:
                    child = directory(fd, part)
                    os.close(fd)
                    fd = child
                self.group = fd
            except BaseException:
                os.close(fd)
                raise
            self.group_identity = os.fstat(self.group)
            if self.group_identity.st_dev != self.hierarchy_identity.st_dev:
                raise RuntimeError("fixture cgroup crossed its captured hierarchy")
            self.pidfd = os.pidfd_open(birth["pid"], 0)
            after = self.observe()
            if (process_start(birth["pid"]) != birth["start"]
                    or process_group(birth["pid"]) != birth["group"]
                    or any(after.get(key) != current.get(key) for key in
                           ["Description", "InvocationID", "ControlGroup", "MainPID"])):
                raise RuntimeError("fixture changed during original handle capture")
            self.birth = birth
            self.captured = True
            write_new(self.output/"unit-captured.json", {**birth,
                      "device": self.group_identity.st_dev, "inode": self.group_identity.st_ino,
                      "namespaces": self.namespace_identity, "diagnostic_only": True})
        except BaseException:
            self.close()
            raise

    def control(self, name, flags):
        if name not in ["cgroup.kill", "cgroup.events"] or self.group is None:
            raise RuntimeError("original fixture descriptor missing")
        return os.open(name, flags | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=self.group)

    def empty(self):
        try:
            fd = self.control("cgroup.events", os.O_RDONLY)
        except FileNotFoundError:
            # kernfs retains st_nlink=2 even after cgroup removal. Require the
            # kernel's deleted mark on this retained directory, not its path
            # being absent or now naming a different group.
            return os.readlink("/proc/self/fd/"+str(self.group)).endswith(" (deleted)")
        try:
            raw = os.read(fd, 4097)
            return len(raw) <= 4096 and b"populated 0" in raw.splitlines()
        finally:
            os.close(fd)

    def root_dead(self):
        poller = select.poll()
        poller.register(self.pidfd, select.POLLIN)
        return any(event & select.POLLIN for _, event in poller.poll(0))

    def stop(self, timeout=8):
        if self.stopped:
            return
        if not self.captured:
            raise RuntimeError("original fixture was never captured; stop remains unknown")
        anchor = os.stat("/sys/fs/cgroup", follow_symlinks=False)
        original = os.fstat(self.group)
        if (boot() != self.birth["boot"] or namespaces() != self.namespace_identity
                or (anchor.st_dev, anchor.st_ino) != (self.hierarchy_identity.st_dev, self.hierarchy_identity.st_ino)
                or (original.st_dev, original.st_ino) != (self.group_identity.st_dev, self.group_identity.st_ino)):
            raise RuntimeError("original fixture hierarchy changed")
        if not self.empty():
            try:
                fd = self.control("cgroup.kill", os.O_WRONLY)
                try:
                    if os.write(fd, b"1") != 1:
                        raise RuntimeError("original fixture kill write failed")
                finally:
                    os.close(fd)
            except FileNotFoundError:
                if not self.empty():
                    raise
        deadline = time.monotonic()+timeout
        while not (self.empty() and self.root_dead()):
            if time.monotonic() >= deadline:
                raise RuntimeError("original fixture physical stop remains unknown")
            time.sleep(.01)
        self.stopped = True
        write_new(self.output/"unit-stopped.json", {**self.birth,
                  "device": original.st_dev, "inode": original.st_ino,
                  "original_empty": True, "original_root_dead": True,
                  "diagnostic_only": True})

    def close(self):
        self.captured = False
        for name in ["pidfd", "group", "hierarchy"]:
            fd = getattr(self, name)
            if fd is not None:
                os.close(fd)
                setattr(self, name, None)


def child(output, payload):
    root = Path(output)
    info = root.lstat()
    if not root.is_dir() or root.is_symlink() or info.st_uid != os.getuid() or info.st_mode & 0o777 != 0o700 or not payload:
        raise RuntimeError("owned private fixture root required")
    birth = {"pid": os.getpid(), "start": process_start(os.getpid()),
             "invocation": os.environ.get("INVOCATION_ID", ""),
             "boot": boot(), "group": process_group(os.getpid())}
    write_new(root/"unit-birth.json", birth)
    deadline = time.monotonic()+8
    while not (root/"unit-go.json").exists():
        if time.monotonic() >= deadline:
            raise RuntimeError("fixture capture acknowledgment missing")
        time.sleep(.01)
    if json.loads(read_bounded(root/"unit-go.json"), object_pairs_hook=unique) != birth:
        raise RuntimeError("fixture capture acknowledgment differs")
    os.execv(payload[0], payload)


if __name__ == "__main__":
    if len(sys.argv) < 4 or sys.argv[1] != "--child":
        raise SystemExit("only a captured inert diagnostic child is supported")
    child(sys.argv[2], sys.argv[3:])
