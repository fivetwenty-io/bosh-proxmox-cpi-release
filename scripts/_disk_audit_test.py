#!/usr/bin/env python3
"""Unit tests for scripts/disk-audit's parker-pool reporting.

Loading strategy
-----------------
scripts/disk-audit has a shebang line and no .py extension, so a plain
``sys.path`` insert plus ``import disk-audit`` cannot work (the hyphen is
not a valid identifier). We load it via importlib with an explicit
SourceFileLoader, mirroring the pattern scripts/_integration_test.py uses
for scripts/cf. The module uses only stdlib and guards the network-driving
``main()`` behind ``if __name__ == "__main__":``, so importing it has no
side effects and no transport stubbing is required.

Run:
    python3 scripts/_disk_audit_test.py
"""

from __future__ import annotations

import importlib.machinery as _ilm
import importlib.util as _ilu
import io
import json
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from types import SimpleNamespace


def _load_disk_audit_module():
    path = str(Path(__file__).resolve().parent / "disk-audit")
    loader = _ilm.SourceFileLoader("disk_audit_script", path)
    spec = _ilu.spec_from_loader("disk_audit_script", loader, origin=path)
    mod = _ilu.module_from_spec(spec)
    mod.__file__ = path  # required for any Path(__file__) use at module level
    loader.exec_module(mod)
    return mod


disk_audit = _load_disk_audit_module()


def _make_cfg() -> SimpleNamespace:
    """A minimal stand-in for AuditConfig carrying only the fields the report
    functions under test read."""
    return SimpleNamespace(
        host="pve.example.com",
        port=8006,
        disk_vmid_start=9000,
        disk_vmid_end=29999,
        parker_vmid_start=90000,
        parker_vmid_end=90999,
        node="",
    )


class TestParkerRecordPool(unittest.TestCase):
    """Tests for ParkerRecord.pool."""

    def test_pool_stored_when_given(self) -> None:
        pr = disk_audit.ParkerRecord(
            vmid=90000, node="pve1", name="parker-svc-a",
            disk_count=1, pool="ops-pool",
        )
        self.assertEqual(pr.pool, "ops-pool")

    def test_pool_defaults_to_empty_string_when_omitted(self) -> None:
        pr = disk_audit.ParkerRecord(
            vmid=90001, node="pve1", name="parker-svc-b",
            disk_count=0,
        )
        self.assertEqual(pr.pool, "")


class TestParkerRecordToDict(unittest.TestCase):
    """Tests for the pool field in ParkerRecord.to_dict()'s JSON shape."""

    def test_to_dict_includes_pool_value(self) -> None:
        pr = disk_audit.ParkerRecord(
            vmid=90000, node="pve1", name="parker-svc-a",
            disk_count=2, pool="ops-pool",
        )
        d = pr.to_dict()
        self.assertIn("pool", d)
        self.assertEqual(d["pool"], "ops-pool")

    def test_to_dict_pool_is_empty_string_not_none_when_no_pool(self) -> None:
        pr = disk_audit.ParkerRecord(
            vmid=90001, node="pve1", name="parker-svc-b",
            disk_count=0,
        )
        d = pr.to_dict()
        self.assertEqual(d["pool"], "")
        self.assertIsNotNone(d["pool"])

    def test_print_json_report_round_trips_pool(self) -> None:
        parkers = [
            disk_audit.ParkerRecord(
                vmid=90000, node="pve1", name="parker-svc-a",
                disk_count=2, pool="ops-pool",
            ),
            disk_audit.ParkerRecord(
                vmid=90001, node="pve1", name="parker-svc-b",
                disk_count=0,
            ),
        ]
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_json_report([], parkers, _make_cfg())
        out = json.loads(buf.getvalue())
        pool_by_vmid = {p["vmid"]: p["pool"] for p in out["parkers"]}
        self.assertEqual(pool_by_vmid[90000], "ops-pool")
        self.assertEqual(pool_by_vmid[90001], "")


