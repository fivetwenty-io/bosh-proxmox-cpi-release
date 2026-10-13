"""Checks for the certification run whose one deployment spans two clusters.

scripts/certify gathers an observation after the deployment is up and again
after the upgrade and recreate: the Director's instance table, each cluster's
/cluster/resources?type=vm rows, and the net and drive keys of every
instance's VM. Everything here is a pure function of those observations and
of the cpi-config entries, so the tests run it against canned JSON and the
run itself needs no second code path.

The tag formats are the ones the CPI writes (src/pve_cpi/internal/cpi/handlers/tags.go):

    bosh-owner--<director uuid, lowercased, every byte outside [A-Za-z0-9-] as "-">
    bosh-cluster--<eight lowercase hex digits>   the cluster stamp in the VM CID
    bosh-cid--<eight lowercase hex digits>       the per-VM token in the VM CID

A VM CID is parsed with scripts/_vmcid.py, the twin of the CPI's parser.

Nothing here reads a credential, and no result carries one.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Callable, NamedTuple

import yaml

import _pve_verify
import _upgrade_checks
import _vmcid

OWNER_TAG_PREFIX = "bosh-owner--"
CLUSTER_TAG_PREFIX = "bosh-cluster--"
CID_TAG_PREFIX = "bosh-cid--"

# The AZ the secondary cloud-config layer names. The ops file is static, so
# the AZ it adds is fixed too.
SECONDARY_AZ = "z2"

# Keys of a VM config worth keeping in an observation: the NICs and the
# drives. Nothing else in a config is needed, and some keys carry cloud-init
# material.
_NIC_KEY = re.compile(r"^net\d+$")
_ROOT_DRIVE_ORDER = ("scsi0", "virtio0", "sata0", "ide0")
_DISK_VOLID = re.compile(r"(?:^|:)(?:[^:/]+/)?vm-(\d+)-disk-\d+")
_TAG_SPLIT = re.compile(r"[;,\s]+")
_VAR_REF = re.compile(r"^\(\(([A-Za-z0-9_]+)\)\)$")


class Finding(NamedTuple):
    """One reported check: a step name, whether it held, and why."""

    name: str
    ok: bool
    detail: str


# --------------------------------------------------------------------------- #
# Small parsers
# --------------------------------------------------------------------------- #

def sanitize_tag_value(value: str) -> str:
    """The CPI's sanitizeTagValue: bytes outside [A-Za-z0-9-] become "-"."""
    return re.sub(r"[^A-Za-z0-9-]", "-", value).strip("-")


def owner_tag(director_uuid: str) -> str:
    """The owner tag the CPI stamps for a Director UUID, or "" for none."""
    cleaned = sanitize_tag_value(director_uuid or "").lower()
    return OWNER_TAG_PREFIX + cleaned if cleaned else ""


def sanitize_vm_name(value: str) -> str:
    """The CPI's sanitizeVMName without the 63-byte cap: runs of non-alphanumerics become one "-"."""
    return re.sub(r"[^A-Za-z0-9]+", "-", value).strip("-")


def tag_list(raw: object) -> list[str]:
    """PVE's tags field as a list. PVE joins with ";" and may echo ","."""
    return [t for t in _TAG_SPLIT.split(str(raw or "")) if t]


def tag_values(tags: list[str], prefix: str) -> list[str]:
    """The distinct, sorted values of every tag that starts with prefix."""
    return sorted({t[len(prefix):] for t in tags if t.startswith(prefix) and len(t) > len(prefix)})


def _norm_key(key: object) -> str:
    return re.sub(r"[\s-]+", "_", str(key).strip().lower())


def parse_instances(rows: list[dict]) -> list[dict]:
    """Normalise `bosh instances --details --json` rows for the placement check.

    Each result carries instance, az, vm_cid, and disk_cids. The Director's
    JSON keys are lowercase with underscores, and older CLIs print the column
    titles, so both spellings are read. A row with no VM CID (an instance that
    has no VM yet) is kept with an empty vm_cid so the check can name it.
    """
    out: list[dict] = []
    for row in rows:
        norm = {_norm_key(k): v for k, v in row.items()}
        disks = [p.strip() for p in str(norm.get("disk_cids") or norm.get("disk_cid") or "").split(",") if p.strip()]
        out.append({
            "instance": str(norm.get("instance") or ""),
            "az": str(norm.get("az") or ""),
            "vm_cid": str(norm.get("vm_cid") or ""),
            "disk_cids": disks,
        })
    return out


