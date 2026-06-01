#!/usr/bin/env python3
"""Unit tests for scripts/_artifacts.py (cpi-artifacts VM + RustFS helpers).

Run: python3 scripts/_artifacts_test.py -v

Pure/offline — no network, no PVE, no SSH. The socket probe and the SSH
orchestration are covered by the integration harness, not here. Each test
exercises a pure function or a tmp-dir file operation; ENVS_DIR is redirected to
a TemporaryDirectory so state-file tests never touch the repo tree.
"""

from __future__ import annotations

import dataclasses
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _artifacts as A  # noqa: E402


def _getters(env: "dict[str, str]", vars_: "dict[str, str]"):
    return (lambda k: env.get(k, "")), (lambda k: vars_.get(k, ""))


def _cfg(**over) -> "A.ArtifactsConfig":
    getenv, getvar = _getters({}, {
        "pve_node": "pve", "pve_vm_storage": "local-lvm",
        "internal_gw": "172.31.0.1", "internal_cidr": "172.31.0.0/24",
        "pve_network_bridge": "cpitest0",
    })
    cfg = A.build_config("cpitest", getenv=getenv, getvar=getvar, access_key="ACC", secret_key="SEC")
    return dataclasses.replace(cfg, **over)


class TestCredentialRule(unittest.TestCase):
    def test_passthrough_when_both_set(self):
        self.assertEqual(A.resolve_credentials("AKIA", "SECRET"), ("AKIA", "SECRET"))

    def test_regenerate_when_either_missing(self):
        seq = iter(["aaaa", "bbbb", "cccc", "dddd"])
        gen = lambda n: next(seq)  # noqa: E731
        self.assertEqual(A.resolve_credentials("AKIA", "", gen=gen), ("aaaa", "bbbb"))
        self.assertEqual(A.resolve_credentials("", "SECRET", gen=gen), ("cccc", "dddd"))

    def test_default_generator_hex_and_sized(self):
        access, secret = A.resolve_credentials("", "")
        self.assertEqual(len(access), 20)
        self.assertEqual(len(secret), 40)
        int(access, 16)  # hex-decodable -> EnvironmentFile-safe
        int(secret, 16)


class TestEndpointAndTLS(unittest.TestCase):
    def test_http_when_disabled(self):
        self.assertEqual(A.endpoint_url("172.31.0.11", 9000, "disabled"), "http://172.31.0.11:9000")

    def test_https_when_self_signed(self):
        self.assertEqual(A.endpoint_url("10.0.0.5", 9443, "self-signed"), "https://10.0.0.5:9443")

    def test_unknown_mode_raises(self):
        with self.assertRaises(ValueError):
            A.normalize_tls_mode("mutual")

    def test_empty_mode_defaults_disabled(self):
        self.assertEqual(A.normalize_tls_mode(""), "disabled")


class TestBucketParsing(unittest.TestCase):
    def test_space_and_comma_dedup(self):
        self.assertEqual(A.parse_buckets("a, b  a c"), ["a", "b", "c"])

    def test_empty(self):
        self.assertEqual(A.parse_buckets("   "), [])


class TestConfigResolution(unittest.TestCase):
    def test_defaults(self):
        getenv, getvar = _getters({}, {})
        cfg = A.build_config("cpitest", getenv=getenv, getvar=getvar)
        self.assertEqual(cfg.ip, A.DEFAULT_IP)
        self.assertEqual((cfg.cores, cfg.memory_mib, cfg.disk_gib), (2, 4096, 100))
        self.assertEqual((cfg.s3_port, cfg.console_port), (9000, 9001))
        self.assertEqual(cfg.tls_mode, "disabled")
        self.assertEqual(cfg.buckets, A.DEFAULT_BUCKETS)
        self.assertEqual(cfg.endpoint, f"http://{A.DEFAULT_IP}:9000")

    def test_env_wins_over_vars(self):
        getenv, getvar = _getters(
            {"ARTIFACTS_VM_MEMORY": "8192", "ARTIFACTS_VM_IP": "172.31.0.77"},
            {"artifacts_vm_memory": "2048", "artifacts_vm_ip": "172.31.0.99"},
        )
        cfg = A.build_config("cpitest", getenv=getenv, getvar=getvar)
        self.assertEqual(cfg.memory_mib, 8192)
        self.assertEqual(cfg.ip, "172.31.0.77")

    def test_vars_used_when_env_absent(self):
        getenv, getvar = _getters({}, {
            "artifacts_vm_cpu": "4", "internal_gw": "172.31.0.1",
            "internal_cidr": "172.31.0.0/24", "pve_node": "pve",
            "pve_vm_storage": "local-lvm", "pve_network_bridge": "cpitest0",
        })
        cfg = A.build_config("cpitest", getenv=getenv, getvar=getvar)
        self.assertEqual(cfg.cores, 4)
        self.assertEqual(cfg.gateway, "172.31.0.1")
        self.assertEqual(cfg.cidr_mask, 24)
        self.assertEqual(cfg.node, "pve")
        self.assertEqual(cfg.vm_storage, "local-lvm")
        self.assertEqual(cfg.bridge, "cpitest0")
        self.assertEqual(cfg.ipconfig0, "ip=172.31.0.11/24,gw=172.31.0.1")

    def test_reuses_supplied_credentials(self):
        getenv, getvar = _getters({}, {})
        cfg = A.build_config("cpitest", getenv=getenv, getvar=getvar, access_key="K", secret_key="S")
        self.assertEqual((cfg.access_key, cfg.secret_key), ("K", "S"))

    def test_invalid_int_raises(self):
        getenv, getvar = _getters({"ARTIFACTS_VM_CPU": "two"}, {})
        with self.assertRaises(ValueError):
            A.build_config("cpitest", getenv=getenv, getvar=getvar)


