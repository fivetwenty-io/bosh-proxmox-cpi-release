"""Offline assertions for the real-runtime acquired-capacity barrier."""
import copy
import json
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _storage_placement_transition import PostRootCapacityHook, transition_ledger, transition_arguments


def record():
    root = {"Role": "root", "StorageID": "e1", "Node": "n1", "CapacityKey": "nfs:x"}
    ephemeral = dict(root, Role="ephemeral")
    return {"id": "allocation", "intent": {"plan": {"Targets": [root, ephemeral], "Charges": [
        {"CapacityKey": "nfs:x", "Charge": {"Bytes": 100,"Role":"root"}}, {"CapacityKey": "nfs:x", "Charge": {"Bytes": 40,"Role":"ephemeral"}}]}},
        "steps": [{"id": "root-step", "kind": "vm.QEMU.Create", "state": "observed", "upid": "UPID:n1:root",
                   "volids": ["e1:vm-123-root"], "charges": [{"backing": "nfs:x", "planned_bytes": 100, "acquired_bytes": 100, "outstanding_bytes": 0}]}]}


def payload(available=1000):
    return json.dumps({"data": [{"storage": "e1", "avail": available, "total": 2000, "used": 2000 - available}]}).encode()


class TransitionTests(unittest.TestCase):
    def hook(self, value=None, reject=False):
        runner = SimpleNamespace(verifier=SimpleNamespace(_get=Mock(side_effect=[
            {"status": "stopped", "exitstatus": "OK"}, [{"volid": "e1:vm-123-root", "size": 100}]])))
        return PostRootCapacityHook(runner, reject), SimpleNamespace(current_record=Mock(return_value=value or record()))

    def test_exact_remaining_debit_excludes_observed_root(self):
        hook, proxy = self.hook()
        result = json.loads(hook("GET", "/nodes/n1/storage", payload(), proxy))
        self.assertEqual(result["data"][0]["avail"], 40)
        self.assertEqual(result["data"][0]["used"], 1960)
        self.assertEqual(hook.evidence["acquired_bytes"], 100)
        self.assertEqual(hook.evidence["remaining_bytes"], 40)
        self.assertEqual(hook.reads, 1)

    def test_capacity_cap_never_invents_real_free_bytes(self):
        hook, proxy = self.hook()
        with self.assertRaisesRegex(RuntimeError, "actual backend"):
            hook("GET", "/nodes/n1/storage", payload(39), proxy)
        self.assertEqual(hook.reads, 0)

    def test_actual_task_or_volume_absence_blocks_barrier(self):
        for results in ([{"status": "running"}], [{"status": "stopped", "exitstatus": "OK"}, []]):
            hook, proxy = self.hook()
            hook.runner.verifier._get.side_effect = results
            with self.subTest(results=results), self.assertRaises(RuntimeError):
                hook("GET", "/nodes/n1/storage", payload(), proxy)
            self.assertIsNone(hook.evidence)

    def test_unknown_root_does_not_trigger_or_certify(self):
        value = record(); value["steps"][0]["state"] = "submitted"
        hook, proxy = self.hook(value)
        original = payload()
        self.assertEqual(hook("GET", "/nodes/n1/storage", original, proxy), original)
        self.assertIsNone(hook.evidence)
        hook.runner.verifier._get.assert_not_called()

    def test_late_barrier_after_ephemeral_submission_rejected(self):
        value = record(); value["steps"].append({"kind": "vm.Storage.CreateVolume", "state": "planned"})
        hook, proxy = self.hook(value)
        with self.assertRaisesRegex(RuntimeError, "after ephemeral submission"):
            hook("GET", "/nodes/n1/storage", payload(), proxy)

    def test_malformed_charge_partition_and_unreflected_acquisition_rejected(self):
        for acquired, outstanding in ((101, 0), (50, 50), (-1, 101)):
            value = record(); charge = value["steps"][0]["charges"][0]
            charge.update(acquired_bytes=acquired, outstanding_bytes=outstanding)
            with self.subTest(acquired=acquired), self.assertRaises(RuntimeError):
                transition_ledger(value)

    def test_next_step_acquisition_reduces_remaining_without_recharging_root(self):
        hook, proxy = self.hook()
        hook("GET", "/nodes/n1/storage", payload(), proxy)
        value = record(); value["steps"].append({"kind": "vm.Storage.CreateVolume", "state": "observed", "charges": [
            {"backing": "nfs:x", "planned_bytes": 40, "acquired_bytes": 40, "outstanding_bytes": 0}]})
        proxy.current_record.return_value = value
        result = json.loads(hook("GET", "/nodes/n1/storage", payload(), proxy))
        self.assertEqual(result["data"][0]["avail"], 0)
        self.assertEqual(transition_ledger(value)["remaining_bytes"], 0)

    def test_journal_identity_cannot_switch_after_barrier(self):
        hook, proxy = self.hook()
        hook("GET", "/nodes/n1/storage", payload(), proxy)
        value = record(); value["id"] = "other"
        proxy.current_record.return_value = value
        with self.assertRaisesRegex(RuntimeError, "UUID"):
            hook("GET", "/nodes/n1/storage", payload(), proxy)

    def test_old_attempt_acquisition_is_excluded(self):
        value = record()
        value["attempts"] = [{"plan": copy.deepcopy(value["intent"])}, {"plan": copy.deepcopy(value["intent"])}]
        with self.assertRaisesRegex(RuntimeError, "root has no durable"):
            transition_ledger(value)
        value["steps"][0]["attempt"] = 1
        self.assertEqual(transition_ledger(value)["remaining_bytes"], 40)

    def test_negative_headroom_is_one_byte_below_outstanding(self):
        hook, proxy = self.hook(reject=True)
        result = json.loads(hook("GET", "/nodes/n1/storage", payload(), proxy))
        self.assertEqual(result["data"][0]["avail"], 39)

    def test_controlled_responses_retain_original_and_substituted_capacity(self):
        hook,proxy=self.hook()
        hook("GET","/nodes/n1/storage",payload(),proxy)
        row=hook.evidence["capacity_responses"][0]
        self.assertEqual(row,{"node":"n1","storage":"e1","total_bytes":2000,"original_available_bytes":1000,"controlled_available_bytes":40,"remaining_bytes":40})

    def test_mutable_charge_cannot_claim_another_roles_budget(self):
        value=record()
        value["steps"][0]["charges"][0].update(planned_bytes=40,acquired_bytes=40)
        with self.assertRaisesRegex(RuntimeError,"immutable role budget"):
            transition_ledger(value)

    def test_transition_clears_per_vm_storage_overrides(self):
        properties={key:"override" for key in ("root_storage_set","storage_pool","storage_tier","ephemeral_storage_set","ephemeral_storage_pool","ephemeral_storage_tier")}
        properties["cpu"]=2
        runner=SimpleNamespace(vm_arguments=lambda _:["agent","stemcell",copy.deepcopy(properties)])
        args=transition_arguments(runner,"case")
        self.assertEqual(args[2],{"cpu":2})
        self.assertEqual(properties["storage_pool"],"override")

    def test_success_uses_actual_ready_to_return_state_and_verifies_cleanup(self):
        import _storage_placement_transition as transition
        import _storage_placement_vm_faults as vm_faults
        import _storage_placement_scenarios as scenarios
        value=record()
        value["state"]="ready_to_return"
        value["steps"].append({"kind":"vm.Storage.CreateVolume","state":"observed","charges":[{"backing":"nfs:x","planned_bytes":40,"acquired_bytes":40,"outstanding_bytes":0}]})
        summary={"ID":"allocation","State":"ready_to_return"}
        tombstone={"ID":"allocation","State":"deleted"}
        hook=SimpleNamespace(evidence={"remaining_bytes":40},reads=1,allocation_id="allocation")
        proxy=Mock(config_override={},evidence={"blocked_mutations":0})
        proxy.__enter__=Mock(return_value=proxy); proxy.__exit__=Mock(return_value=False)
        runner=SimpleNamespace(configured_policy=lambda **kwargs:{},policy={"ephemeral_storage_ids":["e1"]},vm_arguments=lambda _:["agent","stemcell",{}],active_resources={},checkpoint=Mock(),derived_config=lambda *args:"config",base_config={"storage_placement_namespace":"test","storage_allocation_journal_dir":"/private"},config_path="base",verifier=object(),verification=SimpleNamespace(volume_inventory=lambda _:[]),audit=Mock(side_effect=[summary,tombstone]),record_for=lambda audit,*_:audit,call_once=Mock(side_effect=[["123"],None]),observed_vm=Mock(return_value={"vmid":"123"}),observed_root_mechanism=Mock(return_value="full-clone"))
        with patch.object(transition,"PostRootCapacityHook",return_value=hook),patch.object(vm_faults,"JournalVMFaultProxy",return_value=proxy),patch.object(scenarios,"verified_record_payload",return_value=value),patch.object(Path,"read_bytes",return_value=b"verified"):
            result=transition.post_root_transition(runner,{})
        self.assertEqual(result["cleanup_state"],"deleted")
        self.assertEqual(result["final_acquired_bytes"],140)
        self.assertEqual(runner.call_once.call_args.args,("delete_vm",["123"],"base"))
        self.assertFalse(runner.active_resources)

    def test_negative_runtime_phase_uses_sanitized_error_and_never_cleans_unknown_steps(self):
        import _storage_placement_transition as transition
        import _storage_placement_vm_faults as vm_faults
        import _storage_placement_scenarios as scenarios
        value=record()
        summary={"ID":"allocation","State":"reconciliation_required"}
        audits=[{"records":[summary]},{"records":[{"ID":"allocation","State":"cleaned"}]}]
        hook=SimpleNamespace(evidence={"acquired_bytes":100,"remaining_bytes":40},reads=1,allocation_id="allocation")
        proxy=Mock(config_override={},evidence={"blocked_mutations":0,"mutations":[{"kind":"vm.QEMU.Create"}]})
        proxy.__enter__=Mock(return_value=proxy)
        proxy.__exit__=Mock(return_value=False)
        command=Mock(return_value={"allocation_id":"allocation","state":"cleaned"})
        runner=SimpleNamespace(configured_policy=lambda **kwargs:{},policy={"ephemeral_storage_ids":["e1"]},vm_arguments=lambda _:["agent","stemcell",{}],active_resources={},checkpoint=Mock(),derived_config=lambda *args:"config",base_config={"storage_placement_namespace":"test","storage_allocation_journal_dir":"/private"},config_path="base",cpi_bin="cpi",verifier=object(),verification=SimpleNamespace(volume_inventory=lambda _:[],_json_command=command),audit=Mock(side_effect=audits),call_once=Mock(side_effect=scenarios.CPIRejected("allocation allocation requires reconciliation at VM post-create; no alternate allocation was attempted")))
        with patch.object(transition,"PostRootCapacityHook",return_value=hook),patch.object(vm_faults,"JournalVMFaultProxy",return_value=proxy),patch.object(scenarios,"verified_record_payload",return_value=value),patch.object(Path,"read_bytes",return_value=b"verified"):
            result=transition.post_root_capacity_rejection(runner,{})
            self.assertEqual(result["cleanup_state"],"cleaned")
            self.assertEqual(runner.constraint_failure_evidence["rejection"], "allocation allocation requires reconciliation at VM post-create; no alternate allocation was attempted")
            self.assertEqual(runner.constraint_failure_evidence["root_submissions"], 1)
            command.assert_called_once()
            command.reset_mock()
            runner.audit=Mock(return_value=audits[0])
            value["steps"].append({"kind":"vm.Nodes.UpdateQemuConfig","state":"planned"})
            with self.assertRaisesRegex(RuntimeError,"unresolved mutation"):
                transition.post_root_capacity_rejection(runner,{})
            command.assert_not_called()
            self.assertEqual(runner.constraint_failure_evidence["phase"], "record-readback")
            self.assertEqual(runner.constraint_failure_evidence["barrier"], {"acquired_bytes":100,"remaining_bytes":40})

    def test_failed_record_readback_preserves_barrier_and_sanitized_rejection(self):
        import _storage_placement_transition as transition
        import _storage_placement_vm_faults as vm_faults
        import _storage_placement_scenarios as scenarios
        hook = SimpleNamespace(evidence={"acquired_bytes":100,"remaining_bytes":40}, reads=2, allocation_id="allocation")
        proxy = Mock(config_override={}, evidence={"blocked_mutations":0,"mutations":[{"kind":"vm.QEMU.Create"}]})
        proxy.__enter__ = Mock(return_value=proxy); proxy.__exit__ = Mock(return_value=False)
        command = Mock()
        runner = SimpleNamespace(configured_policy=lambda **_: {}, policy={"ephemeral_storage_ids":["e1"]}, vm_arguments=lambda _: ["agent","stemcell",{}], active_resources={}, checkpoint=Mock(), derived_config=lambda *_:"config", verifier=object(), verification=SimpleNamespace(volume_inventory=lambda _:[], _json_command=command), audit=Mock(side_effect=RuntimeError("readback failed")), call_once=Mock(side_effect=scenarios.CPIRejected("allocation allocation requires reconciliation at VM root creation; no alternate allocation was attempted")))
        with patch.object(transition,"PostRootCapacityHook",return_value=hook), patch.object(vm_faults,"JournalVMFaultProxy",return_value=proxy):
            with self.assertRaisesRegex(RuntimeError,"readback failed"):
                transition.post_root_capacity_rejection(runner,{})
        command.assert_not_called()
        self.assertEqual(runner.constraint_failure_evidence["phase"], "record-readback")
        self.assertEqual(runner.constraint_failure_evidence["modified_status_reads"], 2)
        self.assertEqual(runner.active_resources["capacity_transition"]["allocation_id"], "allocation")


if __name__ == "__main__":
    unittest.main()
