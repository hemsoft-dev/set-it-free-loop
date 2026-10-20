#!/usr/bin/env python3
"""Run the pinned unmodified Claude client against a synthetic offline backend.

This script never mounts a real profile or provides a route outside its private
network namespace. It qualifies protocol framing only. It is not a live launcher.
"""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import re
import shutil
import signal
import subprocess
import tempfile
import tarfile
import io

CLIENT_SHA256 = "375988f457756524234695cf2d8f64ace99c5f68682e47ea24862a843c191450"


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            value.update(block)
    return value.hexdigest()


def runtime_files(binary):
    # Only call ldd after the exact official executable hash has matched.
    result = subprocess.run(["ldd", str(binary)], capture_output=True, text=True,
                            timeout=10, check=True)
    if "not found" in result.stdout:
        raise RuntimeError("missing client runtime dependency")
    return set(re.findall(r"=> (/[^ ]+)", result.stdout))


def source_manifest(module):
    root = module.parent
    dirty = bool(subprocess.check_output(["git", "-C", str(root), "status", "--porcelain", "--untracked-files=all"]))
    if dirty:
        raise RuntimeError("qualification requires clean committed source")
    return {"source_commit": subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip(),
            "source_tree": subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD^{tree}"], text=True).strip(),
            "source_dirty": False}


def snapshot_file(source, target, expected):
    value = hashlib.sha256()
    # Hash the same bytes we copy, through one opened source inode.
    with source.open("rb") as incoming, target.open("xb") as output:
        for block in iter(lambda: incoming.read(1 << 20), b""):
            value.update(block)
            output.write(block)
    if value.hexdigest() != expected:
        target.unlink()
        raise RuntimeError("executable snapshot digest differs")
    target.chmod(0o500)
    return target


def offline_environment(cache, module_cache=None):
    # No ambient GOFLAGS, GOWORK, GOCACHEPROG, proxy/auth or toolchain override.
    environment = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8",
                   "HOME": str(pathlib.Path.home()), "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64",
                   "GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off",
                   "GOSUMDB": "off", "GOVCS": "*:off", "GONOPROXY": "none", "GOPRIVATE": "",
                   "GOTELEMETRY": "off", "GOCACHE": str(cache)}
    if module_cache is not None:
        environment["GOMODCACHE"] = str(module_cache)
    return environment


def build_offline(module, go, target, package, manifest, module_cache=None):
    # Compile an immutable committed archive, not mutable checkout bytes.
    archive = subprocess.check_output(["git", "-C", str(module.parent), "archive", "--format=tar",
                                       manifest["source_commit"], "reviewer"])
    if len(archive) > 16 << 20:
        raise RuntimeError("fixture source archive exceeds bound")
    with tempfile.TemporaryDirectory(prefix="sfl-native-build-") as directory:
        root = pathlib.Path(directory)
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:") as tar:
            tar.extractall(root, filter="data")
        subprocess.run([go, "-C", str(root / "reviewer"), "mod", "verify"],
                       env=offline_environment(root / "cache", module_cache), check=True, timeout=120)
        subprocess.run([go, "-C", str(root / "reviewer"), "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
                        "-o", str(target), package], env=offline_environment(root / "cache", module_cache),
                       check=True, timeout=120)
    return digest(target)


