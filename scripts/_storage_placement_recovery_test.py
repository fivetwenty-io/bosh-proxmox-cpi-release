"""Offline tests for recovery isolation, evidence, and command sequencing."""
from __future__ import annotations

import copy
import json
import os
import subprocess
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

import _storage_placement_recovery as recovery


class FakeVerifier:
    def __init__(self, config):
        self.config = config


class FakeScenario(recovery.RecoveryScenario):
    instances = []
    unexpected_success = False

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.deleted = []
        self.disk = {"ID": "11111111-1111-4111-8111-111111111111", "Kind": "disk", "CID": "a:owned.raw", "State": "ready_to_return"}
        self.instances.append(self)

    def initialize(self):
        self.namespace_path.mkdir(mode=0o700)
        (self.namespace_path / "authority.json").write_text("original authority")

    def create_disk(self, **kwargs):
        (self.namespace_path / ("allocation-" + self.disk["ID"] + ".json")).write_text(json.dumps(self.disk))
        return self.disk["CID"]

    def verify_disk(self, cid):
        self.evidence.setdefault("volumes", []).append({"cid": cid, "volid": cid})
        return cid

    def record(self, cid=None):
        return copy.deepcopy(self.disk)

    def call(self, method, args, **kwargs):
        if self.unexpected_success:
            return {"result": "a:unexpected.raw", "error": None}
        return {"result": None, "error": {"type": "CloudError", "message": "secret must not enter evidence"}}

    def delete_disk(self, cid, **kwargs):
        restored = self.namespace_path / ("allocation-" + self.disk["ID"] + ".json")
        if json.loads(restored.read_text()) != self.disk:
            raise AssertionError("cleanup did not use restored exact authority")
        self.deleted.append(cid)

    def audit(self):
        return {"records": [], "audit": {"complete": True}}


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.workspace = self.root / "scenarios"
        self.workspace.mkdir(mode=0o700)
        self.operator = self.root / "operator"
        self.operator.mkdir(mode=0o700)
        self.operator_file = self.operator / "authority.json"
        self.operator_file.write_text("operator authority must remain unchanged")
        config = {"host": "pve.invalid", "storage_allocation_journal_dir": str(self.operator),
                  "storage_placement_namespace": "operator", "storage_sets": {"p": {"names": ["a"]}},
                  "persistent_storage_set": "p"}
        self.runner = types.SimpleNamespace(base_config=config, cpi_bin="/candidate/cpi",
                                            configured_policy=lambda: copy.deepcopy(config))
        self.fixture = {"workspace": str(self.workspace), "cases": ["journal_corrupt"], "disk_size_mb": 16}
        FakeScenario.instances = []
        FakeScenario.unexpected_success = False

    def test_failed_initial_audit_retains_bounded_conflict_without_injection(self):
        original = recovery.RecoveryScenario
        allocation = "bff81208-c672-460b-b3cb-161bb82e1f6e"
        observed = {"records": [], "generation_index_healthy": True, "cluster_continuity": True,
                    "audit": {"complete": False, "vm_scan_complete": True, "issues": None,
                              "conflicts": ["remote allocation " + allocation + " is outside recorded mutation targets", "password=secret"]}}
        class AuditRefusal(FakeScenario):
            audit = original.audit
            record = original.record
            def journal_call(self, *args, **kwargs):
                return {"exit_code": 0, "output": observed}
        with patch.object(recovery, "RecoveryScenario", AuditRefusal), patch.object(recovery, "PVEVerifier", FakeVerifier), patch.object(recovery, "_inventory", side_effect=AssertionError("fault preparation must not run")):
            row = recovery.run_recovery_cases(self.runner, self.fixture)[0]
        self.assertEqual(row["status"], "failed")
        self.assertEqual(row["evidence"]["phase"], "initial allocation audit")
        checkpoint = row["evidence"]["audit_checkpoint"]
        self.assertFalse(checkpoint["complete"])
        self.assertEqual(checkpoint["conflict_count"], 2)
        self.assertEqual(checkpoint["conflict_categories"], [{"kind": "outside_recorded_mutation_targets", "allocation_id": allocation}, {"kind": "unclassified_conflict"}])
        self.assertNotIn("secret", json.dumps(row))
        scenario = FakeScenario.instances[-1]
        self.assertFalse((scenario.directory / "before-allocation").exists())
        self.assertEqual(scenario.deleted, [])

    def test_audit_rejects_malformed_issue_and_conflict_inventory(self):
        with patch.object(recovery, "PVEVerifier", FakeVerifier):
            scenario = recovery.RecoveryScenario(self.runner, self.fixture, "journal_corrupt")
        for field in ["issues", "conflicts"]:
            for bad in [{}, "", False, [None], [{}]]:
                output = {"generation_index_healthy": True, "cluster_continuity": True,
                          "audit": {"complete": True, "vm_scan_complete": True, "issues": None, "conflicts": None}}
                output["audit"][field] = bad
                with self.subTest(field=field,bad=bad), patch.object(scenario, "journal_call", return_value={"output": output}):
                    with self.assertRaisesRegex(RuntimeError, "malformed"):
                        scenario.audit()

    def test_manifest_validation_never_touches_files_or_network(self):
        with patch.object(recovery, "PVEVerifier", side_effect=AssertionError("network")), patch.object(recovery.Path, "stat", side_effect=AssertionError("filesystem")):
            recovery.validate_recovery_manifest(self.fixture)
        for changes in ({"cases": ["not-a-case"]}, {"cases": ["journal_corrupt"] * 2},
                        {"disk_size_mb": True}, {"disk_size_mb": 0}, {"workspace": "relative"},
                        {"shell": "arbitrary command"}, {"cases": ["context_isolation"], "contexts": ["/same", "/same"]}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                recovery.validate_recovery_manifest({**self.fixture, **changes})

    def test_workspace_cannot_overlap_or_redirect_operator_authority(self):
        with self.assertRaises(ValueError):
            recovery._private_workspace(str(self.operator), str(self.operator))
        with self.assertRaises(ValueError):
            recovery._private_workspace(str(self.root), str(self.operator))
        link = self.root / "alias"
        link.symlink_to(self.workspace, target_is_directory=True)
        with self.assertRaises(ValueError):
            recovery._private_workspace(str(link), str(self.operator))
        self.workspace.chmod(0o755)
        with self.assertRaises(ValueError):
            recovery._private_workspace(str(self.workspace), str(self.operator))

    def test_corrupt_missing_stale_restore_only_their_own_authority(self):
        fixture = {**self.fixture, "cases": ["journal_corrupt", "journal_missing", "journal_stale"]}
        with patch.object(recovery, "PVEVerifier", FakeVerifier), patch.object(recovery, "RecoveryScenario", FakeScenario), patch.object(recovery, "_inventory", return_value={"volumes": ["original"]}):
            rows = recovery.run_recovery_cases(self.runner, fixture)
        self.assertEqual([row["status"] for row in rows[:3]], ["passed"] * 3)
        self.assertEqual([row["status"] for row in rows[3:]], ["missing"] * 3)
        self.assertEqual(self.operator_file.read_text(), "operator authority must remain unchanged")
        self.assertEqual(len({item.namespace for item in FakeScenario.instances}), 3)
        for scenario in FakeScenario.instances:
            self.assertEqual(scenario.deleted, ["a:owned.raw"])
            self.assertTrue(scenario.directory.is_relative_to(self.workspace))
            self.assertNotEqual(scenario.namespace, "operator")
            self.assertEqual(os.stat(scenario.config_path).st_mode & 0o777, 0o600)
        self.assertNotIn("secret", json.dumps(rows))

    def test_unexpected_success_never_overwrites_new_authority_with_backup(self):
        FakeScenario.unexpected_success = True
        with patch.object(recovery, "PVEVerifier", FakeVerifier), patch.object(recovery, "RecoveryScenario", FakeScenario), patch.object(recovery, "_inventory", return_value={}):
            rows = recovery.run_recovery_cases(self.runner, self.fixture)
        self.assertEqual(rows[0]["status"], "failed")
        scenario = FakeScenario.instances[0]
        record = scenario.namespace_path / ("allocation-" + scenario.disk["ID"] + ".json")
        self.assertEqual(record.read_text(), "{invalid journal")
        self.assertFalse(scenario.deleted)
        self.assertTrue(rows[0]["evidence"]["resources_may_remain"])
        self.assertIn("scenario_directory", rows[0]["evidence"])

    def test_inventory_change_blocks_restore_and_cleanup(self):
        with patch.object(recovery, "PVEVerifier", FakeVerifier), patch.object(recovery, "RecoveryScenario", FakeScenario), patch.object(recovery, "_inventory", side_effect=[{"volumes": []}, {"volumes": ["unexpected"]}]):
            rows = recovery.run_recovery_cases(self.runner, self.fixture)
        self.assertEqual(rows[0]["status"], "failed")
        self.assertFalse(FakeScenario.instances[0].deleted)

    def test_absent_fixture_has_no_side_effect_and_no_passes(self):
        with patch.object(recovery, "RecoveryScenario", side_effect=AssertionError("unexpected setup")):
            rows = recovery.run_recovery_cases(self.runner, None)
        self.assertEqual(len(rows), len(recovery.RECOVERY_CASES))
        self.assertEqual({row["status"] for row in rows}, {"missing"})

    def test_discarded_cid_uses_actual_null_output_sink(self):
        with patch.object(recovery, "PVEVerifier", FakeVerifier):
            scenario = recovery.RecoveryScenario(self.runner, self.fixture, "unreturned_cid")
        with patch.object(recovery.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, None, "")) as run:
            scenario.call("create_disk", [16, {}, ""], discard=True)
        kwargs = run.call_args.kwargs
        self.assertEqual(kwargs["stdout"], subprocess.DEVNULL)
        request = json.loads(kwargs["input"])
        self.assertEqual(request["method"], "create_disk")
        self.assertEqual(request["api_version"], 2)
        self.assertEqual(run.call_args.args[0], ["/candidate/cpi", "--config", str(scenario.config_path)])

    def test_journal_command_errors_are_bounded(self):
        with patch.object(recovery, "PVEVerifier", FakeVerifier):
            scenario = recovery.RecoveryScenario(self.runner, self.fixture, "unreturned_cid")
        with patch.object(recovery.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "{}", "password=secret")):
            with self.assertRaisesRegex(RuntimeError, "required proof") as caught:
                scenario.journal_call("audit")
        self.assertNotIn("secret", str(caught.exception))

    def test_unreturned_disk_is_discovered_before_adoption_and_cleanup(self):
        with patch.object(recovery, "PVEVerifier", FakeVerifier):
            scenario = recovery.RecoveryScenario(self.runner, self.fixture, "unreturned_cid")
        events = []
        record = {"ID": "allocation", "CID": "a:exact.raw"}
        with patch.object(scenario, "initialize", side_effect=lambda: events.append("initialize")), patch.object(scenario, "call", side_effect=lambda *a, **kw: events.append(("discard", kw["discard"]))), patch.object(scenario, "record", side_effect=lambda: (events.append("audit-discovery") or record)), patch.object(scenario, "verify_disk", side_effect=lambda cid: events.append(("verify", cid))), patch.object(scenario, "journal_call", side_effect=lambda *args: events.append(args)), patch.object(scenario, "delete_disk", side_effect=lambda cid: events.append(("delete", cid))), patch.object(scenario, "audit", return_value={"complete": True}):
            scenario.run_unreturned()
        self.assertEqual(events[:3], ["initialize", ("discard", True), "audit-discovery"])
        self.assertEqual(events[3], ("verify", "a:exact.raw"))
        self.assertEqual(events[4][:5], ("adopt", "--allocation-id", "allocation", "--expected-cid", "a:exact.raw"))
        self.assertEqual(events[5], ("delete", "a:exact.raw"))

    def test_inventory_scopes_node_restrictions_status_and_content(self):
        definitions = [
            {"storage": "local", "nodes": "n1", "content": "images"},
            {"storage": "shared", "content": "images,iso"},
            {"storage": "off", "disable": 1, "content": "images"},
            {"storage": "backup", "content": "backup"},
            {"storage": "inactive", "content": "images"},
        ]
        calls = []
        def get(path):
            calls.append(path)
            if path == "/storage": return definitions
            if path == "/nodes": return [{"node": "n1"}, {"node": "n2"}]
            if path.endswith("/storage"):
                return [{"storage": "local", "active": 1}, {"storage": "shared", "active": 1}, {"storage": "inactive", "active": 0}]
            if path == "/cluster/resources?type=vm": return []
            if "/off/" in path or "/backup/" in path or "/inactive/" in path or path == "/nodes/n2/storage/local/content":
                raise AssertionError("queried an inapplicable location")
            return [{"volid": "shared:disk.raw", "size": 1024}]
        inventory = recovery._inventory(types.SimpleNamespace(_get=get))
        self.assertEqual(inventory["locations"], [("n1", "local"), ("n1", "shared"), ("n2", "shared")])
        self.assertEqual(len(inventory["volumes"]), 3)

    def test_inventory_never_swallows_applicable_failure_or_missing_status(self):
        for missing in (False, True):
            with self.subTest(missing=missing):
                def get(path):
                    if path == "/storage": return [{"storage": "a", "content": "images"}]
                    if path == "/nodes": return [{"node": "n1"}]
                    if path == "/nodes/n1/storage": return [] if missing else [{"storage": "a", "active": 1}]
                    raise RuntimeError("applicable content read failed")
                with self.assertRaises(RuntimeError):
                    recovery._inventory(types.SimpleNamespace(_get=get))

    def test_inventory_flags_reject_float_and_unknown_values(self):
        for value in (1.0, 0.0, "yes", None, [], {}):
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                recovery._flag(value, "active")

    def test_contexts_share_only_scenario_directory_and_route_independently(self):
        paths = []
        for host in ("cluster-a.invalid", "cluster-b.invalid"):
            path = self.root / (host + ".json")
            config = copy.deepcopy(self.runner.base_config)
            config["host"] = host
            config["ephemeral_storage_set"] = "p"
            path.write_text(json.dumps(config))
            paths.append(str(path))
        calls = []
        class ContextScenario(FakeScenario):
            def initialize(inner):
                super().initialize()
                inner.evidence["cluster_id"] = inner.config["host"]
                inner.disk["ID"] = inner.namespace
                inner.disk["CID"] = "a:" + inner.namespace + ".raw"
                inner.present = True
                calls.append(("initialize", inner.namespace, str(inner.journal)))
            def create_disk(inner, **kwargs):
                calls.append(("create", kwargs["context"]["pve_host"], str(kwargs["config_path"])))
                self.assertEqual(kwargs["context"]["pve_storage_placement_namespace"], inner.namespace)
                self.assertNotIn("pve_root_storage_set", kwargs["context"])
                self.assertEqual(kwargs["context"]["pve_ephemeral_storage_set"], "p")
                self.assertEqual(kwargs["context"]["pve_persistent_storage_set"], "p")
                return inner.disk["CID"]
            def audit(inner):
                return {"records": [inner.disk] if inner.present else []}
            def delete_disk(inner, cid, **kwargs):
                inner.present = False
        fixture = {**self.fixture, "cases": ["context_isolation"], "contexts": paths}
        with patch.object(recovery, "PVEVerifier", FakeVerifier), patch.object(recovery, "RecoveryScenario", ContextScenario):
            rows = recovery.run_recovery_cases(self.runner, fixture)
        row = next(row for row in rows if row["scenario_id"] == "context_isolation")
        self.assertEqual(row["status"], "passed", row)
        initialized = [call for call in calls if call[0] == "initialize"]
        self.assertEqual(len(initialized), 2)
        self.assertEqual(initialized[0][2], initialized[1][2])
        self.assertNotEqual(initialized[0][1], initialized[1][1])
        creates = [call for call in calls if call[0] == "create"]
        self.assertEqual({call[1] for call in creates}, {"cluster-a.invalid", "cluster-b.invalid"})
        self.assertEqual(creates[0][2], creates[1][2])
        self.assertFalse(row["evidence"]["same_process_cache_isolation"])

    def test_context_missing_selector_cannot_inherit_other_context_selection(self):
        paths = []
        for index in range(2):
            config = copy.deepcopy(self.runner.base_config)
            config["host"] = "cluster-" + str(index)
            if index == 0:
                config["root_storage_set"] = "p"
            path = self.root / ("context-" + str(index) + ".json")
            path.write_text(json.dumps(config))
            paths.append(str(path))
        fixture = {**self.fixture, "cases": ["context_isolation"], "contexts": paths}
        with patch.object(recovery, "RecoveryScenario", side_effect=AssertionError("enrollment must not start")):
            rows = recovery.run_recovery_cases(self.runner, fixture)
        row = next(row for row in rows if row["scenario_id"] == "context_isolation")
        self.assertEqual(row["status"], "failed")
        self.assertEqual(row["evidence"]["phase"], "context fixture validation")

    def test_context_refusal_detail_is_bounded_and_retained(self):
        with patch.object(recovery, "PVEVerifier", FakeVerifier):
            scenario = recovery.RecoveryScenario(self.runner, self.fixture, "context_isolation")
        for message, expected in [
            ('cpi: context override rejected: config: context override "pve_root_storage_set": root_storage_set must not be blank', "blank_storage_selector_override"),
            ('password=secret https://user:secret@private', "cpi_operation_refused"),
        ]:
            with patch.object(scenario, "call", return_value={"result": None, "error": {"message": message}}):
                with self.assertRaisesRegex(RuntimeError, "operation was refused"):
                    scenario.success("create_disk", [1, {}, ""])
            self.assertEqual(scenario.evidence["cpi_refusal"]["kind"], expected)
            self.assertNotIn("secret", json.dumps(scenario.evidence))

    def test_actual_size_and_terminal_disposition_are_required(self):
        with patch.object(recovery, "PVEVerifier", FakeVerifier):
            scenario = recovery.RecoveryScenario(self.runner, self.fixture, "mixed_legacy_managed")
        scenario.verifier = types.SimpleNamespace(current_disk_volid=lambda cid:"a:disk", volume_entry=lambda cid:{"volid":"a:disk","size":1024**3}, volume_exists=lambda cid:False)
        self.assertEqual(scenario.verify_disk("cid"), "a:disk")
        with self.assertRaisesRegex(RuntimeError,"physical size"):
            scenario.verify_disk("cid", 1040)
        record = {"ID":"allocation","CID":"cid","Kind":"disk","State":"ready_to_return"}
        with patch.object(scenario,"success"), patch.object(scenario,"audit",return_value={"records":[record],"audit":{"evidence":[]}}):
            with self.assertRaisesRegex(RuntimeError,"terminal"):
                scenario.delete_disk("cid")
            record["State"] = "deleted"
            scenario.delete_disk("cid")

    def test_returned_cids_are_persisted_before_followup_observation(self):
        with patch.object(recovery,"PVEVerifier",FakeVerifier):
            scenario = recovery.RecoveryScenario(self.runner,self.fixture,"mixed_legacy_managed")
        with patch.object(scenario,"success",side_effect=["cid-one","cid-two"]):
            scenario.create_disk()
            scenario.create_disk()
        files = sorted(scenario.directory.glob("recovery-evidence-*.json"))
        self.assertEqual(len(files),2)
        self.assertEqual(json.loads(files[-1].read_text())["returned_cids"],["cid-one","cid-two"])

    def test_shared_directory_contexts_have_distinct_durable_cid_checkpoints(self):
        with patch.object(recovery,"PVEVerifier",FakeVerifier):
            first=recovery.RecoveryScenario(self.runner,self.fixture,"context_isolation")
            second=recovery.RecoveryScenario(self.runner,self.fixture,"context_isolation",directory=first.directory)
        with patch.object(first,"success",return_value="cid-a"), patch.object(second,"success",return_value="cid-b"):
            first.create_disk()
            second.create_disk()
        files=list(first.directory.glob("recovery-evidence-*.json"))
        self.assertEqual(len(files),2)
        self.assertEqual({json.loads(path.read_text())["namespace"] for path in files},{first.namespace,second.namespace})

    def test_context_trust_mismatch_refuses_before_authority_creation(self):
        paths = []
        for index in range(2):
            config = dict(self.runner.base_config, pve_ca_cert="CA-"+str(index))
            path = self.root / (str(index)+".json")
            path.write_text(json.dumps(config))
            paths.append(str(path))
        with patch.object(recovery,"RecoveryScenario",side_effect=AssertionError("must not initialize")):
            with self.assertRaisesRegex(ValueError,"trusted CA"):
                recovery._run_contexts(self.runner,dict(self.fixture,contexts=paths),{})

    def test_mixed_lifecycle_uses_removed_sets_for_both_existing_cids(self):
        fixture = {**self.fixture, "cases": ["mixed_legacy_managed"], "legacy_storage": "legacy"}
        with patch.object(recovery, "PVEVerifier", FakeVerifier):
            scenario = recovery.RecoveryScenario(self.runner, fixture, "mixed_legacy_managed")
        events = []
        def success(method, args, **kwargs):
            config = json.loads(kwargs["config_path"].read_text())
            self.assertNotIn("storage_sets", config)
            self.assertNotIn("persistent_storage_set", config)
            self.assertEqual(config["storage_placement_namespace"], scenario.namespace)
            events.append((method, args))
            return method == "has_disk"
        with patch.object(scenario, "initialize"), patch.object(scenario, "create_disk", side_effect=["legacy:old", "a:new"]), patch.object(scenario, "verify_disk"), patch.object(scenario, "success", side_effect=success), patch.object(scenario, "delete_disk") as delete, patch.object(scenario, "audit", return_value={}):
            scenario.run_mixed()
        self.assertEqual(events, [("has_disk", ["legacy:old"]), ("resize_disk", ["legacy:old", 1040]),
                                  ("has_disk", ["a:new"]), ("resize_disk", ["a:new", 1040])])
        self.assertEqual([call.args[0] for call in delete.call_args_list], ["legacy:old", "a:new"])


if __name__ == "__main__":
    unittest.main()
