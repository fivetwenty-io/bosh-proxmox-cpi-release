#!/usr/bin/env python3
"""Unit tests for scripts/_pve_verify.py helpers.

Loading strategy
----------------
_pve_verify.py has a shebang line and no .py extension issue (it IS named
_pve_verify.py), so a standard ``sys.path`` insert + ``import _pve_verify``
works. The module uses only stdlib and has no side-effects at import time,
so no stubbing is required before import.

HTTP transport is stubbed via ``unittest.mock.patch`` on
``urllib.request.urlopen`` and ``urllib.request.Request`` where needed.  Each
test wires its own Response/Error so the real network is never contacted.

Run:
    python3 scripts/_pve_verify_test.py
"""

from __future__ import annotations

import base64
import gzip
import http.client
import io
import json
import ssl
import sys
import tempfile
import unittest
import unittest.mock
import urllib.error
import urllib.request
from pathlib import Path

# Make scripts/ importable regardless of working directory.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _pve_verify
from _pve_verify import PVEVerifier, PVEVerifyError, parse_stemcell_cid, parse_stemcell_path_cid


# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------

def _make_response(payload: dict, status: int = 200) -> unittest.mock.MagicMock:
    """Build a mock that looks like the object returned by urlopen."""
    body = json.dumps(payload).encode("utf-8")
    resp = unittest.mock.MagicMock()
    resp.read.return_value = body
    resp.status = status
    resp.__enter__ = lambda s: s
    resp.__exit__ = unittest.mock.MagicMock(return_value=False)
    return resp


def _make_http_error(code: int, reason: str = "Error") -> urllib.error.HTTPError:
    return urllib.error.HTTPError(url="http://x", code=code, msg=reason, hdrs=None, fp=None)


def _token_config(**overrides) -> dict:
    base = {
        "host": "pve.example.com",
        "port": 8006,
        "node": "pve1",
        "api_token": "root@pam!mytoken=secret",
        "verify_ssl": False,
    }
    base.update(overrides)
    return base


def _password_config(**overrides) -> dict:
    base = {
        "host": "pve.example.com",
        "port": 8006,
        "node": "pve1",
        "user": "root",
        "realm": "pam",
        "password": "secret",
        "verify_ssl": False,
    }
    base.update(overrides)
    return base


# ---------------------------------------------------------------------------
# PVEVerifier construction
# ---------------------------------------------------------------------------

class TestPVEVerifierConstruction(unittest.TestCase):

    def test_token_auth_constructed(self) -> None:
        v = PVEVerifier(_token_config())
        self.assertEqual(v.host, "pve.example.com")
        self.assertEqual(v.port, 8006)
        self.assertEqual(v.node, "pve1")

    def test_password_auth_constructed(self) -> None:
        v = PVEVerifier(_password_config())
        self.assertEqual(v.host, "pve.example.com")

    def test_missing_host_raises(self) -> None:
        cfg = _token_config(host="")
        with self.assertRaises(PVEVerifyError) as ctx:
            PVEVerifier(cfg)
        self.assertIn("host", str(ctx.exception))

    def test_missing_both_credentials_raises(self) -> None:
        cfg = {
            "host": "pve.example.com",
            "port": 8006,
            "node": "pve1",
        }
        with self.assertRaises(PVEVerifyError):
            PVEVerifier(cfg)

    def test_from_config_file_happy(self) -> None:
        cfg = _token_config()
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
            json.dump(cfg, f)
            path = f.name
        v = PVEVerifier.from_config_file(path)
        self.assertEqual(v.host, "pve.example.com")

    def test_from_config_file_missing_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            PVEVerifier.from_config_file("/nonexistent/path/cpi.json")

    def test_from_config_file_invalid_json_raises(self) -> None:
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
            f.write("{not valid json")
            path = f.name
        with self.assertRaises(PVEVerifyError):
            PVEVerifier.from_config_file(path)

    def test_from_config_file_not_object_raises(self) -> None:
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
            json.dump([1, 2, 3], f)
            path = f.name
        with self.assertRaises(PVEVerifyError):
            PVEVerifier.from_config_file(path)

    def test_verify_ssl_true_creates_default_context(self) -> None:
        cfg = _token_config(verify_ssl=True)
        v = PVEVerifier(cfg)
        self.assertTrue(v.verify_ssl)

    def test_default_port_applied_when_missing(self) -> None:
        cfg = {
            "host": "pve.example.com",
            "api_token": "root@pam!tok=x",
        }
        v = PVEVerifier(cfg)
        self.assertEqual(v.port, 8006)


# ---------------------------------------------------------------------------
# vm_exists
# ---------------------------------------------------------------------------

class TestVMExists(unittest.TestCase):
    """Tests for PVEVerifier.vm_exists."""

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def test_happy_path_vm_found(self) -> None:
        v = self._verifier()
        payload = {"data": [{"vmid": 101, "name": "test-vm"}]}
        with unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload)):
            self.assertTrue(v.vm_exists(101))

    def test_happy_path_vm_not_found(self) -> None:
        v = self._verifier()
        payload = {"data": [{"vmid": 200, "name": "other-vm"}]}
        with unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload)):
            self.assertFalse(v.vm_exists(101))

    def test_happy_path_empty_list(self) -> None:
        v = self._verifier()
        payload = {"data": []}
        with unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload)):
            self.assertFalse(v.vm_exists(101))

    def test_vmid_string_matches_int_from_api(self) -> None:
        """vm_exists accepts str vmid and matches against API's integer vmid."""
        v = self._verifier()
        payload = {"data": [{"vmid": 101}]}
        with unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload)):
            self.assertTrue(v.vm_exists("101"))

    def test_auth_401_raises_pve_verify_error(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.vm_exists(101)
            self.assertIn("401", str(ctx.exception))

    def test_not_found_404_raises_pve_verify_error(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(404, "Not Found")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.vm_exists(101)
            self.assertIn("404", str(ctx.exception))

    def test_malformed_json_raises_pve_verify_error(self) -> None:
        """Non-JSON body from urlopen raises PVEVerifyError wrapping ValueError."""
        v = self._verifier()
        bad_resp = unittest.mock.MagicMock()
        bad_resp.read.return_value = b"this is not json{{{{"
        bad_resp.__enter__ = lambda s: s
        bad_resp.__exit__ = unittest.mock.MagicMock(return_value=False)
        with unittest.mock.patch("urllib.request.urlopen", return_value=bad_resp):
            with self.assertRaises(PVEVerifyError):
                v.vm_exists(101)

    def test_missing_node_raises_pve_verify_error(self) -> None:
        cfg = _token_config(node="")
        v = PVEVerifier(cfg)
        with self.assertRaises(PVEVerifyError) as ctx:
            v.vm_exists(101)
        self.assertIn("node", str(ctx.exception).lower())


# ---------------------------------------------------------------------------
# vnet_exists
# ---------------------------------------------------------------------------

class TestVNetExists(unittest.TestCase):
    """Tests for PVEVerifier.vnet_exists."""

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def _urlopen_with(self, vnet_list: list) -> unittest.mock.MagicMock:
        payload = {"data": vnet_list}
        return unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload))

    def test_happy_path_found(self) -> None:
        v = self._verifier()
        with self._urlopen_with([{"vnet": "itvnet"}]):
            self.assertTrue(v.vnet_exists("itvnet"))

    def test_happy_path_not_found(self) -> None:
        v = self._verifier()
        with self._urlopen_with([{"vnet": "othervnet"}]):
            self.assertFalse(v.vnet_exists("itvnet"))

    def test_happy_path_empty_list(self) -> None:
        v = self._verifier()
        with self._urlopen_with([]):
            self.assertFalse(v.vnet_exists("itvnet"))

    def test_auth_401_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.vnet_exists("itvnet")
            self.assertIn("401", str(ctx.exception))

    def test_not_found_404_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(404, "Not Found")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.vnet_exists("itvnet")
            self.assertIn("404", str(ctx.exception))

    def test_malformed_json_raises(self) -> None:
        v = self._verifier()
        bad = unittest.mock.MagicMock()
        bad.read.return_value = b"<html>not json</html>"
        bad.__enter__ = lambda s: s
        bad.__exit__ = unittest.mock.MagicMock(return_value=False)
        with unittest.mock.patch("urllib.request.urlopen", return_value=bad):
            with self.assertRaises(PVEVerifyError):
                v.vnet_exists("itvnet")

    def test_multiple_vnets_correct_match(self) -> None:
        v = self._verifier()
        data = [{"vnet": "vnet1"}, {"vnet": "vnet2"}, {"vnet": "itvnet"}]
        with self._urlopen_with(data):
            self.assertTrue(v.vnet_exists("itvnet"))


