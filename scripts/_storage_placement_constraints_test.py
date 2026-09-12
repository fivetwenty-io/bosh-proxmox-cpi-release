"""Offline outcome tests for production topology and capacity certification."""
from __future__ import annotations

import copy
import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _storage_placement_constraints as constraints
from _storage_placement_scenarios import CPIRejected


def snapshot():
    return {"journal": {"records": [{"ID": "existing", "SHA256": "immutable"}], "evidence": []}, "volumes": [("n1", "p1", "p1:existing", 10)]}


class FakeRunner:
    def __init__(self):
        self.base_config = {}
        self.config_path = "/private/config.json"
        self.policy = {"disk_size_mib": 64, "persistent_storage_ids": ["p1", "p2"], "ephemeral_storage_ids": ["e1", "e2", "e3"]}
        self.verification = SimpleNamespace(cli="/candidate/pve-cid", inventory_pairs={("n1", "p1")})
        self.verifier = SimpleNamespace(_get=Mock())
        self.active_resources = {}
        self.checkpoint = Mock()
        self.call_once = Mock()
        self.derived = []

    def derived_config(self, config, label):
        self.derived.append((copy.deepcopy(config), label))
        return "/private/derived.json"

    def configured_policy(self, e_members=None, p_members=None):
        return {"storage_sets": {"cert-e": {"names": e_members or self.policy["ephemeral_storage_ids"], "strategy": {"name": "spread", "version": 1}},
                                 "cert-p": {"names": p_members or self.policy["persistent_storage_ids"], "strategy": {"name": "spread", "version": 1}}},
                "ephemeral_storage_set": "cert-e", "persistent_storage_set": "cert-p"}

    def vm_arguments(self, identity, properties=None):
        return [identity, "stemcell", properties or {}, {}, [], {}]


def pair(storage, total, available, node="n1"):
    return {"Pair": {"StorageID": storage, "Node": node, "TotalBytes": total, "AvailableBytes": available, "Reason": ""}}


