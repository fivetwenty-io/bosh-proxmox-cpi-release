#!/usr/bin/env python3
"""Unit tests for scripts/_rundoc.py.

Run with:
    python3 scripts/_rundoc_test.py

Uses only stdlib plus PyYAML (a _rundoc import); everything runs against a
temporary directory.
"""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _rundoc  # noqa: E402


def _meta(date: str, started: str, **extra: object) -> dict:
    meta = {
        "date": date,
        "started": started,
        "finished": started,
        "wall_hms": "0:00:01",
        "command_line": "./scripts/test unit",
        "exit_code": 0,
        "verdict": "PASSED",
    }
    meta.update(extra)
    return meta


class StartTimeSuffixTest(unittest.TestCase):
    def test_derives_hhmmss_from_started(self) -> None:
        meta = _meta("2026-08-21", "2026-08-21 13:18:02")
        self.assertEqual(_rundoc.start_time_suffix(meta), "-131802")

    def test_missing_started_falls_back_to_empty(self) -> None:
        self.assertEqual(_rundoc.start_time_suffix({"date": "2026-08-21"}), "")

    def test_malformed_started_falls_back_to_empty(self) -> None:
        meta = _meta("2026-08-21", "sometime today")
        self.assertEqual(_rundoc.start_time_suffix(meta), "")


def _write(runs_dir: Path, meta: dict, doc_suffix: str = "") -> Path:
    return _rundoc.write_run_doc(
        runs_dir=runs_dir,
        meta=meta,
        meta_mark="TEST-RUN",
        title="Test run",
        provenance="Written by _rundoc_test.py.",
        verdict_lines=["**Result: PASSED.**", ""],
        doc_suffix=doc_suffix,
    )


class WriteRunDocTest(unittest.TestCase):
    def test_same_day_runs_write_distinct_documents(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            runs = Path(tmp)
            first = _write(runs, _meta("2026-08-21", "2026-08-21 02:30:00"))
            second = _write(runs, _meta("2026-08-21", "2026-08-21 13:18:02"))
            self.assertEqual(first.name, "2026-08-21-023000.md")
            self.assertEqual(second.name, "2026-08-21-131802.md")
            self.assertTrue(first.exists())
            self.assertTrue(second.exists())

    def test_doc_suffix_appends_after_the_time(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            doc = _write(
                Path(tmp),
                _meta("2026-08-21", "2026-08-21 02:30:00"),
                doc_suffix="-core",
            )
            self.assertEqual(doc.name, "2026-08-21-023000-core.md")

    def test_regeneration_from_same_meta_is_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            meta = _meta("2026-08-21", "2026-08-21 02:30:00")
            first = _write(Path(tmp), meta)
            second = _write(Path(tmp), meta)
            self.assertEqual(first, second)
            self.assertEqual(len(list(Path(tmp).glob("*.md"))), 1)


class CollectRunHistoryTest(unittest.TestCase):
    def test_started_tiebreaks_same_day_runs(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            runs = Path(tmp)
            _write(runs, _meta("2026-08-20", "2026-08-20 02:30:00"))
            _write(runs, _meta("2026-08-21", "2026-08-21 02:30:00"))
            _write(runs, _meta("2026-08-21", "2026-08-21 13:18:02"))
            history = _rundoc.collect_run_history(runs, "TEST-RUN")
            self.assertEqual(
                [m["started"] for m in history],
                [
                    "2026-08-21 13:18:02",
                    "2026-08-21 02:30:00",
                    "2026-08-20 02:30:00",
                ],
            )

    def test_legacy_date_only_documents_still_collect(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            runs = Path(tmp)
            meta = _meta("2026-08-19", "2026-08-19 02:30:00")
            (runs / "2026-08-19.md").write_text(
                "# Test run\n\n<!-- TEST-RUN "
                + json.dumps(meta, sort_keys=True)
                + " -->\n"
            )
            history = _rundoc.collect_run_history(runs, "TEST-RUN")
            self.assertEqual(len(history), 1)
            self.assertEqual(history[0]["_doc"], "2026-08-19.md")


if __name__ == "__main__":
    unittest.main()
