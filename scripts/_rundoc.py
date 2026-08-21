"""Shared committed-run-document layer for the runner scripts.

Extracted from scripts/bats and scripts/certify so every runner that writes a
committed proof document under docs/certification/ reuses one implementation:
the small vars/stemcell helpers, the private-key console-log scrubber, the
run-document frame (title, meta comment, verdict, timings, environment,
artifacts), the run-history collector, and the README summary builder.

Each runner keeps what is genuinely its own — verdict derivation, meta fields,
and any extra report sections — and passes them in as parameters, so the
committed documents render byte-for-byte the same as before the extraction.

Import pattern (matches _integration and _report):
    import sys
    from pathlib import Path
    sys.path.insert(0, str(Path(__file__).parent))
    import _rundoc
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
import tarfile
from pathlib import Path
from typing import IO, Callable

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent))
from _report import SKIP, fmt_hms  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parent.parent
BOSH_VARS = REPO_ROOT / "manifests" / "bosh" / "vars.yml"
ENVS_DIR = REPO_ROOT / "manifests" / "envs"


# --------------------------------------------------------------------------- #
# Small helpers
# --------------------------------------------------------------------------- #

def _bosh_int_opt(path: Path, json_path: str) -> "str | None":
    """bosh int --path, returning None when the file or path is absent."""
    if not path.exists():
        return None
    proc = subprocess.run(
        ["bosh", "int", str(path), "--path", json_path],
        capture_output=True, text=True,
    )
    if proc.returncode != 0:
        return None
    return proc.stdout.rstrip("\n")


def env_vars_file(env_name: str) -> "Path | None":
    """manifests/envs/<env>/vars.yml when it exists (mirrors scripts/bosh)."""
    f = ENVS_DIR / env_name / "vars.yml"
    return f if f.exists() else None


def layered_var(env_name: str, json_path: str) -> "str | None":
    """Read a vars key with env-bundle layering: env vars.yml wins, base
    manifests/bosh/vars.yml fills the rest (mirrors scripts/bosh)."""
    env_f = env_vars_file(env_name)
    if env_f is not None:
        v = _bosh_int_opt(env_f, json_path)
        if v is not None:
            return v
    return _bosh_int_opt(BOSH_VARS, json_path)


def stemcell_manifest(path: Path) -> dict:
    """Read stemcell.MF (name, version, ...) from a stemcell tarball."""
    with tarfile.open(path, "r:gz") as tf:
        member = None
        for name in ("stemcell.MF", "./stemcell.MF"):
            try:
                member = tf.getmember(name)
                break
            except KeyError:
                continue
        if member is None:
            raise ValueError(f"no stemcell.MF inside {path}")
        fh = tf.extractfile(member)
        assert fh is not None
        return yaml.safe_load(fh.read())


def _md_escape(text: str) -> str:
    return text.replace("|", "\\|").replace("\n", " ")


class ScrubbedLog:
    """File-like wrapper that redacts private-key blocks from a console log.

    The runners' child processes can echo ephemeral private keys (BATS prints
    the full deployment spec; `bosh create-env` echoes the jumpbox keypair it
    generates into the vars-store); the console capture should not persist
    them.
    """

    def __init__(self, fh: "IO[str]") -> None:
        self.fh = fh
        self._in_key = False

    def write(self, line: str) -> None:
        is_begin = "-----BEGIN" in line and "PRIVATE KEY" in line
        is_end = "-----END" in line and "PRIVATE KEY" in line
        if is_begin:
            self.fh.write("[private key scrubbed]\n")
            self._in_key = not is_end
            return
        if self._in_key:
            if is_end:
                self._in_key = False
            return
        self.fh.write(line)

    def flush(self) -> None:
        self.fh.flush()


# --------------------------------------------------------------------------- #
# Run documents
# --------------------------------------------------------------------------- #

def run_meta_bullets(meta: dict) -> list[str]:
    """The wall-clock/command/exit-code bullets every run doc's Verdict has."""
    return [
        f"- Wall clock: {meta['wall_hms']} ({meta['started']} to {meta['finished']})",
        "",
        f"- Command: `{meta['command_line']}`",
        "",
        f"- Exit code: {meta['exit_code']}",
        "",
    ]


def start_time_suffix(meta: dict) -> str:
    """Time-of-day filename part derived from meta['started'].

    Run documents used to be named by date alone, so two runs on the same
    day overwrote each other locally and produced add/add conflicts between
    the CI report PRs. Deriving the suffix from the recorded start time keeps
    regeneration from a prior run's artifacts deterministic.
    """
    m = re.search(r"\b(\d{2}):(\d{2}):(\d{2})$", str(meta.get("started", "")))
    if not m:
        return ""
    return "-" + "".join(m.groups())


