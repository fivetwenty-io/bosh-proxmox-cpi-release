#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["pyyaml"]
# ///
"""Config loader for the integration test harness.

Reads ci/integration.yml, extracts secrets via bosh int, synthesizes the CPI
JSON config, and returns per-tier env dicts consumed by scripts/test.

Import pattern (scripts/test):
    import sys
    from pathlib import Path
    sys.path.insert(0, str(Path(__file__).parent))
    import _integration
"""

from __future__ import annotations

import argparse
import json
import os
import ssl
import subprocess
import sys
import urllib.parse
import urllib.request
from pathlib import Path

import yaml

# ---------------------------------------------------------------------------
# Required config keys
# ---------------------------------------------------------------------------

_REQUIRED_TOP = ("tiers", "bosh_vars", "bosh_creds", "cf_vars", "tier1", "tier2", "tier3")

_REQUIRED_TIER1 = (
    "vmid_range_start",
    "vmid_range_end",
    "network_ip",
    "network_range",
    "network_gateway",
    "network_bridge",
    "vm_cores",
    "vm_memory_mib",
    "disk_size_mib",
)

_REQUIRED_TIER2 = ("bosh_env_alias",)

# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------


def load_config(path: "str | Path", dry_run: bool = False) -> dict:
    """Read and validate ci/integration.yml.

    Args:
        path:    Path to the integration config YAML file.
        dry_run: Accepted for interface consistency; has no effect — validation
                 always runs regardless.

    Returns:
        Parsed config dict.

    Raises:
        SystemExit: Config file missing, unparseable, or required key absent.
    """
    config_path = Path(path)
    if not config_path.exists():
        sys.exit(f"config file not found: {config_path}")

    try:
        raw = config_path.read_text(encoding="utf-8")
        cfg = yaml.safe_load(raw)
    except yaml.YAMLError as exc:
        sys.exit(f"failed to parse config: {config_path}: {exc}")

    if not isinstance(cfg, dict):
        sys.exit(f"config file does not contain a YAML mapping: {config_path}")

    for key in _REQUIRED_TOP:
        if key not in cfg:
            sys.exit(f"missing required config key: {key}  (file: {config_path})")

    tier1 = cfg.get("tier1", {})
    for key in _REQUIRED_TIER1:
        if key not in tier1:
            sys.exit(f"missing required config key: tier1.{key}  (file: {config_path})")

    tier2 = cfg.get("tier2", {})
    for key in _REQUIRED_TIER2:
        if key not in tier2:
            sys.exit(f"missing required config key: tier2.{key}  (file: {config_path})")

    return cfg


def bosh_int(file: "str | Path", path: str, dry_run: bool = False) -> str:
    """Run ``bosh int <file> --path <path>`` and return stripped stdout.

    Args:
        file:    Path to the vars or creds YAML file passed to bosh int.
        path:    bosh-interpolation path, e.g. ``/pve_host``.
        dry_run: When True, return a placeholder string without shelling out.

    Returns:
        Stripped stdout from bosh int, or ``"<dry-run:REDACTED>"`` in dry_run mode.

    Raises:
        SystemExit: bosh int exits nonzero.
    """
    if dry_run:
        return "<dry-run:REDACTED>"

    try:
        result = subprocess.run(
            ["bosh", "int", str(file), "--path", path],
            check=True,
            capture_output=True,
            text=True,
        )
    except subprocess.CalledProcessError as exc:
        stderr = exc.stderr.strip() if exc.stderr else "(no stderr)"
        sys.exit(
            f"bosh int failed for path {path!r} in {file}: {stderr}"
        )
    except FileNotFoundError:
        sys.exit("bosh CLI not found — ensure bosh is installed and on PATH")

    return result.stdout.rstrip("\n")


