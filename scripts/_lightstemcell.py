#!/usr/bin/env python3
"""Shared light-stemcell helper for scripts/{bosh,cf,e2e}.

Makes the light-stemcell feature the basis for every deployment path. The disk
image arrives via PVE storage (light) rather than a BOSH-extracted tarball
(heavy), while the OS and version stay identical to the heavy source so the
compiled-release cache keyed to that stemcell stays valid.

One-time cost, then a fast path:

  1. Resolve (os, version, source tarball url+sha1) from manifests/bosh/vars.yml.
  2. If a prior run cached the result and the qcow2 is still present on PVE
     storage, return immediately — no download, extract, hash, or upload.
  3. Otherwise download the heavy tarball once, extract the disk image,
     normalize it to qcow2, SHA-256 it, and upload it to PVE storage under a
     deterministic name embedding the first 8 hex of the SHA. The SHA is the
     fast-validation anchor and is recorded in a cache sidecar.

Two modes (operator-selectable, preuploaded is the default):

  - preuploaded: the qcow2 is placed on PVE storage; stemcells reference it by
    volid via cloud_properties.image_id.
  - fetch: the qcow2 lives at a remote URL; the CPI streams it to PVE storage
    via cloud_properties.image_url.

create-env is special: it bootstraps the Director by running the CPI locally,
reading stemcell cloud_properties only from a tarball's stemcell.MF (there is
no upload-stemcell step yet). build_create_env_light_tarball() packs a tiny
light tarball (1-byte placeholder image + a stemcell.MF carrying the light
cloud_properties); the CPI's IsLight() short-circuit means the placeholder is
never read.

Pure helpers here are unit-tested in _lightstemcell_test.py. The
network/subprocess functions (download, extract, upload, render, orchestrate)
are exercised by the dry-run and the integration harness.
"""

from __future__ import annotations

import dataclasses
import hashlib
import http.client
import json
import os
import re
import ssl
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable, Iterable

# Reuse the PVE auth + GET plumbing already proven by the integration harness.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _integration  # noqa: E402

SHA8_LEN = 8
PLACEHOLDER_IMAGE = b"\x00"  # create-env tarball image; never read (IsLight short-circuit)
DEFAULT_OS_TYPE = "l26"
DEFAULT_DISK_FORMAT = "qcow2"
DEFAULT_DISK_MIB = 10240
# Must be one of the formats the CPI advertises in info.go's stemcell_formats;
# the Director rejects the upload otherwise. pve-qcow2 is the PVE-native qcow2.
STEMCELL_FORMAT = "pve-qcow2"
# Stemcell agent-settings API version. Must be >= 2 (registry-less contract):
# bosh-cli's create-env gate (cmd/deployment_preparer.go) requires
# stemcellApiVersion >= StemcellNoRegistryAsOfVersion (2) AND cpi api_version ==
# MaxCpiApiVersionSupported (2); a stemcell.MF omitting api_version defaults to 1
# and trips the misleading "requires CPI v2.0 or greater, you are using 2" error.
# 2 matches the CPI's own info.go api_version (the registry-less settings the CPI
# emits and the lifecycle test exercises).
STEMCELL_API_VERSION = 2
PLACEHOLDER_SHA1 = "0" * 40
VALID_MODES = ("preuploaded", "fetch")


@dataclasses.dataclass
class LightStemcell:
    """Result of ensure_light_stemcell.

    manifest_path is the rendered upload-stemcell manifest (preuploaded/fetch);
    create_env_tarball is the file:// tarball for `bosh create-env` and is only
    populated when build_create_env_light_tarball was requested.
    """

    os_name: str
    version: str
    mode: str
    sha256: str
    qcow2_filename: str
    storage: str
    image_id: "str | None" = None
    image_url: "str | None" = None
    manifest_path: "str | None" = None
    create_env_tarball: "str | None" = None
    create_env_tarball_sha1: "str | None" = None


# --------------------------------------------------------------------------- #
# Pure helpers (unit-tested)
# --------------------------------------------------------------------------- #

