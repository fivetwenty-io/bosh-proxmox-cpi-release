#!/usr/bin/env python3
"""Tests for scripts/certify's slot guard, preflight order, and disk checks.

Run with:
    python3 scripts/_certify_test.py

The CLI tests load certify in-process and run it in --dry-run mode only, which
prints commands and executes none. They run against a synthetic repository
root in a temporary directory (see _synthetic_repo.py) and a slot beside it,
so neither certify nor its guard ever reads this checkout's Director files.
The verify and guard tests drive Certify's methods directly with a recording
runner, and the workflow tests parse certification.yml and run its slot step
against a stand-in bosh in a temporary directory.
"""

from __future__ import annotations

import argparse
import contextlib
import importlib.machinery as _ilm
import importlib.util as _ilu
import io
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parent
REPO_ROOT = SCRIPTS.parent
CERTIFY = SCRIPTS / "certify"
CONFIG = "ci/integration.scheduled.yml"
sys.path.insert(0, str(SCRIPTS))
import _checkouts  # noqa: E402
import _integration  # noqa: E402
import _rundoc  # noqa: E402
import _slot  # noqa: E402
import _upgrade_checks as uc  # noqa: E402
from _synthetic_repo import SyntheticRepo, install_read_guard  # noqa: E402

install_read_guard()

WORKFLOW = REPO_ROOT / ".github" / "workflows" / "certification.yml"
DENY = {"BOSH_PROTECTED_DIRECTOR_CIDS": "3808"}
GUARD_ENV = ("BOSH_REFUSE_DEFAULT_SLOT", "BOSH_STATE_DIR",
             "BOSH_PROTECTED_DIRECTOR_CIDS", "BOSH_PVE_ENV")
SLOT_YML = (
    "internal_ip: 172.31.0.12\nbosh_alias: pve-certification\n"
    "pve_create_env_deployment: create-env-certification\n"
)


def _load_certify(environ: "dict | None" = None, repo: "SyntheticRepo | None" = None) -> object:
    """A fresh copy of scripts/certify, rebased onto repo when one is given."""
    path = str(CERTIFY)
    with mock.patch.dict(os.environ, environ or {}):
        if environ is None or "BOSH_STATE_DIR" not in environ:
            os.environ.pop("BOSH_STATE_DIR", None)
        loader = _ilm.SourceFileLoader("certify_under_test", path)
        spec = _ilu.spec_from_file_location("certify_under_test", path, loader=loader)
        module = _ilu.module_from_spec(spec)
        loader.exec_module(module)
    if repo is not None:
        repo.rebase_module(module)
    return module


@contextlib.contextmanager
def _guard_environ(environ: dict):
    """os.environ with the guard's variables set to exactly environ's."""
    with mock.patch.dict(os.environ):
        for key in GUARD_ENV:
            os.environ.pop(key, None)
        os.environ.update(environ)
        yield


def _dry_run(repo: SyntheticRepo, state_dir: str, results: str,
             extra: "dict | None" = None) -> "tuple[int, str]":
    """Run `certify upgrade --dry-run` in-process on repo; return (rc, output)."""
    environ = {"BOSH_STATE_DIR": state_dir, **(DENY if extra is None else extra)}
    mod = _load_certify(environ, repo)
    out = io.StringIO()
    argv = ["upgrade", "--dry-run", "--no-color", "--skip-doc",
            "--config", str(repo.root / CONFIG), "--results-dir", results]
    with _guard_environ(environ), repo.rebase_shared(_integration, _rundoc, _checkouts), \
            contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
        try:
            rc = mod.main(argv)
        except SystemExit as exc:
            if isinstance(exc.code, int):
                rc = exc.code
            else:
                print(exc.code)
                rc = 1
    return rc, out.getvalue()


class _SyntheticRoot(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = SyntheticRepo()
        self.addCleanup(self.repo.close)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)