def thin_resources(rows: object) -> list[dict]:
    """Keep the qemu rows of /cluster/resources?type=vm, with the fields the checks read."""
    out: list[dict] = []
    for row in rows or []:
        if not isinstance(row, dict) or row.get("vmid") is None:
            continue
        if row.get("type", "qemu") != "qemu":
            continue
        out.append({
            "vmid": str(row["vmid"]),
            "node": str(row.get("node") or ""),
            "name": str(row.get("name") or ""),
            "tags": str(row.get("tags") or ""),
        })
    return out


def thin_config(config: object) -> dict:
    """Keep only the NIC and drive keys of a VM config."""
    out: dict = {}
    for key, value in (config or {}).items():
        if _NIC_KEY.match(str(key)) or _upgrade_checks.DRIVE_KEY.match(str(key)):
            out[str(key)] = str(value)
    return out


# --------------------------------------------------------------------------- #
# cpi-config entries
# --------------------------------------------------------------------------- #

def _resolve(value: object, reader: Callable[[str], "str | None"], what: str) -> str:
    """Resolve a literal or a single ((var)) reference to a string."""
    if isinstance(value, str):
        match = _VAR_REF.match(value.strip())
        if match:
            resolved = reader("/" + match.group(1))
            if resolved is None or resolved == "":
                raise ValueError(f"{what} names the variable {match.group(1)}, which no vars file sets")
            return resolved
        return value
    if value is None:
        raise ValueError(f"{what} is not set")
    return str(value)


def _band(pve: dict, start_key: str, end_key: str, entry: str) -> tuple[int, int]:
    try:
        lo, hi = int(pve[start_key]), int(pve[end_key])
    except (KeyError, TypeError, ValueError) as exc:
        raise ValueError(
            f"cpi-config entry {entry} sets no usable {start_key} and {end_key}, "
            "so its band cannot be checked"
        ) from exc
    if lo <= 0 or hi < lo:
        raise ValueError(f"cpi-config entry {entry} has an empty band {lo}-{hi} in {start_key}")
    return lo, hi


def cpi_entry_facts(path: Path, cpi_id: str, reader: Callable[[str], "str | None"]) -> dict:
    """What the placement check needs to know about one cpi-config entry.

    Reads the host, port, VM prefix, and the VM and disk bands from the
    entry in manifests/cpi-config.yml. ((var)) references resolve through
    reader, the layered vars lookup, and the credential variables are not
    read here at all. Raises ValueError, naming the entry, when it is
    missing or lacks a field.
    """
    try:
        doc = yaml.safe_load(path.read_text()) or {}
    except (OSError, yaml.YAMLError) as exc:
        raise ValueError(f"cannot read {path}: {exc}") from exc
    entries = doc.get("cpis") if isinstance(doc, dict) else None
    entry = next((e for e in entries or [] if isinstance(e, dict) and e.get("name") == cpi_id), None)
    if entry is None:
        raise ValueError(f"{path.name} has no cpi-config entry named {cpi_id}")
    pve = (entry.get("properties") or {}).get("pve")
    if not isinstance(pve, dict):
        raise ValueError(f"cpi-config entry {cpi_id} has no properties.pve block")
    facts = {
        "cpi_id": cpi_id,
        "host": _resolve(pve.get("host"), reader, f"cpi-config entry {cpi_id} pve.host"),
        "vm_prefix": "",
        "vm_range": _band(pve, "vmid_range_start", "vmid_range_end", cpi_id),
        "disk_range": _band(pve, "disk_vmid_range_start", "disk_vmid_range_end", cpi_id),
    }
    if pve.get("port") is not None:
        try:
            facts["port"] = int(_resolve(pve["port"], reader, f"cpi-config entry {cpi_id} pve.port"))
        except ValueError as exc:
            raise ValueError(f"cpi-config entry {cpi_id} pve.port is not a number") from exc
    if pve.get("vm_prefix") is not None:
        raw = pve["vm_prefix"]
        match = _VAR_REF.match(str(raw).strip())
        # An unset prefix variable means no prefix, the way the CPI reads it.
        facts["vm_prefix"] = (reader("/" + match.group(1)) or "") if match else str(raw)
    return facts


# --------------------------------------------------------------------------- #
# Placement
# --------------------------------------------------------------------------- #

def _vmids(cluster: dict) -> set[str]:
    return {str(r["vmid"]) for r in cluster.get("resources") or []}


def _row(cluster: dict, vmid: str) -> "dict | None":
    return next((r for r in cluster.get("resources") or [] if str(r["vmid"]) == vmid), None)