def qcow2_filename(os_name: str, version: str, sha256: str) -> str:
    """Deterministic on-storage filename. Embeds sha8 so presence can be
    validated by a single storage-content list — no re-download or re-hash."""
    return f"bosh-stemcell-{os_name}-{version}-{sha256[:SHA8_LEN]}.qcow2"


def parse_os_version(stemcell_url: str) -> "tuple[str, str]":
    """Parse (os, version) from a bosh.io stemcell URL.

    e.g. https://bosh.io/d/stemcells/bosh-openstack-kvm-ubuntu-noble?v=1.383
         -> ("ubuntu-noble", "1.383")
    """
    os_match = re.search(r"bosh-[a-z0-9]+-kvm-(?P<os>[a-z0-9]+-[a-z0-9]+)", stemcell_url)
    ver_match = re.search(r"[?&]v=(?P<v>[0-9][0-9.]*)", stemcell_url)
    if not os_match or not ver_match:
        raise ValueError(f"parse_os_version: cannot parse os/version from {stemcell_url!r}")
    return os_match.group("os"), ver_match.group("v")


def import_volid(storage: str, filename: str) -> str:
    """PVE volid for an import-content qcow2: ``<storage>:import/<filename>``."""
    return f"{storage}:import/{filename}"


def find_import_volid(content_rows: Iterable[Any], storage: str, filename: str) -> "str | None":
    """Return the matching volid from parsed PVE storage-content rows, else None.

    Matches on the exact ``<storage>:import/<filename>`` volid so a same-named
    file on a different storage does not produce a mismatched CID.
    """
    target = import_volid(storage, filename)
    for row in content_rows or []:
        if isinstance(row, dict) and row.get("volid") == target:
            return target
    return None


def stemcell_mf(
    os_name: str,
    version: str,
    *,
    image_id: "str | None" = None,
    image_url: "str | None" = None,
    image_url_auth_token: "str | None" = None,
    image_sha1: str = PLACEHOLDER_SHA1,
    sha256: "str | None" = None,
    node: "str | None" = None,
) -> str:
    """Render the stemcell.MF document text for a light stemcell.

    Exactly one of image_id (preuploaded) or image_url (fetch) must be set.
    Returned as plain text so the module carries no YAML dependency; the values
    are controlled (os, version, volid/url) and emit as valid YAML plain
    scalars (colons are followed by non-space). image_sha1 is the sha of the
    tarball's `image` member — placeholder zeros for an upload-stemcell manifest
    (no image), or the real placeholder-image sha for a create-env tarball.
    sha256 is the qcow2's content digest: the CPI REQUIRES it for preuploaded
    (image_id) stemcells — content identity and sha-tag dedup depend on it —
    so it is mandatory when image_id is set. Fetch (image_url) manifests omit
    it; the CPI hashes what it downloads.
    """
    if bool(image_id) == bool(image_url):
        raise ValueError("stemcell_mf: set exactly one of image_id or image_url")
    if image_id and not re.fullmatch(r"[0-9a-f]{64}", sha256 or ""):
        raise ValueError(
            "stemcell_mf: image_id (preuploaded) requires the qcow2's sha256 "
            f"(64 lowercase hex chars), got {sha256!r}"
        )
    name = f"bosh-proxmox-kvm-{os_name}-go_agent-light"
    lines = [
        "---",
        f"name: {name}",
        f'version: "{version}"',
        f"api_version: {STEMCELL_API_VERSION}",
        f"sha1: {image_sha1}",
        f"operating_system: {os_name}",
        "stemcell_formats:",
        f"- {STEMCELL_FORMAT}",
        "cloud_properties:",
    ]
    if image_id:
        lines.append(f"  image_id: {image_id}")
        lines.append(f"  sha256: {sha256}")
    else:
        lines.append(f"  image_url: {image_url}")
        if image_url_auth_token:
            lines.append("  image_url_auth:")
            lines.append("    type: bearer")
            lines.append(f"    bearer_token: {image_url_auth_token}")
    if node:
        # Multi-node clusters with node-local stemcell storage require the
        # pin; single-node clusters and shared storage ignore it harmlessly.
        lines.append(f"  node: {node}")
    lines += [
        f"  name: {os_name}",
        f'  version: "{version}"',
        f"  disk_format: {DEFAULT_DISK_FORMAT}",
        f"  os_type: {DEFAULT_OS_TYPE}",
        f"  disk: {DEFAULT_DISK_MIB}",
    ]
    return "\n".join(lines) + "\n"


