#!/usr/bin/env python3
"""Offline unit tests for _integration helper functions.

Run with:
    python3 scripts/_integration_test.py

Uses only stdlib; no PVE network connection required.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

# Mirror the import pattern used by scripts/test so the module resolves
# regardless of the working directory the test is invoked from.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _integration


class TestSelectLocalDiskPools(unittest.TestCase):
    """Tests for _integration.select_local_disk_pools."""

    def _call(self, entries: list[dict]) -> list[str]:
        return _integration.select_local_disk_pools(entries)

    def test_lvm_with_images_kept(self) -> None:
        entries = [
            {"storage": "local-lvm", "type": "lvm", "content": "images,rootdir"},
        ]
        self.assertEqual(self._call(entries), ["local-lvm"])

    def test_lvmthin_with_images_kept(self) -> None:
        entries = [
            {"storage": "local-lvm-thin", "type": "lvmthin", "content": "images"},
        ]
        self.assertEqual(self._call(entries), ["local-lvm-thin"])

    def test_zfspool_with_images_kept(self) -> None:
        entries = [
            {"storage": "tank-zfs", "type": "zfspool", "content": "images,rootdir"},
        ]
        self.assertEqual(self._call(entries), ["tank-zfs"])

    def test_dir_with_images_and_other_content_kept(self) -> None:
        entries = [
            {"storage": "local", "type": "dir", "content": "images,iso,backup"},
        ]
        self.assertEqual(self._call(entries), ["local"])

    def test_dir_with_iso_only_excluded(self) -> None:
        entries = [
            {"storage": "iso-store", "type": "dir", "content": "iso"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_nfs_with_images_excluded_wrong_type(self) -> None:
        entries = [
            {"storage": "nfs-store", "type": "nfs", "content": "images"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_cephfs_excluded_wrong_type(self) -> None:
        entries = [
            {"storage": "ceph-store", "type": "cephfs", "content": "images"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_disabled_lvm_int_excluded(self) -> None:
        entries = [
            {"storage": "disabled-lvm", "type": "lvm", "content": "images", "disable": 1},
        ]
        self.assertEqual(self._call(entries), [])

    def test_disabled_lvm_str_excluded(self) -> None:
        entries = [
            {"storage": "disabled-lvm", "type": "lvm", "content": "images", "disable": "1"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_disabled_lvm_bool_excluded(self) -> None:
        entries = [
            {"storage": "disabled-lvm", "type": "lvm", "content": "images", "disable": True},
        ]
        self.assertEqual(self._call(entries), [])

    def test_full_representative_list(self) -> None:
        """Combined fixture covering all keep/exclude cases from the spec."""
        entries = [
            # Keep: lvm w/ images
            {"storage": "local-lvm", "type": "lvm", "content": "images,rootdir"},
            # Keep: lvmthin w/ images
            {"storage": "local-lvm-thin", "type": "lvmthin", "content": "images"},
            # Keep: zfspool w/ images
            {"storage": "tank-zfs", "type": "zfspool", "content": "images,rootdir"},
            # Keep: dir w/ images + iso + backup
            {"storage": "local", "type": "dir", "content": "images,iso,backup"},
            # Exclude: dir w/ iso only (no images)
            {"storage": "iso-store", "type": "dir", "content": "iso"},
            # Exclude: nfs w/ images (wrong type)
            {"storage": "nfs-data", "type": "nfs", "content": "images"},
            # Exclude: cephfs (wrong type)
            {"storage": "ceph-fs", "type": "cephfs", "content": "images"},
            # Exclude: lvm w/ images but disabled (int 1)
            {"storage": "old-lvm", "type": "lvm", "content": "images", "disable": 1},
        ]
        result = self._call(entries)
        self.assertEqual(result, ["local", "local-lvm", "local-lvm-thin", "tank-zfs"])

    def test_empty_entries_returns_empty(self) -> None:
        self.assertEqual(self._call([]), [])

    def test_result_is_sorted(self) -> None:
        entries = [
            {"storage": "zz-pool", "type": "lvm", "content": "images"},
            {"storage": "aa-pool", "type": "lvmthin", "content": "images"},
            {"storage": "mm-pool", "type": "zfspool", "content": "images"},
        ]
        result = self._call(entries)
        self.assertEqual(result, sorted(result))

    def test_duplicates_deduplicated(self) -> None:
        entries = [
            {"storage": "mypool", "type": "lvm", "content": "images"},
            {"storage": "mypool", "type": "lvm", "content": "images"},
        ]
        self.assertEqual(self._call(entries), ["mypool"])

    def test_content_whitespace_stripped(self) -> None:
        """Comma-split parts are stripped so ' images' still matches."""
        entries = [
            {"storage": "my-dir", "type": "dir", "content": "iso, images , backup"},
        ]
        self.assertEqual(self._call(entries), ["my-dir"])

    def test_missing_content_field_excluded(self) -> None:
        entries = [
            {"storage": "bare-lvm", "type": "lvm"},
        ]
        self.assertEqual(self._call(entries), [])

    def test_disable_zero_not_excluded(self) -> None:
        """disable=0 means enabled — entry should be kept."""
        entries = [
            {"storage": "enabled-lvm", "type": "lvm", "content": "images", "disable": 0},
        ]
        self.assertEqual(self._call(entries), ["enabled-lvm"])

    def test_disable_false_not_excluded(self) -> None:
        """disable=False means enabled — entry should be kept."""
        entries = [
            {"storage": "enabled-lvm2", "type": "lvm", "content": "images", "disable": False},
        ]
        self.assertEqual(self._call(entries), ["enabled-lvm2"])

    def test_custom_types_parameter(self) -> None:
        """Caller can restrict accepted types via the types parameter."""
        entries = [
            {"storage": "my-lvm", "type": "lvm", "content": "images"},
            {"storage": "my-dir", "type": "dir", "content": "images"},
        ]
        result = _integration.select_local_disk_pools(entries, types=("lvm",))
        self.assertEqual(result, ["my-lvm"])


if __name__ == "__main__":
    unittest.main()