class DefaultSlotRefusalTest(_SyntheticRoot):
    """certify refuses the default slot before printing a single command."""

    def _assert_refused(self, result: "tuple[int, str]") -> None:
        rc, out = result
        self.assertEqual(rc, 1, out)
        self.assertIn("[FAIL", out)
        self.assertIn("director:slot-guard", out)
        self.assertIn("Set BOSH_STATE_DIR", out)
        self.assertNotIn("DRY-RUN: ", out)

    def test_unset_state_dir_is_refused(self) -> None:
        self._assert_refused(_dry_run(self.repo, "", self.tmp.name))

    def test_explicit_default_slot_is_refused(self) -> None:
        self._assert_refused(_dry_run(self.repo, str(self.repo.default_dir), self.tmp.name))

    def test_empty_deny_list_is_refused(self) -> None:
        slot = Path(self.tmp.name) / "certification"
        slot.mkdir()
        (slot / "slot.yml").write_text(SLOT_YML)
        for extra in ({}, {"BOSH_PROTECTED_DIRECTOR_CIDS": ""}):
            rc, out = _dry_run(self.repo, str(slot), self.tmp.name, extra)
            self.assertEqual(rc, 1, out)
            self.assertIn("BOSH_PROTECTED_DIRECTOR_CIDS is empty", out)
            self.assertNotIn("DRY-RUN: ", out)

    def test_malformed_deny_list_is_refused(self) -> None:
        slot = Path(self.tmp.name) / "certification"
        slot.mkdir()
        (slot / "slot.yml").write_text(SLOT_YML)
        for value in ("vm-3808", "3808;3809"):
            rc, out = _dry_run(self.repo, str(slot), self.tmp.name,
                               {"BOSH_PROTECTED_DIRECTOR_CIDS": value})
            self.assertEqual(rc, 1, out)
            self.assertIn("is not a Proxmox VMID", out)
            self.assertIn("3808,3809", out)
            self.assertNotIn("DRY-RUN: ", out)

    def test_slot_without_slot_yml_is_refused(self) -> None:
        slot = Path(self.tmp.name) / "certification"
        slot.mkdir()
        rc, out = _dry_run(self.repo, str(slot), self.tmp.name)
        self.assertEqual(rc, 1, out)
        self.assertIn("slot.yml is missing", out)
        self.assertNotIn("DRY-RUN: ", out)

    def test_main_state_with_the_slots_vm_is_refused(self) -> None:
        (self.repo.default_dir / "state.json").write_text(
            json.dumps({"director_id": "main", "current_vm_cid": "4001"}))
        slot = Path(self.tmp.name) / "certification"
        slot.mkdir()
        (slot / "slot.yml").write_text(SLOT_YML)
        (slot / "state.json").write_text(
            json.dumps({"director_id": "cert", "current_vm_cid": "4001"}))
        rc, out = _dry_run(self.repo, str(slot), self.tmp.name)
        self.assertEqual(rc, 1, out)
        self.assertIn("records the same Director VM as", out)
        self.assertIn("doesn't list that VM", out)
        self.assertNotIn("DRY-RUN: ", out)


    def test_state_and_creds_that_are_not_utf8_fail_the_slot_guard(self) -> None:
        # The guard refuses and certify records the failure, with no traceback.
        slot = Path(self.tmp.name) / "certification"
        slot.mkdir()
        (slot / "slot.yml").write_text(SLOT_YML)
        for name, content in (("state.json", b'{"director_id": "\xff\xfe-planted"}'),
                              ("creds.yml", b"admin_password: \xff\xfe-planted\n")):
            with self.subTest(name):
                (slot / name).write_bytes(content)
                rc, out = _dry_run(self.repo, str(slot), self.tmp.name)
                self.assertEqual(rc, 1, out)
                self.assertIn("[FAIL", out)
                self.assertIn("director:slot-guard", out)
                self.assertIn("is not valid UTF-8 text", out)
                self.assertNotIn("Traceback", out)
                self.assertNotIn("planted", out)
                self.assertNotIn("DRY-RUN: ", out)
                (slot / name).unlink()