def build_create_env_light_tarball(
    dest_dir: "str | Path",
    *,
    os_name: str,
    version: str,
    image_id: "str | None" = None,
    image_url: "str | None" = None,
    image_url_auth_token: "str | None" = None,
    sha256: "str | None" = None,
    node: "str | None" = None,
) -> str:
    """Pack a light stemcell tarball for `bosh create-env`.

    Contains stemcell.MF (light cloud_properties) and a 1-byte placeholder
    `image`. The CPI's IsLight() short-circuit means the placeholder is never
    read. Written deterministically (member + gzip mtime pinned to 0) so a
    re-pack with identical inputs yields an identical tarball.
    """
    dest = Path(dest_dir) / f"light-stemcell-create-env-{os_name}-{version}.tgz"
    # Embed the real sha of the placeholder `image` so the MF is self-consistent
    # even if create-env validates the inner image; the CPI's IsLight()
    # short-circuit means it is never actually read.
    image_sha1 = hashlib.sha1(PLACEHOLDER_IMAGE).hexdigest()
    mf = stemcell_mf(
        os_name, version, image_id=image_id, image_url=image_url,
        image_url_auth_token=image_url_auth_token, image_sha1=image_sha1,
        node=node,
        sha256=sha256,
    ).encode("utf-8")

    dest.parent.mkdir(parents=True, exist_ok=True)
    # Pin gzip mtime to 0 for reproducibility.
    import gzip

    with open(dest, "wb") as raw:
        with gzip.GzipFile(fileobj=raw, mode="wb", mtime=0) as gz:
            with tarfile.open(fileobj=gz, mode="w") as tf:
                _add_bytes(tf, "stemcell.MF", mf)
                _add_bytes(tf, "image", PLACEHOLDER_IMAGE)
    return str(dest)


def _add_bytes(tf: tarfile.TarFile, name: str, data: bytes) -> None:
    import io

    info = tarfile.TarInfo(name=name)
    info.size = len(data)
    info.mtime = 0
    info.mode = 0o644
    tf.addfile(info, io.BytesIO(data))


def file_sha1(path: "str | Path") -> str:
    h = hashlib.sha1()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def file_sha256(path: "str | Path") -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def manifest_vars(
    mode: str,
    *,
    version: str,
    storage: str,
    filename: str,
    image_url: "str | None" = None,
    token: "str | None" = None,
) -> "dict[str, str]":
    """Interpolation vars for the static light-stemcell manifest templates.

    stemcell_version is always supplied so the rendered stemcell record matches
    the standardized version; the OS is fixed to ubuntu-noble in the templates.
    """
    if mode == "preuploaded":
        return {
            "stemcell_version": version,
            "stemcell_storage": storage,
            "stemcell_filename": filename,
        }
    if mode == "fetch":
        return {
            "stemcell_version": version,
            "stemcell_url": image_url or "",
            "stemcell_token": token or "",
        }
    raise ValueError(f"manifest_vars: unknown mode {mode!r} (want one of {VALID_MODES})")


def _bosh_int_opt(vars_path: "str | Path", json_path: str) -> str:
    """Read one var via `bosh int`, returning "" when the key is absent."""
    res = subprocess.run(
        ["bosh", "int", str(vars_path), "--path", json_path],
        capture_output=True, text=True,
    )
    return res.stdout.strip() if res.returncode == 0 else ""