def root_drive(config: dict) -> str:
    """The value of the VM's boot drive, or "" when none of the usual slots is set."""
    for key in _ROOT_DRIVE_ORDER:
        if config.get(key):
            return str(config[key])
    return ""


def _drive_storage(value: str) -> str:
    head = value.split(",", 1)[0].strip()
    return head.split(":", 1)[0] if ":" in head else ""


def _disk_birth_vmid(disk_cid: str) -> "int | None":
    """The VMID in the volume name the disk was born under, or None."""
    try:
        volid = _pve_verify._bare_disk_cid(disk_cid)
    except _pve_verify.PVEVerifyError:
        return None
    match = _DISK_VOLID.search(volid)
    return int(match.group(1)) if match else None


def _disk_on_vm(disk_cid: str, config: dict) -> bool:
    """True when a drive of config carries the disk, by serial or by birth volid."""
    stable = _pve_verify.disk_stable_id(disk_cid)
    try:
        birth = _pve_verify._bare_disk_cid(disk_cid)
    except _pve_verify.PVEVerifyError:
        birth = ""
    for key, value in config.items():
        if not _upgrade_checks.DRIVE_KEY.match(key):
            continue
        parts = [p.strip() for p in str(value).split(",")]
        if stable and f"serial={stable}" in parts[1:]:
            return True
        if birth and parts[0] == birth:
            return True
    return False


def _summary(problems: list[str], ok_detail: str) -> tuple[bool, str]:
    if problems:
        return False, "; ".join(problems)
    return True, ok_detail