class ConstraintsTests(unittest.TestCase):
    def setUp(self):
        self.runner = FakeRunner()

    def test_malformed_members_raise_bounded_value_error(self):
        for value in ([{}, "p1"], [["nested"], "p1"], "p1", ["p1", "p1"]):
            with self.subTest(value=value), self.assertRaises(ValueError):
                constraints.validate_constraints_manifest({"constraints": {"alias_storage_ids": value}}, {})

    def test_unknown_constraints_and_invalid_node_fixtures_rejected(self):
        for value in ({"hook": "anything"}, {"restricted_storage": {"storage_id": "p1", "allowed_node": "n1", "excluded_node": "n1"}}, {"stale_delay_seconds": 0}):
            with self.subTest(value=value), self.assertRaises(ValueError):
                constraints.validate_constraints_manifest({"constraints": value}, {})

    def test_production_diagnostic_invocation_has_private_request(self):
        def command(arguments, **_kwargs):
            self.assertEqual(arguments[:2], ["/candidate/pve-cid", "storage-plan"])
            path = Path(arguments[arguments.index("--request") + 1])
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(json.loads(path.read_text())["method"], "create_disk")
            return SimpleNamespace(returncode=0, stdout='{"observation_only":true,"targets":[]}')
        with patch.object(constraints.subprocess, "run", side_effect=command):
            report, path = constraints._diagnostic(self.runner, {}, "create_disk", [1, {}, ""], "case")
        self.assertTrue(report["observation_only"])
        self.assertEqual(path, "/private/derived.json")

    def test_invalid_diagnostic_json_never_passes(self):
        with patch.object(constraints.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout="not-json")), self.assertRaises(RuntimeError):
            constraints._diagnostic(self.runner, {}, "create_disk", [1, {}, ""], "case")

    def test_actual_cpi_rejection_reason_is_classified(self):
        self.runner.call_once.side_effect = CPIRejected("missing storage IDs: nonexistent")
        with patch.object(constraints, "_snapshot", side_effect=[snapshot(), snapshot()]), patch.object(constraints, "_diagnostic", return_value=({"observation_only": True, "rejections": ["planning rejected"]}, "private")):
            report = constraints._rejected(self.runner, {}, "create_disk", [1, {}, ""], "missing", {"missing_storage": ("missing storage", "nonexistent")})
        self.assertEqual(report["rejection_class"], "missing_storage")
        self.assertNotIn("missing storage IDs", json.dumps(report))

    def test_typed_diagnostic_classifies_real_bounded_managed_failure(self):
        self.runner.call_once.side_effect = CPIRejected("create_disk: managed allocation failed; inspect structured CPI logs")
        report = {"rejections": ["planning rejected: missing_storage_ids"]}
        with patch.object(constraints, "_snapshot", return_value=snapshot()), patch.object(constraints, "_diagnostic", return_value=(report, "private")):
            result = constraints._rejected(self.runner, {}, "create_disk", [], "missing", {}, "missing_storage_ids")
        self.assertEqual(result["rejection_class"], "missing_storage_ids")

    def test_generic_failure_requires_matching_typed_diagnostic_and_not_auth_error(self):
        for report, reason in (({}, "managed allocation failed; inspect logs"),
                               ({"rejections": ["planning rejected: backing_aliases"]}, "managed allocation failed; inspect logs"),
                               ({"rejections": ["planning rejected: missing_storage_ids"]}, "authentication failed")):
            self.runner.call_once.side_effect = CPIRejected(reason)
            with self.subTest(report=report, reason=reason), patch.object(constraints, "_snapshot", return_value=snapshot()), patch.object(constraints, "_diagnostic", return_value=(report, "private")), self.assertRaises(RuntimeError):
                constraints._rejected(self.runner, {}, "create_disk", [], "missing", {}, "missing_storage_ids")

    def test_nonzero_diagnostic_keeps_only_fixed_classification(self):
        for text, expected in (("storage-plan: diagnostic observation failed", "observation_failed"), ("secret remote diagnostic", "diagnostic_failed")):
            with patch.object(constraints.subprocess, "run", return_value=SimpleNamespace(returncode=1, stdout="", stderr=text)):
                report, _ = constraints._diagnostic(self.runner, {}, "create_disk", [], "negative")
            self.assertEqual(report, {"diagnostic_exit": 1, "failure_class": expected})
            self.assertNotIn(text, json.dumps(report))

    def test_transport_or_auth_error_is_not_policy_rejection(self):
        for error in (RuntimeError("missing storage IDs"), CPIRejected("authentication failed")):
            self.runner.call_once.side_effect = error
            with self.subTest(error=type(error)), patch.object(constraints, "_snapshot", return_value=snapshot()), patch.object(constraints, "_diagnostic", return_value=({}, "private")), self.assertRaises(RuntimeError):
                constraints._rejected(self.runner, {}, "create_disk", [1, {}, ""], "missing", {"missing_storage": ("missing storage",)})

    def test_rejected_case_with_actual_mutation_fails(self):
        self.runner.call_once.side_effect = CPIRejected("missing storage IDs")
        after = snapshot(); after["volumes"].append(("n1", "p1", "p1:leak", 10))
        with patch.object(constraints, "_snapshot", side_effect=[snapshot(), after]), patch.object(constraints, "_diagnostic", return_value=({}, "private")), self.assertRaises(RuntimeError):
            constraints._rejected(self.runner, {}, "create_disk", [1, {}, ""], "missing", {"missing_storage": ("missing storage",)})

    def test_unexpected_success_is_checkpointed_and_cleaned_using_valid_authority(self):
        self.runner.call_once.side_effect = ["owned-cid", None]
        with patch.object(constraints, "_snapshot", return_value=snapshot()), patch.object(constraints, "_diagnostic", return_value=({}, "invalid-policy")), self.assertRaises(RuntimeError):
            constraints._rejected(self.runner, {}, "create_disk", [1, {}, ""], "negative", {"policy": ("missing",)})
        self.assertEqual(self.runner.call_once.call_args_list[1].args, ("delete_disk", ["owned-cid"], self.runner.config_path))
        self.assertEqual(self.runner.checkpoint.call_count, 2)
        self.assertEqual(self.runner.active_resources, {})

    def test_failed_unexpected_cleanup_retains_exact_cid(self):
        self.runner.call_once.side_effect = ["owned-cid", RuntimeError("cleanup failed")]
        with patch.object(constraints, "_snapshot", return_value=snapshot()), patch.object(constraints, "_diagnostic", return_value=({}, "invalid-policy")), self.assertRaises(RuntimeError):
            constraints._rejected(self.runner, {}, "create_disk", [1, {}, ""], "negative", {"policy": ("missing",)})
        self.assertEqual(self.runner.active_resources["constraint_unexpected"]["cid"], "owned-cid")

    def test_constraint_inventory_includes_all_fixture_stores_at_applicable_nodes(self):
        options = {"alias_storage_ids": ["alias1", "alias2"], "domain_storage_ids": ["domain1", "domain2"], "restricted_storage": {"storage_id": "restricted", "allowed_node": "n1", "excluded_node": "n2"}, "unequal_storage_ids": ["small", "large"]}
        stores = ["alias1", "alias2", "domain1", "domain2", "restricted", "small", "large"]
        definitions = [{"storage": value, "nodes": "n1" if value == "restricted" else ""} for value in stores]
        self.runner.verifier._get.side_effect = [definitions, [{"node": "n1"}, {"node": "n2"}]]
        constraints.include_constraint_inventory(self.runner, options)
        for storage in stores:
            self.assertIn(("n1", storage), self.runner.verification.inventory_pairs)
        self.assertNotIn(("n2", "restricted"), self.runner.verification.inventory_pairs)
        self.assertIn(("n2", "alias2"), self.runner.verification.inventory_pairs)

    def test_regex_rounds_assert_actual_frozen_membership_for_both_roles(self):
        def diagnostic(_runner, config, _method, _args, _label):
            memberships = {}
            for name, value in config["storage_sets"].items():
                if "name_pattern" in value:
                    import re
                    candidates = self.runner.policy["persistent_storage_ids"] if name == "cert-p" else self.runner.policy["ephemeral_storage_ids"]
                    memberships[name] = [member for member in candidates if re.fullmatch(value["name_pattern"], member)]
            return {"frozen_membership": memberships}
        with patch.object(constraints, "_positive", side_effect=diagnostic) as positive:
            report = constraints.regex_membership(self.runner, {})
        self.assertEqual(positive.call_count, 6)
        self.assertEqual(report["persistent"][0]["frozen_membership"]["cert-p"], ["p1"])
        self.assertEqual(report["ephemeral"][-1]["frozen_membership"]["cert-e"], ["e3"])

    def test_alias_fixture_must_independently_share_actual_backing(self):
        self.runner.verifier._get.return_value = [{"storage": "alias1", "type": "nfs", "server": "nas", "export": "/one"}, {"storage": "alias2", "type": "nfs", "server": "nas", "export": "/two"}]
        with patch.object(constraints, "_rejected") as rejected, self.assertRaises(RuntimeError):
            constraints.alias_storage(self.runner, {"alias_storage_ids": ["alias1", "alias2"]})
        rejected.assert_not_called()

    def test_rounding_checks_bytes_not_just_selected_storage(self):
        def diagnostic(_runner, _config, _method, args, _label):
            expected = ((args[0] + 1023) // 1024) * constraints.GIB
            return {"targets": [{"Role": "persistent", "VirtualBytes": expected, "ChargeBytes": expected}]}
        with patch.object(constraints, "_positive", side_effect=diagnostic):
            self.assertEqual(len(constraints.capacity_rounding(self.runner, {})["diagnostics"]), 4)
        with patch.object(constraints, "_positive", return_value={"targets": [{"Role": "persistent", "VirtualBytes": 1, "ChargeBytes": 1}]}), self.assertRaises(RuntimeError):
            constraints.capacity_rounding(self.runner, {})

    def test_domain_budget_is_minimum_and_charge_is_bound_to_domain(self):
        self.runner.verifier._get.return_value = [{"storage": store, "type": "nfs", "server": "nas", "export": "/" + store} for store in ("p1", "p2")]
        report = {"domains": [{"Name": "cert-shared-budget", "Members": ["p1", "p2"], "TotalBytes": 100, "AvailableBytes": 30}],
                  "capacities": [pair("p1", 100, 50), pair("p2", 200, 30)], "targets": [{"Role": "persistent", "DomainKey": "cert-shared-budget"}]}
        with patch.object(constraints, "_positive", return_value=report):
            constraints.capacity_domain(self.runner, {"domain_storage_ids": ["p1", "p2"]})
        report["domains"][0]["AvailableBytes"] = 80
        with patch.object(constraints, "_positive", return_value=report), self.assertRaises(RuntimeError):
            constraints.capacity_domain(self.runner, {"domain_storage_ids": ["p1", "p2"]})

    def test_ceiling_request_exceeds_each_real_member_budget(self):
        baseline = {"capacities": [pair("p1", 100 * constraints.GIB, 90 * constraints.GIB), pair("p2", 400 * constraints.GIB, 390 * constraints.GIB)]}
        with patch.object(constraints, "_positive", return_value=baseline), patch.object(constraints, "_rejected", return_value={}) as rejected:
            report = constraints.capacity_ceiling(self.runner, {})
        self.assertGreater(report["requested_mib"] * constraints.MIB, 400 * constraints.GIB // 100)
        self.assertEqual(rejected.call_args.args[1]["storage_sets"]["cert-p"]["max_utilization_pct"], 1)

    def test_failure_keeps_every_remaining_required_row_visible(self):
        cases = [(constraints.CONSTRAINT_IDS[0], None, Mock(side_effect=RuntimeError("failure")))]
        cases += [(name, None, Mock()) for name in constraints.CONSTRAINT_IDS[1:]]
        with patch.object(constraints, "CASES", cases):
            rows = constraints.run_constraint_scenarios({}, self.runner)
        self.assertEqual(len(rows), len(constraints.CONSTRAINT_IDS))
        self.assertEqual(rows[0]["status"], "failed")
        self.assertTrue(all(row["status"] == "missing" for row in rows[1:]))
        self.assertEqual(rows[1]["evidence"]["blocked_by"], constraints.CONSTRAINT_IDS[0])

    def test_stale_case_uses_bounded_cpi_error_only_with_control_and_delay_proof(self):
        bounded = "create_disk: managed allocation failed; inspect the allocation journal and PVE read permissions"
        for error, status, elapsed, control, rejection, succeeds in (
            (CPIRejected(bounded), 200, 2.0, True, "configuration or observation", True),
            (RuntimeError(bounded), 200, 2.0, True, "configuration or observation", False),
            (CPIRejected("authentication failed"), 200, 2.0, True, "configuration or observation", False),
            (CPIRejected(bounded), 503, 2.0, True, "configuration or observation", False),
            (CPIRejected(bounded), 200, 0.2, True, "configuration or observation", False),
            (CPIRejected(bounded), 200, 2.0, False, "configuration or observation", False),
            (CPIRejected(bounded), 200, 2.0, True, "NoCapacity", False),
        ):
            proxy = Mock()
            proxy.__enter__ = Mock(return_value=proxy);proxy.__exit__ = Mock(return_value=False)
            proxy.config_override = {};proxy.delayed_reads = 2;proxy.evidence = {"blocked_mutations":0};proxy.reads=[]
            entry = {"inventory":True,"delayed":True,"delay_seconds":elapsed,"status":status,"inventory_payload_valid":True}
            calls=[]
            def diagnostic(*args):
                calls.append(args[-1])
                if len(calls)==1:return {"observation_only":True,"targets":[{}] if control else []},"control"
                proxy.reads.append(entry)
                return {"observation_only":True,"rejections":["planning rejected: "+rejection]},"private"
            def actual(*args):
                proxy.reads.append(entry)
                raise error
            self.runner.call_once.side_effect=actual
            with self.subTest(error=type(error),status=status,elapsed=elapsed,control=control,rejection=rejection),patch.object(constraints,"DelayedInventoryProxy",return_value=proxy),patch.object(constraints,"_snapshot",return_value=snapshot()),patch.object(constraints,"_diagnostic",side_effect=diagnostic):
                if succeeds:
                    report=constraints.stale_observation(self.runner,{})
                    self.assertTrue(report['control']['targets']);self.assertEqual(report['create_reads'],[entry])
                else:
                    with self.assertRaises(RuntimeError):constraints.stale_observation(self.runner,{})

    def test_delay_proxy_retains_upstream_status_before_delivering_response(self):
        proxy=constraints.DelayedInventoryProxy({"storage_placement_namespace":"test"},{"p1"},2)
        proxy.release=Mock()
        handler=Mock();handler.command="GET";handler.path="/api2/json/nodes/node/storage"
        original=handler.send_response
        def forward(actual):
            actual.send_response(200)
            actual.end_headers()
            actual.wfile.write(b'{"data":[{"storage":"p1","active":1,"enabled":1,"total":100,"avail":90}]}')
            self.assertEqual(proxy.reads,[{"inventory":True,"delayed":True,"delay_seconds":2,"status":200,"inventory_payload_valid":True}])
        with patch.object(constraints.time,"monotonic",side_effect=[0,2]),patch.object(constraints.LoopbackPVEFaultProxy,"forward",side_effect=forward):proxy.forward(handler)
        self.assertIs(handler.send_response,original);original.assert_called_once_with(200)
        constraints._stale_phase_proof(proxy.reads,1)

    def test_delay_proxy_records_unavailable_upstream_without_classifying_it_stale(self):
        proxy=constraints.DelayedInventoryProxy({"storage_placement_namespace":"test"},{"p1"},2);proxy.release=Mock()
        handler=Mock();handler.command="GET";handler.path="/api2/json/nodes/node/storage"
        with patch.object(constraints.time,"monotonic",side_effect=[0,2]),patch.object(constraints.LoopbackPVEFaultProxy,"forward",side_effect=RuntimeError("unavailable")):
            with self.assertRaises(RuntimeError):proxy.forward(handler)
        with self.assertRaisesRegex(RuntimeError,"failed upstream"):constraints._stale_phase_proof(proxy.reads,1)

    def test_http_success_with_bad_inventory_is_not_stale_proof(self):
        for payload in [b'not json',b'{"data":null}',b'{"data":[]}',b'{"data":[{"storage":"p1","active":0,"enabled":1,"total":100,"avail":90}]}']:
            self.assertFalse(constraints._valid_capacity_payload(payload,{"p1"}))
        with self.assertRaises(RuntimeError):constraints._stale_phase_proof([{"inventory":True,"delayed":True,"delay_seconds":2,"status":200,"inventory_payload_valid":False}],1)

    def test_stale_unexpected_disk_is_preserved_without_replay_or_delete(self):
        proxy=Mock();proxy.__enter__=Mock(return_value=proxy);proxy.__exit__=Mock(return_value=False);proxy.config_override={};proxy.evidence={"blocked_mutations":0};proxy.reads=[]
        def diagnostic(*args):
            if args[-1]=='stale-control':return {"targets":[{}]},'control'
            proxy.reads.append({"inventory":True,"delayed":True,"delay_seconds":2,"status":200,"inventory_payload_valid":True})
            return {"observation_only":True,"rejections":["planning rejected: configuration or observation"]},'stale'
        self.runner.call_once.return_value='unexpected-cid'
        with patch.object(constraints,'DelayedInventoryProxy',return_value=proxy),patch.object(constraints,'_snapshot',return_value=snapshot()),patch.object(constraints,'_diagnostic',side_effect=diagnostic):
            with self.assertRaisesRegex(RuntimeError,'unexpectedly allocated'):constraints.stale_observation(self.runner,{})
        self.assertEqual(self.runner.active_resources['constraint_unexpected'],{'method':'create_disk','cid':'unexpected-cid'})
        self.runner.call_once.assert_called_once();self.runner.checkpoint.assert_called_once()

    def test_delay_proxy_never_forwards_mutations(self):
        proxy = constraints.DelayedInventoryProxy({"storage_placement_namespace": "test"}, {"p1"}, 2)
        handler = SimpleNamespace(command="POST", send_error=Mock())
        with patch.object(constraints.LoopbackPVEFaultProxy, "forward") as forward:
            proxy.forward(handler)
        forward.assert_not_called()
        handler.send_error.assert_called_once_with(503)
        self.assertEqual(proxy.evidence["blocked_mutations"], 1)

    def test_positive_diagnostic_state_change_is_not_observation_only(self):
        after = snapshot(); after["journal"]["records"][0]["SHA256"] = "changed"
        with patch.object(constraints, "_snapshot", side_effect=[snapshot(), after]), patch.object(constraints, "_diagnostic", return_value=({"targets": [{"Role": "persistent"}]}, "private")), self.assertRaises(RuntimeError):
            constraints._positive(self.runner, {}, "create_disk", [1, {}, ""], "positive")


if __name__ == "__main__":
    unittest.main()