class TestPrintHumanReportPoolColumn(unittest.TestCase):
    """Tests for the POOL column in the human-readable parker table.

    The row line is built as:
      "  " + vmid(>7) + "  " + node(<15) + "  " + name(<30) + "  "
      + pool(<20) + "  " + disks/unused/empty
    so the pool cell sits at columns [60:80) of the printed line. We assert
    on that fixed slice so a value bleeding into a neighboring column, or a
    literal "None" standing in for a blank cell, would fail the test.
    """

    _POOL_COL_START = 60
    _POOL_COL_END = 80

    def _render(self, parker_records: list) -> str:
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_human_report([], parker_records, _make_cfg())
        return buf.getvalue()

    def _row_for_vmid(self, out: str, vmid: int) -> str:
        return next(line for line in out.splitlines() if str(vmid) in line and "parker-svc" in line)

    def test_table_header_has_pool_column(self) -> None:
        pr = disk_audit.ParkerRecord(
            vmid=90000, node="pve1", name="parker-svc-a",
            disk_count=0, pool="ops-pool",
        )
        out = self._render([pr])
        self.assertIn("POOL", out)

    def test_table_row_shows_pool_value_in_its_column(self) -> None:
        pr = disk_audit.ParkerRecord(
            vmid=90000, node="pve1", name="parker-svc-a",
            disk_count=0, pool="ops-pool",
        )
        out = self._render([pr])
        row = self._row_for_vmid(out, 90000)
        cell = row[self._POOL_COL_START:self._POOL_COL_END]
        self.assertEqual(cell.strip(), "ops-pool")

    def test_table_row_shows_empty_cell_for_parker_in_no_pool(self) -> None:
        pr = disk_audit.ParkerRecord(
            vmid=90001, node="pve1", name="parker-svc-b",
            disk_count=0,
        )
        out = self._render([pr])
        row = self._row_for_vmid(out, 90001)
        cell = row[self._POOL_COL_START:self._POOL_COL_END]
        self.assertEqual(cell.strip(), "")
        self.assertNotIn("None", out)

    def test_table_columns_after_pool_stay_aligned(self) -> None:
        # Two rows, one with a pool and one without, must line up identically
        # from the DISKS column onward -- the pool column must not shift
        # later fields regardless of its own content length.
        parkers = [
            disk_audit.ParkerRecord(
                vmid=90000, node="pve1", name="parker-svc-a",
                disk_count=3, unused_count=1, pool="ops-pool",
            ),
            disk_audit.ParkerRecord(
                vmid=90001, node="pve1", name="parker-svc-b",
                disk_count=0,
            ),
        ]
        out = self._render(parkers)
        row_a = self._row_for_vmid(out, 90000)
        row_b = self._row_for_vmid(out, 90001)
        # The "/31" disk-capacity marker is a fixed anchor; it must land at
        # the same column in both rows.
        self.assertEqual(row_a.index("/31"), row_b.index("/31"))


def _parker(vmid: int, pool: str = "") -> "object":
    return disk_audit.ParkerRecord(
        vmid=vmid, node="pve1", name=f"parker-{vmid}", disk_count=0, pool=pool,
    )


class TestParseParkerPoolArgs(unittest.TestCase):
    """Tests for the --parker-pool accumulator."""

    def test_none_gives_an_empty_list(self) -> None:
        self.assertEqual(disk_audit.parse_parker_pool_args(None), [])

    def test_repeated_flag_keeps_every_name_in_order(self) -> None:
        self.assertEqual(
            disk_audit.parse_parker_pool_args(["bosh-parker", "blue-parker"]),
            ["bosh-parker", "blue-parker"],
        )

    def test_comma_separated_value_is_split(self) -> None:
        self.assertEqual(
            disk_audit.parse_parker_pool_args(["bosh-parker, blue-parker"]),
            ["bosh-parker", "blue-parker"],
        )

    def test_blanks_are_dropped_and_duplicates_collapse(self) -> None:
        self.assertEqual(
            disk_audit.parse_parker_pool_args(["bosh-parker,,  ", " bosh-parker "]),
            ["bosh-parker"],
        )


