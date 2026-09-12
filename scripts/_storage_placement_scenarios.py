#!/usr/bin/env python3
"""Execute explicit placement policy certification against a disposable PVE lab.

Policy cases preserve the enrolled journal authority. Recovery cases create
separate disposable authorities. --validate-only is entirely local and cannot
produce successful live-case evidence. Failures retain concrete resource
identities for audited operator recovery; they are never counted as skips.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path
from typing import Any
from urllib.parse import quote

from _pve_verify import PVEVerifier, _decode_cid_payload
from _storage_placement_verify import PlacementVerification

if __name__ == "__main__":
    sys.modules.setdefault("_storage_placement_scenarios", sys.modules[__name__])

STRATEGIES = ("spread", "least_utilized", "weighted_free_space")
POLICY_OPTIONAL_KEYS = {"expected_root_mechanism"}
POLICY_KEYS = {
    "stemcell_cid", "vm_cloud_properties", "networks", "environment",
    "ephemeral_size_mib", "disk_size_mib", "ephemeral_storage_ids",
    "persistent_storage_ids", "root_storage_ids", "fixed_iso_storage_id",
    "encrypted_persistent_storage_ids", "escaping_tier_criteria",
}
AUTHORITY_KEYS = ("storage_placement_namespace", "storage_allocation_journal_dir")
POLICY_CONFIG_KEYS = (
    "storage_sets", "storage_capacity_domains", "root_storage_set",
    "ephemeral_storage_set", "persistent_storage_set", "iso_storage",
    "iso_storage_follow_vm_storage", "require_disjoint_storage_sets",
)
POLICY_IDS = [
    f"policy-{strategy}-p{p_width}-e{e_width}"
    for strategy in STRATEGIES
    for p_width in ("singleton", "plural") for e_width in ("singleton", "plural")
] + ["root-derived-follow-iso", "root-split-follow-iso", "root-split-fixed-iso",
     "selector-subset-encryption", "lifecycle-membership-removal", "strategy-change-resume"]


class ScenarioFailure(RuntimeError):
    """A failed assertion, with only a bounded nonsecret reason."""


class CPIRejected(ScenarioFailure):
    def __init__(self, message: str) -> None:
        super().__init__("CPI rejected the request")
        self.reason = message


def require(condition: Any, reason: str) -> None:
    if not condition:
        raise ScenarioFailure(reason)


def digest_json(value: Any) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def strict_object(value: Any, keys: set[str], label: str) -> dict[str, Any]:
    require(isinstance(value, dict), f"{label} must be an object")
    require(not set(value) - keys, f"{label} contains unsupported fields")
    return value


def storage_ids(value: Any, minimum: int, label: str) -> list[str]:
    require(isinstance(value, list) and len(value) >= minimum, f"{label} has too few declared existing members")
    require(all(isinstance(item, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", item) for item in value), f"{label} has invalid storage IDs")
    require(len(set(value)) == len(value), f"{label} repeats a storage ID")
    return list(value)


def policy_capabilities(cases: list[str] | None = None, suite: str = "policy") -> dict[str, int]:
    require(suite in {"policy", "faults", "recovery", "constraints", "director", "all"}, "unsupported suite")
    require(not cases or suite == "policy", "case selection requires the policy suite")
    if suite == "director":
        return {"ephemeral": 1, "persistent": 1, "root": 0, "encrypted": 0}
    if suite in {"constraints", "recovery", "faults"}:
        minimum = 2 if suite == "constraints" else 1
        return {"ephemeral": minimum, "persistent": minimum, "root": 0, "encrypted": 0}
    if cases is None:
        return {"ephemeral": 3, "persistent": 2, "root": 1, "encrypted": 1}
    require(isinstance(cases, list) and cases and all(isinstance(case, str) for case in cases)
            and len(set(cases)) == len(cases) and set(cases) <= set(POLICY_IDS),
            "selected policy cases are invalid")
    return {
        "ephemeral": 2 if any(case.endswith("-eplural") for case in cases) else 1,
        "persistent": 2 if any("-pplural-" in case for case in cases) else 1,
        "root": int(any(case.startswith("root-split-") for case in cases)),
        "encrypted": int("selector-subset-encryption" in cases),
    }


def validate_manifest(manifest: Any, config: dict[str, Any], cases: list[str] | None = None,
                      suite: str = "policy") -> dict[str, Any]:
    strict_object(manifest, {"version", "policy", "faults", "recovery", "constraints", "director"}, "manifest")
    require(type(manifest.get("version")) is int and manifest["version"] == 1, "unsupported manifest version")
    policy = strict_object(manifest.get("policy"), POLICY_KEYS | POLICY_OPTIONAL_KEYS, "policy fixtures")
    require(POLICY_KEYS <= set(policy), "policy fixtures must declare every required capability")
    for key in AUTHORITY_KEYS:
        require(isinstance(config.get(key), str) and config[key].strip(), "existing journal namespace and directory are required")
    require(Path(config["storage_allocation_journal_dir"]).is_absolute(), "journal path must be absolute")
    require(isinstance(policy["stemcell_cid"], str) and policy["stemcell_cid"], "existing stemcell CID is required")
    for key in ("vm_cloud_properties", "networks", "environment"):
        require(isinstance(policy[key], dict), f"{key} must be an object")
    for key in ("ephemeral_size_mib", "disk_size_mib"):
        require(type(policy[key]) is int and 0 < policy[key] <= (1 << 30), f"{key} must be a bounded positive integer")
    capabilities = policy_capabilities(cases, suite)
    ephemeral = storage_ids(policy["ephemeral_storage_ids"], capabilities["ephemeral"], "ephemeral fixture")
    persistent = storage_ids(policy["persistent_storage_ids"], capabilities["persistent"], "persistent fixture")
    root = storage_ids(policy["root_storage_ids"], capabilities["root"], "root fixture")
    require(not set(ephemeral) & set(persistent), "P/E fixture members must be disjoint")
    require(not set(root) & (set(ephemeral) | set(persistent)), "split-root fixture must use independent storage IDs")
    encrypted = storage_ids(policy["encrypted_persistent_storage_ids"], capabilities["encrypted"], "encrypted fixture")
    require(set(encrypted) <= set(persistent), "encrypted fixtures must be operator-asserted P members")
    storage_ids([policy["fixed_iso_storage_id"]], 1, "fixed ISO fixture")
    require(config.get("agent_mode") != "noagent", "live agent disk-mapping checks require an enabled agent")
    tier = strict_object(policy["escaping_tier_criteria"], {"types", "shared", "encrypted"}, "escaping tier")
    require((suite == "director" and tier == {}) or (isinstance(tier.get("types"), list) and tier["types"] and all(isinstance(item, str) and item for item in tier["types"])), "escaping tier requires explicit existing backend types")
    for key in ("shared", "encrypted"):
        require(key not in tier or type(tier[key]) is bool, "tier assertions must be booleans")
    require(policy.get("expected_root_mechanism", "import") in ({"import", "full_clone", "linked_clone", "auto"} if suite == "director" else {"import", "full_clone", "linked_clone"}), "unsupported expected root mechanism")
    return copy.deepcopy(policy)


class JSONNumber(str):
    """Preserve the Go JSON decoder's numeric spelling for audit fingerprints."""