# ---------------------------------------------------------------------------
# zone_exists
# ---------------------------------------------------------------------------

class TestZoneExists(unittest.TestCase):
    """Tests for PVEVerifier.zone_exists."""

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def _urlopen_with(self, zone_list: list) -> unittest.mock.MagicMock:
        payload = {"data": zone_list}
        return unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload))

    def test_happy_path_found(self) -> None:
        v = self._verifier()
        with self._urlopen_with([{"zone": "itzone", "type": "simple"}]):
            self.assertTrue(v.zone_exists("itzone"))

    def test_happy_path_not_found(self) -> None:
        v = self._verifier()
        with self._urlopen_with([{"zone": "otherzone", "type": "simple"}]):
            self.assertFalse(v.zone_exists("itzone"))

    def test_happy_path_empty_list(self) -> None:
        v = self._verifier()
        with self._urlopen_with([]):
            self.assertFalse(v.zone_exists("itzone"))

    def test_auth_401_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.zone_exists("itzone")
            self.assertIn("401", str(ctx.exception))

    def test_not_found_404_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(404, "Not Found")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.zone_exists("itzone")
            self.assertIn("404", str(ctx.exception))

    def test_malformed_json_raises(self) -> None:
        v = self._verifier()
        bad = unittest.mock.MagicMock()
        bad.read.return_value = b"}}broken json{{"
        bad.__enter__ = lambda s: s
        bad.__exit__ = unittest.mock.MagicMock(return_value=False)
        with unittest.mock.patch("urllib.request.urlopen", return_value=bad):
            with self.assertRaises(PVEVerifyError):
                v.zone_exists("itzone")


# ---------------------------------------------------------------------------
# subnet_present
# ---------------------------------------------------------------------------

class TestSubnetPresent(unittest.TestCase):
    """Tests for PVEVerifier.subnet_present."""

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def _urlopen_with(self, subnet_list: list) -> unittest.mock.MagicMock:
        payload = {"data": subnet_list}
        return unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload))

    def test_happy_path_found_by_cidr(self) -> None:
        v = self._verifier()
        data = [{"cidr": "10.250.0.0/24", "subnet": "itzone-10.250.0.0-24"}]
        with self._urlopen_with(data):
            self.assertTrue(v.subnet_present("itvnet", "10.250.0.0/24"))

    def test_happy_path_found_by_subnet_id(self) -> None:
        """Match falls back to subnet id containing the CIDR with '-' substitution."""
        v = self._verifier()
        data = [{"cidr": "", "subnet": "itzone-10.250.0.0-24"}]
        with self._urlopen_with(data):
            self.assertTrue(v.subnet_present("itvnet", "10.250.0.0/24"))

    def test_happy_path_not_found(self) -> None:
        v = self._verifier()
        data = [{"cidr": "192.168.1.0/24", "subnet": "zone-192.168.1.0-24"}]
        with self._urlopen_with(data):
            self.assertFalse(v.subnet_present("itvnet", "10.250.0.0/24"))

    def test_happy_path_empty_list(self) -> None:
        v = self._verifier()
        with self._urlopen_with([]):
            self.assertFalse(v.subnet_present("itvnet", "10.250.0.0/24"))

    def test_auth_401_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.subnet_present("itvnet", "10.250.0.0/24")
            self.assertIn("401", str(ctx.exception))

    def test_not_found_404_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(404, "Not Found")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.subnet_present("itvnet", "10.250.0.0/24")
            self.assertIn("404", str(ctx.exception))

    def test_malformed_json_raises(self) -> None:
        v = self._verifier()
        bad = unittest.mock.MagicMock()
        bad.read.return_value = b"not-json-at-all"
        bad.__enter__ = lambda s: s
        bad.__exit__ = unittest.mock.MagicMock(return_value=False)
        with unittest.mock.patch("urllib.request.urlopen", return_value=bad):
            with self.assertRaises(PVEVerifyError):
                v.subnet_present("itvnet", "10.250.0.0/24")


# ---------------------------------------------------------------------------
# bridge_exists
# ---------------------------------------------------------------------------

class TestBridgeExists(unittest.TestCase):
    """Tests for PVEVerifier.bridge_exists."""

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def _urlopen_with(self, iface_list: list) -> unittest.mock.MagicMock:
        payload = {"data": iface_list}
        return unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload))

    def test_happy_path_found(self) -> None:
        v = self._verifier()
        with self._urlopen_with([{"iface": "vmbr0", "type": "bridge"}]):
            self.assertTrue(v.bridge_exists("vmbr0"))

    def test_happy_path_not_found(self) -> None:
        v = self._verifier()
        with self._urlopen_with([{"iface": "eth0", "type": "eth"}]):
            self.assertFalse(v.bridge_exists("vmbr0"))

    def test_happy_path_empty_list(self) -> None:
        v = self._verifier()
        with self._urlopen_with([]):
            self.assertFalse(v.bridge_exists("vmbr0"))

    def test_auth_401_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.bridge_exists("vmbr0")
            self.assertIn("401", str(ctx.exception))

    def test_not_found_404_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(404, "Not Found")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.bridge_exists("vmbr0")
            self.assertIn("404", str(ctx.exception))

    def test_malformed_json_raises(self) -> None:
        v = self._verifier()
        bad = unittest.mock.MagicMock()
        bad.read.return_value = b"[broken"
        bad.__enter__ = lambda s: s
        bad.__exit__ = unittest.mock.MagicMock(return_value=False)
        with unittest.mock.patch("urllib.request.urlopen", return_value=bad):
            with self.assertRaises(PVEVerifyError):
                v.bridge_exists("vmbr0")

    def test_missing_node_raises(self) -> None:
        cfg = _token_config(node="")
        v = PVEVerifier(cfg)
        with self.assertRaises(PVEVerifyError) as ctx:
            v.bridge_exists("vmbr0")
        self.assertIn("node", str(ctx.exception).lower())

    def test_queries_with_any_bridge_filter(self) -> None:
        # PVE 9.2's plain node-network listing omits SDN vnet interfaces; they
        # appear only under type=any_bridge. The query must carry that filter
        # or a realized vnet bridge is never seen.
        v = self._verifier()
        seen_urls: list = []

        def fake_urlopen(req, *args, **kwargs):
            seen_urls.append(req.full_url)
            if "type=any_bridge" in req.full_url:
                return _make_response({"data": [
                    {"iface": "vmbr0", "type": "bridge"},
                    {"iface": "itvnet", "type": "vnet"},
                ]})
            return _make_response({"data": [{"iface": "vmbr0", "type": "bridge"}]})

        with unittest.mock.patch("urllib.request.urlopen", side_effect=fake_urlopen):
            self.assertTrue(v.bridge_exists("itvnet"))
        self.assertTrue(any("type=any_bridge" in u for u in seen_urls))


# ---------------------------------------------------------------------------
# volume_exists
# ---------------------------------------------------------------------------

