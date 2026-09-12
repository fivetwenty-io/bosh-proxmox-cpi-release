"""Production planner certification against explicit topology and capacity facts."""
from __future__ import annotations

import copy
import json
import re
import subprocess
import tempfile
import time
from pathlib import Path
from urllib.parse import urlsplit

from _storage_placement_faults import LoopbackPVEFaultProxy
from _storage_placement_transition import post_root_transition, post_root_capacity_rejection

CONSTRAINT_IDS = (
    "membership_regex_add_remove", "missing_storage_ids", "backing_alias_rejection", "disjoint_set_rejection",
    "node_restriction", "ha_reachability", "shared_capacity_domain", "capacity_ceiling", "capacity_overflow",
    "capacity_rounding", "unequal_capacity_members", "stale_capacity_observation", "post_root_acquired_capacity", "post_root_insufficient_capacity",
)
MIB = 1 << 20
GIB = 1 << 30


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def validate_constraints_manifest(manifest, base_config):
    value = manifest.get("constraints", {})
    if not isinstance(value, dict) or set(value) - {"missing_storage_id", "alias_storage_ids", "domain_storage_ids", "restricted_storage", "unequal_storage_ids", "stale_delay_seconds"}:
        raise ValueError("unknown or malformed constraints manifest fields")
    for field in ("alias_storage_ids", "domain_storage_ids", "unequal_storage_ids"):
        if field in value:
            members = value[field]
            if not isinstance(members, list) or len(members) < 2 or any(not isinstance(item, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", item) for item in members) or len(set(members)) != len(members):
                raise ValueError(field + " requires at least two distinct storage IDs")
    if "missing_storage_id" in value and (not isinstance(value["missing_storage_id"], str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", value["missing_storage_id"])):
        raise ValueError("missing_storage_id must be an explicit absent identity")
    restricted = value.get("restricted_storage")
    if restricted is not None:
        if not isinstance(restricted, dict) or set(restricted) != {"storage_id", "allowed_node", "excluded_node"}:
            raise ValueError("restricted_storage requires exactly storage_id, allowed_node, excluded_node")
        if any(not isinstance(item, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", item) for item in restricted.values()) or restricted["allowed_node"] == restricted["excluded_node"]:
            raise ValueError("restricted storage nodes must be distinct explicit identities")
    delay = value.get("stale_delay_seconds", 2)
    if type(delay) not in (int, float) or not 1.5 <= delay <= 5:
        raise ValueError("stale observation delay must be 1.5..5 seconds")
    return value


def _snapshot(runner):
    return {"journal": runner.verification._journal_records(), "volumes": runner.verification.volume_inventory(runner.verifier)}


def _definitions(runner):
    rows = runner.verifier._get("/storage")
    require(isinstance(rows, list), "storage definition observation is malformed")
    values = {}
    for row in rows:
        require(isinstance(row, dict) and isinstance(row.get("storage"), str), "storage definition lacks identity")
        require(row["storage"] not in values, "storage definition identity is ambiguous")
        values[row["storage"]] = row
    return values


def include_constraint_inventory(runner, options):
    stores = set()
    for key in ("alias_storage_ids", "domain_storage_ids", "unequal_storage_ids"):
        stores.update(options.get(key, []))
    if options.get("restricted_storage"):
        stores.add(options["restricted_storage"]["storage_id"])
    if not stores:
        return
    definitions = _definitions(runner)
    rows = runner.verifier._get("/nodes")
    require(isinstance(rows, list) and rows, "constraint inventory requires complete node identities")
    nodes = set()
    for row in rows:
        require(isinstance(row, dict) and isinstance(row.get("node"), str) and row["node"], "constraint node identity is malformed")
        nodes.add(row["node"])
    if options.get("restricted_storage"):
        fixture = options["restricted_storage"]
        require({fixture["allowed_node"], fixture["excluded_node"]} <= nodes, "node-restriction fixture names an absent node")
    for storage in stores:
        require(storage in definitions, "declared constraint inventory storage is absent")
        mask = definitions[storage].get("nodes", "")
        require(isinstance(mask, (str, list)), "storage node visibility declaration is malformed")
        values = mask.split(",") if isinstance(mask, str) and mask else mask
        require(all(isinstance(value, str) and value for value in values), "storage node visibility contains malformed identities")
        allowed = set(values)
        allowed = allowed if allowed else nodes
        require(allowed <= nodes, "storage node visibility includes an unknown node")
        runner.verification.inventory_pairs.update((node, storage) for node in allowed)


def _diagnostic(runner, config, method, arguments, label):
    path = runner.derived_config(config, label)
    with tempfile.TemporaryDirectory(prefix="cpi-constraint-request-") as directory:
        request = Path(directory) / "request.json"
        request.write_text(json.dumps({"method": method, "arguments": arguments}))
        request.chmod(0o600)
        result = subprocess.run([runner.verification.cli, "storage-plan", "--config", path, "--request", str(request), "--json"], capture_output=True, text=True, timeout=180, check=False)
    if result.returncode:
        # Invalid production configuration is checked separately by an actual CPI
        # request, whose fixed error class distinguishes it from auth/API failure.
        known = {
            "storage-plan: configuration or client unavailable": "configuration_or_client_unavailable",
            "storage-plan: diagnostic observation failed": "observation_failed",
            "storage-plan: request file unavailable": "request_unavailable",
        }
        failure = known.get(result.stderr.strip(), "diagnostic_failed")
        return {"diagnostic_exit": result.returncode, "failure_class": failure}, path
    try:
        report = json.loads(result.stdout)
    except ValueError as error:
        raise RuntimeError("production diagnostic returned invalid JSON") from error
    require(isinstance(report, dict) and report.get("observation_only") is True, "diagnostic is not explicitly read-only")
    return report, path


def _positive(runner, config, method, arguments, label):
    before = _snapshot(runner)
    report, _ = _diagnostic(runner, config, method, arguments, label)
    after = _snapshot(runner)
    require(before == after, "observation-only diagnostic mutated allocation or actual volume state")
    require(bool(report.get("targets")), "positive constraint scenario produced no feasible plan")
    return report


def _rejected(runner, config, method, arguments, label, reasons, diagnostic_code=None):
    before = _snapshot(runner)
    report, path = _diagnostic(runner, config, method, arguments, label)
    require(not report.get("targets"), "negative constraint produced a feasible plan")
    if diagnostic_code is not None:
        require("planning rejected: " + diagnostic_code in report.get("rejections", []), "negative constraint lacks its typed production diagnostic")
    from _storage_placement_scenarios import CPIRejected
    try:
        unexpected = runner.call_once(method, arguments, path)
    except CPIRejected as error:
        text = error.reason.lower()
        matches = [name for name, fragments in reasons.items() if all(fragment in text for fragment in fragments)]
        typed_bounded_failure = diagnostic_code is not None and "managed allocation failed; inspect" in text
        require(bool(matches) or typed_bounded_failure, "negative constraint failed for an unrelated error class")
        classification = diagnostic_code if typed_bounded_failure else matches[0]
    else:
        cid = unexpected if method == "create_disk" else unexpected[0] if isinstance(unexpected, list) and unexpected else None
        if isinstance(cid, str) and cid:
            runner.active_resources["constraint_unexpected"] = {"method": method, "cid": cid}
            runner.checkpoint()
            runner.call_once("delete_disk" if method == "create_disk" else "delete_vm", [cid], runner.config_path)
            runner.active_resources.pop("constraint_unexpected")
            runner.checkpoint()
        raise RuntimeError("negative constraint unexpectedly created a resource")
    after = _snapshot(runner)
    require(before == after, "rejected constraint changed allocation or actual volume state")
    return {"diagnostic": report, "rejection_class": classification, "before": before, "after": after}


def _disk(runner, properties=None, size=None):
    return [size or runner.policy["disk_size_mib"], properties or {}, ""]


def _member_ids(report, name, expected):
    actual = report.get("frozen_membership", {}).get(name)
    require(isinstance(actual, list) and set(actual) == set(expected), "production frozen membership differs from the exact expected IDs")
    return actual


def regex_membership(runner, _options):
    evidence = {}
    for role, members in (("persistent", runner.policy["persistent_storage_ids"]), ("ephemeral", runner.policy["ephemeral_storage_ids"])):
        require(len(members) >= 2, "regex membership scenario requires multiple real members")
        rounds = []
        for index, selected in enumerate((members[:1], members, members[-1:])):
            config = runner.configured_policy()
            name = config[role + "_storage_set"]
            config["storage_sets"][name].pop("names")
            config["storage_sets"][name]["name_pattern"] = "^(?:" + "|".join(re.escape(member) for member in selected) + ")$"
            method = "create_disk" if role == "persistent" else "create_vm"
            args = _disk(runner) if method == "create_disk" else runner.vm_arguments("regex-" + str(index))
            report = _positive(runner, config, method, args, "regex-" + role + str(index))
            _member_ids(report, name, selected)
            rounds.append(report)
        evidence[role] = rounds
    return evidence


def missing_storage(runner, options):
    missing = options["missing_storage_id"]
    require(missing not in _definitions(runner), "declared missing storage exists")
    config = runner.configured_policy(p_members=[missing])
    return _rejected(runner, config, "create_disk", _disk(runner), "missing-id", {"missing_storage": ("missing storage", missing.lower())}, diagnostic_code="missing_storage_ids")


def _nfs_backing(definition):
    require(definition.get("type") == "nfs" and definition.get("server") and definition.get("export"), "fixture must be an observed NFS definition")
    return definition["server"], definition["export"].rstrip("/") or "/"


def alias_storage(runner, options):
    members = options["alias_storage_ids"]
    definitions = _definitions(runner)
    require(all(member in definitions for member in members), "alias fixture contains an absent ID")
    require(len({_nfs_backing(definitions[member]) for member in members}) == 1, "alias fixture does not name the same actual NFS backing")
    config = runner.configured_policy(p_members=members)
    return _rejected(runner, config, "create_disk", _disk(runner), "aliases", {"backing_alias": ("backing aliases",)}, diagnostic_code="backing_aliases")


def disjoint_storage(runner, _options):
    shared = runner.policy["persistent_storage_ids"][0]
    config = runner.configured_policy(e_members=[shared])
    config["require_disjoint_storage_sets"] = True
    return _rejected(runner, config, "create_disk", _disk(runner), "disjoint", {"disjoint": ("overlap",), "disjoint_policy": ("disjoint",)}, diagnostic_code="overlapping_sets")


def _restricted_definition(runner, options):
    fixture = options["restricted_storage"]
    definition = _definitions(runner).get(fixture["storage_id"])
    require(definition is not None, "restricted fixture storage is absent")
    _nfs_backing(definition)
    nodes = definition.get("nodes", "")
    nodes = set(nodes.split(",")) if isinstance(nodes, str) else set(nodes)
    require(fixture["allowed_node"] in nodes and fixture["excluded_node"] not in nodes, "actual storage definition does not enforce declared node restriction")
    return fixture


def node_restriction(runner, options):
    fixture = _restricted_definition(runner, options)
    config = runner.configured_policy(p_members=[fixture["storage_id"]])
    positive = _positive(runner, config, "create_disk", _disk(runner, {"node": fixture["allowed_node"]}), "allowed-node")
    require(positive.get("node") == fixture["allowed_node"], "positive node restriction was not honored")
    rejected = _rejected(runner, config, "create_disk", _disk(runner, {"node": fixture["excluded_node"]}), "excluded-node", {"unavailable_target": ("storage placement capacity",), "unavailable_candidate": ("no eligible",), "no_feasible": ("no feasible",)})
    return {"definition": fixture, "allowed": positive, "excluded": rejected}


def ha_reachability(runner, options):
    fixture = _restricted_definition(runner, options)
    config = runner.configured_policy(e_members=[fixture["storage_id"]])
    placement = config.setdefault("placement", {})
    placement.update(az_map={"cert-ha": [fixture["allowed_node"]]}, pin_az_via_ha_rules=True, pin_az_strict=True)
    args = runner.vm_arguments("ha-constraint", {"availability_zone": "cert-ha"})
    positive = _positive(runner, config, "create_vm", args, "ha-single-node")
    placement["az_map"]["cert-ha"].append(fixture["excluded_node"])
    negative = _rejected(runner, config, "create_vm", args, "ha-unreachable-node", {"ha_reachability": ("ha", "storage"), "no_feasible": ("no feasible",), "storage_plan": ("storage placement capacity",)})
    return {"reachable": positive, "unreachable": negative}


def capacity_domain(runner, options):
    members = options["domain_storage_ids"]
    definitions = _definitions(runner)
    require(all(member in definitions for member in members), "capacity domain fixture contains an absent ID")
    require(len({_nfs_backing(definitions[member]) for member in members}) == len(members), "domain scenario requires distinct exports, not backing aliases")
    config = runner.configured_policy(p_members=members)
    config["storage_capacity_domains"] = {"cert-shared-budget": {"members": members}}
    report = _positive(runner, config, "create_disk", _disk(runner), "shared-domain")
    domains = report.get("domains", [])
    domain = next((item for item in domains if item.get("Name") == "cert-shared-budget"), None)
    require(domain is not None and not domain.get("Reason"), "production diagnostic omitted healthy shared domain")
    pairs = [item["Pair"] for item in report.get("capacities", []) if item["Pair"].get("StorageID") in members and not item["Pair"].get("Reason")]
    require(set(domain.get("Members", [])) == set(members) and pairs, "domain membership is incomplete")
    require(domain["TotalBytes"] == min(item["TotalBytes"] for item in pairs), "domain manufactured total capacity")
    require(domain["AvailableBytes"] == min(item["AvailableBytes"] for item in pairs), "domain manufactured available capacity")
    require(all(target.get("DomainKey") == "cert-shared-budget" for target in report["targets"] if target.get("Role") == "persistent"), "persistent charge escaped the declared domain")
    return report


def capacity_ceiling(runner, _options):
    config = runner.configured_policy()
    baseline = _positive(runner, config, "create_disk", _disk(runner), "ceiling-baseline")
    members = set(runner.policy["persistent_storage_ids"])
    pairs = [item["Pair"] for item in baseline.get("capacities", []) if item["Pair"].get("StorageID") in members and not item["Pair"].get("Reason")]
    require(pairs, "ceiling scenario requires actual member capacity")
    size_mib = max(pair["TotalBytes"] for pair in pairs) // (100 * MIB) + 1024
    require(size_mib < (1 << 43), "ceiling fixture size exceeds supported allocation range")
    config["storage_sets"]["cert-p"]["max_utilization_pct"] = 1
    rejected = _rejected(runner, config, "create_disk", _disk(runner, size=size_mib), "one-percent-ceiling", {"capacity": ("storage placement capacity",), "utilization": ("utilization",)})
    return {"baseline": baseline, "requested_mib": size_mib, "rejected": rejected}


def capacity_overflow(runner, _options):
    config = runner.configured_policy()
    request = _rejected(runner, config, "create_disk", _disk(runner, size=(1 << 63) - 1), "request-overflow", {"allocation_range": ("supported allocation range",), "size_range": ("size", "range")})
    config["storage_sets"]["cert-p"]["min_free_mb"] = (1 << 63) - 1
    reserve = _rejected(runner, config, "create_disk", _disk(runner), "reserve-overflow", {"reserve_overflow": ("min_free_mb",), "overflow": ("overflow",)})
    return {"request": request, "reserve": reserve}


def capacity_rounding(runner, _options):
    reports = []
    for size in (1, 1023, 1024, 1025):
        report = _positive(runner, runner.configured_policy(), "create_disk", _disk(runner, size=size), "rounding-" + str(size))
        targets = [target for target in report["targets"] if target.get("Role") == "persistent"]
        expected = ((size + 1023) // 1024) * GIB
        require(len(targets) == 1 and targets[0]["VirtualBytes"] == expected and targets[0]["ChargeBytes"] == expected, "persistent rounding understated virtual allocation or capacity charge")
        reports.append(report)
    return {"diagnostics": reports}


def unequal_members(runner, options):
    members = options["unequal_storage_ids"]
    report = _positive(runner, runner.configured_policy(p_members=members), "create_disk", _disk(runner), "unequal-members")
    pairs = [item["Pair"] for item in report.get("capacities", []) if item["Pair"].get("StorageID") in members and not item["Pair"].get("Reason")]
    require({pair["StorageID"] for pair in pairs} == set(members), "unequal fixture lacks complete live capacity observations")
    require(len({pair["TotalBytes"] for pair in pairs}) > 1, "declared unequal members report equal total capacities")
    for target in report["targets"]:
        if target.get("Role") == "persistent":
            candidates = [pair for pair in pairs if pair["Node"] == target["Node"] and pair["StorageID"] == target["StorageID"]]
            require(len(candidates) == 1 and target["ChargeBytes"] <= candidates[0]["AvailableBytes"], "selected target exceeds its own observed available bytes")
    return report


class DelayedInventoryProxy(LoopbackPVEFaultProxy):
    def __init__(self, config, stores, delay):
        super().__init__(config, stores, "response_lost_after_allocation")
        self.delay = delay
        self.delayed_reads = 0
        self.reads = []

    def forward(self, handler):
        if handler.command != "GET":
            with self.lock:
                self.evidence["blocked_mutations"] += 1
            handler.send_error(503)
            return
        inventory = re.fullmatch(r"/api2/json/nodes/[^/]+/storage", urlsplit(handler.path).path) is not None
        start = time.monotonic()
        delayed = inventory and self.delay > 0
        if delayed:
            with self.lock:
                self.delayed_reads += 1
            self.release.wait(self.delay)
        delay_elapsed = time.monotonic()-start
        original = handler.send_response
        original_end = handler.end_headers
        original_writer = handler.wfile
        status = None
        body_ready = False
        record = {"inventory": inventory, "delayed": delayed, "delay_seconds": delay_elapsed, "status": None, "inventory_payload_valid": False}
        stores = set(self.storage_ids)
        def end_headers():
            nonlocal body_ready
            original_end()
            body_ready = True
        class Writer:
            def __getattr__(self, name):
                return getattr(original_writer, name)
            def write(self, data):
                if body_ready and inventory:
                    record["inventory_payload_valid"] = _valid_capacity_payload(data, stores)
                return original_writer.write(data)
        handler.end_headers = end_headers
        handler.wfile = Writer()
        def capture(code, *args, **kwargs):
            nonlocal status
            status = code
            with self.lock:
                record["status"] = status
                self.reads.append(record)
            return original(code, *args, **kwargs)
        handler.send_response = capture
        try:
            super().forward(handler)
        finally:
            handler.send_response = original
            handler.end_headers = original_end
            handler.wfile = original_writer
            if status is None:
                with self.lock:
                    self.reads.append(record)


def _valid_capacity_payload(data, stores):
    try:
        if not isinstance(data, bytes) or len(data) > 8*1024*1024:
            return False
        rows = json.loads(data).get("data")
        if not isinstance(rows, list) or not all(isinstance(row, dict) for row in rows):
            return False
        selected = [row for row in rows if row.get("storage") in stores]
        return len(selected) == len(stores) and {row.get("storage") for row in selected} == stores and all(
            row.get("active") == 1 and row.get("enabled") == 1
            and type(row.get("total")) is int and type(row.get("avail")) is int
            and 0 <= row["avail"] <= row["total"] and row["total"] > 0 for row in selected)
    except (ValueError, AttributeError, TypeError):
        return False


def _stale_phase_proof(reads, max_age):
    require(reads and all(type(row.get("status")) is int and 200 <= row["status"] < 300 for row in reads), "stale scenario had a failed upstream read")
    inventory = [row for row in reads if row.get("inventory")]
    require(inventory and all(row.get("delayed") is True and row.get("delay_seconds", 0) > max_age and row.get("inventory_payload_valid") is True for row in inventory), "stale scenario did not age every inventory read past the configured limit")



def stale_observation(runner, options):
    config = runner.configured_policy()
    before = _snapshot(runner)
    evidence = {"evidence_kind": "controlled transport delay over real PVE reads; not backend exhaustion"}
    runner.constraint_failure_evidence = evidence
    delay = options.get("stale_delay_seconds", 2)
    with DelayedInventoryProxy(config, runner.policy["persistent_storage_ids"], 0) as proxy:
        config.update(proxy.config_override)
        # Establish that the same authenticated proxy, topology and request work
        # before changing only the observation lifetime and transport delay.
        control, _ = _diagnostic(runner, config, "create_disk", _disk(runner), "stale-control")
        evidence["control"] = control
        require(bool(control.get("targets")), "stale scenario lacks a feasible no-delay control")
        require(proxy.evidence["blocked_mutations"] == 0, "stale control attempted mutation")
        proxy.delay = delay
        config["storage_status_max_age_seconds"] = 1
        mark = len(proxy.reads)
        report, path = _diagnostic(runner, config, "create_disk", _disk(runner), "stale-observation")
        evidence["diagnostic"] = report
        evidence["diagnostic_reads"] = list(proxy.reads[mark:])
        require(report.get("observation_only") is True and not report.get("targets"), "stale diagnostic did not prove read-only rejection")
        require(report.get("rejections") == ["planning rejected: configuration or observation"], "stale diagnostic has an unrelated rejection class")
        _stale_phase_proof(evidence["diagnostic_reads"], 1)
        from _storage_placement_scenarios import CPIRejected
        mark = len(proxy.reads)
        try:
            unexpected = runner.call_once("create_disk", _disk(runner), path)
        except CPIRejected as error:
            evidence["create_reads"] = list(proxy.reads[mark:])
            _stale_phase_proof(evidence["create_reads"], 1)
            require(error.reason == "create_disk: managed allocation failed; inspect the allocation journal and PVE read permissions", "stale scenario failed for an unrelated CPI error")
        else:
            if isinstance(unexpected, str) and unexpected:
                evidence["unexpected_cid"] = unexpected
                runner.active_resources["constraint_unexpected"] = {"method": "create_disk", "cid": unexpected}
                runner.checkpoint()
            raise RuntimeError("stale observation unexpectedly allocated")
        evidence["proxy"] = dict(proxy.evidence)
        require(proxy.delayed_reads > 0 and proxy.evidence["blocked_mutations"] == 0, "stale case did not reject before its first mutation boundary")
        evidence["delayed_reads"] = proxy.delayed_reads
    after = _snapshot(runner)
    require(before == after, "stale rejection changed allocation or actual volume state")
    evidence.update(delay_seconds=delay, before=before, after=after)
    return evidence


CASES = (
    ("membership_regex_add_remove", None, regex_membership), ("missing_storage_ids", "missing_storage_id", missing_storage),
    ("backing_alias_rejection", "alias_storage_ids", alias_storage), ("disjoint_set_rejection", None, disjoint_storage),
    ("node_restriction", "restricted_storage", node_restriction), ("ha_reachability", "restricted_storage", ha_reachability),
    ("shared_capacity_domain", "domain_storage_ids", capacity_domain), ("capacity_ceiling", None, capacity_ceiling),
    ("capacity_overflow", None, capacity_overflow), ("capacity_rounding", None, capacity_rounding),
    ("unequal_capacity_members", "unequal_storage_ids", unequal_members), ("stale_capacity_observation", None, stale_observation),
    ("post_root_acquired_capacity", None, post_root_transition),
    ("post_root_insufficient_capacity", None, post_root_capacity_rejection),
)


def run_constraint_scenarios(manifest, runner):
    options = validate_constraints_manifest(manifest, runner.base_config)
    rows = []
    try:
        include_constraint_inventory(runner, options)
    except (RuntimeError, ValueError, KeyError, TypeError, OSError):
        return [{"scenario_id": identity, "status": "failed" if index == 0 else "missing", "evidence": {"reason": "complete constraint fixture inventory could not be established"}} for index, identity in enumerate(CONSTRAINT_IDS)]
    for index, (identity, fixture, execute) in enumerate(CASES):
        if fixture and fixture not in options:
            rows.append({"scenario_id": identity, "status": "missing", "evidence": {"reason": "declared topology fixture is absent", "required_field": fixture}})
            continue
        runner.constraint_failure_evidence = None
        try:
            evidence = execute(runner, options)
            rows.append({"scenario_id": identity, "status": "passed", "evidence": evidence})
        except (RuntimeError, ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
            rows.append({"scenario_id": identity, "status": "failed", "evidence": {"reason": "production constraint assertion failed; no certification claimed", **({"controlled_observation": runner.constraint_failure_evidence} if runner.constraint_failure_evidence is not None else {})}})
            rows.extend({"scenario_id": next_identity, "status": "missing", "evidence": {"reason": "prior constraint scenario failed", "blocked_by": identity}} for next_identity, _, _ in CASES[index + 1:])
            break
    return rows