def pve_cfg_from_bosh_vars(
    vars_path: "str | Path",
    *,
    reader: "Callable[[str], str] | None" = None,
) -> dict:
    """Build the minimal PVE config the helper needs from manifests/bosh/vars.yml.

    Reads only the pve_* keys required for storage presence checks and uploads
    (host/port/user/node/storage/auth); verify_ssl is always disabled for the
    lab. reader is injectable for testing; it defaults to `bosh int`.
    """
    g = reader or (lambda p: _bosh_int_opt(vars_path, p))
    cfg: dict = {
        "host": g("/pve_host"),
        "user": g("/pve_user") or "root@pam",
        "node": g("/pve_node"),
        "stemcell_storage": g("/pve_stemcell_storage"),
        "vm_storage": g("/pve_vm_storage"),
        "verify_ssl": False,
    }
    port = g("/pve_port")
    cfg["port"] = int(port) if port.isdigit() else 8006
    token = g("/pve_api_token")
    password = g("/pve_password")
    if token:
        cfg["api_token"] = token
    elif password:
        cfg["password"] = password
    return cfg


def _sidecar_path(cache_dir: "str | Path", filename: str) -> Path:
    return Path(cache_dir) / f"{filename}.sha256.json"


def write_sidecar(
    cache_dir: "str | Path",
    filename: str,
    *,
    sha256: str,
    source_sha1: str,
    os_name: str,
    version: str,
) -> None:
    """Record the validated SHA for audit + fast revalidation."""
    Path(cache_dir).mkdir(parents=True, exist_ok=True)
    _sidecar_path(cache_dir, filename).write_text(
        json.dumps(
            {
                "filename": filename,
                "sha256": sha256,
                "source_sha1": source_sha1,
                "os": os_name,
                "version": version,
            },
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )


def read_sidecar(cache_dir: "str | Path", filename: str) -> "dict | None":
    p = _sidecar_path(cache_dir, filename)
    if not p.exists():
        return None
    try:
        return json.loads(p.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return None


# Source-keyed pointer: maps (os, version, source_sha1) -> validated SHA, so the
# fast path can derive the on-storage filename without re-downloading/hashing.
def _pointer_path(cache_dir: "str | Path", os_name: str, version: str, source_sha1: str) -> Path:
    return Path(cache_dir) / f"source-{os_name}-{version}-{source_sha1[:SHA8_LEN]}.json"


def write_source_pointer(
    cache_dir: "str | Path", *, os_name: str, version: str, source_sha1: str,
    sha256: str, qcow2_file: str,
) -> None:
    Path(cache_dir).mkdir(parents=True, exist_ok=True)
    _pointer_path(cache_dir, os_name, version, source_sha1).write_text(
        json.dumps(
            {"os": os_name, "version": version, "source_sha1": source_sha1,
             "sha256": sha256, "qcow2_filename": qcow2_file},
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )


def read_source_pointer(
    cache_dir: "str | Path", *, os_name: str, version: str, source_sha1: str,
) -> "dict | None":
    p = _pointer_path(cache_dir, os_name, version, source_sha1)
    if not p.exists():
        return None
    try:
        return json.loads(p.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return None


# --------------------------------------------------------------------------- #
# Network / subprocess (integration-tested via dry-run + harness)
# --------------------------------------------------------------------------- #

def download_tarball(
    url: str, source_sha1: str, dest: "str | Path", *, log: Callable[[str], None] = print,
) -> Path:
    """Download url to dest once; verify SHA-1. Reuses an existing valid file."""
    dest = Path(dest)
    if dest.exists() and source_sha1 and file_sha1(dest) == source_sha1:
        log(f"    stemcell source cached: {dest.name}")
        return dest
    dest.parent.mkdir(parents=True, exist_ok=True)
    log(f"    downloading stemcell source: {url}")
    tmp = dest.with_suffix(dest.suffix + ".part")
    # bosh.io 403s the default Python-urllib User-Agent; send a conventional one
    # so the 302 to the storage backend is issued (curl/browsers get 200).
    req = urllib.request.Request(url, headers={"User-Agent": "curl/8.0"})
    with urllib.request.urlopen(req, timeout=300) as resp, open(tmp, "wb") as out:
        for chunk in iter(lambda: resp.read(1 << 20), b""):
            out.write(chunk)
    got = file_sha1(tmp)
    if source_sha1 and got != source_sha1:
        tmp.unlink(missing_ok=True)
        raise RuntimeError(
            f"stemcell source sha1 mismatch for {url}: expected {source_sha1}, got {got}"
        )
    tmp.replace(dest)
    return dest


def extract_qcow2(
    tarball: "str | Path", work_dir: "str | Path", *, log: Callable[[str], None] = print,
) -> "tuple[Path, str]":
    """Extract the disk image from a BOSH stemcell tarball, normalize to qcow2.

    Layout: outer .tgz contains `image` (itself a gzipped tar holding
    `root.img`). root.img is normalized to qcow2 via qemu-img when needed.
    Returns (qcow2_path, sha256).
    """
    work = Path(work_dir)
    work.mkdir(parents=True, exist_ok=True)
    with tarfile.open(tarball, "r:*") as tf:
        member = tf.getmember("image")
        tf.extract(member, path=work)  # noqa: S202 - trusted BOSH stemcell artifact
    image = work / "image"

    # The inner `image` is a (possibly gzipped) tar containing root.img.
    with tarfile.open(image, "r:*") as inner:
        root = next((m for m in inner.getmembers() if m.name.endswith("root.img")), None)
        if root is None:
            raise RuntimeError(f"extract_qcow2: root.img not found inside {image}")
        inner.extract(root, path=work)  # noqa: S202 - trusted BOSH stemcell artifact
    root_img = work / root.name

    qcow2 = work / "disk.qcow2"
    if _qemu_img_format(root_img) == "qcow2":
        root_img.replace(qcow2)
    else:
        log("    converting disk image to qcow2")
        try:
            subprocess.run(
                ["qemu-img", "convert", "-O", "qcow2", str(root_img), str(qcow2)],
                check=True,
            )
        except FileNotFoundError as exc:
            raise RuntimeError(
                "qemu-img not found — install it to derive the light stemcell qcow2 "
                "(macOS: brew install qemu; Debian/Ubuntu: apt-get install qemu-utils)"
            ) from exc
    return qcow2, file_sha256(qcow2)


def _qemu_img_format(path: "str | Path") -> str:
    try:
        out = subprocess.run(
            ["qemu-img", "info", "--output=json", str(path)],
            check=True, capture_output=True, text=True,
        ).stdout
        return str(json.loads(out).get("format", ""))
    except (subprocess.CalledProcessError, FileNotFoundError, json.JSONDecodeError):
        return ""


def multipart_frame(boundary: str, filename: str) -> "tuple[bytes, bytes, bytes]":
    """Build the (preamble, file_head, epilogue) for the PVE upload multipart body.

    PVE's upload endpoint takes exactly one text field (content=import) plus the
    file under field name "filename" (the multipart filename= attribute is the
    destination name). This mirrors the CPI SDK's Storage().Upload. There must be
    exactly ONE part named "filename" (the file) — a second text "filename" field
    would be a duplicate field and corrupt the upload. The file bytes go between
    preamble+file_head and epilogue.
    """
    preamble = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="content"\r\n\r\n'
        f"import\r\n"
    ).encode()
    file_head = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="filename"; filename="{filename}"\r\n'
        f"Content-Type: application/octet-stream\r\n\r\n"
    ).encode()
    epilogue = f"\r\n--{boundary}--\r\n".encode()
    return preamble, file_head, epilogue


def _tls_ctx() -> ssl.SSLContext:
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def _auth_headers(cpi_cfg: dict) -> "dict[str, str]":
    """PVE auth headers for a write request (token or cookie+CSRF)."""
    api_token = cpi_cfg.get("api_token", "")
    password = cpi_cfg.get("password", "")
    if api_token and not api_token.startswith("<dry-run:"):
        return {"Authorization": f"PVEAPIToken={api_token}"}
    if password and not password.startswith("<dry-run:"):
        host, port = cpi_cfg["host"], cpi_cfg.get("port", 8006)
        url = f"https://{host}:{port}/api2/json/access/ticket"
        body = urllib.parse.urlencode(
            {"username": cpi_cfg["user"], "password": password}
        ).encode()
        req = urllib.request.Request(url, data=body, method="POST")
        with urllib.request.urlopen(req, context=_tls_ctx(), timeout=15) as resp:
            data = json.loads(resp.read().decode())["data"]
        return {
            "Cookie": f"PVEAuthCookie={data['ticket']}",
            "CSRFPreventionToken": data["CSRFPreventionToken"],
        }
    raise RuntimeError("upload: no usable PVE credentials (set pve_api_token or pve_password)")


def upload_qcow2(
    cpi_cfg: dict, node: str, storage: str, filename: str, qcow2_path: "str | Path",
    *, log: Callable[[str], None] = print,
) -> None:
    """Stream a qcow2 to PVE storage as import content via multipart POST.

    Mirrors the CPI's uploadStemcellImage endpoint
    (POST /nodes/<node>/storage/<storage>/upload, content=import) so the result
    is identical to a CPI-performed upload. The file is streamed (not buffered)
    and the resulting task is awaited.
    """
    qcow2_path = Path(qcow2_path)
    size = qcow2_path.stat().st_size
    boundary = "----pvelightstemcell" + hashlib.sha1(filename.encode()).hexdigest()[:16]
    headers = _auth_headers(cpi_cfg)

    preamble, file_head, epilogue = multipart_frame(boundary, filename)
    content_length = len(preamble) + len(file_head) + size + len(epilogue)

    host, port = cpi_cfg["host"], cpi_cfg.get("port", 8006)
    path = f"/api2/json/nodes/{urllib.parse.quote(node)}/storage/{urllib.parse.quote(storage)}/upload"
    log(f"    uploading {filename} ({size} bytes) to {storage} on {node}")

    conn = http.client.HTTPSConnection(host, port, context=_tls_ctx(), timeout=900)
    try:
        conn.putrequest("POST", path)
        for k, v in headers.items():
            conn.putheader(k, v)
        conn.putheader("Content-Type", f"multipart/form-data; boundary={boundary}")
        conn.putheader("Content-Length", str(content_length))
        conn.endheaders()
        conn.send(preamble)
        conn.send(file_head)
        with open(qcow2_path, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                conn.send(chunk)
        conn.send(epilogue)
        resp = conn.getresponse()
        raw = resp.read().decode("utf-8", "replace")
        if resp.status >= 300:
            raise RuntimeError(f"upload failed: HTTP {resp.status} {resp.reason}: {raw}")
    finally:
        conn.close()

    upid = ""
    try:
        upid = (json.loads(raw) or {}).get("data") or ""
    except json.JSONDecodeError:
        pass
    if upid:
        _await_task(cpi_cfg, node, upid, log=log)


def _await_task(
    cpi_cfg: dict, node: str, upid: str, *, log: Callable[[str], None] = print,
    timeout_s: int = 900, max_consecutive_failures: int = 5,
) -> None:
    """Poll a PVE task UPID until stopped; raise on non-OK exit status.

    The UPID is passed raw (it contains colons but no slashes), matching the
    CPI SDK's task-status path. Transient pve_api_get errors and persistent
    null statuses are tolerated up to max_consecutive_failures so a brief blip
    does not abandon a possibly-successful upload, but a sustained failure
    surfaces promptly rather than spinning for the full timeout.
    """
    deadline = time.monotonic() + timeout_s
    path = f"/nodes/{urllib.parse.quote(node, safe='')}/tasks/{upid}/status"
    failures = 0
    while time.monotonic() < deadline:
        try:
            status = _integration.pve_api_get(cpi_cfg, path)
        except RuntimeError as exc:
            failures += 1
            if failures >= max_consecutive_failures:
                raise RuntimeError(
                    f"upload task {upid}: status poll failed {failures}x: {exc}"
                ) from exc
            time.sleep(2)
            continue
        if status is None:
            failures += 1
            if failures >= max_consecutive_failures:
                raise RuntimeError(f"upload task {upid}: status unavailable")
            time.sleep(2)
            continue
        failures = 0
        if status.get("status") == "stopped":
            exit_status = status.get("exitstatus", "")
            if exit_status and exit_status != "OK":
                raise RuntimeError(f"upload task {upid} failed: {exit_status}")
            return
        time.sleep(2)
    raise RuntimeError(f"upload task {upid} did not finish within {timeout_s}s")


def render_light_manifest(
    dest_dir: "str | Path",
    mode: str,
    *,
    os_name: str,
    version: str,
    image_id: "str | None" = None,
    image_url: "str | None" = None,
    token: "str | None" = None,
    sha256: "str | None" = None,
    node: "str | None" = None,
) -> str:
    """Write a concrete upload-stemcell manifest to dest_dir and return its path.

    An upload-stemcell manifest is schema-identical to a stemcell.MF, so the
    same generator is used for both the manifest and the create-env tarball.
    The fetch manifest omits the image_url_auth block when no token is set, so
    public (token-less) URLs are not forced through bearer auth.
    """
    text = stemcell_mf(
        os_name, version, image_id=image_id, image_url=image_url,
        image_url_auth_token=token if mode == "fetch" else None,
        sha256=sha256, node=node,
    )
    out = Path(dest_dir) / f"light-stemcell-{mode}.rendered.yml"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(text, encoding="utf-8")
    return str(out)


def list_import_content(cpi_cfg: dict, node: str, storage: str) -> "list[dict]":
    """Return import-content rows for a storage, or [] when unavailable."""
    path = (
        f"/nodes/{urllib.parse.quote(node)}/storage/"
        f"{urllib.parse.quote(storage)}/content?content=import"
    )
    rows = _integration.pve_api_get(cpi_cfg, path, allow_missing=True)
    return rows if isinstance(rows, list) else []


def storage_is_shared(cpi_cfg: dict, storage: str) -> bool:
    """True when the datacenter storage config marks this storage shared.

    Shared stemcell storage must not bake a cloud_properties.node pin into the
    light manifest: a multi-CPI director fans upload-stemcell out to every CPI,
    and a node name from one cluster does not exist in the others (their
    pveproxy answers 596). Each CPI's own config.node is the right query
    target, which is what the CPI falls back to when the pin is absent.

    Fails closed: any API error, absent endpoint, or missing flag reads as not
    shared, keeping the pin (today's behavior) rather than silently dropping it
    for node-local storage. PVE reports the flag as integer 1/0.
    """
    try:
        row = _integration.pve_api_get(
            cpi_cfg, f"/storage/{urllib.parse.quote(storage)}", allow_missing=True,
        )
    except RuntimeError:
        return False
    if not isinstance(row, dict):
        return False
    try:
        return int(row.get("shared", 0)) == 1
    except (TypeError, ValueError):
        return False


def ensure_light_stemcell(
    *,
    cpi_cfg: dict,
    mode: str,
    os_name: str,
    version: str,
    source_url: str,
    source_sha1: str,
    cache_dir: "str | Path",
    image_url: "str | None" = None,
    token: "str | None" = None,
    build_create_env_tarball: bool = False,
    log: Callable[[str], None] = print,
) -> LightStemcell:
    """Ensure the light stemcell is available on PVE and return its descriptors.

    Idempotent. The expensive path (download + extract + hash + upload) runs at
    most once per (os, version, source_sha1); subsequent calls fast-path on the
    cached SHA plus a single storage-content list.

    For fetch mode the qcow2 is not uploaded by this helper (the CPI streams it
    from image_url at create_stemcell time); the helper only renders the
    manifest and, for create-env, the light tarball.
    """
    if mode not in VALID_MODES:
        raise ValueError(f"ensure_light_stemcell: unknown mode {mode!r} (want one of {VALID_MODES})")
    cache_dir = Path(cache_dir)
    node = str(cpi_cfg.get("node", "")).strip()
    storage = str(cpi_cfg.get("stemcell_storage", "")).strip() or str(cpi_cfg.get("vm_storage", "")).strip()
    # node stays the API handle for upload/list; pin is what the manifest
    # carries. Shared storage drops the pin so a multi-CPI director's other
    # clusters resolve their own config.node instead of this cluster's name.
    pin = None if (storage and storage_is_shared(cpi_cfg, storage)) else (node or None)

    if mode == "fetch":
        if not image_url:
            raise RuntimeError("ensure_light_stemcell: fetch mode requires image_url")
        # The CPI fetches + names the qcow2 itself; no local upload. SHA is the
        # remote image's, unknown here, so the on-storage filename is the CPI's
        # concern. Render the fetch manifest and (optionally) the create-env
        # tarball pointing at image_url.
        manifest = render_light_manifest(
            cache_dir, "fetch", os_name=os_name, version=version,
            image_url=image_url, token=token, node=pin,
        )
        result = LightStemcell(
            os_name=os_name, version=version, mode="fetch", sha256="",
            qcow2_filename="", storage=storage, image_url=image_url,
            manifest_path=manifest,
        )
        if build_create_env_tarball:
            tb = build_create_env_light_tarball(
                cache_dir, os_name=os_name, version=version,
                image_url=image_url, image_url_auth_token=token,
                node=pin,
            )
            result.create_env_tarball = tb
            result.create_env_tarball_sha1 = file_sha1(tb)
        return result

    # preuploaded -----------------------------------------------------------
    if not node:
        raise RuntimeError("ensure_light_stemcell: cpi_cfg.node is empty")
    if not storage:
        raise RuntimeError("ensure_light_stemcell: no stemcell/vm storage configured")

    sha256 = ""
    ptr = read_source_pointer(cache_dir, os_name=os_name, version=version, source_sha1=source_sha1)
    if ptr:
        sha256 = ptr.get("sha256", "")
        fname = ptr.get("qcow2_filename", "")
        if sha256 and fname and find_import_volid(list_import_content(cpi_cfg, node, storage), storage, fname):
            log(f"    light stemcell present on {storage}: {fname} (cached)")
            return _preuploaded_result(
                cache_dir, os_name, version, sha256, storage, fname,
                build_create_env_tarball, node=pin,
            )

    # Expensive path — pay once.
    src = download_tarball(source_url, source_sha1, cache_dir / f"source-{os_name}-{version}.tgz", log=log)
    with tempfile.TemporaryDirectory(dir=str(cache_dir)) as work:
        qcow2, sha256 = extract_qcow2(src, work, log=log)
        fname = qcow2_filename(os_name, version, sha256)
        if not find_import_volid(list_import_content(cpi_cfg, node, storage), storage, fname):
            upload_qcow2(cpi_cfg, node, storage, fname, qcow2, log=log)
        else:
            log(f"    light stemcell already on {storage}: {fname}")

    write_sidecar(cache_dir, fname, sha256=sha256, source_sha1=source_sha1, os_name=os_name, version=version)
    write_source_pointer(
        cache_dir, os_name=os_name, version=version, source_sha1=source_sha1,
        sha256=sha256, qcow2_file=fname,
    )
    return _preuploaded_result(
        cache_dir, os_name, version, sha256, storage, fname,
        build_create_env_tarball, node=pin,
    )


def _preuploaded_result(
    cache_dir: "str | Path", os_name: str, version: str,
    sha256: str, storage: str, fname: str, build_create_env_tarball: bool,
    node: "str | None" = None,
) -> LightStemcell:
    volid = import_volid(storage, fname)
    manifest = render_light_manifest(
        cache_dir, "preuploaded", os_name=os_name, version=version, image_id=volid,
        sha256=sha256, node=node,
    )
    result = LightStemcell(
        os_name=os_name, version=version, mode="preuploaded", sha256=sha256,
        qcow2_filename=fname, storage=storage, image_id=volid, manifest_path=manifest,
    )
    if build_create_env_tarball:
        tb = build_create_env_light_tarball(
            cache_dir, os_name=os_name, version=version, image_id=volid,
            sha256=sha256, node=node,
        )
        result.create_env_tarball = tb
        result.create_env_tarball_sha1 = file_sha1(tb)
    return result