def go_canonical_json(value: Any) -> str:
    if isinstance(value, JSONNumber):
        return str(value)
    if isinstance(value, dict):
        return "{" + ",".join(go_canonical_json(key) + ":" + go_canonical_json(value[key]) for key in sorted(value)) + "}"
    if isinstance(value, list):
        return "[" + ",".join(go_canonical_json(item) for item in value) + "]"
    encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    for character, escaped in (("&", "\\u0026"), ("<", "\\u003c"), (">", "\\u003e"), ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        encoded = encoded.replace(character, escaped)
    return encoded


def _unique_json_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "allocation JSON contains a duplicate field")
        result[key] = value
    return result


def _verified_record_envelope(raw: bytes):
    require(len(raw) <= 16 * 1024 * 1024, "allocation envelope exceeds certification bound")
    text = raw.decode("utf-8")
    decoder = json.JSONDecoder(object_pairs_hook=_unique_json_object,
                               parse_constant=lambda value: require(False, "allocation JSON contains a nonfinite number"))
    envelope = decoder.decode(text)
    require(isinstance(envelope, dict) and set(envelope) == {"version", "sha256", "payload"}
            and type(envelope["version"]) is int and envelope["version"] == 1,
            "allocation envelope is malformed")
    # Walk top-level fields so text inside a string cannot impersonate the payload key.
    offset = len(text) - len(text.lstrip()) + 1
    payload_text = None
    for _ in envelope:
        while text[offset].isspace() or text[offset] == ",":
            offset += 1
        key, offset = decoder.raw_decode(text, offset)
        while text[offset].isspace() or text[offset] == ":":
            offset += 1
        start = offset
        _, offset = decoder.raw_decode(text, offset)
        if key == "payload":
            payload_text = text[start:offset]
    require(payload_text is not None and isinstance(envelope["payload"], dict), "allocation payload is absent or malformed")
    digest = hashlib.sha256(payload_text.encode()).hexdigest()
    require(digest == envelope["sha256"], "allocation payload checksum differs from envelope")
    return envelope["payload"], payload_text


def verified_record_envelope(raw: bytes) -> dict[str, Any]:
    """Verify the persisted envelope without claiming a fresh live audit."""
    return _verified_record_envelope(raw)[0]


def verified_record_payload(raw: bytes, summary: dict[str, Any]) -> dict[str, Any]:
    payload, payload_text = _verified_record_envelope(raw)
    exact = json.loads(payload_text, parse_float=JSONNumber, parse_int=JSONNumber)
    canonical = hashlib.sha256(go_canonical_json(exact).encode()).hexdigest()
    require(canonical == summary.get("SHA256"), "allocation changed since the complete audit")
    # Go's Record omits an empty CID; the CLI summary always emits CID as a
    # string. Normalize omission only, never null or a malformed value.
    payload_cid = payload.get("cid", "")
    summary_cid = summary.get("CID")
    require(isinstance(payload_cid, str) and isinstance(summary_cid, str)
            and isinstance(payload.get("id"), str) and payload["id"]
            and payload["id"] == summary.get("ID") and payload_cid == summary_cid,
            "allocation payload identity differs from audit")
    return payload


def disk_correlation_token(cid: str) -> str:
    payload = _decode_cid_payload(cid)
    metadata = payload.get("m")
    token = metadata.get("id") if isinstance(metadata, dict) else None
    require(isinstance(token, str) and re.fullmatch(r"bpd-[0-9a-f]{16}", token), "persistent CID lacks a stable token")
    return token


def guest_disk_mapping(payload: Any, dedicated: bool) -> dict[str, list[str]]:
    """Use independent QGA filesystem evidence, not just host drive-slot names."""
    rows = payload.get("result") if isinstance(payload, dict) else payload
    require(isinstance(rows, list), "guest filesystem evidence is unavailable")
    mounts: dict[str, list[str]] = {}
    for row in rows:
        require(isinstance(row, dict), "guest filesystem evidence is malformed")
        mount = row.get("mountpoint")
        if mount not in {"/", "/var/vcap/data"}:
            continue
        require(mount not in mounts, "guest mount mapping is ambiguous")
        disks = row.get("disk")
        require(isinstance(disks, list) and disks, "guest mount lacks disk evidence")
        devices = []
        for disk in disks:
            dev = disk.get("dev") if isinstance(disk, dict) else None
            require(isinstance(dev, str) and dev.startswith("/dev/"), "guest disk device is not observable")
            # QGA reports a whole disk or its partition. These are supported
            # Linux BOSH virtio/SCSI/NVMe names; unknown mapper stacks fail closed.
            match = re.fullmatch(r"(/dev/(?:[svh]d[a-z]+|nvme[0-9]+n[0-9]+))(?:p?[0-9]+)?", dev)
            require(match is not None, "guest disk mapping uses an unsupported device topology")
            devices.append(match.group(1))
        mounts[mount] = sorted(set(devices))
    require(set(mounts) == {"/", "/var/vcap/data"}, "root and BOSH data mounts must both be observed")
    require((mounts["/"] != mounts["/var/vcap/data"]) == dedicated, "guest data mount does not match root-derived/dedicated layout")
    return mounts


