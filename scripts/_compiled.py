"""Helpers for the compiled-release pipeline (``scripts/bosh compile-releases``).

Pure, import-light, unit-testable functions for:

* deriving the canonical compiled-tarball basename for a (release, stemcell) pair,
* constructing the ``file://`` or S3-compatible https reference a director uses to
  fetch that tarball,
* parsing the storage configuration from the environment, and
* generating the create-env ops file that pins the compiled releases.

No network or filesystem side effects live here — callers perform the actual
upload/download and file writes. Keeping the string/þURL logic separate makes the
reserved-name / escaping class of bug (which already cost a clean run once)
testable at desk.
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass

# A compiled tarball is only valid for the exact stemcell it was built against,
# so the stemcell os+version is encoded in the basename. This is the contract
# create-env relies on to decide whether a cached tarball matches.
_SAFE = re.compile(r"[^A-Za-z0-9._-]+")


def _slug(value: str) -> str:
    """Collapse anything outside [A-Za-z0-9._-] to a single '-'.

    Release versions can carry '+' (e.g. dev builds) and names are otherwise
    filesystem-safe, but normalise defensively so the basename is always a
    valid filename and URL path segment.
    """
    return _SAFE.sub("-", value.strip()).strip("-")


def compiled_basename(name: str, version: str, os_name: str, stemcell: str) -> str:
    """Canonical compiled-tarball filename.

    ``<name>-<version>-<os>-<stemcell>.tgz`` — every field slugged. Encoding the
    stemcell makes a mismatch impossible to confuse with a match: a tarball for
    a different stemcell simply has a different name and is not found.
    """
    for label, val in (("name", name), ("version", version), ("os", os_name), ("stemcell", stemcell)):
        if not val or not val.strip():
            raise ValueError(f"compiled_basename: {label} must be non-empty")
    return f"{_slug(name)}-{_slug(version)}-{_slug(os_name)}-{_slug(stemcell)}.tgz"


def file_ref(directory: str, basename: str) -> str:
    """``file://`` URL for a tarball stored locally at ``directory/basename``.

    The directory is made absolute so the reference is valid regardless of the
    cwd create-env runs from.
    """
    if not directory or not directory.strip():
        raise ValueError("file_ref: directory must be non-empty")
    abs_dir = os.path.abspath(os.path.expanduser(directory))
    return "file://" + os.path.join(abs_dir, basename)


def s3_object_key(prefix: str, basename: str) -> str:
    """Object key under an optional prefix (no leading slash, single separators)."""
    p = (prefix or "").strip().strip("/")
    return f"{p}/{basename}" if p else basename


def s3_https_url(endpoint: str, bucket: str, key: str) -> str:
    """Path-style https URL for an S3-compatible object: ``<endpoint>/<bucket>/<key>``.

    Path-style (vs virtual-host) is used because S3-compatible stores (MinIO,
    Ceph RGW) and custom endpoints commonly lack wildcard TLS for
    ``<bucket>.<endpoint>``. The object must be readable by whoever runs
    create-env; private buckets need a presigned URL (not produced here).
    """
    if not endpoint or not endpoint.strip():
        raise ValueError("s3_https_url: endpoint must be non-empty")
    if not bucket or not bucket.strip():
        raise ValueError("s3_https_url: bucket must be non-empty")
    base = endpoint.strip().rstrip("/")
    if "://" not in base:
        base = "https://" + base
    return f"{base}/{bucket.strip().strip('/')}/{key.lstrip('/')}"


@dataclass(frozen=True)
class StoreConfig:
    """Resolved compiled-release storage target.

    kind == "file": tarballs live in ``directory`` (default ``<repo>/compiled_releases``)
    and are referenced as ``file://``.
    kind == "s3": tarballs are uploaded to an S3-compatible bucket and referenced
    via a path-style https URL.
    """

    kind: str  # "file" | "s3"
    directory: str = ""
    s3_endpoint: str = ""
    s3_bucket: str = ""
    s3_region: str = ""
    s3_access_key: str = ""
    s3_secret_key: str = ""
    s3_prefix: str = ""

    def reference(self, basename: str) -> str:
        """The url create-env should use to fetch ``basename`` from this store."""
        if self.kind == "file":
            return file_ref(self.directory, basename)
        if self.kind == "s3":
            return s3_https_url(self.s3_endpoint, self.s3_bucket, s3_object_key(self.s3_prefix, basename))
        raise ValueError(f"StoreConfig.reference: unknown kind {self.kind!r}")


def store_config_from_env(environ: dict[str, str], default_dir: str) -> StoreConfig:
    """Parse COMPILED_RELEASES_* environment into a StoreConfig.

    COMPILED_RELEASES_STORE = file (default) | s3
      file: COMPILED_RELEASES_DIR (default ``default_dir``)
      s3:   COMPILED_RELEASES_S3_{ENDPOINT,BUCKET,REGION,ACCESS_KEY,SECRET_KEY,PREFIX}
            ENDPOINT and BUCKET are required.
    """
    kind = (environ.get("COMPILED_RELEASES_STORE", "file") or "file").strip().lower()
    if kind not in ("file", "s3"):
        raise ValueError(
            f"COMPILED_RELEASES_STORE must be 'file' or 's3', got {kind!r}"
        )
    if kind == "file":
        directory = (environ.get("COMPILED_RELEASES_DIR", "") or "").strip() or default_dir
        return StoreConfig(kind="file", directory=directory)

    endpoint = (environ.get("COMPILED_RELEASES_S3_ENDPOINT", "") or "").strip()
    bucket = (environ.get("COMPILED_RELEASES_S3_BUCKET", "") or "").strip()
    missing = [k for k, v in (("ENDPOINT", endpoint), ("BUCKET", bucket)) if not v]
    if missing:
        raise ValueError(
            "COMPILED_RELEASES_STORE=s3 requires "
            + ", ".join(f"COMPILED_RELEASES_S3_{m}" for m in missing)
        )
    return StoreConfig(
        kind="s3",
        s3_endpoint=endpoint,
        s3_bucket=bucket,
        s3_region=(environ.get("COMPILED_RELEASES_S3_REGION", "") or "").strip(),
        s3_access_key=(environ.get("COMPILED_RELEASES_S3_ACCESS_KEY", "") or "").strip(),
        s3_secret_key=(environ.get("COMPILED_RELEASES_S3_SECRET_KEY", "") or "").strip(),
        s3_prefix=(environ.get("COMPILED_RELEASES_S3_PREFIX", "") or "").strip(),
    )


@dataclass(frozen=True)
class CompiledRelease:
    """One pinned compiled release for the create-env ops file."""

    name: str
    version: str
    url: str
    sha1: str


STEMCELL_MARKER = "# stemcell: "


def stemcell_from_ops(text: str) -> "str | None":
    """Read the ``# stemcell: <os>/<version>`` marker a generated ops file carries.

    create-env uses this to refuse a compiled ops file built for a different
    stemcell than the one it is about to deploy (which would fail to compile or
    mismatch shas). Returns None when no marker is present.
    """
    for line in text.splitlines():
        s = line.strip()
        if s.startswith(STEMCELL_MARKER):
            val = s[len(STEMCELL_MARKER):].strip()
            return val or None
    return None


def compiled_releases_ops(releases: list[CompiledRelease], stemcell: "str | None" = None) -> str:
    """Generate the create-env ops YAML pinning each compiled release.

    Emits one ``replace`` op per release at ``/releases/name=<name>?`` so it is
    insertion-or-replace and order-independent of the source release ops it
    layers after. The ``?`` makes the path optional (create if absent).

    ``stemcell`` (``<os>/<version>``), when given, is recorded as a
    ``# stemcell:`` marker comment so create-env can verify the cache matches the
    stemcell it will deploy. The marker is a YAML comment — inert to ``bosh int``.

    Hand-rolled YAML (no PyYAML dependency — scripts run under bare ``uv``):
    every value here is a controlled literal (release name, semver-ish version,
    a url, a hex/prefixed sha), so no quoting hazard. sha1 values may carry a
    ``sha256:`` prefix (bosh accepts that in the ``sha1`` field), passed through
    verbatim.
    """
    if not releases:
        raise ValueError("compiled_releases_ops: at least one release required")
    seen: set[str] = set()
    lines = [
        "---",
        "# GENERATED by `scripts/bosh compile-releases` — do not edit by hand.",
        "# Pins compiled release tarballs so create-env skips package compilation.",
        "# Valid only for the stemcell these releases were compiled against.",
    ]
    if stemcell and stemcell.strip():
        lines.append(f"{STEMCELL_MARKER}{stemcell.strip()}")
    for r in releases:
        for label, val in (("name", r.name), ("version", r.version), ("url", r.url), ("sha1", r.sha1)):
            if not val or not str(val).strip():
                raise ValueError(f"compiled_releases_ops: release {r.name!r} has empty {label}")
        if r.name in seen:
            raise ValueError(f"compiled_releases_ops: duplicate release {r.name!r}")
        seen.add(r.name)
        lines += [
            "- type: replace",
            f"  path: /releases/name={r.name}?",
            "  value:",
            f"    name: {r.name}",
            f"    version: \"{r.version}\"",
            # Quote the url: a file:// path containing '#' would otherwise be
            # silently truncated as a YAML comment (custom COMPILED_RELEASES_DIR).
            f"    url: \"{r.url}\"",
            f"    sha1: \"{r.sha1}\"",
        ]
    return "\n".join(lines) + "\n"


def local_paths_from_ops(text: str) -> list[str]:
    """Filesystem paths of any ``file://`` release urls in a generated ops file.

    create-env/teardown verify these still exist before layering the cache — a
    ``file://`` url pointing at a deleted tarball would make not just create-env
    but ``delete-env`` fail (and strand the director VM). Quoted or unquoted url
    values are both handled. ``https://`` (S3) refs are not returned: existence
    is not cheaply checkable, and a remote store is the operator's to keep live.
    """
    paths: list[str] = []
    for line in text.splitlines():
        m = re.match(r'\s*url:\s*"?(file://[^"\s]+)"?\s*$', line)
        if m:
            paths.append(m.group(1)[len("file://"):])
    return paths
