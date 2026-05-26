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
import subprocess
import sys
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


def write_cpi_config(cfg: dict, out_path: "str | Path", dry_run: bool = False) -> str:
    """Synthesize the CPI JSON config and write it to out_path.

    Pulls PVE secrets from cfg['bosh_vars'] via bosh int.  Auth preference:
    pve_api_token if non-empty, otherwise pve_password.  network_bridge uses
    tier1.network_bridge when non-empty, falling back to pve_network_bridge from
    vars.  verify_ssl is always false for test isolation.  vmid_range_start comes
    from tier1.

    Args:
        cfg:      Validated config dict from load_config.
        out_path: Destination path for the CPI JSON file.
        dry_run:  When True, skip the disk write and return str(out_path).

    Returns:
        str(out_path).

    Raises:
        SystemExit: bosh int fails for any required var, or out_path not writable.
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

    if dry_run:
        return str(out_path)

    dest = Path(out_path)
    try:
        dest.write_text(json.dumps(cpi_cfg, indent=2), encoding="utf-8")
    except OSError as exc:
        sys.exit(f"failed to write CPI config to {dest}: {exc}")

    return str(dest)


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

    stemcell = str(tier1.get("stemcell_path", "")).strip()
    if not stemcell:
        stemcell = os.environ.get("STEMCELL_PATH", "").strip()
    if not stemcell:
        sys.exit(
            "stemcell_path not set in tier1 config and STEMCELL_PATH env var not set"
        )

    dns_raw = tier1.get("network_dns", ["8.8.8.8"])
    if not isinstance(dns_raw, list):
        dns_raw = [str(dns_raw)]

    return {
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
