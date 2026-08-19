"""Shared reporting + subprocess layer for the orchestrator scripts.

Extracted verbatim from scripts/e2e so scripts/bats (and future runners) reuse
one implementation: ANSI color, per-step PASS/FAIL/SKIP accounting with live
streaming and a bounded output tail, phase (section) timing, the summary
table, results.json + junit.xml artifacts with a latest.json pointer, and the
group-killing timeout watchdog around subprocesses.

Import pattern (matches _integration):
    import sys
    from pathlib import Path
    sys.path.insert(0, str(Path(__file__).parent))
    from _report import Color, Reporter, Runner, exec_stream, PASS, FAIL, SKIP
"""

from __future__ import annotations

import collections
import datetime as _dt
import json
import os
import signal
import subprocess
import sys
import threading
import time
import xml.etree.ElementTree as ET
from dataclasses import dataclass, field
from pathlib import Path
from typing import IO

TAIL_LINES = 400

PASS = "PASS"
FAIL = "FAIL"
SKIP = "SKIP"


def ts() -> str:
    """Wall-clock HH:MM:SS for live phase/step stamping (cleanroom-style)."""
    return _dt.datetime.now().strftime("%H:%M:%S")


def fmt_hms(seconds: float) -> str:
    """Human duration: 0.2s / 12.3s under a minute, else 5m23s / 1h04m09s.

    Sub-minute durations keep tenths so fast steps and dry-runs don't collapse
    to a misleading '0m00s'; minute-and-up matches the cleanroom driver's form.
    """
    if seconds < 60:
        return f"{seconds:.1f}s"
    s = int(round(seconds))
    h, rem = divmod(s, 3600)
    m, sec = divmod(rem, 60)
    return f"{h}h{m:02d}m{sec:02d}s" if h else f"{m}m{sec:02d}s"


def paren_hms(seconds: float) -> str:
    """' (m s)' suffix, only at >=60s so a sub-minute total isn't '12.3s (12.3s)'."""
    return f" ({fmt_hms(seconds)})" if seconds >= 60 else ""


def _kill_group(proc: subprocess.Popen) -> None:
    """SIGKILL the child's whole process group.

    Wrappers spawn grandchildren (scripts/bosh -> bosh -> ssh). Killing only the
    immediate child leaves grandchildren holding the stdout pipe open, so a hung
    process would not release until they exit. start_new_session=True makes the
    child a group leader (pgid == pid); killpg takes the whole tree down.
    """
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError, OSError):
        try:
            proc.kill()
        except OSError:
            pass


# --------------------------------------------------------------------------- #
# Reporting
# --------------------------------------------------------------------------- #

class Color:
    """ANSI color helpers, no-op when color is disabled."""

    def __init__(self, enabled: bool) -> None:
        self.enabled = enabled

    def _wrap(self, code: str, text: str) -> str:
        return f"\033[{code}m{text}\033[0m" if self.enabled else text

    def bold(self, t: str) -> str:
        return self._wrap("1", t)

    def green(self, t: str) -> str:
        return self._wrap("1;32", t)

    def red(self, t: str) -> str:
        return self._wrap("1;31", t)

    def yellow(self, t: str) -> str:
        return self._wrap("1;33", t)

    def dim(self, t: str) -> str:
        return self._wrap("2", t)


@dataclass
class StepResult:
    name: str
    status: str
    seconds: float
    detail: str = ""
    tail: list[str] = field(default_factory=list)


