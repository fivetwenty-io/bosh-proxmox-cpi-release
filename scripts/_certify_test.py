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
import base64
import contextlib
import importlib.machinery as _ilm
import importlib.util as _ilu
import io
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

import yaml

SCRIPTS = Path(__file__).resolve().parent
REPO_ROOT = SCRIPTS.parent
CERTIFY = SCRIPTS / "certify"
CONFIG = "ci/integration.scheduled.yml"
sys.path.insert(0, str(SCRIPTS))
import _checkouts  # noqa: E402
import _integration  # noqa: E402
import _rundoc  # noqa: E402
import _slot  # noqa: E402
import _spanning  # noqa: E402
from _certify_default_dry_run import DEFAULT_DRY_RUN  # noqa: E402
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
             extra: "dict | None" = None, config: str = CONFIG) -> "tuple[int, str]":
    """Run `certify upgrade --dry-run` in-process on repo; return (rc, output)."""
    environ = {"BOSH_STATE_DIR": state_dir, **(DENY if extra is None else extra)}
    mod = _load_certify(environ, repo)
    out = io.StringIO()
    argv = ["upgrade", "--dry-run", "--no-color", "--skip-doc",
            "--config", str(repo.root / config), "--results-dir", results]
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

    @unittest.skipUnless(shutil.which("bosh"), "the dry run past the slot guard needs bosh on PATH")
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


class TeardownCpiTest(_SyntheticRoot):
    """delete-env gets the CPI tarball the standing Director was built with."""

    OLD = "/releases/bosh-proxmox-cpi-old.tgz"
    NEW = "/releases/bosh-proxmox-cpi-new.tgz"

    def setUp(self) -> None:
        super().setUp()
        self.mod = _load_certify(None, self.repo)

    def _certify(self, side: "str | None") -> object:
        c = object.__new__(self.mod.Certify)
        c.old_cpi_path = Path(self.OLD)
        c.new_cpi_path = Path(self.NEW)
        c.director_side = side
        return c

    def test_an_upgraded_director_tears_down_on_the_new_cpi(self) -> None:
        self.assertEqual(self._certify("new")._teardown_env(), {"PVE_CPI_RELEASE_PATH": self.NEW})

    def test_a_director_never_upgraded_tears_down_on_the_old_cpi(self) -> None:
        for side in ("old", None):
            self.assertEqual(self._certify(side)._teardown_env(), {"PVE_CPI_RELEASE_PATH": self.OLD}, side)

    def test_without_a_tarball_scripts_bosh_still_chooses(self) -> None:
        c = self._certify("new")
        c.new_cpi_path = None
        self.assertIsNone(c._teardown_env())

    def test_teardown_hands_the_tarball_to_delete_env(self) -> None:
        c = self._certify("new")
        c.args = argparse.Namespace(keep=False)
        c.rep = _Rep()
        c.dry_run = False
        c.deployed = False
        c.director_up = True
        c.ccfg = {"deployment_name": "certification"}
        c._slot_refusal = lambda _op: ""
        c._env_manages_sdn = lambda: False
        c._sdn_skip_reason = lambda: "the env has no SDN"
        calls = []

        def record(name, argv, timeout, env=None):
            calls.append((name, env))
            return True

        c._cleanup_step = record
        c.teardown()
        self.assertEqual(calls, [("director:delete-env", {"PVE_CPI_RELEASE_PATH": self.NEW})])


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


# --------------------------------------------------------------------------- #
# One deployment that spans two clusters
# --------------------------------------------------------------------------- #

# What build_deployment_ops and _cloud_config_args produced for a one-entry run
# before certification.azs existed. A default run has to keep producing exactly
# this.
GOLDEN_SINGLE_AZ_OPS = """\
- type: replace
  path: /instance_groups/name=simple/azs
  value:
  - z1
- type: replace
  path: /instance_groups/name=simple/vm_type
  value: certification
- type: replace
  path: /instance_groups/name=simple/networks/name=default/name
  value: default
- type: replace
  path: /instance_groups/name=simple/persistent_disk_type?
  value: default
"""
GOLDEN_CC_ARGS = [
    "-o", "<ROOT>/manifests/certification/cloud-config-ops.yml",
    "-v", "certification_vm_cores=2",
    "-v", "certification_vm_memory_mib=2048",
    "-v", "certification_vm_disk_mib=8192",
]
GOLDEN_CC_CPI_ARGS = GOLDEN_CC_ARGS + [
    "-o", "<ROOT>/manifests/certification/cloud-config-cpi-ops.yml",
    "-v", "certification_cpi_id=pve-az1",
]
# The certification release's manifest, as upstream ships it.
UPSTREAM_CERTIFICATION_YML = """\
---
name: ((deployment_name))
instance_groups:
  - name: simple
    instances: 1
    vm_type: default
    stemcell: default
    azs: [z1, z2, z3]
    networks:
      - name: default
        default: [dns, gateway]
    jobs:
      - name: simple
        release: ((release_name))
"""

AZ2_NETWORK = {
    "cidr": "10.255.0.0/16",
    "gateway": "10.255.0.1",
    "reserved": ["10.255.0.1-10.255.48.19", "10.255.48.200-10.255.255.254"],
}


def _spanning_section(**second: object) -> dict:
    """A `certification:` section that spans pve-az1 and pve-az2."""
    entry = {"az": "z2", "cpi_id": "pve-az2", "network": dict(AZ2_NETWORK), **second}
    return {"network_bridge": "vmbr0",
            "azs": [{"az": "z1", "cpi_id": "pve-az1"}, entry]}


def _apply_ops(doc: dict, ops: list, variables: dict) -> dict:
    """The part of go-patch that the certification ops files use.

    It handles `replace` with name= selectors, `/-` appends, and a trailing
    `?`, and it fills ((var)) values the way `bosh int -v` does. It exists so
    the tests can see the document an ops file produces without running bosh.
    """
    def fill(value):
        if isinstance(value, str):
            match = re.fullmatch(r"\(\((\w+)\)\)", value)
            return variables.get(match.group(1), value) if match else value
        if isinstance(value, list):
            return [fill(v) for v in value]
        if isinstance(value, dict):
            return {k: fill(v) for k, v in value.items()}
        return value

    for op in ops:
        assert op["type"] == "replace", op
        tokens = op["path"].split("/")[1:]
        node = doc
        for tok in tokens[:-1]:
            tok = tok.rstrip("?")
            if tok.startswith("name="):
                node = next(i for i in node if i.get("name") == tok[5:])
            else:
                node = node[tok]
        last = tokens[-1].rstrip("?")
        value = fill(op["value"])
        if last == "-":
            node.append(value)
        elif last.startswith("name="):
            names = [i.get("name") for i in node]
            if last[5:] in names:
                node[names.index(last[5:])] = value
            else:
                node.append(value)
        else:
            node[last] = value
    return doc


def _flag_vars(args: list) -> "tuple[list[str], dict]":
    """Split a -o/-v argument list into ops file paths and parsed variables."""
    files, variables = [], {}
    it = iter(args)
    for flag in it:
        value = next(it)
        if flag == "-o":
            files.append(value)
        else:
            name, _, raw = value.partition("=")
            variables[name] = yaml.safe_load(raw)
    return files, variables


