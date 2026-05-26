#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# ///
"""Independent PVE-state verifier for the integration test harness.

The lifecycle harness drives bin/cpi and trusts the CPI's JSON-RPC return
values. This module verifies the *actual* cluster state out-of-band by querying
the PVE REST API directly, reusing the host and auth from the same CPI config
the CPI binary was given. It lets the harness assert that the CPI and the
cluster agree (e.g. after create_network the vnet really exists; after
delete_vm the VM is really gone).

Stdlib only — scripts/lifecycle declares no pip dependencies and imports this
module, so it must not pull anything in.

Auth mirrors src/pve_cpi/internal/pve/client.go:
    api_token present -> header  Authorization: PVEAPIToken=<token>
    else password     -> POST /access/ticket, then  Cookie: PVEAuthCookie=<ticket>
Read-only GETs need no CSRFPreventionToken.

All existence predicates query *list* endpoints and test membership, which
return a 200 array uniformly and sidestep the 404-vs-500 ambiguity of PVE's
per-id GETs.

Standalone usage (CI smoke / debugging):
    ./scripts/_pve_verify.py --config cpi.json vnet itvnet
    ./scripts/_pve_verify.py --config cpi.json vm 901
    ./scripts/_pve_verify.py --config cpi.json subnet itvnet 10.250.0.0/24
Exit 0 when the resource exists, 1 when absent, 2 on usage/transport error.
"""

from __future__ import annotations

import argparse
import json
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


class PVEVerifyError(RuntimeError):
    """Raised on transport, auth, or assertion failure (not on a clean absent)."""


