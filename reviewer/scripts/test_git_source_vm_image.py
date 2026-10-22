import importlib.util
import os
from pathlib import Path
import unittest


class SourceKernelTests(unittest.TestCase):
    def test_actual_pinned_config_requires_cfs_bandwidth(self):
        path = os.environ.get("SFL_SOURCE_GUEST_KERNEL_CONFIG")
        if not path:
            self.skipTest("explicit captured source kernel config required")
        raw = Path(path).read_bytes()
        self.assertIn(b"CONFIG_CFS_BANDWIDTH=y\n", raw)
        spec = importlib.util.spec_from_file_location("source_image", Path(__file__).with_name("git-source-vm-image.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        module.source_kernel_config(raw)
        for value in [b"CONFIG_CFS_BANDWIDTH=n\n", b"# CONFIG_CFS_BANDWIDTH is not set\n"]:
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                module.source_kernel_config(raw.replace(b"CONFIG_CFS_BANDWIDTH=y\n", value))


if __name__ == "__main__":
    unittest.main()