class TestParkerPoolNames(unittest.TestCase):
    """Tests for the set of pools the audit treats as parker pools."""

    def test_derived_from_the_pools_parkers_belong_to(self) -> None:
        parkers = [_parker(90000, "bosh-parker"), _parker(90001, "blue-parker")]
        self.assertEqual(
            disk_audit.parker_pool_names(parkers),
            {"bosh-parker", "blue-parker"},
        )

    def test_parker_in_no_pool_contributes_nothing(self) -> None:
        self.assertEqual(disk_audit.parker_pool_names([_parker(90000)]), set())

    def test_explicit_argument_adds_a_pool_that_holds_no_parker(self) -> None:
        self.assertEqual(
            disk_audit.parker_pool_names([], ["bosh-parker"]),
            {"bosh-parker"},
        )

    def test_explicit_argument_and_derivation_are_unioned(self) -> None:
        self.assertEqual(
            disk_audit.parker_pool_names([_parker(90000, "bosh-parker")], ["blue-parker"]),
            {"bosh-parker", "blue-parker"},
        )


class TestFindPoolIntruders(unittest.TestCase):
    """Tests for the workload VM found sitting in a parker pool."""

    def test_parker_in_its_own_pool_is_not_a_finding(self) -> None:
        vm_map = {
            90000: {"node": "pve1", "name": "bosh-parker-90000", "tags": "bosh-cpi;bosh-parker", "pool": "bosh-parker"},
        }
        self.assertEqual(disk_audit.find_pool_intruders(vm_map, {"bosh-parker"}), [])

    def test_workload_vm_in_the_parker_pool_is_a_finding(self) -> None:
        vm_map = {
            140: {"node": "pve2", "name": "bosh-web-0", "tags": "bosh-cpi", "pool": "bosh-parker"},
        }
        found = disk_audit.find_pool_intruders(vm_map, {"bosh-parker"})
        self.assertEqual(len(found), 1)
        self.assertEqual(found[0].vmid, 140)
        self.assertEqual(found[0].name, "bosh-web-0")
        self.assertEqual(found[0].node, "pve2")
        self.assertEqual(found[0].pool, "bosh-parker")

    def test_workload_vm_in_another_pool_is_not_a_finding(self) -> None:
        vm_map = {
            140: {"node": "pve2", "name": "bosh-web-0", "tags": "bosh-cpi", "pool": "bosh-prod"},
        }
        self.assertEqual(disk_audit.find_pool_intruders(vm_map, {"bosh-parker"}), [])

    def test_vm_in_no_pool_is_not_a_finding(self) -> None:
        vm_map = {
            140: {"node": "pve2", "name": "bosh-web-0", "tags": "bosh-cpi", "pool": ""},
        }
        self.assertEqual(disk_audit.find_pool_intruders(vm_map, {"bosh-parker"}), [])

    def test_no_parker_pools_means_no_findings(self) -> None:
        vm_map = {
            140: {"node": "pve2", "name": "bosh-web-0", "tags": "bosh-cpi", "pool": "bosh-parker"},
        }
        self.assertEqual(disk_audit.find_pool_intruders(vm_map, set()), [])

    def test_findings_come_back_ordered_by_vmid(self) -> None:
        vm_map = {
            141: {"node": "pve2", "name": "bosh-web-1", "tags": "", "pool": "bosh-parker"},
            140: {"node": "pve2", "name": "bosh-web-0", "tags": "", "pool": "bosh-parker"},
        }
        found = disk_audit.find_pool_intruders(vm_map, {"bosh-parker"})
        self.assertEqual([r.vmid for r in found], [140, 141])


