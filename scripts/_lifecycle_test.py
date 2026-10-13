#!/usr/bin/env python3
"""Tests for the VM CID handling the lifecycle and certify harnesses share.

The harnesses hand the CPI the VM CID create_vm returned, which can carry a
cluster stamp and a token, and they use the bare VMID for every PVE API path.
scripts/_vmcid.py is the Python twin of the CPI's parser, and these tests run
it against the vector file the Go parser's tests read, so the two cannot
drift. The PVE reads are stubbed, so nothing here contacts a lab.

Run:
    python3 scripts/_lifecycle_test.py
"""

from __future__ import annotations

import json
import sys
import unittest
import unittest.mock
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS_DIR))
import _pve_verify  # noqa: E402
import _vmcid  # noqa: E402

VECTORS = SCRIPTS_DIR.parent / "src" / "pve_cpi" / "internal" / "vmcid" / "testdata" / "cid_vectors.json"


def _load_vectors() -> dict:
    with VECTORS.open(encoding="utf-8") as fh:
        data = json.load(fh)
    if not data.get("valid") or not data.get("invalid"):
        raise AssertionError(f"{VECTORS} has no valid or no invalid entries")
    return data


class TestSharedVectors(unittest.TestCase):
    """The twin answers every shared vector the way the Go parser does."""

    def test_valid_vectors_parse(self) -> None:
        for case in _load_vectors()["valid"]:
            with self.subTest(raw=case["raw"]):
                got = _vmcid.parse(case["raw"])
                self.assertEqual(got, _vmcid.VMCID(case["vmid"], case["cluster"], case["token"]))
                self.assertEqual(str(got), case["raw"])
                self.assertEqual(got.stamped, bool(case["cluster"]))
                self.assertEqual(got.path_id, str(case["vmid"]))

    def test_invalid_vectors_raise(self) -> None:
        for raw in _load_vectors()["invalid"]:
            with self.subTest(raw=raw):
                with self.assertRaises(ValueError):
                    _vmcid.parse(raw)

    def test_non_string_raises(self) -> None:
        for raw in (101, None, b"101"):
            with self.subTest(raw=raw):
                with self.assertRaises(ValueError):
                    _vmcid.parse(raw)  # type: ignore[arg-type]


class TestAsPathID(unittest.TestCase):
    """as_path_id turns whatever the harness holds into PVE's VMID."""

    def test_forms(self) -> None:
        cases = [
            (101, "101"),
            ("101", "101"),
            (" 101 ", "101"),
            ("101-0a1b2c3d-4e5f6a7b", "101"),
            (" 101-0a1b2c3d-4e5f6a7b\n", "101"),
            ("not-a-cid", "not-a-cid"),
            ("", ""),
        ]
        for value, want in cases:
            with self.subTest(value=value):
                self.assertEqual(_vmcid.as_path_id(value), want)

    def test_bool_is_rejected(self) -> None:
        with self.assertRaises(ValueError):
            _vmcid.as_path_id(True)


class TestVerifierTakesStampedCIDs(unittest.TestCase):
    """The verifier reads PVE by VMID when the harness passes a stamped CID."""

    def _verifier(self) -> _pve_verify.PVEVerifier:
        return _pve_verify.PVEVerifier({
            "host": "pve.example.com",
            "port": 8006,
            "node": "pve1",
            "api_token": "root@pam!t=x",
            "verify_ssl": False,
        })

    def test_vm_exists_matches_the_vmid(self) -> None:
        v = self._verifier()
        with unittest.mock.patch.object(v, "cluster_vms", return_value=[(101, "pve2")]):
            self.assertTrue(v.vm_exists("101-0a1b2c3d-4e5f6a7b"))
            self.assertFalse(v.vm_exists("102-0a1b2c3d-4e5f6a7b"))

    def test_qemu_config_reads_the_bare_vmid_path(self) -> None:
        v = self._verifier()
        paths: list[str] = []

        def fake_get(path: str):
            paths.append(path)
            return {"name": "vm"}

        with unittest.mock.patch.object(v, "cluster_vms", return_value=[(101, "pve2")]), \
                unittest.mock.patch.object(v, "_get", side_effect=fake_get):
            self.assertEqual(v.qemu_config("101-0a1b2c3d-4e5f6a7b"), {"name": "vm"})
        self.assertEqual(paths, ["/nodes/pve2/qemu/101/config"])


if __name__ == "__main__":
    unittest.main()
