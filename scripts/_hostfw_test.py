#!/usr/bin/env python3
"""Unit tests for scripts/_hostfw.py.

Run with:
    python3 scripts/_hostfw_test.py

Uses only stdlib; no PVE network connection required.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _hostfw

# ---------------------------------------------------------------------------
# Fixture
# ---------------------------------------------------------------------------

SUBNET = "172.31.0.0/24"
OTHER_SUBNET = "10.64.64.0/18"

# A realistic host.fw that already has [OPTIONS] + a pre-existing bloc for
# the 10.64.64.0/18 management range.  The cpitest rules are intentionally
# absent so add_rule_block tests start from a clean state.
FIXTURE_WITHOUT_BLOCK = """\
[OPTIONS]
enable: 1

[RULES]
IN ACCEPT -source 10.64.64.0/18 -p tcp -dport 8006 -log nolog # management net
IN ACCEPT -source 10.64.64.0/18 -p icmp -log nolog # management icmp
"""

# Same fixture but with the cpitest bloc already present.
FIXTURE_WITH_BLOCK = """\
[OPTIONS]
enable: 1

[RULES]
IN ACCEPT -source 172.31.0.0/24 -p tcp -dport 8006 -log nolog # cpitest SDN -> PVE API (CPI)
IN ACCEPT -source 172.31.0.0/24 -p icmp -log nolog # cpitest SDN -> host icmp
IN ACCEPT -source 10.64.64.0/18 -p tcp -dport 8006 -log nolog # management net
IN ACCEPT -source 10.64.64.0/18 -p icmp -log nolog # management icmp
"""

# Fixture with no [RULES] section at all.
FIXTURE_NO_RULES = """\
[OPTIONS]
enable: 1
"""


# ---------------------------------------------------------------------------
# has_rule_block
# ---------------------------------------------------------------------------

class TestHasRuleBlock(unittest.TestCase):

    def test_returns_false_when_rules_absent(self) -> None:
        self.assertFalse(_hostfw.has_rule_block(FIXTURE_WITHOUT_BLOCK, SUBNET))

    def test_returns_true_when_rules_present(self) -> None:
        self.assertTrue(_hostfw.has_rule_block(FIXTURE_WITH_BLOCK, SUBNET))

    def test_other_subnet_not_detected(self) -> None:
        self.assertFalse(_hostfw.has_rule_block(FIXTURE_WITHOUT_BLOCK, SUBNET))
        # The 10.64.64.0/18 rules do NOT carry the cpitest marker, so they
        # must not count as a hit for any subnet value.
        self.assertFalse(_hostfw.has_rule_block(FIXTURE_WITHOUT_BLOCK, OTHER_SUBNET))

    def test_empty_content_returns_false(self) -> None:
        self.assertFalse(_hostfw.has_rule_block("", SUBNET))

    def test_empty_subnet_returns_false(self) -> None:
        self.assertFalse(_hostfw.has_rule_block(FIXTURE_WITH_BLOCK, ""))

    def test_none_subnet_returns_false(self) -> None:
        # None should be treated the same as empty: falsy.
        self.assertFalse(_hostfw.has_rule_block(FIXTURE_WITH_BLOCK, None))  # type: ignore[arg-type]


# ---------------------------------------------------------------------------
# add_rule_block
# ---------------------------------------------------------------------------

class TestAddRuleBlock(unittest.TestCase):

    def test_rules_inserted_after_rules_header(self) -> None:
        result = _hostfw.add_rule_block(FIXTURE_WITHOUT_BLOCK, SUBNET)
        lines = result.splitlines()
        rules_idx = lines.index("[RULES]")
        self.assertEqual(
            lines[rules_idx + 1],
            f"IN ACCEPT -source {SUBNET} -p tcp -dport 8006 -log nolog # cpitest SDN -> PVE API (CPI)",
        )
        self.assertEqual(
            lines[rules_idx + 2],
            f"IN ACCEPT -source {SUBNET} -p icmp -log nolog # cpitest SDN -> host icmp",
        )

    def test_both_rules_present_after_add(self) -> None:
        result = _hostfw.add_rule_block(FIXTURE_WITHOUT_BLOCK, SUBNET)
        self.assertTrue(_hostfw.has_rule_block(result, SUBNET))

    def test_idempotent_when_already_present(self) -> None:
        result = _hostfw.add_rule_block(FIXTURE_WITH_BLOCK, SUBNET)
        self.assertEqual(result, FIXTURE_WITH_BLOCK)

    def test_no_duplication_on_second_add(self) -> None:
        once = _hostfw.add_rule_block(FIXTURE_WITHOUT_BLOCK, SUBNET)
        twice = _hostfw.add_rule_block(once, SUBNET)
        # Count occurrences of the tcp rule line.
        tcp_rule = f"IN ACCEPT -source {SUBNET} -p tcp -dport 8006 -log nolog # cpitest SDN -> PVE API (CPI)"
        self.assertEqual(twice.count(tcp_rule), 1)

    def test_other_bloc_preserved(self) -> None:
        result = _hostfw.add_rule_block(FIXTURE_WITHOUT_BLOCK, SUBNET)
        self.assertIn("IN ACCEPT -source 10.64.64.0/18 -p tcp -dport 8006", result)
        self.assertIn("[OPTIONS]", result)
        self.assertIn("enable: 1", result)

    def test_rules_section_created_when_absent(self) -> None:
        result = _hostfw.add_rule_block(FIXTURE_NO_RULES, SUBNET)
        self.assertIn("[RULES]", result)
        self.assertTrue(_hostfw.has_rule_block(result, SUBNET))

    def test_rules_appended_after_existing_content_when_no_section(self) -> None:
        result = _hostfw.add_rule_block(FIXTURE_NO_RULES, SUBNET)
        # [OPTIONS] must still be present above [RULES].
        lines = result.splitlines()
        options_idx = lines.index("[OPTIONS]")
        rules_idx = lines.index("[RULES]")
        self.assertLess(options_idx, rules_idx)

    def test_empty_content_creates_section(self) -> None:
        result = _hostfw.add_rule_block("", SUBNET)
        self.assertIn("[RULES]", result)
        self.assertTrue(_hostfw.has_rule_block(result, SUBNET))

    def test_raises_on_empty_subnet(self) -> None:
        with self.assertRaises(ValueError):
            _hostfw.add_rule_block(FIXTURE_WITHOUT_BLOCK, "")

    def test_exact_rule_text(self) -> None:
        """Confirm byte-for-byte rule text matches the live format."""
        result = _hostfw.add_rule_block(FIXTURE_WITHOUT_BLOCK, SUBNET)
        self.assertIn(
            "IN ACCEPT -source 172.31.0.0/24 -p tcp -dport 8006 -log nolog # cpitest SDN -> PVE API (CPI)",
            result,
        )
        self.assertIn(
            "IN ACCEPT -source 172.31.0.0/24 -p icmp -log nolog # cpitest SDN -> host icmp",
            result,
        )

    def test_original_not_mutated(self) -> None:
        before = FIXTURE_WITHOUT_BLOCK
        _hostfw.add_rule_block(before, SUBNET)
        self.assertEqual(before, FIXTURE_WITHOUT_BLOCK)


# ---------------------------------------------------------------------------
# remove_rule_block
# ---------------------------------------------------------------------------

class TestRemoveRuleBlock(unittest.TestCase):

    def test_removes_cpitest_rules(self) -> None:
        result = _hostfw.remove_rule_block(FIXTURE_WITH_BLOCK, SUBNET)
        self.assertFalse(_hostfw.has_rule_block(result, SUBNET))

    def test_preserves_other_rules(self) -> None:
        result = _hostfw.remove_rule_block(FIXTURE_WITH_BLOCK, SUBNET)
        self.assertIn("IN ACCEPT -source 10.64.64.0/18 -p tcp -dport 8006", result)
        self.assertIn("IN ACCEPT -source 10.64.64.0/18 -p icmp -log nolog", result)

    def test_preserves_options_section(self) -> None:
        result = _hostfw.remove_rule_block(FIXTURE_WITH_BLOCK, SUBNET)
        self.assertIn("[OPTIONS]", result)
        self.assertIn("enable: 1", result)

    def test_idempotent_when_absent(self) -> None:
        result = _hostfw.remove_rule_block(FIXTURE_WITHOUT_BLOCK, SUBNET)
        self.assertEqual(result, FIXTURE_WITHOUT_BLOCK)

    def test_remove_then_add_roundtrip(self) -> None:
        removed = _hostfw.remove_rule_block(FIXTURE_WITH_BLOCK, SUBNET)
        restored = _hostfw.add_rule_block(removed, SUBNET)
        self.assertTrue(_hostfw.has_rule_block(restored, SUBNET))
        self.assertIn("IN ACCEPT -source 10.64.64.0/18", restored)

    def test_raises_on_empty_subnet(self) -> None:
        with self.assertRaises(ValueError):
            _hostfw.remove_rule_block(FIXTURE_WITH_BLOCK, "")

    def test_original_not_mutated(self) -> None:
        before = FIXTURE_WITH_BLOCK
        _hostfw.remove_rule_block(before, SUBNET)
        self.assertEqual(before, FIXTURE_WITH_BLOCK)

    def test_only_cpitest_lines_removed(self) -> None:
        """Management-net lines that share the subnet prefix are NOT removed."""
        # Build a fixture where the management net lines also reference the
        # cpitest subnet address (unlikely, but tests line-exact matching).
        custom = """\
