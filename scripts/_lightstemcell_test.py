#!/usr/bin/env python3
"""Offline unit tests for _lightstemcell helper functions.

Run with:
    python3 scripts/_lightstemcell_test.py

Uses only stdlib; no PVE network connection or bosh CLI required. The
network/subprocess-bound functions (download, extract, upload, orchestrate)
are exercised by the dry-run and integration harness, not here.
"""

from __future__ import annotations

import json
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lightstemcell as ls


SHA = "deadbeef" + "0" * 56  # 64-char sha256 hex; sha8 prefix = "deadbeef"


class FilenameTests(unittest.TestCase):
    def test_qcow2_filename_embeds_sha8(self) -> None:
        name = ls.qcow2_filename("ubuntu-noble", "1.383", SHA)
        self.assertEqual(name, "bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2")

    def test_qcow2_filename_uses_first_8_hex(self) -> None:
        name = ls.qcow2_filename("ubuntu-noble", "1.383", SHA)
        self.assertIn("-deadbeef.qcow2", name)
        self.assertNotIn(SHA, name)  # full sha must not bloat the filename

    def test_import_volid(self) -> None:
        self.assertEqual(
            ls.import_volid("local", "bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2"),
            "local:import/bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2",
        )


class ParseOsVersionTests(unittest.TestCase):
    def test_noble(self) -> None:
        os_name, version = ls.parse_os_version(
            "https://bosh.io/d/stemcells/bosh-openstack-kvm-ubuntu-noble?v=1.383"
        )
        self.assertEqual(os_name, "ubuntu-noble")
        self.assertEqual(version, "1.383")

    def test_jammy(self) -> None:
        os_name, version = ls.parse_os_version(
            "https://bosh.io/d/stemcells/bosh-openstack-kvm-ubuntu-jammy?v=1.438"
        )
        self.assertEqual(os_name, "ubuntu-jammy")
        self.assertEqual(version, "1.438")

    def test_unparseable_raises(self) -> None:
        with self.assertRaises(ValueError):
            ls.parse_os_version("file:///tmp/some-local-tarball.tgz")


class FindVolidTests(unittest.TestCase):
    def _rows(self) -> list[dict]:
        return [
            {"volid": "local:import/other.qcow2"},
            {"volid": "local:import/bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2"},
            {"volid": "local:iso/ubuntu.iso"},
        ]

    def test_find_present(self) -> None:
        got = ls.find_import_volid(
            self._rows(), "local", "bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2"
        )
        self.assertEqual(got, "local:import/bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2")

    def test_find_absent(self) -> None:
        got = ls.find_import_volid(self._rows(), "local", "missing.qcow2")
        self.assertIsNone(got)

    def test_find_wrong_storage(self) -> None:
        # Same filename but a different storage must not match (mismatched CID).
        got = ls.find_import_volid(
            self._rows(), "nfs", "bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2"
        )
        self.assertIsNone(got)

    def test_find_tolerates_non_dict_rows(self) -> None:
        rows = [None, 42, {"volid": "local:import/x.qcow2"}]
        self.assertIsNone(ls.find_import_volid(rows, "local", "missing.qcow2"))


class StemcellMFTests(unittest.TestCase):
    def test_preuploaded_carries_image_id(self) -> None:
        mf = ls.stemcell_mf("ubuntu-noble", "1.383", image_id="local:import/x.qcow2")
        self.assertIn("image_id: local:import/x.qcow2", mf)
        self.assertNotIn("image_url:", mf)
        self.assertIn("operating_system: ubuntu-noble", mf)
        self.assertIn('version: "1.383"', mf)
        self.assertIn("proxmox-kvm", mf)

    def test_fetch_carries_image_url(self) -> None:
        mf = ls.stemcell_mf(
            "ubuntu-noble", "1.383", image_url="https://example.com/x.qcow2"
        )
        self.assertIn("image_url: https://example.com/x.qcow2", mf)
        self.assertNotIn("image_id:", mf)

    def test_declares_registry_less_api_version(self) -> None:
        # bosh-cli create-env rejects a stemcell whose api_version < 2 with the
        # misleading "requires CPI v2.0 or greater" error. The MF must carry a
        # top-level api_version >= 2 (the registry-less contract the CPI emits).
        mf = ls.stemcell_mf("ubuntu-noble", "1.383", image_id="local:import/x.qcow2")
        self.assertIn(f"api_version: {ls.STEMCELL_API_VERSION}", mf)
        self.assertGreaterEqual(ls.STEMCELL_API_VERSION, 2)
        # Top-level field, not nested under cloud_properties.
        head = mf.split("cloud_properties:", 1)[0]
        self.assertIn("api_version:", head)

    def test_requires_exactly_one_source(self) -> None:
        with self.assertRaises(ValueError):
            ls.stemcell_mf("ubuntu-noble", "1.383")
        with self.assertRaises(ValueError):
            ls.stemcell_mf(
                "ubuntu-noble", "1.383", image_id="a", image_url="b"
            )


