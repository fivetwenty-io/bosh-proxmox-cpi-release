"""Offline ownership, artifact and failure-boundary tests for rollout workflows."""
import copy
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _storage_placement_rollout as rollout


def fixture():
    return {"candidate_sha256": "a" * 64, "linux_cpi_sha256": "b" * 64}


def manifest_value():
    return {"candidate_manifest": "/private/candidate.json", "scalar_manifest": "/private/scalar.json", "baseline_manifest": "/private/baseline.json",
            "state_file": "/durable/state.json", "vars_store": "/durable/vars.json", "baseline_archive": "/private/baseline.tgz",
            "baseline_sha256": "c" * 64, "baseline_linux_cpi_sha256": "d" * 64}


def document(name="bosh-proxmox-cpi", version="1", selector="cert-e"):
    properties = {"storage_placement_namespace": "director", "storage_allocation_journal_dir": "/var/vcap/store/journal", "vm_storage": "e1", "iso_storage": "iso1", "iso_storage_follow_vm_storage": False}
    if selector:
        properties["ephemeral_storage_set"] = selector
    return {"name": "dedicated-director", "releases": [{"name": name, "url": "file:///private/" + version + ".tgz", "version": version}],
            "instance_groups": [{"name": "bosh", "jobs": [{"name": "pve_cpi", "release": name, "properties": {"pve": properties}}]}],
            "cloud_provider": {"template": {"name": "pve_cpi", "release": name}, "properties": {"pve": {"storage_placement_namespace": "bootstrap", "storage_allocation_journal_dir": "/durable/journal"}}},
            "resource_pools": [{"cloud_properties": {"root_storage_set": "cert-e"} if selector else {}}]}


