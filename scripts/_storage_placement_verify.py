"""Live lifecycle assertions using production placement diagnostics and PVE state."""
from __future__ import annotations

import hashlib
import json
import os
import re
import subprocess
import tempfile
from pathlib import Path
from typing import Any
from urllib.parse import quote


class PlacementVerification:
    """Record independent placement evidence without predicting random ranks."""

    def __init__(self, config_path: str, cpi_bin: str, report_path: str) -> None:
        self.config_path = config_path
        self.cpi_bin = cpi_bin
        self.cli = str(Path(cpi_bin).resolve().with_name("pve-cid"))
        self.report_path = Path(report_path)
        self.config = json.loads(Path(config_path).read_text())
        self.report: dict[str, Any] = {
            "schema_version": 1,
            "binary_sha256": hashlib.sha256(Path(cpi_bin).read_bytes()).hexdigest(),
            "checks": [],
            "complete_release_matrix": False,
        }
        self.diagnostic: dict[str, Any] = {}
        self.members: dict[str, set[str]] = {}
        self.inventory_pairs: set[tuple[str, str]] = set()

    def _json_command(self, command: list[str], allowed_returncodes: tuple[int, ...] = (0,)) -> Any:
        result = subprocess.run(command, capture_output=True, text=True, timeout=120, check=False)
        if result.returncode not in allowed_returncodes:
            raise RuntimeError("storage verification command failed; inspect CPI diagnostics")
        return json.loads(result.stdout)

    def preflight(self, vm_arguments: list[Any]) -> None:
        properties = vm_arguments[2]
        if self.config.get("root_storage_set") or any(
            properties.get(key) for key in ("root_storage_set", "storage_pool", "ephemeral_storage_pool")
        ):
            raise RuntimeError("base certification requires the default root/ephemeral bundle")
        with tempfile.TemporaryDirectory(prefix="cpi-storage-check-") as directory:
            request = Path(directory) / "request.json"
            request.write_text(json.dumps({"method": "create_vm", "arguments": vm_arguments}))
            request.chmod(0o600)
            diagnostic = self._json_command([
                self.cli, "storage-plan", "--config", self.config_path,
                "--request", str(request), "--json",
            ])
        self.validate_topology(diagnostic)
        self.diagnostic = diagnostic
        self.report["planning"] = diagnostic
        self._passed("minimum_topology_and_feasible_plan")

    def validate_topology(self, diagnostic: dict[str, Any], *, ephemeral_minimum: int = 3,
                          persistent_minimum: int = 2) -> None:
        if diagnostic.get("observation_only") is not True or not diagnostic.get("targets"):
            raise RuntimeError("storage preflight did not produce a feasible observation-only plan")
        frozen = diagnostic.get("frozen_membership", {})
        role_members: dict[str, set[str]] = {}
        inventory_pairs: set[tuple[str, str]] = set()
        for role, key, minimum in (
            ("ephemeral", "ephemeral_storage_set", ephemeral_minimum),
            ("persistent", "persistent_storage_set", persistent_minimum),
        ):
            name = self.config.get(key)
            members = frozen.get(name, [])
            if not name or len(set(members)) < minimum:
                raise RuntimeError(f"certification requires at least {minimum} {role} members")
            role_members[role] = set(members)
        if role_members["ephemeral"] & role_members["persistent"]:
            raise RuntimeError("base certification requires separate persistent and ephemeral sets")
        all_members = role_members["ephemeral"] | role_members["persistent"]
        by_storage: dict[str, str] = {}
        nodes: dict[str, set[str]] = {}
        for row in diagnostic.get("capacities", []):
            pair = row.get("Pair", {})
            storage = pair.get("StorageID")
            if storage not in all_members or pair.get("Reason"):
                continue
            backing = pair.get("BackingKey")
            if not isinstance(backing, str) or not backing.startswith("nfs://"):
                raise RuntimeError("base certification requires observed NFS backing identities")
            if storage in by_storage and by_storage[storage] != backing:
                raise RuntimeError("certification backing differs between nodes")
            by_storage[storage] = backing
            node = pair.get("Node")
            if not isinstance(node, str) or not node:
                raise RuntimeError("certification capacity has no node identity")
            nodes.setdefault(node, set()).add(storage)
            inventory_pairs.add((node, storage))
        if set(by_storage) != all_members or len(set(by_storage.values())) != len(all_members):
            raise RuntimeError("base certification members must have independent observed backings")
        if sum(members == all_members for members in nodes.values()) < 2:
            raise RuntimeError("certification requires two nodes reaching every P/E member")
        self.members = role_members
        self.inventory_pairs = inventory_pairs

    def verify_vm(self, vm_config: dict[str, Any], dedicated: bool, volume_entry: Any) -> None:
        root_slot = "scsi0" if str(self.config.get("root_disk_bus", "")).strip().lower() == "scsi" else "virtio0"
        root = str(vm_config.get(root_slot, "")).split(",", 1)[0]
        if root.split(":", 1)[0] not in self.members["ephemeral"]:
            raise RuntimeError("VM root escaped the ephemeral storage set")
        disks = {
            slot: str(value).split(",", 1)[0]
            for slot, value in vm_config.items()
            if re.fullmatch(r"(?:scsi|virtio|sata|ide|unused)\d+", slot)
            and ":" in str(value) and "media=cdrom" not in str(value)
        }
        expected_slots = {root_slot, "scsi1"} if dedicated else {root_slot}
        if set(disks) != expected_slots or len(set(disks.values())) != len(disks):
            raise RuntimeError("VM configuration has missing, duplicate, or unexpected disks")
        for volume in disks.values():
            self._verified_volume(volume_entry(volume), volume)
        if dedicated:
            ephemeral = disks.get("scsi1", "")
            if not ephemeral or ephemeral.split(":", 1)[0] != root.split(":", 1)[0]:
                raise RuntimeError("dedicated ephemeral disk is not bundled with root")
        elif set(disks) != {root_slot}:
            raise RuntimeError("root-derived ephemeral configuration added an unexpected disk")
        self.report["vm_volumes"] = disks
        self._passed("dedicated_bundle" if dedicated else "root_derived_ephemeral")

    @staticmethod
    def _verified_volume(entry: Any, expected: str | None = None) -> str:
        if not isinstance(entry, dict):
            raise RuntimeError("actual storage volume is missing")
        volume = entry.get("volid")
        size = entry.get("size")
        if (not isinstance(volume, str) or not re.fullmatch(r"[^:,\s]+:[^,\s]+", volume)
                or not isinstance(size, int) or isinstance(size, bool) or size <= 0
                or expected is not None and volume != expected):
            raise RuntimeError("actual storage volume identity or size is invalid")
        return volume

    def verify_disk(self, entry: Any, actual_volume: str) -> None:
        volume = self._verified_volume(entry, actual_volume)
        if volume.split(":", 1)[0] not in self.members["persistent"]:
            raise RuntimeError("persistent disk escaped its storage set")
        self.report["persistent_volume"] = volume
        self._passed("persistent_set")

    def volume_inventory(self, verifier: Any) -> list[tuple[str, str, str, int]]:
        """Read every preflight node/store; a failed read cannot certify no leak."""
        result = []
        if not self.inventory_pairs:
            raise RuntimeError("volume inventory requires completed topology preflight")
        for node, storage in sorted(self.inventory_pairs):
            entries = verifier._get(
                f"/nodes/{quote(node, safe='')}/storage/{quote(storage, safe='')}/content"
            )
            if not isinstance(entries, list):
                raise RuntimeError("storage content inventory is malformed")
            seen = set()
            for entry in entries:
                volume = self._verified_volume(entry)
                if volume.split(":", 1)[0] != storage or volume in seen:
                    raise RuntimeError("storage content inventory has conflicting identities")
                seen.add(volume)
                result.append((node, storage, volume, entry["size"]))
        return sorted(result)

    def rejected_selector_cases(self, call_once: Any, vm_cid: str, size: int, inventory: Any) -> None:
        persistent = self.config["persistent_storage_set"]
        outside = sorted(self.members["ephemeral"])[0]
        cases = [
            ("unknown_set", {"storage_set": "certification-undefined-set"}, "unknown storage set"),
            ("competing_selectors", {"storage_set": persistent, "storage_pool": outside}, "competing"),
            ("outside_boundary", {"storage_pool": outside}, "boundary"),
        ]
        for name, properties, expected in cases:
            before = self._journal_records()
            volumes_before = inventory()
            try:
                unexpected = call_once("create_disk", [size, properties, vm_cid])
            except RuntimeError as error:
                if expected not in str(error).lower():
                    raise RuntimeError(f"{name} failed for an unexpected reason") from error
            else:
                if isinstance(unexpected, str) and unexpected:
                    self.report["unexpected_disk_cid"] = unexpected
                    self._write_report()
                    call_once("delete_disk", [unexpected])
                    if inventory() != volumes_before:
                        raise RuntimeError(f"{name} cleanup left unexpected storage artifacts")
                raise RuntimeError(f"{name} unexpectedly allocated a disk")
            if self._journal_records() != before:
                raise RuntimeError(f"{name} changed allocation evidence before selector rejection")
            if inventory() != volumes_before:
                raise RuntimeError(f"{name} changed actual storage volumes before selector rejection")
            self._passed(name)

    def _journal_records(self) -> Any:
        output = self._json_command([self.cpi_bin, "storage-journal", "audit", "--config", self.config_path])
        audit = output.get("audit", {})
        if not output.get("generation_index_healthy") or not output.get("cluster_continuity") or not audit.get("complete") or not audit.get("vm_scan_complete") or audit.get("conflicts") or audit.get("issues"):
            raise RuntimeError("fault assertion requires a complete consistent journal audit")
        if "records" not in output or "evidence" not in audit:
            raise RuntimeError("fault assertion audit omitted resource evidence")
        return {
            "records": sorted(output["records"] or [], key=lambda row: json.dumps(row, sort_keys=True)),
            "evidence": sorted(audit["evidence"] or [], key=lambda row: json.dumps(row, sort_keys=True)),
        }

    def _passed(self, name: str) -> None:
        self.report["checks"].append(name)
        self._write_report()

    def _write_report(self) -> None:
        self.report_path.parent.mkdir(parents=True, exist_ok=True)
        fd, temporary = tempfile.mkstemp(prefix=".storage-placement-", dir=self.report_path.parent)
        try:
            with os.fdopen(fd, "w") as report:
                report.write(json.dumps(self.report, indent=2) + "\n")
                report.flush()
                os.fsync(report.fileno())
            os.replace(temporary, self.report_path)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