class Reporter:
    """Accumulates step results, streams live output, prints the summary.

    `label` names the harness in the summary banner and the junit suite/class
    names ("E2E" for scripts/e2e, "BATS" for scripts/bats).
    """

    def __init__(self, color: Color, command: str, dry_run: bool,
                 label: str = "E2E") -> None:
        self.color = color
        self.command = command
        self.dry_run = dry_run
        self.label = label
        self.results: list[StepResult] = []
        self.started = time.time()
        self._t0 = time.monotonic()
        # Section (cleanroom "phase") timing: name -> wall-clock elapsed.
        self.sections: list[tuple[str, float]] = []
        self._cur_section: str | None = None
        self._section_t0: float = self._t0

    # -- section + step lifecycle ----------------------------------------- #

    def _cum(self) -> float:
        """Cumulative wall-clock since the run began (cleanroom 'cumulative=')."""
        return time.monotonic() - self._t0

    def _finish_section(self) -> None:
        """Close the open section, record its elapsed, print a timed footer."""
        if self._cur_section is None:
            return
        elapsed = time.monotonic() - self._section_t0
        self.sections.append((self._cur_section, elapsed))
        print(self.color.dim(
            f"    <-- [{ts()}] {self._cur_section} done in {fmt_hms(elapsed)}"
            f"  (cumulative {fmt_hms(self._cum())})"
        ))
        self._cur_section = None

    def section(self, title: str) -> None:
        self._finish_section()
        self._cur_section = title
        self._section_t0 = time.monotonic()
        bar = "=" * 18
        print()
        print(self.color.bold(
            f"{bar} [{ts()}] {title}  (cumulative {fmt_hms(self._cum())}) {bar}"
        ))

    def end_section(self) -> None:
        """Public: close the currently open section (record its elapsed + print
        the footer) without opening a new one. Used before recording a
        concurrently-measured section so PHASE TIMINGS stays in run order."""
        self._finish_section()

    def record_section(self, title: str, elapsed: float) -> None:
        """Append a section whose elapsed was measured outside the live
        section() timer — e.g. a phase that ran concurrently with another in a
        background thread. Does not touch the live-section bookkeeping."""
        self.sections.append((title, elapsed))

    def sections_snapshot(self) -> "list[tuple[str, float]]":
        """Completed sections plus the live one's elapsed so far, in run
        order. Lets a step that runs inside the final section (e.g. report
        generation) include that section's timing without closing it."""
        snap = list(self.sections)
        if self._cur_section is not None:
            snap.append((self._cur_section, time.monotonic() - self._section_t0))
        return snap

    def begin(self, name: str) -> None:
        print(self.color.bold(f"==> [{ts()}] {name}"))

    def _status_line(self, name: str, status: str, seconds: float, detail: str) -> str:
        tag = {
            PASS: self.color.green(f"[{PASS} {seconds:6.1f}s]"),
            FAIL: self.color.red(f"[{FAIL} {seconds:6.1f}s]"),
            SKIP: self.color.yellow(f"[{SKIP}]"),
        }[status]
        suffix = f"  {self.color.dim(detail)}" if detail else ""
        return f"    {tag} {name}{suffix}"

    def record(
        self,
        name: str,
        status: str,
        seconds: float,
        detail: str = "",
        tail: list[str] | None = None,
    ) -> StepResult:
        res = StepResult(name, status, seconds, detail, list(tail or []))
        self.results.append(res)
        line = self._status_line(name, status, seconds, detail)
        if status != SKIP:
            line += self.color.dim(f"   @{ts()} cumulative={fmt_hms(self._cum())}")
        print(line)
        if status == FAIL and res.tail:
            print(self.color.red(f"    ! {name} failed — last {len(res.tail)} line(s):"))
            for line in res.tail:
                print(self.color.dim(f"      {line.rstrip()}"))
        return res

    def skip(self, name: str, reason: str) -> StepResult:
        return self.record(name, SKIP, 0.0, reason)

    # -- summary + artifacts ---------------------------------------------- #

    def _totals(self) -> dict[str, int]:
        t = {PASS: 0, FAIL: 0, SKIP: 0}
        for r in self.results:
            t[r.status] += 1
        return t

    def exit_code(self) -> int:
        return 1 if self._totals()[FAIL] else 0

    def summary(self) -> int:
        self._finish_section()
        total_s = time.monotonic() - self._t0
        t = self._totals()
        print()
        print(self.color.bold("=" * 64))
        print(self.color.bold(f"  {self.label} SUMMARY — command: {self.command}"
                              + ("  (DRY-RUN)" if self.dry_run else "")))
        print(self.color.bold("=" * 64))
        for r in self.results:
            print(self._status_line(r.name, r.status, r.seconds, r.detail))
        if self.sections:
            print(self.color.bold("-" * 64))
            print(self.color.bold("  PHASE TIMINGS"))
            for sname, ssec in self.sections:
                print(f"    {self.color.dim(f'{fmt_hms(ssec):>9}')}  {sname}")
        verdict = (
            self.color.green("ALL PASSED") if not t[FAIL]
            else self.color.red(f"{t[FAIL]} FAILED")
        )
        print(self.color.bold("-" * 64))
        print(
            f"  {self.color.green(str(t[PASS]) + ' pass')}"
            f"  {self.color.red(str(t[FAIL]) + ' fail')}"
            f"  {self.color.yellow(str(t[SKIP]) + ' skip')}"
            f"   total {total_s:.1f}s{paren_hms(total_s)}   ->  {verdict}"
        )
        print(self.color.bold("=" * 64))
        if t[FAIL]:
            failed = ", ".join(r.name for r in self.results if r.status == FAIL)
            print(self.color.red(f"  FAILED STEPS: {failed}"))
        return self.exit_code()

    def run_dir(self, results_dir: Path) -> Path:
        """The per-run artifact directory for this run (timestamp of start).

        Deterministic from self.started so callers can create it early (e.g.
        to open a console log before any step runs) and write_artifacts lands
        in the same place.
        """
        stamp = _dt.datetime.fromtimestamp(self.started).strftime("%Y%m%d-%H%M%S")
        return results_dir / stamp

    def write_artifacts(self, results_dir: Path, junit_name: str = "junit.xml") -> Path:
        run_dir = self.run_dir(results_dir)
        run_dir.mkdir(parents=True, exist_ok=True)
        total_s = time.monotonic() - self._t0
        t = self._totals()

        payload = {
            "command": self.command,
            "dry_run": self.dry_run,
            "started": _dt.datetime.fromtimestamp(self.started).isoformat(),
            "finished": _dt.datetime.now().isoformat(),
            "total_seconds": round(total_s, 3),
            "total_hms": fmt_hms(total_s),
            "sections": [
                {"name": n, "seconds": round(s, 3), "hms": fmt_hms(s)}
                for n, s in self.sections
            ],
            "totals": {"pass": t[PASS], "fail": t[FAIL], "skip": t[SKIP]},
            "exit": self.exit_code(),
            "results": [
                {
                    "name": r.name,
                    "status": r.status,
                    "seconds": round(r.seconds, 3),
                    "detail": r.detail,
                    "tail": r.tail if r.status == FAIL else [],
                }
                for r in self.results
            ],
        }
        results_json = run_dir / "results.json"
        results_json.write_text(json.dumps(payload, indent=2) + "\n")
        # Stable pointer for the `report` command.
        (results_dir / "latest.json").write_text(json.dumps(payload, indent=2) + "\n")

        # JUnit XML (harness step level; junit_name lets a caller that produces
        # its own spec-level junit.xml park this one under another name).
        suite = ET.Element(
            "testsuite",
            name=f"{self.label.lower()}:{self.command}",
            tests=str(len(self.results)),
            failures=str(t[FAIL]),
            skipped=str(t[SKIP]),
            time=f"{total_s:.3f}",
        )
        for r in self.results:
            case = ET.SubElement(
                suite, "testcase", name=r.name,
                classname=self.label.lower(), time=f"{r.seconds:.3f}",
            )
            if r.status == FAIL:
                fail_el = ET.SubElement(case, "failure", message=r.detail or "failed")
                fail_el.text = "\n".join(r.tail)
            elif r.status == SKIP:
                ET.SubElement(case, "skipped", message=r.detail or "skipped")
        junit = run_dir / junit_name
        ET.ElementTree(suite).write(junit, encoding="unicode", xml_declaration=True)

        print(self.color.dim(f"  wrote {results_json}"))
        print(self.color.dim(f"  wrote {junit}"))
        return run_dir


