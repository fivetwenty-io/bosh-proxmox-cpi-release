"""Python twin of the CPI's VM CID parser (src/pve_cpi/internal/vmcid).

A VM CID is either a bare VMID ("123") or a stamped VMID
("123-0a1b2c3d-4e5f6a7b"), where the two suffixes are the eight-character
cluster stamp and the eight-character per-VM token, each lowercase hex. The
harness scripts hand the CID to the CPI as it came back from create_vm and use
the bare VMID for every PVE API path.

Both parsers read the same vector file,
src/pve_cpi/internal/vmcid/testdata/cid_vectors.json, from their tests, so
the two cannot drift.
"""

from __future__ import annotations

from typing import NamedTuple

STAMP_LEN = 8
UNKNOWN_CLUSTER = "00000000"
# The Go parser's VMID is an int, which is 64 bits on every platform the CPI
# ships for.
_MAX_VMID = (1 << 63) - 1
_HEX = frozenset("0123456789abcdef")


class VMCID(NamedTuple):
    """A parsed VM CID. cluster and token are empty for a bare CID."""

    vmid: int
    cluster: str = ""
    token: str = ""

    @property
    def stamped(self) -> bool:
        return bool(self.cluster or self.token)

    @property
    def path_id(self) -> str:
        """The bare VMID string PVE accepts as a vmid path segment."""
        return str(self.vmid)

    @property
    def known_cluster(self) -> bool:
        """True for a stamped CID whose stamp names a cluster.

        The all-zero stamp is what create_vm writes when it could not read the
        cluster identity, and it names no cluster, like a bare CID.
        """
        return self.stamped and self.cluster != UNKNOWN_CLUSTER

    def with_token(self, token: str) -> "VMCID":
        """Return this CID with another token, keeping the VMID and the stamp.

        Only a stamped CID has a token, so a bare CID raises ValueError, as does
        a token that is not eight lowercase hex characters.
        """
        if not self.stamped:
            raise ValueError("a bare CID has no token to replace")
        if not is_stamp(token):
            raise ValueError(f"token must be {STAMP_LEN} lowercase hex characters")
        return VMCID(self.vmid, self.cluster, token)

    def __str__(self) -> str:
        if not self.stamped:
            return self.path_id
        return f"{self.vmid}-{self.cluster}-{self.token}"


def is_stamp(value: str) -> bool:
    """True when value is exactly eight lowercase hexadecimal characters."""
    return len(value) == STAMP_LEN and all(c in _HEX for c in value)


def _parse_vmid(text: str) -> int:
    if not text:
        raise ValueError("vmid is empty")
    if not all("0" <= c <= "9" for c in text):
        raise ValueError("vmid must be all digits")
    if text[0] == "0":
        raise ValueError("vmid must not have a leading zero")
    value = int(text)
    if value > _MAX_VMID:
        raise ValueError("vmid is out of range")
    return value


def parse(raw: str) -> VMCID:
    """Parse a VM CID, raising ValueError for anything the CPI would reject.

    It accepts a bare positive VMID and "<vmid>-<cluster8>-<token8>" with
    lowercase hexadecimal stamps, and rejects signs, spaces, a zero VMID, a
    leading zero, and a VMID that does not fit a 64-bit int.
    """
    if not isinstance(raw, str):
        raise ValueError(f"a VM CID must be a string, got {type(raw).__name__}")
    if raw == "":
        raise ValueError("empty")
    parts = raw.split("-")
    if len(parts) == 1:
        return VMCID(_parse_vmid(parts[0]))
    if len(parts) == 3:
        vmid = _parse_vmid(parts[0])
        if not is_stamp(parts[1]) or not is_stamp(parts[2]):
            raise ValueError(f"stamp and token must each be {STAMP_LEN} lowercase hex characters")
        return VMCID(vmid, parts[1], parts[2])
    raise ValueError("not a VMID or <vmid>-<cluster>-<token>")


def as_path_id(value: "int | str") -> str:
    """Return the bare VMID for a VMID or a VM CID, for PVE paths and lookups.

    An int is a VMID already. A string the parser accepts gives its VMID. Any
    other string comes back stripped, so a caller that compares it against
    PVE's VMIDs finds no match, as it did before CIDs carried stamps.
    """
    if isinstance(value, bool):
        raise ValueError("a VMID must not be a bool")
    if isinstance(value, int):
        return str(value)
    text = str(value).strip()
    try:
        return parse(text).path_id
    except ValueError:
        return text
