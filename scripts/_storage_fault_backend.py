"""Fixed Linux controls for an explicitly declared disposable NFS filesystem.

This module is sent over SSH by the fault runner. It never accepts shell hooks.
"""
from __future__ import annotations

import ctypes
from contextlib import contextmanager
import fcntl
import errno
import json
import os
import re
import stat
import socket
import subprocess
import sys
from pathlib import Path

MARKER = ".bosh-storage-fault-fixture.json"
STATE = ".bosh-storage-fault-state.json"
FILL = ".bosh-storage-fault-inodes"
FILL_LOG = ".bosh-storage-fault-inodes.log"


def command(arguments):
    result = subprocess.run(arguments, check=False, capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise RuntimeError("fixed backend control failed: " + arguments[0])
    return result.stdout


def private_json(path, value):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        json.dump(value, stream)
        stream.flush()
        os.fsync(stream.fileno())
        info = os.fstat(stream.fileno())
    directory = os.open(Path(path).parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)
    return {"device": info.st_dev, "inode": info.st_ino}


def read_regular(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor) as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
            raise RuntimeError("backend fixture metadata must be root-owned and not group/world writable")
        return json.load(stream)


def inspect(fixture):
    mount = Path(fixture["mount_path"])
    if not mount.is_absolute() or mount == Path("/") or str(mount.resolve()) != str(mount):
        raise RuntimeError("dedicated mount path is unsafe")
    expected = {"namespace": fixture["namespace"], "nonce": fixture["marker_nonce"], "filesystem_uuid": fixture["filesystem_uuid"]}
    if read_regular(mount / MARKER) != expected:
        raise RuntimeError("disposable backend marker does not match declaration")
    rows = json.loads(command(["findmnt", "--json", "--list", "--output", "TARGET,SOURCE,UUID,FSTYPE,OPTIONS"]))["filesystems"]
    matching = [row for row in rows if row.get("uuid") == fixture["filesystem_uuid"]]
    if len(matching) != 1 or matching[0].get("target") != str(mount):
        raise RuntimeError("filesystem UUID must identify exactly one dedicated mount")
    row = matching[0]
    if row.get("fstype") not in ("ext4", "xfs") or not str(row.get("source", "")).startswith("/dev/"):
        raise RuntimeError("fault control requires a dedicated Linux block filesystem")
    if any(str(other.get("target", "")).startswith(str(mount) + "/") for other in rows):
        raise RuntimeError("nested mounts are not a disposable filesystem")
    local_addresses = {
        address["local"]
        for interface in json.loads(command(["ip", "-json", "address", "show"]))
        for address in interface.get("addr_info", [])
    }
    server_addresses = {answer[4][0] for answer in socket.getaddrinfo(fixture["nfs_server"], None)}
    if not server_addresses or not server_addresses <= local_addresses:
        raise RuntimeError("declared NFS server does not resolve exclusively to this controlled host")
    row["options"] = ",".join(sorted(row["options"].split(",")))
    exports = []
    for line in command(["exportfs", "-s"]).splitlines():
        match = re.fullmatch(r"(\S+)\s+(\S+)\(([^()]*)\)", line.strip())
        if not match:
            raise RuntimeError("cannot prove exact NFS export inventory")
        path, client, options = match.groups()
        if str(Path(path).resolve()) != path:
            raise RuntimeError("NFS export aliases or symlinks prevent dedication proof")
        options = ",".join(sorted(options.split(",")))
        if path == str(mount):
            exports.append({"client": client, "options": options})
        elif path.startswith(str(mount) + "/") or str(mount).startswith(path.rstrip("/") + "/"):
            raise RuntimeError("nested or parent NFS exports share the disposable filesystem")
    return {"mount": row, "exports": exports}


class Quota(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint64) for name in (
        "block_hard", "block_soft", "space", "inode_hard", "inode_soft", "inodes", "block_time", "inode_time"
    )] + [("valid", ctypes.c_uint32)]


def quota(fixture, write=None):
    value = Quota()
    operation = 0x800007
    if write is not None:
        operation = 0x800008
        for key, item in write.items():
            setattr(value, key, item)
    library = ctypes.CDLL(None, use_errno=True)
    library.quotactl.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_void_p]
    library.quotactl.restype = ctypes.c_int
    code = ctypes.c_int(operation << 8).value
    # quotactl addresses the mounted block device, not an arbitrary quota file.
    device = fixture["device"].encode()
    if library.quotactl(code, device, fixture["quota_uid"], ctypes.byref(value)) != 0:
        raise RuntimeError("dedicated filesystem user quota operation failed")
    return {name: getattr(value, name) for name, _ in Quota._fields_}


