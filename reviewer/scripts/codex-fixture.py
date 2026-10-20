#!/usr/bin/env python3
"""Exercise pinned Codex against a synthetic backend inside private namespaces.

Both cases are offline observations. A passing result never qualifies provider
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

CLIENT_SHA256 = "399ff69c431c066e40de318fba45c2b07f45a76b19939d247ee4828fc70c4215"
HOST_SHA256 = "8724d73ec190536233ab4628d24a071bfbcf27e5124f08fe35129f26fe48b916"

# Shared read-only executable hashing and runtime dependency discovery. Importing
# this sibling fixture starts no process, profile or client session.
_spec = importlib.util.spec_from_file_location("claude_fixture_helpers", pathlib.Path(__file__).with_name("claude-fixture.py"))
_common = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_common)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client", type=pathlib.Path, required=True)
    parser.add_argument("--code-host", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--case", choices=["source", "undeclared-patch"], required=True)
    parser.add_argument("--go", default="go")
    parser.add_argument("--module-cache", type=pathlib.Path)
    args = parser.parse_args()
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
    command = [bwrap, "--unshare-all", "--die-with-parent", "--new-session", "--clearenv",
               "--ro-bind", str(client), "/client", "--ro-bind", str(code_host), "/codex-code-mode-host",
               "--ro-bind", str(helper), "/helper", "--ro-bind", "/usr/bin/env", "/usr/bin/env",
               "--ro-bind", "/lib64/ld-linux-x86-64.so.2", "/lib64/ld-linux-x86-64.so.2",
               "--symlink", "usr/lib", "/lib"]
    for library in sorted(_common.runtime_files(pathlib.Path("/usr/bin/env"))):
        command += ["--ro-bind", library, library]
    command += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
                "--dir", "/profile", "--dir", "/work", "--dir", "/run/sfl",
                "--bind", str(artifacts), "/artifacts", "--setenv", "LANG", "C.UTF-8",
                "--setenv", "GOMAXPROCS", "1", "--setenv", "SFL_FAKE_CLIENT_FIXTURE", "1",
                "--chdir", "/work", "--", "/helper", "--fixture", "--case", args.case]
    timed_out = False
    with (out / "host-stdout.txt").open("wb") as stdout, (out / "host-stderr.txt").open("wb") as stderr:
        process = subprocess.Popen(command, stdout=stdout, stderr=stderr, start_new_session=True,
                                   env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
        try:
            process.wait(timeout=50)
        except subprocess.TimeoutExpired:
            timed_out = True
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=5)
    receipt = {"at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "case": args.case,
               "client_version": "0.163.0", "client_sha256": CLIENT_SHA256, "code_host_sha256": HOST_SHA256,
               "helper_sha256": _common.digest(helper), "command": command,
               **manifest,
               "exit_code": process.returncode, "timed_out": timed_out,
               "fake_only": True, "actual_account_qualified": False,
               "capability_allowlist_qualified": False, "production_boundary_qualified": False}
    try:
        receipt["source_changed"] = _common.source_manifest(module) != manifest
    except RuntimeError:
        receipt["source_changed"] = True
    (out / "host-receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    result = json.loads((artifacts / "result.json").read_text()) if (artifacts / "result.json").exists() else {}
    source = [json.loads(line) for line in (artifacts / "source.jsonl").read_text().splitlines()] if (artifacts / "source.jsonl").exists() else []
    passed = (process.returncode == 0 and not timed_out and not receipt["source_changed"] and result.get("run_error") == ""
              and result.get("normalize_error") == "" and result.get("shape_error") == ""
              and result.get("backend_failures") == 0 and result.get("backend_stop_error") == "" and result.get("negotiations") == 1)
    if args.case == "source":
        passed = (passed and result.get("backend_calls") == 3
                  and result.get("runtime_inventory") == ["mcp__sfl_source__read", "mcp__sfl_source__search"]
                  and len(source) == 2 and [item.get("tool") for item in source] == ["read", "search"]
                  and all(item.get("success") is True for item in source))
    else:
        passed = (passed and result.get("backend_calls") == 2 and not source
                  and result.get("undeclared_patch_denied") is True and result.get("canary_exists") is False)
    print(json.dumps({"observation_passed": passed, "case": args.case, "output": str(out),
                      "capability_allowlist_qualified": False, "production_boundary_qualified": False}))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