class ScenarioRunner:
    def __init__(self, config_path: str, manifest: dict[str, Any], cpi_bin: str, report_path: str,
                 cases: list[str] | None = None, suite: str = "policy") -> None:
        self.config_path, self.cpi_bin = str(Path(config_path).resolve()), str(Path(cpi_bin).resolve())
        self.base_config = json.loads(Path(config_path).read_text())
        self.policy = validate_manifest(manifest, self.base_config, cases, suite)
        self.capabilities = policy_capabilities(cases, suite)
        self.manifest = manifest
        self.report_path = Path(report_path)
        self.verification = PlacementVerification(self.config_path, self.cpi_bin, str(self.report_path.with_suffix(".preflight.json")))
        self.verifier = PVEVerifier(self.base_config)
        self._configs = tempfile.TemporaryDirectory(prefix="cpi-policy-configs-")
        self._sequence = 0
        self._authority: str | None = None
        self.report: dict[str, Any] = {
            "schema_version": 1, "binary_sha256": hashlib.sha256(Path(self.cpi_bin).read_bytes()).hexdigest(),
            "diagnostic_sha256": hashlib.sha256(Path(self.verification.cli).read_bytes()).hexdigest(),
            "manifest_sha256": digest_json(manifest), "rows": [], "complete_release_matrix": False,
            "scope": "policy/lifecycle scenarios; backend and recovery rows require their declared modules",
        }
        self.active_resources: dict[str, Any] = {}

    def checkpoint(self) -> None:
        write_report(self.report_path, {**self.report, "retained_resources": copy.deepcopy(self.active_resources)})

    def close(self) -> None:
        self._configs.cleanup()

    def derived_config(self, config: dict[str, Any], label: str) -> str:
        require(all(config.get(key) == self.base_config[key] for key in AUTHORITY_KEYS), "scenario attempted to replace journal authority")
        self._sequence += 1
        path = Path(self._configs.name) / f"{self._sequence}-{re.sub('[^a-zA-Z0-9_-]', '_', label)}.json"
        path.write_text(json.dumps(config))
        path.chmod(0o600)
        return str(path)

    def configured_policy(self, e_members: list[str] | None = None, p_members: list[str] | None = None,
                          root_members: list[str] | None = None, e_strategy: str = "spread",
                          p_strategy: str = "spread", iso: str = "follow") -> dict[str, Any]:
        require(e_strategy in STRATEGIES and p_strategy in STRATEGIES, "unsupported certification strategy")
        config = copy.deepcopy(self.base_config)
        config["storage_sets"] = {
            "cert-e": {"names": list(e_members or self.policy["ephemeral_storage_ids"]), "strategy": {"name": e_strategy, "version": 1}},
            "cert-p": {"names": list(p_members or self.policy["persistent_storage_ids"]), "strategy": {"name": p_strategy, "version": 1}},
        }
        config["ephemeral_storage_set"], config["persistent_storage_set"] = "cert-e", "cert-p"
        config.pop("root_storage_set", None)
        if root_members:
            config["storage_sets"]["cert-r"] = {"names": list(root_members), "strategy": {"name": e_strategy, "version": 1}}
            config["root_storage_set"] = "cert-r"
        require(iso in {"follow", "fixed"}, "unsupported ISO policy")
        config["iso_storage"] = "local" if iso == "follow" else self.policy["fixed_iso_storage_id"]
        config["iso_storage_follow_vm_storage"] = iso == "follow"
        # Certification must finish exact resource disposition, irrespective of
        # the base deployment's opt-in retention preference.
        return config

    def vm_arguments(self, scenario_id: str, cloud_properties: dict[str, Any] | None = None,
                     dedicated: bool = True) -> list[Any]:
        props = copy.deepcopy(self.policy["vm_cloud_properties"])
        props.update(cloud_properties or {})
        props["retain_ephemeral_on_delete"] = False
        require("bosh-retain-ephemeral" not in str(props.get("tags", "")), "retention tag is incompatible with disposition certification")
        if dedicated:
            props["ephemeral_disk_size_mb"] = self.policy["ephemeral_size_mib"]
        else:
            props.pop("ephemeral_disk_size_mb", None)
        # BOSH uses agent_id as the guest hostname. Scenario labels belong in
        # reports; a UUID stays within the hostname limit and preserves uniqueness.
        return [str(uuid.uuid4()), self.policy["stemcell_cid"], props,
                copy.deepcopy(self.policy["networks"]), [], copy.deepcopy(self.policy["environment"])]

    def call_once(self, method: str, args: list[Any], config_path: str = "") -> Any:
        request = {"method": method, "arguments": args, "context": {"request_id": str(uuid.uuid4())}, "api_version": 2}
        result = subprocess.run([self.cpi_bin, "--config", config_path or self.config_path],
                                input=json.dumps(request) + "\n", capture_output=True, text=True,
                                timeout=1800, check=False)
        if result.returncode:
            # Keep diagnostics available to expected-rejection classification,
            # but never copy arbitrary CPI stderr into a certification report.
            raise CPIRejected(result.stderr)
        try:
            response = json.loads(result.stdout)
        except (ValueError, TypeError) as error:
            raise ScenarioFailure("CPI returned an invalid response envelope") from error
        require(isinstance(response, dict), "CPI returned an invalid response envelope")
        if response.get("error") is not None:
            error = response["error"]
            raise CPIRejected(str(error.get("message", "")) if isinstance(error, dict) else "")
        return response.get("result")

    def diagnostic(self, method: str, args: list[Any], config_path: str) -> dict[str, Any]:
        with tempfile.TemporaryDirectory(prefix="cpi-policy-request-") as directory:
            request = Path(directory) / "request.json"
            request.write_text(json.dumps({"method": method, "arguments": args}))
            request.chmod(0o600)
            value = self.verification._json_command([self.verification.cli, "storage-plan", "--config", config_path, "--request", str(request), "--json"])
        require(isinstance(value, dict) and value.get("observation_only") is True and value.get("targets"), "production diagnostic did not produce a feasible plan")
        return value

    def audit(self, config_path: str = "") -> dict[str, Any]:
        output = self.verification._json_command([self.cpi_bin, "storage-journal", "audit", "--config", config_path or self.config_path])
        audit = output.get("audit", {})
        require(output.get("generation_index_healthy") is True and output.get("cluster_continuity") is True,
                "journal generation index or cluster continuity is not healthy")
        require(audit.get("complete") is True and audit.get("vm_scan_complete") is True and not audit.get("issues") and not audit.get("conflicts"), "journal audit is incomplete or inconsistent")
        require("records" in output, "journal audit omitted record summaries")
        if output["records"] is None:
            output["records"] = []
        require(isinstance(output["records"], list), "journal audit has malformed record summaries")
        self.assert_authority()
        return output

    def assert_authority(self) -> None:
        authority = Path(self.base_config["storage_allocation_journal_dir"]) / hashlib.sha256(self.base_config["storage_placement_namespace"].encode()).hexdigest() / "authority.json"
        fingerprint = hashlib.sha256(authority.read_bytes()).hexdigest()
        if self._authority is None:
            self._authority = fingerprint
        require(self._authority == fingerprint, "journal authority changed during certification")

    def record_for(self, audit: dict[str, Any], cid: str, kind: str, allocation: str | None = None) -> dict[str, Any]:
        rows = [row for row in audit["records"] if row.get("CID") == cid and row.get("Kind") == kind]
        if allocation is not None:
            rows = [row for row in rows if row.get("ID") == allocation]
        elif kind == "vm":
            return self.current_vm_record(audit, cid, rows)
        require(len(rows) == 1 and re.fullmatch(r"[0-9a-f-]{36}", str(rows[0].get("ID", ""))), "CID lacks unique full allocation UUID")
        return rows[0]

    def current_vm_record(self, audit: dict[str, Any], cid: str, rows: list[dict[str, Any]]) -> dict[str, Any]:
        require(audit.get("generation_index_healthy") is True and audit.get("cluster_continuity") is True,
                "VM generation requires healthy audited authority")
        proof = audit.get("audit", {})
        require(proof.get("complete") is True and proof.get("vm_scan_complete") is True and not proof.get("issues") and not proof.get("conflicts"), "VM generation audit is incomplete or inconsistent")
        live = [row for row in rows if row.get("State") not in {"deleted", "cleaned", "vm_deleted_retained"}]
        require(len(live) == 1 and live[0].get("State") in {"ready_to_return", "adopted"}, "VM CID has ambiguous or unready live generations")
        summary = live[0]
        identity = str(summary.get("ID", ""))
        require(re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}", identity), "VM generation lacks a full allocation UUID")
        namespace = self.base_config["storage_placement_namespace"]
        directory = Path(self.base_config["storage_allocation_journal_dir"]) / hashlib.sha256(namespace.encode()).hexdigest()
        with (directory / ("allocation-" + identity + ".json")).open("rb") as stream:
            record = verified_record_payload(stream.read(16 * 1024 * 1024 + 1), summary)
        require(record.get("kind") == "vm" and record.get("namespace") == namespace and record.get("state") == summary.get("State"), "VM generation record differs from authority")
        agent = record.get("agent_id")
        require(isinstance(agent, str) and agent, "VM generation lacks an agent identity")
        agent_hash = hashlib.sha256(agent.encode()).hexdigest()
        with (directory / "index.json").open("rb") as stream:
            index = verified_record_envelope(stream.read(16 * 1024 * 1024 + 1))
        require(index.get("version") == 1 and isinstance(index.get("active_vms"), dict)
                and index["active_vms"].get(agent_hash) == identity, "VM generation differs from current agent index")
        actual = self.verifier.qemu_config(cid)
        description = actual.get("description")
        start, end = "[bosh_storage_allocation]", "[/bosh_storage_allocation]"
        require(isinstance(description, str) and description.count(start) == 1 and description.count(end) == 1, "VM ownership marker is missing or ambiguous")
        opening, closing = description.index(start) + len(start), description.index(end)
        require(closing >= opening, "VM ownership marker is malformed")
        marker = json.loads(description[opening:closing], object_pairs_hook=_unique_json_object)
        require(marker == {"version": 1, "namespace": namespace, "allocation_id": identity, "agent_sha256": agent_hash, "kind": "vm"}
                and type(marker.get("version")) is int, "VM ownership marker differs from its current generation")
        evidence = audit.get("audit", {}).get("evidence") or []
        matches = [item for item in evidence if item.get("kind") == "vm" and str(item.get("vmid")) == cid and not item.get("volume_id")]
        require(len(matches) == 1 and matches[0].get("allocation_id") == identity and matches[0].get("agent_sha256") == agent_hash, "live VM lacks unique complete audit provenance")
        return summary

    def preflight(self) -> None:
        config = self.configured_policy()
        path = self.derived_config(config, "preflight")
        self.audit(path)
        diagnostic = self.diagnostic("create_vm", self.vm_arguments("preflight"), path)
        self.verification.config = config
        self.verification.validate_topology(diagnostic,
                                            ephemeral_minimum=self.capabilities["ephemeral"],
                                            persistent_minimum=self.capabilities["persistent"])
        extra = set(self.policy["root_storage_ids"]) | {self.policy["fixed_iso_storage_id"]}
        nodes = self.verifier.cluster_nodes(online_only=False)
        require(len(nodes) >= 2, "certification requires two fully observable nodes")
        for storage in extra:
            definition = self.verifier.storage_entry(storage)
            require(isinstance(definition, dict), "additional target storage definition is unavailable")
            allowed = definition.get("nodes", "")
            require(isinstance(allowed, str), "target node restriction is malformed")
            permitted = set(allowed.replace(",", " ").split()) or set(nodes)
            require(bool(permitted & set(nodes)), "additional target has no observable permitted node")
            self.verification.inventory_pairs |= {(node, storage) for node in nodes if node in permitted}
        self.verification.volume_inventory(self.verifier)
        self.verification.diagnostic = copy.deepcopy(diagnostic)
        self.report["planning"] = copy.deepcopy(diagnostic)
        self.report["topology"] = {"members": {role: sorted(ids) for role, ids in self.verification.members.items()}, "nodes": sorted(nodes)}
        self.report["authority_sha256"] = self._authority

    def observed_vm(self, vmid: str, config: dict[str, Any], dedicated: bool, root_evidence: dict[str, Any] | None = None, guest_mounts: dict[str, list[str]] | None = None) -> dict[str, Any]:
        actual = self.verifier.qemu_config(vmid)
        root_slot = "scsi0" if config.get("root_disk_bus", "").lower() == "scsi" else "virtio0"
        disks = {slot: str(value).split(",", 1)[0] for slot, value in actual.items()
                 if re.fullmatch(r"(?:scsi|virtio|sata|ide|unused)\d+", slot) and ":" in str(value) and "media=cdrom" not in str(value)}
        external = set((root_evidence or {}).get("external_volumes", []))
        require(external <= set(disks.values()), "verified persistent disk is missing from the guest")
        disks = {slot: volume for slot, volume in disks.items() if volume not in external}
        wanted = {root_slot, "scsi1"} if dedicated else {root_slot}
        if root_evidence:
            auxiliary = set(root_evidence["auxiliary_devices"])
            require(not auxiliary & wanted, "source auxiliary disk collides with a guest role device")
            wanted |= auxiliary
            require(set(root_evidence["root_owned_volumes"]) <= set(disks.values()), "observed root mutation volumes are missing from actual guest devices")
        require(set(disks) == wanted and len(set(disks.values())) == len(disks), "VM has missing, duplicate, or extra disk devices")
        root_set = config.get("root_storage_set", config["ephemeral_storage_set"])
        require(disks[root_slot].split(":", 1)[0] in config["storage_sets"][root_set]["names"], "VM root escaped its effective role set")
        if dedicated:
            require(disks["scsi1"].split(":", 1)[0] in config["storage_sets"][config["ephemeral_storage_set"]]["names"], "ephemeral volume escaped its role set")
            if "root_storage_set" not in config:
                require(disks["scsi1"].split(":", 1)[0] == disks[root_slot].split(":", 1)[0], "default root/ephemeral bundle split unexpectedly")
        for volume in disks.values():
            PlacementVerification._verified_volume(self.observed_volume(volume), volume)
        isos = {slot: str(value).split(",", 1)[0] for slot, value in actual.items() if "media=cdrom" in str(value) and ":" in str(value)}
        require(len(isos) == 1, "agent ISO volume must be independently observed")
        iso = next(iter(isos.values()))
        expected_iso = disks[root_slot].split(":", 1)[0] if config["iso_storage_follow_vm_storage"] else config["iso_storage"]
        require(iso.split(":", 1)[0] == expected_iso, "agent ISO did not follow its effective fixed/root policy")
        info = self.observed_volume(iso)
        PlacementVerification._verified_volume(info, iso)
        require(info["size"] == 10 * 1024 * 1024, "actual configdrive size differs from its frozen allocation")
        return {"devices": disks, "iso": isos, "guest_mounts": guest_mounts if guest_mounts is not None else self.observe_guest_mapping(vmid, dedicated)}

    def observe_guest_mapping(self, vmid: str, dedicated: bool) -> dict[str, list[str]]:
        node = self.verifier._node_hosting(vmid)
        deadline = time.monotonic() + 180
        while True:
            try:
                payload = self.verifier._get(f"/nodes/{quote(node, safe='')}/qemu/{quote(vmid, safe='')}/agent/get-fsinfo")
                return guest_disk_mapping(payload, dedicated)
            except RuntimeError:
                if time.monotonic() >= deadline:
                    raise ScenarioFailure("guest root/data mapping could not be proven") from None
                time.sleep(2)

    def observed_root_mechanism(self, summary: dict[str, Any], config: dict[str, Any]) -> dict[str, Any]:
        namespace = hashlib.sha256(self.base_config["storage_placement_namespace"].encode()).hexdigest()
        record_path = Path(self.base_config["storage_allocation_journal_dir"]) / namespace / ("allocation-" + summary["ID"] + ".json")
        with record_path.open("rb") as stream:
            record = verified_record_payload(stream.read(16 * 1024 * 1024 + 1), summary)
        return self.observe_root_record(record, summary, config)

    def observe_root_record(self, record: dict[str, Any], summary: dict[str, Any], config: dict[str, Any]) -> dict[str, Any]:
        """Observe a record already verified against its authority and audit hash."""
        attempts = record.get("attempts") or []
        active = len(attempts) - 1 if attempts else 0
        intent = attempts[-1]["plan"] if attempts else record["intent"]
        plan = intent["plan"]
        roots = [target for target in plan.get("Targets", []) if target.get("Role") == "root"]
        require(len(roots) == 1, "frozen actual plan lacks one root target")
        root = roots[0]
        mechanism, source = root.get("Mechanism"), root.get("Source")
        require(mechanism in {"import", "full_clone", "linked_clone"} and isinstance(source, dict), "frozen root mechanism or source is unsupported")
        expected = self.policy.get("expected_root_mechanism")
        require(expected is None or expected == "auto" and config.get("clone_mode", "auto") == "auto" or mechanism == expected, "actual allocation did not use the fixture's required root mechanism")
        mode = config.get("clone_mode", "auto")
        require(mode != "linked" or mechanism == "linked_clone", "linked-only clone policy was not honored")
        require(mode != "full" or mechanism != "linked_clone", "full clone policy used a linked overlay")
        volume = source.get("VolumeID", "")
        require(isinstance(volume, str) and ":" in volume and source.get("Node"), "root source identity is incomplete")
        storage, suffix = volume.split(":", 1)
        info = self.verifier._get(f"/nodes/{quote(source['Node'], safe='')}/storage/{quote(storage, safe='')}/content/{quote(suffix, safe='')}")
        require(isinstance(info, dict) and info.get("size") == source.get("VirtualBytes"), "root source virtual size no longer matches the frozen source")
        template = source.get("TemplateVMID", 0)
        if mechanism == "import":
            require(template == 0 and info.get("format") == "qcow2" and suffix.startswith("import/"), "import source format or identity is invalid")
        else:
            require(type(template) is int and template > 0, "clone source lacks a template VMID")
            source_config = self.verifier.qemu_config(template, source["Node"])
            require(source_config.get("template") in (1, "1", True), "recorded clone source is no longer a template")
            require(any(str(value).split(",", 1)[0] == volume for key, value in source_config.items() if re.fullmatch(r"(?:scsi|virtio|sata|ide)\d+", key)), "template no longer references the frozen root source")
        auxiliary_devices = []
        for auxiliary in source.get("AuxiliaryVolumes") or []:
            device, auxiliary_volume = auxiliary.get("Device"), auxiliary.get("VolumeID", "")
            require(isinstance(device, str) and re.fullmatch(r"(?:scsi|virtio|sata|ide)\d+", device) and ":" in auxiliary_volume, "source auxiliary identity is invalid")
            require(template > 0 and str(source_config.get(device, "")).split(",", 1)[0] == auxiliary_volume, "source auxiliary device no longer matches the frozen source")
            aux_storage, aux_suffix = auxiliary_volume.split(":", 1)
            aux_info = self.verifier._get(f"/nodes/{quote(auxiliary['Node'], safe='')}/storage/{quote(aux_storage, safe='')}/content/{quote(aux_suffix, safe='')}")
            require(isinstance(aux_info, dict) and aux_info.get("size") == auxiliary.get("VirtualBytes"), "source auxiliary size changed")
            auxiliary_devices.append(device)
        kind = "vm.QEMU.Create" if mechanism == "import" else "vm.Nodes.CreateQemuClone"
        steps = [step for step in record.get("steps", []) if step.get("attempt", 0) == active and step.get("kind") == kind]
        require(len(steps) == 1 and steps[0].get("state") == "observed", "actual root allocation lacks one observed mutation")
        require(isinstance(steps[0].get("volids"), list) and steps[0]["volids"], "root allocation lacks actual owned volume identities")
        upid = steps[0].get("upid", "")
        require(isinstance(upid, str) and upid.startswith("UPID:") and len(upid.split(":")) > 2, "actual root mutation lacks task identity")
        task_node = upid.split(":")[1]
        require(task_node in {source["Node"], root["Node"]}, "root task belongs to an unrecorded node")
        status = self.verifier._get(f"/nodes/{quote(task_node, safe='')}/tasks/{quote(upid, safe='')}/status")
        require(isinstance(status, dict) and status.get("status") == "stopped" and status.get("exitstatus") == "OK", "actual root task did not complete successfully")
        return {"mechanism": mechanism, "source": source, "target_storage": root["StorageID"], "target_backing": root["BackingKey"], "root_step_id": steps[0]["id"], "upid": upid, "task_status": "stopped", "task_exitstatus": "OK", "record_sha256": summary["SHA256"], "auxiliary_devices": auxiliary_devices, "root_owned_volumes": steps[0].get("volids", [])}

    def observed_volume(self, volume: str) -> dict[str, Any]:
        """Read a physical volume directly; VM volumes are not encoded disk CIDs."""
        rows = self.verification.volume_inventory(self.verifier)
        matches = [row for row in rows if row[2] == volume]
        require(matches and len({row[3] for row in matches}) == 1, "physical volume is absent or has inconsistent size across observed nodes")
        return {"volid": volume, "size": matches[0][3]}

    def observed_disk(self, cid: str, config_path: str, previous: dict[str, Any] | None = None,
                      members: list[str] | None = None, size_mib: int | None = None) -> dict[str, Any]:
        token = disk_correlation_token(cid)
        audit = self.audit(config_path)
        record = self.record_for(audit, cid, "disk")
        evidence = audit["audit"].get("evidence") or []
        volumes = {row.get("volume_id") for row in evidence if row.get("allocation_id") == record["ID"] and row.get("volume_id")}
        require(len(volumes) == 1, "persistent allocation has missing or ambiguous actual volume evidence")
        volume = next(iter(volumes))
        info = self.observed_volume(volume)
        PlacementVerification._verified_volume(info, volume)
        storage = volume.split(":", 1)[0]
        definition = self.verifier.storage_entry(storage)
        require(isinstance(definition, dict) and definition.get("type") == "nfs" and definition.get("server") and definition.get("export"), "persistent backing identity is not observable NFS")
        backing = {key: definition[key] for key in ("type", "server", "export")}
        if members is not None:
            require(storage in members, "persistent disk escaped its selected subset")
        if size_mib is not None:
            rounded = ((size_mib + 1023) // 1024) * 1024**3
            require(info["size"] == rounded, "persistent disk actual size differs from rounded request")
        audit = self.audit(config_path)
        record = self.record_for(audit, cid, "disk")
        # Independent complete audit excludes invisible holders/duplicates that
        # the convenience holder lookup alone could otherwise silently skip.
        evidence = audit["audit"].get("evidence") or []
        require(any(row.get("allocation_id") == record["ID"] and row.get("volume_id") == volume for row in evidence), "actual volume lacks full journal UUID provenance")
        holders = self.verifier.disk_holders(cid)
        require(len(holders) <= 1, "persistent volume has duplicate holders")
        for vmid, slot in holders:
            drive = str(self.verifier.qemu_config(vmid).get(slot, ""))
            require(drive.split(",", 1)[0] == volume and f"serial={token}" in drive.split(","), "actual holder does not prove exact volume and serial")
        observed = {"allocation_uuid": record["ID"], "stable_token": token, "volume_id": volume,
                    "backing": backing, "size_bytes": info["size"], "holders": holders}
        if previous:
            require(all(observed[key] == previous[key] for key in ("allocation_uuid", "stable_token", "backing")), "persistent UUID, stable token, or backing changed across lifecycle")
        return observed

    @staticmethod
    def assert_strategy(diagnostic: dict[str, Any], role: str, name: str) -> None:
        policy = diagnostic.get("strategies", {}).get(role, {})
        require(policy.get("Name") == name and policy.get("Version") == 1, "production plan did not use the requested role strategy version")

    def snapshot_present(self, snapshot: str, expected: bool) -> None:
        require(isinstance(snapshot, str) and re.fullmatch(r"[0-9]+:[^:/\s]+", snapshot), "snapshot CID is malformed")
        vmid, name = snapshot.split(":", 1)
        node = self.verifier._node_hosting(vmid)
        rows = self.verifier._get(f"/nodes/{quote(node, safe='')}/qemu/{vmid}/snapshot")
        require(isinstance(rows, list), "snapshot inventory is unavailable")
        found = sum(isinstance(row, dict) and row.get("name") == name for row in rows)
        require(found == (1 if expected else 0), "snapshot physical presence differs from requested disposition")

    def removed_membership_config(self, config: dict[str, Any]) -> dict[str, Any]:
        changed = copy.deepcopy(config)
        for key in ("storage_sets", "root_storage_set", "ephemeral_storage_set", "persistent_storage_set"):
            changed.pop(key, None)
        # Reversed scalar defaults exercise CID authority rather than accidentally
        # recreating the original set choices. The existing journal is unchanged.
        changed["vm_storage"] = self.policy["persistent_storage_ids"][0]
        changed["disk_storage"] = self.policy["ephemeral_storage_ids"][0]
        return changed

    def lifecycle(self, scenario_id: str, config: dict[str, Any], dedicated: bool = True,
                  remove_membership: bool = False, change_strategy: bool = False,
                  disk_properties: dict[str, Any] | None = None,
                  disk_members: list[str] | None = None) -> dict[str, Any]:
        path = self.derived_config(config, scenario_id)
        args = self.vm_arguments(scenario_id, dedicated=dedicated)
        self.active_resources = {"scenario_id": scenario_id, "agent_id": args[0], "pending_operation": "diagnostic_create_vm"}
        self.checkpoint()
        vm_plan = self.diagnostic("create_vm", args, path)
        e_name = config["storage_sets"][config["ephemeral_storage_set"]]["strategy"]["name"]
        self.assert_strategy(vm_plan, "root", e_name)
        if dedicated:
            self.assert_strategy(vm_plan, "ephemeral", e_name)
        self.active_resources["pending_operation"] = "create_vm"
        self.checkpoint()
        result = self.call_once("create_vm", args, path)
        require(isinstance(result, list) and result and isinstance(result[0], str), "create_vm did not return a CID")
        vmid = result[0]
        self.active_resources = {"scenario_id": scenario_id, "agent_id": args[0], "vm_cid": vmid}
        self.checkpoint()
        vm_record = self.record_for(self.audit(path), vmid, "vm")
        root_evidence = self.observed_root_mechanism(vm_record, config)
        observed_vm = self.observed_vm(vmid, config, dedicated, root_evidence)
        # All planned actual disk sizes must survive realization; allocation
        # overhead does not excuse an undersized guest-visible root or E volume.
        for target in vm_plan["targets"]:
            role = target.get("Role")
            if role not in {"root", "ephemeral"}:
                continue
            slot = "scsi1" if role == "ephemeral" else ("scsi0" if str(config.get("root_disk_bus", "")).lower() == "scsi" else "virtio0")
            actual = self.observed_volume(observed_vm["devices"][slot])
            require(actual["size"] >= target.get("VirtualBytes", 1), "realized guest disk is smaller than its production plan")
        if change_strategy:
            config = copy.deepcopy(config)
            for value in config["storage_sets"].values():
                value["strategy"] = {"name": "weighted_free_space", "version": 1}
            path = self.derived_config(config, scenario_id + "-changed")
            before = self.audit(path)
            resumed = self.call_once("create_vm", args, path)
            require(resumed == result, "strategy change replaced the existing VM generation")
            after = self.audit(path)
            require(self.record_for(after, vmid, "vm")["ID"] == vm_record["ID"], "strategy change replaced the full VM allocation UUID")
            require({row["ID"] for row in before["records"]} == {row["ID"] for row in after["records"]}, "resume allocated an additional generation")
        props = copy.deepcopy(disk_properties or {})
        disk_args = [self.policy["disk_size_mib"], props, vmid]
        disk_plan = self.diagnostic("create_disk", disk_args, path)
        selected = props.get("storage_set", config["persistent_storage_set"])
        self.assert_strategy(disk_plan, "persistent", config["storage_sets"][selected]["strategy"]["name"])
        self.active_resources["pending_operation"] = "create_disk"
        self.checkpoint()
        cid = self.call_once("create_disk", disk_args, path)
        require(isinstance(cid, str), "create_disk did not return a CID")
        self.active_resources["disk_cid"] = cid
        self.active_resources.pop("pending_operation", None)
        self.checkpoint()
        members = disk_members or config["storage_sets"][selected]["names"]
        previous = self.observed_disk(cid, path, members=members, size_mib=self.policy["disk_size_mib"])
        history = [{"operation": "create_disk", **previous}]
        if remove_membership:
            config = self.removed_membership_config(config)
            path = self.derived_config(config, scenario_id + "-removed")
        for method in ("attach_disk", "resize_disk", "snapshot_disk", "detach_disk", "attach_disk", "detach_disk"):
            if method == "resize_disk":
                size = self.policy["disk_size_mib"] + 1024
                self.call_once(method, [cid, size], path)
            elif method == "snapshot_disk":
                snapshot = self.call_once(method, [cid, {"deployment": "storage-certification"}], path)
                self.active_resources["snapshot_cid"] = snapshot
                self.checkpoint()
                self.snapshot_present(snapshot, True)
                self.call_once("delete_snapshot", [snapshot], path)
                self.snapshot_present(snapshot, False)
                self.active_resources.pop("snapshot_cid", None)
                size = self.policy["disk_size_mib"] + 1024
            else:
                self.call_once(method, [vmid, cid], path)
                size = self.policy["disk_size_mib"] if len(history) == 1 else self.policy["disk_size_mib"] + 1024
            current = self.observed_disk(cid, path, previous, size_mib=size)
            holders = current["holders"]
            if method == "attach_disk":
                require(len(holders) == 1 and str(holders[0][0]) == vmid, "attach did not bind the intended VM")
            if method == "detach_disk":
                require(all(str(holder[0]) != vmid for holder in holders), "detach retained the active VM holder")
                if config.get("detached_disk_strategy", "parked") == "parked":
                    require(len(holders) == 1, "parked detach did not preserve a unique holder")
                else:
                    require(not holders, "free detach unexpectedly retained a holder")
            history.append({"operation": method, **current})
            previous = current
        self.call_once("delete_disk", [cid], path)
        self.assert_deleted(cid, "disk", previous["allocation_uuid"], path, {item["volume_id"] for item in history})
        self.active_resources.pop("disk_cid", None)
        self.call_once("delete_vm", [vmid], path)
        require(not self.verifier.vm_exists(vmid), "deleted VM is still present")
        volumes = set(observed_vm["devices"].values()) | set(observed_vm["iso"].values())
        self.assert_deleted(vmid, "vm", vm_record["ID"], path, volumes)
        self.active_resources = {}
        return {"vm_cid": vmid, "vm_allocation_uuid": vm_record["ID"], "vm": observed_vm, "root_execution": root_evidence,
                "disk_cid": cid, "disk_history": history, "membership_removed_before_lifecycle": remove_membership,
                "strategy_changed_before_resume": change_strategy, "actual_absence_verified": True,
                "authority_sha256": self._authority,
                "policy_sha256": digest_json({key: config[key] for key in POLICY_CONFIG_KEYS if key in config})}

    def assert_deleted(self, cid: str, kind: str, allocation: str, path: str, volumes: set[str]) -> None:
        audit = self.audit(path)
        record = self.record_for(audit, cid, kind, allocation)
        require(record["ID"] == allocation and record.get("State") in {"deleted", "cleaned"}, "deleted resource lacks matching terminal journal disposition")
        require(not any(row.get("allocation_id") == allocation for row in audit["audit"].get("evidence") or []), "deleted allocation still has remote evidence")
        inventory = self.verification.volume_inventory(self.verifier)
        require(not any(row[2] in volumes for row in inventory), "deleted allocation has a remaining actual volume")

    def rejected_without_mutation(self, config: dict[str, Any], properties: dict[str, Any], reason: str) -> dict[str, Any]:
        path = self.derived_config(config, "rejected-selector")
        # Use the original valid policy for audit; the deliberately invalid
        # startup assertion itself must not make absence observation impossible.
        before = self.audit()
        volumes = self.verification.volume_inventory(self.verifier)
        try:
            self.call_once("create_disk", [self.policy["disk_size_mib"], properties, ""], path)
        except CPIRejected as error:
            require(re.search(reason, error.reason, re.IGNORECASE) is not None, "request failed for an unrelated cause instead of selector policy")
        else:
            raise ScenarioFailure("forbidden selector unexpectedly allocated a disk")
        after = self.audit()
        require(before.get("records") == after.get("records") and before["audit"].get("evidence") == after["audit"].get("evidence"), "rejected selector changed journal or owned-resource evidence")
        require(volumes == self.verification.volume_inventory(self.verifier), "rejected selector changed physical storage inventory")
        return {"selector_rejected": True, "journal_unchanged": True, "volumes_unchanged": True}

    def selector_case(self) -> dict[str, Any]:
        encrypted = self.policy["encrypted_persistent_storage_ids"]
        config = self.configured_policy(p_members=encrypted)
        config["storage_sets"]["cert-p"]["encrypted"] = True
        config["storage_sets"]["cert-subset"] = copy.deepcopy(config["storage_sets"]["cert-p"])
        config["storage_sets"]["cert-subset"]["names"] = encrypted[:1]
        result = self.lifecycle("selector-subset-encryption", config,
                                disk_properties={"storage_set": "cert-subset", "encrypted": True}, disk_members=encrypted[:1])
        negative = []
        negative.append(self.rejected_without_mutation(config, {"storage_pool": self.policy["ephemeral_storage_ids"][0]}, r"boundary|outside|member"))
        negative.append(self.rejected_without_mutation(config, {"storage_set": "cert-subset", "storage_pool": encrypted[0]}, r"competing|conflict|same.layer"))
        tier_config = copy.deepcopy(config)
        tier_config.setdefault("storage_tiers", {})["cert-escape"] = self.policy["escaping_tier_criteria"]
        negative.append(self.rejected_without_mutation(tier_config, {"storage_tier": "cert-escape"}, r"boundary|outside|member"))
        weakened = copy.deepcopy(config)
        weakened["storage_sets"]["cert-subset"]["encrypted"] = False
        negative.append(self.rejected_without_mutation(weakened, {"storage_set": "cert-subset", "encrypted": False}, r"encrypt|assert|conflict"))
        result["negative_selectors"] = negative
        return result

    def run_policy(self, identifiers: list[str] | None = None) -> list[dict[str, Any]]:
        cases: list[tuple[str, dict[str, Any]]] = []
        for strategy in STRATEGIES:
            for p_width in ("singleton", "plural"):
                for e_width in ("singleton", "plural"):
                    identifier = f"policy-{strategy}-p{p_width}-e{e_width}"
                    config = self.configured_policy(
                        p_members=self.policy["persistent_storage_ids"][:1] if p_width == "singleton" else None,
                        e_members=self.policy["ephemeral_storage_ids"][:1] if e_width == "singleton" else None,
                        e_strategy=strategy, p_strategy=strategy)
                    cases.append((identifier, {"config": config}))
        cases += [
            ("root-derived-follow-iso", {"config": self.configured_policy(), "dedicated": False}),
            ("root-split-follow-iso", {"config": self.configured_policy(root_members=self.policy["root_storage_ids"])}),
            ("root-split-fixed-iso", {"config": self.configured_policy(root_members=self.policy["root_storage_ids"], iso="fixed", e_strategy="least_utilized", p_strategy="weighted_free_space")}),
            ("selector-subset-encryption", {}),
            ("lifecycle-membership-removal", {"config": self.configured_policy(), "remove_membership": True}),
            ("strategy-change-resume", {"config": self.configured_policy(), "change_strategy": True}),
        ]
        if identifiers is not None:
            require(identifiers and len(set(identifiers)) == len(identifiers) and set(identifiers) <= set(POLICY_IDS), "selected policy cases are invalid")
            cases = [(identifier, options) for identifier, options in cases if identifier in identifiers]
        rows = []
        previous_rows = list(self.report["rows"])
        for identifier, options in cases:
            try:
                evidence = self.selector_case() if identifier == "selector-subset-encryption" else self.lifecycle(identifier, **options)
                rows.append({"scenario_id": identifier, "status": "passed", "evidence": evidence})
            except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as error:
                # Do not turn arbitrary server text, request credentials, or
                # shell output into published evidence. Preserve only identities.
                rows.append({"scenario_id": identifier, "status": "failed", "evidence": {"reason": str(error) if type(error) is ScenarioFailure else "scenario assertion or operation failed", "retained_resources": copy.deepcopy(self.active_resources)}})
                # A failed/uncertain generation must be reconciled before any
                # other case can exercise replacement or consume its capacity.
                for remaining, _ in cases[len(rows):]:
                    rows.append({"scenario_id": remaining, "status": "missing", "evidence": {"reason": "earlier failure requires audited recovery"}})
                self.report["rows"] = previous_rows + rows
                self.checkpoint()
                break
            self.report["rows"] = previous_rows + rows
            self.checkpoint()
        return rows


def blocked_rows(identifiers: Any) -> list[dict[str, Any]]:
    return [{"scenario_id": identifier, "status": "missing", "evidence": {"reason": "earlier failure requires audited recovery"}} for identifier in identifiers]


def write_report(path: Path, report: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".storage-certification-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w") as stream:
            json.dump(report, stream, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--cpi-bin", required=True)
    parser.add_argument("--report", required=True)
    parser.add_argument("--suite", choices=("policy", "faults", "recovery", "constraints", "director", "all"), default="policy")
    parser.add_argument("--case", dest="cases", choices=POLICY_IDS, action="append", help="run only this policy case; repeat for multiple cases")
    parser.add_argument("--restore-backend", metavar="STORAGE", help="restore only one declared backend from its retained fault state")
    parser.add_argument("--fault-run-id", metavar="HEX32", help="exact run ID from failed backend evidence")
    parser.add_argument("--validate-only", action="store_true", help="validate local fixture manifest only; no live certification")
    args = parser.parse_args(argv)
    report: dict[str, Any] = {"schema_version": 1, "rows": [], "complete_release_matrix": False}
    runner = None
    try:
        config = json.loads(Path(args.config).read_text())
        manifest = json.loads(Path(args.manifest).read_text())
        validate_manifest(manifest, config, args.cases, args.suite)
        require(not args.cases or args.suite == "policy" and not args.restore_backend, "case selection requires the policy suite")
        require(not args.cases or len(set(args.cases)) == len(args.cases), "policy case selection repeats a case")
        require(bool(args.restore_backend) == bool(args.fault_run_id), "backend restoration requires both storage and run ID")
        require(not args.restore_backend or not args.validate_only, "backend restoration cannot be combined with validation-only")
        if "faults" in manifest:
            from _storage_placement_faults import validate_fault_manifest
            validate_fault_manifest(manifest, config)
        if "constraints" in manifest:
            from _storage_placement_constraints import validate_constraints_manifest
            validate_constraints_manifest(manifest, config)
        if "recovery" in manifest:
            from _storage_placement_recovery import validate_recovery_manifest
            validate_recovery_manifest(manifest["recovery"])
        if "director" in manifest:
            from _storage_placement_director import validate_director_manifest
            validate_director_manifest(manifest, config)
        if args.restore_backend:
            from _storage_placement_faults import restore_backend_fixture
            report["restoration"] = restore_backend_fixture(manifest, config, args.restore_backend, args.fault_run_id)
            report["restoration_only"] = True
        elif args.validate_only:
            report.update({"validation_only": True, "manifest_valid": True, "generated_config_validated": False,
                           "scope": "local fixture syntax only; packaged configuration and all live cases remain unexecuted"})
        else:
            runner = ScenarioRunner(args.config, manifest, args.cpi_bin, args.report, cases=args.cases, suite=args.suite)
            report = runner.report
            runner.preflight()
            if args.suite in {"policy", "all"}:
                report["selected_policy_cases"] = args.cases or POLICY_IDS
                previous_policy_rows = list(report["rows"])
                policy_rows = runner.run_policy(args.cases)
                report["rows"] = previous_policy_rows + policy_rows
            prior_failure = any(row["status"] != "passed" for row in report["rows"])
            if args.suite in {"constraints", "all"}:
                from _storage_placement_constraints import run_constraint_scenarios, CONSTRAINT_IDS
                report["rows"].extend(blocked_rows(CONSTRAINT_IDS) if prior_failure else run_constraint_scenarios(manifest, runner))
            prior_failure = any(row["status"] != "passed" for row in report["rows"])
            if args.suite in {"faults", "all"}:
                from _storage_placement_faults import run_fault_scenarios, BACKEND_IDS, PROXY_IDS, VM_IDS
                report["rows"].extend(blocked_rows(BACKEND_IDS + PROXY_IDS + VM_IDS) if prior_failure else run_fault_scenarios(manifest, runner))
            prior_failure = any(row["status"] != "passed" for row in report["rows"])
            if args.suite in {"recovery", "all"}:
                from _storage_placement_recovery import run_recovery_cases, RECOVERY_CASES
                report["rows"].extend(blocked_rows(RECOVERY_CASES) if prior_failure else run_recovery_cases(runner, manifest.get("recovery")))
            prior_failure = any(row["status"] != "passed" for row in report["rows"])
            if args.suite in {"director", "all"}:
                from _storage_placement_director import run_director_scenarios, DIRECTOR_IDS
                report["rows"].extend(blocked_rows(DIRECTOR_IDS) if prior_failure else run_director_scenarios(manifest, runner))
            report["selected_scenarios_passed"] = bool(report["rows"]) and all(row["status"] == "passed" for row in report["rows"])
            # BATS deployment, two-node independent backend execution,
            # and upgrade/downgrade compatibility remain separate release evidence.
            report["complete_release_matrix"] = False
    except (RuntimeError, OSError, ValueError, ImportError, subprocess.SubprocessError):
        report["execution_error"] = "fixture validation, preflight, or scenario module failed; no live certification claimed"
        report["selected_scenarios_passed"] = False
        if runner is not None:
            report["retained_resources"] = copy.deepcopy(runner.active_resources)
    finally:
        if runner is not None:
            # Modules can report failures without raising through main. Preserve
            # their active resources before closing temporary local state.
            report["retained_resources"] = copy.deepcopy(runner.active_resources)
            try:
                runner.close()
            except (RuntimeError, OSError):
                report["execution_error"] = "local scenario workspace cleanup failed; inspect retained resources"
                report["selected_scenarios_passed"] = False
        write_report(Path(args.report), report)
    return 0 if report.get("manifest_valid") or report.get("selected_scenarios_passed") or report.get("restoration", {}).get("restored") is True else 1


if __name__ == "__main__":
    raise SystemExit(main())
