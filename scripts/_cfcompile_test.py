#!/usr/bin/env python3
"""Offline unit tests for _cfcompile helper functions.

Run with:
    python3 scripts/_cfcompile_test.py

Stdlib only; no director, PVE, or bosh CLI required.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _cfcompile as cfc
import _compiled


class ParseReleaseTargetsTests(unittest.TestCase):
    def test_normalizes_entries(self) -> None:
        rels = [
            {"name": "capi", "version": "1.2.3", "url": "https://x/capi.tgz", "sha1": "abc"},
            {"name": "routing", "version": "0.9", "url": "https://x/routing.tgz"},
        ]
        got = cfc.parse_release_targets(rels)
        self.assertEqual(got[0]["name"], "capi")
        self.assertEqual(got[0]["sha1"], "abc")
        self.assertEqual(got[1]["sha1"], "")  # optional

    def test_missing_name_or_version_raises(self) -> None:
        with self.assertRaises(ValueError):
            cfc.parse_release_targets([{"version": "1.0", "url": "u"}])
        with self.assertRaises(ValueError):
            cfc.parse_release_targets([{"name": "x", "url": "u"}])

    def test_empty_raises(self) -> None:
        with self.assertRaises(ValueError):
            cfc.parse_release_targets([])

    def test_skips_non_dict(self) -> None:
        got = cfc.parse_release_targets([None, 3, {"name": "a", "version": "1"}])
        self.assertEqual(len(got), 1)


class CompilationManifestTests(unittest.TestCase):
    def test_contains_releases_stemcell_and_no_instance_groups(self) -> None:
        targets = [{"name": "capi", "version": "1.2.3"}, {"name": "routing", "version": "0.9"}]
        m = cfc.compilation_manifest("compile-cf", targets, "ubuntu-noble", "1.383")
        self.assertIn("name: compile-cf", m)
        self.assertIn("- name: capi", m)
        self.assertIn('version: "1.2.3"', m)
        self.assertIn("- name: routing", m)
        self.assertIn("os: ubuntu-noble", m)
        self.assertIn('version: "1.383"', m)
        self.assertIn("instance_groups: []", m)


class CacheModeOpsTests(unittest.TestCase):
    def test_marker_roundtrip(self) -> None:
        text = cfc.cache_mode_ops("ubuntu-noble/1.383")
        self.assertEqual(_compiled.stemcell_from_ops(text), "ubuntu-noble/1.383")
        self.assertIn(cfc.MODE_MARKER + cfc.CACHE_MODE, text)
        # Cache sentinel must NOT contain release pins (it is never layered).
        self.assertNotIn("/releases/name=", text)


class PrecompileModeTests(unittest.TestCase):
    def test_real_when_release_ops_present(self) -> None:
        ops = _compiled.compiled_releases_ops(
            [_compiled.CompiledRelease("capi", "1.2.3", "https://x/capi.tgz", "sha256:ab")],
            stemcell="ubuntu-noble/1.383",
        )
        self.assertEqual(cfc.precompile_mode(ops), cfc.STORE_OPS_MODE)

    def test_cache_when_marker_present(self) -> None:
        self.assertEqual(
            cfc.precompile_mode(cfc.cache_mode_ops("ubuntu-noble/1.383")), cfc.CACHE_MODE
        )

    def test_none_when_neither(self) -> None:
        self.assertIsNone(cfc.precompile_mode("---\n# nothing here\n"))


class PrecompileStateTests(unittest.TestCase):
    def test_absent_on_empty(self) -> None:
        self.assertEqual(cfc.precompile_state("", "ubuntu-noble/1.383"), "absent")
        self.assertEqual(cfc.precompile_state("   ", "ubuntu-noble/1.383"), "absent")

    def test_mismatch_on_different_stemcell(self) -> None:
        text = cfc.cache_mode_ops("ubuntu-noble/1.333")
        self.assertEqual(cfc.precompile_state(text, "ubuntu-noble/1.383"), "mismatch")

    def test_cache_mode_match(self) -> None:
        text = cfc.cache_mode_ops("ubuntu-noble/1.383")
        self.assertEqual(cfc.precompile_state(text, "ubuntu-noble/1.383"), cfc.CACHE_MODE)

    def test_store_ops_match(self) -> None:
        ops = _compiled.compiled_releases_ops(
            [_compiled.CompiledRelease("capi", "1.2.3", "https://x/capi.tgz", "sha256:ab")],
            stemcell="ubuntu-noble/1.383",
        )
        self.assertEqual(cfc.precompile_state(ops, "ubuntu-noble/1.383"), cfc.STORE_OPS_MODE)


if __name__ == "__main__":
    unittest.main()