class TestPoolIntruderWarning(unittest.TestCase):
    """Tests for the stderr finding an intruding workload VM produces."""

    def _warnings(self, intruders: list) -> str:
        cfg = _make_cfg()
        cfg.detached_disk_strategy = "parked"
        buf = io.StringIO()
        with redirect_stderr(buf):
            disk_audit.emit_warnings([], [], intruders, cfg)
        return buf.getvalue()

    def test_warning_names_the_vm_the_pool_and_the_move_command(self) -> None:
        intruder = disk_audit.PoolIntruderRecord(
            vmid=140, node="pve2", name="bosh-web-0", pool="bosh-parker",
        )
        out = self._warnings([intruder])
        self.assertIn("VM 140", out)
        self.assertIn("bosh-web-0", out)
        self.assertIn("pve2", out)
        self.assertIn("pool bosh-parker", out)
        self.assertIn("pvesh set /pools/<workload-pool> --vms 140 --allow-move 1", out)

    def test_no_intruders_means_no_warning(self) -> None:
        self.assertEqual(self._warnings([]), "")


# ---------------------------------------------------------------------------
# Volumes that more than one guest names
# ---------------------------------------------------------------------------

def _vm(node: str = "pve1", name: str = "", tags: str = "", vtype: str = "qemu") -> dict:
    return {"node": node, "name": name, "tags": tags, "pool": "", "type": vtype}


class TestVolumeOwnerVmid(unittest.TestCase):
    """PVE reads the owner from the last path segment of the volume name."""

    def test_owner_name_shapes(self) -> None:
        cases = {
            "a:777/vm-777-disk-2.raw": 777,  # dir-style
            "local-lvm:base-100-disk-0/vm-101-disk-0": 101,  # LVM-thin linked clone
            "local:100/base-100-disk-0.qcow2/101/vm-101-disk-0.qcow2": 101,  # dir linked clone
            "ceph:vm-123-disk-0": 123,  # RBD
            "local-lvm:base-100-disk-0": 100,  # template base volume
            "nfs:custom/data-volume.raw": None,  # no VMID in the name
            "nfs:vm-disk-0.raw": None,
        }
        for volid, want in cases.items():
            with self.subTest(volid=volid):
                self.assertEqual(disk_audit.volume_owner_vmid(volid), want)


