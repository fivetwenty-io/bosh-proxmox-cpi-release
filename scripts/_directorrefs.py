"""The record of Director UUIDs a certification slot has created.

Every Director registers its UUID in the `director_refs` of each stemcell cache
template it touches, and nothing removes that ref when the Director is rebuilt
or deleted. The certification flow therefore writes each Director's UUID to a
plain file in the slot directory as soon as the Director is up, and
`scripts/certify cleanup-refs` prunes the refs for every recorded UUID.

The file is `director-uuids`, one UUID per line, append-only while a run is
creating Directors and rewritten only to drop a UUID whose refs were pruned. A
line that is not a UUID is ignored, so nothing but a UUID ever reaches a
command line.
"""
from __future__ import annotations

import os
import re
from pathlib import Path

RECORD_FILE = "director-uuids"

_UUID = re.compile(r"^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$")


def is_uuid(value: str) -> bool:
    return bool(_UUID.match(value.strip()))


def record_path(slot_dir: "str | Path") -> Path:
    return Path(slot_dir) / RECORD_FILE


def load(slot_dir: "str | Path") -> list[str]:
    """The recorded UUIDs in file order, without duplicates or malformed lines."""
    try:
        lines = record_path(slot_dir).read_text(encoding="utf-8").splitlines()
    except OSError:
        return []
    seen: list[str] = []
    for line in lines:
        value = line.strip().lower()
        if is_uuid(value) and value not in seen:
            seen.append(value)
    return seen


def add(slot_dir: "str | Path", uuid: str) -> bool:
    """Append the UUID unless it is already recorded. True when it was added."""
    value = uuid.strip().lower()
    if not is_uuid(value):
        raise ValueError(f"not a Director UUID: {uuid!r}")
    if value in load(slot_dir):
        return False
    path = record_path(slot_dir)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as fh:
        fh.write(value + "\n")
    return True


def remove(slot_dir: "str | Path", uuid: str) -> None:
    """Drop the UUID from the record, leaving the others in place."""
    value = uuid.strip().lower()
    keep = [u for u in load(slot_dir) if u != value]
    path = record_path(slot_dir)
    if not path.exists():
        return
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text("".join(u + "\n" for u in keep), encoding="utf-8")
    os.replace(tmp, path)