@unittest.skipUnless(shutil.which("git"), "git is not on PATH")
class MainCheckoutLookupTest(unittest.TestCase):
    """certify finds the main checkout through a real git repository.

    The synthetic root is a linked worktree of a synthetic main checkout, and
    the only copy of the main Director's state is in the main checkout. The
    preflight refuses a copy of it only when certify looks the main checkout
    up from its own root. Every file here is synthetic.
    """

    def setUp(self) -> None:
        self.repo = SyntheticRepo(git=True)
        self.addCleanup(self.repo.close)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.slot = Path(self.tmp.name) / "certification"
        self.slot.mkdir()
        (self.slot / "slot.yml").write_text(SLOT_YML)
        self.main_state = self.repo.plant_main_state("main-director", "4001")

    def _plant_owner(self) -> None:
        slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(self.slot)})
        (self.slot / "slot-owner.json").write_text(
            json.dumps(_slot.state_identity(_slot.read_state(slot.state))))

    def test_a_copy_of_the_main_state_fails_the_slot_guard(self) -> None:
        shutil.copyfile(self.main_state, self.slot / "state.json")
        self._plant_owner()
        self.assertFalse((self.repo.default_dir / "state.json").exists())
        rc, out = _dry_run(self.repo, str(self.slot), self.tmp.name)
        self.assertEqual(rc, 1, out)
        self.assertIn("director:slot-guard", out)
        self.assertIn("records the same Director VM as", out)
        self.assertIn(os.path.realpath(self.main_state), out)
        self.assertNotIn("DRY-RUN: ", out)

    def test_a_state_of_its_own_passes_the_slot_guard(self) -> None:
        (self.slot / "state.json").write_text(
            json.dumps({"director_id": "own-director", "current_vm_cid": "4002"}))
        self._plant_owner()
        rc, out = _dry_run(self.repo, str(self.slot), self.tmp.name)
        self.assertNotIn("records the same Director", out, out)
        slot_guard = [line for line in out.splitlines() if "director:slot-guard" in line]
        self.assertFalse([line for line in slot_guard if "FAIL" in line], out)


@unittest.skipUnless(all(shutil.which(t) for t in ("uv", "bosh", "git", "gh")),
                     "certify's preflight needs uv, bosh, git, and gh on PATH")
class DedicatedSlotDryRunTest(_SyntheticRoot):
    def test_order_of_the_new_steps(self) -> None:
        slot = Path(self.tmp.name) / "certification"
        slot.mkdir()
        (slot / "slot.yml").write_text(SLOT_YML)
        rc, out = _dry_run(self.repo, str(slot), self.tmp.name)
        self.assertEqual(rc, 0, out)
        lines = out.splitlines()

        def first(fragment: str) -> int:
            for i, line in enumerate(lines):
                if fragment in line:
                    return i
            self.fail(f"{fragment!r} not in the dry-run output")

        self.assertLess(first("bosh check-vars"), first("lab:reachable"))
        self.assertLess(first("bosh check-vars"), first("bosh net-up"))
        self.assertLess(first("cert:write-marker"), first("capture:pre"))
        self.assertLess(first("capture:post"), first("cert:read-marker"))
        self.assertLess(first("cert:read-marker"), first("verify:director-vm-replaced"))
        for name in ("verify:disk-data-survived", "verify:disk-serial-stable"):
            self.assertIn(f"[PASS    0.0s] {name}", out)
        self.assertIn("net:down  the env network is shared", out)
        self.assertNotIn("bosh net-down", out)


class _Rep:
    def __init__(self) -> None:
        self.rows: list[tuple[str, str, str]] = []

    def record(self, name, status, _seconds, detail="", _tail=None):
        self.rows.append((name, status, detail))

    def skip(self, name, reason):
        self.rows.append((name, "SKIP", reason))

    def section(self, _title):
        pass

    def begin(self, _name):
        pass

    def run_dir(self, results_dir):
        return Path(results_dir) / "run"

    def status(self, name):
        return [row[1] for row in self.rows if row[0] == name]


class _RecordingRunner:
    """Stands in for _report.Runner and records every argv it is handed."""

    def __init__(self) -> None:
        self.calls: list[list[str]] = []
        self.base_env: dict[str, str] = {}
        self.logfile = None

    def step(self, name, argv, **_kw):
        self.calls.append([str(a) for a in argv])
        return True, []


def _never(*_a, **_kw):
    raise AssertionError("a refused step ran a command")


