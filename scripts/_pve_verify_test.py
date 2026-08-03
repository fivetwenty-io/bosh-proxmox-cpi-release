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
    """Ticket fetch is triggered when no api_token is present."""

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
            result = v.vm_exists(101)
        self.assertEqual(call_count, 2)
        self.assertFalse(result)

    def test_ticket_fetch_401_raises(self) -> None:
        v = PVEVerifier(_password_config())
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.vm_exists(101)
            self.assertIn("401", str(ctx.exception))

    def test_ticket_fetch_malformed_json_raises(self) -> None:
        v = PVEVerifier(_password_config())
        bad = unittest.mock.MagicMock()
        bad.read.return_value = b"{{bad}}"
        bad.__enter__ = lambda s: s
        bad.__exit__ = unittest.mock.MagicMock(return_value=False)
        with unittest.mock.patch("urllib.request.urlopen", return_value=bad):
            with self.assertRaises(PVEVerifyError):
                v.vm_exists(101)

    def test_ticket_cached_on_second_call(self) -> None:
        """Second GET does not re-fetch the ticket."""
        v = PVEVerifier(_password_config())
        ticket_resp = _make_response(self._ticket_response())
        data_resp = _make_response({"data": []})

        call_returns = [ticket_resp, data_resp, data_resp]
        with unittest.mock.patch("urllib.request.urlopen", side_effect=call_returns):
            v.vm_exists(101)
            v.vm_exists(101)
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


if __name__ == "__main__":
    unittest.main()
