#!/usr/bin/env python3
"""Qualify one pinned, credential-free VM descriptor grant and durable stop."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import sys

_spec = importlib.util.spec_from_file_location("sfl_vm_boot", Path(__file__).with_name("vm-boot-fixture.py"))
_boot = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_boot)
_shared = _boot._shared


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise RuntimeError("duplicate grant receipt key")
        value[key] = item
    return value


def parse_grant_result(raw, expected):
    value = json.loads(raw, object_pairs_hook=unique_object)
    fields = {"Version", "SourceHead", "SourceTree", "Root", "Unit", "StubHash", "BindingHash", "ManifestHash",
              "Started", "BeforeGrantInert", "Granted", "Stopped", "Recovered", "Passed", "FakeOnly",
              "ProductionBoundaryQualified", "Execution", "Journal", "Error"}
    if not isinstance(value, dict) or set(value) != fields or type(value["Version"]) is not int or value["Version"] != 1:
        raise RuntimeError("unexpected grant receipt shape")
    for key in ["Started", "BeforeGrantInert", "Granted", "Stopped", "Recovered", "Passed", "FakeOnly"]:
        if value[key] is not True:
            raise RuntimeError("grant execution or cleanup did not pass")
    if value["ProductionBoundaryQualified"] is not False or value["Error"] != "":
        raise RuntimeError("grant fixture is not a native qualification")
    if not isinstance(value["Root"], str) or not value["Root"]:
        raise RuntimeError("missing private grant root")
    for key in ["SourceHead", "SourceTree", "StubHash", "BindingHash", "ManifestHash", "Unit"]:
        if value[key] != expected[key]:
            raise RuntimeError("grant receipt differs from captured source/binding")
    execution = value["Execution"]
    keys = {"Version", "BindingHash", "ManifestHash", "VerifiedExecutable", "QEMUExited", "FakeOnly", "ProductionBoundaryQualified", "Error"}
    if not isinstance(execution, dict) or set(execution) != keys or type(execution["Version"]) is not int or execution["Version"] != 1:
        raise RuntimeError("unexpected execution receipt shape")
    if (execution["BindingHash"] != expected["BindingHash"] or execution["ManifestHash"] != value["ManifestHash"]
            or any(execution[key] is not True for key in ["VerifiedExecutable", "QEMUExited", "FakeOnly"])
            or execution["ProductionBoundaryQualified"] is not False or execution["Error"] != ""):
        raise RuntimeError("unqualified descriptor execution")
    journal = value["Journal"]
    if (not isinstance(journal, dict) or journal.get("Binding") != expected["Binding"]
            or journal.get("Unit") != expected["Unit"] or journal.get("StubHash") != expected["StubHash"]
            or journal.get("ManifestHash") != value["ManifestHash"] or journal.get("PayloadKind") != "vm-fixture"
            or any(journal.get(key) is not True for key in ["Started", "StartSettled", "Captured", "Aborted", "Stopped"])):
        raise RuntimeError("missing durable VM stop/recovery evidence")
    return value


def terminal_unit(unit):
    cleanup = _shared.execute(["/usr/bin/systemctl", "--user", "show", unit,
                               "-p", "LoadState", "-p", "ActiveState", "-p", "MainPID", "-p", "ControlGroup"],
                              env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8",
                                   "XDG_RUNTIME_DIR": "/run/user/"+str(os.geteuid())})
    properties = dict(line.split("=", 1) for line in cleanup.decode().splitlines())
    if properties != {"LoadState": "not-found", "ActiveState": "inactive", "MainPID": "0", "ControlGroup": ""}:
        raise RuntimeError("unit not terminal/collected after physical stop")
    return cleanup, properties


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kernel", type=Path, required=True)
    parser.add_argument("--kernel-sha256", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go", required=True)
    args = parser.parse_args()
    module = Path(__file__).resolve().parent.parent
    if _shared.execute(["git", "status", "--porcelain"], cwd=module):
        raise RuntimeError("commit fixture source before qualification")
    output = args.output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    _shared.execute([sys.executable, "-B", str(Path(__file__).with_name("isolation-fixture.py")),
                     "--kernel", str(args.kernel.absolute()), "--kernel-sha256", args.kernel_sha256,
                     "--output", str(output / "guest"), "--go", args.go, "--accelerator", "kvm"],
                    cwd=module, timeout=180)
    manifest = json.loads((output / "guest" / "manifest.json").read_text())
    tree = _shared.execute(["git", "rev-parse", "HEAD^{tree}"], cwd=module).decode().strip()
    base_env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "GOENV": "off", "GOTOOLCHAIN": "local",
                "GOPATH": os.environ.get("GOPATH", str(Path.home() / "go"))}
    build_env = {**base_env, "CGO_ENABLED": "0", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "-mod=readonly",
                 "GOCACHE": str(output / "guest" / "go-cache"),
                 "GOMODCACHE": _shared.execute([args.go, "env", "GOMODCACHE"], cwd=module, env=base_env).decode().strip()}
    flags = ("-X=github.com/hemsoft-dev/set-it-free-loop/reviewer.vmFixtureBuildRevision="+manifest["source_head"]
             +" -X=github.com/hemsoft-dev/set-it-free-loop/reviewer.vmFixtureBuildTree="+tree)
    for name in ["vmstub", "vmgrantfixture"]:
        _shared.execute([args.go, "build", "-trimpath", "-buildvcs=true", "-ldflags", flags,
                         "-o", str(output / name), "./cmd/"+name], cwd=module, env=build_env, timeout=180)
        (output / name).chmod(0o500)
    if (_shared.execute(["git", "rev-parse", "HEAD"], cwd=module).decode().strip() != manifest["source_head"]
            or _shared.execute(["git", "status", "--porcelain"], cwd=module)):
        raise RuntimeError("source changed during grant fixture build")
    nonce = manifest["nonce"]
    binding = {"session_hash": _shared.digest(("synthetic-vm-grant:"+manifest["source_head"]).encode()),
               "launch_id": nonce, "containment_id": _shared.digest(("synthetic-grant-unit:"+nonce).encode())}
    def asset(path, digest):
        return {"Path": str(path), "SHA256": digest, "Bytes": path.stat().st_size}
    boot = {"Version": 1, "Binding": binding,
            "QEMU": asset(Path("/usr/bin/qemu-system-x86_64"), manifest["qemu_sha256"]),
            "Kernel": asset(output / "guest" / "vmlinuz", manifest["kernel_sha256"]),
            "Initrd": asset(output / "guest" / "fixture.cpio.gz", manifest["image_sha256"])}
    configuration = output / "fixture-config.json"
    _shared.write_json(configuration, {"Version": 1, "Purpose": "synthetic-role-fixture", "SourceHead": manifest["source_head"], "SourceTree": tree, "Boot": boot})
    configuration.chmod(0o600)
    stub_hash = _shared.digest((output / "vmstub").read_bytes())
    expected = {"SourceHead": manifest["source_head"], "SourceTree": tree, "StubHash": stub_hash,
                "Binding": binding, "BindingHash": _boot.contract_hash(binding), "Unit": "sfl-alpha-"+nonce+".service"}
    expected["ManifestHash"] = _boot.contract_hash({"Version": 1, "Purpose": "synthetic-role-fixture", "Binding": binding,
                                                   "Assets": [{"SHA256": boot[key]["SHA256"], "Bytes": boot[key]["Bytes"]}
                                                              for key in ["QEMU", "Kernel", "Initrd"]]})
    _shared.write_json(output / "manifest.json", {**manifest, "grant_expected": expected, "boot": boot,
                                                 "helper_sha256": _shared.digest((output / "vmgrantfixture").read_bytes())})
    run = subprocess.run([str(output / "vmgrantfixture"), str(configuration), str(output / "vmstub"), stub_hash],
                         cwd=module, env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"},
                         capture_output=True, timeout=140, check=False)
    (output / "helper.stdout").write_bytes(run.stdout)
    (output / "helper.stderr").write_bytes(run.stderr)
    if run.returncode != 0:
        raise RuntimeError("VM grant helper failed; private output retained")
    result = parse_grant_result(run.stdout, expected)
    root = Path(result["Root"])
    info = root.lstat()
    if (not root.is_absolute() or root.parent != Path("/tmp") or not root.name.startswith("sfl-vm-unit-")
            or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o700):
        raise RuntimeError("unexpected private grant evidence root")
    serial_path = root / "g" / nonce / "vm-serial.log"
    fd = os.open(serial_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 4 << 20:
            raise RuntimeError("unexpected private VM serial artifact")
        serial = stream.read((4 << 20)+1)
    (output / "serial.log").write_bytes(serial)
    marker = ("SFL_FIXTURE_RESULT_"+nonce+" ").encode()
    matches = [line.split(marker, 1)[1] for line in serial.splitlines() if marker in line]
    if len(matches) != 1:
        raise RuntimeError("missing or ambiguous synthetic guest result")
    guest = _shared.parse_guest_result(matches[0], nonce, manifest["synthetic_commit"])
    cleanup, properties = terminal_unit(expected["Unit"])
    (output / "final-unit.txt").write_bytes(cleanup)
    _shared.write_json(output / "result.json", {"state": "passed", "grant": result, "guest": guest,
                                               "final_unit": properties, "fake_only": True,
                                               "production_boundary_qualified": False})
    print(json.dumps({"state": "passed", "source_head": manifest["source_head"], "unit": expected["Unit"], "output": str(output)}))


if __name__ == "__main__":
    main()