class GuardedStepsTest(_SyntheticRoot):
    """Each leg that deletes or replaces a Director re-checks the slot first.

    Every test builds a Certify whose slot the guard refuses, then asserts the
    leg records FAIL or SKIP and never hands scripts/bosh create-env or
    teardown, or any other command, to the runner.
    """

    def setUp(self) -> None:
        super().setUp()
        self.mod = _load_certify(None, self.repo)
        rebase = self.repo.rebase_shared(_integration, _rundoc, _checkouts)
        rebase.__enter__()
        self.addCleanup(rebase.__exit__, None, None, None)
        env = _guard_environ(DENY)
        env.__enter__()
        self.addCleanup(env.__exit__, None, None, None)

    def _certify(self, slot: "_slot.Slot") -> object:
        c = object.__new__(self.mod.Certify)
        c.rep = _Rep()
        c.runner = _RecordingRunner()
        c.args = argparse.Namespace(force=True, keep=False)
        c.dry_run = False
        c.env_name = "cpitest"
        c.slot = slot
        c.cfg = {}
        c.ccfg = {"deployment_name": "certification", "cpi_id": ""}
        c.deployed = True
        c.director_up = True
        c._director_identity = lambda: {"vm_cid": "3808", "disk_cids": []}
        c._backup_state = lambda _tag: None
        c._env_manages_sdn = lambda: False
        c._sdn_skip_reason = lambda: "the env has no SDN"
        c._director_env = _never
        return c

    def _default_slot(self) -> "_slot.Slot":
        return _slot.resolve(self.repo.root, {})

    def _copied_slot(self) -> "_slot.Slot":
        d = Path(self.tmp.name) / "certification"
        d.mkdir()
        (d / "slot.yml").write_text(SLOT_YML)
        (d / "state.json").write_text(json.dumps({"director_id": "d", "current_vm_cid": "4001"}))
        return _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(d)})

    def _slots(self):
        yield "default", self._default_slot()
        yield "copied", self._copied_slot()

    def test_deploy_old_with_force_never_tears_down(self) -> None:
        for label, slot in self._slots():
            c = self._certify(slot)
            self.assertFalse(c.deploy_old(), label)
            self.assertEqual(c.runner.calls, [], label)
            self.assertEqual(c.rep.status("director:teardown-existing"), ["FAIL"], label)

    def test_deploy_old_without_a_standing_director_never_creates(self) -> None:
        c = self._certify(self._default_slot())
        c._director_identity = lambda: {"vm_cid": "", "disk_cids": []}
        self.assertFalse(c.deploy_old())
        self.assertEqual(c.runner.calls, [])
        self.assertEqual(c.rep.status("director:create-env-old"), ["FAIL"])

    def test_upgrade_never_creates(self) -> None:
        for label, slot in self._slots():
            c = self._certify(slot)
            self.assertFalse(c.upgrade(), label)
            self.assertEqual(c.runner.calls, [], label)
            self.assertEqual(c.rep.status("director:create-env-new"), ["FAIL"], label)

    def test_teardown_never_deletes(self) -> None:
        for label, slot in self._slots():
            c = self._certify(slot)
            with mock.patch.object(self.mod, "exec_stream", side_effect=_never):
                c.teardown()
            self.assertEqual(c.runner.calls, [], label)
            for name in ("cert:delete-deployment", "director:clean-up", "director:delete-env"):
                self.assertEqual(c.rep.status(name), ["SKIP"], f"{label} {name}")

    def test_preflight_guard_fails_first(self) -> None:
        c = self._certify(self._default_slot())
        self.assertFalse(c._preflight_slot_guard())
        self.assertEqual(c.rep.status("director:slot-guard"), ["FAIL"])


