"""Observe real root acquisition while controlling subsequent capacity readings."""
from __future__ import annotations

import copy
import hashlib
import json
import re
import threading
from collections import Counter
from urllib.parse import quote
from pathlib import Path

ROOT_KINDS = {"vm.QEMU.Create", "vm.Nodes.CreateQemuClone"}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def active_plan(record):
    return (record.get("attempts") or [{"plan": record["intent"]}])[-1]["plan"]["plan"]


def transition_ledger(record):
    """Compare immutable planned bytes with durable acquired evidence by backing."""
    plan = active_plan(record)
    targets = {item["Role"]: item for item in plan["Targets"]}
    require("root" in targets and "ephemeral" in targets, "transition requires separate root and ephemeral roles")
    root, ephemeral = targets["root"], targets["ephemeral"]
    require(root["StorageID"] == ephemeral["StorageID"] and root["CapacityKey"] == ephemeral["CapacityKey"], "transition requires one actual bundled backing")
    backing = root["CapacityKey"]
    planned = sum(item["Charge"]["Bytes"] for item in plan["Charges"] if item["CapacityKey"] == backing)
    attempt = max(0, len(record.get("attempts", [])) - 1)
    steps = [step for step in record.get("steps", []) if step.get("attempt", 0) == attempt]
    roots = [step for step in steps if step.get("kind") in ROOT_KINDS and step.get("state") == "observed"]
    require(len(roots) == 1 and roots[0].get("upid") and roots[0].get("volids"), "root has no durable task and volume acquisition proof")
    planned_charges = Counter((item["Charge"]["Role"], item["Charge"]["Bytes"]) for item in plan["Charges"] if item["CapacityKey"] == backing)
    acquired = 0
    for step in steps:
        for charge in step.get("charges", []):
            if charge["backing"] != backing:
                continue
            role = "root" if step.get("kind") in ROOT_KINDS else {"vm.Storage.CreateVolume":"ephemeral","vm.Storage.Upload":"iso"}.get(step.get("kind"))
            key = (role, charge["planned_bytes"])
            require(role is not None and planned_charges[key] > 0, "journal charge differs from the immutable role budget")
            planned_charges[key] -= 1
            values = [charge[key] for key in ("planned_bytes", "acquired_bytes", "outstanding_bytes")]
            require(all(type(value) is int and value >= 0 for value in values) and values[0] == sum(values[1:]), "journal charge partition is malformed")
            if charge["acquired_bytes"]:
                require(step.get("state") == "observed" and charge["outstanding_bytes"] == 0, "acquisition is not observed and fully reflected")
            acquired += charge["acquired_bytes"]
    require(0 < acquired <= planned, "root acquisition is absent or exceeds its frozen backing plan")
    return {"root": root, "root_step": roots[0], "planned_bytes": planned, "acquired_bytes": acquired,
            "remaining_bytes": planned - acquired, "steps": steps}