class TestFindMultiplyReferenced(unittest.TestCase):
    """Tests for the pure finder."""

    BAND = (90000, 90999)

    def _find(self, vm_map: dict, configs: dict) -> tuple:
        return disk_audit.find_multiply_referenced(vm_map, configs, self.BAND)

    def test_active_slot_and_unused_entry_on_two_guests(self) -> None:
        vm_map = {777: _vm(name="web-0"), 90656: _vm(name="bosh-parker-90656", tags="bosh-cpi;bosh-parker")}
        configs = {
            777: {"unused0": "a:123/vm-123-disk-0.raw"},
            90656: {"scsi1": "a:123/vm-123-disk-0.raw,serial=bpd-0011223344556677"},
        }
        records, unreadable = self._find(vm_map, configs)
        self.assertEqual(unreadable, [])
        self.assertEqual(len(records), 1)
        mr = records[0]
        self.assertEqual(mr.volid, "a:123/vm-123-disk-0.raw")
        self.assertEqual(mr.owner_vmid, 123)
        self.assertFalse(mr.owner_present)
        refs = {(r["vmid"], r["slot"], r["kind"], r["parker"]) for r in mr.references}
        self.assertEqual(refs, {(777, "unused0", "unused", False), (90656, "scsi1", "active", True)})

    def test_two_active_slots_on_two_guests(self) -> None:
        vm_map = {777: _vm(), 888: _vm(node="pve2")}
        configs = {777: {"scsi1": "a:777/vm-777-disk-2.raw,size=5G"}, 888: {"virtio2": "a:777/vm-777-disk-2.raw"}}
        records, _ = self._find(vm_map, configs)
        self.assertEqual(len(records), 1)
        self.assertEqual({r["kind"] for r in records[0].references}, {"active"})

    def test_same_guest_twice_is_not_a_finding(self) -> None:
        vm_map = {777: _vm()}
        configs = {777: {"scsi1": "a:777/vm-777-disk-2.raw", "unused0": "a:777/vm-777-disk-2.raw"}}
        records, _ = self._find(vm_map, configs)
        self.assertEqual(records, [])

    def test_cdrom_none_and_passthrough_are_skipped(self) -> None:
        vm_map = {777: _vm(), 888: _vm()}
        shared = {
            "ide2": "local:iso/ubuntu.iso,media=cdrom",
            "ide3": "local-lvm:vm-100-cloudinit,media=cdrom",
            "ide0": "none,media=cdrom",
            "sata0": "/dev/disk/by-id/ata-shared",
            "scsi3": "none",
        }
        records, _ = self._find(vm_map, {777: dict(shared), 888: dict(shared)})
        self.assertEqual(records, [])

    def test_volume_outside_the_disk_band_is_reported(self) -> None:
        vm_map = {777: _vm(), 90100: _vm(tags="bosh-parker")}
        configs = {777: {"unused0": "a:90100/vm-90100-disk-3.raw"}, 90100: {"scsi0": "a:90100/vm-90100-disk-3.raw"}}
        records, _ = self._find(vm_map, configs)
        self.assertEqual([r.volid for r in records], ["a:90100/vm-90100-disk-3.raw"])

    def test_owner_marker_goes_on_the_owner_only(self) -> None:
        vm_map = {777: _vm(), 888: _vm()}
        configs = {777: {"scsi1": "a:777/vm-777-disk-2.raw"}, 888: {"unused0": "a:777/vm-777-disk-2.raw"}}
        records, _ = self._find(vm_map, configs)
        owns = {r["vmid"]: r["owns"] for r in records[0].references}
        self.assertEqual(owns, {777: True, 888: False})
        self.assertTrue(records[0].owner_holds)

    def test_non_owner_unused_reference_carries_no_owner_marker(self) -> None:
        vm_map = {777: _vm(), 888: _vm()}
        configs = {777: {"unused0": "a:888/vm-888-disk-1.raw"}, 888: {"scsi1": "a:888/vm-888-disk-1.raw"}}
        records, _ = self._find(vm_map, configs)
        unused = next(r for r in records[0].references if r["kind"] == "unused")
        self.assertEqual(unused["vmid"], 777)
        self.assertFalse(unused["owns"])

    def test_owner_present_but_holding_no_reference(self) -> None:
        vm_map = {123: _vm(name="reused-vmid"), 777: _vm(), 888: _vm()}
        configs = {123: {}, 777: {"unused0": "a:123/vm-123-disk-0.raw"}, 888: {"scsi1": "a:123/vm-123-disk-0.raw"}}
        records, _ = self._find(vm_map, configs)
        mr = records[0]
        self.assertEqual(mr.owner_vmid, 123)
        self.assertTrue(mr.owner_present)
        self.assertFalse(mr.owner_holds)
        self.assertIn("destroy-unreferenced-disks", disk_audit._multi_ref_owner_note(mr))

    def test_name_with_no_owner_gives_null_owner(self) -> None:
        vm_map = {777: _vm(), 888: _vm()}
        configs = {777: {"scsi1": "nfs:custom/data.raw"}, 888: {"unused0": "nfs:custom/data.raw"}}
        records, _ = self._find(vm_map, configs)
        self.assertIsNone(records[0].owner_vmid)
        self.assertIsNone(records[0].to_dict()["owner_vmid"])

    def test_container_row_is_skipped_and_not_unreadable(self) -> None:
        vm_map = {200: _vm(vtype="lxc"), 777: _vm()}
        records, unreadable = self._find(vm_map, {200: None, 777: {}})
        self.assertEqual(records, [])
        self.assertEqual(unreadable, [])

    def test_unreadable_qemu_guest_is_listed(self) -> None:
        vm_map = {777: _vm(), 778: _vm()}
        _, unreadable = self._find(vm_map, {777: {}, 778: None})
        self.assertEqual(unreadable, [778])