# --------------------------------------------------------------------------- #
# Subprocess execution
# --------------------------------------------------------------------------- #

def exec_stream(
    argv: list[str],
    *,
    env: dict[str, str] | None,
    cwd: Path,
    timeout: int | None,
    dry_run: bool,
    live: bool = True,
    logfile: "IO[str] | None" = None,
) -> tuple[int, list[str]]:
    """Run argv, streaming combined stdout/stderr live while keeping a tail.

    Returns (returncode, tail_lines). In dry-run, prints the command and
    returns (0, []).

    When ``live`` is False the child's output is NOT written to stdout as it
    arrives — it is buffered and returned in full (not truncated to the tail).
    This lets a background phase run concurrently with a live foreground phase
    without interleaving their output; the caller replays the buffer afterward.

    When ``logfile`` is set, every output line is also written to it as it
    arrives (a full console capture, independent of the bounded tail). The
    caller owns opening/closing the file and any scrubbing wrapper around it.
    """
    printable = " ".join(argv)
    if dry_run:
        print(f"    DRY-RUN: {printable}")
        return 0, []

    full_env = os.environ.copy()
    if env:
        full_env.update(env)

    tail: collections.deque[str] = collections.deque(maxlen=TAIL_LINES)
    buffered: list[str] = []
    try:
        proc = subprocess.Popen(
            argv,
            cwd=str(cwd),
            env=full_env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
            start_new_session=True,
        )
    except FileNotFoundError as exc:
        msg = f"command not found: {argv[0]} ({exc})"
        print(f"    {msg}")
        return 127, [msg]

    assert proc.stdout is not None

    # Wall-clock timeout via a watchdog thread. The main thread does blocking
    # readline; a chatty process streams fine, but a SILENTLY hung process
    # (e.g. a wedged bosh/cf deploy producing no output) would never reach an
    # in-loop deadline check — so the watchdog kills it independently. kill()
    # closes stdout, the readline loop hits EOF, and we report exit 124.
    timed_out = threading.Event()

    def _watch() -> None:
        try:
            proc.wait(timeout)
        except subprocess.TimeoutExpired:
            timed_out.set()
            _kill_group(proc)

    watcher: threading.Thread | None = None
    if timeout:
        watcher = threading.Thread(target=_watch, daemon=True)
        watcher.start()

    try:
        for line in proc.stdout:
            if live:
                sys.stdout.write("    | " + line)
            else:
                buffered.append(line.rstrip("\n"))
            if logfile is not None:
                logfile.write(line)
            tail.append(line.rstrip("\n"))
        rc = proc.wait()
    except KeyboardInterrupt:
        _kill_group(proc)
        proc.wait()
        raise
    finally:
        if watcher is not None:
            watcher.join(timeout=1)
        if logfile is not None:
            logfile.flush()

    out = buffered if not live else list(tail)
    if timed_out.is_set():
        out.append(f"TIMEOUT after {timeout}s — process killed")
        return 124, out
    return rc, out


