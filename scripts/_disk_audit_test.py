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

    def vm_views(self, node: str, vmid: int):
        cfg = self.vm_config(node, vmid)
        return None if cfg is None else (cfg, cfg)

    def vm_views_soft(self, node: str, vmid: int):
        cfg = self.vm_config_soft(node, vmid)
        return None if cfg is None else (cfg, cfg)

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
        _, _, _, report, _ = disk_audit.collect_inventory(client, _audit_cfg())
        self.assertEqual([r.volid for r in report.records], ["a:123/vm-123-disk-0.raw"])
        self.assertEqual(report.unreadable_vmids, [])
        self.assertNotIn(("pve1", 200), client.reads)
        self.assertTrue(report.complete)

    def test_missing_vm_audit_limits_visibility(self) -> None:
        client = _StubClient(_DOUBLE_ROWS, _DOUBLE_CONFIGS, privs={})
        _, _, _, report, _ = disk_audit.collect_inventory(client, _audit_cfg())
        self.assertEqual(report.visibility, "limited")
        self.assertFalse(report.complete)

    def test_failed_permissions_read_makes_visibility_unknown(self) -> None:
        client = _StubClient(_DOUBLE_ROWS, _DOUBLE_CONFIGS, perm_err="HTTP 403 Forbidden")
        _, _, _, report, _ = disk_audit.collect_inventory(client, _audit_cfg())
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
        if url.endswith("/nodes/pve1/qemu/777/pending"):
            err = urllib.error.HTTPError(url, 500, "Internal Server Error", None, None)
            self.addCleanup(err.close)
            raise err
        if url.endswith("/nodes/pve2/qemu/778/pending"):
            raise urllib.error.URLError("No route to host")
        if url.endswith("/qemu/779/pending"):
            return _FakeResponse([{"key": "scsi1", "value": "a:779/vm-779-disk-1.raw"}])
        if url.endswith("/qemu/780/pending"):
            return _FakeResponse([{"key": "unused0", "value": "a:779/vm-779-disk-1.raw"}])
        if "/access/permissions" in url:
            return _FakeResponse({"/vms": {"VM.Audit": 1}})
        raise AssertionError(f"unexpected request {url}")

    def test_failed_reads_are_listed_and_the_audit_completes(self) -> None:
        from unittest import mock
        cfg = disk_audit.AuditConfig({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"})
        client = disk_audit.PVEClient(cfg)
        with mock.patch.object(disk_audit.urllib.request, "urlopen", self._urlopen):
            _, _, _, report, _ = disk_audit.collect_inventory(client, cfg)
        self.assertEqual(report.unreadable_vmids, [777, 778])
        self.assertEqual([r.volid for r in report.records], ["a:779/vm-779-disk-1.raw"])
        self.assertFalse(report.complete)


class _DiscoveryClient(_StubClient):
    """A stub with storages, their content, and per-storage read failures."""

    def __init__(self, rows, configs, storages, content, fail_content=()) -> None:
        super().__init__(rows, configs)
        self.storages = storages
        self.content = content
        self.fail_content = set(fail_content)
        self.content_reads: list = []

    def node_storages(self, node: str) -> list:
        return self.storages

    def storage_content(self, node: str, storage: str) -> list:
        self.content_reads.append(storage)
        if storage in self.fail_content:
            raise SystemExit(2)
        return [{"volid": v, "size": 1 << 30} for v in self.content.get(storage, [])]


def _images(name: str, **extra) -> dict:
    return {"storage": name, "content": "images,rootdir", **extra}


_BPD = "bpd-0011223344556677"


def _inventory(client):
    err = io.StringIO()
    with redirect_stderr(err):
        disks, parkers, _, report, _ = disk_audit.collect_inventory(client, _audit_cfg())
    return {d.volid: d for d in disks}, parkers, report, err.getvalue()


def _inventory_skipped(client):
    """Like _inventory, but returns the skipped-storage list."""
    with redirect_stderr(io.StringIO()):
        return disk_audit.collect_inventory(client, _audit_cfg())[4]


class TestDisabledStorages(unittest.TestCase):
    """A storage the listing marks off is skipped, and the run carries on."""

    def _run(self, **flags):
        client = _DiscoveryClient(
            [], {}, [_images("off", **flags), _images("on")],
            {"off": ["off:vm-9001-disk-0"], "on": ["on:vm-9002-disk-0"]},
            fail_content={"off"},
        )
        disks, _, _, err = _inventory(client)
        return client, disks, err

    def test_enabled_zero_is_skipped_with_a_line(self) -> None:
        client, disks, err = self._run(enabled=0)
        self.assertNotIn("off", client.content_reads)
        self.assertEqual(list(disks), ["on:vm-9002-disk-0"])
        self.assertIn("SKIPPED: node pve1 storage off:", err)

    def test_active_zero_is_skipped_with_a_line(self) -> None:
        client, disks, err = self._run(active=0)
        self.assertNotIn("off", client.content_reads)
        self.assertIn("SKIPPED: node pve1 storage off:", err)
        self.assertEqual(err.count("SKIPPED"), 1)

    def test_string_and_false_forms_are_skipped(self) -> None:
        for flags in ({"enabled": "0"}, {"active": "0"}, {"active": False}, {"enabled": False}):
            client, _, err = self._run(**flags)
            self.assertNotIn("off", client.content_reads, flags)
            self.assertIn("SKIPPED", err, flags)

    def test_missing_and_set_fields_are_still_read(self) -> None:
        for flags in ({}, {"enabled": 1, "active": 1}, {"enabled": "1", "active": "1"}):
            client = _DiscoveryClient([], {}, [_images("on", **flags)], {"on": ["on:vm-9002-disk-0"]})
            disks, _, _, err = _inventory(client)
            self.assertEqual(list(disks), ["on:vm-9002-disk-0"], flags)
            self.assertNotIn("SKIPPED", err)

    def test_a_500_on_an_enabled_storage_still_fails_the_run(self) -> None:
        client = _DiscoveryClient([], {}, [_images("on")], {"on": ["on:vm-9002-disk-0"]}, fail_content={"on"})
        with self.assertRaises(SystemExit) as ctx:
            _inventory(client)
        self.assertEqual(ctx.exception.code, 2)

    def test_real_client_still_dies_on_http_500(self) -> None:
        import urllib.error
        from unittest import mock
        cfg = disk_audit.AuditConfig({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"})
        client = disk_audit.PVEClient(cfg)

        def urlopen(req, context=None, timeout=None):
            err = urllib.error.HTTPError(req.full_url, 500, "storage 'x' is disabled", None, None)
            self.addCleanup(err.close)
            raise err

        with mock.patch.object(disk_audit.urllib.request, "urlopen", urlopen):
            with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as ctx:
                client.storage_content("pve1", "x")
        self.assertEqual(ctx.exception.code, 2)


class TestStableIDParser(unittest.TestCase):
    """stable_id_from_drive_opt_str ports StableIDFromDriveOptStr."""

    def test_shapes(self) -> None:
        f = disk_audit.stable_id_from_drive_opt_str
        self.assertEqual(f(f"a:vm-1-disk-0,serial={_BPD},size=8G"), _BPD)
        self.assertEqual(f(f"a:vm-1-disk-0,size=8G,serial={_BPD}"), _BPD)
        self.assertEqual(f("a:vm-1-disk-0,serial=guest123,size=8G"), "")
        self.assertEqual(f("a:vm-1-disk-0,size=8G"), "")
        self.assertEqual(f(f"serial={_BPD}"), "")  # the volid slot is never an option
        self.assertEqual(f("a:vm-1-disk-0"), "")


class TestRenamedDiskDiscovery(unittest.TestCase):
    """Disks found by identity rather than by the VMID in their name."""

    ROWS = [
        {"vmid": 777, "node": "pve1", "name": "web-0", "type": "qemu"},
        {"vmid": 778, "node": "pve1", "name": "web-1", "type": "qemu"},
        {"vmid": 90656, "node": "pve1", "name": "bosh-parker-90656", "type": "qemu", "tags": "bosh-cpi;bosh-parker"},
    ]

    def _client(self, configs, volumes):
        return _DiscoveryClient(self.ROWS, configs, [_images("a")], {"a": volumes})

    def test_band_discovery_is_unchanged(self) -> None:
        client = self._client({777: {"scsi1": "a:vm-9001-disk-0"}}, ["a:vm-9001-disk-0", "a:vm-9002-disk-0"])
        disks, _, _, _ = _inventory(client)
        self.assertEqual(disks["a:vm-9001-disk-0"].classification, "attached")
        self.assertEqual(disks["a:vm-9002-disk-0"].classification, "free-floating")
        self.assertEqual(disks["a:vm-9001-disk-0"].discovered_by, ["band"])
        self.assertEqual(disks["a:vm-9001-disk-0"].to_dict()["discovered_by"], ["band"])

    def test_guest_vmid_disk_with_bpd_serial_is_found_and_attached(self) -> None:
        client = self._client({777: {"scsi1": f"a:vm-777-disk-1,serial={_BPD},size=8G"}}, ["a:vm-777-disk-1"])
        disks, _, _, _ = _inventory(client)
        rec = disks["a:vm-777-disk-1"]
        self.assertEqual(rec.classification, "attached")
        self.assertEqual(rec.holder_vmid, 777)
        self.assertEqual(rec.discovered_by, ["serial"])
        self.assertEqual(rec.to_dict()["discovered_by"], ["serial"])

    def test_parker_vmid_disk_named_in_a_sentinel_is_found_and_parked(self) -> None:
        desc = "<!--BOSH:" + json.dumps({"bosh_parked_disks": {_BPD: {
            "volid": "a:vm-90656-disk-0", "disk_cid": "cid-1", "node": "pve1"}}}) + "-->"
        client = self._client(
            {90656: {"scsi0": "a:vm-90656-disk-0", "description": desc}}, ["a:vm-90656-disk-0"])
        disks, _, _, _ = _inventory(client)
        rec = disks["a:vm-90656-disk-0"]
        self.assertEqual(rec.classification, "parked")
        self.assertEqual(rec.disk_cid, "cid-1")
        self.assertEqual(rec.discovered_by, ["parker", "sentinel"])

    def test_attached_sentinel_full_volid_key_names_the_volume(self) -> None:
        desc = "<!--BOSH:" + json.dumps({"bosh_attached_disks": {"a:vm-777-disk-2": "cid-2"}}) + "-->"
        client = self._client(
            {777: {"scsi2": "a:vm-777-disk-2", "description": desc}}, ["a:vm-777-disk-2"])
        disks, _, _, _ = _inventory(client)
        self.assertEqual(disks["a:vm-777-disk-2"].classification, "attached")
        self.assertEqual(disks["a:vm-777-disk-2"].discovered_by, ["sentinel"])

    def test_renamed_disk_two_guests_name_is_listed_as_shared(self) -> None:
        client = self._client({
            777: {"scsi1": f"a:vm-777-disk-1,serial={_BPD}"},
            778: {"unused0": "a:vm-777-disk-1"},
        }, ["a:vm-777-disk-1"])
        disks, _, report, _ = _inventory(client)
        self.assertIn("a:vm-777-disk-1", disks)
        self.assertEqual([r.volid for r in report.records], ["a:vm-777-disk-1"])
        self.assertEqual({ref["vmid"] for ref in report.records[0].references}, {777, 778})

    def test_non_bosh_volume_outside_the_band_is_ignored(self) -> None:
        client = self._client({
            777: {"scsi1": "a:vm-777-disk-1,serial=guest-serial", "scsi2": "a:vm-777-disk-2"},
        }, ["a:vm-777-disk-1", "a:vm-777-disk-2", "a:vm-31000-disk-0"])
        disks, _, _, _ = _inventory(client)
        self.assertEqual(disks, {})

    def test_unreadable_guest_is_warned_about(self) -> None:
        client = self._client({777: None}, ["a:vm-777-disk-1"])
        _, _, _, err = _inventory(client)
        self.assertIn("could not be read", err)

    def test_json_report_carries_discovered_by(self) -> None:
        client = self._client({777: {"scsi1": f"a:vm-777-disk-1,serial={_BPD}"}}, ["a:vm-777-disk-1"])
        disks, parkers, report, _ = _inventory(client)
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_json_report(list(disks.values()), parkers, _make_cfg(), report, [])
        out = json.loads(buf.getvalue())
        self.assertEqual(out["disks"][0]["discovered_by"], ["serial"])
        self.assertEqual(out["summary"]["attached"], 1)
        self.assertEqual(out["skipped_storages"], [])


def _sentinel(data: dict) -> str:
    return "<!--BOSH:" + json.dumps(data) + "-->"


class _FailClient(_DiscoveryClient):
    """A discovery stub whose soft and hard config reads fail for chosen VMIDs."""

    def __init__(self, *args, soft_fail=(), hard_fail=(), **kw) -> None:
        super().__init__(*args, **kw)
        self.soft_fail = set(soft_fail)
        self.hard_fail = set(hard_fail)
        self.hard_reads: list = []

    def vm_config_soft(self, node: str, vmid: int):
        if vmid in self.soft_fail:
            return None
        return self.configs.get(vmid)

    def vm_config(self, node: str, vmid: int):
        self.hard_reads.append(vmid)
        if vmid in self.hard_fail:
            raise SystemExit(2)
        return self.configs.get(vmid)


class _ViewsClient(_DiscoveryClient):
    """A discovery stub that answers both config views, one pair per VM."""

    def __init__(self, *args, views=None, **kw) -> None:
        super().__init__(*args, **kw)
        self.views = views or {}

    def vm_views_soft(self, node: str, vmid: int):
        return self.views.get(vmid)

    def vm_views(self, node: str, vmid: int):
        return self.views.get(vmid)


_FIX_ROWS = [
    {"vmid": 777, "node": "pve1", "name": "web-0", "type": "qemu"},
    {"vmid": 778, "node": "pve1", "name": "web-1", "type": "qemu"},
    {"vmid": 500, "node": "pve1", "name": "operator-vm", "type": "qemu"},
    {"vmid": 90656, "node": "pve1", "name": "bosh-parker-90656", "type": "qemu", "tags": "bosh-cpi;bosh-parker"},
]


class TestSentinelOnlyEvidence(unittest.TestCase):
    """A sentinel counts by its full volid only, and a lone sentinel proves little."""

    def test_bare_name_never_matches_a_volume_on_another_storage(self) -> None:
        desc = _sentinel({"bosh_attached_disks": {"vm-500-disk-0": "cid"}})
        client = _DiscoveryClient(
            _FIX_ROWS, {777: {"scsi1": "a:vm-500-disk-0", "description": desc}, 500: {"unused0": "b:vm-500-disk-0"}},
            [_images("a"), _images("b")], {"a": ["a:vm-500-disk-0"], "b": ["b:vm-500-disk-0"]},
        )
        disks, _, _, _ = _inventory(client)
        self.assertEqual(disks, {})

    def test_parse_returns_only_full_volids(self) -> None:
        desc = _sentinel({
            "bosh_attached_disks": {"vm-1-disk-0": "c", "a:vm-1-disk-1": "c", _BPD: "c"},
            "bosh_parked_disks": {_BPD: {"volid": "vm-2-disk-0"}, "a:vm-3-disk-0": {"volid": "a:vm-3-disk-0"}},
        })
        self.assertEqual(
            disk_audit.parse_sentinel_volume_names(desc), {"a:vm-1-disk-1", "a:vm-3-disk-0"})

    def _lone_sentinel(self, configs):
        desc = _sentinel({"bosh_parked_disks": {_BPD: {"volid": "a:vm-31000-disk-0", "slot": "scsi9"}}})
        configs = dict(configs)
        configs[90656] = {"description": desc}
        return _DiscoveryClient(_FIX_ROWS, configs, [_images("a")], {"a": ["a:vm-31000-disk-0"]})

    def test_lone_sentinel_volume_is_marked_not_plain(self) -> None:
        disks, parkers, report, _ = _inventory(self._lone_sentinel({}))
        rec = disks["a:vm-31000-disk-0"]
        self.assertEqual(rec.classification, "free-floating")
        self.assertTrue(rec.sentinel_only)
        self.assertEqual((rec.sentinel_vmid, rec.sentinel_name), (90656, "bosh-parker-90656"))
        d = rec.to_dict()
        self.assertTrue(d["found_only_by_sentinel"])
        self.assertEqual(d["sentinel_vmid"], 90656)
        self.assertEqual(d["sentinel_name"], "bosh-parker-90656")
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_human_report(list(disks.values()), parkers, _make_cfg(), report, [])
        out = buf.getvalue()
        self.assertIn("found only by a sentinel note on VM 90656 (bosh-parker-90656)", out)
        self.assertIn("`bosh disks --orphaned` before deleting", out)

    def test_band_orphan_is_not_marked(self) -> None:
        client = _DiscoveryClient(_FIX_ROWS, {}, [_images("a")], {"a": ["a:vm-9001-disk-0"]})
        disks, _, _, _ = _inventory(client)
        rec = disks["a:vm-9001-disk-0"]
        self.assertFalse(rec.sentinel_only)
        self.assertNotIn("found_only_by_sentinel", rec.to_dict())

    def test_guest_naming_the_volume_on_an_unused_entry_is_reported_as_its_holder(self) -> None:
        client = self._lone_sentinel({500: {"unused0": "a:vm-31000-disk-0"}})
        disks, parkers, report, _ = _inventory(client)
        rec = disks["a:vm-31000-disk-0"]
        self.assertTrue(rec.sentinel_only)
        self.assertEqual((rec.holder_vmid, rec.holder_name, rec.holder_slot), (500, "operator-vm", "unused0"))
        self.assertEqual(rec.to_dict()["holder_slot"], "unused0")
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_human_report(list(disks.values()), parkers, _make_cfg(), report, [])
        out = buf.getvalue()
        self.assertIn("DO NOT DELETE: VM 500 (operator-vm) still names it on unused0, so PVE references it", out)
        self.assertLess(out.index("DO NOT DELETE"), out.index("found only by a sentinel note"))
        self.assertTrue(rec.to_dict()["held_by_unused_entry"])

    def test_band_volume_named_on_an_unused_entry_gets_the_holder(self) -> None:
        client = _DiscoveryClient(
            _FIX_ROWS, {500: {"unused0": "a:vm-9001-disk-0"}}, [_images("a")], {"a": ["a:vm-9001-disk-0"]},
        )
        disks, _, _, _ = _inventory(client)
        rec = disks["a:vm-9001-disk-0"]
        self.assertEqual(rec.classification, "free-floating")
        self.assertFalse(rec.sentinel_only)
        self.assertEqual((rec.holder_vmid, rec.holder_slot), (500, "unused0"))
        self.assertTrue(rec.to_dict()["held_by_unused_entry"])

    def test_unheld_volume_has_no_held_key(self) -> None:
        disks, _, _, _ = _inventory(_DiscoveryClient(_FIX_ROWS, {}, [_images("a")], {"a": ["a:vm-9001-disk-0"]}))
        self.assertNotIn("held_by_unused_entry", disks["a:vm-9001-disk-0"].to_dict())

    def test_exit_line_names_the_holding_guest(self) -> None:
        import os
        import tempfile
        client = self._lone_sentinel({500: {"unused0": "a:vm-31000-disk-0"}})
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"}, fh)
            path = fh.name
        original = disk_audit.PVEClient
        disk_audit.PVEClient = lambda cfg: client
        err = io.StringIO()
        try:
            with redirect_stdout(io.StringIO()), redirect_stderr(err):
                disk_audit.main(["--config", path])
        finally:
            disk_audit.PVEClient = original
            os.unlink(path)
        self.assertIn("still named by VM 500", err.getvalue())

    def test_exit_code_stays_one_for_a_lone_sentinel_volume(self) -> None:
        import os
        import tempfile
        client = self._lone_sentinel({})
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"}, fh)
            path = fh.name
        original = disk_audit.PVEClient
        disk_audit.PVEClient = lambda cfg: client
        try:
            with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                rc = disk_audit.main(["--config", path])
        finally:
            disk_audit.PVEClient = original
            os.unlink(path)
        self.assertEqual(rc, 1)


class TestFailClosedConfigReads(unittest.TestCase):
    """An unreadable guest or parker ends the run, whatever the volumes are named."""

    _HELD = {777: {"scsi1": f"a:vm-777-disk-2,serial={_BPD}"}}

    def test_unreadable_holder_with_no_band_volume_fails_the_run(self) -> None:
        desc = _sentinel({"bosh_parked_disks": {_BPD: {"volid": "a:vm-777-disk-2", "slot": "scsi3"}}})
        client = _FailClient(
            _FIX_ROWS, {**self._HELD, 90656: {"description": desc}}, [_images("a")],
            {"a": ["a:vm-777-disk-2"]}, soft_fail={777}, hard_fail={777},
        )
        with self.assertRaises(SystemExit) as ctx:
            _inventory(client)
        self.assertEqual(ctx.exception.code, 2)

    def test_unreadable_parker_with_no_band_volume_fails_the_run(self) -> None:
        client = _FailClient(
            _FIX_ROWS, {777: {"scsi0": "a:vm-777-disk-0"}, 90656: {"scsi0": f"a:vm-90656-disk-0,serial={_BPD}"}},
            [_images("a")], {"a": ["a:vm-777-disk-0", "a:vm-90656-disk-0"]}, soft_fail={90656}, hard_fail={90656},
        )
        with self.assertRaises(SystemExit) as ctx:
            _inventory(client)
        self.assertEqual(ctx.exception.code, 2)

    def test_failed_soft_read_falls_back_to_a_hard_read_that_can_succeed(self) -> None:
        client = _FailClient(
            _FIX_ROWS, dict(self._HELD), [_images("a")], {"a": ["a:vm-777-disk-2"]}, soft_fail={777},
        )
        disks, _, _, _ = _inventory(client)
        self.assertIn(777, client.hard_reads)
        self.assertEqual(disks["a:vm-777-disk-2"].classification, "attached")

    def test_parker_whose_step_four_read_failed_is_hard_read_again(self) -> None:
        client = _FailClient(
            _FIX_ROWS, {777: {}}, [_images("a")], {"a": ["a:vm-9001-disk-0"]}, soft_fail={90656},
        )
        _, parkers, _, _ = _inventory(client)
        self.assertGreaterEqual(client.hard_reads.count(90656), 2)
        self.assertFalse(parkers[0].config_read)

    def test_no_volumes_keeps_the_soft_reads(self) -> None:
        client = _FailClient(_FIX_ROWS, {}, [_images("a")], {"a": []}, soft_fail={777}, hard_fail={777})
        _, _, report, _ = _inventory(client)
        self.assertIn(777, report.unreadable_vmids)


class TestSkippedStorageReport(unittest.TestCase):
    """A skipped storage shows up in the report, the JSON, and the return value."""

    def _client(self, **flags):
        return _DiscoveryClient(
            [], {}, [_images("off", **flags), _images("on")],
            {"off": ["off:vm-9001-disk-0"], "on": ["on:vm-9002-disk-0"]},
        )

    def test_collect_inventory_returns_the_skipped_tuples(self) -> None:
        skipped = _inventory_skipped(self._client(enabled=0))
        self.assertEqual(len(skipped), 1)
        self.assertEqual(skipped[0][:2], ("pve1", "off"))
        self.assertIn("disabled", skipped[0][2])
        self.assertEqual(_inventory_skipped(_DiscoveryClient([], {}, [_images("on")], {"on": []})), [])

    def test_inactive_storage_is_worded_as_partial_coverage(self) -> None:
        skipped = _inventory_skipped(self._client(active=0))
        self.assertIn("not active", skipped[0][2])
        self.assertIn("partial", skipped[0][2])

    def test_human_header_lists_the_skipped_storages(self) -> None:
        skipped = _inventory_skipped(self._client(enabled=0))
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_human_report([], [], _make_cfg(), None, skipped)
        out = buf.getvalue()
        self.assertIn("Skipped storages", out)
        self.assertIn("are not in this report", out)
        self.assertIn("node pve1 storage off:", out)
        self.assertLess(out.index("Parker band"), out.index("Skipped storages"))
        self.assertLess(out.index("Skipped storages"), out.index("Total disk volumes"))

    def test_human_header_omits_the_block_when_nothing_was_skipped(self) -> None:
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_human_report([], [], _make_cfg(), None, [])
        self.assertNotIn("Skipped storages", buf.getvalue())

    def test_json_carries_a_skipped_storages_array(self) -> None:
        skipped = _inventory_skipped(self._client(active="0"))
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_json_report([], [], _make_cfg(), None, skipped)
        out = json.loads(buf.getvalue())
        self.assertEqual(out["skipped_storages"][0]["node"], "pve1")
        self.assertEqual(out["skipped_storages"][0]["storage"], "off")
        self.assertIn("reason", out["skipped_storages"][0])

    def test_exit_code_is_unchanged_by_a_skipped_storage(self) -> None:
        import os
        import tempfile
        client = self._client(enabled=0)
        client.content["on"] = []
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"}, fh)
            path = fh.name
        original = disk_audit.PVEClient
        disk_audit.PVEClient = lambda cfg: client
        try:
            with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                rc = disk_audit.main(["--config", path])
        finally:
            disk_audit.PVEClient = original
            os.unlink(path)
        self.assertEqual(rc, 0)


class TestParkerSlotsAndCrashedTransfers(unittest.TestCase):
    """A volume on a parker's bus slot is ours, even in a crashed transfer window."""

    def test_crash_window_volume_on_a_parker_slot_is_found(self) -> None:
        desc = _sentinel({"bosh_parked_disks": {_BPD: {"volid": "a:vm-777-disk-2", "slot": "scsi3", "disk_cid": "c"}}})
        client = _DiscoveryClient(
            _FIX_ROWS, {90656: {"scsi3": "a:vm-90656-disk-1", "description": desc}}, [_images("a")],
            {"a": ["a:vm-90656-disk-1"]},
        )
        disks, parkers, _, _ = _inventory(client)
        rec = disks["a:vm-90656-disk-1"]
        self.assertEqual(rec.classification, "parked")
        self.assertIn("parker", rec.discovered_by)
        self.assertEqual(parkers[0].disk_count, 1)

    def test_any_bus_slot_volume_of_a_parker_counts_without_a_sentinel(self) -> None:
        client = _DiscoveryClient(
            _FIX_ROWS, {90656: {"scsi0": "a:vm-90656-disk-0"}}, [_images("a")], {"a": ["a:vm-90656-disk-0"]})
        disks, _, _, _ = _inventory(client)
        self.assertEqual(disks["a:vm-90656-disk-0"].discovered_by, ["parker"])
        self.assertEqual(disks["a:vm-90656-disk-0"].classification, "parked")

    def test_untagged_vm_in_the_parker_band_does_not_count(self) -> None:
        rows = [{"vmid": 90700, "node": "pve1", "name": "intruder", "type": "qemu"}]
        client = _DiscoveryClient(rows, {90700: {"scsi0": "a:vm-90700-disk-0"}}, [_images("a")],
                                  {"a": ["a:vm-90700-disk-0"]})
        disks, _, _, _ = _inventory(client)
        self.assertEqual(disks, {})

    def test_tagged_vm_outside_the_parker_band_does_not_count(self) -> None:
        rows = [{"vmid": 600, "node": "pve1", "name": "tagged", "type": "qemu", "tags": "bosh-parker"}]
        client = _DiscoveryClient(rows, {600: {"scsi0": "a:vm-600-disk-0"}}, [_images("a")],
                                  {"a": ["a:vm-600-disk-0"]})
        disks, _, _, _ = _inventory(client)
        self.assertEqual(disks, {})

    def test_slot_field_names_the_volume_on_that_slot(self) -> None:
        desc = _sentinel({"bosh_parked_disks": {_BPD: {"volid": "a:vm-1-disk-0", "slot": "scsi3"}}})
        got = disk_audit.parse_sentinel_volume_names(desc, {"scsi3": "a:vm-90656-disk-1,size=8G"})
        self.assertEqual(got, {"a:vm-1-disk-0", "a:vm-90656-disk-1"})
        self.assertEqual(disk_audit.parse_sentinel_volume_names(desc, {}), {"a:vm-1-disk-0"})


class TestPendingViews(unittest.TestCase):
    """A key counts as held when either the current or the pending view names it."""

    _ROWS = [
        {"key": "scsi1", "value": f"a:vm-777-disk-2,serial={_BPD}", "delete": 1},
        {"key": "scsi2", "pending": f"a:vm-777-disk-3,serial=bpd-aabbccddeeff0011"},
        {"key": "scsi3", "value": "a:vm-777-disk-4", "pending": "a:vm-777-disk-5"},
        {"key": "digest", "value": "abc"},
        {"key": "memory", "value": 2048},
    ]

    def test_parse_pending_views_shape(self) -> None:
        current, applied = disk_audit.parse_pending_views(self._ROWS)
        self.assertIn("scsi1", current)
        self.assertNotIn("scsi1", applied)
        self.assertNotIn("scsi2", current)
        self.assertEqual(applied["scsi2"], "a:vm-777-disk-3,serial=bpd-aabbccddeeff0011")
        self.assertEqual(current["scsi3"], "a:vm-777-disk-4")
        self.assertEqual(applied["scsi3"], "a:vm-777-disk-5")
        self.assertEqual(applied["memory"], "2048")
        self.assertIsNone(disk_audit.parse_pending_views(None))
        self.assertIsNone(disk_audit.parse_pending_views({"scsi0": "x"}))

    def test_merged_config_keeps_a_pending_delete(self) -> None:
        merged = disk_audit.merge_views(disk_audit.parse_pending_views(self._ROWS))
        self.assertIn("scsi1", merged)
        self.assertEqual(merged["scsi3"], "a:vm-777-disk-5")

    def test_pending_delete_still_holds_the_volume_and_carries_its_serial(self) -> None:
        views = {777: ({"scsi1": f"a:vm-777-disk-2,serial={_BPD}"}, {})}
        desc = _sentinel({"bosh_parked_disks": {_BPD: {"volid": "a:vm-777-disk-2", "slot": "scsi3"}}})
        views[90656] = ({"description": desc}, {"description": desc})
        client = _ViewsClient(_FIX_ROWS, {}, [_images("a")], {"a": ["a:vm-777-disk-2"]}, views=views)
        disks, _, _, _ = _inventory(client)
        rec = disks["a:vm-777-disk-2"]
        self.assertEqual(rec.classification, "attached")
        self.assertEqual(rec.holder_vmid, 777)
        self.assertEqual(rec.discovered_by, ["serial", "sentinel"])

    def test_pending_only_key_is_held(self) -> None:
        views = {777: ({}, {"scsi1": f"a:vm-777-disk-2,serial={_BPD}"})}
        client = _ViewsClient(_FIX_ROWS, {}, [_images("a")], {"a": ["a:vm-777-disk-2"]}, views=views)
        disks, _, _, _ = _inventory(client)
        self.assertEqual(disks["a:vm-777-disk-2"].classification, "attached")

    def test_real_client_reads_the_pending_endpoint_with_get_only(self) -> None:
        from unittest import mock
        cfg = disk_audit.AuditConfig({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"})
        client = disk_audit.PVEClient(cfg)
        seen = []

        def urlopen(req, context=None, timeout=None):
            seen.append((req.get_method(), req.full_url))
            return _FakeResponse(self._ROWS)

        with mock.patch.object(disk_audit.urllib.request, "urlopen", urlopen):
            hard = client.vm_views("pve1", 777)
            soft = client.vm_views_soft("pve1", 777)
        self.assertEqual(hard, soft)
        self.assertEqual([m for m, _ in seen], ["GET", "GET"])
        self.assertTrue(all(u.endswith("/nodes/pve1/qemu/777/pending") for _, u in seen))

    def test_failed_pending_read_still_fails_closed(self) -> None:
        import urllib.error
        from unittest import mock
        cfg = disk_audit.AuditConfig({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"})
        client = disk_audit.PVEClient(cfg)

        def urlopen(req, context=None, timeout=None):
            if req.full_url.endswith("/pending"):
                err = urllib.error.HTTPError(req.full_url, 500, "boom", None, None)
                self.addCleanup(err.close)
                raise err
            return _FakeResponse([{"vmid": 777, "node": "pve1", "type": "qemu"}] if "resources" in req.full_url
                                 else [{"node": "pve1", "status": "online"}] if req.full_url.endswith("/nodes")
                                 else [_images("a")] if req.full_url.endswith("/storage")
                                 else [{"volid": "a:vm-777-disk-1", "size": 1}])

        with mock.patch.object(disk_audit.urllib.request, "urlopen", urlopen):
            with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as ctx:
                disk_audit.collect_inventory(client, _audit_cfg())
        self.assertEqual(ctx.exception.code, 2)


class TestDiscoveredByShape(unittest.TestCase):
    """Every disk record carries discovered_by, so readers see one shape."""

    def test_band_only_record_carries_band(self) -> None:
        rec = disk_audit.DiskRecord("a:vm-9001-disk-0", "a", "pve1", 1)
        self.assertEqual(rec.to_dict()["discovered_by"], ["band"])

    def test_json_report_has_discovered_by_on_every_disk(self) -> None:
        client = _DiscoveryClient(
            _FIX_ROWS, {777: {"scsi1": "a:vm-9001-disk-0", "scsi2": f"a:vm-777-disk-2,serial={_BPD}"}},
            [_images("a")], {"a": ["a:vm-9001-disk-0", "a:vm-777-disk-2"]},
        )
        disks, parkers, report, _ = _inventory(client)
        buf = io.StringIO()
        with redirect_stdout(buf):
            disk_audit.print_json_report(list(disks.values()), parkers, _make_cfg(), report, [])
        out = json.loads(buf.getvalue())
        self.assertEqual(len(out["disks"]), 2)
        self.assertTrue(all("discovered_by" in d for d in out["disks"]))
        self.assertEqual(
            {d["volid"]: d["discovered_by"] for d in out["disks"]},
            {"a:vm-9001-disk-0": ["band"], "a:vm-777-disk-2": ["serial"]},
        )


class TestDuplicateSerials(unittest.TestCase):
    """One bpd- token on two volumes draws a warning that names both."""

    def test_clone_sharing_a_serial_warns_with_both_volids_and_holders(self) -> None:
        client = _DiscoveryClient(
            _FIX_ROWS,
            {777: {"scsi1": f"a:vm-777-disk-1,serial={_BPD}"}, 778: {"scsi1": f"a:vm-778-disk-1,serial={_BPD}"}},
            [_images("a")], {"a": ["a:vm-777-disk-1", "a:vm-778-disk-1"]},
        )
        disks, _, _, err = _inventory(client)
        self.assertEqual(len(disks), 2)
        self.assertIn(f"serial {_BPD} appears on 2 different volumes", err)
        self.assertIn("a:vm-777-disk-1 (held by VM 777 (web-0))", err)
        self.assertIn("a:vm-778-disk-1 (held by VM 778 (web-1))", err)

    def test_one_volume_named_by_two_guests_is_not_a_duplicate_serial(self) -> None:
        client = _DiscoveryClient(
            _FIX_ROWS,
            {777: {"scsi1": f"a:vm-777-disk-1,serial={_BPD}"}, 778: {"scsi1": f"a:vm-777-disk-1,serial={_BPD}"}},
            [_images("a")], {"a": ["a:vm-777-disk-1"]},
        )
        _, _, _, err = _inventory(client)
        self.assertNotIn("different volumes", err)

    def test_distinct_serials_do_not_warn(self) -> None:
        client = _DiscoveryClient(
            _FIX_ROWS,
            {777: {"scsi1": f"a:vm-777-disk-1,serial={_BPD}"}, 778: {"scsi1": "a:vm-778-disk-1,serial=bpd-1122334455667788"}},
            [_images("a")], {"a": ["a:vm-777-disk-1", "a:vm-778-disk-1"]},
        )
        _, _, _, err = _inventory(client)
        self.assertNotIn("different volumes", err)


class TestPendingViewShapes(unittest.TestCase):
    """A reply the audit cannot read as config rows ends the run."""

    def _client(self, data):
        cfg = disk_audit.AuditConfig({"host": "pve.example.com", "user": "root@pam", "api_token": "root@pam!t=x"})
        client = disk_audit.PVEClient(cfg)
        client.get = lambda path, allow_missing=False: data
        return client

    def _dies(self, data) -> None:
        with redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as ctx:
                self._client(data).vm_views("pve1", 777)
        self.assertEqual(ctx.exception.code, 2)

    def test_non_list_body_ends_the_run(self) -> None:
        self._dies({"key": "scsi0"})

    def test_malformed_row_ends_the_run(self) -> None:
        self._dies([{"key": "scsi0", "value": "a:vm-1-disk-0"}, "oops"])
        self._dies([{"value": "x"}])

    def test_missing_guest_and_good_rows_still_read(self) -> None:
        self.assertIsNone(self._client(None).vm_views("pve1", 777))
        views = self._client([{"key": "scsi0", "value": "a:vm-1-disk-0"}]).vm_views("pve1", 777)
        self.assertEqual(views[0], {"scsi0": "a:vm-1-disk-0"})


class TestPendingSerialIsNotAClone(unittest.TestCase):
    def test_same_disk_with_a_new_volid_in_the_pending_view_does_not_warn(self) -> None:
        client = _ViewsClient(
            _FIX_ROWS, {}, [_images("a")], {"a": ["a:vm-777-disk-1", "a:vm-777-disk-2"]},
            views={
                777: (
                    {"scsi1": f"a:vm-777-disk-1,serial={_BPD}"},
                    {"scsi1": f"a:vm-777-disk-2,serial={_BPD}"},
                ),
            },
        )
        _, _, _, err = _inventory(client)
        self.assertNotIn("different volumes", err)


if __name__ == "__main__":
    unittest.main()
