"""Verify the archives CI builds and the release workflow will publish."""

import hashlib
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


class PackageTests(unittest.TestCase):
    def test_release_archives_and_checksums(self):
        root = Path(__file__).resolve().parents[2]
        tag = os.environ.get("TAG", "v0.0.0")
        with tempfile.TemporaryDirectory() as temporary:
            out = Path(os.environ.get("TAP_PACKAGE_DIR", temporary)).resolve()
            if "TAP_PACKAGE_DIR" not in os.environ:
                subprocess.run([str(root / ".github/release/package.sh"), str(out)],
                               cwd=root, env=dict(os.environ, TAG=tag), check=True)
            platforms = ("linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64")
            expected = {f"tap_{tag}_{platform}.tar.gz" for platform in platforms}
            checksums = {}
            for line in (out / "checksums.txt").read_text().splitlines():
                digest, name = line.split()
                self.assertNotIn(name, checksums)
                checksums[name] = digest
            self.assertEqual(set(checksums), expected)
            native = subprocess.check_output(["go", "env", "GOHOSTOS", "GOHOSTARCH"],
                                             text=True).split()
            for name in sorted(expected):
                with self.subTest(archive=name):
                    archive = out / name
                    self.assertEqual(hashlib.sha256(archive.read_bytes()).hexdigest(), checksums[name])
                    with tarfile.open(archive) as tar:
                        self.assertEqual(sorted(tar.getnames()), ["LICENSE", "tap"])
                        self.assertTrue(all(member.isfile() for member in tar.getmembers()))
                        self.assertEqual(tar.extractfile("LICENSE").read(), (root / "LICENSE").read_bytes())
                        self.assertTrue(tar.getmember("tap").mode & 0o111)
                        if name == f"tap_{tag}_{'_'.join(native)}.tar.gz":
                            binary = Path(temporary) / "tap"
                            binary.write_bytes(tar.extractfile("tap").read())
                            binary.chmod(0o755)
                            version = subprocess.check_output([str(binary), "--version"], text=True).strip()
                            self.assertEqual(version, tag)
            # The formula must consume the manifest that accompanies these archives.
            formula = subprocess.check_output([str(root / ".github/release/formula.sh"), tag,
                                               str(out / "checksums.txt")], cwd=root, text=True)
            for digest in checksums.values():
                self.assertIn(f"sha256 '{digest}'", formula)