class PVEVerifier:
    """Minimal read-only PVE REST API client built from a CPI config dict."""

    def __init__(self, config: dict[str, Any]) -> None:
        host = str(config.get("host", "")).strip()
        if not host:
            raise PVEVerifyError("cpi config missing 'host'")
        self.host = host
        self.port = int(config.get("port", 8006) or 8006)
        self.node = str(config.get("node", "")).strip()
        # The harness always synthesizes verify_ssl=false; default to false so a
        # self-signed lab cert never blocks verification.
        self.verify_ssl = bool(config.get("verify_ssl", False))
        self.base = f"https://{self.host}:{self.port}/api2/json"

        if self.verify_ssl:
            self._ssl = ssl.create_default_context()
        else:
            self._ssl = ssl.create_default_context()
            self._ssl.check_hostname = False
            self._ssl.verify_mode = ssl.CERT_NONE

        self._token = str(config.get("api_token", "") or "").strip()
        self._password = str(config.get("password", "") or "").strip()
        self._user = str(config.get("user", "") or "").strip()
        self._realm = str(config.get("realm", "") or "").strip() or "pam"
        self._ticket = ""  # populated lazily for password auth

        if not self._token and not self._password:
            raise PVEVerifyError("cpi config has neither 'api_token' nor 'password'")

    @classmethod
    def from_config_file(cls, path: str | Path) -> "PVEVerifier":
        p = Path(path)
        try:
            cfg = json.loads(p.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            raise PVEVerifyError(f"cannot read CPI config {p}: {exc}") from exc
        if not isinstance(cfg, dict):
            raise PVEVerifyError(f"CPI config {p} is not a JSON object")
        return cls(cfg)

    # -- auth -----------------------------------------------------------------

    def _username_with_realm(self) -> str:
        # Mirror client.go: only append @realm when the user has no '@' already.
        if "@" in self._user:
            return self._user
        return f"{self._user}@{self._realm}"

    def _ensure_ticket(self) -> None:
        if self._token or self._ticket:
            return
        body = urllib.parse.urlencode(
            {"username": self._username_with_realm(), "password": self._password}
        ).encode("utf-8")
        req = urllib.request.Request(
            f"{self.base}/access/ticket", data=body, method="POST"
        )
        req.add_header("Content-Type", "application/x-www-form-urlencoded")
        try:
            with urllib.request.urlopen(req, context=self._ssl, timeout=30) as resp:
                payload = json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            raise PVEVerifyError(
                f"PVE ticket auth failed: HTTP {exc.code} {exc.reason}"
            ) from exc
        except (urllib.error.URLError, OSError, ValueError) as exc:
            raise PVEVerifyError(f"PVE ticket auth failed: {exc}") from exc
        ticket = (payload.get("data") or {}).get("ticket")
        if not ticket:
            raise PVEVerifyError("PVE ticket auth returned no ticket")
        self._ticket = ticket

    # -- transport ------------------------------------------------------------

    def _get(self, path: str) -> Any:
        """GET <base><path>; return the parsed 'data' field. Raise on any error."""
        self._ensure_ticket()
        req = urllib.request.Request(f"{self.base}{path}", method="GET")
        if self._token:
            req.add_header("Authorization", f"PVEAPIToken={self._token}")
        else:
            req.add_header("Cookie", f"PVEAuthCookie={self._ticket}")
        try:
            with urllib.request.urlopen(req, context=self._ssl, timeout=30) as resp:
                payload = json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            raise PVEVerifyError(
                f"GET {path} failed: HTTP {exc.code} {exc.reason}"
            ) from exc
        except (urllib.error.URLError, OSError, ValueError) as exc:
            raise PVEVerifyError(f"GET {path} failed: {exc}") from exc
        return payload.get("data")

    def _require_node(self) -> str:
        if not self.node:
            raise PVEVerifyError("cpi config missing 'node' — required for this check")
        return self.node

    @staticmethod
    def _as_list(data: Any) -> list[dict[str, Any]]:
        return [e for e in data if isinstance(e, dict)] if isinstance(data, list) else []

    # -- predicates -----------------------------------------------------------

    def vm_exists(self, vmid: int | str) -> bool:
        node = self._require_node()
        want = str(vmid).strip()
        for e in self._as_list(self._get(f"/nodes/{urllib.parse.quote(node)}/qemu")):
            if str(e.get("vmid", "")).strip() == want:
                return True
        return False

    def vnet_exists(self, vnet: str) -> bool:
        for e in self._as_list(self._get("/cluster/sdn/vnets")):
            if e.get("vnet") == vnet:
                return True
        return False

    def zone_exists(self, zone: str) -> bool:
        for e in self._as_list(self._get("/cluster/sdn/zones")):
            if e.get("zone") == zone:
                return True
        return False

    def subnet_present(self, vnet: str, cidr: str) -> bool:
        """True when vnet has a subnet matching cidr.

        Matches against the entry's 'cidr' field first (PVE normalizes the CIDR
        there) and falls back to the 'subnet' id (often '<zone>-<ip>-<mask>').
        Normalizes by stripping whitespace and comparing the CIDR's network/mask
        substring, since PVE may render the id with '-' instead of '/'.
        """
        want = cidr.strip()
        want_dashed = want.replace("/", "-")
        path = f"/cluster/sdn/vnets/{urllib.parse.quote(vnet)}/subnets"
        for e in self._as_list(self._get(path)):
            stored_cidr = str(e.get("cidr", "")).strip()
            stored_id = str(e.get("subnet", "")).strip()
            if stored_cidr == want:
                return True
            if want in stored_id or want_dashed in stored_id:
                return True
        return False

    def bridge_exists(self, iface: str) -> bool:
        node = self._require_node()
        for e in self._as_list(self._get(f"/nodes/{urllib.parse.quote(node)}/network")):
            if e.get("iface") == iface:
                return True
        return False

    def bridge_realized(self, vnet: str) -> bool:
        """Soft check that the SDN apply realized the vnet as a like-named bridge.

        Used warn-only by the harness: realization can lag the config plane
        (ifreload / non-simple zones), so a False here is reported, not fatal.
        """
        return self.bridge_exists(vnet)

    def volume_exists(self, disk_cid: str) -> bool:
        """True when a storage volume with volid == disk_cid exists.

        disk_cid is '<storage>:<volid>'; the storage content listing reports the
        full volid (storage-qualified), so match disk_cid directly.
        """
        node = self._require_node()
        if ":" not in disk_cid:
            raise PVEVerifyError(f"disk_cid {disk_cid!r} is not '<storage>:<volid>'")
        storage = disk_cid.split(":", 1)[0]
        path = (
            f"/nodes/{urllib.parse.quote(node)}"
            f"/storage/{urllib.parse.quote(storage)}/content"
        )
        for e in self._as_list(self._get(path)):
            if e.get("volid") == disk_cid:
                return True
        return False

    # -- assertion helpers ----------------------------------------------------

    def assert_true(self, label: str, cond: bool) -> None:
        if cond:
            print(f"    verify: {label} = ok")
        else:
            raise PVEVerifyError(f"verify FAILED: expected {label!r} to be present")

    def assert_false(self, label: str, cond: bool) -> None:
        if not cond:
            print(f"    verify: {label} = ok (absent)")
        else:
            raise PVEVerifyError(f"verify FAILED: expected {label!r} to be absent")


# ---------------------------------------------------------------------------
# CLI — standalone smoke / debugging
# ---------------------------------------------------------------------------


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="_pve_verify.py",
        description="Independent PVE-state verifier (reads a CPI JSON config).",
    )
    parser.add_argument("--config", required=True, metavar="FILE", help="CPI JSON config")
    sub = parser.add_subparsers(dest="check", required=True)
    sub.add_parser("vm").add_argument("vmid")
    sub.add_parser("vnet").add_argument("vnet")
    sub.add_parser("zone").add_argument("zone")
    p_subnet = sub.add_parser("subnet")
    p_subnet.add_argument("vnet")
    p_subnet.add_argument("cidr")
    sub.add_parser("bridge").add_argument("iface")
    sub.add_parser("volume").add_argument("disk_cid")

    args = parser.parse_args(argv)

    try:
        v = PVEVerifier.from_config_file(args.config)
        if args.check == "vm":
            present = v.vm_exists(args.vmid)
        elif args.check == "vnet":
            present = v.vnet_exists(args.vnet)
        elif args.check == "zone":
            present = v.zone_exists(args.zone)
        elif args.check == "subnet":
            present = v.subnet_present(args.vnet, args.cidr)
        elif args.check == "bridge":
            present = v.bridge_exists(args.iface)
        elif args.check == "volume":
            present = v.volume_exists(args.disk_cid)
        else:  # pragma: no cover - argparse enforces choices
            parser.error(f"unknown check {args.check!r}")
    except PVEVerifyError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    print("present" if present else "absent")
    return 0 if present else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
