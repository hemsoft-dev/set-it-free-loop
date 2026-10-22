import base64
import importlib.util
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import struct
from unittest import mock
import unittest

DIRECTORY = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("git_vm_prerequisite", DIRECTORY/"run.py")
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)
NONCE = "013821658b5b49d4845db4613b935413"


class GuestRecordTests(unittest.TestCase):
    def test_auxiliary_inputs_are_bounded_regular_files(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-input-") as temporary:
            root = Path(temporary)
            source = root/"input"
            source.write_bytes(b"data")
            self.assertEqual(probe.read_bounded(source, 4), b"data")
            self.assertEqual(probe.hash_file(source, 4), hashlib.sha256(b"data").hexdigest())
            for operation in [probe.read_bounded, probe.hash_file]:
                with self.assertRaises(RuntimeError):
                    operation(source, 3)
                link = root/("link-"+operation.__name__)
                link.symlink_to(source)
                with self.assertRaises(OSError):
                    operation(link, 4)
                fifo = root/("fifo-"+operation.__name__)
                os.mkfifo(fifo)
                with self.assertRaises(RuntimeError):
                    operation(fifo, 4)

    def actual(self):
        return probe.decode_guest_result(base64.b64decode((DIRECTORY/"testdata/actual-guest-result.b64").read_bytes().strip(), validate=True), NONCE)

    def frame(self, value):
        return ("SFL_GIT_VM_PREREQ_"+NONCE+" "+json.dumps(value)+"\n").encode()

    def test_actual_pinned_producer_record(self):
        host = json.loads((DIRECTORY/"testdata/actual-host-receipt.json").read_bytes())
        self.assertEqual(hashlib.sha256((DIRECTORY/"probe.c").read_bytes()).hexdigest(), host["guest_probe_source_sha256"])
        self.assertEqual(host["nonce"], NONCE)
        self.assertTrue(host["unit_stopped"] and host["original_cgroup_empty"] and host["runtime_verified_after_stop"])
        self.assertEqual(host["final_unit"]["MainPID"], "0")
        self.assertIn(host["final_unit"]["ActiveState"], ["inactive", "failed"])
        self.assertTrue(host["observed"]["ControlGroup"].endswith("/sfl-git-vm-probe-"+NONCE+".service"))
        self.assertEqual(host["observed"]["kernel_limits"], {"memory.max": "1073741824", "memory.swap.max": "0", "pids.max": "64", "cpu.max": "100000 100000"})
        self.assertTrue(host["fake_only"] and not host["model_inference"] and not host["production_qualified"])
        self.assertRegex(host["observed"]["InvocationID"], r"^[0-9a-f]{32}$")
        self.assertEqual(hashlib.sha256(base64.b64decode((DIRECTORY/"testdata/actual-guest-result.b64").read_bytes().strip(), validate=True)).hexdigest(), host["guest_wire_sha256"])
        value = self.actual()
        self.assertTrue(probe.prerequisites_passed(value))
        self.assertEqual(value["kernel"], "7.0.0-38-generic")
        self.assertFalse(value["production_qualified"])

    def test_legacy_record_lacks_writability_evidence(self):
        legacy = base64.b64decode((DIRECTORY/"testdata/legacy-guest-result-v1.b64").read_bytes().strip(), validate=True)
        self.assertEqual(hashlib.sha256(legacy).hexdigest(), "42bd090440c0adbb8db6e74d3bb347b385dca8dde19103a3214983dd8c215085")
        with self.assertRaises(RuntimeError):
            probe.decode_guest_result(legacy, "883c661671d20c825627c9d854192bae")

    def test_schema_framing_and_provenance_refuse(self):
        actual = base64.b64decode((DIRECTORY/"testdata/actual-guest-result.b64").read_bytes().strip(), validate=True)
        malformed = [actual+actual, b"x"*(4*1024*1024+1), actual.replace(NONCE.encode(), b"0"*32),
                     actual.replace(b'"version":2', b'"version":2,"version":2'), actual.rstrip()+b" {}\n"]
        for change in [{"version": 1}, {"size_bytes": True}, {"sealed_proc_exec": 1},
                       {"production_qualified": True}, {"unexpected": True}]:
            malformed.append(self.frame(dict(self.actual(), **change)))
        missing = self.actual()
        del missing["noswap_reported"]
        malformed.append(self.frame(missing))
        for raw in malformed:
            with self.subTest(raw=raw[:80]):
                with self.assertRaises((RuntimeError, ValueError)):
                    probe.decode_guest_result(raw, NONCE)

    def test_observed_prerequisite_failures_never_pass(self):
        for change in [{"mount_succeeded": False}, {"mount_errno": 1}, {"noswap_reported": False},
                       {"size_bytes": 268431360}, {"inode_limit": 65535}, {"filesystem_type": 0},
                       {"mount_flags": 0}, {"mount_flags": 15}, {"work_mode": 0o755}, {"work_uid": 0},
                       {"work_gid": 0}, {"work_writable": False}, {"sealed_proc_exec": False}, {"sealed_execveat": False}]:
            with self.subTest(change=change):
                value = probe.decode_guest_result(self.frame(dict(self.actual(), **change)), NONCE)
                self.assertFalse(probe.prerequisites_passed(value))

    def test_compiled_guest_refuses_ordinary_host_execution(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-guard-") as temporary:
            binary = Path(temporary)/"probe"
            environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}
            subprocess.run(["/usr/bin/cc", "-static", "-O2", "-Wall", "-Wextra", "-Werror",
                            "-o", str(binary), str(DIRECTORY/"probe.c")],
                           env=environment, check=True, timeout=30)
            for arguments in [[], ["--guest-init"]]:
                result = subprocess.run([str(binary), *arguments], env=environment,
                                        capture_output=True, timeout=3)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, b"")
                self.assertEqual(result.stderr, b"")