def check_placement(obs: dict, entries: dict, expect_stamped: bool) -> list[Finding]:
    """Assert where each instance's VM and disk sit, and how the CPI tagged them.

    obs is the observation scripts/certify gathers: director_uuid, instances
    (parse_instances rows), and clusters, keyed by AZ, each with the thinned
    /cluster/resources rows, the thinned config of every instance VM on that
    cluster, and any read error. entries maps each AZ to its cpi-config
    facts (cpi_entry_facts) plus the optional bridge and storage overrides.

    With expect_stamped the VM CIDs must be stamped, which is what the new
    release returns. Before the upgrade the old release returns bare CIDs, and
    those VMs are checked for location, band, and prefix only. A stamped CID
    is held to its tags either way.
    """
    instances = obs.get("instances") or []
    clusters = obs.get("clusters") or {}
    findings: list[Finding] = []

    if not instances:
        return [Finding("placement:instances", False, "the Director reports no instances")]
    unread = [f"{az}: {c.get('error')}" for az, c in sorted(clusters.items()) if c.get("error")]
    missing = sorted(set(entries) - set(clusters))
    if unread or missing:
        detail = "; ".join(unread + [f"{az}: not read" for az in missing])
        return [Finding("placement:cluster-resources", False, f"cannot read every cluster ({detail})")]

    per_az: dict[str, int] = {az: 0 for az in entries}
    problems: dict[str, list[str]] = {
        k: [] for k in ("vm-cluster", "vm-band", "vm-prefix", "tags", "disk-cluster", "disk-band",
                        "bridge", "storage")
    }
    stamps: dict[str, set[str]] = {az: set() for az in entries}
    unchecked_disks = 0
    owner = owner_tag(obs.get("director_uuid", ""))

    for inst in instances:
        label = inst.get("instance") or "?"
        az = inst.get("az") or ""
        if az not in entries:
            problems["vm-cluster"].append(f"{label} is in AZ {az or '?'}, which this run does not configure")
            continue
        per_az[az] += 1
        facts = entries[az]
        cluster = clusters[az]
        try:
            cid = _vmcid.parse(inst.get("vm_cid", ""))
        except ValueError as exc:
            problems["vm-cluster"].append(f"{label} has VM CID {inst.get('vm_cid')!r} ({exc})")
            continue
        vmid = cid.path_id

        # Location: the VM sits on its AZ's cluster and on no other.
        if vmid not in _vmids(cluster):
            problems["vm-cluster"].append(f"{label} VM {vmid} is not on the {az} cluster ({facts['cpi_id']})")
        elsewhere = sorted(o for o, c in clusters.items() if o != az and vmid in _vmids(c))
        if elsewhere:
            problems["vm-cluster"].append(f"{label} VM {vmid} also exists on the {', '.join(elsewhere)} cluster")

        lo, hi = facts["vm_range"]
        if not lo <= cid.vmid <= hi:
            problems["vm-band"].append(f"{label} VM {vmid} is outside {facts['cpi_id']}'s band {lo}-{hi}")

        row = _row(cluster, vmid)
        prefix = sanitize_vm_name(facts.get("vm_prefix", ""))
        if prefix and row is not None:
            name = row.get("name", "")
            if not (name == prefix or name.startswith(prefix + "-")):
                problems["vm-prefix"].append(f"{label} VM {vmid} is named {name!r}, not {prefix}-...")

        # Tags: a stamped CID must match the VM's tags exactly.
        if cid.stamped:
            stamps[az].add(cid.cluster)
            if row is not None:
                tags = tag_list(row.get("tags"))
                if not owner:
                    problems["tags"].append(f"{label} cannot be checked: the Director UUID is unknown")
                elif tag_values(tags, OWNER_TAG_PREFIX) != [owner[len(OWNER_TAG_PREFIX):]]:
                    got = tag_values(tags, OWNER_TAG_PREFIX)
                    problems["tags"].append(
                        f"{label} VM {vmid} carries owner tags {got or 'none'}, want {owner}"
                    )
                if tag_values(tags, CLUSTER_TAG_PREFIX) != [cid.cluster]:
                    problems["tags"].append(
                        f"{label} VM {vmid} carries cluster tags {tag_values(tags, CLUSTER_TAG_PREFIX) or 'none'}, "
                        f"want {CLUSTER_TAG_PREFIX}{cid.cluster}"
                    )
                if tag_values(tags, CID_TAG_PREFIX) != [cid.token]:
                    problems["tags"].append(
                        f"{label} VM {vmid} carries token tags {tag_values(tags, CID_TAG_PREFIX) or 'none'}, "
                        f"want {CID_TAG_PREFIX}{cid.token}"
                    )
        elif expect_stamped:
            problems["tags"].append(f"{label} VM CID {vmid} is a bare CID, but the new release stamps its CIDs")

        # Network and storage names: a distinct override must reach the VM.
        config = (cluster.get("configs") or {}).get(vmid) or {}
        want_bridge = facts.get("bridge", "")
        if want_bridge:
            nics = {k: v for k, v in config.items() if _NIC_KEY.match(k)}
            wrong = sorted(k for k, v in nics.items() if f"bridge={want_bridge}" not in v.split(","))
            if not nics:
                problems["bridge"].append(f"{label} VM {vmid} has no NIC in the config that was read")
            elif wrong:
                problems["bridge"].append(f"{label} VM {vmid} {', '.join(wrong)} is not on bridge {want_bridge}")
        want_storage = facts.get("storage", "")
        if want_storage:
            root = root_drive(config)
            if not root:
                problems["storage"].append(f"{label} VM {vmid} has no boot drive in the config that was read")
            elif _drive_storage(root) != want_storage:
                problems["storage"].append(
                    f"{label} VM {vmid} boots from storage {_drive_storage(root) or '?'}, not {want_storage}"
                )

        # Disks: each persistent disk is a drive of this VM, born in this
        # entry's disk band.
        dlo, dhi = facts["disk_range"]
        for disk in inst.get("disk_cids") or []:
            if not config:
                problems["disk-cluster"].append(f"{label} VM {vmid}: its config was not read, so disk {disk[:20]} is unplaced")
            elif not _disk_on_vm(disk, config):
                problems["disk-cluster"].append(f"{label} disk {disk[:20]} is not a drive of VM {vmid} on the {az} cluster")
            born = _disk_birth_vmid(disk)
            if born is None:
                unchecked_disks += 1
            elif not dlo <= born <= dhi:
                problems["disk-band"].append(f"{label} disk was born as VM {born}, outside {facts['cpi_id']}'s band {dlo}-{dhi}")
        if not inst.get("disk_cids"):
            problems["disk-cluster"].append(f"{label} has no persistent disk")

    empty = sorted(az for az, n in per_az.items() if n == 0)
    findings.append(Finding(
        "placement:spans-azs", not empty and len(instances) >= len(entries),
        f"instances per AZ: {', '.join(f'{az}={n}' for az, n in sorted(per_az.items()))}"
        + (f"; no instance in {', '.join(empty)}" if empty else ""),
    ))

    ok, detail = _summary(problems["vm-cluster"], f"{len(instances)} VMs, each on its AZ's cluster and on no other")
    findings.append(Finding("placement:vm-cluster", ok, detail))
    ok, detail = _summary(problems["vm-band"], "every VMID sits in its entry's band")
    findings.append(Finding("placement:vm-band", ok, detail))
    if any(e.get("vm_prefix") for e in entries.values()):
        ok, detail = _summary(problems["vm-prefix"], "every VM name starts with its entry's vm_prefix")
    else:
        ok, detail = True, "no vm_prefix is configured"
    findings.append(Finding("placement:vm-prefix", ok, detail))
    ok, detail = _summary(problems["tags"], "owner, cluster, and token tags match each stamped CID")
    findings.append(Finding("placement:tags", ok, detail))

    # One stamp per cluster, and a different stamp on each.
    stamp_problems: list[str] = []
    for az, seen in sorted(stamps.items()):
        if len(seen) > 1:
            stamp_problems.append(f"AZ {az} holds VMs with {len(seen)} cluster stamps")
    known = {az: next(iter(s)) for az, s in stamps.items() if len(s) == 1}
    if len(known) > 1 and len(set(known.values())) < len(known):
        stamp_problems.append("two AZs report the same cluster stamp, so they look like one cluster")
    if not known and expect_stamped:
        stamp_problems.append("no instance carries a stamped CID")
    ok, detail = _summary(stamp_problems, "each AZ has its own cluster stamp"
                          if len(known) > 1 else "fewer than two stamped AZs to compare")
    findings.append(Finding("placement:cluster-stamps", ok, detail))

    ok, detail = _summary(problems["disk-cluster"], "every persistent disk is a drive of its instance's VM")
    findings.append(Finding("placement:disk-cluster", ok, detail))
    ok, detail = _summary(
        problems["disk-band"],
        "every disk was born in its entry's disk band"
        + (f" ({unchecked_disks} CIDs carry no vm-N-disk volume name, so their band is unchecked)"
           if unchecked_disks else ""),
    )
    findings.append(Finding("placement:disk-band", ok, detail))

    bridges = {az: e["bridge"] for az, e in entries.items() if e.get("bridge")}
    if bridges:
        ok, detail = _summary(problems["bridge"], f"every NIC is on the override bridge ({', '.join(f'{a}={b}' for a, b in sorted(bridges.items()))})")
    else:
        ok, detail = True, "no bridge override is set"
    findings.append(Finding("placement:bridge-override", ok, detail))
    pools = {az: e["storage"] for az, e in entries.items() if e.get("storage")}
    if pools:
        ok, detail = _summary(problems["storage"], f"every boot drive is on the override storage ({', '.join(f'{a}={s}' for a, s in sorted(pools.items()))})")
    else:
        ok, detail = True, "no storage override is set"
    findings.append(Finding("placement:storage-override", ok, detail))
    return findings