class _StubClient:
    """A PVE client that answers from fixed data, for collect_inventory and main."""

    def __init__(self, rows: list, configs: dict, privs: "dict | None" = None, perm_err: str = "") -> None:
        self.rows = rows
        self.configs = configs
        self.privs = {"VM.Audit": 1} if privs is None else privs
        self.perm_err = perm_err
        self.reads: list = []

    def cluster_resources_vms(self) -> list:
        return self.rows

    def list_nodes(self) -> list:
        return ["pve1"]

    def node_storages(self, node: str) -> list:
        return []

    def storage_content(self, node: str, storage: str) -> list:
        return []

    def vm_config(self, node: str, vmid: int):
        self.reads.append((node, vmid))
        return self.configs.get(vmid)

    def vm_config_soft(self, node: str, vmid: int):
        return self.vm_config(node, vmid)

    def vms_permissions(self):
        if self.perm_err:
            return None, self.perm_err
        return self.privs, ""


def _audit_cfg() -> SimpleNamespace:
    cfg = _make_cfg()
    cfg.detached_disk_strategy = "parked"
    return cfg


_DOUBLE_ROWS = [
    {"vmid": 777, "node": "pve1", "name": "web-0", "type": "qemu"},
    {"vmid": 90656, "node": "pve1", "name": "bosh-parker-90656", "type": "qemu", "tags": "bosh-cpi;bosh-parker"},
    {"vmid": 200, "node": "pve1", "name": "ct", "type": "lxc"},
]
_DOUBLE_CONFIGS = {
    777: {"unused0": "a:123/vm-123-disk-0.raw"},
    90656: {"scsi1": "a:123/vm-123-disk-0.raw,serial=bpd-0011223344556677", "tags": "bosh-cpi;bosh-parker"},
}


class TestCollectInventoryDoubleReferences(unittest.TestCase):
    """Step 8 through collect_inventory with a stub client."""

    def test_reads_guests_and_skips_the_container(self) -> None:
        client = _StubClient(_DOUBLE_ROWS, _DOUBLE_CONFIGS)
        _, _, _, report = disk_audit.collect_inventory(client, _audit_cfg())
        self.assertEqual([r.volid for r in report.records], ["a:123/vm-123-disk-0.raw"])
        self.assertEqual(report.unreadable_vmids, [])
        self.assertNotIn(("pve1", 200), client.reads)
        self.assertTrue(report.complete)

    def test_missing_vm_audit_limits_visibility(self) -> None:
        client = _StubClient(_DOUBLE_ROWS, _DOUBLE_CONFIGS, privs={})
        _, _, _, report = disk_audit.collect_inventory(client, _audit_cfg())
        self.assertEqual(report.visibility, "limited")
        self.assertFalse(report.complete)

    def test_failed_permissions_read_makes_visibility_unknown(self) -> None:
        client = _StubClient(_DOUBLE_ROWS, _DOUBLE_CONFIGS, perm_err="HTTP 403 Forbidden")
        _, _, _, report = disk_audit.collect_inventory(client, _audit_cfg())
        self.assertEqual(report.visibility, "unknown")
        self.assertEqual(report.visibility_error, "HTTP 403 Forbidden")