def build_cpi_config(
    cfg: dict,
    *,
    dry_run: bool = False,
    disk_storage_override: "str | None" = None,
    sdn_zone: "str | None" = None,
    sdn_auto_manage_zone: "bool | None" = None,
    allow_disk_ops_with_snapshots: "bool | None" = None,
) -> dict:
    """Build and return the CPI config dict from cfg without writing any file.

    Pulls PVE secrets from cfg['bosh_vars'] via bosh int.  Auth preference:
    pve_api_token if non-empty, otherwise pve_password.  network_bridge uses
    tier1.network_bridge when non-empty, falling back to pve_network_bridge from
    vars.  verify_ssl is always False for test isolation.  vmid_range_start comes
    from tier1.

    Args:
        cfg:                   Validated config dict from load_config.
        dry_run:               When True, bosh int returns placeholder strings.
        disk_storage_override: When not None, overrides the disk_storage value
                               read from bosh_vars.

    Returns:
        CPI config dict suitable for JSON serialization.

    Raises:
        SystemExit: bosh int fails for any required var.
        ValueError:  pve_port present but not an integer.
    """
    bosh_vars = cfg["bosh_vars"]
    tier1 = cfg["tier1"]

    def _int(path: str) -> str:
        return bosh_int(bosh_vars, path, dry_run=dry_run)

    host = _int("/pve_host")
    port_raw = _int("/pve_port")
    user = _int("/pve_user")
    node = _int("/pve_node")
    vm_storage = _int("/pve_vm_storage")
    disk_storage = _int("/pve_disk_storage")
    if disk_storage_override is not None:
        disk_storage = disk_storage_override
    stemcell_storage = _int("/pve_stemcell_storage")
    iso_storage = _int("/pve_iso_storage")
    network_bridge_pve = _int("/pve_network_bridge")
    verify_ssl_raw = _int("/pve_verify_ssl")

    # Auth: prefer api_token; fall back to password.
    api_token = _int("/pve_api_token")
    password = _int("/pve_password")

    # network_bridge: tier1 config takes precedence over vars.yml value.
    tier1_bridge = str(tier1.get("network_bridge", "")).strip()
    network_bridge = tier1_bridge if tier1_bridge else network_bridge_pve

    # Port coercion — bosh int returns a string; CPI expects int.
    # Absent/empty → default 8006.  Present but non-integer → error.
    if not port_raw or port_raw == "<dry-run:REDACTED>":
        port = 8006
    else:
        try:
            port = int(port_raw)
        except (ValueError, TypeError) as exc:
            raise ValueError(
                f"pve_port value {port_raw!r} in {cfg['bosh_vars']} is not a valid integer"
            ) from exc

    # verify_ssl: always false for test isolation regardless of vars value.
    del verify_ssl_raw  # intentionally ignored

    cpi_cfg: dict = {
        "host": host,
        "port": port,
        "user": user,
        "node": node,
        "vm_storage": vm_storage,
        "disk_storage": disk_storage,
        "stemcell_storage": stemcell_storage,
        "iso_storage": iso_storage,
        "network_bridge": network_bridge,
        "verify_ssl": False,
        "vmid_range_start": int(tier1["vmid_range_start"]),
    }

    # Attach auth — api_token wins if non-empty (and not a dry-run placeholder).
    is_placeholder = api_token.startswith("<dry-run:")
    if api_token and not is_placeholder:
        cpi_cfg["api_token"] = api_token
    elif password and not password.startswith("<dry-run:"):
        cpi_cfg["password"] = password
    else:
        # dry_run mode: include both fields with placeholder values.
        cpi_cfg["api_token"] = api_token
        cpi_cfg["password"] = password

    # Optional SDN knobs (set by the auto network-mode detection for sdn passes).
    # Omitted by default so non-network runs keep the existing config shape.
    if sdn_zone is not None:
        cpi_cfg["sdn_zone"] = sdn_zone
    if sdn_auto_manage_zone is not None:
        cpi_cfg["sdn_auto_manage_zone"] = sdn_auto_manage_zone

    # Optional snapshot-guard bypass (set by the snapshot-detach bypass pass).
    # Omitted by default so non-bypass runs keep the existing config shape.
    if allow_disk_ops_with_snapshots is not None:
        cpi_cfg["allow_disk_ops_with_snapshots"] = allow_disk_ops_with_snapshots

    return cpi_cfg