class BackupStateTest(_SyntheticRoot):
    """The run directory's backup keeps the owner record with the state."""

    def test_backup_copies_the_owner_record(self) -> None:
        slot_dir = Path(self.tmp.name) / "certification"
        slot_dir.mkdir()
        mod = _load_certify({"BOSH_STATE_DIR": str(slot_dir)}, self.repo)
        files = {"state.json": '{"current_vm_cid": "4001"}', "creds.yml": "admin_password: x\n",
                 "slot-owner.json": '{"current_vm_cid": "4001"}'}
        for name, text in files.items():
            (slot_dir / name).write_text(text)
        run_dir = Path(self.tmp.name) / "run"
        run_dir.mkdir()
        c = object.__new__(mod.Certify)
        c.dry_run = False
        c.run_dir = run_dir
        c._backup_state("pre-upgrade")
        for name, text in files.items():
            self.assertEqual((run_dir / f"{name}.pre-upgrade").read_text(), text, name)

    def test_backup_skips_files_the_slot_lacks(self) -> None:
        slot_dir = Path(self.tmp.name) / "certification"
        slot_dir.mkdir()
        mod = _load_certify({"BOSH_STATE_DIR": str(slot_dir)}, self.repo)
        (slot_dir / "state.json").write_text("{}")
        run_dir = Path(self.tmp.name) / "run"
        run_dir.mkdir()
        c = object.__new__(mod.Certify)
        c.dry_run = False
        c.run_dir = run_dir
        c._backup_state("pre")
        self.assertEqual(sorted(p.name for p in run_dir.iterdir()), ["state.json.pre"])


class RunnerEnvTest(_SyntheticRoot):
    """Every child certify starts carries the refusal and an absolute slot."""

    def test_base_env_pins_the_slot_and_refuses_the_default(self) -> None:
        tmp = self.tmp.name
        (Path(tmp) / "certification").mkdir()
        old = os.getcwd()
        os.chdir(tmp)
        try:
            mod = _load_certify({"BOSH_STATE_DIR": "certification"}, self.repo)
        finally:
            os.chdir(old)
        args = argparse.Namespace(dry_run=True, env="cpitest", results_dir=tmp,
                                  config=CONFIG)
        with _guard_environ({}), \
                self.repo.rebase_shared(_integration, _rundoc, _checkouts), \
                mock.patch.object(mod._integration, "load_config", return_value={}), \
                mock.patch.object(mod, "certification_config", return_value={}):
            c = mod.Certify(args, _Rep())
            in_process = os.environ.get("BOSH_REFUSE_DEFAULT_SLOT")
        env = c.runner.base_env
        self.assertEqual(env.get("BOSH_REFUSE_DEFAULT_SLOT"), "1")
        self.assertEqual(in_process, "1")
        self.assertTrue(Path(env["BOSH_STATE_DIR"]).is_absolute())
        self.assertEqual(os.path.realpath(env["BOSH_STATE_DIR"]),
                         os.path.realpath(Path(tmp) / "certification"))


def _workflow() -> dict:
    import yaml
    return yaml.safe_load(WORKFLOW.read_text())


class CertificationWorkflowTest(unittest.TestCase):
    """The certification job pins the guard's settings for every step."""

    GUARD_KEYS = ("BOSH_STATE_DIR", "BOSH_REFUSE_DEFAULT_SLOT", "BOSH_PROTECTED_DIRECTOR_CIDS")

    def setUp(self) -> None:
        self.job = _workflow()["jobs"]["certification"]

    def test_job_env_sets_the_guard(self) -> None:
        env = self.job["env"]
        self.assertEqual(str(env["BOSH_REFUSE_DEFAULT_SLOT"]), "1")
        self.assertTrue(str(env["BOSH_STATE_DIR"]).startswith("/"))
        self.assertEqual(env["BOSH_PROTECTED_DIRECTOR_CIDS"],
                         "${{ vars.BOSH_PROTECTED_DIRECTOR_CIDS }}")

    def test_no_step_overrides_the_guard(self) -> None:
        for step in self.job["steps"]:
            overridden = set(step.get("env") or {}) & set(self.GUARD_KEYS)
            self.assertEqual(overridden, set(), step.get("name"))
            run = step.get("run") or ""
            for key in self.GUARD_KEYS:
                self.assertNotIn(f"export {key}", run, step.get("name"))
                self.assertNotIn(f"{key}=", run.replace(f"${{{key}", ""), step.get("name"))

    def test_main_directors_state_is_never_mounted(self) -> None:
        state_dir = self.job["env"]["BOSH_STATE_DIR"]
        sources = {}
        for volume in self.job["container"]["volumes"]:
            source, _, target = volume.partition(":")
            sources[target] = source
            self.assertNotEqual(source.rstrip("/"), "/home/runner/gha-lab-state", volume)
        self.assertIn(state_dir, sources)
        self.assertNotEqual(sources[state_dir].rstrip("/"), "/home/runner/gha-lab-state")