class PostRootCapacityHook:
    """Cap real available bytes only after observed root task and content proof."""
    def __init__(self, runner, reject=False):
        self.runner, self.reject = runner, reject
        self.evidence = None
        self.reads = 0
        self.allocation_id = None
        self.lock = threading.Lock()

    def __call__(self, method, path, payload, proxy):
        with self.lock:
            return self.observe(method, path, payload, proxy)

    def observe(self, method, path, payload, proxy):
        if method != "GET" or not re.fullmatch(r"/nodes/[^/]+/storage", path):
            return payload
        try:
            record = proxy.current_record()
        except ValueError:
            if self.evidence is not None:
                raise
            return payload
        if not any(step.get("kind") in ROOT_KINDS and step.get("state") == "observed" for step in record.get("steps", [])):
            return payload
        ledger = transition_ledger(record)
        root, root_step = ledger["root"], ledger["root_step"]
        if self.evidence is None:
            require(not any(step.get("kind") == "vm.Storage.CreateVolume" for step in ledger["steps"]), "capacity barrier was reached after ephemeral submission")
            require(ledger["remaining_bytes"] > 0, "capacity barrier lacks an outstanding allocation")
            node = quote(root["Node"], safe="")
            upid = root_step["upid"]
            require(upid.startswith("UPID:") and len(upid.split(":")) > 2, "root task identity is malformed")
            task = self.runner.verifier._get("/nodes/" + quote(upid.split(":")[1], safe="") + "/tasks/" + quote(upid, safe="") + "/status")
            require(isinstance(task, dict) and task.get("status") == "stopped" and task.get("exitstatus") == "OK", "actual root task has not completed")
            rows = self.runner.verifier._get("/nodes/" + node + "/storage/" + quote(root["StorageID"], safe="") + "/content")
            require(isinstance(rows, list), "actual root content observation is malformed")
            for volume in root_step["volids"]:
                matches = [item for item in rows if item.get("volid") == volume]
                require(len(matches) == 1 and type(matches[0].get("size")) is int and matches[0]["size"] > 0, "actual root volume is missing or ambiguous")
            self.allocation_id = record["id"]
            self.evidence = {"allocation_id": record["id"], "root_step_id": root_step["id"], "upid": upid,
                             "root_volumes": root_step["volids"], "planned_bytes": ledger["planned_bytes"],
                             "acquired_bytes": ledger["acquired_bytes"], "remaining_bytes": ledger["remaining_bytes"],
                             "record_observation_sha256": hashlib.sha256(json.dumps(record, sort_keys=True, separators=(",", ":")).encode()).hexdigest()}
        require(record["id"] == self.allocation_id, "capacity transition changed allocation UUID")
        envelope = json.loads(payload)
        require(isinstance(envelope, dict) and isinstance(envelope.get("data"), list), "actual storage response is malformed")
        rows = [row for row in envelope["data"] if row.get("storage") == root["StorageID"]]
        # Other nodes may legitimately lack this member; their unchanged response
        # still participates in normal production topology validation.
        if not rows:
            return payload
        require(len(rows) == 1, "capacity response contains ambiguous storage identity")
        row = rows[0]
        available, total = row.get("avail"), row.get("total")
        require(type(available) is int and type(total) is int and 0 <= available <= total, "actual capacity fields are malformed")
        cap = max(0, ledger["remaining_bytes"] - int(self.reject))
        require(available >= ledger["remaining_bytes"], "actual backend cannot support the controlled capacity experiment")
        row["avail"] = cap
        row["used"] = total - cap
        observations = self.evidence.setdefault("capacity_responses", [])
        require(len(observations) < 1000, "capacity experiment exceeded its bounded observation log")
        observations.append({"node":path.split("/")[2],"storage":root["StorageID"],"total_bytes":total,
                             "original_available_bytes":available,"controlled_available_bytes":cap,
                             "remaining_bytes":ledger["remaining_bytes"]})
        self.reads += 1
        return json.dumps(envelope, separators=(",", ":")).encode()


def transition_arguments(runner, identifier):
    args = runner.vm_arguments(identifier)
    for key in ("root_storage_set","root_storage_pool","root_storage_tier","storage_pool","storage_tier", "ephemeral_storage_set","ephemeral_storage_pool","ephemeral_storage_tier"):
        args[2].pop(key,None)
    return args