[OPTIONS]
enable: 1

[RULES]
IN ACCEPT -source 172.31.0.0/24 -p tcp -dport 8006 -log nolog # cpitest SDN -> PVE API (CPI)
IN ACCEPT -source 172.31.0.0/24 -p icmp -log nolog # cpitest SDN -> host icmp
IN ACCEPT -source 172.31.0.0/24 -p tcp -dport 22 -log nolog # some other rule
"""
        result = _hostfw.remove_rule_block(custom, SUBNET)
        # The two cpitest-keyed lines are gone.
        self.assertNotIn("# cpitest SDN ->", result)
        # The unrelated rule for the same subnet must survive.
        self.assertIn("IN ACCEPT -source 172.31.0.0/24 -p tcp -dport 22", result)


# ---------------------------------------------------------------------------
# Round-trip: gawk reserved-name regression guard
# ---------------------------------------------------------------------------

class TestReservedNameRegression(unittest.TestCase):
    """Regression test for the gawk 'sub' reserved-name bug.

    The old awk invocation used ``-v sub=...`` where ``sub`` is a gawk
    builtin function name; gawk fatalled with:
        awk: fatal: cannot use gawk builtin 'sub' as variable name

    These helpers have no awk dependency, so the bug cannot recur here.
    The test verifies the full add→has→remove roundtrip completes without
    error, which is what the awk code was supposed to do.
    """

    def test_add_has_remove_roundtrip(self) -> None:
        content = FIXTURE_WITHOUT_BLOCK
        after_add = _hostfw.add_rule_block(content, SUBNET)
        self.assertTrue(_hostfw.has_rule_block(after_add, SUBNET))
        after_remove = _hostfw.remove_rule_block(after_add, SUBNET)
        self.assertFalse(_hostfw.has_rule_block(after_remove, SUBNET))
        # Other content intact throughout.
        for snapshot in (after_add, after_remove):
            self.assertIn("[OPTIONS]", snapshot)
            self.assertIn("enable: 1", snapshot)
            self.assertIn("IN ACCEPT -source 10.64.64.0/18", snapshot)


if __name__ == "__main__":
    unittest.main()