def write_run_doc(
    *,
    runs_dir: Path,
    meta: dict,
    meta_mark: str,
    title: str,
    provenance: str,
    verdict_lines: list[str],
    pre_timings: "list[str] | None" = None,
    timings_intro: "list[str] | None" = None,
    phases: "list[dict] | None" = None,
    steps: "list[dict] | None" = None,
    skip_duration_dash: bool = False,
    env_table: "list[tuple[str, str]] | None" = None,
    extra_sections: "list[str] | None" = None,
    artifacts_lines: "list[str] | None" = None,
    doc_suffix: str = "",
) -> Path:
    """Write runs_dir/<date>-<hhmmss><doc_suffix>.md and return its path.

    phases and steps use the same dict shapes results.json records
    (sections: name/seconds, results: name/status/seconds), so the doc can
    be regenerated from a prior run's artifacts. The meta dict is embedded
    verbatim as an HTML comment (meta_mark) on line 3, which is what
    collect_run_history scrapes to rebuild the README summary.
    """
    runs_dir.mkdir(parents=True, exist_ok=True)
    doc = runs_dir / f"{meta['date']}{start_time_suffix(meta)}{doc_suffix}.md"

    lines: list[str] = []
    lines.append(f"# {title}")
    lines.append("")
    lines.append(f"<!-- {meta_mark} {json.dumps(meta, sort_keys=True)} -->")
    lines.append("")
    lines.append(provenance)
    lines.append("")
    lines.append("## Verdict")
    lines.append("")
    lines.extend(verdict_lines)
    if pre_timings:
        lines.extend(pre_timings)
    if phases or steps:
        lines.append("## Timings")
        lines.append("")
        if timings_intro:
            lines.extend(timings_intro)
        if phases:
            lines.append("| Phase | Duration |")
            lines.append("|---|---|")
            for ph in phases:
                lines.append(
                    f"| {_md_escape(ph['name'])} | {fmt_hms(ph['seconds'])} |"
                )
            lines.append("")
        if steps:
            lines.append("| Step | Result | Duration |")
            lines.append("|---|---|---|")
            for st in steps:
                if skip_duration_dash and st["status"] == SKIP:
                    dur = "-"
                else:
                    dur = fmt_hms(st["seconds"])
                lines.append(
                    f"| {_md_escape(st['name'])} | {st['status']} | {dur} |"
                )
            lines.append("")
    if env_table is not None:
        lines.append("## Environment")
        lines.append("")
        lines.append("| Item | Value |")
        lines.append("|---|---|")
        for label, key in env_table:
            value = meta.get(key) or "unknown"
            lines.append(f"| {label} | {_md_escape(str(value))} |")
        lines.append("")
    if extra_sections:
        lines.extend(extra_sections)
    if artifacts_lines:
        lines.append("## Artifacts")
        lines.append("")
        lines.extend(artifacts_lines)
    doc.write_text("\n".join(lines))
    return doc


def collect_run_history(runs_dir: Path, meta_mark: str) -> list[dict]:
    """Read the metadata comment from every committed run document."""
    history: list[dict] = []
    if not runs_dir.is_dir():
        return history
    pattern = re.compile(rf"<!-- {meta_mark} (\{{.*\}}) -->")
    for doc in sorted(runs_dir.glob("*.md")):
        match = pattern.search(doc.read_text())
        if not match:
            continue
        try:
            meta = json.loads(match.group(1))
        except json.JSONDecodeError:
            continue
        meta["_doc"] = doc.name
        history.append(meta)
    # started tiebreaks same-day runs (old date-only docs carry it too).
    history.sort(
        key=lambda m: (m.get("date", ""), m.get("started", "")), reverse=True,
    )
    return history


def write_summary_doc(
    *,
    docs_dir: Path,
    runs_dir: Path,
    meta_mark: str,
    title: str,
    intro: str,
    latest_headline: "Callable[[dict], str]",
    latest_table: "list[tuple[str, str]]",
    history_header: list[str],
    history_row: "Callable[[dict], str]",
    headline_pass_fail_only: bool = False,
) -> Path:
    """Regenerate docs_dir/README.md from the run history and return its path.

    headline_pass_fail_only makes the "Latest run" section track the most
    recent run with a definite PASS/FAIL verdict, skipping UNKNOWN runs
    (which stay in the history table and keep their committed run document).
    Scheduled runs pass it so a run that asserted nothing never replaces the
    headline; the default keeps local behavior unchanged.
    """
    docs_dir.mkdir(parents=True, exist_ok=True)
    history = collect_run_history(runs_dir, meta_mark)
    latest = history[0] if history else None
    if headline_pass_fail_only:
        latest = next(
            (m for m in history if m.get("verdict") != "UNKNOWN"), None,
        )

    lines: list[str] = []
    lines.append(f"# {title}")
    lines.append("")
    lines.append(intro)
    lines.append("")
    if latest:
        lines.append("## Latest run")
        lines.append("")
        lines.append(latest_headline(latest))
        lines.append("")
        lines.append("| Item | Value |")
        lines.append("|---|---|")
        for label, key in latest_table:
            lines.append(f"| {label} | {_md_escape(str(latest.get(key) or 'unknown'))} |")
        lines.append("")
    lines.append("## Run history")
    lines.append("")
    if history:
        lines.extend(history_header)
        for meta in history:
            lines.append(history_row(meta))
    else:
        lines.append("No runs recorded yet.")
    lines.append("")
    out = docs_dir / "README.md"
    out.write_text("\n".join(lines))
    return out
