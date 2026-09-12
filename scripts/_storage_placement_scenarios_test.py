"""Offline certification-runner regressions; no PVE or guest execution."""
import base64
import copy
import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

import _storage_placement_scenarios as scenarios


class EmptyCIDSerializationTests(unittest.TestCase):
    def verify(self, payload, summary_cid="", include_summary=True):
        text = json.dumps(payload, separators=(",", ":"))
        envelope = {"version": 1, "sha256": hashlib.sha256(text.encode()).hexdigest(), "payload": payload}
        summary = {"ID": payload["id"], "SHA256": hashlib.sha256(scenarios.go_canonical_json(payload).encode()).hexdigest()}
        if include_summary:
            summary["CID"] = summary_cid
        return scenarios.verified_record_payload(json.dumps(envelope, separators=(",", ":")).encode(), summary)

    def test_go_omitted_cid_matches_explicit_empty_summary(self):
        payload = {"id": "11111111-1111-4111-8111-111111111111", "state": "reconciliation_required"}
        self.assertEqual(self.verify(payload), payload)
        self.assertNotIn("cid", payload)
        self.assertEqual(self.verify(dict(payload, cid=""))["cid"], "")

    def test_only_omission_normalizes_not_null_or_malformed_cids(self):
        for cid in (None, False, 0, [], {}):
            with self.subTest(cid=cid), self.assertRaisesRegex(RuntimeError, "identity"):
                self.verify({"id": "allocation", "cid": cid})
        for cid in (None, False, 0, [], {}, "wrong"):
            with self.subTest(summary=cid), self.assertRaisesRegex(RuntimeError, "identity"):
                self.verify({"id": "allocation"}, cid)
        with self.assertRaisesRegex(RuntimeError, "identity"):
            self.verify({"id": "allocation"}, include_summary=False)
        with self.assertRaisesRegex(RuntimeError, "identity"):
            self.verify({"id": "allocation", "cid": "100"}, "101")


def fixture():
    return {"version": 1, "policy": {
        "stemcell_cid": "fixture-stemcell", "vm_cloud_properties": {"memory": 1024, "cores": 1},
        "networks": {}, "environment": {}, "ephemeral_size_mib": 1024, "disk_size_mib": 1024,
        "ephemeral_storage_ids": ["e1", "e2", "e3"], "persistent_storage_ids": ["p1", "p2"],
        "root_storage_ids": ["r1"], "fixed_iso_storage_id": "iso",
        "encrypted_persistent_storage_ids": ["p1"], "escaping_tier_criteria": {"types": ["lvmthin"]}}}


def config():
    return {"host": "pve.invalid", "node": "node1", "user": "test", "password": "NEVER-REPORT-SECRET",
            "vm_storage": "e1", "disk_storage": "p1", "storage_placement_namespace": "test-namespace",
            "storage_allocation_journal_dir": "/private/fixture/authority"}


def bare_runner():
    runner = scenarios.ScenarioRunner.__new__(scenarios.ScenarioRunner)
    runner.base_config, runner.policy = config(), fixture()["policy"]
    runner.active_resources = {}
    runner.report = {"rows": []}
    runner.checkpoint = Mock()
    return runner


