"""Exercise recovery against isolated journals and disposable lab allocations.

Each scenario creates a new namespace. Fault injection never edits the operator's
journal, and unsuccessful scenarios retain their private files for diagnosis.
"""
from __future__ import annotations

import concurrent.futures
import copy
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import tempfile
import uuid
from pathlib import Path
from typing import Any
from urllib.parse import quote

from _pve_verify import PVEVerifier

RECOVERY_CASES = (
    "journal_corrupt", "journal_missing", "journal_stale", "unreturned_cid",
    "context_isolation", "mixed_legacy_managed",
)
_FIELDS = {"workspace", "cases", "disk_size_mb", "legacy_storage", "contexts"}
_CONTEXT_FIELDS = (
    "host", "port", "user", "password", "api_token", "realm", "node",
    "vm_storage", "disk_storage", "stemcell_storage", "iso_storage",
    "network_bridge", "verify_ssl", "vmid_range_start", "vmid_range_end",
    "disk_vmid_range_start", "disk_vmid_range_end", "parked_disk_vmid_range_start",
    "parked_disk_vmid_range_end", "detached_disk_strategy", "vm_disk_format",
    "agent_mode", "placement", "storage_sets", "storage_capacity_domains",
    "ephemeral_storage_set", "persistent_storage_set", "root_storage_set",
    "storage_placement_namespace", "require_disjoint_storage_sets",
    "storage_status_max_age_seconds",
)


def validate_recovery_manifest(value: Any) -> None:
    """Validate the fixture without opening a journal or contacting PVE."""
    if not isinstance(value, dict) or set(value) - _FIELDS:
        raise ValueError("recovery fixture has unknown fields")
    workspace = value.get("workspace")
    if not isinstance(workspace, str) or not Path(workspace).is_absolute():
        raise ValueError("recovery workspace must be an absolute private directory")
    cases = value.get("cases")
    if (not isinstance(cases, list) or not cases or
            any(not isinstance(case, str) or case not in RECOVERY_CASES for case in cases)
            or len(set(cases)) != len(cases)):
        raise ValueError("recovery cases must be unique supported scenario names")
    size = value.get("disk_size_mb")
    if not isinstance(size, int) or isinstance(size, bool) or not 1 <= size <= 102400:
        raise ValueError("recovery disk_size_mb must be between 1 and 102400")
    if "mixed_legacy_managed" in cases:
        storage = value.get("legacy_storage")
        if not isinstance(storage, str) or not storage or any(c in storage for c in "/:, "):
            raise ValueError("mixed lifecycle requires a concrete legacy storage ID")
    if "context_isolation" in cases:
        contexts = value.get("contexts")
        if (not isinstance(contexts, list) or len(contexts) != 2 or
                any(not isinstance(path, str) or not Path(path).is_absolute() for path in contexts)
                or contexts[0] == contexts[1]):
            raise ValueError("context isolation requires two distinct absolute CPI config paths")


def _private_workspace(path: str, source: str) -> Path:
    workspace = Path(path)
    if workspace.resolve() != workspace or not workspace.is_dir():
        raise ValueError("recovery workspace must exist without symlink components")
    info = workspace.stat()
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise ValueError("recovery workspace must belong to the current user with mode 0700")
    if source:
        original = Path(source).resolve()
        if workspace == original or workspace.is_relative_to(original) or original.is_relative_to(workspace):
            raise ValueError("recovery workspace must be separate from the operator journal")
    return workspace


def _write_private(path: Path, value: Any) -> None:
    with path.open("x", encoding="utf-8") as stream:
        os.chmod(path, 0o600)
        json.dump(value, stream)
        stream.flush()
        os.fsync(stream.fileno())


def _flag(value: Any, label: str) -> bool:
    if type(value) is bool:
        return value
    if type(value) is int and value in (0, 1):
        return value == 1
    if isinstance(value, str) and value in ("0", "1"):
        return value == "1"
    raise RuntimeError(f"recovery {label} flag is malformed")


