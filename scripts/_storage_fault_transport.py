"""Explicit, immutable pmx binding for the fixed backend fault controller."""
from __future__ import annotations

import copy
import hashlib
import json
import os
import re
from pathlib import Path

KEYS = {
    "kind",
    "binary",
    "config",
    "context",
    "node",
    "identity_file",
    "known_hosts_file",
    "host_key_alias",
}


def validate_transport(value):
    if value is None:
        return
    if not isinstance(value, dict) or set(value) != KEYS or value["kind"] != "pmx":
        raise ValueError("backend transport must be an exact pmx binding")
    for key in ("binary", "config", "identity_file", "known_hosts_file"):
        path = value[key]
        if not isinstance(path, str) or not Path(path).is_absolute() or "\x00" in path or "\n" in path or ".." in Path(path).parts:
            raise ValueError("backend pmx paths must be explicit absolute paths")
    for key in ("context", "node", "host_key_alias"):
        if not isinstance(value[key], str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", value[key]):
            raise ValueError("backend pmx context and node must be explicit identities")


def _digest(path, limit):
    try:
        with Path(path).open("rb") as stream:
            data = stream.read(limit + 1)
        if len(data) > limit or not data:
            raise ValueError("invalid size")
        return hashlib.sha256(data).hexdigest()
    except (OSError, ValueError):
        raise RuntimeError("backend pmx binding file is unavailable or invalid") from None


class PMXBinding:
    def __init__(self, value):
        validate_transport(value)
        self.value = copy.deepcopy(value)
        self.fingerprint = self._fingerprint()

    def _fingerprint(self):
        value = dict(self.value)
        value["binary_sha256"] = _digest(value["binary"], 256 * 1024 * 1024)
        value["config_sha256"] = _digest(value["config"], 4 * 1024 * 1024)
        value["identity_file_sha256"] = _digest(value["identity_file"], 1024 * 1024)
        value["known_hosts_file_sha256"] = _digest(value["known_hosts_file"], 4 * 1024 * 1024)
        return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()

    def command(self, quoted_source):
        if self._fingerprint() != self.fingerprint:
            raise RuntimeError("backend pmx binding changed; original binding required")
        value = self.value
        return [value["binary"], "--config", value["config"], "--context", value["context"],
                "ssh", "--user", "root", "--identity", value["identity_file"], value["node"],
                "-oBatchMode=yes", "-oStrictHostKeyChecking=yes",
                "-oUserKnownHostsFile=" + value["known_hosts_file"],
                "-oHostKeyAlias=" + value["host_key_alias"], "-oConnectTimeout=15", "--",
                "python3", "-c", quoted_source]

    @staticmethod
    def environment():
        return {key: value for key, value in os.environ.items() if not key.startswith("PMX_")}