def check_old_vms_gone(pre: dict, post: dict) -> Finding:
    """Every VM the first release created is gone from both clusters after the recreate.

    The first release leaves bare, unstamped VMs on both clusters. The
    recreate under the new release deletes them through its legacy path, so
    none of their VMIDs may still be listed.
    """
    name = "placement:old-vms-deleted"
    old = [i for i in (pre.get("instances") or []) if i.get("vm_cid")]
    if not old:
        return Finding(name, False, "the first observation lists no VMs to look for")
    clusters = post.get("clusters") or {}
    unread = sorted(az for az, c in clusters.items() if c.get("error"))
    if not clusters or unread:
        return Finding(name, False, f"cannot read the clusters after the recreate ({', '.join(unread) or 'none read'})")
    live: dict[str, str] = {}
    for az, cluster in clusters.items():
        for vmid in _vmids(cluster):
            live[vmid] = az
    left: list[str] = []
    for inst in old:
        try:
            vmid = _vmcid.parse(inst["vm_cid"]).path_id
        except ValueError:
            left.append(f"{inst.get('instance') or '?'} has an unreadable VM CID {inst['vm_cid']!r}")
            continue
        if vmid in live:
            left.append(f"{inst.get('instance') or '?'} VM {vmid} is still on the {live[vmid]} cluster")
    return Finding(name, not left, "; ".join(left) if left else f"{len(old)} first-release VMs are gone from both clusters")


# --------------------------------------------------------------------------- #
# Reachability probe
# --------------------------------------------------------------------------- #

_ROUTING_MARKERS = (
    "timed out pinging", "unresponsive agent", "timed out sending", "agent is not responding",
    "no route to host", "network is unreachable", "connection refused", "i/o timeout",
    "failed to download", "unable to download", "blobstore", "nats",
)


def classify_probe_failure(lines: list[str]) -> str:
    """Whether a failed probe deploy reads as a routing gap or as something else.

    The probe VM builds through the secondary entry's CPI, so a failure there
    can be the CPI or the network. An agent that never answers, or never
    reaches the blobstore, points at the route between the networks. Anything
    else is the CPI's or the lab's own error.
    """
    text = "\n".join(lines).lower()
    return "routing" if any(m in text for m in _ROUTING_MARKERS) else "other"
