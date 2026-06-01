"""Pure-Python helpers for editing the PVE host firewall file (host.fw).

The [RULES] section of /etc/pve/nodes/<node>/host.fw contains
line-oriented firewall rules.  These functions operate on the file
*content* as a string so they can be tested offline without any
host/ssh/network dependency.

Rule format emitted by add_rule_block (exact text, subnet substituted):

    IN ACCEPT -source <subnet> -p tcp -dport 8006 -log nolog # cpitest SDN -> PVE API (CPI)
    IN ACCEPT -source <subnet> -p icmp -log nolog # cpitest SDN -> host icmp

The comment suffix ``# cpitest SDN ->`` is both the removal key and the
idempotency key, together with the subnet.  The two lines are always
written together and removed together.
"""

from __future__ import annotations

_COMMENT_MARKER = "# cpitest SDN ->"

# ---- public API ------------------------------------------------------------


def has_rule_block(content: str, subnet: str) -> bool:
    """Return True if the keyed cpitest ACCEPT rules for *subnet* are present.

    Args:
        content: Full text of host.fw.
        subnet:  CIDR string, e.g. ``172.31.0.0/24``.

    Returns:
        True when both keyed rule lines are present in *content*.
    """
    if not subnet:
        return False
    tcp_rule, icmp_rule = _rule_lines(subnet)
    return tcp_rule in content and icmp_rule in content


def add_rule_block(content: str, subnet: str) -> str:
    """Insert the two keyed ACCEPT rules for *subnet* after the [RULES] header.

    Idempotent: returns *content* unchanged when the rules are already present.
    If ``[RULES]`` is absent, appends it followed by the two rules.

    Args:
        content: Full text of host.fw (may be empty or partial).
        subnet:  CIDR string, e.g. ``172.31.0.0/24``.

    Returns:
        Updated content string.  The original is never mutated.

    Raises:
        ValueError: *subnet* is empty or None.
    """
    if not subnet:
        raise ValueError("subnet must be a non-empty string")

    # Idempotent: already present -> no change.
    if has_rule_block(content, subnet):
        return content

    tcp_rule, icmp_rule = _rule_lines(subnet)
    block = tcp_rule + "\n" + icmp_rule + "\n"

    lines = content.splitlines(keepends=True)

    # Find the [RULES] header line.
    rules_idx = _find_rules_header(lines)

    if rules_idx is None:
        # No [RULES] section — append one followed by the two rules.
        # Ensure there is a trailing newline before the new section.
        if lines and not lines[-1].endswith("\n"):
            lines[-1] += "\n"
        lines.append("[RULES]\n")
        lines.append(block)
    else:
        # Insert the two rules immediately after the [RULES] line.
        lines.insert(rules_idx + 1, block)

    return "".join(lines)


def remove_rule_block(content: str, subnet: str) -> str:
    """Remove the keyed cpitest ACCEPT rules for *subnet*.

    Removes only the two lines that match the cpitest keyed pattern for
    *subnet*.  All other content — other rule blocs, [OPTIONS], comments —
    is preserved unchanged.  Idempotent: a no-op when the rules are absent.

    Args:
        content: Full text of host.fw.
        subnet:  CIDR string, e.g. ``172.31.0.0/24``.

    Returns:
        Updated content string.  The original is never mutated.

    Raises:
        ValueError: *subnet* is empty or None.
    """
    if not subnet:
        raise ValueError("subnet must be a non-empty string")

    if not has_rule_block(content, subnet):
        return content

    tcp_rule, icmp_rule = _rule_lines(subnet)
    keep: list[str] = []
    for line in content.splitlines(keepends=True):
        stripped = line.rstrip("\n").rstrip("\r")
        if stripped == tcp_rule or stripped == icmp_rule:
            continue
        keep.append(line)
    return "".join(keep)


# ---- internal helpers -------------------------------------------------------


def _rule_lines(subnet: str) -> tuple[str, str]:
    """Return the (tcp_rule, icmp_rule) strings for *subnet* (no newlines)."""
    tcp = (
        f"IN ACCEPT -source {subnet} -p tcp -dport 8006 -log nolog"
        f" {_COMMENT_MARKER} PVE API (CPI)"
    )
    icmp = (
        f"IN ACCEPT -source {subnet} -p icmp -log nolog"
        f" {_COMMENT_MARKER} host icmp"
    )
    return tcp, icmp


def _find_rules_header(lines: list[str]) -> "int | None":
    """Return the index of the ``[RULES]`` line, or None if absent."""
    for i, line in enumerate(lines):
        if line.strip() == "[RULES]":
            return i
    return None