class RolloutTests(unittest.TestCase):
    def test_manifest_contract_rejects_extra_hook_and_same_artifacts(self):
        for updates in ({"hook": "arbitrary"}, {"baseline_sha256": "a" * 64}, {"baseline_linux_cpi_sha256": "b" * 64}, {"state_file": "relative"}, {"vars_store": "/durable/state.json"}):
            value = manifest_value(); value.update(updates)
            with self.subTest(updates=updates), self.assertRaises(RuntimeError):
                rollout.validate_rollout_manifest(value, fixture())
        self.assertEqual(rollout.validate_rollout_manifest(None, fixture()), {})
        self.assertEqual(rollout.validate_rollout_manifest(manifest_value(), fixture()), manifest_value())

    def test_private_inputs_refuse_symlink_broad_permissions_and_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory).resolve() / "input.json"; path.write_text("{}"); path.chmod(0o600)
            self.assertEqual(rollout.private_file(str(path), "input"), path)
            link = Path(directory).resolve() / "link"; link.symlink_to(path)
            for candidate in (str(link), directory):
                with self.subTest(candidate=candidate), self.assertRaises(RuntimeError):
                    rollout.private_file(candidate, "input")
            path.chmod(0o644)
            with self.assertRaises(RuntimeError):
                rollout.private_file(str(path), "input")
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)

    def test_exact_artifact_identity_not_just_release_name(self):
        artifact = {"name": "bosh-proxmox-cpi", "path": "/private/1.tgz", "version": "1"}
        rollout.validate_manifest_artifact(document(), artifact)
        for mutate in (lambda doc: doc["releases"][0].update(url="https://example/other.tgz"),
                       lambda doc: doc["releases"].append(copy.deepcopy(doc["releases"][0])),
                       lambda doc: doc["cloud_provider"]["template"].update(release="other"),
                       lambda doc: doc["releases"][0].update(version="2")):
            value = document(); mutate(value)
            with self.assertRaises(RuntimeError):
                rollout.validate_manifest_artifact(value, artifact)

    def test_rollout_shape_allows_only_cpi_release_and_selector_changes(self):
        candidate = document()
        baseline = document("bosh-pve-cpi", "0", selector="")
        self.assertEqual(rollout.rollout_shape(candidate), rollout.rollout_shape(baseline))
        for mutate in (lambda doc: doc.update(name="another-director"),
                       lambda doc: doc["cloud_provider"]["properties"]["pve"].update(host="different-cluster"),
                       lambda doc: doc["instance_groups"][0]["jobs"][0]["properties"]["pve"].update(vm_storage="other")):
            changed = copy.deepcopy(baseline); mutate(changed)
            self.assertNotEqual(rollout.rollout_shape(candidate), rollout.rollout_shape(changed))

    def test_selector_detection_covers_nested_resource_and_global_layers(self):
        for value in ({"root_storage_set": "s"}, {"vm_types": [{"cloud_properties": {"storage_set": "s"}}]}, {"disk_pools": [{"cloud_properties": {"persistent_storage_set": "s"}}]}):
            self.assertTrue(rollout.contains_selector(value))
        self.assertFalse(rollout.contains_selector({"storage_sets": {"s": {}}, "root_storage_set": ""}))

    def test_bootstrap_state_requires_unique_existing_cids(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory).resolve() / "state.json"
            for value in ({"current_vm_cid": "101", "disks": []}, {"current_vm_cid": "101", "disks": [{"cid": "one"}, {"cid": "one"}]}, {"current_vm_cid": "", "disks": [{"cid": "one"}]}):
                path.write_text(json.dumps(value)); path.chmod(0o600)
                with self.assertRaises(RuntimeError):
                    rollout.state_identity(str(path))
            path.write_text(json.dumps({"current_vm_cid": "101", "disks": [{"cid": "one"}]}))
            self.assertEqual(rollout.state_identity(str(path)), {"vm_cid": "101", "disk_cids": ["one"]})

    def test_absent_rollout_still_cleans_successful_core_resources(self):
        director = SimpleNamespace(fixture=fixture(), cleanup=Mock(return_value={"all_owned_records_terminal": True}))
        rows = rollout.run_rollout_scenarios(director, None)
        director.cleanup.assert_called_once()
        self.assertEqual([row["scenario_id"] for row in rows], list(rollout.ROLLOUT_IDS))
        self.assertEqual({row["status"] for row in rows}, {"missing"})

    def test_absent_rollout_cleanup_failure_is_not_a_missing_pass(self):
        director = SimpleNamespace(fixture=fixture(), cleanup=Mock(side_effect=RuntimeError("cleanup")))
        rows = rollout.run_rollout_scenarios(director, None)
        self.assertEqual([row["status"] for row in rows], ["failed", "missing", "missing"])

    def test_stage_failure_retains_resources_and_visible_remaining_rows(self):
        director = SimpleNamespace(fixture=fixture(), cleanup=Mock())
        scenario = SimpleNamespace(preflight=Mock(), global_removal=Mock(return_value={}), downgrade=Mock(side_effect=RuntimeError("old binary refused")), upgrade=Mock())
        with patch.object(rollout, "RolloutScenarios", return_value=scenario):
            rows = rollout.run_rollout_scenarios(director, manifest_value())
        self.assertEqual([row["status"] for row in rows], ["passed", "failed", "missing"])
        scenario.upgrade.assert_not_called(); director.cleanup.assert_not_called()

    def test_malformed_release_archive_reports_failed_and_remaining_missing(self):
        director = SimpleNamespace(fixture=fixture(), cleanup=Mock())
        with patch.object(rollout, "RolloutScenarios", side_effect=rollout.tarfile.ReadError("malformed archive")):
            rows = rollout.run_rollout_scenarios(director, manifest_value())
        self.assertEqual([row["status"] for row in rows], ["failed", "missing", "missing"])
        director.cleanup.assert_not_called()

    def test_scalar_bootstrap_binding_cannot_survive_global_removal(self):
        candidate = document()
        scalar = document(selector="")
        scalar["cloud_provider"]["properties"]["pve"]["root_storage_set"] = "hidden-bootstrap-binding"
        artifact = {"name":"bosh-proxmox-cpi","path":"/private/1.tgz","sha256":"a"*64,"version":"1"}
        baseline = {**artifact,"version":"0"}
        director = SimpleNamespace(observer=SimpleNamespace(artifact=artifact),fixture={**fixture(),"namespace":"director"},runner=SimpleNamespace(base_config={"storage_placement_namespace":"bootstrap","storage_allocation_journal_dir":"/durable/journal"}))
        inputs = [Mock(read_text=lambda:json.dumps(candidate)),Mock(read_text=lambda:json.dumps(scalar))]
        with patch("_storage_placement_director.archive_fingerprints",return_value=baseline), patch.object(rollout,"private_file",side_effect=inputs), self.assertRaisesRegex(RuntimeError,"global set bindings"):
            rollout.RolloutScenarios(director,manifest_value())

    def test_successful_upgrade_requires_final_cleanup(self):
        director = SimpleNamespace(fixture=fixture(), cleanup=Mock(return_value={"all_owned_records_terminal": True}))
        scenario = SimpleNamespace(preflight=Mock(), global_removal=Mock(return_value={}), downgrade=Mock(return_value={}), upgrade=Mock(return_value={}))
        with patch.object(rollout, "RolloutScenarios", return_value=scenario):
            rows = rollout.run_rollout_scenarios(director, manifest_value())
        self.assertEqual({row["status"] for row in rows}, {"passed"})
        self.assertTrue(rows[-1]["evidence"]["cleanup"]["all_owned_records_terminal"])

    def test_final_cleanup_failure_does_not_overrun_row_list(self):
        director = SimpleNamespace(fixture=fixture(), cleanup=Mock(side_effect=RuntimeError("cleanup")))
        scenario = SimpleNamespace(preflight=Mock(), global_removal=Mock(return_value={}), downgrade=Mock(return_value={}), upgrade=Mock(return_value={}))
        with patch.object(rollout, "RolloutScenarios", return_value=scenario):
            rows = rollout.run_rollout_scenarios(director, manifest_value())
        self.assertEqual([row["status"] for row in rows], ["passed", "passed", "failed"])

    def test_old_binary_snapshot_uses_retained_candidate_auditor_only(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        observer = SimpleNamespace(snapshot=Mock(return_value={"director_uuid": "director-id", "records": {}}))
        scenario.director = SimpleNamespace(observer=observer, fixture=fixture(), bosh=Mock(return_value={"uuid": "director-id"}))
        scenario.remote_before = {"director_uuid": "director-id"}
        scenario.remote_history = scenario.local_history = {}
        scenario.local_records = Mock(return_value={})
        scenario.baseline, scenario.candidate = {"version": "0"}, {"version": "1"}
        scenario.value = manifest_value()
        scenario.retained = {"binary": "fixed/candidate-cpi", "config": "fixed/candidate-config.json"}
        scenario.snapshot(True)
        self.assertEqual(observer.snapshot.call_args.kwargs, {"expected_artifact": scenario.baseline, "expected_linux_sha": "d" * 64, "audit_binary": "fixed/candidate-cpi", "audit_config": "fixed/candidate-config.json"})
        scenario.snapshot(False)
        self.assertIsNone(observer.snapshot.call_args.kwargs["audit_binary"])
        self.assertEqual(observer.snapshot.call_args.kwargs["expected_linux_sha"], "b" * 64)

    def test_history_preserves_intent_and_every_prior_evidence_entry(self):
        before = {"allocation": {"id": "allocation", "kind": "disk", "intent": {"policy": "frozen"}, "steps": [{"id": "create", "upid": "UPID:actual", "volids": ["pool:owned"]}], "verifications": [{"evidence": "durable"}], "attempts": [{"plan": {"seed": "old"}}]}}
        after = copy.deepcopy(before)
        after["allocation"]["steps"].append({"id": "attach"})
        after["allocation"]["verifications"].append({"evidence": "fresh"})
        rollout.preserve_history(before, after)
        for field in ("intent", "steps", "verifications", "attempts"):
            changed = copy.deepcopy(after); changed["allocation"][field] = {} if field == "intent" else []
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                rollout.preserve_history(before, changed)

    def test_unregistered_returnable_cid_blocks_downgrade(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        scenario.director = SimpleNamespace(bosh=Mock(return_value={"Tables": [{"Rows": [{"vm_cid": "101"}]}]}), instances=Mock(return_value={"disk_cids": ["disk"]}))
        scenario.remote_before = {"records": {"orphan": {"state": "ready_to_return", "kind": "vm", "cid": "999"}}}
        scenario.local_history = {}; scenario.bootstrap = {"vm_cid": "100", "disk_cids": ["bootstrap"]}
        with self.assertRaisesRegex(RuntimeError, "registered Director owner"):
            scenario.prove_registered_returnable_records()
        scenario.remote_before["records"]["orphan"]["cid"] = "101"
        scenario.prove_registered_returnable_records()
        scenario.local_history = {"unknown": {"state": "ready_to_return", "kind": "disk", "cid": "orphan-disk"}}
        with self.assertRaisesRegex(RuntimeError, "create-env owner"):
            scenario.prove_registered_returnable_records()

    def test_bootstrap_vm_mismatch_stops_before_remote_copy_or_update(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        scenario.director = SimpleNamespace(snapshot=Mock(return_value={"vm_cid": "other"}))
        scenario.bootstrap = {"vm_cid": "101"}
        scenario.runner = SimpleNamespace(audit=Mock())
        scenario.retain_auditor = Mock()
        with self.assertRaisesRegex(RuntimeError, "different VM"):
            scenario.preflight()
        scenario.runner.audit.assert_not_called(); scenario.retain_auditor.assert_not_called()

    def test_create_env_is_artifact_pinned_and_uses_exact_existing_state(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        scenario.value = manifest_value()
        scenario.candidate = {"path": "/private/candidate.tgz", "sha256": "a" * 64, "version": "1"}
        scenario.baseline = {"path": "/private/baseline.tgz", "sha256": "c" * 64, "version": "0"}
        scenario.documents = {"candidate_manifest": document()}
        scenario.director = SimpleNamespace(write_json=Mock(return_value="/private/rendered-candidate.json"))
        scenario.runner = SimpleNamespace(active_resources={}, checkpoint=Mock())
        scenario.remote_before = {"director_uuid": "director", "records": {"original": {}}}
        scenario.snapshot = Mock(return_value={"vm_cid": "102", "records": {"original": {}}})
        scenario.disk_before = [{"cid": "persistent", "allocation_uuid": "uuid"}]
        scenario.bootstrap_disks = Mock(return_value=copy.deepcopy(scenario.disk_before))
        with patch("_storage_placement_rollout_trust.begin_update"), patch("_storage_placement_rollout_trust.finish_update"), patch("_storage_placement_rollout_trust.preflight_rollout_trust") as preflight, patch("_storage_placement_rollout_trust.refresh_rollout_trust") as refresh, patch("_storage_placement_director.archive_fingerprints", return_value=scenario.candidate), patch.object(rollout, "state_identity", return_value={"vm_cid": "102"}), patch.object(rollout.subprocess, "run", return_value=SimpleNamespace(returncode=0)) as command:
            scenario.create_env("candidate_manifest")
            refresh.assert_called_once_with(scenario.director, scenario.value["state_file"], "director", "candidate_manifest")
        self.assertEqual(command.call_args.args[0], ["bosh", "-n", "--tty", "create-env", "/private/rendered-candidate.json", "--state=/durable/state.json", "--vars-store=/durable/vars.json"])
        self.assertEqual(scenario.runner.active_resources["director_rollout"]["release_sha256"], "a" * 64)

    def test_changed_archive_is_rejected_before_create_env(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        scenario.candidate = {"path": "/private/candidate.tgz", "sha256": "a" * 64}
        with patch("_storage_placement_director.archive_fingerprints", side_effect=ValueError("checksum differs")), patch.object(rollout.subprocess, "run") as command, self.assertRaises(ValueError):
            scenario.create_env("candidate_manifest")
        command.assert_not_called()

    def test_workload_recreation_rejects_root_iso_or_vm_leaks(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        scenario.runner = SimpleNamespace(verifier=SimpleNamespace(vm_exists=Mock(return_value=False)), verification=SimpleNamespace(volume_inventory=Mock(return_value=[])))
        previous = {"vm_cid": "101", "vm": {"devices": {"virtio0": "pool:root", "scsi1": "pool:ephemeral"}, "iso": {"ide2": "pool:iso"}}}
        scenario.old_workload_absent(previous, {"vm_cid": "102"})
        scenario.runner.verification.volume_inventory.return_value = [("node", "pool", "pool:iso", 10)]
        with self.assertRaisesRegex(RuntimeError, "previous root"):
            scenario.old_workload_absent(previous, {"vm_cid": "102"})
        scenario.runner.verification.volume_inventory.return_value = []
        scenario.runner.verifier.vm_exists.return_value = True
        with self.assertRaisesRegex(RuntimeError, "previous VM"):
            scenario.old_workload_absent(previous, {"vm_cid": "102"})

    def test_downgrade_rejects_managed_vm_before_old_binary_invocation(self):
        for location in ("remote", "bootstrap"):
            for state in ("ready_to_return", "adopted", "submitted"):
                with self.subTest(location=location, state=state):
                    scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
                    scenario.director = SimpleNamespace(service={"instance": "service/0"}, operation=Mock())
                    record = {"kind": "vm", "state": state}
                    scenario.local_history = {"vm": record} if location == "bootstrap" else {}
                    scenario.snapshot = Mock(return_value={"records": {"vm": record} if location == "remote" else {}})
                    scenario.create_env = Mock()
                    with self.assertRaisesRegex(RuntimeError, "downgrade requires"):
                        scenario.downgrade()
                    scenario.create_env.assert_not_called()
                    scenario.director.operation.assert_not_called()

    def test_candidate_scalar_transition_then_baseline_lifecycle(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        terminal = {"vm": {"kind": "vm", "state": "deleted"}, "disk": {"kind": "disk", "state": "ready_to_return"}}
        scenario.local_history = copy.deepcopy(terminal)
        observed = {"records": copy.deepcopy(terminal), "policy": {}, "authority_sha256": "authority"}
        scenario.director = SimpleNamespace(service={"instance": "service/0", "vm_cid": "101"}, operation=Mock(return_value={"task": "done"}))
        scenario.dispose_kept_errands = Mock(return_value={"all_errand_resources_absent": True})
        scenario.scalar_cloud = Mock()
        scenario.create_env = Mock(return_value=observed)
        scenario.snapshot = Mock(return_value=observed)
        scenario.service_disks = Mock(side_effect=[{"vm_cid": "102"}, {"vm_cid": "103"}])
        scenario.old_workload_absent = Mock()
        scenario.candidate = {"sha256": "candidate"}; scenario.baseline = {"sha256": "baseline"}
        scenario.retained = {"binary": "retained-candidate"}
        scalar = scenario.global_removal()
        downgraded = scenario.downgrade()
        self.assertTrue(scalar["global_and_resource_selectors_removed"])
        self.assertEqual(downgraded["service"]["vm_cid"], "103")
        self.assertEqual([call.args for call in scenario.create_env.call_args_list], [("scalar_manifest",), ("baseline_manifest",)])
        self.assertTrue(scenario.create_env.call_args_list[-1].kwargs["baseline"])
        self.assertEqual(scenario.old_workload_absent.call_count, 2)
        self.assertEqual(scenario.local_history, terminal)

    def test_scalar_transition_refuses_unsettled_old_vm_history(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        scenario.director = SimpleNamespace(service={"instance": "service/0", "vm_cid": "101"}, operation=Mock())
        scenario.dispose_kept_errands = Mock(return_value={"all_errand_resources_absent": True})
        scenario.scalar_cloud = Mock(); scenario.create_env = Mock(return_value={"policy": {}})
        scenario.snapshot = Mock(return_value={"records": {"old": {"kind": "vm", "state": "ready_to_return", "cid": "101"}}})
        scenario.service_disks = Mock(return_value={"vm_cid": "102"})
        scenario.old_workload_absent = Mock(); scenario.local_history = {}
        with self.assertRaisesRegex(RuntimeError, "candidate-disposed"):
            scenario.global_removal()
        self.assertEqual(scenario.director.service["vm_cid"], "101")

    def test_scalar_create_env_recreates_vm_without_replacing_persistent_disks(self):
        scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
        scenario.value = manifest_value()
        scenario.candidate = {"path": "/private/candidate.tgz", "sha256": "a" * 64, "version": "1"}
        scenario.documents = {"scalar_manifest": document(selector="")}
        scenario.director = SimpleNamespace(write_json=Mock(return_value="/private/scalar.json"))
        scenario.runner = SimpleNamespace(active_resources={}, checkpoint=Mock(), verifier=SimpleNamespace(vm_exists=Mock(return_value=False)))
        scenario.remote_before = {"director_uuid": "director", "records": {}}
        scenario.snapshot = Mock(return_value={"vm_cid": "102", "records": {}})
        scenario.disk_before = [{"cid": "same", "allocation_uuid": "same-uuid"}]
        scenario.bootstrap_disks = Mock(return_value=copy.deepcopy(scenario.disk_before))
        with patch("_storage_placement_rollout_trust.begin_update"), patch("_storage_placement_rollout_trust.finish_update"), patch("_storage_placement_rollout_trust.preflight_rollout_trust") as preflight, patch("_storage_placement_rollout_trust.refresh_rollout_trust") as refresh, patch("_storage_placement_director.archive_fingerprints", return_value=scenario.candidate), patch.object(rollout, "state_identity", side_effect=[{"vm_cid": "101"}, {"vm_cid": "102"}]), patch.object(rollout.subprocess, "run", return_value=SimpleNamespace(returncode=0)) as command:
            scenario.create_env("scalar_manifest")
            refresh.assert_called_once_with(scenario.director, scenario.value["state_file"], "director", "scalar_manifest")
        argv = command.call_args.args[0]
        self.assertIn("--recreate", argv)
        self.assertNotIn("--recreate-persistent-disks", argv)
        scenario.runner.verifier.vm_exists.assert_called_once_with("101")
        scenario.bootstrap_disks.assert_called_once()


    def test_kept_errand_uses_candidate_cleanup_and_checks_all_artifacts(self):
        for failure in (None, "ready", "VM", "volume"):
            with self.subTest(failure=failure):
                scenario = rollout.RolloutScenarios.__new__(rollout.RolloutScenarios)
                before = {"id": "errand", "state": "ready_to_return", "cid": "110", "steps": [{"volids": ["e:root", "e:iso"]}]}
                after = {**before, "state": "ready_to_return" if failure == "ready" else "deleted"}
                scenario.snapshot = Mock(side_effect=[{"phase": "before"}, {"phase": "after"}])
                scenario.director = SimpleNamespace(fixture={"errand_name": "storage-cert-errand"}, records_with_tag=Mock(side_effect=[[before], [after]]), operation=Mock(return_value={"task": "done"}))
                scenario.runner = SimpleNamespace(verifier=SimpleNamespace(vm_exists=Mock(return_value=failure == "VM")), verification=SimpleNamespace(volume_inventory=Mock(return_value=[("node", "e", "e:iso", 10)] if failure == "volume" else [])))
                if failure:
                    with self.assertRaises(RuntimeError):
                        scenario.dispose_kept_errands()
                else:
                    result = scenario.dispose_kept_errands()
                    self.assertTrue(result["all_errand_resources_absent"])
                scenario.director.operation.assert_called_once_with(["run-errand", "storage-cert-errand"])


    def test_no_ready_alias_accepts_an_inflight_record(self):
        self.assertIn("ready_to_return", rollout.TERMINAL_OR_READY)
        for state in ("ready", "planned", "submitted", "reconciliation_required", "observed"):
            self.assertNotIn(state, rollout.TERMINAL_OR_READY)


if __name__ == "__main__":
    unittest.main()
