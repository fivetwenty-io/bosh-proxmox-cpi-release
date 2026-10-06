"""Pure helpers behind scripts/certify's vars preflight and disk checks.

Nothing here touches a lab. scripts/bosh check-vars and scripts/certify call
these to parse `bosh int --var-errs` output, build the marker scripts they run
on the certification instance over `bosh ssh`, read the marker back, and judge
whether each persistent disk kept its stable serial across the upgrade.
"""

from __future__ import annotations

import base64
import re

# Vars scripts/bosh passes with -v on every create-env and delete-env, so a
# vars file never has to carry them.
SELF_SUPPLIED_VARS = frozenset({
    "director_name",
    "pve_cpi_release_path",
    "pve_cpi_release_name",
})

_EXPECTED = "Expected to find variables:"
_VAR_NAME = re.compile(r"^[A-Za-z0-9_.\-/]+$")


def parse_missing_vars(stderr: str) -> list[str]:
    """Variable names a `bosh int --var-errs` failure reports as missing.

    bosh prints them as an indented list under "Expected to find variables:".
    Some callers echo the same error on one line, comma separated, so both
    shapes are read. Returns [] when the output holds no such error.
    """
    names: list[str] = []
    lines = stderr.splitlines()
    for i, line in enumerate(lines):
        idx = line.find(_EXPECTED)
        if idx < 0:
            continue
        inline = line[idx + len(_EXPECTED):].strip()
        if inline:
            for part in inline.split(","):
                part = part.strip()
                if part and _VAR_NAME.match(part):
                    names.append(part)
            continue
        for follow in lines[i + 1:]:
            stripped = follow.strip()
            if not stripped:
                break
            if not stripped.startswith("- "):
                break
            name = stripped[2:].strip()
            if _VAR_NAME.match(name):
                names.append(name)
    seen: set[str] = set()
    unique = []
    for name in names:
        if name not in seen:
            seen.add(name)
            unique.append(name)
    return unique


def declared_variable_names(variables: object) -> set[str]:
    """Names under a manifest's `variables:` section.

    create-env generates these into the vars-store, so they are never missing
    even though `bosh int --var-errs` without a vars-store reports them.
    """
    if not isinstance(variables, list):
        return set()
    return {
        str(entry["name"]) for entry in variables
        if isinstance(entry, dict) and entry.get("name")
    }


def truly_missing(reported: list[str], declared: set[str]) -> list[str]:
    """Reported names a vars file must supply: not generated, not passed by -v."""
    return [
        name for name in reported
        if name not in declared and name not in SELF_SUPPLIED_VARS
    ]


# --------------------------------------------------------------------------- #
# Persistent-disk marker
# --------------------------------------------------------------------------- #

STORE = "/var/vcap/store"
MARKER_DIR = f"{STORE}/upgrade-marker"
MARKER_BYTES = 1048576
NONCE_RE = re.compile(r"^[0-9a-f]{32}$")
_MARKER_LINE = re.compile(r"MARKER nonce=([0-9a-f]{32}) sha256=([0-9a-f]{64})")
_MARKER_ERROR = re.compile(r"MARKER-ERROR (.+)")


def write_marker_script(nonce: str) -> str:
    """Shell script that writes a nonce and a checksummed data block.

    It refuses when /var/vcap/store is not a mount, because a marker on the
    root disk would survive nothing and prove nothing. sync runs before the
    script reports, so the bytes are on the persistent disk before the
    Director is upgraded underneath the instance.
    """
    if not NONCE_RE.match(nonce):
        raise ValueError(f"nonce must be 32 lowercase hex characters, got {nonce!r}")
    return f"""set -eu
store={STORE}
dir={MARKER_DIR}
if ! mountpoint -q "$store"; then
  echo "MARKER-ERROR $store is not a mounted persistent disk"
  exit 3
fi
mkdir -p "$dir"
head -c {MARKER_BYTES} /dev/urandom > "$dir/data"
printf '%s\\n' {nonce} > "$dir/nonce"
sum=$(sha256sum "$dir/data" | cut -d' ' -f1)
printf '%s\\n' "$sum" > "$dir/data.sha256"
sync
echo "MARKER nonce=$(cat "$dir/nonce") sha256=$sum"
"""


