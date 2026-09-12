"""Offline CPI candidate provenance regressions."""
import hashlib
import io
from pathlib import Path
import tarfile
import tempfile
import unittest

from _release_artifact import release_identity


class ReleaseIdentityTest(unittest.TestCase):
    def artifact(self, directory, manifest, *, duplicate=False, link=False):
        path = Path(directory) / "candidate.tgz"
        with tarfile.open(path, "w:gz") as archive:
            for _ in range(2 if duplicate else 1):
                entry = tarfile.TarInfo("release.MF")
                payload = manifest.encode()
                entry.size = len(payload)
                if link:
                    entry.type = tarfile.SYMTYPE
                    entry.linkname = "/private/secret"
                archive.addfile(entry, io.BytesIO(payload))
        return path

    def test_candidate_version_and_checksum_come_from_actual_artifact(self):
        with tempfile.TemporaryDirectory() as directory:
            path = self.artifact(directory, "name: bosh-proxmox-cpi\nversion: '0.0.0+dev.17'\njobs: []\n")
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            self.assertEqual(release_identity(path, digest)["version"], "0.0.0+dev.17")
            self.assertEqual(release_identity(path)["sha256"], digest)
            with self.assertRaises(ValueError):
                release_identity(path, "0" * 64)

    def test_ambiguous_or_foreign_manifest_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            for manifest, options in [
                ("name: other\nversion: 1\n", {}),
                ("name: bosh-proxmox-cpi\nversion: 1\nversion: 2\n", {}),
                ("name: bosh-proxmox-cpi\nversion: !!str 1\n", {}),
                ("name: bosh-proxmox-cpi\nversion: 1\n", {"duplicate": True}),
                ("name: bosh-proxmox-cpi\nversion: 1\n", {"link": True}),
            ]:
                with self.subTest(manifest=manifest, options=options):
                    with self.assertRaises(ValueError):
                        release_identity(self.artifact(directory, manifest, **options))


if __name__ == "__main__":
    unittest.main()