def _members(value: Any, label: str) -> set[str]:
    if isinstance(value, str):
        return {item for item in value.replace(",", " ").split() if item}
    if isinstance(value, list) and all(isinstance(item, str) and item for item in value):
        return set(value)
    raise RuntimeError(f"recovery {label} is malformed")


def _inventory(verifier: PVEVerifier) -> dict[str, Any]:
    definitions = verifier._get("/storage")
    nodes = verifier._get("/nodes")
    if not isinstance(definitions, list) or not isinstance(nodes, list) or not nodes:
        raise RuntimeError("recovery inventory is incomplete")
    stores = {}
    for row in definitions:
        if not isinstance(row, dict) or not isinstance(row.get("storage"), str):
            raise RuntimeError("recovery storage definitions are malformed")
        name = row["storage"]
        if not name or name in stores:
            raise RuntimeError("recovery storage definitions are ambiguous")
        stores[name] = row
    volumes, locations = [], []
    for node in nodes:
        name = node.get("node") if isinstance(node, dict) else None
        if not isinstance(name, str) or not name:
            raise RuntimeError("recovery node identity is missing")
        statuses = verifier._get(f"/nodes/{quote(name, safe='')}/storage")
        if not isinstance(statuses, list):
            raise RuntimeError("recovery node storage status is unreadable")
        by_id = {}
        for status in statuses:
            storage = status.get("storage") if isinstance(status, dict) else None
            if not isinstance(storage, str) or storage in by_id:
                raise RuntimeError("recovery node storage status is ambiguous")
            by_id[storage] = status
        for storage, definition in stores.items():
            if _flag(definition.get("disable", 0), "disabled"):
                continue
            allowed = _members(definition.get("nodes", ""), "storage nodes")
            if allowed and name not in allowed:
                continue
            content = _members(definition.get("content", ""), "storage content")
            if not content & {"images", "iso"}:
                continue
            status = by_id.get(storage)
            if status is None:
                raise RuntimeError("applicable storage status is missing")
            if not _flag(status.get("active"), "active"):
                continue
            if not _flag(status.get("enabled", 1), "enabled"):
                continue
            entries = verifier._get(f"/nodes/{quote(name, safe='')}/storage/{quote(storage, safe='')}/content")
            if not isinstance(entries, list):
                raise RuntimeError("recovery storage content is unreadable")
            locations.append((name, storage))
            for entry in entries:
                if (not isinstance(entry, dict) or not isinstance(entry.get("volid"), str)
                        or type(entry.get("size")) is not int or entry["size"] < 0):
                    raise RuntimeError("recovery volume identity is malformed")
                volumes.append((name, storage, entry["volid"], entry.get("size")))
    guests = verifier._get("/cluster/resources?type=vm")
    if not isinstance(guests, list):
        raise RuntimeError("recovery guest inventory is unreadable")
    identities = []
    for row in guests:
        if not isinstance(row, dict) or not isinstance(row.get("vmid"), int) or not isinstance(row.get("node"), str):
            raise RuntimeError("recovery guest identity is malformed")
        config_hash = ""
        if row.get("type") == "qemu":
            config = verifier._get(f"/nodes/{quote(row['node'], safe='')}/qemu/{row['vmid']}/config")
            if not isinstance(config, dict):
                raise RuntimeError("recovery guest configuration is unreadable")
            config_hash = hashlib.sha256(json.dumps(config, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        identities.append((row["node"], row["vmid"], row.get("type"), config_hash))
    return {"volumes": sorted(volumes), "guests": sorted(identities), "locations": sorted(locations)}


class RecoveryScenario:
    """Own one fresh namespace and retain its evidence through cleanup."""

    def __init__(self, runner: Any, fixture: dict[str, Any], case: str,
                 base: dict[str, Any] | None = None, directory: Path | None = None) -> None:
        self.runner = runner
        self.fixture = fixture
        self.case = case
        config = copy.deepcopy(base if base is not None else runner.configured_policy())
        workspace = _private_workspace(fixture["workspace"], config.get("storage_allocation_journal_dir", ""))
        self.directory = directory or Path(tempfile.mkdtemp(prefix="recovery-", dir=workspace))
        self.directory.chmod(0o700)
        self.journal = self.directory / "journal"
        self.journal.mkdir(mode=0o700, exist_ok=True)
        self.namespace = "cert-recovery-" + uuid.uuid4().hex
        config["storage_placement_namespace"] = self.namespace
        config["storage_allocation_journal_dir"] = str(self.journal)
        self.config = config
        self.config_path = self.directory / (self.namespace + ".json")
        _write_private(self.config_path, config)
        self.verifier = PVEVerifier(config)
        self.namespace_path = self.journal / hashlib.sha256(self.namespace.encode()).hexdigest()
        self.evidence: dict[str, Any] = {"namespace": self.namespace, "scenario_directory": str(self.directory)}
        self.phase = "initialization"

    def journal_call(self, action: str, *flags: str, expected_success: bool = True) -> dict[str, Any]:
        result = subprocess.run([self.runner.cpi_bin, "storage-journal", action,
                                 "--config", str(self.config_path), *flags],
                                capture_output=True, text=True, timeout=600, check=False)
        try:
            data = json.loads(result.stdout) if result.stdout.strip() else {}
        except json.JSONDecodeError as error:
            raise RuntimeError("journal command returned malformed evidence") from error
        if expected_success and result.returncode != 0:
            raise RuntimeError("journal command did not establish the required proof")
        return {"exit_code": result.returncode, "output": data}

    def initialize(self) -> None:
        # The random namespace has no prior writer. Initialization independently
        # requires complete remote absence before accepting these attestations.
        result = self.journal_call("initialize", "--authority-id", self.namespace,
                                   "--previous-writer-fenced", "--remote-tasks-settled")
        cluster_id = result["output"].get("cluster_id")
        if not isinstance(cluster_id, str) or not cluster_id:
            raise RuntimeError("enrollment omitted observed cluster identity")
        self.evidence["cluster_id"] = cluster_id

    def call(self, method: str, args: list[Any], config_path: Path | None = None,
             context: dict[str, Any] | None = None, discard: bool = False) -> Any:
        request = {"method": method, "arguments": args, "api_version": 2,
                   "context": context or {"request_id": uuid.uuid4().hex}}
        result = subprocess.run([self.runner.cpi_bin, "--config", str(config_path or self.config_path)],
                                input=json.dumps(request) + "\n", text=True,
                                stdout=subprocess.DEVNULL if discard else subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=600, check=False)
        if result.returncode != 0:
            raise RuntimeError("CPI scenario process failed")
        if discard:
            return None
        try:
            response = json.loads(result.stdout)
        except (TypeError, json.JSONDecodeError) as error:
            raise RuntimeError("CPI scenario response is malformed") from error
        if not isinstance(response, dict):
            raise RuntimeError("CPI scenario response is malformed")
        return response

    def success(self, method: str, args: list[Any], **kwargs: Any) -> Any:
        response = self.call(method, args, **kwargs)
        if response.get("error") is not None:
            refusal = {"kind": "cpi_operation_refused", "method": method if method in {"create_disk", "delete_disk", "resize_disk", "has_disk"} else "operation"}
            error = response["error"]
            message = error.get("message") if isinstance(error, dict) else None
            match = re.fullmatch(r'cpi: context override rejected: config: context override "pve_(root_storage_set|ephemeral_storage_set|persistent_storage_set)": \1 must not be blank', message) if isinstance(message, str) else None
            if match:
                refusal.update(kind="blank_storage_selector_override", field=match.group(1))
            self.evidence["cpi_refusal"] = refusal
            raise RuntimeError("CPI scenario operation was refused")
        return response.get("result")

    def create_disk(self, **kwargs: Any) -> str:
        cid = self.success("create_disk", [self.fixture["disk_size_mb"], {}, ""], **kwargs)
        if not isinstance(cid, str) or not cid:
            raise RuntimeError("disk creation did not return an exact CID")
        self.evidence.setdefault("returned_cids", []).append(cid)
        _write_private(self.directory / ("recovery-evidence-" + self.namespace + "-" + str(len(self.evidence["returned_cids"])) + ".json"), self.evidence)
        return cid

    def audit(self) -> dict[str, Any]:
        output = self.journal_call("audit")["output"]
        audit = output.get("audit", {})
        if not isinstance(audit, dict):
            self.evidence["audit_checkpoint"] = {"failure_detail": "malformed audit object"}
            raise RuntimeError("recovery audit is malformed")
        conflicts = audit.get("conflicts")
        issues = audit.get("issues")
        categories = []
        for conflict in conflicts[:16] if isinstance(conflicts, list) else []:
            match = re.fullmatch(r"remote allocation ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}) is outside recorded mutation targets", conflict) if isinstance(conflict, str) else None
            categories.append({"kind": "outside_recorded_mutation_targets", "allocation_id": match.group(1)} if match else {"kind": "unclassified_conflict"})
        self.evidence["audit_checkpoint"] = {
            "generation_index_healthy": output.get("generation_index_healthy") is True,
            "cluster_continuity": output.get("cluster_continuity") is True,
            "complete": audit.get("complete") is True,
            "vm_scan_complete": audit.get("vm_scan_complete") is True,
            "issue_count": len(issues) if isinstance(issues, list) else 0 if issues is None else -1,
            "conflict_count": len(conflicts) if isinstance(conflicts, list) else 0 if conflicts is None else -1,
            "conflict_categories": categories,
        }
        if any(items is not None and (not isinstance(items, list) or any(not isinstance(item, str) for item in items)) for items in (issues, conflicts)):
            raise RuntimeError("recovery audit issue or conflict inventory is malformed")
        if (output.get("generation_index_healthy") is not True or output.get("cluster_continuity") is not True
                or audit.get("complete") is not True or audit.get("vm_scan_complete") is not True
                or audit.get("issues") or audit.get("conflicts")):
            raise RuntimeError("recovery audit does not prove complete consistent authority")
        return output

    def record(self, cid: str | None = None) -> dict[str, Any]:
        records = self.audit().get("records")
        if not isinstance(records, list):
            raise RuntimeError("journal audit omitted allocation summaries")
        found = [row for row in records if row.get("Kind") == "disk" and
                 (cid is None or row.get("CID") == cid) and row.get("State") not in ("deleted", "cleaned")]
        if len(found) != 1 or not isinstance(found[0].get("CID"), str) or not found[0]["CID"]:
            raise RuntimeError("journal audit did not identify one exact returnable disk")
        return found[0]

    def verify_disk(self, cid: str, size_mb: int | None = None) -> str:
        volume = self.verifier.current_disk_volid(cid)
        entry = self.verifier.volume_entry(cid)
        if not volume or not isinstance(entry, dict) or entry.get("volid") != volume:
            raise RuntimeError("disk physical identity was not observed")
        expected = ((size_mb if size_mb is not None else self.fixture["disk_size_mb"]) + 1023) // 1024 * 1024**3
        if type(entry.get("size")) is not int or entry["size"] != expected:
            raise RuntimeError("disk physical size differs from the rounded request")
        self.evidence.setdefault("volumes", []).append({"cid": cid, "volid": volume, "size": entry.get("size")})
        return volume

    def delete_disk(self, cid: str, managed: bool = True, **kwargs: Any) -> None:
        self.success("delete_disk", [cid], **kwargs)
        if self.verifier.volume_exists(cid):
            raise RuntimeError("disk remains after completed deletion")
        if managed:
            audit = self.audit()
            rows = [row for row in audit.get("records") or [] if row.get("CID") == cid and row.get("Kind") == "disk"]
            if (len(rows) != 1 or rows[0].get("State") not in ("deleted", "cleaned")
                    or any(row.get("allocation_id") == rows[0].get("ID") for row in audit["audit"].get("evidence") or [])):
                raise RuntimeError("deleted managed disk lacks terminal absent journal disposition")

    def run_journal_fault(self) -> None:
        self.phase = "namespace initialization"
        self.initialize()
        backup = self.directory / "before-allocation"
        if self.case == "journal_stale":
            shutil.copytree(self.namespace_path, backup)
        self.phase = "initial disk allocation"
        cid = self.create_disk()
        self.phase = "initial disk verification"
        self.verify_disk(cid)
        self.phase = "initial allocation audit"
        record = self.record(cid)
        if self.case != "journal_stale":
            shutil.copytree(self.namespace_path, backup)
        before = _inventory(self.verifier)
        self.phase = "fault injection"
        if self.case == "journal_corrupt":
            target = self.namespace_path / ("allocation-" + record["ID"] + ".json")
            target.write_text("{invalid journal", encoding="utf-8")
        else:
            shutil.move(self.namespace_path, self.directory / "retained-authority")
            if self.case == "journal_stale":
                shutil.copytree(backup, self.namespace_path)
        self.phase = "refusal verification"
        response = self.call("create_disk", [self.fixture["disk_size_mb"], {}, ""])
        if response.get("error") is None:
            raise RuntimeError("damaged authority admitted another allocation")
        after = _inventory(self.verifier)
        if before != after:
            raise RuntimeError("authority refusal changed remote inventory")
        self.evidence["refusal"] = {"error_present": True, "inventory_unchanged": True}
        # All calls above have exited, and no other process knows this random
        # namespace. Restore its retained authority only after proving no writes.
        if self.namespace_path.exists():
            shutil.move(self.namespace_path, self.directory / "faulted-authority")
        original = self.directory / "retained-authority" if self.case == "journal_stale" else backup
        shutil.copytree(original, self.namespace_path)
        self.phase = "restored authority cleanup"
        self.delete_disk(cid)
        self.evidence["final_audit"] = self.audit()

    def run_unreturned(self) -> None:
        self.initialize()
        self.phase = "discard CID delivery"
        self.call("create_disk", [self.fixture["disk_size_mb"], {}, ""], discard=True)
        record = self.record()
        cid = record["CID"]
        self.verify_disk(cid)
        self.evidence["recovered_allocation_id"] = record["ID"]
        self.journal_call("adopt", "--allocation-id", record["ID"], "--expected-cid", cid,
                          "--decision-id", "certification-discarded-delivery")
        self.phase = "adopted disk cleanup"
        self.delete_disk(cid)
        self.evidence["final_audit"] = self.audit()

    def run_mixed(self) -> None:
        self.initialize()
        legacy = copy.deepcopy(self.config)
        for field in ("storage_sets", "storage_capacity_domains", "ephemeral_storage_set",
                      "persistent_storage_set", "root_storage_set"):
            legacy.pop(field, None)
        legacy["disk_storage"] = self.fixture["legacy_storage"]
        path = self.directory / "legacy.json"
        _write_private(path, legacy)
        self.phase = "mixed allocation"
        old_cid = self.create_disk(config_path=path)
        new_cid = self.create_disk()
        if old_cid == new_cid:
            raise RuntimeError("legacy and managed allocations share a CID")
        self.verify_disk(old_cid)
        self.verify_disk(new_cid)
        self.phase = "lifecycle after set removal"
        for cid in (old_cid, new_cid):
            if self.success("has_disk", [cid], config_path=path) is not True:
                raise RuntimeError("existing disk disappeared after set removal")
            self.success("resize_disk", [cid, self.fixture["disk_size_mb"] + 1024], config_path=path)
            self.verify_disk(cid, self.fixture["disk_size_mb"] + 1024)
            self.delete_disk(cid, managed=cid == new_cid, config_path=path)
        self.evidence["final_audit"] = self.audit()


def _run_contexts(runner: Any, fixture: dict[str, Any], evidence: dict[str, Any]) -> dict[str, Any]:
    evidence["phase"] = "context fixture validation"
    bases = [json.loads(Path(path).read_text()) for path in fixture["contexts"]]
    if any(not isinstance(base, dict) or not base.get("persistent_storage_set") for base in bases):
        raise ValueError("context fixtures must provide complete set-managed CPI configurations")
    process_fields = [{key: value for key, value in base.items()
                       if key not in _CONTEXT_FIELDS and key != "storage_allocation_journal_dir"} for base in bases]
    if process_fields[0] != process_fields[1]:
        raise ValueError("context fixtures must share process-scoped configuration, including the trusted CA bundle")
    cleared = {"password", "api_token"}
    if any((key in bases[0]) != (key in bases[1]) for key in _CONTEXT_FIELDS if key not in cleared):
        raise ValueError("context fixtures must explicitly provide the same override fields")
    shared = RecoveryScenario(runner, fixture, "context_isolation", bases[0])
    other = RecoveryScenario(runner, fixture, "context_isolation", bases[1], shared.directory)
    scenarios = [shared, other]
    evidence["contexts"] = [scenario.evidence for scenario in scenarios]
    evidence["phase"] = "context enrollment"
    for scenario in scenarios:
        scenario.initialize()
    if shared.evidence["cluster_id"] == other.evidence["cluster_id"]:
        raise RuntimeError("context fixtures do not reach two distinct PVE clusters")
    contexts = []
    for scenario in scenarios:
        override = {"pve_" + key: scenario.config[key] for key in _CONTEXT_FIELDS if key in scenario.config}
        for key in ("password", "api_token"):
            override.setdefault("pve_" + key, "")
        contexts.append(override)
    evidence["phase"] = "context allocation"
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        futures = [pool.submit(scenario.create_disk, config_path=shared.config_path, context=context)
                   for scenario, context in zip(scenarios, contexts)]
        cids = [future.result() for future in futures]
    evidence["phase"] = "context verification and cleanup"
    for scenario, cid, context in zip(scenarios, cids, contexts):
        scenario.verify_disk(cid)
        record = scenario.record(cid)
        other_scenario = other if scenario is shared else shared
        if any(row.get("ID") == record["ID"] for row in other_scenario.audit()["records"]):
            raise RuntimeError("allocation record leaked across context namespaces")
        scenario.delete_disk(cid, config_path=shared.config_path, context=context)
        scenario.evidence.update({"allocation_id": record["ID"], "final_audit": scenario.audit()})
    evidence.update({"distinct_clusters": True, "same_process_cache_isolation": False,
                     "distinct_namespaces": len({s.namespace for s in scenarios}) == 2})
    return evidence


def run_recovery_cases(runner: Any, fixture: dict[str, Any] | None) -> list[dict[str, Any]]:
    """Return actual outcomes; missing fixtures never count as passing cases."""
    if fixture is None:
        return [{"scenario_id": case, "status": "missing", "evidence": {"reason": "recovery fixture absent"}}
                for case in RECOVERY_CASES]
    validate_recovery_manifest(fixture)
    rows = []
    selected = set(fixture["cases"])
    for case in RECOVERY_CASES:
        if case not in selected:
            rows.append({"scenario_id": case, "status": "missing", "evidence": {"reason": "fixture not selected"}})
            continue
        scenario = None
        evidence: dict[str, Any] = {}
        try:
            if case == "context_isolation":
                evidence = _run_contexts(runner, fixture, evidence)
            else:
                scenario = RecoveryScenario(runner, fixture, case)
                if case.startswith("journal_"):
                    scenario.run_journal_fault()
                elif case == "unreturned_cid":
                    scenario.run_unreturned()
                else:
                    scenario.run_mixed()
                evidence = scenario.evidence
            rows.append({"scenario_id": case, "status": "passed", "evidence": evidence})
        except (RuntimeError, ValueError, OSError, subprocess.SubprocessError) as error:
            evidence = dict(scenario.evidence) if scenario else evidence
            evidence.update({"phase": scenario.phase if scenario else evidence.get("phase", "context setup or execution"),
                             "failure": type(error).__name__, "resources_may_remain": True})
            rows.append({"scenario_id": case, "status": "failed", "evidence": evidence})
    return rows
