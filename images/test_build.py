"""Downloader contract tests, with no gh and no credentials available."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("image_build", Path(__file__).with_name("build.py"))
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)


class Downloads(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.cache = self.root / "cache"
        self.cache.mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.log = self.root / "requests"
        curl = self.bin / "curl"
        curl.write_text(f"#!{sys.executable}\n" + '''import os, pathlib, sys, json
args=sys.argv[1:]
pathlib.Path(os.environ["REQUEST_LOG"]).write_text(json.dumps(args))
pathlib.Path(args[args.index("-o")+1]).write_bytes(b"fixture artifact")
sys.exit(int(os.environ.get("FAIL_DOWNLOAD", "0")))
''')
        curl.chmod(0o700)
        self.env = patch.dict(os.environ, {"PATH": str(self.bin), "REQUEST_LOG": str(self.log)}, clear=True)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.artifact = {"file": "release.tgz", "url": "https://example.invalid/releases/v1/release.tgz",
                         "sha256": hashlib.sha256(b"fixture artifact").hexdigest()}

    def test_uncached_and_cached_without_gh_or_authentication(self):
        self.assertFalse((self.bin / "gh").exists())
        path = build.download_artifact(self.artifact, self.cache)
        self.assertEqual(path.read_bytes(), b"fixture artifact")
        args = json.loads(self.log.read_text())
        self.assertIn(self.artifact["url"], args)
        self.assertEqual(args[0], "--disable", "curl must ignore personal configuration")
        self.assertNotIn("--user", args)
        self.assertNotIn("--netrc", args)
        self.log.unlink()
        (self.bin / "curl").unlink()
        self.assertEqual(build.download_artifact(self.artifact, self.cache), path)
        self.assertFalse(self.log.exists())
        self.assertEqual(list(self.cache.iterdir()), [path])

    def test_download_checksum_failure_leaves_no_output(self):
        self.artifact["sha256"] = "0" * 64
        with self.assertRaisesRegex(RuntimeError, "download SHA-256 mismatch"):
            build.download_artifact(self.artifact, self.cache)
        self.assertEqual(list(self.cache.iterdir()), [])

    def test_cached_checksum_failure(self):
        (self.cache / self.artifact["file"]).write_bytes(b"corrupted")
        with self.assertRaisesRegex(RuntimeError, "cached SHA-256 mismatch"):
            build.download_artifact(self.artifact, self.cache)
        self.assertFalse(self.log.exists())

    def test_partial_failed_download_cleanup(self):
        os.environ["FAIL_DOWNLOAD"] = "22"
        with self.assertRaises(subprocess.CalledProcessError):
            build.download_artifact(self.artifact, self.cache)
        self.assertEqual(list(self.cache.iterdir()), [])

    def test_lock_uses_pinned_https_urls(self):
        artifacts = json.loads(Path(__file__).with_name("artifacts.lock.json").read_text())
        for artifact in artifacts:
            self.assertTrue(artifact["url"].startswith("https://"))
            self.assertNotIn("latest", artifact["url"])
            self.assertEqual(len(bytes.fromhex(artifact["sha256"])), 32)


if __name__ == "__main__":
    unittest.main()