def write_cpi_config(
    cfg: dict,
    out_path: "str | Path",
    dry_run: bool = False,
    *,
    disk_storage_override: "str | None" = None,
    sdn_zone: "str | None" = None,
    sdn_auto_manage_zone: "bool | None" = None,
    allow_disk_ops_with_snapshots: "bool | None" = None,
) -> str:
    """Synthesize the CPI JSON config and write it to out_path.

    Delegates dict construction to build_cpi_config.  Auth preference:
    pve_api_token if non-empty, otherwise pve_password.  network_bridge uses
    tier1.network_bridge when non-empty, falling back to pve_network_bridge from
    vars.  verify_ssl is always false for test isolation.  vmid_range_start comes
    from tier1.

    Args:
        cfg:                   Validated config dict from load_config.
        out_path:              Destination path for the CPI JSON file.
        dry_run:               When True, skip the disk write and return str(out_path).
        disk_storage_override: When not None, overrides the disk_storage value
                               read from bosh_vars. Used by the multi-storage
                               matrix loop in tier_lifecycle.

    Returns:
        str(out_path).

    Raises:
        SystemExit: bosh int fails for any required var, or out_path not writable.
    """
    cpi_cfg = build_cpi_config(
        cfg,
        dry_run=dry_run,
        disk_storage_override=disk_storage_override,
        sdn_zone=sdn_zone,
        sdn_auto_manage_zone=sdn_auto_manage_zone,
        allow_disk_ops_with_snapshots=allow_disk_ops_with_snapshots,
    )

    if dry_run:
        return str(out_path)

    dest = Path(out_path)
    try:
        dest.write_text(json.dumps(cpi_cfg, indent=2), encoding="utf-8")
    except OSError as exc:
        sys.exit(f"failed to write CPI config to {dest}: {exc}")

    return str(dest)


# ---------------------------------------------------------------------------
# Storage pool detection
# ---------------------------------------------------------------------------

_LOCAL_DISK_TYPES = ("lvm", "lvmthin", "zfspool", "dir")


def select_local_disk_pools(
    entries: "list[dict]",
    types: "tuple[str, ...]" = _LOCAL_DISK_TYPES,
) -> "list[str]":
    """Filter PVE storage entries to local disk pools capable of holding VM images.

    Args:
        entries: List of storage entry dicts from the PVE /api2/json/storage
                 endpoint (the ``data`` array).
        types:   Storage types considered local disk pools.  Defaults to
                 ``("lvm", "lvmthin", "zfspool", "dir")``.

    Returns:
        Sorted, de-duplicated list of storage names that:
        - have a type in ``types``,
        - advertise ``images`` in their ``content`` field (comma-separated),
        - are not disabled (``disable`` absent, falsy, or not one of 1/"1"/True).

    No network calls are made; function is pure and unit-testable.
    """
    seen: set[str] = set()
    result: list[str] = []
    for entry in entries:
        if entry.get("type") not in types:
            continue
        # Disabled check: PVE sets disable=1 (int) or "1" (str) or True.
        disabled = entry.get("disable")
        if disabled in (1, "1", True):
            continue
        content = entry.get("content", "")
        parts = [p.strip() for p in content.split(",")]
        if "images" not in parts:
            continue
        name = entry["storage"]
        if name not in seen:
            seen.add(name)
            result.append(name)
    return sorted(result)


