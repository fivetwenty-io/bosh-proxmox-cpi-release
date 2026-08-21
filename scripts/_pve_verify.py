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
    ./scripts/_pve_verify.py --config cpi.json parked pvd-... 90000 90999
Exit 0 when the resource exists, 1 when absent, 2 on usage/transport error.
"""

from __future__ import annotations

import argparse
import base64
import json
import re
import ssl
import sys
import zlib
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


class PVEVerifyError(RuntimeError):
    """Raised on transport, auth, or assertion failure (not on a clean absent)."""


# Decompression cap for pvz- envelopes, mirroring maxDiskCIDEnvelopeBytes in
# internal/pve/disk.go: real envelopes are a few hundred bytes; the cap stops
# a hostile CID from decompression-bombing the verifier.
_MAX_ENVELOPE_BYTES = 64 * 1024


# The BOSH sentinel block the CPI embeds in a VM description, e.g.
# <!--BOSH:{"bosh_parked_disks":{...}}-->  (internal/pve/sentinel.go).
_SENTINEL_RE = re.compile(r"<!--BOSH:(.*?)-->", re.DOTALL)

# QEMU config keys that can reference a storage volume: the bus slots a disk is
# attached through, plus the unusedN keys PVE demotes a volume to when it is
# taken off a bus without being deleted. The parked strategy only ever uses
# scsiN, but a verification that ignored the others could not tell "detached and
# gone" from "detached and left dangling in unused0". "scsihw" must not match,
# so the digit is required.
#
# One pattern, used by both the holder probe and the slot probe: two spellings
# of the same key set drift, and a holder probe that stopped matching what the
# slot probe matches would report a referenced volume as free.
_DISK_SLOT_RE = re.compile(r"^(?:scsi|virtio|ide|sata|unused)\d+$")


# Storage types PVE marks shared by definition, mirroring StorageInfo.IsShared
# in internal/pve/storage_info.go: rbd/cephfs/nfs/cifs/glusterfs/pbs are
# cluster-visible by type; every other type is shared only when storage.cfg
# flags it so (the entry's integer "shared" field).
_SHARED_STORAGE_TYPES = frozenset({"rbd", "cephfs", "nfs", "cifs", "glusterfs", "pbs"})


# Tag separators, mirroring splitTagString in internal/pve/parker.go: PVE writes
# semicolons, but a hand-edited tag field can carry commas or spaces, and a
# separator this splitter does not know turns every parker assertion into a
# vacuous pass rather than a failure.
_TAG_SEP_RE = re.compile(r"[;, ]+")


def _tags_contain(tag_str: str, want: str) -> bool:
    """True when a PVE tag string carries want as a whole tag.

    Separators are ';', ',', and ' ', and matching is case-insensitive, both
    mirroring splitTagString / tagContainsParker in internal/pve/parker.go —
    PVE normalizes tag case, so an exact match would be fragile.
    """
    return any(
        t.strip().lower() == want.lower() for t in _TAG_SEP_RE.split(tag_str) if t.strip()
    )


def _decode_cid_payload(disk_cid: str) -> dict:
    """Decode a pvd-/pvz- envelope CID to its JSON payload dict.

    Mirrors the Go codec's decode order (internal/pve/disk.go
    ParseEncodedDiskCID), which accepts ONLY the two envelope forms below —
    there is no legacy bare-CID or '|'-suffixed fallback; pre-release software
    carries no backward-compatibility requirement, and any other input is a
    hard parse error.

    1. 'pvd-<base64url(json)>' envelope — the bare volid is the payload's "v".
    2. 'pvz-<base64url(gzip(json))>' compressed envelope (opt-in
       disk_cid_compression) — same payload, gzip container, size-capped.
    """
    if disk_cid.startswith("pvd-"):
        payload = disk_cid[len("pvd-"):]
        try:
            # Pre-validate the charset: Go's RawURLEncoding strictly rejects
            # '+', '/', and '=' while Python's urlsafe_b64decode silently
            # tolerates them — enforce parity so both sides fail the same way.
            if not re.fullmatch(r"[A-Za-z0-9_-]+", payload):
                raise ValueError("payload is not unpadded base64url")
            raw = base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4))
            data = json.loads(raw)
            if not isinstance(data, dict):
                raise ValueError("envelope payload is not an object")
            return data
        except (ValueError, KeyError, TypeError) as exc:
            raise PVEVerifyError(
                f"disk_cid {disk_cid!r}: invalid pvd envelope: {exc}"
            ) from exc
    if disk_cid.startswith("pvz-"):
        payload = disk_cid[len("pvz-"):]
        try:
            if not re.fullmatch(r"[A-Za-z0-9_-]+", payload):
                raise ValueError("payload is not unpadded base64url")
            raw = base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4))
            # wbits=16+MAX_WBITS accepts only the gzip container, matching
            # Go's gzip.NewReader. max_length caps the inflation; a stream
            # that fills the cap without reaching EOF is over the limit.
            decomp = zlib.decompressobj(16 + zlib.MAX_WBITS)
            data = decomp.decompress(raw, _MAX_ENVELOPE_BYTES + 1)
            if decomp.unconsumed_tail or len(data) > _MAX_ENVELOPE_BYTES:
                raise ValueError(
                    f"decompressed payload exceeds {_MAX_ENVELOPE_BYTES} bytes"
                )
            obj = json.loads(data)
            if not isinstance(obj, dict):
                raise ValueError("envelope payload is not an object")
            return obj
        except (ValueError, KeyError, TypeError, zlib.error) as exc:
            raise PVEVerifyError(
                f"disk_cid {disk_cid!r}: invalid pvz envelope: {exc}"
            ) from exc
    raise PVEVerifyError(
        f"disk_cid {disk_cid!r} is not a pvd-/pvz- envelope CID (the only forms the CPI emits)"
    )


def _bare_disk_cid(disk_cid: str) -> str:
    """Decode a disk CID to its bare '<storage>:<volid>' form (payload "v")."""
    volid = _decode_cid_payload(disk_cid).get("v")
    if not isinstance(volid, str) or not volid:
        raise PVEVerifyError(f"disk_cid {disk_cid!r}: envelope volid is empty")
    return volid


def disk_stable_id(disk_cid: str) -> str:
    """Return the stable identity token recorded in a CID's metadata, or "".

    The CPI records the token under "m"."id" (DiskCIDMeta.ID in
    internal/pve/disk.go) for disks created under the parked strategy. The
    token also rides the PVE side as a drive serial= option, which is what
    survives the volume rename PVE performs on every move_disk reassignment —
    so identity-aware matching here mirrors the CPI's own resolution order:
    serial first, envelope volid as the birth-record fallback. Best-effort
    like _cid_node: a legacy envelope answers "".
    """
    try:
        meta = _decode_cid_payload(disk_cid).get("m")
    except PVEVerifyError:
        return ""
    if not isinstance(meta, dict):
        return ""
    sid = meta.get("id")
    return sid if isinstance(sid, str) else ""


def _drive_entry_matches(value: str, bare_volid: str, stable_id: str) -> bool:
    """True when a PVE drive value names the disk, by volid or by serial.

    Mirrors internal/pve/disk.go matchDiskIdentity: the value matches when its
    bare volid equals bare_volid, or (for stable-ID disks) when its option
    string carries serial=<stable_id> — the identity that survives the rename
    a reassignment performs.
    """
    parts = value.split(",")
    if parts[0].strip() == bare_volid:
        return True
    if not stable_id:
        return False
    return any(p.strip() == f"serial={stable_id}" for p in parts[1:])


def _cid_node(disk_cid: str) -> str:
    """Return the node recorded in a CID's metadata, or "" when it carries none.

    Path-form CIDs embed the node the volume was created on ("m"."node"), which
    is the node whose storage listing can actually see a node-local volume on a
    multi-node cluster. Best-effort: an envelope without metadata answers "",
    and the caller falls back to scanning wider.
    """
    try:
        meta = _decode_cid_payload(disk_cid).get("m")
    except PVEVerifyError:
        return ""
    if not isinstance(meta, dict):
        return ""
    node = meta.get("node")
    return node if isinstance(node, str) else ""


def _cid_format(disk_cid: str) -> str:
    """Return the disk-image format recorded in a CID's metadata, or "".

    The CPI records the resolved format under "m"."f" (DiskCIDMeta.Format in
    internal/pve/disk.go) so attach_disk can reuse the value the disk was
    created under. Best-effort like _cid_node: a legacy envelope without the
    field answers "", and the caller treats that as "nothing to compare".
    """
    try:
        meta = _decode_cid_payload(disk_cid).get("m")
    except PVEVerifyError:
        return ""
    if not isinstance(meta, dict):
        return ""
    fmt = meta.get("f")
    return fmt if isinstance(fmt, str) else ""


def parse_stemcell_cid(cid: str) -> "tuple[str, str]":
    """Split a bare '<storage>:import/<file>' CID into (storage, volume_path).

    Mirrors internal/pve/stemcell_volume.go ParseStemcellCID. volume_path is
    the full "import/<filename>" tail. Raises PVEVerifyError for an empty
    CID, a CID with no ':' separator, or a path component that does not
    start with "import/".
    """
    if not cid:
        raise PVEVerifyError("parse_stemcell_cid: empty CID")
    idx = cid.find(":")
    if idx < 0:
        raise PVEVerifyError(f"parse_stemcell_cid: CID {cid!r} missing ':' separator")
    storage = cid[:idx]
    volume_path = cid[idx + 1 :]
    if not volume_path.startswith("import/"):
        raise PVEVerifyError(
            f"parse_stemcell_cid: CID {cid!r} volume path {volume_path!r} "
            'does not start with "import/"'
        )
    return storage, volume_path


def parse_stemcell_path_cid(cid: str) -> "tuple[str, str, str]":
    """Validate and decompose a path-identity stemcell CID.

    Mirrors internal/pve/stemcell_volume.go ParseStemcellPathCID with the same
    strictness (same accepted forms, same error cases).

    Accepted forms:
        ":light:<storage>:import/<filename>"
        ":heavy:<storage>:import/<filename>"

    Returns (kind, storage, volume_path); kind is the literal string "light"
    or "heavy". volume_path is the "import/<filename>" tail.

    Raises PVEVerifyError for:
      - empty CID
      - missing leading ':' -- this covers every retired grammar this
        pre-release codebase no longer accepts: "light:...", "template:<vmid>",
        bare "<storage>:import/...", and bare integers
      - unknown kind segment (anything other than "light"/"heavy")
      - doubled prefix (":light::heavy:...") -- the payload after the kind
        must itself parse as "<storage>:import/<filename>", and a storage
        name cannot be empty or contain ':'
      - payload whose path component does not start with "import/"
    """
    if not cid:
        raise PVEVerifyError("parse_stemcell_path_cid: empty CID")
    if not cid.startswith(":"):
        raise PVEVerifyError(
            f"parse_stemcell_path_cid: CID {cid!r} missing leading ':' -- expected "
            '":light:<storage>:import/<file>" or ":heavy:<storage>:import/<file>"'
        )

    light_prefix, heavy_prefix = ":light:", ":heavy:"
    if cid.startswith(light_prefix):
        kind = "light"
        rest = cid[len(light_prefix) :]
    elif cid.startswith(heavy_prefix):
        kind = "heavy"
        rest = cid[len(heavy_prefix) :]
    else:
        raise PVEVerifyError(
            f"parse_stemcell_path_cid: CID {cid!r} has unknown kind -- expected "
            '":light:" or ":heavy:" prefix'
        )

    if rest.startswith(":"):
        raise PVEVerifyError(
            f"parse_stemcell_path_cid: CID {cid!r} has an empty storage segment "
            "(doubled prefix?)"
        )

    try:
        storage, volume_path = parse_stemcell_cid(rest)
    except PVEVerifyError as exc:
        raise PVEVerifyError(
            f"parse_stemcell_path_cid: CID {cid!r} payload invalid: {exc}"
        ) from exc

    return kind, storage, volume_path


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
        """True when vmid exists anywhere in the cluster.

        Cluster-wide, like parker discovery and holder lookup. A parker lands on
        the disk's own node, so a node-scoped presence check on a multi-node
        cluster answers False for a parker that is plainly there -- and the
        assertion built on it reports that the parker was destroyed, which is the
        most alarming way to be wrong about it.
        """
        want = str(vmid).strip()
        # cluster_vms already falls back to the configured node's own listing
        # when /cluster/resources cannot be read, so there is no second fallback
        # to write here -- and an empty result means an empty cluster, which is a
        # real answer rather than a reason to go looking somewhere else.
        return any(str(candidate) == want for candidate, _node in self.cluster_vms())

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
        # type=any_bridge is required to see realized SDN vnet bridges: PVE
        # 9.2's plain node-network listing returns only /etc/network/interfaces
        # entries, and vnets live in interfaces.d/sdn.
        node = self._require_node()
        path = f"/nodes/{urllib.parse.quote(node)}/network?type=any_bridge"
        for e in self._as_list(self._get(path)):
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
        """True when a storage volume with volid == the bare disk_cid exists.

        Thin wrapper over volume_entry, which carries the node-fallback scan
        and the raise-when-every-node-failed rule; see it for the details.
        """
        return self.volume_entry(disk_cid) is not None

    def volume_entry(self, disk_cid: str) -> "dict[str, Any] | None":
        """Return the storage content entry matching disk_cid, or None.

        The CPI emits only pvd- ('pvd-<base64url(json)>') or, opt-in, pvz-
        (gzip-compressed) envelope CIDs, with the bare volid in the payload's
        "v" field. PVE's storage content listing reports only the bare volid,
        so decode the CID to its bare form (mirroring the Go codec's decode
        order in internal/pve/disk.go) before matching. The matched entry is
        returned whole so callers can assert on its other fields (format,
        size) rather than just on presence.
        """
        bare_cid = _bare_disk_cid(disk_cid)
        if ":" not in bare_cid:
            raise PVEVerifyError(f"disk_cid {disk_cid!r} is not '<storage>:<volid>'")
        # A stable-ID disk's volume is renamed by every reassignment; the
        # storage content listing then names it by its CURRENT volid, not the
        # envelope's birth record. Accept either. The storage prefix is stable
        # across renames, so the listing path below stays correct.
        accepted = {bare_cid, self.current_disk_volid(disk_cid)}
        storage = bare_cid.split(":", 1)[0]
        # A node-local storage's content listing is per node, and the CPI places
        # a disk on whichever node has headroom -- not necessarily the configured
        # one. Ask the node the CID's own metadata names first, then every node
        # that hosts a VM, then the configured node, deduplicated in that order.
        # A node whose listing cannot be read is skipped only when another node
        # answers: node-local storage is not defined on every node, so a per-node
        # 404 is expected. When EVERY node fails, the last error is raised
        # rather than answering False -- a 401 that reads as "volume gone" would
        # pass the post-delete absence assertion without proving anything.
        nodes: list[str] = []
        cid_node = _cid_node(disk_cid)
        if cid_node:
            nodes.append(cid_node)
        try:
            for _vmid, vm_node in self.cluster_vms():
                if vm_node not in nodes:
                    nodes.append(vm_node)
        except PVEVerifyError:
            pass
        if self.node and self.node not in nodes:
            nodes.append(self.node)
        if not nodes:
            nodes.append(self._require_node())
        answered = False
        last_err: PVEVerifyError | None = None
        for node in nodes:
            path = (
                f"/nodes/{urllib.parse.quote(node)}"
                f"/storage/{urllib.parse.quote(storage)}/content"
            )
            try:
                entries = self._as_list(self._get(path))
            except PVEVerifyError as exc:
                last_err = exc
                continue
            answered = True
            for e in entries:
                if e.get("volid") in accepted:
                    return e
        if not answered and last_err is not None:
            raise last_err
        return None

    # -- storage classification ----------------------------------------------

    def storage_entry(self, storage: str) -> "dict[str, Any] | None":
        """Return the /storage index entry for the named storage, or None.

        The cluster-wide /storage listing is the same index the CPI's
        StorageInfoCache classifies from, so type/shared answers here match
        what the CPI itself resolved.
        """
        for e in self._as_list(self._get("/storage")):
            if e.get("storage") == storage:
                return e
        return None

    def storage_type(self, storage: str) -> str:
        """Return the storage's PVE type ("rbd", "lvmthin", ...), or ""."""
        entry = self.storage_entry(storage)
        if entry is None:
            return ""
        stype = entry.get("type")
        return str(stype).strip().lower() if isinstance(stype, str) else ""

    def storage_is_shared(self, storage: str) -> bool:
        """True when the storage is cluster-visible ("shared").

        Mirrors StorageInfo.IsShared in internal/pve/storage_info.go: shared
        by type (rbd/cephfs/nfs/cifs/glusterfs/pbs), or explicitly flagged
        shared in storage.cfg — PVE reports the flag as an integer 1/0. A
        storage absent from the index answers False, matching the CPI's
        treat-unknown-as-local safety default.
        """
        entry = self.storage_entry(storage)
        if entry is None:
            return False
        if entry.get("shared") in (1, "1", True):
            return True
        stype = str(entry.get("type", "") or "").strip().lower()
        return stype in _SHARED_STORAGE_TYPES

    def check_volume_format(self, disk_cid: str) -> str:
        """Compare the CID envelope's recorded format with the volume's.

        The CPI records the resolved disk-image format in the CID metadata
        ("m"."f") and attach_disk trusts it, so a recorded format that
        contradicts what PVE actually created (rbd volumes are always raw,
        whatever the CID says) is a CPI defect this check exists to catch.

        Returns "ok" when both sides carry a format and they agree, or a
        "skipped (...)" string when either side carries none — a legacy CID
        predates the field, and some storage plugins elide "format" from
        their content listings; neither is a mismatch. Raises PVEVerifyError
        when the volume is absent or the two formats disagree.
        """
        entry = self.volume_entry(disk_cid)
        if entry is None:
            raise PVEVerifyError(
                f"check_volume_format: no volume found for disk_cid {disk_cid!r}"
            )
        recorded = _cid_format(disk_cid).strip().lower()
        actual_raw = entry.get("format")
        actual = str(actual_raw).strip().lower() if isinstance(actual_raw, str) else ""
        if not recorded:
            return "skipped (CID envelope records no format)"
        if not actual:
            return "skipped (content listing reports no format)"
        if recorded != actual:
            raise PVEVerifyError(
                f"verify FAILED: disk_cid {disk_cid!r} records format {recorded!r} "
                f"but PVE reports the volume as {actual!r}"
            )
        return "ok"

    # -- parked-disk (parker VM) inspection -----------------------------------
    #
    # These read the same cluster state the parked detached-disk strategy
    # writes: a detached persistent disk is attached to a scsi slot of a
    # never-started "parker" VM inside the configured parker VMID band, and the
    # parker records provenance in its description sentinel.
    #
    # Parker discovery and holder lookup are both cluster-wide. A park lands on
    # the disk's own node, which is not necessarily the configured one, so a
    # node-scoped parker scan on a multi-node cluster would classify a real
    # parker as an ordinary VM and turn "no parker was created" into a vacuous
    # pass. The same reasoning applies to holder lookup: a "nobody holds this
    # volume" assertion that only looked at one node would pass while another
    # node still had the disk attached.

    def _node_hosting(self, vmid: int | str) -> str:
        """Return the node hosting vmid, falling back to the configured node.

        A parker lands on the disk's own node, which on a multi-node cluster is
        not necessarily the configured one, so reads that assumed the configured
        node would 404 on a VM that is plainly there.
        """
        try:
            wanted = int(str(vmid).strip())
        except ValueError:
            return self._require_node()
        try:
            listing = self.cluster_vms()
        except PVEVerifyError:
            # No cluster listing to consult. The configured node is the only
            # answer left, and a read that then 404s says so plainly.
            return self._require_node()
        for candidate, node in listing:
            if candidate == wanted:
                return node
        return self._require_node()

    def qemu_config(self, vmid: int | str, node: str | None = None) -> dict[str, Any]:
        """Return the QEMU config of vmid, on its own node unless told otherwise.

        Raises PVEVerifyError when the VM does not exist, so callers that expect
        a VM to be there get a failure rather than an empty dict.
        """
        node = node or self._node_hosting(vmid)
        path = f"/nodes/{urllib.parse.quote(node)}/qemu/{urllib.parse.quote(str(vmid))}/config"
        data = self._get(path)
        if not isinstance(data, dict):
            raise PVEVerifyError(f"qemu config for vmid {vmid} is not an object")
        return data

    def vmids_in_range(self, start: int, end: int) -> list[int]:
        """Return the cluster VMIDs that fall within [start, end], ascending."""
        return sorted(
            vmid for vmid, _node in self.cluster_vms() if start <= vmid <= end
        )

    def parker_vmids(self, start: int, end: int) -> list[int]:
        """Return the parker VMIDs in the cluster, ascending.

        A parker is a VM inside the band that also carries the bosh-parker tag,
        matching pve.IsParkerVM in internal/pve/parker.go: the band alone is not
        enough, or an unrelated VM parked in the same VMID range would be
        mistaken for one.

        A VM whose config cannot be read is not counted. It cannot be proven to
        be a parker, and raising here would fail the whole lifecycle over a VM
        that migrated or went away mid-scan.
        """
        parkers: list[int] = []
        for vmid, node in self.cluster_vms():
            if not (start <= vmid <= end):
                continue
            try:
                config = self.qemu_config(vmid, node)
            except PVEVerifyError:
                continue
            if _tags_contain(str(config.get("tags", "")), "bosh-parker"):
                parkers.append(vmid)
        return sorted(parkers)

    def disk_holders(self, disk_cid: str) -> list[tuple[int, str]]:
        """Return every (vmid, slot) in the CLUSTER whose config references disk_cid.

        Scans every VM on every node, and every disk-bearing config key on each,
        including the unusedN
        slots PVE demotes a volume to when it is removed from a bus without
        being deleted. disk_cid must be a pvd-/pvz- envelope CID, the only form
        the CPI emits — the same requirement volume_exists makes.

        The list is what makes the answer useful: an empty one means the volume
        is free-floating, one entry names its owner, and more than one is a
        double-attach — the corruption the park path's real-VM guard exists to
        prevent — which a single-holder lookup would hide.
        """
        bare = _bare_disk_cid(disk_cid)
        stable_id = disk_stable_id(disk_cid)
        holders: list[tuple[int, str]] = []
        for vmid, node in self.cluster_vms():
            try:
                config = self.qemu_config(vmid, node)
            except PVEVerifyError:
                # The VM went away between the listing and the read, or PVE
                # refused the read while it was locked or migrating. It cannot
                # be proven to hold the disk, and aborting the whole lifecycle
                # run over an unrelated guest would be worse than skipping it.
                # internal/pve/parker.go treats the same case the same way.
                continue
            for key, value in config.items():
                if not _DISK_SLOT_RE.match(key):
                    continue
                if _drive_entry_matches(str(value), bare, stable_id):
                    holders.append((vmid, key))
        return sorted(holders)

    def current_disk_volid(self, disk_cid: str) -> str:
        """Return the volid the cluster currently knows the disk's volume by.

        For a stable-ID disk a move_disk reassignment renames the volume, so
        the envelope volid is only its birth record; the drive entry carrying
        the serial= token names it now. Falls back to the birth volid when no
        holder carries the serial (never transferred, or free-floating).
        """
        bare = _bare_disk_cid(disk_cid)
        stable_id = disk_stable_id(disk_cid)
        if not stable_id:
            return bare
        for vmid, node in self.cluster_vms():
            try:
                config = self.qemu_config(vmid, node)
            except PVEVerifyError:
                continue
            for key, value in config.items():
                if not _DISK_SLOT_RE.match(key):
                    continue
                if _drive_entry_matches(str(value), bare, stable_id):
                    return str(value).split(",", 1)[0].strip()
        return bare

    def cluster_vms(self) -> list[tuple[int, str]]:
        """Return every (vmid, node) VM in the cluster, ascending by VMID.

        Read fresh on every call, deliberately. The lifecycle harness holds one
        verifier for a whole run and asserts on cluster state immediately after
        creating a VM, parking a disk, and deleting a VM; a cached listing turns
        every one of those into an answer about the cluster as it was earlier in
        the run. The handful of extra GETs is not worth a verifier that cannot
        see what it is verifying.

        Uses /cluster/resources so a holder on another node is visible. When the
        token cannot read that endpoint, falls back to the configured node's own
        listing rather than failing: a narrower answer is still worth having,
        and every caller that needs certainty asserts on a non-empty result.
        """
        vms: list[tuple[int, str]] = []
        try:
            entries = self._as_list(self._get("/cluster/resources?type=vm"))
        except PVEVerifyError:
            node = self._require_node()
            entries = [
                dict(e, node=node)
                for e in self._as_list(self._get(f"/nodes/{urllib.parse.quote(node)}/qemu"))
            ]
        for e in entries:
            try:
                vmid = int(str(e.get("vmid", "")).strip())
            except ValueError:
                continue
            # /cluster/resources?type=vm returns LXC containers alongside QEMU
            # guests, and none of these predicates read a container config. A
            # row that elides the field is kept, mirroring the leniency of
            # ListParkersForNode in internal/pve/parker.go.
            kind = str(e.get("type", "") or "").strip()
            if kind and kind != "qemu":
                continue
            node = str(e.get("node", "") or self.node or "")
            if node:
                vms.append((vmid, node))
        return sorted(vms)

    def cluster_nodes(self, online_only: bool = True) -> list[str]:
        """Return the cluster's node names, ascending; online ones by default.

        Reads GET /nodes, the same endpoint `pvesh get /nodes` uses. A node
        whose status is not "online" is excluded unless online_only is False:
        the callers that exist gate multi-node passes (a disk cannot migrate
        to or from a node that is down), so an offline node must not count
        toward "this cluster can move disks between nodes".
        """
        nodes: list[str] = []
        for e in self._as_list(self._get("/nodes")):
            name = str(e.get("node", "") or "").strip()
            if not name:
                continue
            if online_only and str(e.get("status", "") or "").strip() != "online":
                continue
            nodes.append(name)
        return sorted(nodes)

    def parker_disk_slots(self, vmid: int | str) -> dict[str, str]:
        """Return the disk-bearing slots of a VM, keyed by slot.

        Covers the active buses and the unusedN keys PVE demotes a detached
        volume to, because an unused entry is still a reference: destroying the
        VM with --purge takes that volume too. Reads what the VM config actually
        holds rather than the provenance sentinel, which is best-effort and can
        be empty on a parker that holds disks.
        """
        out: dict[str, str] = {}
        for key, val in self.qemu_config(vmid).items():
            if not _DISK_SLOT_RE.match(str(key)):
                continue
            volid = str(val).split(",", 1)[0].strip()
            if volid and volid != "none":
                out[str(key)] = volid
        return out

    def parked_disks(self, vmid: int | str) -> dict[str, Any]:
        """Return the bosh_parked_disks provenance map from a parker's description.

        Empty dict when the parker carries no sentinel or the sentinel holds no
        parked-disk key — provenance is best-effort in the CPI (a park never
        fails because the description write did), so callers must treat an empty
        map as "no record", not as "not parked".
        """
        desc = str(self.qemu_config(vmid).get("description", ""))
        match = _SENTINEL_RE.search(desc)
        if not match:
            return {}
        try:
            sentinel = json.loads(match.group(1))
        except ValueError:
            return {}
        disks = sentinel.get("bosh_parked_disks") if isinstance(sentinel, dict) else None
        return disks if isinstance(disks, dict) else {}

    def parked_disk_recorded(self, vmid: "int | str", disk_cid: str) -> bool:
        """True when a parker's provenance sentinel records this disk.

        Dual-keyed like the CPI's own removal: legacy entries are keyed by the
        bare volid; stable-ID entries are keyed by the bpd- token and name the
        current volid in their "volid" field. All three matches are accepted so
        the harness keeps proving provenance across both generations.
        """
        entries = self.parked_disks(vmid)
        bare = _bare_disk_cid(disk_cid)
        stable_id = disk_stable_id(disk_cid)
        if stable_id and stable_id in entries:
            return True
        if bare in entries:
            return True
        current = self.current_disk_volid(disk_cid)
        for entry in entries.values():
            if isinstance(entry, dict) and entry.get("volid") in (bare, current):
                return True
        return False

    def parked_disk_overlay(self, vmid: "int | str", disk_cid: str) -> dict[str, Any]:
        """Return the drive-option overrides a parker's provenance entry records.

        The CPI stores operator updates made through update_disk in the
        entry's "opts" field while the disk is parked, and merges them into
        the drive string at the next attach. Empty dict when the entry is
        absent or carries no overrides. Same dual-keyed matching as
        parked_disk_recorded.
        """
        entries = self.parked_disks(vmid)
        bare = _bare_disk_cid(disk_cid)
        stable_id = disk_stable_id(disk_cid)
        entry: Any = None
        if stable_id and stable_id in entries:
            entry = entries[stable_id]
        elif bare in entries:
            entry = entries[bare]
        else:
            current = self.current_disk_volid(disk_cid)
            for candidate in entries.values():
                if isinstance(candidate, dict) and candidate.get("volid") in (bare, current):
                    entry = candidate
                    break
        if not isinstance(entry, dict):
            return {}
        opts = entry.get("opts")
        return opts if isinstance(opts, dict) else {}

    def disk_option_overlay(self, vmid: "int | str", disk_cid: str) -> dict[str, Any]:
        """Return the bosh_disk_opt_overlays entry a VM's description records.

        This is the attached-side carrier of the same overrides
        parked_disk_overlay reads from a parker. Empty dict when the sentinel,
        the key, or the entry is absent — the record moves with the disk, so
        an ex-holder legitimately has none.
        """
        desc = str(self.qemu_config(vmid).get("description", ""))
        match = _SENTINEL_RE.search(desc)
        if not match:
            return {}
        try:
            sentinel = json.loads(match.group(1))
        except ValueError:
            return {}
        overlays = sentinel.get("bosh_disk_opt_overlays") if isinstance(sentinel, dict) else None
        if not isinstance(overlays, dict):
            return {}
        for key in (
            disk_stable_id(disk_cid),
            _bare_disk_cid(disk_cid),
            self.current_disk_volid(disk_cid),
        ):
            entry = overlays.get(key) if key else None
            if isinstance(entry, dict):
                return entry
        return {}

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
    p_parked = sub.add_parser("parked")
    p_parked.add_argument("disk_cid")
    p_parked.add_argument("range_start", type=int)
    p_parked.add_argument("range_end", type=int)

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
        elif args.check == "parked":
            holders = v.disk_holders(args.disk_cid)
            parkers = v.parker_vmids(args.range_start, args.range_end)
            present = any(vmid in parkers for vmid, _ in holders)
            for vmid, slot in holders:
                kind = "parker" if vmid in parkers else "vm"
                print(f"held by {kind} {vmid} slot {slot}")
        else:  # pragma: no cover - argparse enforces choices
            parser.error(f"unknown check {args.check!r}")
    except PVEVerifyError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    print("present" if present else "absent")
    return 0 if present else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