def squashed_uid(options):
    values = options.split(",")
    if len(values) != len(set(values)) or values.count("all_squash") != 1 or "no_all_squash" in values:
        raise RuntimeError("quota fixture requires unambiguous all-squash export options")
    declared = [value for value in values if value.startswith("anonuid")]
    if not declared:
        # exports(5): omitted anonuid uses the default squashed UID 65534.
        return 65534
    if len(declared) != 1 or not re.fullmatch(r"anonuid=[1-9][0-9]*", declared[0]):
        raise RuntimeError("quota fixture anonymous UID is malformed or ambiguous")
    value = int(declared[0].split("=", 1)[1])
    if value > 2147483647:
        raise RuntimeError("quota fixture anonymous UID is out of range")
    return value


def apply(fixture, fault):
    if fault not in ("backend_export_outage", "backend_read_only", "backend_quota_exhaustion", "backend_inode_exhaustion"):
        raise RuntimeError("unknown fixed backend fault")
    observed = inspect(fixture)
    exports = observed["exports"]
    if len(exports) != 1 or exports[0]["client"] != fixture["export_client"]:
        raise RuntimeError("fault requires exactly the declared NFS export client")
    options = observed["mount"]["options"].split(",")
    if "rw" not in options or "ro" in options:
        raise RuntimeError("backend must start read-write")
    mount = Path(fixture["mount_path"])
    saved = {"fault": fault, "fixture": fixture, "before": observed}
    if fault == "backend_quota_exhaustion":
        if squashed_uid(exports[0]["options"]) != fixture["quota_uid"]:
            raise RuntimeError("quota fixture must map all NFS writes to the declared isolated UID")
        if observed["mount"]["fstype"] != "ext4" or "usrquota" not in options:
            raise RuntimeError("quota fixture requires active ext4 user quotas")
        saved["fixture"] = dict(fixture, device=observed["mount"]["source"])
        saved["quota"] = quota(saved["fixture"])
    if fault == "backend_inode_exhaustion":
        if os.path.lexists(mount / FILL) or os.path.lexists(mount / FILL_LOG):
            raise RuntimeError("inode fault paths already exist; no ownership was acquired")
        info = os.statvfs(mount)
        if info.f_favail <= 0 or info.f_favail > fixture["inode_limit"]:
            raise RuntimeError("free inode count exceeds declared bounded disposable budget")
    state_identity = private_json(mount / STATE, saved)
    try:
        if fault == "backend_export_outage":
            command(["exportfs", "-u", fixture["export_client"] + ":" + str(mount)])
        elif fault == "backend_read_only":
            command(["mount", "-o", "remount,ro", str(mount)])
        elif fault == "backend_quota_exhaustion":
            limits = dict(saved["quota"])
            limits.update(block_hard=max(1, (limits["space"] + 1023) // 1024), block_soft=0, valid=1)
            quota(saved["fixture"], limits)
        elif fault == "backend_inode_exhaustion":
            directory = mount / FILL
            directory.mkdir(mode=0o700)
            info = directory.lstat()
            saved["filler_directory"] = {"device": info.st_dev, "inode": info.st_ino}
            descriptor = os.open(mount / FILL_LOG, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            info = os.fstat(descriptor)
            saved["filler_log"] = {"device": info.st_dev, "inode": info.st_ino}
            # Persist exact ownership before creating any filler entry. A crash
            # before this checkpoint leaves an explicit unowned-directory refusal.
            state_descriptor = os.open(mount / STATE, os.O_WRONLY | os.O_NOFOLLOW)
            with os.fdopen(state_descriptor, "w") as state:
                info = os.fstat(state.fileno())
                if {"device": info.st_dev, "inode": info.st_ino} != state_identity:
                    raise RuntimeError("fault state file was replaced before ownership checkpoint")
                state.truncate(0)
                json.dump(saved, state)
                state.flush()
                os.fsync(state.fileno())
            with os.fdopen(descriptor, "w") as log:
                fill_inodes(directory, fixture["inode_limit"], log)
            if os.statvfs(mount).f_favail != 0:
                raise RuntimeError("bounded fill did not exhaust disposable inodes")
        else:
            raise RuntimeError("unknown fixed backend fault")
        effect = inspect(fixture)
        if fault == "backend_export_outage" and effect["exports"]:
            raise RuntimeError("export outage was not observed")
        if fault == "backend_read_only" and ("ro" not in effect["mount"]["options"].split(",") or "rw" in effect["mount"]["options"].split(",")):
            raise RuntimeError("read-only mount was not observed")
        if fault == "backend_quota_exhaustion":
            current = quota(saved["fixture"])
            if current["block_hard"] != limits["block_hard"] or current["block_soft"] != 0:
                raise RuntimeError("quota exhaustion limit was not observed")
        return {"fault": fault, "before": observed, "effect": effect, "applied": True}
    except BaseException:
        restore(fixture)
        raise


def fill_inodes(directory, limit, log):
    for index in range(limit):
        try:
            fd = os.open(directory / str(index), os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        except OSError as error:
            if error.errno != errno.ENOSPC:
                raise
            break
        try:
            info = os.fstat(fd)
        finally:
            os.close(fd)
        log.write(json.dumps({"name": str(index), "device": info.st_dev, "inode": info.st_ino}) + "\n")
        log.flush()
        os.fsync(log.fileno())


def restore_inodes(mount, saved):
    directory = mount / FILL
    expected = saved.get("filler_directory")
    if expected is None:
        raise RuntimeError("filler directory ownership was not durably recorded")
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_dev != expected["device"] or info.st_ino != expected["inode"]:
        raise RuntimeError("inode filler directory identity changed")
    descriptor = os.open(mount / FILL_LOG, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(descriptor) as log:
        info = os.fstat(log.fileno())
        if {"device": info.st_dev, "inode": info.st_ino} != saved.get("filler_log") or not stat.S_ISREG(info.st_mode):
            raise RuntimeError("inode filler ownership log changed")
        records = [json.loads(line) for line in log]
    entries = {entry["name"]: entry for entry in records}
    if len(entries) != len(records) or set(entries) != {path.name for path in directory.iterdir()}:
        raise RuntimeError("inode filler includes entries without durable ownership evidence")
    for name, expected_file in entries.items():
        if not name.isdecimal():
            raise RuntimeError("inode filler name is invalid")
        info = (directory / name).lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_dev != expected_file["device"] or info.st_ino != expected_file["inode"]:
            raise RuntimeError("inode filler entry was replaced")
    for name in entries:
        (directory / name).unlink()
    directory.rmdir()
    (mount / FILL_LOG).unlink()


def restore(fixture):
    mount = Path(fixture["mount_path"])
    observed = inspect(fixture)
    saved = read_regular(mount / STATE)
    # Saved quota device is derived from mount proof, never operator-supplied.
    declared = dict(saved["fixture"])
    declared.pop("device", None)
    if declared != fixture or saved["before"]["mount"]["source"] != observed["mount"]["source"]:
        raise RuntimeError("backend restoration identity changed")
    fault = saved["fault"]
    before_mount = saved["before"]["mount"]
    expected_mount = dict(before_mount)
    if fault == "backend_read_only":
        expected_mount["options"] = ",".join(sorted("ro" if value == "rw" else value for value in before_mount["options"].split(",")))
    if observed["mount"] not in (before_mount, expected_mount):
        raise RuntimeError("mount state changed during fault; audited restoration required")
    if fault != "backend_export_outage" and observed["exports"] != saved["before"]["exports"]:
        raise RuntimeError("export state changed during fault; audited restoration required")
    if fault == "backend_export_outage":
        before = saved["before"]["exports"]
        if observed["exports"] not in ([], before):
            raise RuntimeError("export changed during outage; audited restoration required")
        entry = before[0]
        command(["exportfs", "-i", "-o", entry["options"], entry["client"] + ":" + str(mount)])
    elif fault == "backend_read_only":
        command(["mount", "-o", "remount,rw", str(mount)])
    elif fault == "backend_quota_exhaustion":
        limits = dict(saved["quota"], valid=1)
        current = quota(saved["fixture"])
        prior = (limits["block_hard"], limits["block_soft"])
        injected = (max(1, (limits["space"] + 1023) // 1024), 0)
        if (current["block_hard"], current["block_soft"]) not in (prior, injected):
            raise RuntimeError("quota limits changed during fault; audited restoration required")
        quota(saved["fixture"], limits)
        current = quota(saved["fixture"])
        if any(current[key] != limits[key] for key in ("block_hard", "block_soft")):
            raise RuntimeError("quota restoration was not observed")
    elif fault == "backend_inode_exhaustion":
        restore_inodes(mount, saved)
    else:
        raise RuntimeError("unknown saved backend fault")
    after = inspect(fixture)
    if after != saved["before"]:
        raise RuntimeError("backend restoration did not match initial mount and exports")
    (mount / STATE).unlink()
    return {"restored": True, "after": after}


@contextmanager
def backend_lock(fixture, action):
    # Recheck dedication before creating even the coordination file.
    inspect(fixture)
    path = Path(fixture["mount_path"]) / ".bosh-storage-fault.lock"
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        if action != "apply":
            raise RuntimeError("fault restoration requires the original coordination lock")
        descriptor = os.open(path, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o077:
            raise RuntimeError("backend lock must be a private root-owned regular file")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise RuntimeError("another fault operation is still running; retain recovery evidence") from error
        yield
    finally:
        os.close(descriptor)


def main():
    request = json.load(sys.stdin)
    if os.geteuid() != 0:
        raise RuntimeError("backend controls require explicit root SSH access")
    action = request["action"]
    fixture = request["fixture"]
    if action == "inspect":
        result = inspect(fixture)
    elif action in ("apply", "restore"):
        with backend_lock(fixture, action):
            if action == "apply":
                result = apply(fixture, request["fault"])
            elif action == "restore":
                result = restore(fixture)
    else:
        raise RuntimeError("unknown backend control action")
    json.dump(result, sys.stdout)


if __name__ == "__main__":
    main()
