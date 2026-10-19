"""Tests for the Director UUID record in a certification slot."""
from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import _directorrefs as refs  # noqa: E402

A = "11111111-2222-3333-4444-555555555555"
B = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"


class DirectorRecordTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.slot = Path(self.tmp.name)

    def test_an_absent_record_is_empty(self) -> None:
        self.assertEqual(refs.load(self.slot), [])

    def test_add_appends_once(self) -> None:
        self.assertTrue(refs.add(self.slot, A))
        self.assertFalse(refs.add(self.slot, A.upper()))
        self.assertTrue(refs.add(self.slot, B))
        self.assertEqual(refs.load(self.slot), [A, B])
        self.assertEqual((self.slot / refs.RECORD_FILE).read_text(), f"{A}\n{B}\n")

    def test_add_refuses_a_non_uuid(self) -> None:
        with self.assertRaises(ValueError):
            refs.add(self.slot, "not-a-uuid; rm -rf /")
        self.assertEqual(refs.load(self.slot), [])

    def test_load_skips_malformed_and_repeated_lines(self) -> None:
        (self.slot / refs.RECORD_FILE).write_text(f"junk\n{A}\n\n{A}\n--flag\n{B}\n")
        self.assertEqual(refs.load(self.slot), [A, B])

    def test_remove_drops_only_that_uuid(self) -> None:
        refs.add(self.slot, A)
        refs.add(self.slot, B)
        refs.remove(self.slot, A)
        self.assertEqual(refs.load(self.slot), [B])
        refs.remove(self.slot, B)
        self.assertEqual(refs.load(self.slot), [])

    def test_remove_with_no_record_is_a_no_op(self) -> None:
        refs.remove(self.slot, A)
        self.assertFalse((self.slot / refs.RECORD_FILE).exists())


if __name__ == "__main__":
    unittest.main()