def observation_passed(receipt, result, source, case="source"):
    if case == "guarded-bash":
        return (receipt["exit_code"] == 0 and not receipt["timed_out"] and not receipt["source_changed"]
                and result.get("provider_response_guarded") is True and result.get("forbidden_tool_pre_dispatch_denied") is True
                and bool(result.get("run_error")) and bool(result.get("report_error")) and bool(result.get("provider_access_error"))
                and result.get("provider_stop_error") == "" and result.get("backend_stop_error") == ""
                and result.get("backend_calls") == 1 and result.get("backend_requests") == 1 and result.get("backend_failures") == 0
                and result.get("canary_exists") is False and not source)
    passed = (result.get("provider_response_guarded") is True and receipt["exit_code"] == 0 and not receipt["timed_out"] and not receipt["source_changed"] and result.get("run_error") == ""
              and result.get("backend_stop_error") == "" and result.get("provider_stop_error") == "" and result.get("provider_access_error") == ""
              and result.get("report_error") == "" and result.get("backend_calls") == 3 and result.get("backend_requests") == 3 and result.get("backend_failures") == 0
              and len(source) == 2 and [item.get("tool") for item in source] == ["read", "search"]
              and all(item.get("success") is True for item in source)
              and source[0].get("revision") == "a" * 40 and source[0].get("path") == "source.go"
              and source[1].get("revision") == "a" * 40 and source[1].get("needle") == "immutable" and source[1].get("max_results") == 5)
    return passed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True,
                        help="new private artifact directory; must not exist")
    parser.add_argument("--case", choices=["source", "guarded-bash"], default="source")
    parser.add_argument("--go", default="go")
    parser.add_argument("--module-cache", type=pathlib.Path, help="optional prefilled module cache; missing modules fail offline")
    args = parser.parse_args()
    if os.name != "posix" or not pathlib.Path("/proc/self/ns/net").exists():
        parser.error("Linux namespaces required")
    client = args.client.resolve(strict=True)
    if digest(client) != CLIENT_SHA256:
        parser.error("client SHA256 does not match pinned Claude 2.1.303")
    bwrap = shutil.which("bwrap")
    if bwrap is None:
        parser.error("Bubblewrap required")
    module = pathlib.Path(__file__).resolve().parent.parent
    manifest = source_manifest(module)
    out = args.output.absolute()
    out.mkdir(mode=0o700)  # Refuse reuse, symlinks and mixed-run artifacts.
    client = snapshot_file(client, out / "client-snapshot", CLIENT_SHA256)
    helper = out / "helper"
    artifacts = out / "artifacts"
    artifacts.mkdir(mode=0o700)
    helper_sha = build_offline(module, args.go, helper, "./cmd/claudefixture", manifest, args.module_cache)
    if source_manifest(module) != manifest:
        raise RuntimeError("source changed during helper build")
    command = [bwrap, "--unshare-all", "--die-with-parent", "--new-session",
               "--clearenv", "--ro-bind", str(client), "/client",
               "--ro-bind", str(helper), "/helper",
               "--ro-bind", "/usr/bin/env", "/usr/bin/env",
               "--ro-bind", "/lib64/ld-linux-x86-64.so.2", "/lib64/ld-linux-x86-64.so.2",
               "--symlink", "usr/lib", "/lib"]
    for library in sorted(runtime_files(client) | runtime_files(pathlib.Path("/usr/bin/env"))):
        command += ["--ro-bind", library, library]
    command += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
                "--dir", "/profile", "--dir", "/work", "--dir", "/run/sfl",
                "--bind", str(artifacts), "/artifacts", "--setenv", "LANG", "C.UTF-8",
                "--setenv", "GOMAXPROCS", "1", "--setenv", "SFL_FAKE_CLIENT_FIXTURE", "1",
                "--chdir", "/work", "--", "/helper", "--fixture"]
    if args.case == "guarded-bash":
        command += ["--case", "guarded-bash"]
    timed_out = False
    with (out / "host-stdout.txt").open("wb") as stdout, (out / "host-stderr.txt").open("wb") as stderr:
        process = subprocess.Popen(command, stdout=stdout, stderr=stderr,
                                   start_new_session=True,
                                   env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
        try:
            process.wait(timeout=50)
        except subprocess.TimeoutExpired:
            timed_out = True
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)
    receipt = {"at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
               "client_version": "2.1.303", "client_sha256": CLIENT_SHA256,
               "helper_sha256": helper_sha, **manifest, "command": command,
               "exit_code": process.returncode, "timed_out": timed_out,
               "fake_only": True, "actual_account_qualified": False,
               "production_boundary_qualified": False,
               "limits": "private PID/mount/net namespace; 35s phase/50s outer deadline; no memory/CPU qualification"}
    try:
        receipt["source_changed"] = source_manifest(module) != manifest
    except RuntimeError:
        receipt["source_changed"] = True
    (out / "host-receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    result = json.loads((artifacts / "result.json").read_text()) if (artifacts / "result.json").exists() else {}
    source = [json.loads(line) for line in (artifacts / "source.jsonl").read_text().splitlines()] if (artifacts / "source.jsonl").exists() else []
    passed = observation_passed(receipt, result, source, args.case)
    print(json.dumps({"protocol_passed": passed, "output": str(out),
                      "actual_account_qualified": False, "production_boundary_qualified": False}))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