def pve_api_get(cpi_cfg: dict, path: str, *, allow_missing: bool = False) -> "Any":
    """GET an arbitrary PVE API path and return the parsed ``data`` field.

    Auth (reused from cpi_cfg):
    - ``api_token`` set and not a ``<dry-run:`` placeholder -> sends
      ``Authorization: PVEAPIToken=<token>`` header.
    - Else ``password`` set -> POST /access/ticket, then GET with the cookie.
    - Otherwise raises RuntimeError (no usable credentials).

    TLS verification is always disabled (verify_ssl is always False in the
    test harness).

    Args:
        cpi_cfg:       CPI config dict as returned by build_cpi_config.
        path:          API path beginning with '/', e.g. '/storage' or
                       '/cluster/sdn/zones'.
        allow_missing: When True, a 404/501 response (endpoint absent — e.g. SDN
                       not installed) returns None instead of raising. Other
                       errors still raise.

    Returns:
        The parsed ``response["data"]`` value, or None when allow_missing and the
        endpoint is absent.

    Raises:
        RuntimeError: HTTP error, network error, JSON parse error, or missing
                      credentials.
    """
    host = cpi_cfg["host"]
    port = cpi_cfg.get("port", 8006)
    base_url = f"https://{host}:{port}/api2/json"
    timeout = 15

    # Build a TLS context that skips certificate verification.
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE

    api_token = cpi_cfg.get("api_token", "")
    password = cpi_cfg.get("password", "")

    is_token_placeholder = bool(api_token and api_token.startswith("<dry-run:"))
    is_password_placeholder = bool(password and password.startswith("<dry-run:"))

    extra_headers: "dict[str, str]" = {}
    if api_token and not is_token_placeholder:
        extra_headers["Authorization"] = f"PVEAPIToken={api_token}"
    elif password and not is_password_placeholder:
        # Password auth: POST to /access/ticket, then use the cookie.
        ticket_url = f"{base_url}/access/ticket"
        body = urllib.parse.urlencode({"username": cpi_cfg["user"], "password": password}).encode()
        ticket_req = urllib.request.Request(ticket_url, data=body, method="POST")
        try:
            with urllib.request.urlopen(ticket_req, context=ctx, timeout=timeout) as resp:
                ticket_raw = resp.read().decode("utf-8")
        except urllib.error.HTTPError as exc:
            raise RuntimeError(
                f"PVE ticket auth to {host}:{port} failed: HTTP {exc.code} {exc.reason}"
            ) from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(
                f"PVE ticket auth to {host}:{port} failed: {exc.reason}"
            ) from exc
        try:
            ticket_resp = json.loads(ticket_raw)
            ticket = ticket_resp["data"]["ticket"]
            csrf = ticket_resp["data"]["CSRFPreventionToken"]
        except (KeyError, TypeError, json.JSONDecodeError) as exc:
            raise RuntimeError(
                f"PVE ticket response from {host}:{port} missing expected fields: {exc}"
            ) from exc
        extra_headers["Cookie"] = f"PVEAuthCookie={ticket}"
        extra_headers["CSRFPreventionToken"] = csrf
    else:
        raise RuntimeError(
            f"No usable credentials for PVE API at {host}:{port}: "
            "set pve_api_token or pve_password in bosh_vars."
        )

    req = urllib.request.Request(f"{base_url}{path}", headers=extra_headers)
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        if allow_missing and exc.code in (404, 501):
            return None
        raise RuntimeError(
            f"PVE GET {path} on {host}:{port} failed: HTTP {exc.code} {exc.reason}"
        ) from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(
            f"PVE GET {path} on {host}:{port} failed: {exc.reason}"
        ) from exc

    try:
        parsed = json.loads(raw)
        return parsed["data"]
    except (json.JSONDecodeError, KeyError, TypeError) as exc:
        raise RuntimeError(
            f"Unexpected PVE response for {path} from {host}:{port}: {exc}"
        ) from exc


def fetch_storage_index(cpi_cfg: dict) -> "list[dict]":
    """Query PVE /storage and return the data array.

    Thin wrapper over pve_api_get; see it for auth details.

    Args:
        cpi_cfg: CPI config dict as returned by build_cpi_config.

    Returns:
        List of storage entry dicts from PVE (``response["data"]``).

    Raises:
        RuntimeError: HTTP error, network error, JSON parse error, or
                      unexpected response structure.
    """
    host = cpi_cfg["host"]
    port = cpi_cfg.get("port", 8006)
    data = pve_api_get(cpi_cfg, "/storage")
    if not isinstance(data, list):
        raise RuntimeError(
            f"PVE storage index data from {host}:{port} is not a list (got {type(data).__name__})"
        )
    return data