def post_root_transition(runner, _options):
    """Create a real VM with headroom below root plus remaining allocation bytes."""
    from _storage_placement_vm_faults import JournalVMFaultProxy
    from _storage_placement_scenarios import verified_record_payload
    config = runner.configured_policy(e_members=runner.policy["ephemeral_storage_ids"][:1])
    config.pop("root_storage_set", None)
    config.pop("storage_capacity_domains", None)
    config.setdefault("placement", {})["reserve_storage_headroom"] = False
    config["placement"]["fallback_max"] = 0
    config.setdefault("storage", {})["max_utilization_pct"] = 100
    config["storage"]["max_utilization_mode"] = "enforce"
    before_volumes = runner.verification.volume_inventory(runner.verifier)
    args = transition_arguments(runner,"post-root-capacity")
    hook = PostRootCapacityHook(runner)
    runner.active_resources["capacity_transition"] = {"agent_id": args[0], "phase": "creating"}
    runner.checkpoint()
    with JournalVMFaultProxy(config, args[0], response_hook=hook) as proxy:
        mediated = copy.deepcopy(config)
        mediated.update(proxy.config_override)
        path = runner.derived_config(mediated, "post-root-capacity")
        result = runner.call_once("create_vm", args, path)
        require(isinstance(result, list) and result and isinstance(result[0], str) and result[0], "capacity transition lacks a returned VM CID")
        cid = result[0]
        runner.active_resources["capacity_transition"].update({"vm_cid": cid, "phase": "verifying"})
        runner.checkpoint()
        require(hook.evidence is not None and hook.reads > 0, "actual runtime did not cross the post-root capacity barrier")
        require(proxy.evidence.get("blocked_mutations", 0) == 0, "capacity experiment encountered an unsupported or unowned mutation")
    audit = runner.audit()
    summary = runner.record_for(audit, cid, "vm")
    namespace = hashlib.sha256(runner.base_config["storage_placement_namespace"].encode()).hexdigest()
    raw = (Path(runner.base_config["storage_allocation_journal_dir"]) / namespace / ("allocation-" + summary["ID"] + ".json")).read_bytes()
    record = verified_record_payload(raw, summary)
    final = transition_ledger(record)
    require(record["id"] == hook.allocation_id and record["state"] == "ready_to_return", "final allocation did not preserve the observed root identity")
    require(final["remaining_bytes"] == 0, "ready allocation retained unacquired frozen bytes")
    actual = runner.observed_vm(cid, config, True, runner.observed_root_mechanism(summary, config))
    runner.call_once("delete_vm", [cid], runner.config_path)
    after = runner.audit()
    tombstone = runner.record_for(after, cid, "vm", hook.allocation_id)
    require(tombstone["State"] == "deleted", "capacity experiment cleanup lacks a verified tombstone")
    require(runner.verification.volume_inventory(runner.verifier) == before_volumes, "capacity experiment leaked or changed an unrelated physical volume")
    runner.active_resources.pop("capacity_transition")
    runner.checkpoint()
    return {"evidence_kind": "controlled capacity response over real PVE operations; not backend exhaustion",
            "barrier": hook.evidence, "modified_status_reads": hook.reads, "final_acquired_bytes": final["acquired_bytes"],
            "vm": actual, "cleanup_state": tombstone["State"]}