class TestStateIO(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self._patch = mock.patch.object(A, "ENVS_DIR", Path(self._tmp.name))
        self._patch.start()

    def tearDown(self):
        self._patch.stop()
        self._tmp.cleanup()

    def test_roundtrip(self):
        cfg = _cfg()
        A.write_state("cpitest", A.state_from_config(cfg, vmid=901))
        back = A.read_state("cpitest")
        self.assertEqual(back["vmid"], 901)
        self.assertEqual(back["access_key"], "ACC")
        self.assertEqual(back["endpoint"], f"http://{A.DEFAULT_IP}:9000")
        self.assertEqual(back["buckets"], list(A.DEFAULT_BUCKETS))
        self.assertTrue(A.delete_state("cpitest"))
        self.assertIsNone(A.read_state("cpitest"))
        self.assertFalse(A.delete_state("cpitest"))

    def test_absent(self):
        self.assertIsNone(A.read_state("nope"))

    def test_corrupt(self):
        p = A.state_path("cpitest")
        p.parent.mkdir(parents=True)
        p.write_text("{not json", encoding="utf-8")
        self.assertIsNone(A.read_state("cpitest"))


class TestProbeAndStoreConfig(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self._patch = mock.patch.object(A, "ENVS_DIR", Path(self._tmp.name))
        self._patch.start()

    def tearDown(self):
        self._patch.stop()
        self._tmp.cleanup()

    def test_probe_empty_host_false(self):
        self.assertFalse(A.probe_tcp("", 9000))

    def test_probe_unreachable_false(self):
        # 203.0.113.0/24 is TEST-NET-3 (RFC 5737) — guaranteed unroutable.
        self.assertFalse(A.probe_tcp("203.0.113.1", 9, timeout=0.2))

    def test_online_false_without_state(self):
        self.assertFalse(A.artifacts_online("cpitest"))

    def test_store_config_none_without_state(self):
        self.assertIsNone(A.artifacts_store_config("cpitest", "pve-cpi-bosh"))

    def test_store_config_from_state(self):
        A.write_state("cpitest", {
            "ip": "172.31.0.11", "s3_port": 9000,
            "endpoint": "http://172.31.0.11:9000", "region": "us-east-1",
            "access_key": "K", "secret_key": "S",
        })
        sc = A.artifacts_store_config("cpitest", "pve-cpi-cf")
        self.assertIsNotNone(sc)
        self.assertEqual(sc.kind, "s3")
        self.assertEqual(sc.s3_endpoint, "http://172.31.0.11:9000")
        self.assertEqual(sc.s3_bucket, "pve-cpi-cf")
        self.assertEqual(sc.s3_region, "us-east-1")
        self.assertEqual(sc.reference("x.tgz"), "http://172.31.0.11:9000/pve-cpi-cf/x.tgz")


class TestCompiledStoreOverride(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self._patch = mock.patch.object(A, "ENVS_DIR", Path(self._tmp.name))
        self._patch.start()

    def tearDown(self):
        self._patch.stop()
        self._tmp.cleanup()

    def _write_online_state(self):
        A.write_state("cpitest", {
            "ip": "172.31.0.11", "s3_port": 9000,
            "endpoint": "http://172.31.0.11:9000", "region": "us-east-1",
            "access_key": "K", "secret_key": "S",
        })

    def test_none_when_env_unselected(self):
        self._write_online_state()
        with mock.patch.object(A, "artifacts_online", return_value=True):
            self.assertIsNone(A.compiled_store_override({}, "pve-cpi-bosh"))

    def test_none_when_explicit_store_override(self):
        self._write_online_state()
        with mock.patch.object(A, "artifacts_online", return_value=True):
            self.assertIsNone(A.compiled_store_override(
                {"BOSH_PVE_ENV": "cpitest", "COMPILED_RELEASES_STORE": "file"}, "pve-cpi-bosh"))
            self.assertIsNone(A.compiled_store_override(
                {"BOSH_PVE_ENV": "cpitest", "COMPILED_RELEASES_S3_ENDPOINT": "https://x"}, "pve-cpi-bosh"))

    def test_none_when_offline(self):
        self._write_online_state()
        with mock.patch.object(A, "artifacts_online", return_value=False):
            self.assertIsNone(A.compiled_store_override({"BOSH_PVE_ENV": "cpitest"}, "pve-cpi-bosh"))

    def test_store_config_when_selected_and_online(self):
        self._write_online_state()
        with mock.patch.object(A, "artifacts_online", return_value=True):
            sc = A.compiled_store_override({"BOSH_PVE_ENV": "cpitest"}, "pve-cpi-bosh")
        self.assertIsNotNone(sc)
        self.assertEqual(sc.kind, "s3")
        self.assertEqual(sc.s3_bucket, "pve-cpi-bosh")
        self.assertEqual(sc.s3_endpoint, "http://172.31.0.11:9000")


class TestAwsEnv(unittest.TestCase):
    def test_sets_path_style_and_creds(self):
        env = A.aws_env({"access_key": "K", "secret_key": "S", "region": "us-east-1"})
        self.assertEqual(env["AWS_ACCESS_KEY_ID"], "K")
        self.assertEqual(env["AWS_SECRET_ACCESS_KEY"], "S")
        self.assertEqual(env["AWS_S3_ADDRESSING_STYLE"], "path")
        self.assertEqual(env["AWS_EC2_METADATA_DISABLED"], "true")


class TestRenderers(unittest.TestCase):
    def test_qm_create_sizing_and_network(self):
        s = A.render_qm_create_script(_cfg(), vmid=901, image_path="/tmp/noble.img", sshkey_path="/tmp/k.pub")
        self.assertIn("--memory 4096", s)
        self.assertIn("--cores 2", s)
        self.assertIn("--net0 virtio,bridge=cpitest0", s)
        self.assertIn('qm resize "$VMID" scsi0 100G', s)
        self.assertIn("--ipconfig0 ip=172.31.0.11/24,gw=172.31.0.1", s)
        self.assertIn('import-from="$IMG"', s)
        self.assertNotIn("--scsi1", s)

    def test_qm_create_adds_data_disk(self):
        s = A.render_qm_create_script(_cfg(data_disk_gib=200), vmid=901, image_path="/i.img", sshkey_path="/k.pub")
        self.assertIn("--scsi1 local-lvm:200", s)

    def test_install_disabled_tls_http(self):
        s = A.render_install_script(_cfg())
        self.assertIn("RUSTFS_ADDRESS=:9000", s)
        self.assertIn("RUSTFS_CONSOLE_ADDRESS=:9001", s)
        self.assertIn("RUSTFS_ACCESS_KEY=ACC", s)
        self.assertIn("RUSTFS_SECRET_KEY=SEC", s)
        self.assertNotIn("RUSTFS_TLS_PATH", s)
        self.assertIn('EP=http"://127.0.0.1:9000"', s)
        self.assertIn('s3 mb "s3://$b"', s)
        self.assertIn("pve-cpi-bosh", s)
        self.assertIn("pve-cpi-cf", s)
        self.assertNotIn("--no-verify-ssl", s)
        self.assertNotIn("zpool", s)

    def test_install_self_signed_tls_https(self):
        s = A.render_install_script(_cfg(tls_mode="self-signed"))
        self.assertIn("RUSTFS_TLS_PATH=/opt/rustfs/tls", s)
        self.assertIn('EP=https"://127.0.0.1:9000"', s)
        self.assertIn("--no-verify-ssl", s)
        self.assertIn("openssl req -x509", s)
        self.assertIn("subjectAltName=IP:172.31.0.11", s)

    def test_install_data_disk_lays_zfs(self):
        s = A.render_install_script(_cfg(data_disk_gib=200))
        self.assertIn("zpool create", s)
        self.assertIn("zfsutils-linux", s)

    def test_image_fetch_guards_presence(self):
        s = A.render_image_fetch_script("https://x/noble.img", "/var/tmp/noble.img")
        self.assertIn('if [ -s "$IMG" ]', s)
        self.assertIn("curl -fL", s)


if __name__ == "__main__":
    unittest.main()
