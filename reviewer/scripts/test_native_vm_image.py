import hashlib
import importlib.util
from pathlib import Path
import stat
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("native_image", Path(__file__).with_name("native-vm-image.py"))
image = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image)


class NativeImageTests(unittest.TestCase):
    def test_exact_source_inode_bytes_and_symlink_denial(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"snapshot"
            path.write_bytes(b"synthetic pinned input")
            pin = hashlib.sha256(path.read_bytes()).hexdigest()
            self.assertEqual(image.pinned_bytes(path, pin, 128), b"synthetic pinned input")
            with self.assertRaises(RuntimeError):
                image.pinned_bytes(path, "a"*64, 128)
            link = path.with_name("link")
            link.symlink_to(path)
            with self.assertRaises(OSError):
                image.pinned_bytes(link, pin, 128)

    def test_fixed_role_paths_and_file_parent_conflicts(self):
        runtime = image.Image("a"*40)
        runtime.runtime("/native-init", "/native-init", b"fixed helper", image.ROLES)
        runtime.runtime("/native-init", "/client/codex-code-mode-host", b"fixed helper", ["codex"])
        self.assertEqual(len(runtime.release()["Files"]), 2)
        with self.assertRaises(RuntimeError):
            runtime.runtime("/runtime/other", "/native-init", b"other", ["reader"])
        with self.assertRaises(RuntimeError):
            runtime.runtime("/profile/auth.json", "/native-init", b"auth", ["reader"])
        with self.assertRaises(RuntimeError):
            runtime.entry("native-init/child", b"inert")
        with self.assertRaises(RuntimeError):
            runtime.entry("../outside", b"inert")
        self.assertTrue(stat.S_ISREG(runtime.entries["native-init"][0]))

    def test_builtin_kernel_required_and_duplicate_rejected(self):
        with self.assertRaises(RuntimeError):
            image.kernel_config(b"CONFIG_VIRTIO_CONSOLE=m\n")
        with self.assertRaises(RuntimeError):
            image.kernel_config(b"CONFIG_CGROUPS=y\nCONFIG_CGROUPS=y\n")

    def test_canonical_release_struct_order_and_html_escaping(self):
        release = image.Image("a"*40).release()
        self.assertTrue(image.canonical(release).startswith(b'{"Version":1,"Purpose":"native-review","SourceHead":'))
        self.assertEqual(image.canonical({"value":"<&>"}), b'{"value":"\\u003c\\u0026\\u003e"}')


if __name__ == "__main__":
    unittest.main()
