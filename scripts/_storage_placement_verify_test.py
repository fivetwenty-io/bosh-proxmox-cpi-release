"""Offline acceptance checks for the opt-in storage placement harness."""
from __future__ import annotations

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
from _storage_placement_verify import PlacementVerification


def topology():
    stores = ["e1", "e2", "e3", "p1", "p2"]
    return {
        "observation_only": True,
        "targets": [{"role": "root", "storage_id": "e1"}],
        "frozen_membership": {"E": stores[:3], "P": stores[3:]},
        "capacities": [{"Pair": {"Node": node, "StorageID": store,
            "BackingKey": "nfs://server/" + store, "Reason": ""}}
            for node in ("n1", "n2") for store in stores],
    }


def clean_audit():
    return {"generation_index_healthy": True, "cluster_continuity": True,
        "records": [{"allocation_id": "existing", "record_sha256": "frozen"}],
        "audit": {"complete": True, "vm_scan_complete": True, "issues": [],
                  "conflicts": [], "evidence": [{"volume_id": "e1:root"}]}}


class PlacementVerificationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        config = self.root / "config.json"
        config.write_text(json.dumps({"ephemeral_storage_set": "E", "persistent_storage_set": "P"}))
        binary = self.root / "cpi"
        binary.write_bytes(b"offline fixture")
        self.verification = PlacementVerification(str(config), str(binary), str(self.root / "report.json"))
        self.verification.validate_topology(topology())

    @staticmethod
    def content(volume):
        return {"volid": volume, "size": 8 * 1024 * 1024, "format": "raw"}

    def test_feasible_topology_keeps_all_ten_observations(self):
        self.assertEqual(len(self.verification.inventory_pairs), 10)
        self.assertEqual(self.verification.members["persistent"], {"p1", "p2"})

    def test_focused_topology_keeps_nfs_identity_and_two_node_requirements(self):
        diagnostic = topology()
        diagnostic["frozen_membership"]["P"] = ["p1"]
        diagnostic["capacities"] = [row for row in diagnostic["capacities"] if row["Pair"]["StorageID"] != "p2"]
        self.verification.validate_topology(diagnostic, ephemeral_minimum=2, persistent_minimum=1)
        self.assertEqual(self.verification.members["persistent"], {"p1"})
        self.assertEqual(len(self.verification.inventory_pairs), 8)
        with self.assertRaises(RuntimeError):
            self.verification.validate_topology(diagnostic)
        for mutation in ("one-node", "alias", "block"):
            changed = copy.deepcopy(diagnostic)
            if mutation == "one-node":
                changed["capacities"] = changed["capacities"][:4]
            else:
                for row in changed["capacities"]:
                    if row["Pair"]["StorageID"] == "p1":
                        row["Pair"]["BackingKey"] = "nfs://server/e1" if mutation == "alias" else "lvm://vg"
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                self.verification.validate_topology(changed, ephemeral_minimum=2, persistent_minimum=1)

    def test_missing_unhealthy_aliased_and_one_node_topologies_fail(self):
        cases = []
        missing = topology(); missing["frozen_membership"]["E"].pop(); cases.append(missing)
        aliased = topology()
        for row in aliased["capacities"]:
            row["Pair"]["BackingKey"] = "nfs://server/same"
        cases.append(aliased)
        failed = topology(); failed["capacities"][0]["Pair"]["Reason"] = "offline"; cases.append(failed)
        one = topology(); one["capacities"] = one["capacities"][:5]; cases.append(one)
        changed = topology(); changed["capacities"][0]["Pair"]["BackingKey"] = "nfs://other/export"; cases.append(changed)
        for diagnostic in cases:
            with self.subTest(diagnostic=diagnostic), self.assertRaises(RuntimeError):
                self.verification.validate_topology(diagnostic)

    def test_observation_only_and_feasible_targets_required(self):
        for key, value in (("observation_only", False), ("targets", [])):
            diagnostic = topology(); diagnostic[key] = value
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                self.verification.validate_topology(diagnostic)

    def test_preflight_uses_private_request_and_production_cli(self):
        def observe(command):
            self.assertEqual(command[:2], [str(self.root.resolve() / "pve-cid"), "storage-plan"])
            request = Path(command[command.index("--request") + 1])
            self.assertEqual(request.stat().st_mode & 0o777, 0o600)
            self.assertEqual(json.loads(request.read_text())["method"], "create_vm")
            return topology()
        with patch.object(self.verification, "_json_command", side_effect=observe):
            self.verification.preflight(["agent", "stemcell", {}, {}, [], {}])
        report = json.loads((self.root / "report.json").read_text())
        self.assertFalse(report["complete_release_matrix"])
        self.assertEqual((self.root / "report.json").stat().st_mode & 0o777, 0o600)

    def test_explicit_root_override_refuses_base_bundle_certification(self):
        for properties in ({"storage_pool": "e1"}, {"root_storage_set": "E"}, {"ephemeral_storage_pool": "e1"}):
            with self.subTest(properties=properties), self.assertRaises(RuntimeError):
                self.verification.preflight(["agent", "stemcell", properties, {}, [], {}])

    def test_root_derived_layout_checks_actual_volume_and_ignores_cdrom(self):
        read = Mock(side_effect=self.content)
        self.verification.verify_vm({"virtio0": "e2:root,size=8M", "ide2": "iso:env.iso,media=cdrom", "scsihw": "virtio-scsi-pci"}, False, read)
        read.assert_called_once_with("e2:root")

    def test_dedicated_bundle_accepts_both_root_buses(self):
        for bus, root in (("virtio", "virtio0"), (" SCSI ", "scsi0")):
            self.verification.config["root_disk_bus"] = bus
            self.verification.verify_vm({root: "e3:root", "scsi1": "e3:ephemeral"}, True, self.content)

    def test_extra_duplicate_missing_and_split_ephemeral_disks_fail(self):
        base = {"virtio0": "e1:root", "scsi1": "e1:ephemeral"}
        cases = [dict(base, scsi2="e1:leaked"), dict(base, unused0="e1:orphan"),
                 dict(base, scsi1="e2:ephemeral"), dict(base, scsi1="e1:root"), {"virtio0": "e1:root"}]
        for config in cases:
            with self.subTest(config=config), self.assertRaises(RuntimeError):
                self.verification.verify_vm(config, True, self.content)

    def test_root_derived_layout_never_adds_a_disk(self):
        with self.assertRaises(RuntimeError):
            self.verification.verify_vm({"virtio0": "e1:root", "scsi1": "e1:extra"}, False, self.content)

    def test_actual_vm_volume_absence_and_identity_mismatch_fail(self):
        for entry in (None, {"volid": "e1:other", "size": 10}, {"volid": "e1:root", "size": 0}):
            with self.subTest(entry=entry), self.assertRaises(RuntimeError):
                self.verification.verify_vm({"virtio0": "e1:root"}, False, lambda _: entry)

    def test_persistent_placement_uses_actual_renamed_volume(self):
        self.verification.verify_disk(self.content("p2:777/renamed.raw"), "p2:777/renamed.raw")
        self.assertEqual(self.verification.report["persistent_volume"], "p2:777/renamed.raw")
        for entry, expected in ((None, "p1:birth"), (self.content("p1:birth"), "p1:renamed"),
                                (self.content("e1:wrong"), "e1:wrong")):
            with self.subTest(entry=entry), self.assertRaises(RuntimeError):
                self.verification.verify_disk(entry, expected)

    def test_invalid_content_size_or_volume_never_certifies_placement(self):
        for entry in ({"volid": "p1:", "size": 1}, {"volid": "p1:disk", "size": True},
                      {"volid": "p1:disk", "size": "1"}, {"volid": "p1:disk", "size": -1}):
            with self.subTest(entry=entry), self.assertRaises(RuntimeError):
                self.verification.verify_disk(entry, entry["volid"])

    def test_inventory_reads_all_nodes_and_stores_and_fails_partial_reads(self):
        get = Mock(return_value=[])
        self.assertEqual(self.verification.volume_inventory(SimpleNamespace(_get=get)), [])
        self.assertEqual(get.call_count, 10)
        get.side_effect = RuntimeError("unavailable")
        with self.assertRaises(RuntimeError):
            self.verification.volume_inventory(SimpleNamespace(_get=get))

    def test_inventory_rejects_malformed_and_duplicate_rows(self):
        for entries in (None, {}, [self.content("foreign:disk")], [self.content("e1:disk")] * 2):
            with self.subTest(entries=entries), self.assertRaises(RuntimeError):
                self.verification.volume_inventory(SimpleNamespace(_get=Mock(return_value=entries)))

    @staticmethod
    def reject(method, arguments):
        properties = arguments[1]
        if properties.get("storage_set") == "certification-undefined-set":
            raise RuntimeError("unknown storage set")
        if "storage_set" in properties:
            raise RuntimeError("competing selectors")
        raise RuntimeError("boundary violation")

    def test_selector_rejections_require_unchanged_evidence_and_inventory(self):
        with patch.object(self.verification, "_json_command", return_value=clean_audit()):
            inventory = Mock(return_value=[])
            self.verification.rejected_selector_cases(self.reject, "777", 10, inventory)
        self.assertEqual(inventory.call_count, 6)
        self.assertEqual(self.verification.report["checks"], ["unknown_set", "competing_selectors", "outside_boundary"])

    def test_rejected_request_with_unrecorded_disk_leak_fails(self):
        with patch.object(self.verification, "_json_command", return_value=clean_audit()), self.assertRaisesRegex(RuntimeError, "actual storage volumes"):
            self.verification.rejected_selector_cases(self.reject, "777", 10, Mock(side_effect=[[], [("n1", "p1", "p1:leak", 10)]]))

    def test_rejected_request_with_changed_record_or_remote_evidence_fails(self):
        for changed in ("record", "evidence"):
            before = clean_audit(); after = copy.deepcopy(before)
            if changed == "record":
                after["records"][0]["record_sha256"] = "changed"
            else:
                after["audit"]["evidence"].append({"volume_id": "p1:leak"})
            with self.subTest(changed=changed), patch.object(self.verification, "_json_command", side_effect=[before, after]), self.assertRaises(RuntimeError):
                self.verification.rejected_selector_cases(self.reject, "777", 10, lambda: [])

    def test_partial_or_unhealthy_audit_never_certifies_rejection(self):
        for field in ("complete", "vm_scan_complete", "issues", "conflicts"):
            audit = clean_audit(); audit["audit"][field] = False if "complete" in field else ["issue"]
            with self.subTest(field=field), patch.object(self.verification, "_json_command", return_value=audit), self.assertRaises(RuntimeError):
                self.verification._journal_records()

    def test_wrong_rejection_reason_is_not_a_pass(self):
        with patch.object(self.verification, "_json_command", return_value=clean_audit()), self.assertRaisesRegex(RuntimeError, "unexpected reason"):
            self.verification.rejected_selector_cases(Mock(side_effect=RuntimeError("authentication failed")), "777", 10, lambda: [])

    def test_unexpected_success_cleans_returned_disk_and_still_fails(self):
        call = Mock(side_effect=["pvd-owned-created-cid", None])
        with patch.object(self.verification, "_json_command", return_value=clean_audit()), self.assertRaisesRegex(RuntimeError, "unexpectedly allocated"):
            self.verification.rejected_selector_cases(call, "777", 10, lambda: [])
        self.assertEqual(call.call_args_list[1].args, ("delete_disk", ["pvd-owned-created-cid"]))

    def test_report_replaces_symlink_without_modifying_its_target(self):
        target = self.root / "unrelated"
        target.write_text("preserve")
        os.symlink(target, self.root / "report.json")
        self.verification._passed("offline")
        self.assertEqual(target.read_text(), "preserve")
        self.assertFalse((self.root / "report.json").is_symlink())
        self.assertEqual((self.root / "report.json").stat().st_mode & 0o777, 0o600)

    def test_subprocess_failure_does_not_expose_raw_diagnostics(self):
        result = SimpleNamespace(returncode=1, stdout="secret", stderr="secret")
        with patch("_storage_placement_verify.subprocess.run", return_value=result), self.assertRaisesRegex(RuntimeError, "verification command failed") as raised:
            self.verification._json_command(["offline"])
        self.assertNotIn("secret", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