class TestDoubleReferenceOutput(unittest.TestCase):
    """Rendering, warnings, and the exit code."""

    def _report(self, **kw) -> object:
        client = _StubClient(_DOUBLE_ROWS, _DOUBLE_CONFIGS, **kw)
        return disk_audit.collect_inventory(client, _audit_cfg())[3]

    def test_human_report_lists_the_volume_and_each_reference(self) -> None:
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_human_report([], [], _make_cfg(), self._report())
        out = buf.getvalue()
        self.assertIn("Volumes named by more than one guest: 1", out)
        self.assertIn("MULTIPLY REFERENCED VOLUMES", out)
        self.assertIn("  a:123/vm-123-disk-0.raw", out)
        self.assertIn("VM 777 (web-0) on pve1 unused0", out)
        self.assertIn("VM 90656 (bosh-parker-90656) on pve1 scsi1 [parker]", out)
        self.assertIn("VM 123 would own this volume by name", out)

    def test_human_report_without_the_report_is_unchanged(self) -> None:
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_human_report([], [], _make_cfg())
        self.assertNotIn("more than one guest", buf.getvalue())

    def test_json_fields_and_existing_keys(self) -> None:
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_json_report([], [], _make_cfg(), self._report(privs={}))
        out = json.loads(buf.getvalue())
        for key in ("host", "port", "disk_band", "parker_band", "summary", "disks", "parkers"):
            self.assertIn(key, out)
        self.assertEqual(out["summary"]["multiply_referenced"], 1)
        self.assertEqual(out["multiply_referenced"][0]["owner_vmid"], 123)
        self.assertEqual(out["multiply_referenced_unreadable_vmids"], [])
        self.assertEqual(out["multiply_referenced_visibility"], "limited")
        self.assertFalse(out["multiply_referenced_complete"])

    def test_json_lists_unreadable_vmids(self) -> None:
        configs = dict(_DOUBLE_CONFIGS)
        del configs[777]
        client = _StubClient(_DOUBLE_ROWS, configs)
        report = disk_audit.collect_inventory(client, _audit_cfg())[3]
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_json_report([], [], _make_cfg(), report)
        out = json.loads(buf.getvalue())
        self.assertEqual(out["multiply_referenced_unreadable_vmids"], [777])
        self.assertFalse(out["multiply_referenced_complete"])

    def _warnings(self, report: object) -> str:
        buf = io.StringIO()
        with redirect_stderr(buf):
            disk_audit.emit_warnings([], [], [], _audit_cfg(), report)
        return buf.getvalue()

    def test_warning_names_every_reference_and_points_at_the_docs(self) -> None:
        out = self._warnings(self._report())
        self.assertIn("volume a:123/vm-123-disk-0.raw is named by 2 guests", out)
        self.assertIn("VM 777 (web-0) on pve1 unused0", out)
        self.assertIn("Leave every reference in place until we know which guest really holds the disk", out)
        self.assertIn('see "Auditing parked disks with scripts/disk-audit" in docs/operations.md of bosh-proxmox-cpi-release', out)
        self.assertNotIn("qm ", out)

    def test_warning_names_the_owner_hazard(self) -> None:
        vm_map = {777: _vm(name="web-0"), 888: _vm(name="web-1")}
        configs = {777: {"scsi1": "a:777/vm-777-disk-2.raw"}, 888: {"unused0": "a:777/vm-777-disk-2.raw"}}
        records, _ = disk_audit.find_multiply_referenced(vm_map, configs, (90000, 90999))
        out = self._warnings(disk_audit.MultiRefReport(records, []))
        self.assertIn("VM 777 (web-0) on pve1 scsi1 [owns by name]", out)
        self.assertIn("destroying VM 777, or removing its unused entry, deletes the volume", out)

    def test_limited_and_unknown_visibility_warnings(self) -> None:
        limited = self._warnings(self._report(privs={}))
        self.assertIn("lacks VM.Audit on /vms", limited)
        self.assertIn("covers only the guests this token can see", limited)
        unknown = self._warnings(self._report(perm_err="HTTP 500 boom"))
        self.assertIn("could not read this token's permissions on /vms (HTTP 500 boom)", unknown)
        self.assertIn("coverage of the report of volumes named by more than one guest is unknown", unknown)

    def test_full_visibility_gives_no_coverage_warning(self) -> None:
        out = self._warnings(self._report())
        self.assertNotIn("VM.Audit", out)
        self.assertNotIn("did not come back", out)

    def test_exit_code_stays_zero_with_only_a_double_reference(self) -> None:
        import os
        import tempfile
        client = _StubClient(_DOUBLE_ROWS, _DOUBLE_CONFIGS, privs={})
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"}, fh)
            path = fh.name
        original = disk_audit.PVEClient
        disk_audit.PVEClient = lambda cfg: client
        try:
            out, err = io.StringIO(), io.StringIO()
            with redirect_stdout(out), redirect_stderr(err):
                rc = disk_audit.main(["--config", path])
        finally:
            disk_audit.PVEClient = original
            os.unlink(path)
        self.assertEqual(rc, 0)
        self.assertIn("is named by 2 guests", err.getvalue())
        self.assertIn("lacks VM.Audit", err.getvalue())