class LaunchBoundaryTests(unittest.TestCase):
    def test_runtime_frame_from_actual_encoder_and_mutations(self):
        path = DIRECTORY.parents[1]/"native-vm-runtime.py"
        spec = importlib.util.spec_from_file_location("runtime_producer", path)
        producer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(producer)
        bodies = {name: name.encode() for name in ["lib64/ld-linux-x86-64.so.2", "usr/lib/x86_64-linux-gnu/libc.so.6",
                  "firmware/kvmvapic.bin", "firmware/linuxboot.bin", "firmware/linuxboot_dma.bin"]}
        program = {"SHA256": hashlib.sha256(b"qemu").hexdigest(), "Bytes": 4}
        _, frame = producer.encode(program, bodies)
        self.assertEqual(probe.decode_runtime(frame, program), bodies)
        for raw in [frame+b"x", frame[:-1], frame.replace(b"libc.so.6", b"libx.so.6"),
                    frame.replace(b'"Version":1', b'"Version":2'),
                    frame[:len(producer.MAGIC)]+struct.pack(">I", 32769)+frame[len(producer.MAGIC)+4:]]:
            with self.subTest(raw=raw[:100]):
                with self.assertRaises((RuntimeError, ValueError)):
                    probe.decode_runtime(raw, program)
        with self.assertRaises(RuntimeError):
            probe.decode_runtime(frame, dict(program, Bytes=5))

    def test_runtime_copies_do_not_follow_replaced_input(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-copy-") as temporary:
            directory = Path(temporary)
            source = directory/"source"
            source.write_bytes(b"original")
            root = directory/"runtime"
            identity = probe.prepare_runtime(root, {"kernel": probe.read_bounded(source, 64)})
            source.unlink()
            source.symlink_to("/does-not-exist")
            probe.verify_runtime(root, identity)
            self.assertEqual((root/"kernel").read_bytes(), b"original")
            (root/"kernel").chmod(0o600)
            with self.assertRaises(RuntimeError):
                probe.verify_runtime(root, identity)
            (root/"kernel").chmod(0o444)
            root.chmod(0o700)
            (root/"kernel").unlink()
            (root/"kernel").symlink_to(source)
            root.chmod(0o555)
            with self.assertRaises(RuntimeError):
                probe.verify_runtime(root, identity)
            root.chmod(0o700)

    def test_unknown_and_replacement_units_are_never_stopped(self):
        description = "inert test"
        original = {"Description": description, "InvocationID": "a"*32, "MainPID": "123", "ActiveState": "active"}
        self.assertEqual(probe.cleanup_action(original, original, description), "wait-original")
        for observed, current in [(None, original), ({}, original), (original, dict(original, InvocationID="b"*32)),
                                  (original, dict(original, Description="other")), (original, dict(original, InvocationID=""))]:
            with self.subTest(observed=observed, current=current):
                with self.assertRaises(RuntimeError):
                    probe.cleanup_action(current, observed, description)
        self.assertEqual(probe.cleanup_action({"MainPID": "0", "ActiveState": "inactive"}, None, description), "already-stopped")

    def test_owned_unit_entry_refuses_host_before_opening_inputs(self):
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(probe, "read_bounded") as read:
            with self.assertRaises(RuntimeError):
                probe.owned_unit("/not-opened/unit.json")
            read.assert_not_called()

    def test_mount_options_use_superblock_exact_tokens(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-mountinfo-") as temporary:
            root = Path(temporary)
            harness = root/"test.c"
            harness.write_text('#define main guest_main\n#include "'+str(DIRECTORY/"probe.c")+'"\n#undef main\n'
                'int main(int argc, char **argv) { return argc == 2 && work_mount_noswap(argv[1]) ? 0 : 1; }\n')
            binary = root/"test"
            subprocess.run(["/usr/bin/cc", "-O2", "-Wall", "-Wextra", "-Werror", "-o", str(binary), str(harness)], check=True, timeout=30)
            valid = "23 1 0:2 / /work rw,nosuid,nodev,noexec - tmpfs tmpfs rw,size=262144k,nr_inodes=65536,noswap"
            self.assertEqual(subprocess.run([str(binary), valid], timeout=3).returncode, 0)
            for row in [valid.replace(",noswap", ",noswap=1"), valid.replace("tmpfs rw,size", "noswap rw,size").replace(",noswap", ""),
                        valid.replace("/work", "/other"), valid.replace("- tmpfs", "- ext4"), valid+" extra"]:
                self.assertEqual(subprocess.run([str(binary), row], timeout=3).returncode, 1)


class BootstrapEvidenceTests(unittest.TestCase):
    def test_bootstrap_list_requires_exact_complete_sorted_pins(self):
        loader = {"Path": "/lib64/ld-linux-x86-64.so.2", "SHA256": "a"*64, "Bytes": 1}
        libc = {"Path": "/usr/lib/x86_64-linux-gnu/libc.so.6", "SHA256": "b"*64, "Bytes": 1}
        probe.validate_bootstrap_pins([loader, libc])
        for assets in [[], [loader], [libc, loader], [loader, loader], [loader, dict(libc, Bytes=True)],
                       [loader, dict(libc, Bytes=0)], [loader, dict(libc, SHA256="invalid")],
                       [loader, dict(libc, extra=True)], [loader, dict(libc, Path="/usr/lib/x86_64-linux-gnu/libother.so.1")],
                       [loader, libc]*17, [dict(loader, Path="/firmware/linuxboot.bin"), libc],
                       [dict(loader, Path="/usr/lib/x86_64-linux-gnu/libother.so.1"), libc],
                       [loader, dict(libc, Path="usr/lib/x86_64-linux-gnu/libc.so.6")]]:
            with self.subTest(assets=assets):
                with self.assertRaises(RuntimeError):
                    probe.validate_bootstrap_pins(assets)

    def test_selected_image_runtime_survives_relocation_and_refuses_mutation(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-relocated-") as temporary:
            directory = Path(temporary)
            old = directory/"old"
            new = directory/"new"
            runtime = old/"host-runtime/native-qemu-runtime.bin"
            runtime.parent.mkdir(parents=True)
            runtime.write_bytes(b"measured runtime")
            asset = {"Path": str(runtime), "SHA256": hashlib.sha256(runtime.read_bytes()).hexdigest(), "Bytes": runtime.stat().st_size}
            old.rename(new)
            self.assertEqual(probe.image_runtime_bytes(new, asset), b"measured runtime")
            old.mkdir()
            (old/"host-runtime").mkdir()
            runtime.write_bytes(b"different old artifact")
            self.assertEqual(probe.image_runtime_bytes(new, asset), b"measured runtime")
            selected = new/"host-runtime/native-qemu-runtime.bin"
            selected.write_bytes(b"changed")
            with self.assertRaises(RuntimeError):
                probe.image_runtime_bytes(new, asset)
            selected.unlink()
            selected.symlink_to(runtime)
            with self.assertRaises(OSError):
                probe.image_runtime_bytes(new, asset)

    def test_actual_isolated_python_bootstrap_and_changed_pin_refusal(self):
        # Actual interpreter, mappings and stdlib producer. No manager/KVM opens.
        code = "import importlib.util,json; s=importlib.util.spec_from_file_location('probe',"+repr(str(DIRECTORY/"run.py"))+"); p=importlib.util.module_from_spec(s); s.loader.exec_module(p); a=importlib.util.spec_from_file_location('inert_vm_archive',"+repr(str(DIRECTORY.parents[1]/"isolation-fixture.py"))+"); m=importlib.util.module_from_spec(a); a.loader.exec_module(m); v=p.capture_python_bootstrap(); p.verify_python_bootstrap(v); print(json.dumps(v))"
        result = subprocess.run(["/usr/bin/python3", "-I", "-S", "-B", "-c", code], check=True,
                                capture_output=True, timeout=10, env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
        value = json.loads(result.stdout)
        probe.validate_python_bootstrap(value)
        changed = dict(value, Mapped=[])
        with self.assertRaises(RuntimeError):
            probe.validate_python_bootstrap(changed)
        changed = json.loads(json.dumps(value))
        changed["Program"]["SHA256"] = "0"*64
        # A changed program is rejected from installed bytes, before comparing
        # this unittest process's extra imported modules or opening a VM.
        with self.assertRaises(RuntimeError):
            probe.verify_python_bootstrap(changed)
        for field in ["Mapped", "Modules"]:
            changed = json.loads(json.dumps(value))
            changed[field][0]["SHA256"] = "0"*64
            with self.assertRaises(RuntimeError):
                probe.verify_python_bootstrap(changed)


    def test_nonisolated_cli_refuses_before_creating_output(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-python-entry-") as temporary:
            output = Path(temporary)/"not-created"
            result = subprocess.run(["/usr/bin/python3", "-B", str(DIRECTORY/"run.py"), "--run-inert-only",
                                     "--image", "/does-not-open-image", "--output", str(output)],
                                    capture_output=True, timeout=3, env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"})
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b"isolated Python bootstrap required", result.stderr)
            self.assertFalse(output.exists())


    def test_python_mapping_checks_actual_device_and_inode(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-mapping-") as temporary:
            paths = [Path(temporary)/"one", Path(temporary)/"two"]
            rows = []
            for path in paths:
                path.write_bytes(b"mapped input")
                info = path.stat()
                rows.append("1000-2000 r--p 00000000 %x:%x %d %s" % (os.major(info.st_dev), os.minor(info.st_dev), info.st_ino, path))
            with mock.patch.object(probe, "read_bounded", return_value=("\n".join(rows)+"\n").encode()):
                self.assertEqual(probe.python_mappings(), sorted(map(str, paths)))
            for row in [rows[0]+" (deleted)", rows[0].replace(str(paths[0].stat().st_ino), "1", 1)]:
                with mock.patch.object(probe, "read_bounded", return_value=(row+"\n"+rows[1]+"\n").encode()):
                    with self.assertRaises(RuntimeError):
                        probe.python_mappings()


class TerminationAndRunnerTests(unittest.TestCase):
    def test_passive_shutdown_never_targets_a_replacement_unit(self):
        original = {"Description": "test", "InvocationID": "a"*32, "MainPID": "123", "ActiveState": "active"}
        inactive = {"LoadState": "not-found", "MainPID": "0", "ActiveState": "inactive"}
        sleeps = []
        with mock.patch.object(probe.subprocess, "run", side_effect=AssertionError("named stop invoked")):
            observations = iter([original, inactive])
            self.assertEqual(probe.wait_for_original_stop(lambda: next(observations), original, "test", 2,
                             now=lambda: 0, sleep=sleeps.append), inactive)
            self.assertEqual(sleeps, [0.1])
            observations = iter([original, dict(original, InvocationID="b"*32)])
            with self.assertRaises(RuntimeError):
                probe.wait_for_original_stop(lambda: next(observations), original, "test", 2,
                                             now=lambda: 0, sleep=lambda _: None)
            with self.assertRaises(RuntimeError):
                probe.wait_for_original_stop(lambda: original, original, "test", 2,
                                             now=lambda: 2, sleep=lambda _: self.fail("deadline slept"))

    def test_retained_runner_replacement_is_independently_rejected(self):
        with tempfile.TemporaryDirectory(prefix="sfl-git-vm-runner-") as temporary:
            runner = Path(temporary)/"runner.py"
            pin = probe.snapshot_file(runner, b"trusted bootstrap", 0o400)
            probe.verify_retained_runner(runner, pin)
            runner.chmod(0o600)
            runner.write_bytes(b"forged successful output")
            runner.chmod(0o400)
            with self.assertRaises(RuntimeError):
                probe.verify_retained_runner(runner, pin)
            runner.unlink()
            other = Path(temporary)/"other"
            other.write_bytes(b"trusted bootstrap")
            runner.symlink_to(other)
            with self.assertRaises(OSError):
                probe.verify_retained_runner(runner, pin)


if __name__ == "__main__":
    unittest.main()