class TestVolumeExists(unittest.TestCase):
    """Tests for PVEVerifier.volume_exists."""

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def _urlopen_with(self, vol_list: list) -> unittest.mock.MagicMock:
        payload = {"data": vol_list}
        return unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload))

    def test_happy_path_found(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("local-lvm:vm-101-disk-0")
        with self._urlopen_with([{"volid": "local-lvm:vm-101-disk-0"}]):
            self.assertTrue(v.volume_exists(cid))

    def test_happy_path_not_found(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("local-lvm:vm-101-disk-0")
        with self._urlopen_with([{"volid": "local-lvm:vm-200-disk-0"}]):
            self.assertFalse(v.volume_exists(cid))

    def test_bare_cid_raises(self) -> None:
        """A bare (unenveloped) disk CID is a hard parse error: this package
        carries no backward-compatibility requirement, and the legacy '|'
        pipe-suffix and bare-CID fallback paths have been removed entirely."""
        v = self._verifier()
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("data:vm-9897-disk-0")

    def test_legacy_pipe_suffix_raises(self) -> None:
        v = self._verifier()
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("data:vm-9897-disk-0|eyJwb29sIjoiZGF0YSIsIm5vZGUiOiJwdmUifQ")

    def test_happy_path_empty_list(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("local-lvm:vm-101-disk-0")
        with self._urlopen_with([]):
            self.assertFalse(v.volume_exists(cid))

    @staticmethod
    def _pvd_cid(volid: str, meta: dict | None = None) -> str:
        """Build a pvd- envelope CID the way the Go codec does (no padding)."""
        payload: dict = {"v": volid}
        if meta:
            payload["m"] = meta
        raw = json.dumps(payload, separators=(",", ":")).encode()
        return "pvd-" + base64.urlsafe_b64encode(raw).decode().rstrip("=")

    def test_pvd_envelope_matches_bare_volid(self) -> None:
        """A pvd- envelope CID (current emitted format) must match the bare
        volid PVE reports."""
        v = self._verifier()
        cid = self._pvd_cid("data:vm-9897-disk-0", {"pool": "data", "node": "pve"})
        with self._urlopen_with([{"volid": "data:vm-9897-disk-0"}]):
            self.assertTrue(v.volume_exists(cid))

    def test_pvd_envelope_no_meta(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("local-lvm:vm-101-disk-0")
        with self._urlopen_with([{"volid": "local-lvm:vm-101-disk-0"}]):
            self.assertTrue(v.volume_exists(cid))

    def test_pvd_envelope_path_form_volid(self) -> None:
        """Path-form volids (file/qcow storage) embed '/' — the shape that
        motivated the envelope."""
        v = self._verifier()
        cid = self._pvd_cid("local:9001/vm-9001-disk-0.qcow2", {"pool": "local"})
        with self._urlopen_with([{"volid": "local:9001/vm-9001-disk-0.qcow2"}]):
            self.assertTrue(v.volume_exists(cid))

    def test_pvd_envelope_not_found(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("data:vm-9897-disk-0")
        with self._urlopen_with([{"volid": "data:vm-200-disk-0"}]):
            self.assertFalse(v.volume_exists(cid))

    def test_pvd_standard_alphabet_payload_raises(self) -> None:
        """Go's RawURLEncoding rejects '+', '/', and '='; the Python decoder
        must fail identically instead of silently tolerating them."""
        v = self._verifier()
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvd-ab+cd")
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvd-abcd=")

    def test_pvd_malformed_payload_raises(self) -> None:
        """A pvd- CID with an undecodable payload and no ':' anywhere was meant
        to be an envelope; its corruption must raise, mirroring the Go codec."""
        v = self._verifier()
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvd-!!!notbase64")

    def test_pvd_named_storage_hard_errors(self) -> None:
        """A PVE storage literally named 'pvd-*' produces a bare CID that
        starts with the envelope prefix but contains ':' (outside the
        base64url alphabet). With the legacy fallback removed, this is now a
        hard parse error rather than a silent fallback."""
        v = self._verifier()
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvd-foo:vm-100-disk-0")

    @staticmethod
    def _pvz_cid(volid: str, meta: dict | None = None) -> str:
        """Build a pvz- compressed envelope CID the way the Go codec does:
        base64url(gzip(json)), no padding."""
        payload: dict = {"v": volid}
        if meta:
            payload["m"] = meta
        raw = json.dumps(payload, separators=(",", ":")).encode()
        return "pvz-" + base64.urlsafe_b64encode(gzip.compress(raw, 9, mtime=0)).decode().rstrip("=")

    def test_pvz_envelope_matches_bare_volid(self) -> None:
        """A pvz- compressed envelope CID (opt-in disk_cid_compression) must
        match the bare volid PVE reports."""
        v = self._verifier()
        cid = self._pvz_cid("data:vm-9897-disk-0", {"pool": "data", "node": "pve"})
        with self._urlopen_with([{"volid": "data:vm-9897-disk-0"}]):
            self.assertTrue(v.volume_exists(cid))

    def test_pvz_envelope_no_meta(self) -> None:
        v = self._verifier()
        cid = self._pvz_cid("local-lvm:vm-101-disk-0")
        with self._urlopen_with([{"volid": "local-lvm:vm-101-disk-0"}]):
            self.assertTrue(v.volume_exists(cid))

    def test_pvz_envelope_path_form_volid(self) -> None:
        v = self._verifier()
        cid = self._pvz_cid("local:9001/vm-9001-disk-0.qcow2", {"pool": "local"})
        with self._urlopen_with([{"volid": "local:9001/vm-9001-disk-0.qcow2"}]):
            self.assertTrue(v.volume_exists(cid))

    def test_pvz_envelope_not_found(self) -> None:
        v = self._verifier()
        cid = self._pvz_cid("data:vm-9897-disk-0")
        with self._urlopen_with([{"volid": "data:vm-200-disk-0"}]):
            self.assertFalse(v.volume_exists(cid))

    def test_pvz_go_encoder_fixture(self) -> None:
        """Frozen fixture emitted by the Go encoder (EncodeDiskCIDCompressed):
        pins cross-implementation decode of the compressed wire format."""
        v = self._verifier()
        cid = (
            "pvz-H4sIAAAAAAAC_2yOQWrEMAxF7_LXVquZWbT4MsWxDTGpI1c2SZmQuxcnBLqY5RPvfbRhgYWPZSQdAs1LjtRS1Jt9"
            "ML8vmR7MFFKdiN9-vKx3GGTYDUXk-3UJg1lChEVRCVTyL3Um_oCBe8LCPUmdn6iPSWm173nnx96smlocnJ9gEFL1TgMs"
            "ZIZBklK_Dvxk5uuw6v9DGzW6rvQ38nAFtztffPgn13qa-77_BQAA__-aFK-nCAEAAA"
        )
        with self._urlopen_with([{"volid": "ceph-rbd-nvme-tier1:300/vm-300-disk-0.qcow2"}]):
            self.assertTrue(v.volume_exists(cid))

    def test_pvz_standard_alphabet_payload_raises(self) -> None:
        """Charset parity with Go's RawURLEncoding: '+', '/', '=' must raise."""
        v = self._verifier()
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvz-ab+cd")
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvz-abcd=")

    def test_pvz_malformed_payload_raises(self) -> None:
        v = self._verifier()
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvz-!!!notbase64")

    def test_pvz_not_gzip_raises(self) -> None:
        """Valid base64url whose bytes are not a gzip stream must raise."""
        v = self._verifier()
        payload = base64.urlsafe_b64encode(b"plainbytesnotgzip").decode().rstrip("=")
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvz-" + payload)

    def test_pvz_bad_json_raises(self) -> None:
        v = self._verifier()
        payload = base64.urlsafe_b64encode(gzip.compress(b"notjson", 9, mtime=0)).decode().rstrip("=")
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvz-" + payload)

    def test_pvz_empty_volid_raises(self) -> None:
        v = self._verifier()
        raw = json.dumps({"m": {"pool": "data"}}, separators=(",", ":")).encode()
        payload = base64.urlsafe_b64encode(gzip.compress(raw, 9, mtime=0)).decode().rstrip("=")
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvz-" + payload)

    def test_pvz_decompression_bomb_raises(self) -> None:
        """A short payload that inflates past the size cap must raise, matching
        the Go decoder's 64 KiB guard."""
        v = self._verifier()
        payload = base64.urlsafe_b64encode(gzip.compress(b"0" * (10 << 20), 9, mtime=0)).decode().rstrip("=")
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvz-" + payload)

    def test_pvz_named_storage_hard_errors(self) -> None:
        """A PVE storage literally named 'pvz-*' is a hard parse error,
        mirroring the pvd- rule."""
        v = self._verifier()
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("pvz-foo:vm-100-disk-0")

    def test_auth_401_raises(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("local-lvm:vm-101-disk-0")
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.volume_exists(cid)
            self.assertIn("401", str(ctx.exception))

    def test_not_found_404_raises(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("local-lvm:vm-101-disk-0")
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(404, "Not Found")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.volume_exists(cid)
            self.assertIn("404", str(ctx.exception))

    def test_malformed_json_raises(self) -> None:
        v = self._verifier()
        bad = unittest.mock.MagicMock()
        bad.read.return_value = b"garbage"
        bad.__enter__ = lambda s: s
        bad.__exit__ = unittest.mock.MagicMock(return_value=False)
        with unittest.mock.patch("urllib.request.urlopen", return_value=bad):
            with self.assertRaises(PVEVerifyError):
                v.volume_exists("local-lvm:vm-101-disk-0")

    def test_missing_colon_raises(self) -> None:
        """A disk_cid without a pvd-/pvz- envelope prefix (here also lacking
        ':') is invalid and must raise PVEVerifyError."""
        v = self._verifier()
        with self.assertRaises(PVEVerifyError) as ctx:
            v.volume_exists("no-colon-here")
        self.assertIn("envelope", str(ctx.exception).lower())

    def test_missing_node_raises(self) -> None:
        cfg = _token_config(node="")
        v = PVEVerifier(cfg)
        with self.assertRaises(PVEVerifyError):
            v.volume_exists("local-lvm:vm-101-disk-0")


# ---------------------------------------------------------------------------
# Password auth — ticket fetch
# ---------------------------------------------------------------------------

class TestPasswordAuth(unittest.TestCase):
    """Ticket fetch is triggered when no api_token is present.

    These drive the auth flow through vnet_exists rather than vm_exists because
    they count HTTP calls: vnet_exists is exactly one GET, so the counts measure
    the ticket flow instead of some other predicate's request pattern.
    """

    def _ticket_response(self) -> dict:
        return {"data": {"ticket": "PVE:root@pam:DEADBEEF", "CSRFPreventionToken": "token"}}

    def test_ticket_fetched_on_first_get(self) -> None:
        """Password auth: a POST to /access/ticket must precede the GET."""
        v = PVEVerifier(_password_config())
        ticket_resp = _make_response(self._ticket_response())
        data_resp = _make_response({"data": []})

        call_count = 0

        def _fake_urlopen(req, context=None, timeout=None):
            nonlocal call_count
            call_count += 1
            if call_count == 1:
                # First call: ticket POST
                self.assertEqual(req.get_method(), "POST")
                return ticket_resp
            return data_resp

        with unittest.mock.patch("urllib.request.urlopen", side_effect=_fake_urlopen):
            result = v.vnet_exists("cpitest0")
        self.assertEqual(call_count, 2)
        self.assertFalse(result)

    def test_ticket_fetch_401_raises(self) -> None:
        v = PVEVerifier(_password_config())
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.vnet_exists("cpitest0")
            self.assertIn("401", str(ctx.exception))

    def test_ticket_fetch_malformed_json_raises(self) -> None:
        v = PVEVerifier(_password_config())
        bad = unittest.mock.MagicMock()
        bad.read.return_value = b"{{bad}}"
        bad.__enter__ = lambda s: s
        bad.__exit__ = unittest.mock.MagicMock(return_value=False)
        with unittest.mock.patch("urllib.request.urlopen", return_value=bad):
            with self.assertRaises(PVEVerifyError):
                v.vnet_exists("cpitest0")

    def test_ticket_cached_on_second_call(self) -> None:
        """Second GET does not re-fetch the ticket."""
        v = PVEVerifier(_password_config())
        ticket_resp = _make_response(self._ticket_response())
        data_resp = _make_response({"data": []})

        call_returns = [ticket_resp, data_resp, data_resp]
        with unittest.mock.patch("urllib.request.urlopen", side_effect=call_returns):
            v.vnet_exists("cpitest0")
            v.vnet_exists("cpitest0")
        # 1 ticket POST + 2 GET calls = 3 total; urlopen called exactly 3 times.
        # (If ticket were not cached, a 4th call would occur.)


# ---------------------------------------------------------------------------
# assert_true / assert_false helpers
# ---------------------------------------------------------------------------

class TestAssertHelpers(unittest.TestCase):

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def test_assert_true_passes(self) -> None:
        v = self._verifier()
        v.assert_true("vm 101 exists", True)  # must not raise

    def test_assert_true_fails(self) -> None:
        v = self._verifier()
        with self.assertRaises(PVEVerifyError) as ctx:
            v.assert_true("vm 101 exists", False)
        self.assertIn("vm 101 exists", str(ctx.exception))
        self.assertIn("present", str(ctx.exception).lower())

    def test_assert_false_passes(self) -> None:
        v = self._verifier()
        v.assert_false("vm 101 absent", False)  # must not raise

    def test_assert_false_fails(self) -> None:
        v = self._verifier()
        with self.assertRaises(PVEVerifyError) as ctx:
            v.assert_false("vm 101 absent", True)
        self.assertIn("vm 101 absent", str(ctx.exception))
        self.assertIn("absent", str(ctx.exception).lower())


# ---------------------------------------------------------------------------
# bridge_realized (delegates to bridge_exists)
# ---------------------------------------------------------------------------

class TestBridgeRealized(unittest.TestCase):

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def test_delegates_to_bridge_exists(self) -> None:
        v = self._verifier()
        payload = {"data": [{"iface": "itvnet", "type": "bridge"}]}
        with unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload)):
            self.assertTrue(v.bridge_realized("itvnet"))

    def test_returns_false_when_not_realized(self) -> None:
        v = self._verifier()
        payload = {"data": []}
        with unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload)):
            self.assertFalse(v.bridge_realized("itvnet"))


# ---------------------------------------------------------------------------
# parse_stemcell_cid / parse_stemcell_path_cid
# ---------------------------------------------------------------------------

class TestParseStemcellCID(unittest.TestCase):
    """Tests for parse_stemcell_cid (bare '<storage>:import/<file>' form)."""

    def test_happy_path(self) -> None:
        storage, volume_path = parse_stemcell_cid(
            "nfs-stemcells:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2"
        )
        self.assertEqual(storage, "nfs-stemcells")
        self.assertEqual(volume_path, "import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2")

    def test_empty_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_cid("")

    def test_missing_colon_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_cid("nfs-stemcellsimport/x.qcow2")

    def test_non_import_path_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_cid("nfs-stemcells:iso/x.qcow2")


class TestParseStemcellPathCID(unittest.TestCase):
    """Tests for parse_stemcell_path_cid, mirroring the Go
    ParseStemcellPathCID error contract (internal/pve/stemcell_volume.go)."""

    # Fixture pinned to the same string the Go tests use, for
    # cross-implementation parity.
    LIGHT_FIXTURE = ":light:nfs-stemcells:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2"
    HEAVY_FIXTURE = ":heavy:nfs-stemcells:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2"

    def test_light_roundtrip_fixture(self) -> None:
        kind, storage, volume_path = parse_stemcell_path_cid(self.LIGHT_FIXTURE)
        self.assertEqual(kind, "light")
        self.assertEqual(storage, "nfs-stemcells")
        self.assertEqual(volume_path, "import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2")
        self.assertEqual(
            f":{kind}:{storage}:{volume_path}", self.LIGHT_FIXTURE
        )

    def test_heavy_roundtrip_fixture(self) -> None:
        kind, storage, volume_path = parse_stemcell_path_cid(self.HEAVY_FIXTURE)
        self.assertEqual(kind, "heavy")
        self.assertEqual(storage, "nfs-stemcells")
        self.assertEqual(volume_path, "import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2")
        self.assertEqual(
            f":{kind}:{storage}:{volume_path}", self.HEAVY_FIXTURE
        )

    def test_empty_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid("")

    def test_bare_storage_form_raises(self) -> None:
        """Retired grammar: bare '<storage>:import/<file>' has no leading ':'."""
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid("nfs-stemcells:import/x.qcow2")

    def test_old_light_prefix_without_colon_raises(self) -> None:
        """Retired grammar: 'light:<storage>:import/<file>' (no leading ':')."""
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid("light:nfs-stemcells:import/x.qcow2")

    def test_template_form_raises(self) -> None:
        """Retired grammar: 'template:<vmid>' (no leading ':')."""
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid("template:6042")

    def test_bare_integer_raises(self) -> None:
        """Retired grammar: legacy bare-VMID stemcell CID."""
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid("5042")

    def test_unknown_kind_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid(":medium:nfs-stemcells:import/x.qcow2")

    def test_doubled_prefix_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid(":light::heavy:nfs-stemcells:import/x.qcow2")

    def test_payload_missing_import_prefix_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid(":light:nfs-stemcells:iso/x.qcow2")

    def test_payload_missing_colon_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid(":light:nfs-stemcellsimport/x.qcow2")

    def test_kind_prefix_only_no_payload_raises(self) -> None:
        with self.assertRaises(PVEVerifyError):
            parse_stemcell_path_cid(":light:")


# ---------------------------------------------------------------------------
# Parked-disk (parker VM) inspection
# ---------------------------------------------------------------------------

class TestParkerInspection(unittest.TestCase):
    """Tests for the helpers the lifecycle harness uses to verify parked disks.

    Transport is stubbed at _get rather than at urlopen, because these helpers
    make a different call per VM and the tests need each path to answer for
    itself.
    """

    PARKED_VOLID = "local-lvm:vm-9001-disk-0"

    def _verifier(self, cluster: dict) -> PVEVerifier:
        """Build a verifier whose _get answers from a path -> payload dict."""
        v = PVEVerifier(_token_config())

        def fake_get(path: str):
            if path not in cluster:
                raise AssertionError(f"unexpected GET {path}")
            return cluster[path]

        v._get = fake_get  # type: ignore[method-assign]
        return v

    @staticmethod
    def _pvd_cid(volid: str) -> str:
        raw = json.dumps({"v": volid}, separators=(",", ":")).encode()
        return "pvd-" + base64.urlsafe_b64encode(raw).decode().rstrip("=")

    def _cluster(self, parker_cfg: dict, vm_cfg: dict | None = None) -> dict:
        vms = [{"vmid": 90000}]
        resources = [{"vmid": 90000, "node": "pve1"}]
        cluster = {
            "/nodes/pve1/qemu": vms,
            "/cluster/resources?type=vm": resources,
            "/nodes/pve1/qemu/90000/config": parker_cfg,
        }
        if vm_cfg is not None:
            vms.append({"vmid": 101})
            resources.append({"vmid": 101, "node": "pve1"})
            cluster["/nodes/pve1/qemu/101/config"] = vm_cfg
        return cluster

    # -- cluster_vms / holder resilience -------------------------------------

    def test_holder_on_another_node_is_found(self) -> None:
        """A holder on a second node must not read as free-floating. This is the
        one assertion in the harness that fails OPEN when it is wrong: a
        node-scoped scan returns [] and 'no VM holds the disk' passes."""
        v = self._verifier({
            "/cluster/resources?type=vm": [{"vmid": 105, "node": "pve2"}],
            "/nodes/pve2/qemu/105/config": {"scsi1": self.PARKED_VOLID},
        })
        self.assertEqual(v.disk_holders(self._pvd_cid(self.PARKED_VOLID)), [(105, "scsi1")])

    def test_unreadable_vm_config_is_skipped_not_fatal(self) -> None:
        """A guest destroyed mid-scan, or one PVE refuses to read while it is
        locked, must not abort the run. The Go side skips the same case."""
        cluster = {
            "/cluster/resources?type=vm": [
                {"vmid": 101, "node": "pve1"},
                {"vmid": 102, "node": "pve1"},
            ],
            "/nodes/pve1/qemu/102/config": {"scsi1": self.PARKED_VOLID},
        }

        def fake_get(path: str):
            if path not in cluster:
                raise PVEVerifyError(f"GET {path} failed: 500")
            return cluster[path]

        v = PVEVerifier(_token_config())
        v._get = fake_get  # type: ignore[method-assign]
        self.assertEqual(v.disk_holders(self._pvd_cid(self.PARKED_VOLID)), [(102, "scsi1")])

    def test_cluster_vms_falls_back_to_node_listing(self) -> None:
        """A token without /cluster/resources still gets the node's own VMs."""
        cluster = {"/nodes/pve1/qemu": [{"vmid": 101}]}

        def fake_get(path: str):
            if path not in cluster:
                raise PVEVerifyError(f"GET {path} forbidden")
            return cluster[path]

        v = PVEVerifier(_token_config())
        v._get = fake_get  # type: ignore[method-assign]
        self.assertEqual(v.cluster_vms(), [(101, "pve1")])

    # -- parker_vmids --------------------------------------------------------

    def test_parker_recognized_by_band_and_tag(self) -> None:
        v = self._verifier(self._cluster({"tags": "bosh-cpi;bosh-parker"}))
        self.assertEqual(v.parker_vmids(90000, 90999), [90000])

    def test_vm_in_band_without_tag_is_not_a_parker(self) -> None:
        """The band alone must not classify: an operator VM that happens to sit
        at 90000 is not a parker, and treating it as one would let the harness
        pass while the CPI parked nothing."""
        v = self._verifier(self._cluster({"tags": "some-other-tag"}))
        self.assertEqual(v.parker_vmids(90000, 90999), [])

    def test_parker_tag_match_is_case_insensitive(self) -> None:
        v = self._verifier(self._cluster({"tags": "BOSH-Parker"}))
        self.assertEqual(v.parker_vmids(90000, 90999), [90000])

    def test_parker_tag_match_accepts_comma_and_space_separators(self) -> None:
        """splitTagString in internal/pve/parker.go tokenizes on ';', ',', and
        ' '. A splitter that knew only ';' would return no parkers at all, which
        turns every "no parker was created" assertion into a vacuous pass."""
        for tags in ("bosh-cpi,bosh-parker", "bosh-cpi bosh-parker", "bosh-cpi; bosh-parker"):
            with self.subTest(tags=tags):
                v = self._verifier(self._cluster({"tags": tags}))
                self.assertEqual(v.parker_vmids(90000, 90999), [90000])

    def test_vm_outside_band_is_ignored(self) -> None:
        v = self._verifier(self._cluster({"tags": "bosh-parker"}))
        self.assertEqual(v.parker_vmids(91000, 91999), [])

    def test_parker_on_another_node_is_found(self) -> None:
        """A park lands on the disk's own node, not necessarily the configured
        one. A node-scoped scan returns [] here, which makes "no parker was
        created" pass while a parker holds the disk on the next node over."""
        v = self._verifier({
            "/cluster/resources?type=vm": [{"vmid": 90001, "node": "pve2", "type": "qemu"}],
            "/nodes/pve2/qemu/90001/config": {"tags": "bosh-cpi;bosh-parker"},
        })
        self.assertEqual(v.parker_vmids(90000, 90999), [90001])

    def test_cluster_listing_is_read_fresh_every_call(self) -> None:
        """The harness holds one verifier for a whole run and asserts on cluster
        state right after creating a VM, parking a disk, and deleting a VM. A
        cached listing answers every one of those with the cluster as it was
        earlier in the run: the delete assertion sees the VM it just deleted, and
        the parker assertions miss a parker created mid-run."""
        v = PVEVerifier(_token_config())
        state: list[dict] = [{"vmid": 101, "node": "pve1", "type": "qemu"}]

        def fake_get(path: str):
            if path == "/cluster/resources?type=vm":
                return list(state)
            if path == "/nodes/pve2/qemu/90001/config":
                return {"tags": "bosh-cpi;bosh-parker"}
            raise AssertionError(f"unexpected GET {path}")

        v._get = fake_get  # type: ignore[method-assign]
        self.assertTrue(v.vm_exists(101), "the VM is there before the delete")
        self.assertEqual(v.parker_vmids(90000, 90999), [], "no parker yet")
        state.clear()
        self.assertFalse(v.vm_exists(101), "the VM must be gone after the delete")
        state.append({"vmid": 90001, "node": "pve2", "type": "qemu"})
        self.assertTrue(v.vm_exists(90001), "a VM created mid-run must be visible")
        self.assertEqual(
            v.parker_vmids(90000, 90999), [90001],
            "a parker created mid-run must be visible",
        )

    def test_vm_exists_finds_a_vm_on_another_node(self) -> None:
        """vm_exists has to match parker discovery: a node-scoped presence check
        answers False for a parker on the next node over, and the assertion built
        on it then reports that the parker was destroyed."""
        v = self._verifier({
            "/cluster/resources?type=vm": [{"vmid": 90001, "node": "pve2", "type": "qemu"}],
        })
        self.assertTrue(v.vm_exists(90001))
        self.assertFalse(v.vm_exists(90002))

    def test_container_in_parker_band_is_skipped(self) -> None:
        """/cluster/resources?type=vm lists LXC containers too, and none of
        these predicates can read a container config."""
        v = self._verifier({
            "/cluster/resources?type=vm": [
                {"vmid": 90001, "node": "pve1", "type": "lxc"},
                {"vmid": 90002, "node": "pve1", "type": "qemu"},
            ],
            "/nodes/pve1/qemu/90002/config": {"tags": "bosh-parker"},
        })
        self.assertEqual(v.parker_vmids(90000, 90999), [90002])

    # -- disk_holders --------------------------------------------------------

    def test_holder_found_on_parker_slot(self) -> None:
        v = self._verifier(
            self._cluster({"tags": "bosh-parker", "scsi3": f"{self.PARKED_VOLID},size=1G"})
        )
        self.assertEqual(
            v.disk_holders(self._pvd_cid(self.PARKED_VOLID)), [(90000, "scsi3")]
        )

    def test_free_floating_disk_has_no_holder(self) -> None:
        v = self._verifier(self._cluster({"tags": "bosh-parker"}))
        self.assertEqual(v.disk_holders(self._pvd_cid(self.PARKED_VOLID)), [])

    def test_unused_slot_counts_as_a_holder(self) -> None:
        """PVE demotes a volume to unusedN when it leaves a bus without being
        deleted. That is still a config reference, and reporting it as free
        would hide a half-finished detach."""
        v = self._verifier(
            self._cluster({"tags": "bosh-parker", "unused0": self.PARKED_VOLID})
        )
        self.assertEqual(
            v.disk_holders(self._pvd_cid(self.PARKED_VOLID)), [(90000, "unused0")]
        )

    def test_double_attach_reports_both_holders(self) -> None:
        v = self._verifier(
            self._cluster(
                {"tags": "bosh-parker", "scsi0": f"{self.PARKED_VOLID},size=1G"},
                {"scsi1": f"{self.PARKED_VOLID},size=1G"},
            )
        )
        self.assertEqual(
            v.disk_holders(self._pvd_cid(self.PARKED_VOLID)),
            [(101, "scsi1"), (90000, "scsi0")],
        )

    def test_volid_prefix_does_not_match(self) -> None:
        """'vm-9001-disk-0' must not match 'vm-9001-disk-01'."""
        v = self._verifier(
            self._cluster({"tags": "bosh-parker", "scsi0": f"{self.PARKED_VOLID}1,size=1G"})
        )
        self.assertEqual(v.disk_holders(self._pvd_cid(self.PARKED_VOLID)), [])

    def test_non_disk_config_key_is_ignored(self) -> None:
        v = self._verifier(
            self._cluster({"tags": "bosh-parker", "description": self.PARKED_VOLID})
        )
        self.assertEqual(v.disk_holders(self._pvd_cid(self.PARKED_VOLID)), [])

    # -- parked_disks (provenance sentinel) ----------------------------------

    def test_provenance_entry_read_from_sentinel(self) -> None:
        desc = (
            'parker\n<!--BOSH:{"bosh_parked_disks":{"'
            + self.PARKED_VOLID
            + '":{"disk_cid":"pvd-x","parked_at":"2026-08-19T00:00:00Z","node":"pve1"}}}-->'
        )
        v = self._verifier(self._cluster({"tags": "bosh-parker", "description": desc}))
        self.assertIn(self.PARKED_VOLID, v.parked_disks(90000))

    def test_no_sentinel_yields_empty_map(self) -> None:
        v = self._verifier(self._cluster({"tags": "bosh-parker", "description": "plain"}))
        self.assertEqual(v.parked_disks(90000), {})

    def test_corrupt_sentinel_yields_empty_map(self) -> None:
        """Provenance is best-effort in the CPI, so unreadable provenance must
        not crash the verification that reads it."""
        v = self._verifier(
            self._cluster({"tags": "bosh-parker", "description": "<!--BOSH:{not json-->"})
        )
        self.assertEqual(v.parked_disks(90000), {})

    def test_sentinel_without_parked_key_yields_empty_map(self) -> None:
        v = self._verifier(
            self._cluster(
                {"tags": "bosh-parker", "description": '<!--BOSH:{"bosh_attached_disks":{}}-->'}
            )
        )
        self.assertEqual(v.parked_disks(90000), {})

    # -- qemu_config ---------------------------------------------------------

    def test_qemu_config_rejects_non_object(self) -> None:
        v = self._verifier({
            "/cluster/resources?type=vm": [{"vmid": 90000, "node": "pve1", "type": "qemu"}],
            "/nodes/pve1/qemu/90000/config": None,
        })
        with self.assertRaises(PVEVerifyError):
            v.qemu_config(90000)

    def test_qemu_config_reads_the_node_hosting_the_vm(self) -> None:
        """The configured node is a fallback, not the answer: a parker lands on
        the disk's own node, and reading the wrong one 404s on a VM that is
        plainly there."""
        v = self._verifier({
            "/cluster/resources?type=vm": [{"vmid": 90000, "node": "pve2", "type": "qemu"}],
            "/nodes/pve2/qemu/90000/config": {"name": "bosh-parker-90000"},
        })
        self.assertEqual(v.qemu_config(90000).get("name"), "bosh-parker-90000")


class ParkerDiskSlotsTests(unittest.TestCase):
    """parker_disk_slots reports what a --purge would actually destroy."""

    @staticmethod
    def _verifier(cfg: dict) -> PVEVerifier:
        v = PVEVerifier.__new__(PVEVerifier)
        v.node = "pve1"

        def fake_get(path: str):
            if path == "/cluster/resources?type=vm":
                return [{"vmid": 90000, "node": "pve1", "type": "qemu"}]
            if path != "/nodes/pve1/qemu/90000/config":
                raise AssertionError(f"unexpected GET {path}")
            return cfg

        v._get = fake_get  # type: ignore[method-assign]
        return v

    def test_active_and_unused_slots_counted(self) -> None:
        v = self._verifier(
            {
                "scsihw": "virtio-scsi-pci",
                "scsi0": "local-lvm:vm-9001-disk-0,iothread=1",
                "scsi3": "local-lvm:vm-9002-disk-0",
                "unused0": "local-lvm:vm-9003-disk-0",
                "protection": 1,
                "name": "bosh-parker-1",
            }
        )
        self.assertEqual(
            v.parker_disk_slots(90000),
            {
                "scsi0": "local-lvm:vm-9001-disk-0",
                "scsi3": "local-lvm:vm-9002-disk-0",
                "unused0": "local-lvm:vm-9003-disk-0",
            },
        )

    def test_scsihw_is_not_a_disk(self) -> None:
        """The digit anchor is the point: every parker carries scsihw, so a bare
        ^scsi match reads an empty parker as one holding a disk."""
        v = self._verifier({"scsihw": "virtio-scsi-pci", "protection": 1})
        self.assertEqual(v.parker_disk_slots(90000), {})


# ---------------------------------------------------------------------------
# volume_exists across nodes
# ---------------------------------------------------------------------------

class TestVolumeExistsMultiNode(unittest.TestCase):
    """volume_exists on a multi-node cluster with node-local storage.

    The CPI places a disk on whichever node has headroom, and a node-local
    storage's content listing is per node — so a probe that only asked the
    configured node answered False for a volume that is plainly there, and the
    lifecycle run aborted right after create_disk.
    """

    VOLID = "local-lvm-data:vm-17000-disk-0"

    @staticmethod
    def _pvd_cid(volid: str, meta: dict | None = None) -> str:
        payload: dict = {"v": volid}
        if meta:
            payload["m"] = meta
        raw = json.dumps(payload, separators=(",", ":")).encode()
        return "pvd-" + base64.urlsafe_b64encode(raw).decode().rstrip("=")

    def _verifier(self, routes: dict) -> PVEVerifier:
        v = PVEVerifier(_token_config())

        def fake_get(path: str):
            if path not in routes:
                raise PVEVerifyError(f"unrouted GET {path}")
            return routes[path]

        v._get = fake_get  # type: ignore[method-assign]
        return v

    def test_cid_metadata_node_is_asked_first(self) -> None:
        """The CID's own metadata names the node the volume was created on;
        that listing is the one that can actually see it."""
        v = self._verifier({
            "/nodes/pve2/storage/local-lvm-data/content": [{"volid": self.VOLID}],
        })
        cid = self._pvd_cid(self.VOLID, meta={"pool": "local-lvm-data", "node": "pve2"})
        self.assertTrue(v.volume_exists(cid))

    def test_volume_on_unconfigured_node_found_via_cluster(self) -> None:
        """No metadata node: every node hosting a VM is asked, then the
        configured one. The volume sits on pve2 only."""
        v = self._verifier({
            "/cluster/resources?type=vm": [
                {"vmid": 101, "node": "pve1"},
                {"vmid": 102, "node": "pve2"},
            ],
            "/nodes/pve1/storage/local-lvm-data/content": [],
            "/nodes/pve2/storage/local-lvm-data/content": [{"volid": self.VOLID}],
            "/nodes/pve/storage/local-lvm-data/content": [],
        })
        self.assertTrue(v.volume_exists(self._pvd_cid(self.VOLID)))

    def test_absent_everywhere_is_false(self) -> None:
        v = self._verifier({
            "/cluster/resources?type=vm": [{"vmid": 101, "node": "pve1"}],
            "/nodes/pve1/storage/local-lvm-data/content": [],
            "/nodes/pve/storage/local-lvm-data/content": [],
        })
        self.assertFalse(v.volume_exists(self._pvd_cid(self.VOLID)))

    def test_unreadable_node_listing_skipped(self) -> None:
        """A node whose storage listing cannot be read is skipped: the volume
        may still be visible from another node."""
        routes = {
            "/cluster/resources?type=vm": [
                {"vmid": 101, "node": "pve1"},
                {"vmid": 102, "node": "pve2"},
            ],
            # pve1's listing is deliberately unrouted -> PVEVerifyError.
            "/nodes/pve2/storage/local-lvm-data/content": [{"volid": self.VOLID}],
        }
        self.assertTrue(self._verifier(routes).volume_exists(self._pvd_cid(self.VOLID)))

    def test_cid_node_extraction(self) -> None:
        mod = sys.modules[PVEVerifier.__module__]
        self.assertEqual(
            mod._cid_node(self._pvd_cid(self.VOLID, meta={"node": "pve7"})), "pve7"
        )
        self.assertEqual(mod._cid_node(self._pvd_cid(self.VOLID)), "")
        self.assertEqual(mod._cid_node("not-an-envelope"), "")


# ---------------------------------------------------------------------------
# volume_entry / rbd volids
# ---------------------------------------------------------------------------

class TestVolumeEntry(unittest.TestCase):
    """volume_entry returns the whole content entry, not just presence, so
    format and size assertions can be built on it. volume_exists delegates to
    it, so the node-fallback and raise-when-all-nodes-failed behavior above
    covers both."""

    @staticmethod
    def _pvd_cid(volid: str, meta: dict | None = None) -> str:
        payload: dict = {"v": volid}
        if meta:
            payload["m"] = meta
        raw = json.dumps(payload, separators=(",", ":")).encode()
        return "pvd-" + base64.urlsafe_b64encode(raw).decode().rstrip("=")

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def _urlopen_with(self, vol_list: list) -> unittest.mock.MagicMock:
        payload = {"data": vol_list}
        return unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload))

    def test_returns_matching_entry_whole(self) -> None:
        v = self._verifier()
        entry = {"volid": "local-lvm:vm-101-disk-0", "format": "raw", "size": 1073741824}
        cid = self._pvd_cid("local-lvm:vm-101-disk-0")
        with self._urlopen_with([{"volid": "local-lvm:vm-200-disk-0"}, entry]):
            self.assertEqual(v.volume_entry(cid), entry)

    def test_returns_none_when_absent(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("local-lvm:vm-101-disk-0")
        with self._urlopen_with([{"volid": "local-lvm:vm-200-disk-0"}]):
            self.assertIsNone(v.volume_entry(cid))

    def test_rbd_shaped_volid_matches(self) -> None:
        """rbd volids are bare '<storage>:vm-<vmid>-disk-N' with no path
        component and a raw-only format; they must keep matching."""
        v = self._verifier()
        cid = self._pvd_cid("ceph-rbd:vm-9001-disk-0", {"pool": "ceph-rbd"})
        listing = [{"volid": "ceph-rbd:vm-9001-disk-0", "format": "raw", "size": 1073741824}]
        with self._urlopen_with(listing):
            self.assertTrue(v.volume_exists(cid))
            entry = v.volume_entry(cid)
        self.assertIsNotNone(entry)
        self.assertEqual(entry["format"], "raw")


# ---------------------------------------------------------------------------
# check_volume_format
# ---------------------------------------------------------------------------

class TestCheckVolumeFormat(unittest.TestCase):
    """check_volume_format compares the CID envelope's recorded format ("m"."f")
    against the content entry's "format" field."""

    VOLID = "ceph-rbd:vm-9001-disk-0"

    @staticmethod
    def _pvd_cid(volid: str, meta: dict | None = None) -> str:
        payload: dict = {"v": volid}
        if meta:
            payload["m"] = meta
        raw = json.dumps(payload, separators=(",", ":")).encode()
        return "pvd-" + base64.urlsafe_b64encode(raw).decode().rstrip("=")

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def _urlopen_with(self, vol_list: list) -> unittest.mock.MagicMock:
        payload = {"data": vol_list}
        return unittest.mock.patch("urllib.request.urlopen", return_value=_make_response(payload))

    def test_matching_formats_ok(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid(self.VOLID, {"pool": "ceph-rbd", "f": "raw"})
        with self._urlopen_with([{"volid": self.VOLID, "format": "raw"}]):
            self.assertEqual(v.check_volume_format(cid), "ok")

    def test_match_is_case_insensitive(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid(self.VOLID, {"f": "RAW"})
        with self._urlopen_with([{"volid": self.VOLID, "format": "raw"}]):
            self.assertEqual(v.check_volume_format(cid), "ok")

    def test_mismatch_raises(self) -> None:
        """The latent-defect shape: the CID says qcow2 over a volume PVE
        created as raw (block-native storage ignores file formats)."""
        v = self._verifier()
        cid = self._pvd_cid(self.VOLID, {"f": "qcow2"})
        with self._urlopen_with([{"volid": self.VOLID, "format": "raw"}]):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.check_volume_format(cid)
        self.assertIn("qcow2", str(ctx.exception))
        self.assertIn("raw", str(ctx.exception))

    def test_cid_without_format_skips(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid(self.VOLID, {"pool": "ceph-rbd"})
        with self._urlopen_with([{"volid": self.VOLID, "format": "raw"}]):
            result = v.check_volume_format(cid)
        self.assertTrue(result.startswith("skipped"))

    def test_entry_without_format_skips(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid(self.VOLID, {"f": "raw"})
        with self._urlopen_with([{"volid": self.VOLID}]):
            result = v.check_volume_format(cid)
        self.assertTrue(result.startswith("skipped"))

    def test_absent_volume_raises(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid(self.VOLID, {"f": "raw"})
        with self._urlopen_with([]):
            with self.assertRaises(PVEVerifyError):
                v.check_volume_format(cid)

    def test_cid_format_extraction(self) -> None:
        mod = sys.modules[PVEVerifier.__module__]
        self.assertEqual(mod._cid_format(self._pvd_cid(self.VOLID, {"f": "raw"})), "raw")
        self.assertEqual(mod._cid_format(self._pvd_cid(self.VOLID)), "")
        self.assertEqual(mod._cid_format("not-an-envelope"), "")


# ---------------------------------------------------------------------------
# storage classification (/storage index)
# ---------------------------------------------------------------------------

class TestStorageClassification(unittest.TestCase):
    """storage_type / storage_is_shared mirror StorageInfo.IsShared in
    internal/pve/storage_info.go so the harness gates its per-type assertions
    the same way the CPI classifies backends."""

    INDEX = [
        {"storage": "ceph-rbd", "type": "rbd", "content": "images"},
        {"storage": "local-lvm-thin", "type": "lvmthin", "content": "images"},
        {"storage": "shared-dir", "type": "dir", "content": "images", "shared": 1},
        {"storage": "plain-dir", "type": "dir", "content": "images", "shared": 0},
        {"storage": "ceph-fs", "type": "cephfs", "content": "images"},
    ]

    def _verifier(self) -> PVEVerifier:
        v = PVEVerifier(_token_config())

        def fake_get(path: str):
            if path != "/storage":
                raise AssertionError(f"unexpected GET {path}")
            return self.INDEX

        v._get = fake_get  # type: ignore[method-assign]
        return v

    def test_storage_entry_found(self) -> None:
        v = self._verifier()
        self.assertEqual(v.storage_entry("ceph-rbd"), self.INDEX[0])

    def test_storage_entry_absent_is_none(self) -> None:
        self.assertIsNone(self._verifier().storage_entry("nope"))

    def test_storage_type(self) -> None:
        v = self._verifier()
        self.assertEqual(v.storage_type("ceph-rbd"), "rbd")
        self.assertEqual(v.storage_type("local-lvm-thin"), "lvmthin")
        self.assertEqual(v.storage_type("nope"), "")

    def test_shared_by_type(self) -> None:
        v = self._verifier()
        self.assertTrue(v.storage_is_shared("ceph-rbd"))
        self.assertTrue(v.storage_is_shared("ceph-fs"))

    def test_local_types_not_shared(self) -> None:
        v = self._verifier()
        self.assertFalse(v.storage_is_shared("local-lvm-thin"))
        self.assertFalse(v.storage_is_shared("plain-dir"))

    def test_shared_flag_wins_over_local_type(self) -> None:
        """PVE reports the storage.cfg shared flag as an integer boolean; a
        dir storage flagged shared=1 is shared even though the type is not."""
        self.assertTrue(self._verifier().storage_is_shared("shared-dir"))

    def test_unknown_storage_not_shared(self) -> None:
        self.assertFalse(self._verifier().storage_is_shared("nope"))


# ---------------------------------------------------------------------------
# Stable disk identity (D13)
# ---------------------------------------------------------------------------

class TestStableDiskIdentity(unittest.TestCase):
    """disk_stable_id, drive-entry matching, and the dual-keyed provenance
    lookup that keep the harness identity-aware after a move_disk rename."""

    STABLE_ID = "bpd-0011223344556677"

    @staticmethod
    def _pvd_cid(volid: str, meta: dict | None = None) -> str:
        payload: dict = {"v": volid}
        if meta:
            payload["m"] = meta
        raw = json.dumps(payload, separators=(",", ":")).encode()
        return "pvd-" + base64.urlsafe_b64encode(raw).decode().rstrip("=")

    def _verifier(self) -> PVEVerifier:
        return PVEVerifier(_token_config())

    def test_disk_stable_id_present(self) -> None:
        cid = self._pvd_cid("data:vm-9001-disk-0", {"id": self.STABLE_ID})
        self.assertEqual(_pve_verify.disk_stable_id(cid), self.STABLE_ID)

    def test_disk_stable_id_absent(self) -> None:
        self.assertEqual(_pve_verify.disk_stable_id(self._pvd_cid("data:vm-9001-disk-0")), "")
        self.assertEqual(
            _pve_verify.disk_stable_id(self._pvd_cid("data:vm-9001-disk-0", {"pool": "data"})), ""
        )

    def test_disk_stable_id_garbage_cid(self) -> None:
        self.assertEqual(_pve_verify.disk_stable_id("not-a-cid"), "")

    def test_drive_entry_matches(self) -> None:
        match = _pve_verify._drive_entry_matches
        self.assertTrue(match("data:vm-9001-disk-0,size=10G", "data:vm-9001-disk-0", ""))
        self.assertTrue(
            match(
                f"data:vm-700-disk-1,serial={self.STABLE_ID},size=10G",
                "data:vm-9001-disk-0",
                self.STABLE_ID,
            )
        )
        self.assertFalse(
            match("data:vm-700-disk-1,serial=other,size=10G", "data:vm-9001-disk-0", self.STABLE_ID)
        )
        self.assertFalse(match("data:vm-700-disk-1,size=10G", "data:vm-9001-disk-0", ""))

    def test_disk_holders_matches_renamed_volume_by_serial(self) -> None:
        v = self._verifier()
        cid = self._pvd_cid("data:vm-9001-disk-0", {"id": self.STABLE_ID})
        with unittest.mock.patch.object(v, "cluster_vms", return_value=[(700, "pve1")]):
            with unittest.mock.patch.object(
                v,
                "qemu_config",
                return_value={"scsi1": f"data:vm-700-disk-1,serial={self.STABLE_ID},size=10G"},
            ):
                self.assertEqual(v.disk_holders(cid), [(700, "scsi1")])
                self.assertEqual(v.current_disk_volid(cid), "data:vm-700-disk-1")

    def test_parked_disk_recorded_dual_keying(self) -> None:
        v = self._verifier()
        legacy_cid = self._pvd_cid("data:vm-9001-disk-0")
        id_cid = self._pvd_cid("data:vm-9001-disk-0", {"id": self.STABLE_ID})
        legacy_entries = {"data:vm-9001-disk-0": {"disk_cid": legacy_cid, "node": "pve1"}}
        id_entries = {
            self.STABLE_ID: {"disk_cid": id_cid, "node": "pve1", "volid": "data:vm-90000-disk-2"}
        }
        with unittest.mock.patch.object(v, "cluster_vms", return_value=[]):
            with unittest.mock.patch.object(v, "parked_disks", return_value=legacy_entries):
                self.assertTrue(v.parked_disk_recorded(90000, legacy_cid))
            with unittest.mock.patch.object(v, "parked_disks", return_value=id_entries):
                self.assertTrue(v.parked_disk_recorded(90000, id_cid))
            with unittest.mock.patch.object(v, "parked_disks", return_value={}):
                self.assertFalse(v.parked_disk_recorded(90000, id_cid))

    def test_parked_disk_recorded_matches_entry_volid(self) -> None:
        """A stable-ID entry is found through its volid field when the caller
        resolves the current volid (mid-transfer intent records)."""
        v = self._verifier()
        id_cid = self._pvd_cid("data:vm-9001-disk-0", {"id": self.STABLE_ID})
        entries = {"bpd-otherkey000000000": {"volid": "data:vm-700-disk-1", "node": "pve1"}}
        with unittest.mock.patch.object(v, "parked_disks", return_value=entries):
            with unittest.mock.patch.object(
                v, "current_disk_volid", return_value="data:vm-700-disk-1"
            ):
                self.assertTrue(v.parked_disk_recorded(90000, id_cid))


if __name__ == "__main__":
    unittest.main()
