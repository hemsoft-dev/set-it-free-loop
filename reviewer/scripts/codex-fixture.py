#!/usr/bin/env python3
"""Exercise pinned Codex against a synthetic backend inside private namespaces.

All cases are offline observations. A passing result never qualifies provider
eligibility, a complete capability allowlist, or production isolation.
"""
import argparse
import datetime
import importlib.util
import json
import os
import pathlib
import shutil
import signal
import subprocess
import tempfile
import time

CLIENT_SHA256 = "399ff69c431c066e40de318fba45c2b07f45a76b19939d247ee4828fc70c4215"
HOST_SHA256 = "8724d73ec190536233ab4628d24a071bfbcf27e5124f08fe35129f26fe48b916"

# Shared read-only executable hashing and runtime dependency discovery. Importing
# this sibling fixture starts no process, profile or client session.
_spec = importlib.util.spec_from_file_location("claude_fixture_helpers", pathlib.Path(__file__).with_name("claude-fixture.py"))
_common = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_common)


def metadata_observed(result):
    counts = result.get("metadata_routes", {})
    return (isinstance(counts, dict) and len(counts) == 3 and counts.get("/v1/models") in (1, 2)
            and counts.get("/backend-api/wham/accounts/check") == 2
            and counts.get("/backend-api/wham/settings/user") == 1
            and sum(counts.values()) == result.get("metadata_requests"))


def observation_passed(receipt, result, source, case):
    if case == "guarded-undeclared-patch":
        return (receipt["exit_code"] == 0 and not receipt["timed_out"] and not receipt["source_changed"]
                and result.get("provider_response_guarded") is True
                and result.get("forbidden_tool_pre_dispatch_denied") is True
                and bool(result.get("run_error")) and bool(result.get("normalize_error")) and bool(result.get("shape_error"))
                and bool(result.get("provider_access_error")) and result.get("provider_stop_error") == ""
                and result.get("backend_failures") == 0 and result.get("backend_stop_error") == ""
                and result.get("telemetry_requests") == 0 and result.get("negotiations") == 1
                and result.get("metadata_requests") in (4, 5) and metadata_observed(result)
                and result.get("backend_calls") == 1 and result.get("canary_exists") is False
                and result.get("undeclared_patch_output") == "" and not source)
    passed = (receipt["exit_code"] == 0 and not receipt["timed_out"] and not receipt["source_changed"] and result.get("run_error") == ""
              and result.get("normalize_error") == "" and result.get("shape_error") == ""
              and result.get("telemetry_requests") == 0 and result.get("metadata_requests") in (4, 5) and metadata_observed(result)
              and result.get("provider_access_error") == "" and result.get("provider_stop_error") == ""
              and result.get("backend_failures") == 0 and result.get("backend_stop_error") == "" and result.get("negotiations") == 1)
    if case == "source":
        passed = (passed and result.get("provider_response_guarded") is True and result.get("backend_calls") == 3
                  and result.get("runtime_inventory") == ["mcp__sfl_source__read", "mcp__sfl_source__search"]
                  and len(source) == 2 and [item.get("tool") for item in source] == ["read", "search"]
                  and all(item.get("success") is True for item in source)
                  and source[1].get("needle") == "immutable" and source[1].get("max_results") == 5)
    else:
        passed = (passed and result.get("backend_calls") == 2 and not source
                  and result.get("undeclared_patch_denied") is True and result.get("canary_exists") is False)
    return passed