def detect_disk_storage_pools(cfg: dict) -> "list[str]":
    """Autodetect local disk storage pools on the PVE host.

    Calls build_cpi_config to resolve connection details, fetches the PVE
    storage index via the API, and filters to local image-capable pools.

    Args:
        cfg: Validated config dict from load_config.

    Returns:
        Sorted list of storage pool names suitable for disk_storage_override.

    Raises:
        RuntimeError: Network error, auth failure, or malformed response.
    """
    cpi_cfg = build_cpi_config(cfg, dry_run=False)
    entries = fetch_storage_index(cpi_cfg)
    return select_local_disk_pools(entries)


# ---------------------------------------------------------------------------
# Network mode detection
# ---------------------------------------------------------------------------


def select_network_modes(
    *,
    sdn_installed: bool,
    existing_zones: "list[str]",
    sdn_cfg: dict,
    bridge_iface: str,
    bridge_exists: bool,
) -> "list[dict]":
    """Decide which network-test passes to run from detected capabilities.

    Pure function (no I/O) so it is unit-testable. Returns a list of pass
    descriptors, each a dict with:

        mode  — "sdn" or "bridge" (value for NETWORK_TEST_MODE)
        env   — extra env vars to inject for that pass (e.g. SDN_ZONE)
        cpi   — CPI-config overrides for that pass (sdn_zone / sdn_auto_manage_zone);
                empty dict means reuse the default synthesized CPI config

    Policy ("both whenever possible"):
      - bridge: always runnable on any node, so included whenever a target iface
        is configured AND not already present (we never create+delete a bridge
        we do not own).
      - sdn: included only when SDN is installed, the lifecycle SDN parameters
        (vnet/range/gateway/ip) are configured, and a usable zone is resolvable:
          * configured zone that already exists -> reuse it (pin sdn_zone, leave
            auto-manage OFF so delete_network never removes a pre-existing zone);
          * no configured zone but zones exist -> adopt the first (same pin);
          * configured zone that is absent -> create+teardown it (auto-manage ON,
            sdn_zone left unset so the teardown pin-rule does not block deletion).
        When SDN is installed but no zone is resolvable, sdn is skipped.
    """
    passes: "list[dict]" = []

    if bridge_iface and not bridge_exists:
        passes.append({"mode": "bridge", "env": {}, "cpi": {}})

    if sdn_installed:
        params_ok = all(str(sdn_cfg.get(k, "")).strip() for k in ("vnet", "range", "gateway", "ip"))
        if params_ok:
            zone_cfg = str(sdn_cfg.get("zone", "")).strip()
            if zone_cfg and zone_cfg in existing_zones:
                passes.append({
                    "mode": "sdn",
                    "env": {"SDN_ZONE": zone_cfg},
                    "cpi": {"sdn_zone": zone_cfg, "sdn_auto_manage_zone": False},
                })
            elif not zone_cfg and existing_zones:
                adopted = sorted(existing_zones)[0]
                passes.append({
                    "mode": "sdn",
                    "env": {"SDN_ZONE": adopted},
                    "cpi": {"sdn_zone": adopted, "sdn_auto_manage_zone": False},
                })
            elif zone_cfg:
                # Configured zone is absent — create and tear it down ourselves.
                passes.append({
                    "mode": "sdn",
                    "env": {"SDN_ZONE": zone_cfg},
                    "cpi": {"sdn_auto_manage_zone": True},
                })
            # else: no zone configured and none exist -> cannot run sdn; skip.

    return passes


