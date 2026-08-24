#!/usr/bin/env python3
"""Unit tests for scripts/_compiled.py (compiled-release pipeline helpers).

Run: python3 scripts/_compiled_test.py -v
Pure/offline — no network, no director, no filesystem writes.
"""

import os
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _compiled as c  # noqa: E402


class TestBasename(unittest.TestCase):
    def test_canonical(self):
        self.assertEqual(
            c.compiled_basename("bosh", "282.1.13", "ubuntu-noble", "1.383"),
            "bosh-282.1.13-ubuntu-noble-1.383.tgz",
        )

    def test_slugs_unsafe_chars(self):
        # '+' in a dev version must not break the filename.
        self.assertEqual(
            c.compiled_basename("bosh-proxmox-cpi", "0.1.0+dev.1", "ubuntu-noble", "1.383"),
            "bosh-proxmox-cpi-0.1.0-dev.1-ubuntu-noble-1.383.tgz",
        )

    def test_stemcell_encoded(self):
        # Different stemcell → different name (mismatch can't masquerade as match).
        a = c.compiled_basename("bosh", "282.1.13", "ubuntu-noble", "1.383")
        b = c.compiled_basename("bosh", "282.1.13", "ubuntu-noble", "1.333")
        self.assertNotEqual(a, b)

    def test_empty_field_raises(self):
        for args in (("", "1", "o", "s"), ("n", "", "o", "s"), ("n", "1", "", "s"), ("n", "1", "o", "")):
            with self.assertRaises(ValueError):
                c.compiled_basename(*args)


class TestFileRef(unittest.TestCase):
    def test_absolute(self):
        ref = c.file_ref("/srv/compiled", "bosh-1-o-s.tgz")
        self.assertEqual(ref, "file:///srv/compiled/bosh-1-o-s.tgz")

    def test_relative_made_absolute(self):
        ref = c.file_ref("compiled_releases", "x.tgz")
        self.assertTrue(ref.startswith("file://"))
        self.assertTrue(ref.endswith("/compiled_releases/x.tgz"))
        # no relative segment leaked
        self.assertNotIn("file://compiled_releases", ref)

    def test_empty_dir_raises(self):
        with self.assertRaises(ValueError):
            c.file_ref("", "x.tgz")


class TestS3(unittest.TestCase):
    def test_object_key_with_prefix(self):
        self.assertEqual(c.s3_object_key("releases/compiled", "x.tgz"), "releases/compiled/x.tgz")

    def test_object_key_strips_slashes(self):
        self.assertEqual(c.s3_object_key("/a/b/", "x.tgz"), "a/b/x.tgz")

    def test_object_key_no_prefix(self):
        self.assertEqual(c.s3_object_key("", "x.tgz"), "x.tgz")

    def test_https_path_style(self):
        url = c.s3_https_url("https://s3.lab.example", "bosh-compiled", "releases/x.tgz")
        self.assertEqual(url, "https://s3.lab.example/bosh-compiled/releases/x.tgz")

    def test_https_adds_scheme(self):
        url = c.s3_https_url("minio.lab:9000", "b", "x.tgz")
        self.assertEqual(url, "https://minio.lab:9000/b/x.tgz")

    def test_https_requires_endpoint_and_bucket(self):
        with self.assertRaises(ValueError):
            c.s3_https_url("", "b", "k")
        with self.assertRaises(ValueError):
            c.s3_https_url("e", "", "k")


class TestStoreConfig(unittest.TestCase):
    def test_default_is_file(self):
        cfg = c.store_config_from_env({}, "/repo/compiled_releases")
        self.assertEqual(cfg.kind, "file")
        self.assertEqual(cfg.directory, "/repo/compiled_releases")

    def test_file_dir_override(self):
        cfg = c.store_config_from_env({"COMPILED_RELEASES_DIR": "/data/cr"}, "/default")
        self.assertEqual(cfg.directory, "/data/cr")

    def test_file_reference(self):
        cfg = c.store_config_from_env({}, "/repo/cr")
        self.assertEqual(cfg.reference("bosh-1-o-s.tgz"), "file:///repo/cr/bosh-1-o-s.tgz")

    def test_s3_requires_endpoint_bucket(self):
        with self.assertRaises(ValueError):
            c.store_config_from_env({"COMPILED_RELEASES_STORE": "s3"}, "/d")
        with self.assertRaises(ValueError):
            c.store_config_from_env(
                {"COMPILED_RELEASES_STORE": "s3", "COMPILED_RELEASES_S3_ENDPOINT": "e"}, "/d"
            )

    def test_s3_full(self):
        cfg = c.store_config_from_env(
            {
                "COMPILED_RELEASES_STORE": "s3",
                "COMPILED_RELEASES_S3_ENDPOINT": "https://s3.lab",
                "COMPILED_RELEASES_S3_BUCKET": "cr",
                "COMPILED_RELEASES_S3_PREFIX": "compiled",
            },
            "/d",
        )
        self.assertEqual(cfg.kind, "s3")
        self.assertEqual(cfg.reference("bosh-1-o-s.tgz"), "https://s3.lab/cr/compiled/bosh-1-o-s.tgz")

    def test_unknown_store_raises(self):
        with self.assertRaises(ValueError):
            c.store_config_from_env({"COMPILED_RELEASES_STORE": "gcs"}, "/d")


