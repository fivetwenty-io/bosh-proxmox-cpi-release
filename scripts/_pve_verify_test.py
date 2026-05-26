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
from _pve_verify import PVEVerifier, PVEVerifyError


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
        with self._urlopen_with([{"volid": "local-lvm:vm-101-disk-0"}]):
            self.assertTrue(v.volume_exists("local-lvm:vm-101-disk-0"))

    def test_happy_path_not_found(self) -> None:
        v = self._verifier()
        with self._urlopen_with([{"volid": "local-lvm:vm-200-disk-0"}]):
            self.assertFalse(v.volume_exists("local-lvm:vm-101-disk-0"))

    def test_happy_path_empty_list(self) -> None:
        v = self._verifier()
        with self._urlopen_with([]):
            self.assertFalse(v.volume_exists("local-lvm:vm-101-disk-0"))

    def test_auth_401_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(401, "Unauthorized")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.volume_exists("local-lvm:vm-101-disk-0")
            self.assertIn("401", str(ctx.exception))

    def test_not_found_404_raises(self) -> None:
        v = self._verifier()
        with unittest.mock.patch("urllib.request.urlopen", side_effect=_make_http_error(404, "Not Found")):
            with self.assertRaises(PVEVerifyError) as ctx:
                v.volume_exists("local-lvm:vm-101-disk-0")
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
        """disk_cid without ':' is invalid and must raise PVEVerifyError."""
        v = self._verifier()
        with self.assertRaises(PVEVerifyError) as ctx:
            v.volume_exists("no-colon-here")
        self.assertIn("storage", str(ctx.exception).lower())

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


if __name__ == "__main__":
    unittest.main()