class _CertifyModule(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.mod = _load_certify()

    def _config(self, section: dict) -> dict:
        return self.mod.certification_config({"certification": section}, "cpitest", True)

    def _certify(self, section: dict) -> object:
        c = object.__new__(self.mod.Certify)
        c.ccfg = self._config(section)
        return c

    def _args(self, section: dict) -> list:
        root = str(self.mod.REPO_ROOT)
        return [a.replace(root, "<ROOT>") for a in self._certify(section)._cloud_config_args()]


def _normalise_dry_run(text: str, repo_root: str, tmp: str) -> str:
    """Replace what differs between runs of the same dry run with placeholders."""
    for old, new in ((repo_root, "<REPO>"), (tmp, "<TMP>")):
        text = text.replace(old, new)
    text = re.sub(r"\d{8}-\d{6}", "<RUN>", text)
    text = re.sub(r"\[\d\d:\d\d:\d\d\]", "[T]", text)
    text = re.sub(r"@\d\d:\d\d:\d\d cumulative=[\d.]+s", "@T", text)
    text = re.sub(r"\[(PASS|FAIL|SKIP)\s+[\d.]+s\]", r"[\1]", text)
    text = re.sub(r"\(cumulative [\d.]+s\)", "(cumulative)", text)
    text = re.sub(r"done in [\d.]+s", "done", text)
    text = re.sub(r"^(\s+)[\d.]+s  ", r"\1", text, flags=re.M)
    text = re.sub(r"total [\d.]+s", "total", text)
    text = re.sub(r"-c echo [A-Za-z0-9+/=]+ \|", "-c echo <B64> |", text)
    return "\n".join(line.rstrip() for line in text.splitlines()) + "\n"


class DefaultAzsUnchangedTest(_CertifyModule):
    """A run that does not set certification.azs generates what it always did."""

    def test_the_default_is_the_single_entry(self) -> None:
        ccfg = self._config({"network_bridge": "vmbr0"})
        self.assertEqual(ccfg["azs"], [{"az": "z1", "cpi_id": "", "network": None,
                                        "bridge": "", "storage": ""}])
        ccfg = self._config({"network_bridge": "vmbr0", "az": "z9", "cpi_id": "pve-az1"})
        self.assertEqual(ccfg["azs"][0]["az"], "z9")
        self.assertEqual(ccfg["azs"][0]["cpi_id"], "pve-az1")
        self.assertEqual((ccfg["az"], ccfg["cpi_id"]), ("z9", "pve-az1"))

    def test_deployment_ops_are_byte_identical(self) -> None:
        ccfg = self._config({"network_bridge": "vmbr0", "cpi_id": "pve-az1"})
        text = yaml.safe_dump(self.mod.build_deployment_ops(ccfg),
                              default_flow_style=False, sort_keys=False)
        self.assertEqual(text, GOLDEN_SINGLE_AZ_OPS)

    def test_cloud_config_flags_are_identical(self) -> None:
        self.assertEqual(self._args({"network_bridge": "vmbr0"}), GOLDEN_CC_ARGS)
        self.assertEqual(self._args({"network_bridge": "vmbr0", "cpi_id": "pve-az1"}),
                         GOLDEN_CC_CPI_ARGS)

    def test_an_explicit_single_entry_matches_the_default(self) -> None:
        single = {"network_bridge": "vmbr0",
                  "azs": [{"az": "z1", "cpi_id": "pve-az1"}]}
        self.assertEqual(self._args(single), GOLDEN_CC_CPI_ARGS)
        ccfg = self._config(single)
        self.assertEqual(yaml.safe_dump(self.mod.build_deployment_ops(ccfg),
                                        default_flow_style=False, sort_keys=False),
                         GOLDEN_SINGLE_AZ_OPS)

    def test_a_default_dry_run_is_the_output_it_always_was(self) -> None:
        repo = SyntheticRepo()
        self.addCleanup(repo.close)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        slot = Path(tmp.name) / "certification"
        slot.mkdir()
        (slot / "slot.yml").write_text(SLOT_YML)
        rc, out = _dry_run(repo, str(slot), tmp.name)
        self.assertEqual(rc, 0, out)
        self.assertEqual(_normalise_dry_run(out, str(repo.root), tmp.name), DEFAULT_DRY_RUN)


class SpanningConfigTest(_CertifyModule):
    def test_two_entries_resolve(self) -> None:
        ccfg = self._config(_spanning_section())
        self.assertTrue(self.mod.is_spanning(ccfg))
        first, second = ccfg["azs"]
        self.assertEqual((first["az"], first["cpi_id"], first["network"]), ("z1", "pve-az1", None))
        self.assertEqual((second["az"], second["cpi_id"]), ("z2", "pve-az2"))
        self.assertEqual(second["network"], AZ2_NETWORK)
        self.assertEqual((second["bridge"], second["storage"]), ("", ""))
        self.assertEqual((ccfg["az"], ccfg["cpi_id"]), ("z1", "pve-az1"))

    def _refused(self, section: dict, fragment: str) -> None:
        with self.assertRaises(SystemExit) as caught:
            self._config(section)
        self.assertIn(fragment, str(caught.exception))

    def test_bad_lists_are_refused(self) -> None:
        good = _spanning_section()
        self._refused({"azs": "z1"}, "must be a list of mappings")
        self._refused({"azs": [{"az": "z1"}, {"az": "z1"}, {"az": "z1"}]}, "one or two clusters")
        self._refused({"azs": [{"cpi_id": "pve-az1"}]}, "azs[0].az is required")
        self._refused({"azs": [{"az": "z1", "cpi_id": "a", "surprise": 1}]}, "unknown keys: surprise")
        self._refused({**good, "cpi_id": "pve-az9"}, "disagrees with certification.azs[0].cpi_id")
        self._refused({**good, "az": "z7"}, "disagrees with certification.azs[0].az")

    def test_the_first_entry_takes_no_network_or_overrides(self) -> None:
        for key, value in (("network", AZ2_NETWORK), ("bridge", "vmbr9"), ("storage", "s")):
            section = _spanning_section()
            section["azs"][0][key] = value
            self._refused(section, f"certification.azs[0].{key} is not supported")

    def test_the_second_entry_is_checked(self) -> None:
        self._refused(_spanning_section(az="z3"), "azs[1].az must be z2")
        self._refused(_spanning_section(cpi_id="pve-az1"), "binds both AZs to one")
        self._refused(_spanning_section(cpi_id=""), "needs a cpi_id on both entries")
        self._refused(_spanning_section(network=None), "network is required")
        self._refused(_spanning_section(network={**AZ2_NETWORK, "gateway": "10.254.0.1"}),
                      "is outside 10.255.0.0/16")
        self._refused(_spanning_section(network={**AZ2_NETWORK, "cidr": "nope"}),
                      "invalid address")
        self._refused(_spanning_section(network={**AZ2_NETWORK, "reserved": "10.0.0.1"}),
                      "list of addresses or ranges")
        self._refused(_spanning_section(network={"cidr": "10.255.0.0/16", "gateway": "10.255.0.1"}),
                      "network.reserved")
        self._refused(_spanning_section(network={**AZ2_NETWORK, "extra": 1}), "unknown keys: extra")
        self._refused(_spanning_section(bridge="vmbr 1;"), "not a valid name")
        self._refused(_spanning_section(storage="9"), "not a valid name")


class SpanningOpsTest(_CertifyModule):
    """The generated ops describe one deployment across two AZs."""

    def _cloud_config(self, section: dict) -> dict:
        files, variables = _flag_vars(self._certify(section)._cloud_config_args())
        doc = yaml.safe_load((self.mod.REPO_ROOT / "manifests/bosh/cloud-config.yml").read_text())
        for path in files:
            _apply_ops(doc, yaml.safe_load(Path(path).read_text()), variables)
        return doc

    def test_the_deployment_spans_both_azs_with_a_disk_on_each(self) -> None:
        ccfg = self._config(_spanning_section())
        manifest = yaml.safe_load(UPSTREAM_CERTIFICATION_YML)
        _apply_ops(manifest, self.mod.build_deployment_ops(ccfg), {})
        group = manifest["instance_groups"][0]
        self.assertEqual(group["azs"], ["z1", "z2"])
        self.assertEqual(group["instances"], 2)
        self.assertEqual(group["persistent_disk_type"], "default")
        self.assertEqual(group["vm_type"], "certification")

    def test_cloud_config_binds_each_az_to_its_entry_and_names_every_subnet_az(self) -> None:
        doc = self._cloud_config(_spanning_section())
        azs = {a["name"]: a for a in doc["azs"]}
        self.assertEqual(azs["z1"]["cpi"], "pve-az1")
        self.assertEqual(azs["z2"]["cpi"], "pve-az2")
        first, second = doc["networks"][0]["subnets"]
        self.assertEqual(first["az"], "z1")
        self.assertNotIn("azs", first)
        self.assertEqual(second["az"], "z2")
        self.assertEqual(second["range"], "10.255.0.0/16")
        self.assertEqual(second["gateway"], "10.255.0.1")
        self.assertEqual(second["reserved"], AZ2_NETWORK["reserved"])
        for subnet in doc["networks"][0]["subnets"]:
            self.assertTrue(subnet.get("az") or subnet.get("azs"), subnet)

    def test_the_bridge_defaults_to_the_jobs_and_an_override_wins(self) -> None:
        doc = self._cloud_config(_spanning_section())
        self.assertEqual(doc["networks"][0]["subnets"][1]["cloud_properties"]["bridge"], "vmbr0")
        self.assertNotIn("storage_pool", {a["name"]: a for a in doc["azs"]}["z2"]["cloud_properties"])
        doc = self._cloud_config(_spanning_section(bridge="vmbr7", storage="az2-fast"))
        self.assertEqual(doc["networks"][0]["subnets"][1]["cloud_properties"]["bridge"], "vmbr7")
        z2 = {a["name"]: a for a in doc["azs"]}["z2"]
        self.assertEqual(z2["cloud_properties"]["storage_pool"], "az2-fast")
        # z1 keeps the Director's pin and gains nothing.
        z1 = {a["name"]: a for a in doc["azs"]}["z1"]
        self.assertNotIn("storage_pool", z1["cloud_properties"])

    def test_string_flags_stay_strings(self) -> None:
        _, variables = _flag_vars(self._certify(
            _spanning_section(bridge="on", storage="no"))._cloud_config_args())
        self.assertEqual(variables["certification_secondary_bridge"], "on")
        self.assertEqual(variables["certification_secondary_storage"], "no")
        self.assertEqual(variables["certification_secondary_reserved"], AZ2_NETWORK["reserved"])

    def test_the_probe_runs_one_diskless_instance_in_the_second_az(self) -> None:
        ccfg = self._config(_spanning_section())
        manifest = yaml.safe_load(UPSTREAM_CERTIFICATION_YML)
        _apply_ops(manifest, self.mod.build_probe_ops(ccfg), {})
        group = manifest["instance_groups"][0]
        self.assertEqual(group["azs"], ["z2"])
        self.assertEqual(group["instances"], 1)
        self.assertNotIn("persistent_disk_type", group)

    def test_a_dry_run_lists_the_spanning_steps(self) -> None:
        repo = SyntheticRepo()
        self.addCleanup(repo.close)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        slot = Path(tmp.name) / "certification"
        slot.mkdir()
        (slot / "slot.yml").write_text(SLOT_YML)
        cfg = yaml.safe_load((repo.root / CONFIG).read_text())
        cfg["certification"] = {**cfg["certification"], "azs": _spanning_section(
            bridge="vmbr7", storage="az2-fast")["azs"]}
        cfg["certification"].pop("cpi_id", None)
        (repo.root / "ci").mkdir(exist_ok=True)
        (repo.root / "ci" / "spanning.yml").write_text(yaml.safe_dump(cfg))
        rc, out = _dry_run(repo, str(slot), tmp.name, config="ci/spanning.yml")
        self.assertEqual(rc, 0, out)
        for fragment in ("cloud-config-cpi2-ops.yml", "cloud-config-cpi2-storage-ops.yml",
                         "certification_secondary_cpi_id=", "precheck:z2-reachability",
                         "precheck:z2-probe-delete", "verify:placement", "cert:delete-probe",
                         "lab:reachable-secondary", "spanning:cpi-config"):
            self.assertIn(fragment, out)
        lines = out.splitlines()
        first = lambda frag: next(i for i, line in enumerate(lines) if frag in line)  # noqa: E731
        self.assertLess(first("precheck:z2-reachability"), first("cert:deploy"))
        self.assertLess(first("precheck:z2-probe-delete"), first("cert:deploy"))


# A disk CID is a pvd- envelope; the CPI records the volume it was born as and
# the stable id the guest sees as the drive serial.
def _disk_cid(volid: str, stable: str) -> str:
    raw = json.dumps({"v": volid, "m": {"id": stable}}).encode()
    return "pvd-" + base64.urlsafe_b64encode(raw).decode().rstrip("=")


DIRECTOR_UUID = "7a6d4f2e-1111-4222-8333-444455556666"
STAMP_Z1, STAMP_Z2 = "0a1b2c3d", "4e5f6a7b"
CID_Z1 = f"301-{STAMP_Z1}-11111111"
CID_Z2 = f"5101-{STAMP_Z2}-22222222"
DISK_Z1 = _disk_cid("local-lvm-data:vm-9001-disk-0", "bpd-aaaaaaaaaaaaaaaa")
DISK_Z2 = _disk_cid("local-lvm-data:vm-20001-disk-0", "bpd-bbbbbbbbbbbbbbbb")
ENTRIES = {
    "z1": {"cpi_id": "pve-az1", "host": "10.254.0.10", "vm_prefix": "cpi",
           "vm_range": (200, 4999), "disk_range": (9000, 19999),
           "bridge": "", "storage": ""},
    "z2": {"cpi_id": "pve-az2", "host": "10.255.0.10", "vm_prefix": "cpi",
           "vm_range": (5000, 8999), "disk_range": (20000, 29999),
           "bridge": "", "storage": ""},
}


def _tags(stamp: str, token: str, owner: str = DIRECTOR_UUID) -> str:
    return f"bosh-cpi;bosh-owner--{owner};bosh-cluster--{stamp};bosh-cid--{token}"


def _canned_observation(*, stamped: bool = True) -> dict:
    """Both clusters' /cluster/resources JSON, as PVE returns it, thinned the way certify does."""
    z1 = [
        {"id": "qemu/301", "type": "qemu", "vmid": 301, "node": "lab-pve-cpi-0", "status": "running",
         "name": "cpi-certification-simple-0",
         "tags": _tags(STAMP_Z1, "11111111") if stamped else "bosh-cpi"},
        {"id": "qemu/200", "type": "qemu", "vmid": 200, "node": "lab-pve-cpi-1", "status": "running",
         "name": "unrelated", "tags": ""},
        {"id": "lxc/150", "type": "lxc", "vmid": 150, "node": "lab-pve-cpi-1", "name": "a-container"},
    ]
    z2 = [
        {"id": "qemu/5101", "type": "qemu", "vmid": 5101, "node": "lab-pve-cpi-az2-0",
         "status": "running", "name": "cpi-certification-simple-1",
         "tags": _tags(STAMP_Z2, "22222222") if stamped else "bosh-cpi"},
    ]
    return {
        "director_uuid": DIRECTOR_UUID,
        "instances": _spanning.parse_instances([
            {"instance": "simple/aaaa", "az": "z1", "vm_cid": CID_Z1 if stamped else "301",
             "disk_cids": DISK_Z1},
            {"instance": "simple/bbbb", "az": "z2", "vm_cid": CID_Z2 if stamped else "5101",
             "disk_cids": DISK_Z2},
        ]),
        "clusters": {
            "z1": {"resources": _spanning.thin_resources(z1), "error": "", "configs": {"301": _spanning.thin_config({
                "net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0",
                "scsi0": "local-lvm-data:vm-301-disk-0,size=8G",
                "scsi1": "local-lvm-data:vm-9001-disk-0,serial=bpd-aaaaaaaaaaaaaaaa",
                "ide2": "local:iso/vm-301-config.iso,media=cdrom",
                "cipassword": "not-kept"})}},
            "z2": {"resources": _spanning.thin_resources(z2), "error": "", "configs": {"5101": _spanning.thin_config({
                "net0": "virtio=BC:24:11:00:00:02,bridge=vmbr7",
                "scsi0": "az2-fast:vm-5101-disk-0,size=8G",
                "scsi1": "local-lvm-data:vm-20001-disk-0,serial=bpd-bbbbbbbbbbbbbbbb"})}},
        },
    }


def _held(findings: list) -> dict:
    return {f.name: f.ok for f in findings}


def _why(findings: list, name: str) -> str:
    return next(f.detail for f in findings if f.name == name)


class PlacementCheckTest(unittest.TestCase):
    def check(self, obs: dict, entries: "dict | None" = None, stamped: bool = True) -> list:
        return _spanning.check_placement(obs, entries or ENTRIES, stamped)

    def test_the_canned_resources_are_thinned_to_qemu_rows(self) -> None:
        obs = _canned_observation()
        self.assertEqual([r["vmid"] for r in obs["clusters"]["z1"]["resources"]], ["301", "200"])
        self.assertNotIn("cipassword", obs["clusters"]["z1"]["configs"]["301"])
        self.assertIn("ide2", obs["clusters"]["z1"]["configs"]["301"])

    def test_a_correct_stamped_deployment_holds_every_check(self) -> None:
        findings = self.check(_canned_observation())
        self.assertTrue(all(f.ok for f in findings), [f for f in findings if not f.ok])
        self.assertEqual(
            [f.name for f in findings],
            ["placement:spans-azs", "placement:vm-cluster", "placement:strays", "placement:vm-band",
             "placement:vm-prefix", "placement:tags", "placement:cluster-stamps",
             "placement:disk-cluster", "placement:disk-band", "placement:bridge-override",
             "placement:storage-override"],
        )

    def test_bare_cids_from_the_old_release_pass_before_the_upgrade_only(self) -> None:
        obs = _canned_observation(stamped=False)
        self.assertTrue(all(f.ok for f in self.check(obs, stamped=False)))
        findings = self.check(obs, stamped=True)
        self.assertFalse(_held(findings)["placement:tags"])
        self.assertIn("bare CID", _why(findings, "placement:tags"))
        self.assertFalse(_held(findings)["placement:cluster-stamps"])

    def test_a_vm_on_the_wrong_cluster_fails(self) -> None:
        obs = _canned_observation()
        z2 = obs["clusters"]["z2"]
        z1 = obs["clusters"]["z1"]
        z1["resources"], z2["resources"] = z2["resources"], z1["resources"]
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:vm-cluster"])
        self.assertIn("is not on the z1 cluster", _why(findings, "placement:vm-cluster"))

    def test_a_vm_that_also_exists_on_the_other_cluster_fails(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z1"]["resources"].append(
            {"vmid": "5101", "node": "lab-pve-cpi-2", "name": "stray", "tags": ""})
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:vm-cluster"])
        self.assertIn("also exists on the z1 cluster", _why(findings, "placement:vm-cluster"))

    def test_a_vmid_outside_its_entrys_band_fails(self) -> None:
        entries = {**ENTRIES, "z2": {**ENTRIES["z2"], "vm_range": (6000, 8999)}}
        findings = self.check(_canned_observation(), entries)
        self.assertFalse(_held(findings)["placement:vm-band"])
        self.assertIn("5101", _why(findings, "placement:vm-band"))

    def test_the_vm_name_must_carry_the_entrys_prefix(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z2"]["resources"][0]["name"] = "certification-simple-1"
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:vm-prefix"])
        entries = {az: {**e, "vm_prefix": ""} for az, e in ENTRIES.items()}
        self.assertTrue(_held(self.check(obs, entries))["placement:vm-prefix"])

    def test_the_owner_tag_must_name_this_director(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z1"]["resources"][0]["tags"] = _tags(STAMP_Z1, "11111111", owner="someone-else")
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:tags"])
        self.assertIn(f"want bosh-owner--{DIRECTOR_UUID}", _why(findings, "placement:tags"))

    def test_a_missing_owner_tag_fails(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z1"]["resources"][0]["tags"] = f"bosh-cpi;bosh-cluster--{STAMP_Z1};bosh-cid--11111111"
        self.assertFalse(_held(self.check(obs))["placement:tags"])

    def test_the_owner_tag_is_compared_the_way_the_cpi_writes_it(self) -> None:
        # The CPI lowercases the UUID and turns every other byte into "-".
        self.assertEqual(_spanning.owner_tag("ABC_def.01"), "bosh-owner--abc-def-01")
        self.assertEqual(_spanning.owner_tag(""), "")
        obs = _canned_observation()
        obs["director_uuid"] = DIRECTOR_UUID.upper()
        self.assertTrue(_held(self.check(obs))["placement:tags"])
        obs["director_uuid"] = ""
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:tags"])
        self.assertIn("Director UUID is unknown", _why(findings, "placement:tags"))

    def test_the_cluster_tag_must_match_the_cids_stamp(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z2"]["resources"][0]["tags"] = _tags(STAMP_Z1, "22222222")
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:tags"])
        self.assertIn(f"want bosh-cluster--{STAMP_Z2}", _why(findings, "placement:tags"))

    def test_the_token_tag_must_match_the_cids_token(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z2"]["resources"][0]["tags"] = _tags(STAMP_Z2, "99999999")
        self.assertFalse(_held(self.check(obs))["placement:tags"])

    def test_two_clusters_with_one_stamp_fail(self) -> None:
        obs = _canned_observation()
        obs["instances"][1]["vm_cid"] = f"5101-{STAMP_Z1}-22222222"
        obs["clusters"]["z2"]["resources"][0]["tags"] = _tags(STAMP_Z1, "22222222")
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:cluster-stamps"])
        self.assertIn("same cluster stamp", _why(findings, "placement:cluster-stamps"))

    def test_an_instance_in_each_az_is_required(self) -> None:
        obs = _canned_observation()
        obs["instances"][1]["az"] = "z1"
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:spans-azs"])
        self.assertIn("no instance in z2", _why(findings, "placement:spans-azs"))

    def test_an_instance_in_an_unconfigured_az_fails(self) -> None:
        obs = _canned_observation()
        obs["instances"][1]["az"] = "z3"
        self.assertFalse(_held(self.check(obs))["placement:vm-cluster"])

    def test_a_disk_must_be_a_drive_of_its_instances_vm(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z2"]["configs"]["5101"].pop("scsi1")
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:disk-cluster"])
        obs = _canned_observation()
        obs["instances"][0]["disk_cids"] = []
        self.assertIn("no persistent disk", _why(self.check(obs), "placement:disk-cluster"))

    def test_a_disk_is_found_by_its_volume_when_it_has_no_serial(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z1"]["configs"]["301"]["scsi1"] = "local-lvm-data:vm-9001-disk-0,size=1G"
        self.assertTrue(_held(self.check(obs))["placement:disk-cluster"])

    def test_a_disk_born_outside_the_entrys_disk_band_fails(self) -> None:
        entries = {**ENTRIES, "z1": {**ENTRIES["z1"], "disk_range": (9500, 19999)}}
        findings = self.check(_canned_observation(), entries)
        self.assertFalse(_held(findings)["placement:disk-band"])
        self.assertIn("9001", _why(findings, "placement:disk-band"))

    def test_a_distinct_bridge_must_reach_every_nic(self) -> None:
        entries = {**ENTRIES, "z2": {**ENTRIES["z2"], "bridge": "vmbr7"}}
        self.assertTrue(_held(self.check(_canned_observation(), entries))["placement:bridge-override"])
        entries = {**ENTRIES, "z2": {**ENTRIES["z2"], "bridge": "vmbr8"}}
        findings = self.check(_canned_observation(), entries)
        self.assertFalse(_held(findings)["placement:bridge-override"])
        self.assertIn("not on bridge vmbr8", _why(findings, "placement:bridge-override"))

    def test_a_fallback_to_the_jobs_bridge_fails(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z2"]["configs"]["5101"]["net0"] = "virtio=BC:24:11:00:00:02,bridge=vmbr0"
        entries = {**ENTRIES, "z2": {**ENTRIES["z2"], "bridge": "vmbr7"}}
        self.assertFalse(_held(self.check(obs, entries))["placement:bridge-override"])

    def test_a_distinct_storage_must_hold_the_boot_drive(self) -> None:
        entries = {**ENTRIES, "z2": {**ENTRIES["z2"], "storage": "az2-fast"}}
        self.assertTrue(_held(self.check(_canned_observation(), entries))["placement:storage-override"])
        obs = _canned_observation()
        obs["clusters"]["z2"]["configs"]["5101"]["scsi0"] = "local-lvm-data:vm-5101-disk-0,size=8G"
        findings = self.check(obs, entries)
        self.assertFalse(_held(findings)["placement:storage-override"])
        self.assertIn("not az2-fast", _why(findings, "placement:storage-override"))

    def test_an_unreadable_cluster_is_one_clear_finding(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z2"]["error"] = "PVE API read failed: HTTP 401 Unauthorized"
        findings = self.check(obs)
        self.assertEqual([f.name for f in findings], ["placement:cluster-resources"])
        self.assertFalse(findings[0].ok)
        self.assertIn("z2: PVE API read failed", findings[0].detail)

    def test_no_instances_fails(self) -> None:
        obs = _canned_observation()
        obs["instances"] = []
        self.assertFalse(self.check(obs)[0].ok)

    def test_an_unparseable_cid_is_named(self) -> None:
        obs = _canned_observation()
        obs["instances"][0]["vm_cid"] = "not-a-cid"
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:vm-cluster"])
        self.assertIn("'not-a-cid'", _why(findings, "placement:vm-cluster"))

    def test_instances_parse_from_either_key_spelling(self) -> None:
        rows = [{"Instance": "simple/x", "AZ": "z1", "VM CID": "301", "Disk CIDs": "d1, d2"},
                {"instance": "simple/y", "az": "z2", "vm_cid": "", "disk_cids": ""}]
        parsed = _spanning.parse_instances(rows)
        self.assertEqual(parsed[0], {"instance": "simple/x", "index": "", "az": "z1", "vm_cid": "301",
                                     "disk_cids": ["d1", "d2"]})
        self.assertEqual(parsed[1]["vm_cid"], "")
        self.assertEqual(parsed[1]["disk_cids"], [])


class OldVmsGoneTest(unittest.TestCase):
    def test_the_first_releases_vms_must_be_gone_from_both_clusters(self) -> None:
        pre = _canned_observation(stamped=False)
        post = _canned_observation()
        # The recreate built new VMs under new VMIDs.
        post["clusters"]["z1"]["resources"] = _spanning.thin_resources(
            [{"vmid": 302, "node": "n", "name": "cpi-new", "tags": ""}])
        post["clusters"]["z2"]["resources"] = _spanning.thin_resources(
            [{"vmid": 5102, "node": "n", "name": "cpi-new", "tags": ""}])
        finding = _spanning.check_old_vms_gone(pre, post)
        self.assertTrue(finding.ok, finding.detail)
        self.assertEqual(finding.name, "placement:old-vms-deleted")

    def test_a_leftover_vm_names_its_cluster(self) -> None:
        pre = _canned_observation(stamped=False)
        post = _canned_observation()
        post["clusters"]["z2"]["resources"] = _spanning.thin_resources(
            [{"vmid": 5101, "node": "n", "name": "cpi-old", "tags": ""}])
        finding = _spanning.check_old_vms_gone(pre, post)
        self.assertFalse(finding.ok)
        self.assertIn("VM 5101 is still on the z2 cluster", finding.detail)

    def test_an_unreadable_cluster_cannot_prove_absence(self) -> None:
        pre = _canned_observation(stamped=False)
        post = _canned_observation()
        post["clusters"]["z1"]["error"] = "timeout"
        self.assertFalse(_spanning.check_old_vms_gone(pre, post).ok)
        self.assertFalse(_spanning.check_old_vms_gone({"instances": []}, post).ok)


class CpiEntryFactsTest(unittest.TestCase):
    """The real manifests/cpi-config.yml supplies the hosts, bands, and prefix."""

    VARS = {"/pve_az1_host": "10.254.0.10", "/pve_az2_host": "10.255.0.10",
            "/pve_port": "8006", "/pve_vm_prefix": "cpi"}

    def _facts(self, cpi_id: str, variables: "dict | None" = None) -> dict:
        reads: list[str] = []
        table = self.VARS if variables is None else variables

        def reader(path: str) -> "str | None":
            reads.append(path)
            return table.get(path)

        self.reads = reads
        return _spanning.cpi_entry_facts(REPO_ROOT / "manifests" / "cpi-config.yml", cpi_id, reader)

    def test_the_two_lab_entries_have_disjoint_bands(self) -> None:
        one, two = self._facts("pve-az1"), self._facts("pve-az2")
        self.assertEqual((one["host"], two["host"]), ("10.254.0.10", "10.255.0.10"))
        self.assertEqual(one["vm_range"], (200, 4999))
        self.assertEqual(two["vm_range"], (5000, 8999))
        self.assertEqual(one["disk_range"], (9000, 19999))
        self.assertEqual(two["disk_range"], (20000, 29999))
        self.assertEqual(one["vm_prefix"], "cpi")
        self.assertLess(one["vm_range"][1], two["vm_range"][0])
        self.assertLess(one["disk_range"][1], two["disk_range"][0])

    def test_only_non_secret_variables_are_read(self) -> None:
        self._facts("pve-az2")
        self.assertTrue(set(self.reads) <= set(self.VARS), self.reads)
        for path in self.reads:
            self.assertNotRegex(path, r"password|token|secret")

    def test_a_missing_entry_or_variable_is_named(self) -> None:
        with self.assertRaisesRegex(ValueError, "no cpi-config entry named pve-az9"):
            self._facts("pve-az9")
        with self.assertRaisesRegex(ValueError, "pve_az2_host"):
            self._facts("pve-az2", {"/pve_port": "8006"})

    def test_a_missing_band_is_named(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "cpi-config.yml"
            path.write_text(yaml.safe_dump({"cpis": [{"name": "e", "properties": {"pve": {
                "host": "h", "vmid_range_start": 200, "vmid_range_end": 4999}}}]}))
            with self.assertRaisesRegex(ValueError, "disk_vmid_range_start"):
                _spanning.cpi_entry_facts(path, "e", lambda _p: None)

    def test_an_unset_prefix_variable_means_no_prefix(self) -> None:
        facts = self._facts("pve-az1", {k: v for k, v in self.VARS.items() if k != "/pve_vm_prefix"})
        self.assertEqual(facts["vm_prefix"], "")


class ProbeTest(_CertifyModule):
    def test_routing_failures_are_told_from_cpi_failures(self) -> None:
        routing = ["Task 12 | Timed out pinging VM 'vm-1' with agent 'x' after 600 seconds"]
        other = ["Task 12 | CPI error 'Bosh::Clouds::CloudError' with message 'storage nope not found'"]
        self.assertEqual(_spanning.classify_probe_failure(routing), "routing")
        self.assertEqual(_spanning.classify_probe_failure(other), "other")
        self.assertEqual(_spanning.classify_probe_failure([]), "other")

    def _certify_with(self, deploy_ok: bool, tail: list) -> object:
        c = self._certify(_spanning_section())
        c.ccfg["deployment_name"] = "certification"
        c.rep = _Rep()
        c.dry_run = True
        c.checkout = Path("/nowhere")
        c.old_stemcell = (Path("/nowhere"), "stemcell", "1", "x")
        c.run_dir = Path("/nowhere/run")
        c._director_env = lambda: {}
        c.calls = []

        def bosh(name, argv, timeout):
            c.calls.append((name, argv))
            return (deploy_ok or name != "precheck:z2-reachability"), (tail if not deploy_ok else [])

        c._bosh = bosh
        return c

    def test_a_good_probe_deploys_then_deletes(self) -> None:
        c = self._certify_with(True, [])
        self.assertTrue(c._probe_secondary_network())
        self.assertEqual([n for n, _ in c.calls], ["precheck:z2-reachability", "precheck:z2-probe-delete"])
        self.assertEqual(c.calls[0][1][:4], ["bosh", "-n", "-d", "certification-z2-probe"])
        self.assertIn("delete-deployment", c.calls[1][1])
        self.assertFalse(c.probe_deployed)

    def test_a_routing_gap_is_reported_as_one_and_the_probe_is_still_deleted(self) -> None:
        c = self._certify_with(False, ["Timed out pinging VM 'v' with agent 'a' after 600 seconds"])
        self.assertFalse(c._probe_secondary_network())
        self.assertEqual([n for n, _ in c.calls], ["precheck:z2-reachability", "precheck:z2-probe-delete"])
        diagnosis = [row for row in c.rep.rows if row[0] == "precheck:z2-diagnosis"]
        self.assertEqual(len(diagnosis), 1)
        self.assertIn("routing gap", diagnosis[0][2])

    def test_another_failure_is_not_blamed_on_routing(self) -> None:
        c = self._certify_with(False, ["CPI error: storage not found"])
        self.assertFalse(c._probe_secondary_network())
        diagnosis = [row for row in c.rep.rows if row[0] == "precheck:z2-diagnosis"]
        self.assertNotIn("routing gap", diagnosis[0][2])


class SpanningSecretsTest(_CertifyModule):
    """The PVE reads use the existing variables and print none of them."""

    PASSWORD, TOKEN = "hunter2-sentinel", "root@pam!sentinel=00000000-0000-4000-8000-000000000000"

    def _certify_for_reads(self) -> object:
        c = self._certify(_spanning_section())
        c.rep = _Rep()
        c.az_facts = {az: {"host": e["host"], "cpi_id": e["cpi_id"]} for az, e in ENTRIES.items()}
        c._pve_cfg = lambda: {"host": "10.254.0.10", "port": 8006, "user": "root@pam",
                              "password": self.PASSWORD, "api_token": self.TOKEN,
                              "verify_ssl": False}
        c._capture_json = lambda argv, **_kw: (
            {"Tables": [{"Rows": [{"uuid": DIRECTOR_UUID}]}]} if argv[:2] == ["bosh", "env"] else
            {"Tables": [{"Rows": [
                {"instance": "simple/a", "az": "z1", "vm_cid": CID_Z1, "disk_cids": DISK_Z1},
                {"instance": "simple/b", "az": "z2", "vm_cid": CID_Z2, "disk_cids": DISK_Z2}]}]})
        return c

    def test_each_cluster_is_read_with_its_own_host_and_the_same_credentials(self) -> None:
        c = self._certify_for_reads()
        seen: list[tuple[str, str, str]] = []

        def api(cfg, path, **_kw):
            seen.append((cfg["host"], path, cfg.get("password", "")))
            if path.startswith("/cluster/resources"):
                return [{"vmid": 301, "node": "n1", "name": "x", "tags": "", "type": "qemu"},
                        {"vmid": 5101, "node": "n2", "name": "y", "tags": "", "type": "qemu"}]
            return {"scsi0": "s:vm-1-disk-0", "cipassword": "kept-out"}

        with mock.patch.object(_integration, "pve_api_get", api):
            obs = c._placement_observation()
        hosts = {(host, path) for host, path, _ in seen}
        self.assertIn(("10.254.0.10", "/cluster/resources?type=vm"), hosts)
        self.assertIn(("10.255.0.10", "/cluster/resources?type=vm"), hosts)
        self.assertIn(("10.254.0.10", "/nodes/n1/qemu/301/config"), hosts)
        self.assertIn(("10.255.0.10", "/nodes/n2/qemu/5101/config"), hosts)
        self.assertEqual({pw for _, _, pw in seen}, {self.PASSWORD})
        self.assertEqual(obs["director_uuid"], DIRECTOR_UUID)
        text = json.dumps(obs)
        for secret in (self.PASSWORD, self.TOKEN, "kept-out"):
            self.assertNotIn(secret, text)

    def test_a_failed_read_is_kept_without_a_secret(self) -> None:
        c = self._certify_for_reads()

        def api(cfg, path, **_kw):
            raise RuntimeError(f"PVE GET {path} on {cfg['host']}:8006 failed: HTTP 401 Unauthorized")

        with mock.patch.object(_integration, "pve_api_get", api):
            obs = c._placement_observation()
        self.assertIn("HTTP 401", obs["clusters"]["z2"]["error"])
        text = json.dumps(obs)
        self.assertNotIn(self.PASSWORD, text)
        self.assertNotIn(self.TOKEN, text)

    def test_verify_records_findings_without_a_secret(self) -> None:
        c = self._certify_for_reads()
        c.invariants = []
        c.az_facts = {az: {**{k: v for k, v in e.items() if k not in ("bridge", "storage")}}
                      for az, e in ENTRIES.items()}
        c.pre = {"placement": _canned_observation(stamped=False)}
        post = _canned_observation()
        post["clusters"]["z1"]["resources"] = _spanning.thin_resources(
            [{"vmid": 302, "node": "n", "name": "cpi-new", "tags": _tags(STAMP_Z1, "11111111")}])
        post["clusters"]["z2"]["resources"] = _spanning.thin_resources(
            [{"vmid": 5101, "node": "n", "name": "cpi-new", "tags": _tags(STAMP_Z2, "22222222")}])
        c.post = {"placement": post}
        self.assertFalse(c._verify_placement())
        names = [r[0] for r in c.rep.rows]
        self.assertIn("verify:placement-pre-vm-cluster", names)
        self.assertIn("verify:placement-post-vm-cluster", names)
        self.assertIn("verify:placement-old-vms-deleted", names)
        self.assertEqual(c.rep.status("verify:placement-old-vms-deleted"), ["FAIL"])
        self.assertFalse(c.invariants[-1]["held"])
        blob = json.dumps(c.rep.rows) + json.dumps(c.invariants)
        for secret in (self.PASSWORD, self.TOKEN):
            self.assertNotIn(secret, blob)

    def test_the_real_preflight_reads_only_non_secret_variables(self) -> None:
        c = self._certify_for_reads()
        c.dry_run = False
        c.env_name = "pve-cpi"
        reads: list[str] = []

        def layered(_env, path):
            reads.append(path)
            return {"/pve_az1_host": "10.254.0.10", "/pve_az2_host": "10.255.0.10",
                    "/pve_port": "8006", "/pve_vm_prefix": "cpi"}.get(path)

        with mock.patch.object(self.mod, "layered_var", layered):
            self.assertTrue(c._preflight_spanning())
        self.assertTrue(reads)
        for path in reads:
            self.assertNotRegex(path, r"password|token|secret")
        self.assertEqual(c.rep.status("spanning:cpi-config"), ["PASS"])

    def test_two_entries_on_one_host_are_refused(self) -> None:
        c = self._certify_for_reads()
        c.dry_run = False
        c.env_name = "pve-cpi"
        with mock.patch.object(self.mod, "layered_var", lambda _e, p: {"/pve_port": "8006", "/pve_vm_prefix": "cpi"}.get(p, "10.0.0.1")):
            self.assertFalse(c._preflight_spanning())
        self.assertIn("same PVE host", c.rep.rows[-1][2])


# --------------------------------------------------------------------------- #
# Fix round: disk serials across clusters, strays, probe cleanup, markers
# --------------------------------------------------------------------------- #

DISK_DIRECTOR = _disk_cid("local-lvm-data:vm-9100-disk-0", "bpd-dddddddddddddddd")
HOST_Z1, HOST_Z2 = "10.254.0.10", "10.255.0.10"


def _serial_capture_target() -> dict:
    return {
        "director": {"vm_cid": "100", "disk_cids": [DISK_DIRECTOR]},
        "deployment": {"vm_cids": [CID_Z1, CID_Z2], "disk_cids": [DISK_Z1, DISK_Z2]},
    }


def _cluster_api(tables: dict):
    """A pve_api_get over canned per-host tables: {host: {"resources": [...], "configs": {vmid: {...}}}}."""
    seen: list[tuple[str, str]] = []

    def api(cfg, path, **_kw):
        seen.append((cfg["host"], path))
        table = tables[cfg["host"]]
        if isinstance(table, Exception):
            raise table
        if path == "/cluster/resources?type=vm":
            return table["resources"]
        match = re.fullmatch(r"/nodes/[^/]+/qemu/(\d+)/config", path)
        if match and match.group(1) in table["configs"]:
            return table["configs"][match.group(1)]
        raise RuntimeError(f"PVE GET {path} on {cfg['host']}:8006 failed: HTTP 500 no such VM")

    api.seen = seen
    return api


def _two_cluster_tables() -> dict:
    return {
        HOST_Z1: {
            "resources": [{"vmid": 100, "node": "n0", "type": "qemu"},
                          {"vmid": 301, "node": "n1", "type": "qemu"}],
            "configs": {
                "100": {"scsi0": "local:vm-100-disk-0", "scsi1": "local:vm-9100-disk-0,serial=bpd-dddddddddddddddd"},
                "301": {"scsi0": "local:vm-301-disk-0", "scsi1": "local:vm-9001-disk-0,serial=bpd-aaaaaaaaaaaaaaaa"},
            },
        },
        HOST_Z2: {
            "resources": [{"vmid": 5101, "node": "n2", "type": "qemu"}],
            "configs": {
                "5101": {"scsi0": "local:vm-5101-disk-0", "scsi1": "local:vm-20001-disk-0,serial=bpd-bbbbbbbbbbbbbbbb"},
            },
        },
    }


class DiskSerialsAcrossClustersTest(_CertifyModule):
    """The disk-serial check reads every AZ's cluster, so a correct span passes it."""

    def _certify_for(self, section: dict) -> object:
        c = self._certify(section)
        c.rep = _Rep()
        c.invariants = []
        c.az_facts = {az: {"host": e["host"], "cpi_id": e["cpi_id"]} for az, e in ENTRIES.items()}
        c._pve_cfg = lambda: {"host": HOST_Z1, "port": 8006, "user": "root@pam", "password": "x"}
        return c

    def _capture(self, c: object, tables: dict) -> dict:
        with mock.patch.object(_integration, "pve_api_get", _cluster_api(tables)):
            return c._disk_serials(_serial_capture_target())

    def _verify(self, c: object, capture: dict) -> bool:
        c.pre, c.post = {"disks": capture}, {"disks": capture}
        return c._verify_disk_serials()

    def test_a_correct_span_passes(self) -> None:
        c = self._certify_for(_spanning_section())
        capture = self._capture(c, _two_cluster_tables())
        self.assertEqual(capture["errors"], [])
        self.assertEqual(capture["locations"][DISK_Z1], "z1:301/scsi1")
        self.assertEqual(capture["locations"][DISK_Z2], "z2:5101/scsi1")
        self.assertEqual(capture["locations"][DISK_DIRECTOR], "z1:100/scsi1")
        self.assertTrue(self._verify(c, capture))
        self.assertEqual(c.rep.status("verify:disk-serial-stable"), ["PASS"])

    def test_each_cluster_is_asked_only_for_its_own_vms(self) -> None:
        c = self._certify_for(_spanning_section())
        api = _cluster_api(_two_cluster_tables())
        with mock.patch.object(_integration, "pve_api_get", api):
            c._disk_serials(_serial_capture_target())
        self.assertIn((HOST_Z1, "/nodes/n0/qemu/100/config"), api.seen)
        self.assertIn((HOST_Z1, "/nodes/n1/qemu/301/config"), api.seen)
        self.assertIn((HOST_Z2, "/nodes/n2/qemu/5101/config"), api.seen)
        self.assertNotIn((HOST_Z2, "/nodes/n0/qemu/100/config"), api.seen)

    def test_a_z2_disk_missing_from_z2_fails(self) -> None:
        tables = _two_cluster_tables()
        tables[HOST_Z2]["configs"]["5101"].pop("scsi1")
        c = self._certify_for(_spanning_section())
        capture = self._capture(c, tables)
        self.assertEqual(capture["locations"][DISK_Z2], "")
        self.assertFalse(self._verify(c, capture))
        self.assertIn("no drive carried serial=bpd-bbbbbbbbbbbbbbbb", c.rep.rows[-1][2])

    def test_a_disk_listed_on_both_clusters_fails(self) -> None:
        tables = _two_cluster_tables()
        tables[HOST_Z1]["configs"]["301"]["scsi2"] = "local:vm-20001-disk-0,serial=bpd-bbbbbbbbbbbbbbbb"
        c = self._certify_for(_spanning_section())
        capture = self._capture(c, tables)
        self.assertEqual(capture["locations"][DISK_Z2], "")
        self.assertTrue(any("more than one cluster (z1, z2)" in e for e in capture["errors"]), capture["errors"])
        self.assertFalse(self._verify(c, capture))
        self.assertIn("more than one cluster", c.rep.rows[-1][2])

    def test_a_vm_no_cluster_lists_is_named(self) -> None:
        tables = _two_cluster_tables()
        tables[HOST_Z2]["resources"] = []
        c = self._certify_for(_spanning_section())
        capture = self._capture(c, tables)
        self.assertIn(f"VM {CID_Z2} is not in /cluster/resources on any cluster", capture["errors"])
        self.assertFalse(self._verify(c, capture))

    def test_an_unreadable_cluster_is_named_and_the_other_is_still_read(self) -> None:
        tables = _two_cluster_tables()
        tables[HOST_Z2] = RuntimeError("PVE GET /cluster/resources?type=vm on 10.255.0.10:8006 failed: HTTP 401")
        c = self._certify_for(_spanning_section())
        capture = self._capture(c, tables)
        self.assertTrue(any("on the z2 cluster" in e and "HTTP 401" in e for e in capture["errors"]), capture["errors"])
        self.assertEqual(capture["locations"][DISK_Z1], "z1:301/scsi1")
        self.assertFalse(self._verify(c, capture))

    def test_a_one_cluster_run_still_reads_the_one_cluster(self) -> None:
        c = self._certify_for({"network_bridge": "vmbr0"})
        tables = {HOST_Z1: {
            "resources": [{"vmid": 100, "node": "n0"}, {"vmid": 301, "node": "n1"}, {"vmid": 5101, "node": "n1"}],
            "configs": {
                "100": {"scsi1": "local:vm-9100-disk-0,serial=bpd-dddddddddddddddd"},
                "301": {"scsi1": "local:vm-9001-disk-0,serial=bpd-aaaaaaaaaaaaaaaa"},
                "5101": {"scsi1": "local:vm-20001-disk-0,serial=bpd-bbbbbbbbbbbbbbbb"},
            },
        }}
        capture = self._capture(c, tables)
        self.assertEqual(capture["errors"], [])
        self.assertEqual(capture["locations"][DISK_Z1], "301/scsi1")
        self.assertEqual(sorted(capture["drive_serials"]), ["100", "301", "5101"])
        self.assertTrue(self._verify(c, capture))
        # A VM the cluster does not list keeps its original message.
        tables[HOST_Z1]["resources"].pop()
        self.assertIn(f"VM {CID_Z2} is not in /cluster/resources", self._capture(c, tables)["errors"])


class PlacementStraysAndStampsTest(unittest.TestCase):
    def check(self, obs: dict, entries: "dict | None" = None) -> list:
        return _spanning.check_placement(obs, entries or ENTRIES, True)

    def test_a_stray_vm_with_this_directors_owner_tag_fails(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z2"]["resources"].extend(_spanning.thin_resources(
            [{"vmid": 5900, "node": "n", "name": "cpi-orphan", "tags": _tags(STAMP_Z2, "33333333")}]))
        findings = _spanning.check_placement(obs, ENTRIES, True)
        self.assertFalse(_held(findings)["placement:strays"])
        self.assertIn("VM 5900 on the z2 cluster", _why(findings, "placement:strays"))

    def test_the_directors_own_vm_and_foreign_vms_are_not_strays(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z1"]["resources"].extend(_spanning.thin_resources([
            {"vmid": 100, "node": "n", "name": "cpi-director", "tags": _tags(STAMP_Z1, "44444444")},
            {"vmid": 4000, "node": "n", "name": "someone-else", "tags": _tags(STAMP_Z1, "55555555", owner="other")},
        ]))
        self.assertFalse(_held(self.check(obs))["placement:strays"])
        obs["exempt_vmids"] = ["100"]
        self.assertTrue(_held(self.check(obs))["placement:strays"])

    def test_an_unknown_owner_leaves_nothing_to_look_for(self) -> None:
        obs = _canned_observation(stamped=False)
        obs["director_uuid"] = ""
        findings = _spanning.check_placement(obs, ENTRIES, False)
        self.assertTrue(_held(findings)["placement:strays"])

    def test_a_vm_stamped_with_the_unknown_cluster_fails(self) -> None:
        obs = _canned_observation()
        unknown = "00000000"
        obs["instances"][1]["vm_cid"] = f"5101-{unknown}-22222222"
        obs["clusters"]["z2"]["resources"][0]["tags"] = _tags(unknown, "22222222")
        findings = self.check(obs)
        self.assertTrue(_held(findings)["placement:tags"])
        self.assertFalse(_held(findings)["placement:cluster-stamps"])
        self.assertIn("unknown cluster", _why(findings, "placement:cluster-stamps"))

    def test_a_disk_that_is_also_a_drive_on_the_other_cluster_fails(self) -> None:
        obs = _canned_observation()
        obs["clusters"]["z1"]["configs"]["301"]["scsi2"] = "local-lvm-data:vm-20001-disk-0,serial=bpd-bbbbbbbbbbbbbbbb"
        findings = self.check(obs)
        self.assertFalse(_held(findings)["placement:disk-cluster"])
        self.assertIn("is also a drive of VM 301 on the z1 cluster", _why(findings, "placement:disk-cluster"))


class ProbeCleanupTest(_CertifyModule):
    def _certify_with(self, results: dict) -> object:
        c = self._certify(_spanning_section())
        c.ccfg["deployment_name"] = "certification"
        c.rep = _Rep()
        c.dry_run = True
        c.checkout = Path("/nowhere")
        c.old_stemcell = (Path("/nowhere"), "stemcell", "1", "x")
        c.run_dir = Path("/nowhere/run")
        c._director_env = lambda: {}
        c.calls = []
        c.flag_at_deploy = None

        def bosh(name, argv, timeout):
            c.calls.append(name)
            if name == "precheck:z2-reachability":
                c.flag_at_deploy = c.probe_deployed
            return results.get(name, True), []

        c._bosh = bosh
        return c

    def test_the_probe_is_marked_deployed_before_it_deploys(self) -> None:
        c = self._certify_with({})
        self.assertTrue(c._probe_secondary_network())
        self.assertTrue(c.flag_at_deploy)
        self.assertFalse(c.probe_deployed)

    def test_a_failed_probe_delete_leaves_the_probe_marked_and_fails_the_step(self) -> None:
        c = self._certify_with({"precheck:z2-probe-delete": False})
        self.assertFalse(c._probe_secondary_network())
        self.assertTrue(c.probe_deployed)

    def test_a_failed_deploy_with_a_good_delete_clears_the_mark_and_still_fails(self) -> None:
        c = self._certify_with({"precheck:z2-reachability": False})
        self.assertFalse(c._probe_secondary_network())
        self.assertFalse(c.probe_deployed)


class SpanningTeardownTest(_CertifyModule):
    def _certify_for_teardown(self, section: dict, *, keep: bool = False, probe: bool = False,
                              deployed: bool = False) -> object:
        c = self._certify(section)
        c.ccfg["deployment_name"] = "certification"
        c.args = argparse.Namespace(keep=keep)
        c.rep = _Rep()
        c.rep.color = types.SimpleNamespace(yellow=lambda text: text)
        c.slot = types.SimpleNamespace(dir=Path("/nowhere/slot"))
        c.dry_run = False
        c.deployed = deployed
        c.probe_deployed = probe
        c.director_up = False
        c._slot_refusal = lambda _op: ""
        c._env_manages_sdn = lambda: False
        c._sdn_skip_reason = lambda: "the env has no SDN"
        c._director_env = lambda: {}
        c.calls = []

        def record(name, argv, timeout, env=None):
            c.calls.append((name, argv))
            return True

        c._cleanup_step = record
        return c

    def test_a_marked_probe_is_deleted_by_name(self) -> None:
        c = self._certify_for_teardown(_spanning_section(), probe=True)
        c.teardown()
        self.assertEqual([n for n, _ in c.calls], ["cert:delete-probe"])
        self.assertIn("certification-z2-probe", c.calls[0][1])
        self.assertIn("delete-deployment", c.calls[0][1])

    def test_an_unmarked_probe_is_left_alone(self) -> None:
        c = self._certify_for_teardown(_spanning_section(), probe=False)
        c.teardown()
        self.assertEqual(c.calls, [])

    def test_the_deployment_is_deleted_when_it_was_started(self) -> None:
        c = self._certify_for_teardown(_spanning_section(), deployed=True)
        c.teardown()
        self.assertEqual([n for n, _ in c.calls], ["cert:delete-deployment", "director:clean-up"])

    def test_keep_names_a_probe_that_is_still_there(self) -> None:
        c = self._certify_for_teardown(_spanning_section(), keep=True, probe=True)
        c.teardown()
        row = next(r for r in c.rep.rows if r[0] == "cert:delete-probe")
        self.assertEqual(row[1], "SKIP")
        self.assertIn("certification-z2-probe", row[2])
        c = self._certify_for_teardown(_spanning_section(), keep=True, probe=False)
        c.teardown()
        self.assertNotIn("cert:delete-probe", [r[0] for r in c.rep.rows])


class DeployMarksDeploymentTest(_CertifyModule):
    """A deploy that fails partway can leave VMs on both clusters, so teardown must delete it."""

    def _deploy(self, section: dict, deploy_ok: bool) -> "tuple[object, bool]":
        c = self._certify(section)
        c.ccfg["deployment_name"] = "certification"
        c.rep = _Rep()
        c.dry_run = True
        c.deployed = False
        c.checkout = Path("/nowhere")
        c.old_stemcell = (Path("/nowhere"), "stemcell", "1", "x")
        c._director_env = lambda: {}
        c.runner = _RecordingRunner()
        c._probe_secondary_network = lambda: True
        c._render_deployment_manifest = lambda **_kw: Path("/nowhere/certification.yml")
        c._bosh = lambda name, argv, timeout: ((deploy_ok if name == "cert:deploy" else True), [])
        return c, c.deploy_cert()

    def test_a_spanning_run_marks_the_deployment_even_when_the_deploy_fails(self) -> None:
        c, ok = self._deploy(_spanning_section(), deploy_ok=False)
        self.assertFalse(ok)
        self.assertTrue(c.deployed)

    def test_a_good_deploy_is_marked(self) -> None:
        c, ok = self._deploy(_spanning_section(), deploy_ok=True)
        self.assertTrue(ok)
        self.assertTrue(c.deployed)

    def test_a_one_cluster_run_keeps_its_behavior(self) -> None:
        c, ok = self._deploy({"network_bridge": "vmbr0"}, deploy_ok=False)
        self.assertFalse(ok)
        self.assertFalse(c.deployed)


class MarkerAzTest(_CertifyModule):
    def _certify_for_marker(self, rows: list) -> object:
        c = self._certify(_spanning_section())
        c.ccfg["deployment_name"] = "certification"
        c.rep = _Rep()
        c.dry_run = False
        c._capture_json = lambda argv, **_kw: {"Tables": [{"Rows": rows}]}
        return c

    ROWS = [{"instance": "simple/aaa", "index": "0", "az": "z2", "vm_cid": CID_Z2, "disk_cids": DISK_Z2},
            {"instance": "simple/bbb", "index": "1", "az": "z1", "vm_cid": CID_Z1, "disk_cids": DISK_Z1}]

    def test_the_report_names_the_az_of_the_marker_instance(self) -> None:
        c = self._certify_for_marker(self.ROWS)
        c._record_marker_az()
        name, status, detail = c.rep.rows[-1]
        self.assertEqual((name, status), ("cert:write-marker-az", "PASS"))
        self.assertIn("simple/0, in AZ z2 (pve-az2)", detail)
        self.assertIn("other AZ's disk is covered by the serial and placement checks", detail)

    def test_an_instance_table_without_an_index_says_so(self) -> None:
        c = self._certify_for_marker([{k: v for k, v in r.items() if k != "index"} for r in self.ROWS])
        c._record_marker_az()
        self.assertIn("an AZ the instance table did not name", c.rep.rows[-1][2])

    def test_a_good_write_records_the_az_in_a_spanning_run_only(self) -> None:
        nonce = "ab" * 16
        for section, expect in ((_spanning_section(), True), ({"network_bridge": "vmbr0"}, False)):
            c = self._certify_for_marker(self.ROWS)
            c.ccfg = self._config(section)
            c.ccfg["deployment_name"] = "certification"
            c._marker_ssh = lambda step, script: (True, f"MARKER nonce={nonce} sha256={'c' * 64}")
            with mock.patch.object(self.mod.secrets, "token_hex", return_value=nonce):
                self.assertTrue(c.write_marker())
            self.assertEqual("cert:write-marker-az" in [r[0] for r in c.rep.rows], expect)


class ClassifierAndExampleTest(_CertifyModule):
    def test_bare_nats_or_blobstore_mentions_are_not_routing(self) -> None:
        for line in ("CPI error: cannot store blobstore id", "nats: stale subject", "failed to download stemcell"):
            self.assertEqual(_spanning.classify_probe_failure([line]), "other", line)
        self.assertEqual(_spanning.classify_probe_failure(["Timed out pinging VM 'x'"]), "routing")

    def test_the_example_block_resolves_when_uncommented(self) -> None:
        text = (self.mod.REPO_ROOT / "ci" / "integration.yml.example").read_text()
        lines = text.splitlines()
        start = next(i for i, line in enumerate(lines) if line.startswith("  # azs:"))
        block = []
        for line in lines[start:]:
            if not line.startswith("  # "):
                break
            block.append(line[4:] if line.startswith("  #   ") is False else line[4:])
        cfg = yaml.safe_load("certification:\n  " + "\n  ".join(b.rstrip() for b in block).replace("  \n", "\n"))
        ccfg = self.mod.certification_config(cfg, "cpitest", True)
        self.assertTrue(self.mod.is_spanning(ccfg))
        self.assertEqual([e["cpi_id"] for e in ccfg["azs"]], ["pve-az1", "pve-az2"])
        self.assertEqual(ccfg["azs"][1]["network"]["cidr"], "10.255.0.0/16")
        self.assertEqual((ccfg["azs"][1]["bridge"], ccfg["azs"][1]["storage"]), ("", ""))


class VerifyPlacementPassesTest(_CertifyModule):
    def test_a_correct_span_holds_every_placement_check(self) -> None:
        c = self._certify(_spanning_section())
        c.rep = _Rep()
        c.invariants = []
        c.az_facts = {az: {k: v for k, v in e.items() if k not in ("bridge", "storage")}
                      for az, e in ENTRIES.items()}
        pre = _canned_observation(stamped=False)
        post = _canned_observation()
        # The recreate built new VMs under new VMIDs and deleted the old ones.
        new_z1, new_z2 = f"302-{STAMP_Z1}-11111111", f"5102-{STAMP_Z2}-22222222"
        post["instances"][0]["vm_cid"], post["instances"][1]["vm_cid"] = new_z1, new_z2
        post["clusters"]["z1"]["resources"] = _spanning.thin_resources(
            [{"vmid": 302, "node": "n", "name": "cpi-certification-simple-0", "tags": _tags(STAMP_Z1, "11111111")}])
        post["clusters"]["z2"]["resources"] = _spanning.thin_resources(
            [{"vmid": 5102, "node": "n", "name": "cpi-certification-simple-1", "tags": _tags(STAMP_Z2, "22222222")}])
        post["clusters"]["z1"]["configs"] = {"302": post["clusters"]["z1"]["configs"]["301"]}
        post["clusters"]["z2"]["configs"] = {"5102": post["clusters"]["z2"]["configs"]["5101"]}
        c.pre, c.post = {"placement": pre}, {"placement": post}
        self.assertTrue(c._verify_placement(), [r for r in c.rep.rows if r[1] != "PASS"])
        self.assertTrue(c.invariants[-1]["held"])
        self.assertTrue(all(r[1] == "PASS" for r in c.rep.rows), c.rep.rows)
        self.assertIn("verify:placement-pre-strays", [r[0] for r in c.rep.rows])
        self.assertIn("verify:placement-post-spans-azs", [r[0] for r in c.rep.rows])
        self.assertIn("verify:placement-old-vms-deleted", [r[0] for r in c.rep.rows])


if __name__ == "__main__":
    unittest.main(verbosity=2)
