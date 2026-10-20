#!/usr/bin/env python3
"""Build/run a marked, credential-free Linux role fixture. Never a live launcher."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import time


def execute(args, *, cwd=None, data=None, timeout=120, env=None):
    process = subprocess.run(args, cwd=cwd, input=data, capture_output=True, timeout=timeout,
                             env=env or {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
    if process.returncode:
        raise RuntimeError(f"{Path(str(args[0])).name} failed ({process.returncode}): "
                           + process.stderr.decode(errors="replace")[:2000])
    return process.stdout


def digest(data):
    return hashlib.sha256(data).hexdigest()


def write_json(path, value):
    with path.open("x", encoding="utf-8") as output:
        json.dump(value, output, indent=2)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())


def archive(entries, target):
    """Deterministic newc image, explicit data/modes/owners, no host traversal."""
    with target.open("xb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", mtime=0, filename="") as output:
        def item(index, name, mode, data, major=0, minor=0):
            encoded = name.encode() + b"\0"
            numbers = [index, mode, 0, 0, 1, 0, len(data), 0, 0, major, minor, len(encoded), 0]
            header = b"070701" + "".join(f"{number:08x}" for number in numbers).encode()
            output.write(header + encoded)
            output.write(b"\0" * (-(len(header) + len(encoded)) % 4))
            output.write(data)
            output.write(b"\0" * (-len(data) % 4))
        for index, (name, (mode, data, major, minor)) in enumerate(sorted(entries.items()), 1):
            item(index, name, mode, data, major, minor)
        item(len(entries)+1, "TRAILER!!!", 0, b"")


def run_fixture(output, manifest, kernel, image):
    nonce = manifest["nonce"]
    unit = "sfl-isolation-fixture-" + nonce + ".service"
    description = "SFL credential-free fixture " + nonce
    boot = output / "boot.log"
    qemu = ["/usr/bin/qemu-system-x86_64", "-no-user-config", "-nodefaults",
            "-machine", "q35,dump-guest-core=off", "-accel", "tcg,thread=single", "-cpu", "max",
            "-m", "512M", "-smp", "1", "-display", "none", "-monitor", "none", "-nic", "none",
            "-no-reboot", "-kernel", str(kernel), "-initrd", str(image),
            "-append", "console=ttyS0 rdinit=/init panic=1 sfl-fixture="+nonce+" -- --guest-init",
            "-serial", "file:"+str(boot),
            "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny"]
    command = ["/usr/bin/systemd-run", "--user", "--unit="+unit, "--description="+description,
               "--collect", "--wait", "--pipe", "--service-type=exec",
               "--property=MemoryMax=1G", "--property=MemorySwapMax=0", "--property=TasksMax=64",
               "--property=CPUQuota=100%", "--property=RuntimeMaxSec=120s",
               "--property=KillMode=control-group", "--property=TimeoutStopSec=5s",
               "--property=NoNewPrivileges=yes", "--", "/usr/bin/env", "-i",
               "LANG=C.UTF-8", "PATH=/usr/bin:/bin", *qemu]
    # This is fake-only execution. It never selects or falls back from a
    # production accelerator, credentials, provider endpoint or App identity.
    prepared = {"state": "prepared", "nonce": nonce, "unit": unit, "description": description,
                "image_sha256": digest(image.read_bytes()), "kernel_sha256": digest(kernel.read_bytes()),
                "command": command, "fake_clients": True, "production_boundary_qualified": False}
    write_json(output / "launch-prepared.json", prepared)
    manager_env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8",
                   "XDG_RUNTIME_DIR": "/run/user/"+str(os.getuid()),
                   "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/"+str(os.getuid())+"/bus"}
    properties = {}
    process = None
    failure = None
    final = None
    try:
        with (output / "unit.log").open("xb") as log:
            process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, env=manager_env)
            deadline = time.monotonic()+130
            while process.poll() is None:
                if time.monotonic() >= deadline:
                    raise RuntimeError("bounded fixture deadline elapsed")
                if boot.exists() and boot.stat().st_size > 4*1024*1024:
                    raise RuntimeError("bounded guest boot output exceeded")
                if not properties:
                    observed = subprocess.run(["/usr/bin/systemctl", "--user", "show", unit,
                        "--property=Description,MainPID,InvocationID,ControlGroup,MemoryMax,MemorySwapMax,TasksMax,CPUQuotaPerSecUSec,RuntimeMaxUSec,KillMode,ActiveState"],
                        env=manager_env, capture_output=True, timeout=3)
                    fields = dict(line.split("=", 1) for line in observed.stdout.decode().splitlines() if "=" in line)
                    if fields.get("Description") == description and fields.get("ActiveState") == "active":
                        properties = fields
                        write_json(output / "launch-observed.json", fields)
                time.sleep(.2)
        if process.returncode != 0:
            raise RuntimeError("fake VM unit failed; inspect private unit/boot logs")
        if not properties or properties.get("MemoryMax") != str(1<<30) or properties.get("MemorySwapMax") != "0" or properties.get("TasksMax") != "64" or properties.get("KillMode") != "control-group":
            raise RuntimeError("actual host resource/ownership observations missing")
        raw = boot.read_bytes()
        marker = ("SFL_FIXTURE_RESULT_"+nonce+" ").encode()
        matches = [line[len(marker):] for line in raw.splitlines() if line.startswith(marker)]
        if len(matches) != 1:
            raise RuntimeError("missing or ambiguous current-invocation guest receipt")
        result = json.loads(matches[0])
        if result.get("nonce") != nonce or not result.get("passed") or result.get("production_boundary_qualified") is not False or result.get("fake_clients") is not True:
            raise RuntimeError("guest role qualification failed: "+str(result.get("error")))
        access = result.get("source_access", {})
        if access.get("tool_calls") != 2 or access.get("reader_calls") != 3 or len(access.get("reads", [])) != 3 or len(result.get("roles", [])) != 2:
            raise RuntimeError("actual source receipt or role count differs")
        for role, uid in zip(result["roles"], [1201, 1202]):
            if role.get("uid") != uid or role.get("stopped") is not True or role.get("memory_max") != "100663296" or role.get("pids_max") != "32" or role.get("cpu_max") != "100000 100000":
                raise RuntimeError("guest kernel role/resource observation differs")
        if result["roles"][1].get("descendants_killed", 0) < 1 or result.get("reaped_orphans", 0) < 1:
            raise RuntimeError("escaped child containment/reaping not established")
        final = {"state": "passed", "nonce": nonce, "manifest_sha256": digest((output / "manifest.json").read_bytes()),
                 "guest": result, "host_unit": properties, "host_exit": process.returncode,
                 "scope": "Trusted fake guest identity/filesystem/source/cgroup fixture only; TCG is not a production security boundary."}
    except BaseException as error:
        failure = error
    cleanup_error = None
    try:
        # Ownership is the fresh unit nonce and fixed description. Never stop a
        # unit whose live description differs, even if a stale name was reused.
        observed = subprocess.run(["/usr/bin/systemctl", "--user", "show", unit, "--property=Description,InvocationID,ActiveState,LoadState"],
                                  env=manager_env, capture_output=True, timeout=3)
        state = dict(line.split("=", 1) for line in observed.stdout.decode().splitlines() if "=" in line)
        if state.get("Description") == description:
            if properties and state.get("InvocationID") and state["InvocationID"] != properties["InvocationID"]:
                raise RuntimeError("unit invocation changed; cleanup ownership uncertain")
            stopped = subprocess.run(["/usr/bin/systemctl", "--user", "stop", unit], env=manager_env, capture_output=True, timeout=10)
            if stopped.returncode != 0:
                raise RuntimeError("owned fixture unit could not be stopped")
        elif state.get("LoadState") != "not-found" and state.get("ActiveState") != "inactive":
            raise RuntimeError("fixture unit ownership changed; no unrelated unit stopped")
        if process is not None and process.poll() is None:
            process.terminate()
            process.wait(timeout=5)
        observed = subprocess.run(["/usr/bin/systemctl", "--user", "show", unit, "--property=ActiveState,LoadState"],
                                  env=manager_env, capture_output=True, timeout=3)
        state = dict(line.split("=", 1) for line in observed.stdout.decode().splitlines() if "=" in line)
        if state.get("LoadState") != "not-found" and state.get("ActiveState") != "inactive":
            raise RuntimeError("fixture unit termination not established")
    except Exception as error:
        cleanup_error = error
    if failure is not None:
        if cleanup_error is not None:
            failure.add_note("Fixture cleanup also failed: "+str(cleanup_error))
        raise failure
    if cleanup_error is not None:
        raise cleanup_error
    final["host_unit_stopped"] = True
    write_json(output / "result.json", final)
    print(json.dumps({"result": str(output / "result.json"), "state": "passed"}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kernel", type=Path, required=True)
    parser.add_argument("--kernel-sha256", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go", required=True)
    parser.add_argument("--run", action="store_true", help="Run trusted fake guest only")
    args = parser.parse_args()
    module = Path(__file__).resolve().parent.parent
    if not re.fullmatch("[0-9a-f]{64}", args.kernel_sha256) or args.kernel.is_symlink() or digest(args.kernel.read_bytes()) != args.kernel_sha256:
        raise RuntimeError("kernel must match the supplied immutable hash")
    if execute(["git", "status", "--porcelain"], cwd=module):
        raise RuntimeError("commit fixture source before building qualification evidence")
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    nonce = secrets.token_hex(16)
    cache_env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8",
                 "GOPATH": os.environ.get("GOPATH", str(Path.home() / "go"))}
    build_env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "CGO_ENABLED": "0",
                 "GOCACHE": str(output / "go-cache"), "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "-mod=readonly",
                 "GOMODCACHE": execute([args.go, "env", "GOMODCACHE"], cwd=module, env=cache_env).decode().strip()}
    # The build environment contains no provider, GitHub or interactive auth.
    for name, package in [("init", "./cmd/isolationfixture"), ("sourceproxy", "./cmd/sourceproxy")]:
        execute([args.go, "build", "-trimpath", "-buildvcs=true", "-o", str(output / name), package], cwd=module, env=build_env)
    gitdir = output / "objects-fixture"
    git_env = {"PATH": "/usr/bin:/bin", "LANG": "C", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
               "GIT_AUTHOR_NAME": "SFL fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
               "GIT_COMMITTER_NAME": "SFL fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
               "GIT_AUTHOR_DATE": "2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00Z"}
    execute(["git", "init", "--bare", str(gitdir)], env=git_env)
    rows = []
    for name, data in [("source.go", b"original source\n"), ("worker.go", b"worker-only evidence\n")]:
        oid = execute(["git", "--git-dir="+str(gitdir), "hash-object", "-w", "--stdin"], data=data, env=git_env).decode().strip()
        rows.append(f"100644 blob {oid}\t{name}\n")
    tree = execute(["git", "--git-dir="+str(gitdir), "mktree"], data="".join(rows).encode(), env=git_env).decode().strip()
    commit = execute(["git", "--git-dir="+str(gitdir), "commit-tree", tree], data=b"inert fixture\n", env=git_env).decode().strip()
    fixture = {"version": 1, "nonce": nonce, "commit": commit}
    entries = {}
    def add(name, data=b"", mode=stat.S_IFREG|0o444, major=0, minor=0):
        parts = name.split("/")
        for end in range(1, len(parts)):
            entries.setdefault("/".join(parts[:end]), (stat.S_IFDIR|0o755, b"", 0, 0))
        entries[name] = (mode, data, major, minor)
    for name in ["proc", "sys", "run", "source", "dev"]:
        add(name, mode=stat.S_IFDIR|0o755)
    add("dev/console", mode=stat.S_IFCHR|0o600, major=5, minor=1)
    add("dev/null", mode=stat.S_IFCHR|0o666, major=1, minor=3)
    add("init", (output / "init").read_bytes(), stat.S_IFREG|0o555)
    add("artifacts/sourceproxy", (output / "sourceproxy").read_bytes(), stat.S_IFREG|0o555)
    add("fixture.json", json.dumps(fixture, separators=(",", ":")).encode())
    add("source/config", b"[core]\nrepositoryformatversion = 0\nbare = true\n")
    add("source/HEAD", (commit+"\n").encode())
    add("source/refs", mode=stat.S_IFDIR|0o755)
    for file in sorted((gitdir / "objects").rglob("*")):
        if file.is_file():
            add("source/objects/"+str(file.relative_to(gitdir / "objects")), file.read_bytes())
    libraries = execute(["/usr/bin/ldd", "/usr/bin/git"]).decode()
    runtime = {"/usr/bin/git"}
    runtime.update(re.findall(r"(?:=>\s+)?(/[^\s()]+)", libraries))
    runtime_manifest = []
    for name in sorted(runtime):
        if not name.startswith(("/usr/bin/git", "/lib/", "/lib64/", "/usr/lib/", "/usr/lib64/")):
            raise RuntimeError("unexpected Git runtime dependency")
        data = Path(name).read_bytes()
        add(name.lstrip("/"), data, stat.S_IFREG|0o555)
        runtime_manifest.append({"path": name, "sha256": digest(data), "bytes": len(data)})
    image = output / "fixture.cpio.gz"
    archive(entries, image)
    manifest = {"version": 1, "nonce": nonce, "source_head": execute(["git", "rev-parse", "HEAD"], cwd=module).decode().strip(),
                "kernel_path": str(args.kernel.absolute()), "kernel_sha256": args.kernel_sha256,
                "init_sha256": digest((output / "init").read_bytes()), "sourceproxy_sha256": digest((output / "sourceproxy").read_bytes()),
                "image_sha256": digest(image.read_bytes()), "synthetic_commit": commit, "runtime": runtime_manifest,
                "go_version": execute([args.go, "version"]).decode().strip(), "fake_clients": True,
                "production_boundary_qualified": False}
    write_json(output / "manifest.json", manifest)
    if args.run:
        run_fixture(output, manifest, args.kernel.absolute(), image)
    else:
        print(json.dumps({"manifest": str(output / "manifest.json"), "state": "built-only"}))


if __name__ == "__main__":
    main()
