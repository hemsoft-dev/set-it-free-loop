#!/usr/bin/env python3
"""Build and qualify sealed descriptor boot of the synthetic guest. No live worker."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

_spec = importlib.util.spec_from_file_location("sfl_isolation_fixture", Path(__file__).with_name("isolation-fixture.py"))
_shared = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_shared)


def contract_hash(value):
    # Match Go's encoding/json struct order, UTF-8 and HTML escaping. Typed
    # dictionaries below follow the declarations; no candidate data is included.
    raw = json.dumps(value, separators=(",", ":"), ensure_ascii=False)
    for character, escaped in [("&", "\\u0026"), ("<", "\\u003c"), (">", "\\u003e"),
                               ("\u2028", "\\u2028"), ("\u2029", "\\u2029")]:
        raw = raw.replace(character, escaped)
    return _shared.digest(raw.encode())


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
    base_env = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "GOENV": "off", "GOTOOLCHAIN": "local",
                "GOPATH": os.environ.get("GOPATH", str(Path.home() / "go"))}
    build_env = {**base_env, "CGO_ENABLED": "0", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "-mod=readonly",
                 "GOCACHE": str(output / "go-cache"),
                 "GOMODCACHE": _shared.execute([args.go, "env", "GOMODCACHE"], cwd=module, env=base_env).decode().strip()}
    program = output / "vmbootfixture"
    _shared.execute([args.go, "build", "-trimpath", "-buildvcs=true", "-o", str(program), "./cmd/vmbootfixture"],
                    cwd=module, env=build_env, timeout=180)
    program.chmod(0o500)
    binding = {"session_hash": _shared.digest(("synthetic-vm:"+manifest["source_head"]).encode()),
               "launch_id": manifest["nonce"], "containment_id": _shared.digest(("synthetic-unit:"+manifest["nonce"]).encode())}
    def asset(path, digest):
        return {"Path": str(path), "SHA256": digest, "Bytes": path.stat().st_size}
    qemu = Path("/usr/bin/qemu-system-x86_64")
    kernel, image = output / "guest" / "vmlinuz", output / "guest" / "fixture.cpio.gz"
    boot = {"Version": 1, "Binding": binding, "QEMU": asset(qemu, manifest["qemu_sha256"]),
            "Kernel": asset(kernel, manifest["kernel_sha256"]), "Initrd": asset(image, manifest["image_sha256"])}
    config = output / "fixture-config.json"
    _shared.write_json(config, {"Version": 1, "Purpose": "synthetic-role-fixture", "SourceHead": manifest["source_head"], "Boot": boot})
    config.chmod(0o600)
    sealed = {"program": str(program), "program_sha256": _shared.digest(program.read_bytes()),
              "configuration": str(config), "SourceHead": manifest["source_head"],
              "BindingHash": contract_hash(binding), "BootSpecHash": contract_hash(boot)}
    manifest["sealed_boot"] = sealed
    _shared.write_json(output / "manifest.json", manifest)
    _shared.run_fixture(output, manifest, kernel, image, sealed_boot=sealed)


if __name__ == "__main__":
    main()