class TestDoubleReferenceDocsPointer(unittest.TestCase):
    """The warning names a docs heading that has to exist."""

    def test_heading_exists_in_operations_doc(self) -> None:
        pointer = disk_audit._MULTI_REF_DOCS
        heading = pointer.split('"')[1]
        doc = Path(__file__).resolve().parent.parent / "docs" / "operations.md"
        headings = [
            line.lstrip("#").strip().replace("`", "")
            for line in doc.read_text(encoding="utf-8").splitlines()
            if line.startswith("#")
        ]
        self.assertIn(heading, headings)
        self.assertIn("docs/operations.md of bosh-proxmox-cpi-release", pointer)


class _FakeResponse:
    def __init__(self, data: object) -> None:
        self._body = json.dumps({"data": data}).encode("utf-8")

    def read(self) -> bytes:
        return self._body

    def __enter__(self) -> "_FakeResponse":
        return self

    def __exit__(self, *exc: object) -> None:
        return None


class TestStepEightReadsSoftly(unittest.TestCase):
    """A guest config that fails to come back is listed, not fatal.

    The real PVEClient runs against a patched urlopen, so the soft read is the
    one under test. VM 777 answers HTTP 500, VM 778 fails at the network, and
    VM 779 and VM 780 name the same volume.
    """

    def _urlopen(self, req, context=None, timeout=None):
        import urllib.error
        url = req.full_url
        if url.endswith("/cluster/resources?type=vm"):
            return _FakeResponse([
                {"vmid": 777, "node": "pve1", "type": "qemu"},
                {"vmid": 778, "node": "pve2", "type": "qemu"},
                {"vmid": 779, "node": "pve1", "type": "qemu"},
                {"vmid": 780, "node": "pve1", "type": "qemu"},
            ])
        if url.endswith("/nodes"):
            return _FakeResponse([{"node": "pve1", "status": "online"}])
        if url.endswith("/nodes/pve1/storage"):
            return _FakeResponse([])
        if url.endswith("/nodes/pve1/qemu/777/config"):
            raise urllib.error.HTTPError(url, 500, "Internal Server Error", None, None)
        if url.endswith("/nodes/pve2/qemu/778/config"):
            raise urllib.error.URLError("No route to host")
        if url.endswith("/qemu/779/config"):
            return _FakeResponse({"scsi1": "a:779/vm-779-disk-1.raw"})
        if url.endswith("/qemu/780/config"):
            return _FakeResponse({"unused0": "a:779/vm-779-disk-1.raw"})
        if "/access/permissions" in url:
            return _FakeResponse({"/vms": {"VM.Audit": 1}})
        raise AssertionError(f"unexpected request {url}")

    def test_failed_reads_are_listed_and_the_audit_completes(self) -> None:
        from unittest import mock
        cfg = disk_audit.AuditConfig({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"})
        client = disk_audit.PVEClient(cfg)
        with mock.patch.object(disk_audit.urllib.request, "urlopen", self._urlopen):
            _, _, _, report = disk_audit.collect_inventory(client, cfg)
        self.assertEqual(report.unreadable_vmids, [777, 778])
        self.assertEqual([r.volid for r in report.records], ["a:779/vm-779-disk-1.raw"])
        self.assertFalse(report.complete)


if __name__ == "__main__":
    unittest.main()