def post_root_capacity_rejection(runner, _options):
    """Reject before E submission, then explicitly clean the verified owned root."""
    from _storage_placement_vm_faults import JournalVMFaultProxy
    from _storage_placement_scenarios import CPIRejected, verified_record_payload
    config = runner.configured_policy(e_members=runner.policy["ephemeral_storage_ids"][:1])
    config.pop("root_storage_set", None)
    config.pop("storage_capacity_domains", None)
    config.setdefault("placement", {}).update(reserve_storage_headroom=False, fallback_max=0)
    config.setdefault("storage", {}).update(max_utilization_pct=100, max_utilization_mode="enforce")
    args = transition_arguments(runner,"post-root-insufficient")
    before_volumes = runner.verification.volume_inventory(runner.verifier)
    hook = PostRootCapacityHook(runner, reject=True)
    runner.active_resources["capacity_transition"] = {"agent_id": args[0], "phase": "creating-negative"}
    runner.checkpoint()
    with JournalVMFaultProxy(config, args[0], response_hook=hook) as proxy:
        mediated = copy.deepcopy(config)
        mediated.update(proxy.config_override)
        path = runner.derived_config(mediated, "post-root-insufficient")
        try:
            result = runner.call_once("create_vm", args, path)
        except CPIRejected as error:
            expected = {"allocation " + str(hook.allocation_id) + " requires reconciliation at " + phase + "; no alternate allocation was attempted"
                        for phase in ("VM root creation","VM post-create")}
            runner.constraint_failure_evidence = {
                "evidence_kind": "controlled capacity response over real PVE operations; not backend exhaustion",
                "phase": "rejection-validation", "barrier": copy.deepcopy(hook.evidence),
                "modified_status_reads": hook.reads,
                "rejection_matches_expected": error.reason in expected,
                "rejection": error.reason if error.reason in expected else "unrelated CPI rejection"}
            require(hook.evidence is not None and error.reason in expected,
                    "post-root rejection was unrelated to the exact allocation phase")
        else:
            if isinstance(result, list) and result and isinstance(result[0], str):
                runner.active_resources["capacity_transition"]["vm_cid"] = result[0]
                runner.checkpoint()
            raise RuntimeError("insufficient post-root capacity unexpectedly produced a VM")
        require(hook.evidence is not None and hook.reads > 0, "rejection did not reach the actual acquired-root barrier")
        require(proxy.evidence.get("blocked_mutations", 0) == 0, "rejection was caused by the proxy mutation fence")
        writes = proxy.evidence["mutations"]
        runner.constraint_failure_evidence.update(
            root_submissions=sum(item["kind"] in ROOT_KINDS for item in writes),
            later_allocation_submissions=sum(item["kind"] in {"vm.Storage.CreateVolume", "vm.Storage.Upload"} for item in writes),
            blocked_mutations=proxy.evidence.get("blocked_mutations", 0))
        require(sum(item["kind"] in ROOT_KINDS for item in writes) == 1, "negative capacity case submitted multiple roots")
        require(not any(item["kind"] in {"vm.Storage.CreateVolume", "vm.Storage.Upload"} for item in writes), "insufficient capacity submitted a later allocation")
    runner.active_resources["capacity_transition"].update(allocation_id=hook.allocation_id, phase="explicit-cleanup")
    runner.constraint_failure_evidence["phase"] = "record-readback"
    runner.checkpoint()
    audit = runner.audit()
    summaries = [item for item in audit["records"] if item["ID"] == hook.allocation_id]
    require(len(summaries) == 1, "failed root lacks its unique durable allocation")
    namespace = hashlib.sha256(runner.base_config["storage_placement_namespace"].encode()).hexdigest()
    raw = (Path(runner.base_config["storage_allocation_journal_dir"]) / namespace / ("allocation-" + hook.allocation_id + ".json")).read_bytes()
    record = verified_record_payload(raw, summaries[0])
    ledger = transition_ledger(record)
    require(ledger["acquired_bytes"] == hook.evidence["acquired_bytes"] and ledger["remaining_bytes"] == hook.evidence["remaining_bytes"], "failure erased or changed acquired/outstanding evidence")
    require(not any(step.get("state") in {"planned", "submitted", "unknown"} for step in ledger["steps"]), "failed root has an unresolved mutation and cannot be automatically cleaned")
    runner.constraint_failure_evidence["phase"] = "explicit-cleanup"
    cleanup = runner.verification._json_command([runner.cpi_bin, "storage-journal", "cleanup", "--config", runner.config_path,
                                                "--allocation-id", hook.allocation_id, "--decision-id", "cert-post-root-" + hook.allocation_id])
    require(cleanup.get("allocation_id") == hook.allocation_id and cleanup.get("state") == "cleaned", "explicit owned-root cleanup did not complete")
    final = runner.audit()
    require(any(item["ID"] == hook.allocation_id and item["State"] == "cleaned" for item in final["records"]), "cleanup lacks an audited tombstone")
    require(runner.verification.volume_inventory(runner.verifier) == before_volumes, "negative capacity scenario leaked or changed unrelated physical volumes")
    runner.active_resources.pop("capacity_transition")
    runner.checkpoint()
    return {"evidence_kind": "controlled capacity response over real PVE operations; not backend exhaustion", "barrier": hook.evidence,
            "modified_status_reads": hook.reads, "root_submissions": 1, "later_allocation_submissions": 0,
            "acquired_bytes_retained_on_failure": ledger["acquired_bytes"], "cleanup_state": "cleaned"}