FAKE_BOSH = """#!/usr/bin/env bash
touch "${FAKE_BOSH_CALLED}"
case "${FAKE_BOSH_MODE}" in
  ok) printf 'internal_ip: 172.31.0.12\\n' ;;
  missing) printf 'Expected to find variables:\\n  - certification_director_ip\\n' >&2; exit 1 ;;
  other) printf 'Parsing vars file: yaml: line 3: did not find expected key\\n' >&2; exit 1 ;;
esac
"""


@unittest.skipUnless(shutil.which("bash"), "bash is not on PATH")
class SlotStepTest(unittest.TestCase):
    """The workflow's slot step, run as written against a stand-in bosh."""

    def setUp(self) -> None:
        steps = {s.get("name"): s for s in _workflow()["jobs"]["certification"]["steps"]}
        self.script = steps["Write the certification slot config"]["run"]
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.slot = root / "slot"
        self.slot.mkdir()
        self.work = root / "work"
        self.work.mkdir()
        self.bin = root / "bin"
        self.bin.mkdir()
        fake = self.bin / "bosh"
        fake.write_text(FAKE_BOSH)
        fake.chmod(0o755)
        self.called = root / "bosh-called"

    def _run(self, mode: str = "ok", deny: str = "3808") -> "subprocess.CompletedProcess[str]":
        env = {**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}",
               "BOSH_STATE_DIR": str(self.slot), "BOSH_PROTECTED_DIRECTOR_CIDS": deny,
               "FAKE_BOSH_MODE": mode, "FAKE_BOSH_CALLED": str(self.called)}
        return subprocess.run(["bash", "-c", self.script], cwd=self.work, env=env,
                              capture_output=True, text=True, timeout=60)

    def test_an_empty_slot_gets_its_slot_yml(self) -> None:
        proc = self._run()
        self.assertEqual(proc.returncode, 0, proc.stderr)
        written = self.slot / "slot.yml"
        self.assertEqual(written.read_text(), "internal_ip: 172.31.0.12\n")
        self.assertEqual(stat.S_IMODE(written.stat().st_mode), 0o600)

    def test_a_seeded_slot_is_refused(self) -> None:
        for name in ("state.json", "creds.yml"):
            with self.subTest(name=name):
                (self.slot / name).write_text("{}")
                proc = self._run()
                (self.slot / name).unlink()
                self.assertEqual(proc.returncode, 1)
                self.assertIn("The slot must start empty", proc.stderr)
                self.assertIn("gha-lab-state", proc.stderr)
                self.assertFalse((self.slot / "slot.yml").exists())
                self.assertFalse(self.called.exists())

    def test_a_dangling_link_counts_as_seeded(self) -> None:
        (self.slot / "state.json").symlink_to(Path(self.tmp.name) / "elsewhere.json")
        proc = self._run()
        self.assertEqual(proc.returncode, 1)
        self.assertIn("The slot must start empty", proc.stderr)

    def test_a_slot_from_an_earlier_run_is_kept(self) -> None:
        (self.slot / "slot.yml").write_text("internal_ip: 172.31.0.12\n")
        (self.slot / "state.json").write_text("{}")
        (self.slot / "creds.yml").write_text("{}")
        proc = self._run()
        self.assertEqual(proc.returncode, 0, proc.stderr)

    def test_an_empty_deny_list_stops_the_step(self) -> None:
        for deny in ("", " , "):
            proc = self._run(deny=deny)
            self.assertEqual(proc.returncode, 1)
            self.assertIn("gh variable set BOSH_PROTECTED_DIRECTOR_CIDS", proc.stderr)
            self.assertFalse(self.called.exists())

    def test_a_malformed_deny_list_stops_the_step(self) -> None:
        for deny in ("vm-3808", "3808;3809", "3808,", "0", "3808 3809"):
            with self.subTest(deny=deny):
                proc = self._run(deny=deny)
                self.assertEqual(proc.returncode, 1)
                self.assertIn("is not a list of VMIDs", proc.stderr)
                self.assertIn("gh variable set BOSH_PROTECTED_DIRECTOR_CIDS", proc.stderr)
                self.assertFalse(self.called.exists())
                self.assertFalse((self.slot / "slot.yml").exists())

    def test_a_spaced_deny_list_is_accepted(self) -> None:
        proc = self._run(deny=" 3808 , 3809 ")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertTrue(self.called.exists())

    def test_a_missing_director_ip_names_the_key(self) -> None:
        proc = self._run("missing")
        self.assertEqual(proc.returncode, 1)
        self.assertIn("has no certification_director_ip", proc.stderr)
        self.assertFalse((self.slot / "slot.yml").exists())

    def test_any_other_bosh_error_is_printed_as_is(self) -> None:
        proc = self._run("other")
        self.assertEqual(proc.returncode, 1)
        self.assertIn("did not find expected key", proc.stderr)
        self.assertNotIn("has no certification_director_ip", proc.stderr)


class VerifyDiskChecksTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.mod = _load_certify()

    def _certify(self) -> object:
        c = object.__new__(self.mod.Certify)
        c.rep = _Rep()
        c.invariants = []
        c.marker_written = None
        c.marker_read = None
        c.marker_problem = "the run never reached the marker write"
        c.pre, c.post = {}, {}
        return c

    def test_data_survived(self) -> None:
        c = self._certify()
        c.marker_written = c.marker_read = ("0" * 32, "a" * 64)
        self.assertTrue(c._verify_disk_data())
        self.assertEqual(c.rep.rows[-1][:2], ("verify:disk-data-survived", "PASS"))
        self.assertTrue(c.invariants[-1]["held"])

    def test_data_changed(self) -> None:
        c = self._certify()
        c.marker_written = ("0" * 32, "a" * 64)
        c.marker_read = ("0" * 32, "b" * 64)
        self.assertFalse(c._verify_disk_data())
        self.assertIn("sha256 changed", c.rep.rows[-1][2])

    def test_wrong_disk(self) -> None:
        c = self._certify()
        c.marker_written = ("0" * 32, "a" * 64)
        c.marker_read = ("1" * 32, "a" * 64)
        self.assertFalse(c._verify_disk_data())
        self.assertIn("nonce", c.rep.rows[-1][2])

    def test_marker_never_written(self) -> None:
        c = self._certify()
        self.assertFalse(c._verify_disk_data())
        self.assertEqual(c.rep.rows[-1][1], "FAIL")

    def _section(self, vmid: str, drive: "str | None") -> dict:
        ids = {"pvd-cid": "bpd-00112233aabbccdd"}
        serials = {vmid: {drive: "bpd-00112233aabbccdd"}} if drive else {vmid: {}}
        return {"stable_ids": ids, "drive_serials": serials,
                "locations": uc.serial_locations(ids, serials), "errors": []}

    def test_serials_stable(self) -> None:
        c = self._certify()
        c.pre = {"disks": self._section("101", "scsi1")}
        c.post = {"disks": self._section("202", "scsi1")}
        self.assertTrue(c._verify_disk_serials())
        self.assertEqual(c.rep.rows[-1][:2], ("verify:disk-serial-stable", "PASS"))
        self.assertIn("101/scsi1", c.invariants[-1]["before"])
        self.assertIn("202/scsi1", c.invariants[-1]["after"])

    def test_serial_dropped_reports_api_errors(self) -> None:
        c = self._certify()
        c.pre = {"disks": self._section("101", "scsi1")}
        post = self._section("202", None)
        post["errors"] = ["VM 202 is not in /cluster/resources"]
        c.post = {"disks": post}
        self.assertFalse(c._verify_disk_serials())
        self.assertIn("VM 202 is not in /cluster/resources", c.rep.rows[-1][2])


if __name__ == "__main__":
    unittest.main(verbosity=2)