class TestOpsGeneration(unittest.TestCase):
    def _ops(self):
        return c.compiled_releases_ops(
            [
                c.CompiledRelease("bosh", "282.1.13", "file:///cr/bosh.tgz", "sha256:aaa"),
                c.CompiledRelease("bosh-proxmox-cpi", "0.1.0", "https://s3/cr/cpi.tgz", "sha256:bbb"),
            ]
        )

    def test_replaces_each_release(self):
        ops = self._ops()
        self.assertIn("path: /releases/name=bosh?", ops)
        self.assertIn("path: /releases/name=bosh-proxmox-cpi?", ops)

    def test_carries_url_and_sha(self):
        ops = self._ops()
        self.assertIn('url: "file:///cr/bosh.tgz"', ops)
        self.assertIn('sha1: "sha256:aaa"', ops)
        self.assertIn('url: "https://s3/cr/cpi.tgz"', ops)
        self.assertIn("version: \"282.1.13\"", ops)

    def test_url_with_hash_not_truncated(self):
        # A '#' in a file:// path must survive (quoted), and parse back whole.
        try:
            import yaml  # type: ignore
        except ImportError:
            self.skipTest("pyyaml not available")
        ops = c.compiled_releases_ops(
            [c.CompiledRelease("bosh", "1", "file:///srv/c#d/bosh.tgz", "sha256:aaa")]
        )
        docs = yaml.safe_load(ops)
        self.assertEqual(docs[0]["value"]["url"], "file:///srv/c#d/bosh.tgz")

    def test_is_valid_yaml_when_pyyaml_present(self):
        try:
            import yaml  # type: ignore
        except ImportError:
            self.skipTest("pyyaml not available")
        docs = yaml.safe_load(self._ops())
        self.assertEqual(len(docs), 2)
        self.assertEqual(docs[0]["path"], "/releases/name=bosh?")
        self.assertEqual(docs[0]["value"]["sha1"], "sha256:aaa")

    def test_empty_raises(self):
        with self.assertRaises(ValueError):
            c.compiled_releases_ops([])

    def test_duplicate_raises(self):
        with self.assertRaises(ValueError):
            c.compiled_releases_ops(
                [c.CompiledRelease("bosh", "1", "u", "s"), c.CompiledRelease("bosh", "2", "u", "s")]
            )

    def test_empty_field_raises(self):
        with self.assertRaises(ValueError):
            c.compiled_releases_ops([c.CompiledRelease("bosh", "1", "", "s")])


class TestStemcellMarker(unittest.TestCase):
    def _ops(self, stemcell):
        return c.compiled_releases_ops(
            [c.CompiledRelease("bosh", "282.1.13", "file:///cr/bosh.tgz", "sha256:aaa")],
            stemcell=stemcell,
        )

    def test_marker_written_and_read_roundtrip(self):
        ops = self._ops("ubuntu-noble/1.383")
        self.assertIn("# stemcell: ubuntu-noble/1.383", ops)
        self.assertEqual(c.stemcell_from_ops(ops), "ubuntu-noble/1.383")

    def test_no_marker_when_absent(self):
        ops = self._ops(None)
        self.assertIsNone(c.stemcell_from_ops(ops))

    def test_marker_is_a_comment(self):
        # The marker must not become a YAML op (it is a comment line).
        try:
            import yaml  # type: ignore
        except ImportError:
            self.skipTest("pyyaml not available")
        docs = yaml.safe_load(self._ops("ubuntu-noble/1.383"))
        self.assertEqual(len(docs), 1)  # one release op, marker is inert

    def test_reader_handles_missing(self):
        self.assertIsNone(c.stemcell_from_ops("---\nfoo: bar\n"))


class TestLocalPaths(unittest.TestCase):
    def test_extracts_file_urls(self):
        ops = c.compiled_releases_ops(
            [
                c.CompiledRelease("bosh", "1", "file:///srv/cr/bosh.tgz", "sha256:a"),
                c.CompiledRelease("cpi", "2", "https://s3/cr/cpi.tgz", "sha256:b"),
            ]
        )
        # only the file:// ref is returned; the https one is skipped
        self.assertEqual(c.local_paths_from_ops(ops), ["/srv/cr/bosh.tgz"])

    def test_no_file_urls(self):
        ops = c.compiled_releases_ops([c.CompiledRelease("cpi", "2", "https://s3/x.tgz", "sha256:b")])
        self.assertEqual(c.local_paths_from_ops(ops), [])

    def test_handles_unquoted(self):
        text = "- type: replace\n  value:\n    url: file:///a/b.tgz\n"
        self.assertEqual(c.local_paths_from_ops(text), ["/a/b.tgz"])


if __name__ == "__main__":
    unittest.main()