class Runner:
    """Binds reporter + shared run context; runs subprocess steps.

    ``repo_root`` is the default cwd for steps; ``logfile`` (optional) receives
    a full console capture of every step's output via exec_stream.
    """

    def __init__(self, rep: Reporter, base_env: dict[str, str], dry_run: bool,
                 repo_root: Path, logfile: "IO[str] | None" = None) -> None:
        self.rep = rep
        self.base_env = base_env
        self.dry_run = dry_run
        self.repo_root = repo_root
        self.logfile = logfile

    def step(
        self,
        name: str,
        argv: list[str],
        *,
        env: dict[str, str] | None = None,
        cwd: Path | None = None,
        timeout: int | None = None,
        retries: int = 0,
        retry_delay: int = 10,
    ) -> tuple[bool, list[str]]:
        """Run a subprocess as a reported step. Returns (ok, tail)."""
        self.rep.begin(name)
        merged = dict(self.base_env)
        if env:
            merged.update(env)

        attempt = 0
        start = time.monotonic()
        rc: int = 0
        tail: list[str] = []
        while True:
            attempt += 1
            rc, tail = exec_stream(
                argv, env=merged, cwd=cwd or self.repo_root, timeout=timeout,
                dry_run=self.dry_run, logfile=self.logfile,
            )
            if rc == 0 or attempt > retries:
                break
            print(self.rep.color.yellow(
                f"    retry {attempt}/{retries} after exit {rc}; sleeping {retry_delay}s"
            ))
            if not self.dry_run:
                time.sleep(retry_delay)
            start = time.monotonic()

        seconds = time.monotonic() - start
        if rc == 0:
            self.rep.record(name, PASS, seconds)
            return True, tail
        self.rep.record(name, FAIL, seconds, f"exit {rc}", tail)
        return False, tail

    def capture(
        self,
        name: str,
        argv: list[str],
        *,
        env: dict[str, str] | None = None,
        cwd: Path | None = None,
        timeout: int | None = None,
    ) -> tuple[bool, str]:
        """Run a subprocess, capture full stdout, do NOT record a step.

        Used for short auxiliary queries (e.g. credhub get). Returns (ok, out).
        """
        if self.dry_run:
            print(f"    DRY-RUN: {' '.join(argv)}")
            return True, ""
        merged = os.environ.copy()
        merged.update(self.base_env)
        if env:
            merged.update(env)
        try:
            cp = subprocess.run(
                argv, cwd=str(cwd or self.repo_root), env=merged,
                capture_output=True, text=True, timeout=timeout,
            )
        except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
            return False, str(exc)
        return cp.returncode == 0, cp.stdout