def read_marker_script() -> str:
    """Shell script that reports the marker's nonce and the data's sha256 now."""
    return f"""set -eu
store={STORE}
dir={MARKER_DIR}
if ! mountpoint -q "$store"; then
  echo "MARKER-ERROR $store is not a mounted persistent disk"
  exit 3
fi
if [ ! -f "$dir/data" ] || [ ! -f "$dir/nonce" ]; then
  echo "MARKER-ERROR no marker under $dir"
  exit 4
fi
echo "MARKER nonce=$(cat "$dir/nonce") sha256=$(sha256sum "$dir/data" | cut -d' ' -f1)"
"""


def remote_command(script: str) -> str:
    """A `bosh ssh -c` command that runs script as root.

    The script travels base64 encoded, so no quoting survives or breaks on the
    way through bosh ssh and the remote login shell.
    """
    encoded = base64.b64encode(script.encode("utf-8")).decode("ascii")
    return f"echo {encoded} | base64 -d | sudo bash"


def parse_marker(output: str) -> "tuple[str, str] | None":
    """(nonce, sha256) from a marker script's output, or None."""
    match = _MARKER_LINE.search(output)
    if match is None:
        return None
    return match.group(1), match.group(2)


def marker_error(output: str) -> str:
    """The script's own MARKER-ERROR explanation, or ""."""
    match = _MARKER_ERROR.search(output)
    return match.group(1).strip() if match else ""


# --------------------------------------------------------------------------- #
# Drive serials
# --------------------------------------------------------------------------- #

DRIVE_KEY = re.compile(r"^(?:scsi|virtio|ide|sata)\d+$")
_SERIAL = re.compile(r"(?:^|,)serial=([^,]+)")
STABLE_PREFIX = "bpd-"


def drive_serials(vm_config: dict) -> dict[str, str]:
    """{drive key: serial} for every attached drive that carries serial=."""
    out: dict[str, str] = {}
    for key, value in (vm_config or {}).items():
        if not DRIVE_KEY.match(str(key)):
            continue
        match = _SERIAL.search(str(value))
        if match:
            out[str(key)] = match.group(1)
    return out


def serial_locations(stable_ids: dict[str, str],
                     serials: dict[str, dict[str, str]]) -> dict[str, str]:
    """For each disk CID, the "<vmid>/<drive>" whose serial is its stable id.

    stable_ids maps disk CID to the bpd- id in its envelope; serials maps VM
    CID to that VM's drive serials. A CID with no matching drive maps to "".
    """
    found: dict[str, str] = {}
    for cid, sid in stable_ids.items():
        where = ""
        if sid:
            for vmid in sorted(serials):
                for drive, serial in sorted(serials[vmid].items()):
                    if serial == sid:
                        where = f"{vmid}/{drive}"
                        break
                if where:
                    break
        found[cid] = where
    return found


def describe_serials(stable_ids: dict[str, str], locations: dict[str, str]) -> str:
    """One line per disk for the run report: "bpd-... on 1234/scsi1"."""
    parts = []
    for cid in sorted(stable_ids):
        sid = stable_ids[cid] or "(no stable id)"
        where = locations.get(cid) or "no drive"
        parts.append(f"{sid} on {where}")
    return "; ".join(parts)


def serials_stable(pre: dict, post: dict) -> "tuple[bool, str]":
    """Whether every disk kept a bpd- serial that matches its CID.

    pre and post are capture sections with "stable_ids" (disk CID to stable
    id) and "locations" (disk CID to the drive carrying it). The check holds
    when both captures name the same disks, every stable id is a bpd- id, and
    a drive carried it as serial= both before and after. The second value
    explains the first failure, or is "" when the check holds.
    """
    pre_ids = pre.get("stable_ids") or {}
    post_ids = post.get("stable_ids") or {}
    if not pre_ids or not post_ids:
        return False, "no persistent disks were captured"
    if set(pre_ids) != set(post_ids):
        return False, "the set of disk CIDs changed across the upgrade"
    for cid in sorted(post_ids):
        sid = post_ids[cid]
        if not sid.startswith(STABLE_PREFIX):
            return False, f"disk {cid[:24]}... carries no {STABLE_PREFIX} stable id"
        if pre_ids.get(cid) != sid:
            return False, f"disk {cid[:24]}... changed stable id"
        if not (pre.get("locations") or {}).get(cid):
            return False, f"no drive carried serial={sid} before the upgrade"
        if not (post.get("locations") or {}).get(cid):
            return False, f"no drive carried serial={sid} after the upgrade"
    return True, ""