def code_host_observation_passed(value):
    if not isinstance(value, dict):
        return False
    if (type(value.get("version")) is not int or value.get("version") != 1 or value.get("passed") is not True or value.get("probe_passed") is not True
            or value.get("fake_only") is not True or value.get("production_boundary_qualified") is not False):
        return False
    for name in ["input", "output"]:
        stream = value.get(name, {})
        if not isinstance(stream, dict):
            return False
        if (type(stream.get("bytes")) is not int or not 0 < stream["bytes"] <= 8 << 20
                or stream.get("synthetic_secret_seen") is not False or stream.get("capped") is not False
                or not isinstance(stream.get("sha256"), str) or len(stream["sha256"]) != 64
                or any(c not in "0123456789abcdef" for c in stream["sha256"])):
            return False
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client", type=pathlib.Path, required=True)
    parser.add_argument("--code-host", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--case", choices=["source", "undeclared-patch", "guarded-undeclared-patch"], required=True)
    parser.add_argument("--go", default="go")
    parser.add_argument("--isolate-code-host", action="store_true", help="Synthetic-only nested code-host namespace/stdio observation")
    parser.add_argument("--module-cache", type=pathlib.Path)
    args = parser.parse_args()
    if args.isolate_code_host and args.case != "source":
        parser.error("code-host transport qualification supports the source case only")
    client = args.client.resolve(strict=True)
    code_host = args.code_host.resolve(strict=True)
    if _common.digest(client) != CLIENT_SHA256 or _common.digest(code_host) != HOST_SHA256:
        parser.error("Codex 0.163.0 client/code-mode host hashes must match")
    bwrap = shutil.which("bwrap")
    if bwrap is None or not pathlib.Path("/proc/self/ns/net").exists():
        parser.error("Linux and Bubblewrap required")
    module = pathlib.Path(__file__).resolve().parent.parent
    manifest = _common.source_manifest(module)
    out = args.output.absolute()
    out.mkdir(mode=0o700)
    artifacts = out / "artifacts"
    artifacts.mkdir(mode=0o700)
    client = _common.snapshot_file(client, out / "client-snapshot", CLIENT_SHA256)
    code_host = _common.snapshot_file(code_host, out / "code-host-snapshot", HOST_SHA256)
    helper = out / "helper"
    _common.build_offline(module, args.go, helper, "./cmd/codexfixture", manifest, args.module_cache)
    if _common.source_manifest(module) != manifest:
        raise RuntimeError("source changed during helper build")
    relay = None
    host_mount = "/codex-code-mode-host"
    relay_mounts = []
    runtime = set(_common.runtime_files(pathlib.Path("/usr/bin/env")))
    isolation_manifest = None
    ipc = None
    broker_command = None
    if args.isolate_code_host:
        relay = out / "code-host-relay"
        _common.build_offline(module, args.go, relay, "./cmd/codehostfixture", manifest, args.module_cache)
        marker = out / "synthetic-codehost-marker"
        marker.write_text("sfl-codehost-synthetic-only-1\n")
        marker.chmod(0o400)
        ipc = tempfile.TemporaryDirectory(prefix="sfl-codehost-")
        broker_command = [str(relay), "--broker", str(pathlib.Path(ipc.name) / "stdio.sock"), str(code_host), str(relay), str(out / "codehost-broker.json")]
        relay_mounts = ["--ro-bind", str(relay), "/codex-code-mode-host", "--ro-bind", ipc.name, "/codehost", "--ro-bind", str(marker), "/run/sfl-codehost-fixture"]
        host_mount = None
        isolation_manifest = {"relay_sha256": _common.digest(relay), "bwrap_sha256": _common.digest(pathlib.Path(bwrap)),
                              "broker_mode": "sibling-namespace-stdio-1",
                              "runtime": [{"path": name, "sha256": _common.digest(pathlib.Path(name))} for name in sorted(runtime)],
                              "separate_guest_uid_qualified": False, "vm_worker_qualified": False}
    command = [bwrap, "--unshare-all", "--die-with-parent", "--new-session", "--clearenv",
               "--ro-bind", str(client), "/client",
               "--ro-bind", str(helper), "/helper", "--ro-bind", "/usr/bin/env", "/usr/bin/env",
               "--ro-bind", "/lib64/ld-linux-x86-64.so.2", "/lib64/ld-linux-x86-64.so.2",
               "--symlink", "usr/lib", "/lib"]
    if host_mount is not None:
        command += ["--ro-bind", str(code_host), host_mount]
    for library in sorted(runtime):
        command += ["--ro-bind", library, library]
    command += relay_mounts
    command += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
                "--dir", "/profile", "--dir", "/work", "--dir", "/run/sfl",
                "--bind", str(artifacts), "/artifacts", "--setenv", "LANG", "C.UTF-8",
                "--setenv", "GOMAXPROCS", "1", "--setenv", "SFL_FAKE_CLIENT_FIXTURE", "1",
                "--chdir", "/work", "--", "/helper", "--fixture", "--case", args.case]
    timed_out = False
    broker = None
    broker_stopped = True
    try:
        if broker_command is not None:
            with (out / "broker-stdout.txt").open("wb") as broker_out, (out / "broker-stderr.txt").open("wb") as broker_err:
                broker = subprocess.Popen(broker_command, stdout=broker_out, stderr=broker_err, start_new_session=True,
                                          env={"LANG": "C.UTF-8", "SFL_FAKE_CODEHOST_BROKER": "1"})
            deadline = time.monotonic() + 5
            while not (pathlib.Path(ipc.name) / "stdio.sock").exists():
                if broker.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError("code-host broker did not become ready")
                time.sleep(0.02)
        with (out / "host-stdout.txt").open("wb") as stdout, (out / "host-stderr.txt").open("wb") as stderr:
            process = subprocess.Popen(command, stdout=stdout, stderr=stderr, start_new_session=True,
                                       env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
            try:
                process.wait(timeout=50)
            except subprocess.TimeoutExpired:
                timed_out = True
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=5)
    finally:
        if broker is not None:
            try:
                broker.wait(timeout=3)
            except subprocess.TimeoutExpired:
                broker_stopped = False
                os.killpg(broker.pid, signal.SIGKILL)
                broker.wait(timeout=5)
        if ipc is not None:
            ipc.cleanup()
    receipt = {"at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "case": args.case,
               "client_version": "0.163.0", "client_sha256": CLIENT_SHA256, "code_host_sha256": HOST_SHA256,
               "helper_sha256": _common.digest(helper), "command": command,
               **manifest,
               "exit_code": process.returncode, "timed_out": timed_out,
               "code_host_isolation": isolation_manifest, "broker_stopped_cleanly": broker_stopped,
               "fake_only": True, "actual_account_qualified": False,
               "capability_allowlist_qualified": False, "production_boundary_qualified": False}
    try:
        receipt["source_changed"] = _common.source_manifest(module) != manifest
    except RuntimeError:
        receipt["source_changed"] = True
    (out / "host-receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    result = json.loads((artifacts / "result.json").read_text()) if (artifacts / "result.json").exists() else {}
    source = [json.loads(line) for line in (artifacts / "source.jsonl").read_text().splitlines()] if (artifacts / "source.jsonl").exists() else []
    passed = observation_passed(receipt, result, source, args.case)
    if args.isolate_code_host:
        broker_path = out / "codehost-broker.json"
        broker_result = json.loads(broker_path.read_text()) if broker_path.exists() else {}
        passed = (passed and broker_stopped and broker.returncode == 0
                  and code_host_observation_passed(broker_result))
    print(json.dumps({"observation_passed": passed, "case": args.case, "output": str(out),
                      "capability_allowlist_qualified": False, "production_boundary_qualified": False}))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
