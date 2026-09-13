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
from contextlib import redirect_stdout
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


if __name__ == "__main__":
    unittest.main()
