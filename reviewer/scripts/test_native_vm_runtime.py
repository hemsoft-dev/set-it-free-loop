import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("native_runtime", Path(__file__).with_name("native-vm-runtime.py"))
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


def fixture():
    names = ["firmware/kvmvapic.bin", "firmware/linuxboot.bin", "firmware/linuxboot_dma.bin",
             "lib64/ld-linux-x86-64.so.2", "usr/lib/x86_64-linux-gnu/libc.so.6"]
    return {name: ("producer fixture for "+name).encode() for name in names}


class NativeRuntimeTests(unittest.TestCase):
    def test_actual_producer_matches_committed_cross_language_frame(self):
        program = {"SHA256": runtime.digest(b"synthetic QEMU pin"), "Bytes": 18}
        manifest, frame = runtime.encode(program, fixture())
        self.assertEqual(frame, (Path(__file__).parent.parent/"testdata/native-qemu-runtime.bin").read_bytes())
        self.assertEqual([item["Path"] for item in manifest["Files"]], sorted(fixture()))
        self.assertEqual(runtime.encode(program, dict(reversed(list(fixture().items()))))[1], frame)

    def test_metadata_cannot_encode_link_traversal_or_commands(self):
        program = {"SHA256": "a"*64, "Bytes": 1}
        for path in ["../outside", "/absolute", "usr/lib/x86_64-linux-gnu/../libc.so.6",
                     "usr/lib/x86_64-linux-gnu/sub/libc.so.6", "bin/sh", "profile/auth.json"]:
            bodies = fixture()
            bodies[path] = b"inert"
            with self.assertRaises(RuntimeError):
                runtime.encode(program, bodies)
        bodies = fixture()
        del bodies["firmware/linuxboot_dma.bin"]
        with self.assertRaises(RuntimeError):
            runtime.encode(program, bodies)
        with self.assertRaises(RuntimeError):
            runtime.encode({"SHA256": "invalid", "Bytes": 1}, fixture())