class ScenarioTests(unittest.TestCase):
    def test_vm_agent_ids_are_unique_bounded_hostnames(self):
        runner = bare_runner()
        labels = list(scenarios.POLICY_IDS) + ["fault-post-create-response-loss", "ha-constraint", "batch-3", "long-label-" * 100]
        identities = set()
        for label in labels:
            for _ in range(4):
                args = runner.vm_arguments(label)
                identity = args[0]
                self.assertLessEqual(len(identity.encode("ascii")), 63)
                self.assertRegex(identity, r"^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$")
                self.assertNotIn(identity, identities)
                identities.add(identity)
                self.assertEqual(args[1], runner.policy["stemcell_cid"])
                self.assertEqual(args[3], runner.policy["networks"])
        self.assertEqual(len(identities), 4 * len(labels))

    def test_manifest_rejects_typos_null_boolean_sizes_and_authority_omission(self):
        self.assertEqual(scenarios.validate_manifest(fixture(), config()), fixture()["policy"])
        for field, value in (("disk_size_mib", True), ("root_storage_ids", []), ("environment", None),
                             ("encrypted_persistent_storage_ids", ["outside"])):
            with self.subTest(field=field):
                manifest = fixture()
                manifest["policy"][field] = value
                with self.assertRaises(scenarios.ScenarioFailure):
                    scenarios.validate_manifest(manifest, config())
        manifest = fixture()
        manifest["policy"]["disk_size_mb"] = 64
        with self.assertRaises(scenarios.ScenarioFailure):
            scenarios.validate_manifest(manifest, config())
        with self.assertRaises(scenarios.ScenarioFailure):
            scenarios.validate_manifest(fixture(), {})

    def test_focused_singleton_p_accepts_four_e_without_encryption_or_split_root(self):
        manifest = fixture()
        manifest["policy"].update(persistent_storage_ids=["p1"],
                                  ephemeral_storage_ids=["e1", "e2", "e3", "e4"],
                                  encrypted_persistent_storage_ids=[], root_storage_ids=[])
        cases = [f"policy-{strategy}-psingleton-eplural" for strategy in scenarios.STRATEGIES]
        self.assertEqual(scenarios.validate_manifest(manifest, config(), cases), manifest["policy"])
        for selected in (None, ["policy-spread-pplural-eplural"],
                         ["selector-subset-encryption"], ["root-split-follow-iso"]):
            with self.subTest(selected=selected), self.assertRaises(scenarios.ScenarioFailure):
                scenarios.validate_manifest(manifest, config(), selected)
        manifest["policy"]["ephemeral_storage_ids"] = ["e1"]
        with self.assertRaises(scenarios.ScenarioFailure):
            scenarios.validate_manifest(manifest, config(), cases)
        scenarios.validate_manifest(manifest, config(), ["policy-spread-psingleton-esingleton"])
        manifest["policy"]["encrypted_persistent_storage_ids"] = ["outside"]
        with self.assertRaises(scenarios.ScenarioFailure):
            scenarios.validate_manifest(manifest, config(), ["policy-spread-psingleton-esingleton"])

    def test_focused_cli_validation_preserves_full_suite_and_selection_boundaries(self):
        manifest = fixture()
        manifest["policy"].update(persistent_storage_ids=["p1"],
                                  encrypted_persistent_storage_ids=[], root_storage_ids=[])
        with tempfile.TemporaryDirectory() as directory:
            cfg, fixtures, report = [Path(directory) / name for name in ("config", "fixtures", "report")]
            cfg.write_text(json.dumps(config()))
            fixtures.write_text(json.dumps(manifest))
            arguments = ["--config", str(cfg), "--manifest", str(fixtures), "--report", str(report),
                         "--cpi-bin", "absent", "--validate-only"]
            selected = ["--case", "policy-spread-psingleton-eplural"]
            with patch.object(scenarios, "ScenarioRunner", side_effect=AssertionError("live runner")):
                self.assertEqual(scenarios.main(arguments + selected), 0)
                self.assertFalse(json.loads(report.read_text())["complete_release_matrix"])
                self.assertEqual(scenarios.main(arguments), 1)
                self.assertEqual(scenarios.main(arguments + selected + ["--suite", "all"]), 1)
                self.assertEqual(scenarios.main(arguments + selected + selected), 1)

    def test_constraint_and_recovery_cli_require_only_their_actual_topology(self):
        with tempfile.TemporaryDirectory() as directory:
            cfg, fixtures, report = [Path(directory) / name for name in ("config", "fixtures", "report")]
            cfg.write_text(json.dumps(config()))
            arguments = ["--config", str(cfg), "--manifest", str(fixtures), "--report", str(report),
                         "--cpi-bin", "absent", "--validate-only"]
            for suite, minimum in (("constraints", 2), ("recovery", 1)):
                manifest = fixture()
                manifest["policy"].update(ephemeral_storage_ids=["e1", "e2"][:minimum],
                                          persistent_storage_ids=["p1", "p2"][:minimum],
                                          encrypted_persistent_storage_ids=[], root_storage_ids=[])
                fixtures.write_text(json.dumps(manifest))
                with self.subTest(suite=suite), patch.object(scenarios, "ScenarioRunner", side_effect=AssertionError("live runner")):
                    self.assertEqual(scenarios.main(arguments + ["--suite", suite]), 0)
                    self.assertFalse(json.loads(report.read_text())["complete_release_matrix"])
                    self.assertEqual(scenarios.main(arguments + ["--suite", "all"]), 1)
                    self.assertEqual(scenarios.main(arguments + ["--suite", suite, "--case", "policy-spread-psingleton-esingleton"]), 1)
                    manifest["recovery"] = {"unknown": "must still validate"}
                    fixtures.write_text(json.dumps(manifest))
                    self.assertEqual(scenarios.main(arguments + ["--suite", suite]), 1)
                    manifest.pop("recovery")
                    manifest["policy"]["persistent_storage_ids"] = manifest["policy"]["persistent_storage_ids"][:-1]
                    fixtures.write_text(json.dumps(manifest))
                    self.assertEqual(scenarios.main(arguments + ["--suite", suite]), 1)

    def test_faults_cli_accepts_singletons_but_keeps_all_fixture_validation(self):
        from _storage_placement_faults_test import fixture as backend_fixture
        with tempfile.TemporaryDirectory() as directory:
            cfg, fixtures, report = [Path(directory) / name for name in ("config", "fixtures", "report")]
            cfg.write_text(json.dumps(config()))
            value = fixture()
            value["policy"].update(ephemeral_storage_ids=["e1"], persistent_storage_ids=["p1"],
                                   root_storage_ids=[], encrypted_persistent_storage_ids=[])
            value["faults"] = {"namespace": config()["storage_placement_namespace"], "backends": [backend_fixture()],
                                "proxy_storage_id": "p1", "vm_phases": ["root", "ephemeral", "iso", "ha"]}
            args = ["--config", str(cfg), "--manifest", str(fixtures), "--report", str(report),
                    "--cpi-bin", "absent", "--suite", "faults", "--validate-only"]
            with patch.object(scenarios, "ScenarioRunner", side_effect=AssertionError("live execution")):
                fixtures.write_text(json.dumps(value))
                self.assertEqual(scenarios.main(args), 0)
                self.assertFalse(json.loads(report.read_text())["complete_release_matrix"])
                with patch("_storage_placement_faults.restore_backend_fixture", return_value={"restored": True}) as restore:
                    self.assertEqual(scenarios.main(args[:-1] + ["--restore-backend", "p1", "--fault-run-id", "a" * 32]), 0)
                restore.assert_called_once()
                self.assertTrue(json.loads(report.read_text())["restoration_only"])
                for change in (
                    lambda v: v["policy"].update(ephemeral_storage_ids=[]),
                    lambda v: v["policy"].update(persistent_storage_ids=[]),
                    lambda v: v["policy"].pop("root_storage_ids"),
                    lambda v: v["faults"]["backends"][0].pop("filesystem_uuid"),
                    lambda v: v["faults"].update(vm_phases=["not-a-phase"]),
                    lambda v: v.update(recovery={"unknown": True}),
                    lambda v: v.update(constraints={"unknown": True}),
                ):
                    changed = copy.deepcopy(value)
                    change(changed)
                    fixtures.write_text(json.dumps(changed))
                    self.assertEqual(scenarios.main(args), 1)

    def test_checkpoint_during_second_policy_case_retains_completed_first_case(self):
        runner = bare_runner()
        runner.checkpoint = scenarios.ScenarioRunner.checkpoint.__get__(runner)
        with tempfile.TemporaryDirectory() as directory:
            runner.report_path = Path(directory) / "report"
            def lifecycle(identifier, **options):
                if identifier == scenarios.POLICY_IDS[1]:
                    runner.active_resources = {"vmid": "8011", "pending_operation": "create_disk"}
                    runner.checkpoint()
                    raise KeyboardInterrupt("simulated interruption")
                return {"verified": True}
            runner.lifecycle = lifecycle
            with self.assertRaises(KeyboardInterrupt):
                runner.run_policy(scenarios.POLICY_IDS[:2])
            saved = json.loads(runner.report_path.read_text())
            self.assertEqual([row["scenario_id"] for row in saved["rows"]], [scenarios.POLICY_IDS[0]])
            self.assertEqual(saved["rows"][0]["status"], "passed")
            self.assertEqual(saved["retained_resources"], runner.active_resources)

    def test_main_does_not_append_checkpointed_policy_rows_twice(self):
        runner = bare_runner()
        runner.checkpoint = scenarios.ScenarioRunner.checkpoint.__get__(runner)
        runner.lifecycle = Mock(return_value={"verified": True})
        runner.preflight = Mock()
        runner.close = Mock()
        with tempfile.TemporaryDirectory() as directory:
            cfg, manifest, report = [Path(directory) / name for name in ("config", "manifest", "report")]
            cfg.write_text(json.dumps(config()))
            manifest.write_text(json.dumps(fixture()))
            runner.report_path = report
            args = ["--config", str(cfg), "--manifest", str(manifest), "--report", str(report),
                    "--cpi-bin", "unused", "--case", scenarios.POLICY_IDS[0], "--case", scenarios.POLICY_IDS[1]]
            with patch.object(scenarios, "ScenarioRunner", return_value=runner):
                self.assertEqual(scenarios.main(args), 0)
            rows = json.loads(report.read_text())["rows"]
            self.assertEqual([row["scenario_id"] for row in rows], scenarios.POLICY_IDS[:2])

    def test_focused_runner_preflight_observes_singleton_p_with_both_nodes(self):
        manifest = fixture()
        manifest["policy"].update(persistent_storage_ids=["p1"],
                                  encrypted_persistent_storage_ids=[], root_storage_ids=[])
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cfg, binary = root / "config", root / "cpi"
            cfg.write_text(json.dumps(config()))
            binary.write_bytes(b"offline-cpi")
            (root / "pve-cid").write_bytes(b"offline-diagnostic")
            runner = scenarios.ScenarioRunner(str(cfg), manifest, str(binary), str(root / "report"),
                                              cases=["policy-spread-psingleton-eplural"])
            self.addCleanup(runner.close)
            runner.audit = Mock(return_value={})
            stores = ["e1", "e2", "e3", "p1"]
            runner.diagnostic = Mock(return_value={
                "observation_only": True, "targets": [{"role": "root", "storage_id": "e1"}],
                "frozen_membership": {"cert-e": stores[:3], "cert-p": ["p1"]},
                "capacities": [{"Pair": {"Node": node, "StorageID": store,
                                            "BackingKey": "nfs://server/" + store}}
                               for node in ("n1", "n2") for store in stores]})
            runner.verifier = Mock()
            runner.verifier.cluster_nodes.return_value = ["n1", "n2"]
            runner.verifier.storage_entry.return_value = {}
            runner.verification.volume_inventory = Mock()
            runner.preflight()
            self.assertEqual(runner.report["topology"]["members"]["persistent"], ["p1"])
            self.assertEqual(runner.report["topology"]["nodes"], ["n1", "n2"])
            self.assertFalse(runner.report["complete_release_matrix"])
            runner.verification.volume_inventory.assert_called_once_with(runner.verifier)

            # Exercise the real preflight-to-backend consumer, rather than
            # manually populating verification.diagnostic in a fault fixture.
            import _storage_placement_faults as faults
            self.assertEqual(runner.verification.diagnostic, runner.diagnostic.return_value)
            self.assertEqual(runner.report["planning"], runner.diagnostic.return_value)
            self.assertIsNot(runner.verification.diagnostic, runner.diagnostic.return_value)
            backend = {"storage_id": "p1", "nfs_server": "server", "mount_path": "/p1"}
            controller = Mock()
            controller.invoke.side_effect = RuntimeError("inspection reached without fault application")
            with patch.object(faults, "BackendController", return_value=controller) as constructor, patch.object(faults, "_snapshot", return_value={}):
                with self.assertRaisesRegex(RuntimeError, "inspection reached"):
                    faults._backend_case(runner, backend, "test-namespace", "backend_export_outage", 64, {})
                controller.invoke.assert_called_once_with("inspect")
                constructor.reset_mock()
                for bad in (dict(backend, nfs_server="foreign"), dict(backend, mount_path="/other")):
                    with self.subTest(backend=bad), self.assertRaisesRegex(RuntimeError, "declared export"):
                        faults._backend_case(runner, bad, "test-namespace", "backend_export_outage", 64, {})
                constructor.assert_not_called()
            runner.call_once = Mock(side_effect=AssertionError("unexpected mutation"))
            runner.diagnostic.return_value = {"observation_only": False}
            with self.assertRaisesRegex(RuntimeError, "feasible observation-only"):
                runner.preflight()
            self.assertNotEqual(runner.report["planning"], runner.diagnostic.return_value)
            runner.call_once.assert_not_called()

    def test_config_copies_policy_without_moving_authority_or_retention_field(self):
        runner = bare_runner()
        original = copy.deepcopy(runner.base_config)
        generated = runner.configured_policy(root_members=["r1"], e_strategy="least_utilized", p_strategy="weighted_free_space", iso="fixed")
        self.assertEqual(runner.base_config, original)
        self.assertNotIn("retain_ephemeral_on_delete", generated)
        self.assertEqual(generated["storage_sets"]["cert-r"]["names"], ["r1"])
        self.assertFalse(generated["iso_storage_follow_vm_storage"])
        self.assertEqual(generated["storage_sets"]["cert-p"]["strategy"]["name"], "weighted_free_space")
        runner.policy["vm_cloud_properties"]["retain_ephemeral_on_delete"] = True
        args = runner.vm_arguments("retention")
        self.assertFalse(args[2]["retain_ephemeral_on_delete"])
        runner.policy["vm_cloud_properties"]["tags"] = {"bosh-retain-ephemeral": "true"}
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.vm_arguments("retention")

    def test_guest_mapping_requires_independent_mount_evidence(self):
        rows = [{"mountpoint": "/", "disk": [{"dev": "/dev/vda1"}]},
                {"mountpoint": "/var/vcap/data", "disk": [{"dev": "/dev/sdb1"}]}]
        self.assertEqual(scenarios.guest_disk_mapping({"result": rows}, True)["/"], ["/dev/vda"])
        with self.assertRaises(scenarios.ScenarioFailure):
            scenarios.guest_disk_mapping(rows, False)
        rows[1]["disk"][0]["dev"] = "/dev/vda3"
        scenarios.guest_disk_mapping(rows, False)
        with self.assertRaises(scenarios.ScenarioFailure):
            scenarios.guest_disk_mapping(rows[:1], False)
        rows[1]["disk"][0]["dev"] = "/dev/mapper/data"
        with self.assertRaises(scenarios.ScenarioFailure):
            scenarios.guest_disk_mapping(rows, False)

    def test_authority_uses_real_namespace_hash_and_rejects_replacement(self):
        runner = bare_runner()
        runner._authority = None
        with tempfile.TemporaryDirectory() as directory:
            runner.base_config["storage_allocation_journal_dir"] = directory
            path = Path(directory) / hashlib.sha256(b"test-namespace").hexdigest() / "authority.json"
            path.parent.mkdir()
            path.write_text("enrolled")
            runner.assert_authority()
            path.write_text("replacement")
            with self.assertRaises(scenarios.ScenarioFailure):
                runner.assert_authority()

    def test_empty_real_cli_summary_is_normalized_but_missing_rejected(self):
        runner = bare_runner()
        runner.cpi_bin, runner.config_path = "unused", "unused"
        runner.assert_authority = Mock()
        envelope = {"generation_index_healthy": True, "cluster_continuity": True,
                    "audit": {"complete": True, "vm_scan_complete": True}, "records": None}
        runner.verification = SimpleNamespace(_json_command=Mock(return_value=envelope))
        self.assertEqual(runner.audit()["records"], [])
        envelope.pop("records")
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.audit()

    def test_atomic_report_does_not_follow_existing_symlink(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "target"
            target.write_text("keep")
            report = Path(directory) / "report"
            report.symlink_to(target)
            scenarios.write_report(report, {"rows": []})
            self.assertEqual(target.read_text(), "keep")
            self.assertFalse(report.is_symlink())
            self.assertEqual(report.stat().st_mode & 0o777, 0o600)

    def test_validation_only_never_constructs_client_or_runs_subprocess(self):
        with tempfile.TemporaryDirectory() as directory:
            paths = [Path(directory) / name for name in ("config", "manifest", "report")]
            paths[0].write_text(json.dumps(config()))
            paths[1].write_text(json.dumps(fixture()))
            with patch.object(scenarios, "ScenarioRunner", side_effect=AssertionError("live runner")), patch.object(scenarios.subprocess, "run", side_effect=AssertionError("subprocess")):
                code = scenarios.main(["--config", str(paths[0]), "--manifest", str(paths[1]), "--report", str(paths[2]), "--cpi-bin", "not-present", "--validate-only"])
            self.assertEqual(code, 0)
            report = json.loads(paths[2].read_text())
            self.assertTrue(report["manifest_valid"])
            self.assertFalse(report["complete_release_matrix"])
            self.assertFalse(report["generated_config_validated"])
            self.assertEqual(report["rows"], [])

    def test_all_suite_failure_records_blocked_rows_and_validates_other_declared_sections(self):
        with tempfile.TemporaryDirectory() as directory:
            paths = [Path(directory) / name for name in ("config", "manifest", "report")]
            paths[0].write_text(json.dumps(config()))
            paths[1].write_text(json.dumps(fixture()))
            runner = bare_runner()
            runner.report = {"rows": [], "complete_release_matrix": False}
            runner.preflight = Mock()
            runner.close = Mock()
            runner.run_policy = Mock(return_value=[{"scenario_id": "policy-failure", "status": "failed", "evidence": {}}])
            arguments = ["--config", str(paths[0]), "--manifest", str(paths[1]), "--report", str(paths[2]), "--cpi-bin", "unused", "--suite", "all"]
            with patch.object(scenarios, "ScenarioRunner", return_value=runner):
                self.assertEqual(scenarios.main(arguments), 1)
            rows = json.loads(paths[2].read_text())["rows"]
            from _storage_placement_constraints import CONSTRAINT_IDS
            self.assertEqual(len(rows), 30 + len(CONSTRAINT_IDS))
            self.assertTrue(all(row["status"] == "missing" for row in rows[1:]))
            manifest = fixture()
            manifest["faults"] = {"typo": True}
            paths[1].write_text(json.dumps(manifest))
            arguments[-1] = "policy"
            arguments += ["--validate-only"]
            with patch.object(scenarios, "ScenarioRunner", side_effect=AssertionError("live execution")):
                self.assertEqual(scenarios.main(arguments), 1)

    def test_restore_branch_never_runs_scenarios(self):
        with tempfile.TemporaryDirectory() as directory:
            paths = [Path(directory) / name for name in ("config", "manifest", "report")]
            paths[0].write_text(json.dumps(config()))
            paths[1].write_text(json.dumps(fixture()))
            arguments = ["--config", str(paths[0]), "--manifest", str(paths[1]), "--report", str(paths[2]), "--cpi-bin", "unused", "--restore-backend", "p1", "--fault-run-id", "a" * 32]
            with patch.object(scenarios, "ScenarioRunner", side_effect=AssertionError("live scenarios")), patch("_storage_placement_faults.restore_backend_fixture", return_value={"restored": True}) as restore:
                self.assertEqual(scenarios.main(arguments), 0)
            restore.assert_called_once()
            self.assertTrue(json.loads(paths[2].read_text())["restoration_only"])

    def test_matrix_runs_every_case_and_blocks_after_failure_without_secret_output(self):
        runner = bare_runner()
        runner.lifecycle = Mock(return_value={"verified": True})
        runner.selector_case = Mock(return_value={"verified": True})
        with tempfile.TemporaryDirectory() as directory:
            runner.report_path = Path(directory) / "report"
            rows = runner.run_policy()
            self.assertEqual([row["scenario_id"] for row in rows], scenarios.POLICY_IDS)
            self.assertTrue(all(row["status"] == "passed" for row in rows))
            calls = runner.lifecycle.call_args_list
            self.assertEqual(len(calls), 17)
            self.assertTrue(any(call.kwargs.get("remove_membership") for call in calls))
            self.assertTrue(any(call.kwargs.get("change_strategy") for call in calls))
            runner.lifecycle.side_effect = scenarios.CPIRejected("NEVER-REPORT-SECRET")
            rows = runner.run_policy()
            self.assertEqual(rows[0]["status"], "failed")
            self.assertEqual(len(rows), 18)
            self.assertTrue(all(row["status"] == "missing" for row in rows[1:]))
            self.assertNotIn("NEVER-REPORT-SECRET", json.dumps(rows))

    def test_mechanism_fixture_can_select_one_compatible_case_without_claiming_other_rows(self):
        runner = bare_runner()
        runner.lifecycle = Mock(return_value={"verified": True})
        runner.selector_case = Mock(return_value={"verified": True})
        with tempfile.TemporaryDirectory() as directory:
            runner.report_path = Path(directory) / "report"
            rows = runner.run_policy([scenarios.POLICY_IDS[0]])
        self.assertEqual([row["scenario_id"] for row in rows], [scenarios.POLICY_IDS[0]])
        self.assertEqual(runner.lifecycle.call_count, 1)
        manifest = fixture()
        manifest["policy"]["expected_root_mechanism"] = "linked_clone"
        scenarios.validate_manifest(manifest, config())
        manifest["policy"]["expected_root_mechanism"] = "guessed"
        with self.assertRaises(scenarios.ScenarioFailure):
            scenarios.validate_manifest(manifest, config())

    def test_vm_assertions_reject_wrong_iso_layout_and_guest_disk_sizes(self):
        runner = bare_runner()
        generated = runner.configured_policy()
        values = {"virtio0": "e1:root", "scsi1": "e1:ephemeral", "ide2": "e1:iso,media=cdrom"}
        runner.verifier = SimpleNamespace(qemu_config=lambda _: values)
        runner.observed_volume = lambda volume: {"volid": volume, "size": 10 * 1024**2}
        runner.observe_guest_mapping = Mock(return_value={"/": ["/dev/vda"], "/var/vcap/data": ["/dev/sdb"]})
        runner.observed_vm("100", generated, True)
        values["ide2"] = "iso:wrong,media=cdrom"
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.observed_vm("100", generated, True)
        values["ide2"] = "e1:iso,media=cdrom"
        values["scsi1"] = "e2:ephemeral"
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.observed_vm("100", generated, True)
        values["scsi1"] = "e1:ephemeral"
        values["unused0"] = "e1:unplanned"
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.observed_vm("100", generated, True)

    def test_disk_uuid_token_backing_and_renamed_volume_are_observed(self):
        runner = bare_runner()
        token = "bpd-0123456789abcdef"
        cid = "pvd-" + base64.urlsafe_b64encode(json.dumps({"v": "p1:birth", "m": {"id": token}}).encode()).decode().rstrip("=")
        allocation = "11111111-1111-4111-8111-111111111111"
        envelope = {"records": [{"CID": cid, "Kind": "disk", "ID": allocation}],
                    "audit": {"evidence": [{"allocation_id": allocation, "volume_id": "p1:renamed"}]}}
        runner.audit = Mock(return_value=envelope)
        runner.observed_volume = lambda volume: {"volid": volume, "size": 1024**3}
        runner.verifier = SimpleNamespace(storage_entry=lambda _: {"type": "nfs", "server": "nas", "export": "/p1"},
                                         disk_holders=lambda _: [(200, "scsi0")],
                                         qemu_config=lambda _: {"scsi0": "p1:renamed,serial=" + token})
        observed = runner.observed_disk(cid, "config", members=["p1"], size_mib=1)
        self.assertEqual(observed["volume_id"], "p1:renamed")
        self.assertEqual(observed["allocation_uuid"], allocation)
        previous = copy.deepcopy(observed)
        previous["backing"]["export"] = "/different"
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.observed_disk(cid, "config", previous)
        runner.verifier.qemu_config = lambda _: {"scsi0": "p1:renamed,serial=bpd-0000000000000000"}
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.observed_disk(cid, "config")

    def test_actual_root_execution_requires_record_checksum_source_and_completed_task(self):
        runner = bare_runner()
        generated = runner.configured_policy()
        allocation = "11111111-1111-4111-8111-111111111111"
        task = {"status": "stopped", "exitstatus": "OK"}
        def get(path):
            return task if "/tasks/" in path else {"size": 1024**3, "format": "qcow2"}
        runner.verifier = SimpleNamespace(_get=get)
        root = {"Role": "root", "Mechanism": "import", "Node": "node1", "StorageID": "e1", "BackingKey": "nfs://nas/e1",
                "Source": {"Node": "node1", "StorageID": "images", "VolumeID": "images:import/root.qcow2", "TemplateVMID": 0, "VirtualBytes": 1024**3}}
        record = {"id": allocation, "cid": "100", "intent": {"plan": {"Targets": [root]}},
                  "steps": [{"id": "root-mutation", "kind": "vm.QEMU.Create", "state": "observed", "upid": "UPID:node1:1234:", "volids": ["e1:root"]}]}
        payload = json.dumps(record, separators=(",", ":"))
        summary = {"ID": allocation, "CID": "100", "SHA256": hashlib.sha256(scenarios.go_canonical_json(record).encode()).hexdigest()}
        envelope = {"version": 1, "sha256": hashlib.sha256(payload.encode()).hexdigest(), "payload": record}
        with tempfile.TemporaryDirectory() as directory:
            runner.base_config["storage_allocation_journal_dir"] = directory
            record_path = Path(directory) / hashlib.sha256(b"test-namespace").hexdigest() / ("allocation-" + allocation + ".json")
            record_path.parent.mkdir()
            record_path.write_text(json.dumps(envelope, separators=(",", ":")))
            observed = runner.observed_root_mechanism(summary, generated)
            self.assertEqual(observed["mechanism"], "import")
            self.assertEqual(observed["source"]["VolumeID"], "images:import/root.qcow2")
            task["status"] = "running"
            with self.assertRaises(scenarios.ScenarioFailure):
                runner.observed_root_mechanism(summary, generated)
            task["status"] = "stopped"
            runner.policy["expected_root_mechanism"] = "full_clone"
            with self.assertRaises(scenarios.ScenarioFailure):
                runner.observed_root_mechanism(summary, generated)
            runner.policy.pop("expected_root_mechanism")
            changed = dict(summary, SHA256="0" * 64)
            with self.assertRaises(scenarios.ScenarioFailure):
                runner.observed_root_mechanism(changed, generated)
            envelope["sha256"] = "0" * 64
            record_path.write_text(json.dumps(envelope, separators=(",", ":")))
            with self.assertRaises(scenarios.ScenarioFailure):
                runner.observed_root_mechanism(summary, generated)

    def test_envelope_rejects_duplicate_fields_boolean_version_and_nonfinite_payload(self):
        for raw in (b'{"version":1,"version":1,"sha256":"x","payload":{}}',
                    b'{"version":true,"sha256":"x","payload":{}}',
                    b'{"version":1,"sha256":"x","payload":{"id":1,"id":2}}',
                    b'{"version":1,"sha256":"x","payload":{"value":NaN}}'):
            with self.subTest(raw=raw), self.assertRaises(scenarios.ScenarioFailure):
                scenarios.verified_record_envelope(raw)

    def test_failed_vm_creation_keeps_agent_identity_before_submission(self):
        runner = bare_runner()
        runner.derived_config = lambda *args: "private-config"
        runner.diagnostic = lambda *args: {"strategies": {role: {"Name":"spread","Version":1} for role in ("root","ephemeral")}}
        runner.call_once = Mock(side_effect=scenarios.CPIRejected("SECRET"))
        runner.checkpoint = Mock()
        with self.assertRaises(scenarios.CPIRejected):
            runner.lifecycle("lost-create", runner.configured_policy())
        args = runner.call_once.call_args.args[1]
        self.assertEqual(runner.active_resources, {"scenario_id":"lost-create", "agent_id":args[0], "pending_operation":"create_vm"})
        self.assertEqual(runner.checkpoint.call_count, 2)
        self.assertNotIn("SECRET", json.dumps(runner.active_resources))

    def test_failed_vm_diagnostic_replaces_prior_case_identity(self):
        runner = bare_runner()
        runner.active_resources = {"scenario_id": "completed-prior-case"}
        runner.derived_config = lambda *args: "private-config"
        runner.diagnostic = Mock(side_effect=scenarios.ScenarioFailure("diagnostic failed"))
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.lifecycle("current-case", runner.configured_policy())
        self.assertEqual(runner.active_resources["scenario_id"], "current-case")
        self.assertEqual(runner.active_resources["pending_operation"], "diagnostic_create_vm")
        runner.checkpoint.assert_called_once()

    def test_removed_membership_keeps_authority_and_changes_scalar_defaults(self):
        runner = bare_runner()
        before = runner.configured_policy()
        after = runner.removed_membership_config(before)
        self.assertNotIn("storage_sets", after)
        for key in scenarios.AUTHORITY_KEYS:
            self.assertEqual(before[key], after[key])
        self.assertEqual(after["disk_storage"], "e1")
        self.assertEqual(after["vm_storage"], "p1")

    def test_real_lifecycle_sequence_removes_membership_before_all_existing_disk_operations(self):
        runner = bare_runner()
        runner._authority = "authority-hash"
        configurations = {}
        def derived(value, label):
            configurations[label] = copy.deepcopy(value)
            return label
        runner.derived_config = derived
        runner.diagnostic = lambda method, args, path: {
            "targets": [{"Role": "root", "VirtualBytes": 1024**3}, {"Role": "ephemeral", "VirtualBytes": 1024**3}],
            "strategies": {role: {"Name": "spread", "Version": 1} for role in ("root", "ephemeral", "persistent")}}
        runner.observed_root_mechanism = Mock(return_value={"mechanism": "import", "task_exitstatus": "OK"})
        runner.observed_vm = lambda *args: {"devices": {"virtio0": "e1:root", "scsi1": "e1:ephemeral"}, "iso": {"ide2": "e1:iso"}}
        runner.observed_volume = lambda volume: {"volid": volume, "size": 1024**3}
        runner.audit = lambda path: {"records": [{"CID": "100", "Kind": "vm", "ID": "11111111-1111-4111-8111-111111111111"}]}
        # This sequence test substitutes observations; generation evidence has
        # separate filesystem, marker, index, and audit contract regressions.
        runner.current_vm_record = lambda audit, cid, rows: rows[0]
        runner.verifier = SimpleNamespace(vm_exists=lambda _: False)
        runner.snapshot_present = Mock()
        runner.assert_deleted = Mock()
        operations = []
        attached = False
        def call(method, args, path):
            nonlocal attached
            operations.append((method, args, configurations[path]))
            if method == "create_vm":
                return ["100", {}]
            if method == "create_disk":
                return "disk-cid"
            if method == "attach_disk":
                attached = True
            if method == "detach_disk":
                attached = False
            if method == "snapshot_disk":
                return "100:cert-snapshot"
            return None
        runner.call_once = call
        observations = []
        def observe(cid, path, previous=None, members=None, size_mib=None):
            observations.append(size_mib)
            return {"allocation_uuid": "22222222-2222-4222-8222-222222222222", "stable_token": "bpd-0123456789abcdef",
                    "volume_id": "p1:volume", "backing": {"server": "nas", "export": "/p1"},
                    "size_bytes": size_mib * 1024**2, "holders": [(100 if attached else 200, "scsi0")]}
        runner.observed_disk = observe
        evidence = runner.lifecycle("removed", runner.configured_policy(), remove_membership=True)
        self.assertEqual([row[0] for row in operations], ["create_vm", "create_disk", "attach_disk", "resize_disk", "snapshot_disk", "delete_snapshot", "detach_disk", "attach_disk", "detach_disk", "delete_disk", "delete_vm"])
        self.assertTrue(all("storage_sets" not in row[2] for row in operations[2:]))
        self.assertEqual(observations, [1024, 1024, 2048, 2048, 2048, 2048, 2048])
        self.assertEqual(runner.snapshot_present.call_args_list[0].args, ("100:cert-snapshot", True))
        self.assertEqual(runner.snapshot_present.call_args_list[1].args, ("100:cert-snapshot", False))
        self.assertEqual(runner.assert_deleted.call_count, 2)
        self.assertTrue(evidence["actual_absence_verified"])
        self.assertEqual(runner.active_resources, {})

    def test_wrong_rejection_reason_or_inventory_change_is_failure(self):
        runner = bare_runner()
        runner.derived_config = Mock(return_value="path")
        runner.audit = Mock(return_value={"records": [], "audit": {"evidence": []}})
        runner.verification = SimpleNamespace(volume_inventory=Mock(return_value=[]))
        runner.verifier = object()
        runner.call_once = Mock(side_effect=scenarios.CPIRejected("authentication unavailable"))
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.rejected_without_mutation(runner.configured_policy(), {}, "boundary")
        runner.call_once.side_effect = scenarios.CPIRejected("outside boundary")
        runner.verification.volume_inventory.side_effect = [[], [("node1", "p1", "p1:leak", 1024)]]
        with self.assertRaises(scenarios.ScenarioFailure):
            runner.rejected_without_mutation(runner.configured_policy(), {}, "boundary")


if __name__ == "__main__":
    unittest.main()