def detect_network_modes(cfg: dict) -> "list[dict]":
    """Autodetect runnable network-test passes against the live PVE host.

    Probes the PVE API for SDN availability + existing zones and whether the
    configured bridge iface already exists, then delegates the decision to
    select_network_modes.

    Args:
        cfg: Validated config dict from load_config.

    Returns:
        List of pass descriptors (see select_network_modes).

    Raises:
        RuntimeError: Network error, auth failure, or malformed response from a
                      probe that is expected to succeed (SDN-absent is handled
                      gracefully via allow_missing, not an error).
    """
    tier1 = cfg["tier1"]
    nt = tier1.get("network_test", {}) or {}
    sdn_cfg = nt.get("sdn", {}) or {}
    bridge_cfg = nt.get("bridge", {}) or {}
    bridge_iface = str(bridge_cfg.get("iface", "")).strip()

    cpi_cfg = build_cpi_config(cfg, dry_run=False)
    node = str(cpi_cfg.get("node", "")).strip()

    # SDN capability: a present /cluster/sdn/zones endpoint means SDN is
    # installed; allow_missing maps 404/501 (not installed) to None.
    zones_data = pve_api_get(cpi_cfg, "/cluster/sdn/zones", allow_missing=True)
    sdn_installed = zones_data is not None
    existing_zones: "list[str]" = []
    if isinstance(zones_data, list):
        existing_zones = [
            str(e["zone"]) for e in zones_data if isinstance(e, dict) and e.get("zone")
        ]

    # Bridge capability: is the configured iface already present on the node?
    # Without a node we cannot create a bridge anyway — treat as blocked so the
    # selector skips bridge rather than attempting a doomed create.
    bridge_exists = False
    if bridge_iface:
        if not node:
            bridge_exists = True
        else:
            net_data = pve_api_get(
                cpi_cfg, f"/nodes/{urllib.parse.quote(node)}/network", allow_missing=True
            )
            if isinstance(net_data, list):
                bridge_exists = any(
                    isinstance(e, dict) and e.get("iface") == bridge_iface for e in net_data
                )

    return select_network_modes(
        sdn_installed=sdn_installed,
        existing_zones=existing_zones,
        sdn_cfg=sdn_cfg,
        bridge_iface=bridge_iface,
        bridge_exists=bridge_exists,
    )


def stemcell_path(cfg: dict) -> str:
    """Resolve the stemcell path used by Tier 1 (lifecycle) and Tier 2 (upload).

    Resolution order:
    1. cfg['tier1']['stemcell_path'] if non-empty.
    2. STEMCELL_PATH environment variable.

    Raises:
        SystemExit: neither source provides a path.
    """
    tier1 = cfg.get("tier1", {})
    stemcell = str(tier1.get("stemcell_path", "")).strip()
    if not stemcell:
        stemcell = os.environ.get("STEMCELL_PATH", "").strip()
    if not stemcell:
        sys.exit(
            "stemcell_path not set in tier1 config and STEMCELL_PATH env var not set"
        )
    return stemcell


def tier1_env(cfg: dict, cpi_config_path: "str | Path", dry_run: bool = False) -> dict:
    """Build the env dict required by scripts/lifecycle (Tier 1).

    STEMCELL_PATH resolution order:
    1. cfg['tier1']['stemcell_path'] if non-empty.
    2. STEMCELL_PATH environment variable.

    All values are coerced to str.

    Args:
        cfg:             Validated config dict from load_config.
        cpi_config_path: Path to the CPI JSON config written by write_cpi_config.
        dry_run:         Accepted for interface consistency; has no effect here
                         (all values come from config, no shell calls).

    Returns:
        Dict of env var names to string values.

    Raises:
        SystemExit: STEMCELL_PATH absent from both config and env.
    """
    tier1 = cfg["tier1"]

    stemcell = stemcell_path(cfg)

    dns_raw = tier1.get("network_dns", ["8.8.8.8"])
    if not isinstance(dns_raw, list):
        dns_raw = [str(dns_raw)]

    env_out = {
        "CPI_CONFIG": str(cpi_config_path),
        "STEMCELL_PATH": stemcell,
        "NETWORK_IP": str(tier1["network_ip"]),
        "NETWORK_RANGE": str(tier1["network_range"]),
        "NETWORK_GATEWAY": str(tier1["network_gateway"]),
        "NETWORK_BRIDGE": str(tier1["network_bridge"]),
        "NETWORK_DNS": json.dumps(dns_raw),
        "VM_CORES": str(tier1["vm_cores"]),
        "VM_MEMORY_MIB": str(tier1["vm_memory_mib"]),
        "DISK_SIZE_MIB": str(tier1["disk_size_mib"]),
    }

    # Optional network-test parameters consumed by create_network/delete_network
    # in scripts/lifecycle. NETWORK_TEST_MODE itself is injected per-pass by
    # tier_lifecycle (scripts/test); here we only surface the SDN/bridge knobs so
    # they are present whenever a mode is selected. Absent block -> nothing added
    # (lifecycle defaults to mode=off).
    network_test = tier1.get("network_test", {}) or {}
    if network_test:
        sdn = network_test.get("sdn", {}) or {}
        bridge = network_test.get("bridge", {}) or {}
        env_out.update(
            {
                "SDN_ZONE": str(sdn.get("zone", "")),
                "SDN_ZONE_TYPE": str(sdn.get("zone_type", "simple")),
                "SDN_VNET": str(sdn.get("vnet", "")),
                "SDN_RANGE": str(sdn.get("range", "")),
                "SDN_GATEWAY": str(sdn.get("gateway", "")),
                "SDN_IP": str(sdn.get("ip", "")),
                "BRIDGE_TEST_IFACE": str(bridge.get("iface", "")),
            }
        )

    return env_out