class CreateEnvTarballTests(unittest.TestCase):
    def test_tarball_has_mf_and_placeholder_image(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            path = ls.build_create_env_light_tarball(
                d, os_name="ubuntu-noble", version="1.383",
                image_id="local:import/x.qcow2",
            )
            self.assertTrue(Path(path).exists())
            with tarfile.open(path, "r:gz") as tf:
                names = tf.getnames()
                self.assertIn("stemcell.MF", names)
                self.assertIn("image", names)
                mf = tf.extractfile("stemcell.MF").read().decode()
                self.assertIn("image_id: local:import/x.qcow2", mf)
                img = tf.extractfile("image").read()
                # Placeholder only — must never be a real disk. Kept tiny.
                self.assertLessEqual(len(img), 16)

    def test_tarball_sha1_is_stable_and_hex(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            path = ls.build_create_env_light_tarball(
                d, os_name="ubuntu-noble", version="1.383",
                image_id="local:import/x.qcow2",
            )
            sha = ls.file_sha1(path)
            self.assertEqual(len(sha), 40)
            int(sha, 16)  # raises if not hex


class ManifestVarsTests(unittest.TestCase):
    def test_preuploaded_vars(self) -> None:
        v = ls.manifest_vars(
            "preuploaded", version="1.383", storage="local",
            filename="bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2",
        )
        self.assertEqual(v["stemcell_version"], "1.383")
        self.assertEqual(v["stemcell_storage"], "local")
        self.assertEqual(
            v["stemcell_filename"], "bosh-stemcell-ubuntu-noble-1.383-deadbeef.qcow2"
        )

    def test_fetch_vars(self) -> None:
        v = ls.manifest_vars(
            "fetch", version="1.383", storage="local", filename="x.qcow2",
            image_url="https://example.com/x.qcow2", token="t0ken",
        )
        self.assertEqual(v["stemcell_version"], "1.383")
        self.assertEqual(v["stemcell_url"], "https://example.com/x.qcow2")
        self.assertEqual(v["stemcell_token"], "t0ken")

    def test_fetch_defaults_empty_token(self) -> None:
        v = ls.manifest_vars(
            "fetch", version="1.383", storage="local", filename="x.qcow2",
            image_url="https://example.com/x.qcow2",
        )
        self.assertEqual(v["stemcell_token"], "")

    def test_unknown_mode_rejected(self) -> None:
        with self.assertRaises(ValueError):
            ls.manifest_vars("bogus", version="1.383", storage="local", filename="x.qcow2")


class MultipartFrameTests(unittest.TestCase):
    def test_exactly_one_filename_field(self) -> None:
        pre, head, epi = ls.multipart_frame("BOUND", "disk.qcow2")
        blob = (pre + head + epi).decode()
        # Regression: must NOT emit a second text "filename" field (duplicate
        # field corrupts the PVE upload). Exactly one name="filename" (the file).
        self.assertEqual(blob.count('name="filename"'), 1)
        self.assertIn('name="content"', blob)
        self.assertIn("import", blob)
        self.assertIn('filename="disk.qcow2"', blob)

    def test_frame_boundaries_and_crlf(self) -> None:
        pre, head, epi = ls.multipart_frame("BOUND", "x.qcow2")
        self.assertTrue(pre.startswith(b"--BOUND\r\n"))
        self.assertTrue(head.startswith(b"--BOUND\r\n"))
        self.assertEqual(epi, b"\r\n--BOUND--\r\n")
        # file_head ends with a blank line so the file bytes follow immediately.
        self.assertTrue(head.endswith(b"\r\n\r\n"))


class PveCfgFromVarsTests(unittest.TestCase):
    def _reader(self, mapping: dict) -> "callable":
        return lambda path: mapping.get(path, "")

    def test_token_auth(self) -> None:
        cfg = ls.pve_cfg_from_bosh_vars(
            "ignored",
            reader=self._reader({
                "/pve_host": "10.0.0.1", "/pve_port": "8006",
                "/pve_node": "pve", "/pve_stemcell_storage": "local",
                "/pve_vm_storage": "data", "/pve_api_token": "tok=secret",
            }),
        )
        self.assertEqual(cfg["host"], "10.0.0.1")
        self.assertEqual(cfg["port"], 8006)
        self.assertEqual(cfg["node"], "pve")
        self.assertEqual(cfg["stemcell_storage"], "local")
        self.assertEqual(cfg["api_token"], "tok=secret")
        self.assertNotIn("password", cfg)
        self.assertFalse(cfg["verify_ssl"])

    def test_password_fallback_and_port_default(self) -> None:
        cfg = ls.pve_cfg_from_bosh_vars(
            "ignored",
            reader=self._reader({
                "/pve_host": "10.0.0.1", "/pve_node": "pve",
                "/pve_password": "pw",
            }),
        )
        self.assertEqual(cfg["port"], 8006)  # absent -> default
        self.assertEqual(cfg["password"], "pw")
        self.assertNotIn("api_token", cfg)
        self.assertEqual(cfg["user"], "root@pam")  # absent -> default


class SidecarTests(unittest.TestCase):
    def test_roundtrip(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            ls.write_sidecar(
                d, "x.qcow2", sha256=SHA, source_sha1="a" * 40,
                os_name="ubuntu-noble", version="1.383",
            )
            got = ls.read_sidecar(d, "x.qcow2")
            self.assertEqual(got["sha256"], SHA)
            self.assertEqual(got["source_sha1"], "a" * 40)
            self.assertEqual(got["version"], "1.383")

    def test_read_absent_returns_none(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            self.assertIsNone(ls.read_sidecar(d, "nope.qcow2"))

    def test_sidecar_is_valid_json(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            ls.write_sidecar(
                d, "x.qcow2", sha256=SHA, source_sha1="a" * 40,
                os_name="ubuntu-noble", version="1.383",
            )
            raw = (Path(d) / "x.qcow2.sha256.json").read_text()
            json.loads(raw)  # raises on malformed


if __name__ == "__main__":
    unittest.main()
