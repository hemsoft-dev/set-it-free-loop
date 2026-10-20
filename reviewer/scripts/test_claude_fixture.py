import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("claude_fixture", Path(__file__).with_name("claude-fixture.py"))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class QualificationTests(unittest.TestCase):
    def test_build_environment_blocks_ambient_download_and_source_overrides(self):
        with patch.dict(os.environ, {"GOPROXY": "https://proxy.invalid", "GONOPROXY": "*",
                                     "GOCACHEPROG": "remote-cache", "GOFLAGS": "-overlay=untrusted", "GOTOOLCHAIN": "auto"}):
            env = fixture.offline_environment("/cache", "/empty-module-cache")
        self.assertEqual(env["GOPROXY"], "off")
        self.assertEqual(env["GONOPROXY"], "none")
        self.assertEqual(env["GOVCS"], "*:off")
        self.assertEqual(env["GOTOOLCHAIN"], "local")
        self.assertEqual(env["GOMODCACHE"], "/empty-module-cache")
        self.assertNotIn("GOFLAGS", env)
        self.assertNotIn("GOCACHEPROG", env)

    def test_snapshot_survives_replacement_and_rejects_wrong_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "client"
            source.write_bytes(b"pinned executable")
            expected = fixture.digest(source)
            copied = fixture.snapshot_file(source, root / "snapshot", expected)
            source.unlink()
            source.write_bytes(b"replacement")
            self.assertEqual(fixture.digest(copied), expected)
            with self.assertRaisesRegex(RuntimeError, "digest differs"):
                fixture.snapshot_file(source, root / "bad", expected)
            self.assertFalse((root / "bad").exists())

    def test_dirty_source_cannot_supply_a_qualification_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            module = root / "reviewer"
            module.mkdir()
            (module / "source.go").write_text("package fixture\n")
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            subprocess.run(["git", "-C", str(root), "add", "."], check=True)
            subprocess.run(["git", "-C", str(root), "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], check=True)
            self.assertFalse(fixture.source_manifest(module)["source_dirty"])
            (module / "source.go").write_text("package modified\n")
            with self.assertRaisesRegex(RuntimeError, "clean committed source"):
                fixture.source_manifest(module)