def director_env(cfg: dict, dry_run: bool = False) -> dict:
    """Build the BOSH director env dict for Tier 2 / credhub operations.

    Args:
        cfg:     Validated config dict from load_config.
        dry_run: Passed to bosh_int; prevents shell-out in dry_run mode.

    Returns:
        Dict with BOSH_ENVIRONMENT, BOSH_CLIENT, BOSH_CLIENT_SECRET, BOSH_CA_CERT.

    Raises:
        SystemExit: bosh int fails for admin_password or director_ssl/ca.
    """
    bosh_creds = cfg["bosh_creds"]
    tier2 = cfg["tier2"]

    return {
        "BOSH_ENVIRONMENT": str(tier2["bosh_env_alias"]),
        "BOSH_CLIENT": "admin",
        "BOSH_CLIENT_SECRET": bosh_int(bosh_creds, "/admin_password", dry_run=dry_run),
        "BOSH_CA_CERT": bosh_int(bosh_creds, "/director_ssl/ca", dry_run=dry_run),
    }


def system_domain(cfg: dict, dry_run: bool = False) -> str:
    """Return the CF system domain from cf_vars.

    Args:
        cfg:     Validated config dict from load_config.
        dry_run: Passed to bosh_int.

    Returns:
        System domain string, e.g. ``"sys.cf.example.com"``.

    Raises:
        SystemExit: bosh int fails.
    """
    return bosh_int(cfg["cf_vars"], "/system_domain", dry_run=dry_run)


# ---------------------------------------------------------------------------
# CLI — standalone testability
# ---------------------------------------------------------------------------


def _print_summary(cfg: dict, dry_run: bool) -> None:
    """Print a human-readable config summary without exposing secret values."""
    tier1 = cfg["tier1"]
    tier2 = cfg["tier2"]
    tier3 = cfg["tier3"]
    tiers = cfg["tiers"]

    print("=== Integration Config Summary ===")
    print(f"  bosh_vars:   {cfg['bosh_vars']}")
    print(f"  bosh_creds:  {cfg['bosh_creds']}")
    print(f"  cf_vars:     {cfg['cf_vars']}")
    print()
    print("Tiers enabled:")
    print(f"  lifecycle: {tiers.get('lifecycle', False)}")
    print(f"  bosh:      {tiers.get('bosh', False)}")
    print(f"  cf:        {tiers.get('cf', False)}")
    print()
    print("Tier 1 (lifecycle):")
    print(f"  vmid_range_start: {tier1['vmid_range_start']}")
    print(f"  vmid_range_end:   {tier1['vmid_range_end']}")
    print(f"  network_ip:       {tier1['network_ip']}")
    print(f"  network_range:    {tier1['network_range']}")
    print(f"  network_gateway:  {tier1['network_gateway']}")
    print(f"  network_bridge:   {tier1['network_bridge']}")
    print(f"  network_dns:      {tier1.get('network_dns', ['8.8.8.8'])}")
    print(f"  vm_cores:         {tier1['vm_cores']}")
    print(f"  vm_memory_mib:    {tier1['vm_memory_mib']}")
    print(f"  disk_size_mib:    {tier1['disk_size_mib']}")
    stemcell = str(tier1.get("stemcell_path", "")).strip() or os.environ.get("STEMCELL_PATH", "(not set)")
    print(f"  stemcell_path:    {stemcell}")
    nt = tier1.get("network_test", {}) or {}
    modes = nt.get("modes", []) or []
    if modes:
        sdn = nt.get("sdn", {}) or {}
        bridge = nt.get("bridge", {}) or {}
        print(f"  network_test:     modes={modes}")
        if "sdn" in modes:
            print(f"    sdn:            zone={sdn.get('zone')} vnet={sdn.get('vnet')} range={sdn.get('range')}")
        if "bridge" in modes:
            print(f"    bridge:         iface={bridge.get('iface')}")
    else:
        print("  network_test:     (disabled)")
    print()
    print("Tier 2 (bosh):")
    print(f"  bosh_env_alias:   {tier2.get('bosh_env_alias')}")
    print(f"  director_name:    {tier2.get('director_name')}")
    print(f"  deployment_name:  {tier2.get('deployment_name')}")
    print(f"  smoke_timeout_s:  {tier2.get('smoke_timeout_s')}")
    print()
    print("Tier 3 (cf):")
    print(f"  deployment_name:  {tier3.get('deployment_name')}")
    print(f"  smoke_org:        {tier3.get('smoke_org')}")
    print(f"  smoke_space:      {tier3.get('smoke_space')}")
    print(f"  smoke_app:        {tier3.get('smoke_app')}")
    print(f"  smoke_timeout_s:  {tier3.get('smoke_timeout_s')}")
    print()

    bosh_vars_path = Path(cfg["bosh_vars"])
    bosh_creds_path = Path(cfg["bosh_creds"])
    cf_vars_path = Path(cfg["cf_vars"])

    if dry_run:
        print("dry_run=true — bosh int calls skipped; CPI config not written")
        print("  pve_host:  <dry-run:REDACTED>")
        print("  pve_user:  <dry-run:REDACTED>")
        print(f"  BOSH_ENVIRONMENT: {tier2.get('bosh_env_alias')}")
        print("  BOSH_CLIENT: admin")
        print("  BOSH_CLIENT_SECRET: <dry-run:REDACTED>")
        print("  BOSH_CA_CERT: <dry-run:REDACTED>")
        print("  system_domain: <dry-run:REDACTED>")
    else:
        print("Resolving secrets via bosh int...")
        if bosh_vars_path.exists():
            host = bosh_int(bosh_vars_path, "/pve_host")
            user = bosh_int(bosh_vars_path, "/pve_user")
            print(f"  pve_host: {host}")
            print(f"  pve_user: {user}")
        else:
            print(f"  bosh_vars not found at {bosh_vars_path} — skipping live secret resolution")

        if bosh_creds_path.exists():
            bosh_env_val = director_env(cfg)
            print(f"  BOSH_ENVIRONMENT: {bosh_env_val['BOSH_ENVIRONMENT']}")
            print("  BOSH_CLIENT: admin")
            print("  BOSH_CLIENT_SECRET: (resolved)")
            print("  BOSH_CA_CERT: (resolved)")
        else:
            print(f"  bosh_creds not found at {bosh_creds_path} — skipping director env resolution")

        if cf_vars_path.exists():
            sd = system_domain(cfg)
            print(f"  system_domain: {sd}")
        else:
            print(f"  cf_vars not found at {cf_vars_path} — skipping system_domain resolution")


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="_integration.py",
        description="Integration config loader — standalone smoke test.",
    )
    parser.add_argument(
        "--config",
        default="ci/integration.yml",
        metavar="FILE",
        help="Path to integration config YAML (default: ci/integration.yml)",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Print parsed summary without calling bosh int or writing files.",
    )
    args = parser.parse_args(argv)

    cfg = load_config(args.config, dry_run=args.dry_run)
    _print_summary(cfg, dry_run=args.dry_run)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
