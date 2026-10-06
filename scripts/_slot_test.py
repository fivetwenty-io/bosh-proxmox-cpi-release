#!/usr/bin/env python3
"""Unit tests for scripts/_slot.py and the slot handling in scripts/bosh.

Run with:
    python3 scripts/_slot_test.py

Uses only stdlib plus PyYAML (a _slot import). Every slot lives in a temporary
directory, and every test that loads scripts/bosh or _integration rebases it
onto a synthetic repository root (see _synthetic_repo.py). An audit hook fails
any test that opens this checkout's or the main checkout's state.json,
creds.yml, or vars.yml, and no test runs bosh against a Director.
"""

from __future__ import annotations

import contextlib
import hashlib
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
import unittest
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
import _integration  # noqa: E402
import _slot  # noqa: E402
from _synthetic_repo import SyntheticRepo, install_read_guard  # noqa: E402

install_read_guard()

PROTECTED = "3808"
DENY = {"BOSH_PROTECTED_DIRECTOR_CIDS": PROTECTED}
GOOD_SLOT_YML = (
    "internal_ip: 192.0.2.12\nbosh_alias: pve-cert\ndirector_name: cert\n"
    "pve_create_env_deployment: create-env-cert\n"
)
FACTS = _slot.EnvFacts(
    env_name="cpitest",
    default_ip="192.0.2.10",
    gateway="192.0.2.1",
    deployment="create-env",
    reserved=["192.0.2.1-192.0.2.19", "192.0.2.200-192.0.2.254"],
    artifacts_ip="192.0.2.11",
)


def _write_state(path: Path, vm_cid: str, director_id: str = "director-1", **extra) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"director_id": director_id, "current_vm_cid": vm_cid, **extra}))


def _record(director_id: str = "director-1", vm_cid: str = "", disk_id: str = "",
            disks: "list[str] | None" = None, stemcells: "list[str] | None" = None) -> dict:
    """The owner record write_owner makes for a state with these values."""
    return {"director_id": director_id, "current_vm_cid": vm_cid,
            "current_disk_id": disk_id, "disk_cids": sorted(disks or []),
            "stemcell_cids": sorted(stemcells or [])}


# A state that a create-env left behind after it deleted the old VM and
# failed to build the new one: a Director and its disk, but no VM.
def _disk_only(director_id: str = "director-1", disk_cid: str = "vm-disk-1") -> dict:
    return {"current_vm_cid": "", "director_id": director_id,
            "current_disk_id": "disk-1",
            "disks": [{"id": "disk-1", "cid": disk_cid, "size": 65536}],
            "stemcells": [{"id": "sc-1", "cid": "stemcell-1"}]}


def _plant_owner(slot: "_slot.Slot") -> None:
    """Write slot-owner.json for the slot's current state, with no checks."""
    state = _slot.read_state(slot.state)
    slot.owner.write_text(json.dumps(_slot.state_identity(state)))


def _assert_own_state_remedy(test: unittest.TestCase, message: str) -> None:
    """The message sends the operator to PVE before it empties the slot."""
    test.assertIn(_slot.OWN_STATE_REMEDY, message)
    test.assertIn("delete it there by hand and then empty the slot", message)
    test.assertIn("would leave that Director running on the slot's IP", message)
    test.assertIn('"Destructive by design" section of docs/certification/upgrade.md',
                  message)
    # Telling the operator to remove the state and leave the Director
    # running strands the slot's own Director on its IP.
    test.assertNotIn("leave that Director alone", message)
    test.assertNotIn("remove state.json and creds.yml and", message)
    test.assertNotIn("Remove state.json and creds.yml", message)


def _assert_record_time_steps(test: unittest.TestCase, message: str) -> None:
    """A refused or failed owner record says the slot's own Director may exist."""
    test.assertIn("may have created or changed a Director VM on the slot's internal_ip",
                  message)
    test.assertIn("do not remove state.json yet", message)
    _assert_own_state_remedy(test, message)
    # A rerun refuses on the missing record, so no step here says to run again.
    test.assertNotIn("again", message)
    test.assertNotIn("Fix or restore that file", message)


@contextlib.contextmanager
def _environ(**values: str):
    """os.environ with the guard's variables cleared, then set to values."""
    with mock.patch.dict(os.environ):
        for key in (_slot.REFUSE_DEFAULT_ENV, _slot.PROTECTED_CIDS_ENV, "BOSH_PVE_ENV"):
            os.environ.pop(key, None)
        os.environ.update(values)
        yield


class FakeRepo:
    """A throwaway repo root with its own manifests/bosh default slot."""

    def __init__(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name) / "repo"
        self.default_dir = self.root / "manifests" / "bosh"
        self.default_dir.mkdir(parents=True)

    def slot_dir(self, name: str = "certification") -> Path:
        d = Path(self.tmp.name) / "slots" / name
        d.mkdir(parents=True, exist_ok=True)
        return d

    def close(self) -> None:
        self.tmp.cleanup()


class ResolveTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = FakeRepo()
        self.addCleanup(self.repo.close)

    def test_unset_is_the_default_slot(self) -> None:
        slot = _slot.resolve(self.repo.root, {})
        self.assertTrue(slot.is_default)
        self.assertEqual(slot.state, self.repo.default_dir / "state.json")
        self.assertEqual(slot.creds, self.repo.default_dir / "creds.yml")

    def test_empty_is_the_default_slot(self) -> None:
        slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": "  "})
        self.assertTrue(slot.is_default)

    def test_named_directory_is_its_own_slot(self) -> None:
        d = self.repo.slot_dir()
        slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(d)})
        self.assertFalse(slot.is_default)
        self.assertEqual(slot.state, d / "state.json")
        self.assertEqual(slot.creds, d / "creds.yml")

    def test_relative_directory_resolves_against_cwd(self) -> None:
        d = self.repo.slot_dir()
        old = os.getcwd()
        os.chdir(d.parent)
        try:
            slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": d.name})
        finally:
            os.chdir(old)
        self.assertEqual(os.path.realpath(slot.dir), os.path.realpath(d))

    def test_symlink_to_the_default_slot_is_the_default_slot(self) -> None:
        link = Path(self.repo.tmp.name) / "sneaky"
        link.symlink_to(self.repo.default_dir)
        slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(link)})
        self.assertTrue(slot.is_default)

    def test_default_slot_values(self) -> None:
        slot = _slot.resolve(self.repo.root, {})
        self.assertEqual(slot.alias, "pve")
        self.assertEqual(slot.director_name, "ocfp-mgmt")
        self.assertEqual(slot.vars_layer(), [])

    def test_slot_yml_sets_alias_name_and_layer(self) -> None:
        d = self.repo.slot_dir()
        (d / "slot.yml").write_text(
            "internal_ip: 192.0.2.12\nbosh_alias: pve-cert\ndirector_name: cert\n"
        )
        slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(d)})
        self.assertEqual(slot.alias, "pve-cert")
        self.assertEqual(slot.director_name, "cert")
        self.assertEqual(slot.vars_layer(), ["-l", str(d / "slot.yml")])

    def test_alias_falls_back_to_the_directory_name(self) -> None:
        d = self.repo.slot_dir("lab state.certification")
        slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(d)})
        self.assertEqual(slot.alias, "pve-lab-state-certification")
        self.assertEqual(slot.vars_layer(), [])

    def test_slot_yml_that_is_not_a_mapping_is_an_error(self) -> None:
        d = self.repo.slot_dir()
        (d / "slot.yml").write_text("- just\n- a list\n")
        slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(d)})
        with self.assertRaises(_slot.SlotError):
            _ = slot.alias


class _GuardBase(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = FakeRepo()
        self.addCleanup(self.repo.close)
        self.main = Path(self.repo.tmp.name) / "main"
        (self.main / "manifests" / "bosh").mkdir(parents=True)

    def _slot(self, d: "Path | None") -> _slot.Slot:
        env = {} if d is None else {"BOSH_STATE_DIR": str(d)}
        return _slot.resolve(self.repo.root, env)

    def _guard(self, action: str, slot: _slot.Slot, main: "Path | None" = None,
               environ: "dict | None" = None, **kw) -> "str | None":
        return _slot.guard(action, slot, self.main if main is None else main,
                           DENY if environ is None else environ, **kw)

    def _owned(self, d: Path, vm_cid: str, director_id: str = "director-1") -> _slot.Slot:
        """A slot whose own create-env built the VM its state records.

        The record is planted directly, so a state that write_owner would
        refuse (a copy of the main Director's) can still be set up for the
        guard to refuse.
        """
        _write_state(d / "state.json", vm_cid, director_id)
        slot = self._slot(d)
        _plant_owner(slot)
        return slot


class GuardTest(_GuardBase):
    def test_refuses_the_default_slot(self) -> None:
        message = self._guard("delete-env", self._slot(None))
        self.assertIsNotNone(message)
        self.assertIn("refusing to delete-env", message)
        self.assertIn("default slot", message)
        self.assertIn("BOSH_STATE_DIR", message)

    def test_refuses_the_default_slot_named_explicitly(self) -> None:
        message = self._guard("create-env", self._slot(self.repo.default_dir))
        self.assertIsNotNone(message)
        self.assertIn("default slot", message)

    def test_refuses_the_main_checkouts_state(self) -> None:
        main_slot = self.main / "manifests" / "bosh"
        message = self._guard("delete-env", self._slot(main_slot))
        self.assertIsNotNone(message)
        self.assertIn("main checkout", message)

    def test_refuses_a_copy_of_the_main_directors_state(self) -> None:
        _write_state(self.main / "manifests" / "bosh" / "state.json", "3001")
        d = self.repo.slot_dir()
        self._owned(d, "3001")
        message = self._guard("delete-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("same Director VM", message)
        self.assertNotIn("3001", message)

    def test_refuses_a_copy_of_the_default_slots_state(self) -> None:
        _write_state(self.repo.default_dir / "state.json", "3002")
        d = self.repo.slot_dir()
        self._owned(d, "3002")
        self.assertIsNotNone(_slot.guard("delete-env", self._slot(d), None, DENY))

    def test_allows_a_dedicated_slot(self) -> None:
        _write_state(self.main / "manifests" / "bosh" / "state.json", "3001", "main")
        _write_state(self.repo.default_dir / "state.json", "3001", "main")
        d = self.repo.slot_dir()
        self._owned(d, "4001")
        self.assertIsNone(self._guard("delete-env", self._slot(d)))

    def test_allows_an_empty_dedicated_slot(self) -> None:
        self.assertIsNone(self._guard("create-env", self._slot(self.repo.slot_dir())))

    def test_allows_a_slot_whose_state_records_no_vm(self) -> None:
        d = self.repo.slot_dir()
        (d / "state.json").write_text("{}")
        self.assertIsNone(self._guard("create-env", self._slot(d)))


class OwnerRecordTest(_GuardBase):
    """A state the slot's own create-env didn't write is refused."""

    def test_state_without_an_owner_record_is_refused(self) -> None:
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001")
        for action in ("create-env", "delete-env"):
            message = self._guard(action, self._slot(d))
            self.assertIsNotNone(message, action)
            self.assertIn("was not created in this slot", message)
            self.assertIn("slot-owner.json is missing", message)
            self.assertNotIn("4001", message)

    def test_owner_record_naming_another_vm_is_refused(self) -> None:
        d = self.repo.slot_dir()
        self._owned(d, "4001")
        _write_state(d / "state.json", "4002")
        message = self._guard("delete-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("was not created in this slot", message)

    def test_owner_record_naming_another_director_is_refused(self) -> None:
        d = self.repo.slot_dir()
        self._owned(d, "4001", "director-1")
        _write_state(d / "state.json", "4001", "director-2")
        message = self._guard("delete-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("was not created in this slot", message)

    def test_unreadable_owner_record_is_refused(self) -> None:
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001")
        (d / "slot-owner.json").write_text("not json")
        message = self._guard("delete-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("was not created in this slot", message)

    def test_write_owner_records_director_and_vm(self) -> None:
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001", "director-9")
        slot = self._slot(d)
        self.assertTrue(_slot.write_owner(slot, self.main, {}))
        record = json.loads(slot.owner.read_text())
        self.assertEqual(record, _record("director-9", "4001"))
        self.assertEqual(stat.S_IMODE(slot.owner.stat().st_mode), 0o600)

    def test_write_owner_skips_an_absent_or_empty_state(self) -> None:
        d = self.repo.slot_dir()
        slot = self._slot(d)
        self.assertFalse(_slot.write_owner(slot, self.main, {}))
        for empty in ({}, {"current_vm_cid": "", "disks": [], "stemcells": []}):
            (d / "state.json").write_text(json.dumps(empty))
            self.assertFalse(_slot.write_owner(slot, self.main, {}))
        self.assertFalse(slot.owner.exists())

    def test_write_owner_records_a_state_without_a_vm(self) -> None:
        d = self.repo.slot_dir()
        (d / "state.json").write_text(json.dumps(_disk_only()))
        slot = self._slot(d)
        self.assertTrue(_slot.write_owner(slot, self.main, {}))
        self.assertEqual(json.loads(slot.owner.read_text()),
                         _record("director-1", "", "disk-1", ["vm-disk-1"], ["stemcell-1"]))
        self.assertIsNone(self._guard("create-env", slot))

    def test_a_state_without_a_vm_needs_an_owner_record(self) -> None:
        # A Director, a disk, a current disk, or a stemcell each make the state
        # non-empty, because the next create-env attaches or deletes them.
        cases = {
            "director": {"director_id": "director-1", "installation_id": "i-1"},
            "disk": {"disks": [{"id": "disk-1", "cid": "vm-disk-1"}]},
            "current disk": {"current_disk_id": "disk-1"},
            "stemcell": {"stemcells": [{"id": "sc-1", "cid": "stemcell-1"}]},
            "failed upgrade": _disk_only(),
        }
        for label, state in cases.items():
            with self.subTest(label):
                d = self.repo.slot_dir(label.replace(" ", "-"))
                (d / "state.json").write_text(json.dumps(state))
                for require in (True, False):
                    message = self._guard("create-env", self._slot(d), require_deny_list=require)
                    self.assertIsNotNone(message, label)
                    self.assertIn("was not created in this slot", message)
                    self.assertIn("slot-owner.json is missing", message)

    def test_owner_record_naming_other_disks_is_refused(self) -> None:
        d = self.repo.slot_dir()
        (d / "state.json").write_text(json.dumps(_disk_only()))
        slot = self._slot(d)
        self.assertTrue(_slot.write_owner(slot, self.main, {}))
        (d / "state.json").write_text(json.dumps(_disk_only(disk_cid="vm-disk-2")))
        message = self._guard("create-env", slot)
        self.assertIsNotNone(message)
        self.assertIn("was not created in this slot", message)

    def test_an_owner_record_from_before_disks_were_recorded_is_refused(self) -> None:
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001", disks=[{"id": "disk-1", "cid": "vm-disk-1"}])
        (d / "slot-owner.json").write_text(
            json.dumps({"director_id": "director-1", "current_vm_cid": "4001"}))
        self.assertIn("was not created in this slot", self._guard("delete-env", self._slot(d)))

    def test_a_disk_only_copy_of_the_main_state_is_refused(self) -> None:
        main_state = self.main / "manifests" / "bosh" / "state.json"
        main_state.write_text(json.dumps({**_disk_only("main-director"), "current_vm_cid": "3808"}))
        for label, copied in (("same director", _disk_only("main-director", "other-disk")),
                              ("same disk", _disk_only("other-director"))):
            with self.subTest(label):
                d = self.repo.slot_dir(label.replace(" ", "-"))
                (d / "state.json").write_text(json.dumps(copied))
                # Even an owner record planted over the copy doesn't help.
                _plant_owner(self._slot(d))
                message = self._guard("create-env", self._slot(d))
                self.assertIsNotNone(message, label)
                self.assertIn("same Director or persistent disk", message)

    def test_write_owner_never_writes_in_the_default_slot(self) -> None:
        _write_state(self.repo.default_dir / "state.json", "3001")
        self.assertFalse(_slot.write_owner(self._slot(None), self.main, {}))
        self.assertFalse((self.repo.default_dir / "slot-owner.json").exists())

    def test_unreadable_slot_state_is_refused(self) -> None:
        d = self.repo.slot_dir()
        (d / "state.json").write_text("{not json")
        message = self._guard("delete-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("is not valid JSON", message)
        self.assertIn("Restore state.json and slot-owner.json together", message)

    def _assert_recovery_steps(self, message: str) -> None:
        """The message sends the operator to PVE before it empties the slot."""
        self.assertIn("was not created in this slot", message)
        _assert_own_state_remedy(self, message)

    def test_a_mismatched_owner_record_gives_the_recovery_steps(self) -> None:
        # An old-format record from before disks were recorded, and an
        # interrupted upgrade that uploaded a stemcell, are both this slot's
        # own state and both reach the mismatch message.
        d = self.repo.slot_dir("old-format")
        _write_state(d / "state.json", "4001", disks=[{"id": "disk-1", "cid": "vm-disk-1"}])
        (d / "slot-owner.json").write_text(
            json.dumps({"director_id": "director-1", "current_vm_cid": "4001"}))
        old_format = self._guard("delete-env", self._slot(d))
        d = self.repo.slot_dir("interrupted")
        _write_state(d / "state.json", "4001")
        _plant_owner(self._slot(d))
        _write_state(d / "state.json", "4001", stemcells=[{"id": "sc-1", "cid": "stemcell-1"}])
        interrupted = self._guard("create-env", self._slot(d))
        for label, message in (("old format", old_format), ("interrupted", interrupted)):
            with self.subTest(label):
                self.assertIsNotNone(message)
                self.assertIn("records a different Director, VM, disk, or stemcell", message)
                self._assert_recovery_steps(message)

    def test_a_missing_owner_record_gives_the_same_recovery_steps(self) -> None:
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001")
        message = self._guard("delete-env", self._slot(d))
        self.assertIn("slot-owner.json is missing", message)
        self._assert_recovery_steps(message)

    def test_the_recovery_steps_are_in_the_docs_section_they_name(self) -> None:
        docs = (SCRIPTS.parent / _slot.DOCS).read_text(encoding="utf-8")
        self.assertIn("\n## Destructive by design\n", docs)
        section = docs.split("\n## Destructive by design\n", 1)[1].split("\n## ", 1)[0]
        self.assertIn("delete it there by hand", section)
        self.assertIn("then empty the slot", section)

    def _swapped_slot(self, d: Path, state: dict) -> _slot.Slot:
        """A slot that owns VM 4000, then has `state` swapped in."""
        _write_state(d / "state.json", "4000", "own-director")
        slot = self._slot(d)
        _plant_owner(slot)
        (d / "state.json").write_text(json.dumps(state))
        return slot

    def test_write_owner_refuses_a_state_that_fails_the_guard(self) -> None:
        main_state = self.main / "manifests" / "bosh" / "state.json"
        main_state.write_text(json.dumps({
            **_disk_only("main-director"), "current_vm_cid": "3001",
            "disks": [{"id": "disk-1", "cid": "main-disk"}]}))
        cases = {
            "the main VM": ({"director_id": "other", "current_vm_cid": "3001"}, {}),
            "the main Director": ({"director_id": "main-director", "current_vm_cid": "4001"}, {}),
            "the main disk": ({"director_id": "other", "current_vm_cid": "4001",
                               "disks": [{"id": "d", "cid": "main-disk"}]}, {}),
            "a listed VM": ({"director_id": "other", "current_vm_cid": PROTECTED}, DENY),
        }
        for index, (label, (state, environ)) in enumerate(cases.items()):
            with self.subTest(label):
                d = self.repo.slot_dir(f"swapped-{index}")
                slot = self._swapped_slot(d, state)
                before = slot.owner.read_text()
                with self.assertRaises(_slot.OwnerRefusedError) as cm:
                    _slot.write_owner(slot, self.main, environ)
                message = str(cm.exception)
                self.assertIn("will not record", message)
                self.assertNotIn("main-disk", message)
                _assert_record_time_steps(self, message)
                stale = "may be stale and reassigned to this slot's Director"
                if label == "a listed VM":
                    self.assertIn(stale, message)
                    self.assertIn("carries this slot's Director name and IP", message)
                else:
                    self.assertNotIn(stale, message)
                self.assertEqual(slot.owner.read_text(), before)

    def test_write_owner_refuses_a_new_state_whose_own_vm_is_listed(self) -> None:
        # The variable went stale and the CPI gave the freed VMID to this
        # slot's own Director, so the message must not call it another's.
        d = self.repo.slot_dir()
        _write_state(d / "state.json", PROTECTED, "own-director")
        slot = self._slot(d)
        with self.assertRaises(_slot.OwnerRefusedError) as cm:
            _slot.write_owner(slot, self.main, DENY)
        message = str(cm.exception)
        self.assertIn(f"a Director VM that {_slot.PROTECTED_CIDS_ENV} lists", message)
        self.assertIn("may be stale and reassigned to this slot's Director", message)
        self.assertNotIn("another Director's state", message)
        self.assertNotIn("leave that VM alone, and", message)
        _assert_record_time_steps(self, message)
        self.assertFalse(slot.owner.exists())

    def test_write_owner_names_an_unreadable_protected_state_without_its_contents(self) -> None:
        (self.main / "manifests" / "bosh" / "state.json").write_text(
            '{"director_id": "planted-secret"')
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001")
        slot = self._slot(d)
        with self.assertRaises(_slot.OwnerRefusedError) as cm:
            _slot.write_owner(slot, self.main, {})
        message = str(cm.exception)
        self.assertIn("can't be read", message)
        self.assertNotIn("planted-secret", message)
        _assert_record_time_steps(self, message)
        self.assertFalse(slot.owner.exists())

    def test_write_owner_does_not_follow_a_link_at_the_old_temporary_path(self) -> None:
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001", "own")
        target = Path(self.repo.tmp.name) / "victim.txt"
        target.write_text("untouched")
        (d / ".slot-owner.json.tmp").symlink_to(target)
        slot = self._slot(d)
        self.assertTrue(_slot.write_owner(slot, self.main, DENY))
        self.assertEqual(target.read_text(), "untouched")
        self.assertFalse(slot.owner.is_symlink())
        self.assertEqual(json.loads(slot.owner.read_text()), _record("own", "4001"))
        self.assertEqual(sorted(p.name for p in d.iterdir() if p.name.endswith(".tmp")),
                         [".slot-owner.json.tmp"])

    def test_write_owner_syncs_the_record_before_it_replaces_the_old_one(self) -> None:
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001", "own")
        slot = self._slot(d)
        calls: "list[str]" = []
        real_fsync, real_replace = os.fsync, os.replace
        with mock.patch.object(os, "fsync", lambda fd: (calls.append("fsync"), real_fsync(fd))[1]), \
                mock.patch.object(os, "replace",
                                  lambda a, b: (calls.append("replace"), real_replace(a, b))[1]):
            self.assertTrue(_slot.write_owner(slot, self.main, DENY))
        self.assertEqual(calls, ["fsync", "replace"])

    def test_write_owner_refuses_a_state_at_a_protected_path(self) -> None:
        slot = self._slot(self.main / "manifests" / "bosh")
        _write_state(slot.state, "4001")
        with self.assertRaises(_slot.OwnerRefusedError):
            _slot.write_owner(slot, self.main, {})
        self.assertFalse(slot.owner.exists())

    def test_write_owner_still_records_an_independent_state(self) -> None:
        _write_state(self.main / "manifests" / "bosh" / "state.json", "3001", "main")
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001", "own")
        slot = self._slot(d)
        self.assertTrue(_slot.write_owner(slot, self.main, DENY))
        self.assertEqual(json.loads(slot.owner.read_text()), _record("own", "4001"))

    def test_write_owner_fails_on_a_malformed_deny_list(self) -> None:
        d = self.repo.slot_dir()
        _write_state(d / "state.json", "4001")
        slot = self._slot(d)
        with self.assertRaises(_slot.SlotError):
            _slot.write_owner(slot, self.main, {"BOSH_PROTECTED_DIRECTOR_CIDS": "vm-3808"})
        self.assertFalse(slot.owner.exists())


MALFORMED_DENY_LISTS = ("vm-3808", "VM 3808", "3808;3809", "3808 ;3809", "3808,",
                        "3808,,3809", " , ", "0", "0100", "+3808", "3808.0", "3808 3809",
                        "\u0663\u0668\u0660\u0668")


class DenyListTest(_GuardBase):
    def test_parses_a_comma_separated_list(self) -> None:
        self.assertEqual(_slot.protected_cids({"BOSH_PROTECTED_DIRECTOR_CIDS": " 3808 ,\t3809 "}),
                         ["3808", "3809"])
        self.assertEqual(_slot.protected_cids({"BOSH_PROTECTED_DIRECTOR_CIDS": "3808"}), ["3808"])
        self.assertEqual(_slot.protected_cids({"BOSH_PROTECTED_DIRECTOR_CIDS": "  "}), [])
        self.assertEqual(_slot.protected_cids({}), [])

    def test_a_malformed_list_raises(self) -> None:
        for value in MALFORMED_DENY_LISTS:
            with self.subTest(value=value):
                with self.assertRaises(_slot.SlotError) as cm:
                    _slot.protected_cids({"BOSH_PROTECTED_DIRECTOR_CIDS": value})
                self.assertIn("comma-separated list of positive whole numbers", str(cm.exception))
                self.assertIn("such as 3808 or 3808,3809", str(cm.exception))

    def test_a_malformed_list_names_the_bad_entry(self) -> None:
        with self.assertRaises(_slot.SlotError) as cm:
            _slot.protected_cids({"BOSH_PROTECTED_DIRECTOR_CIDS": "3808,vm-3809"})
        self.assertIn("entry 2 is not a Proxmox VMID", str(cm.exception))
        self.assertNotIn("vm-3809", str(cm.exception))

    def test_a_malformed_list_is_refused_whether_or_not_it_is_required(self) -> None:
        d = self.repo.slot_dir()
        self._owned(d, "4001")
        for value in MALFORMED_DENY_LISTS:
            for require in (True, False):
                with self.subTest(value=value, require=require):
                    message = self._guard("delete-env", self._slot(d),
                                          environ={"BOSH_PROTECTED_DIRECTOR_CIDS": value},
                                          require_deny_list=require)
                    self.assertIsNotNone(message)
                    self.assertIn("such as 3808 or 3808,3809", message)

    def test_empty_deny_list_is_refused_when_required(self) -> None:
        d = self.repo.slot_dir()
        for environ in ({}, {"BOSH_PROTECTED_DIRECTOR_CIDS": "  "}):
            message = self._guard("create-env", self._slot(d), environ=environ)
            self.assertIsNotNone(message)
            self.assertIn("BOSH_PROTECTED_DIRECTOR_CIDS is empty", message)

    def test_empty_deny_list_is_allowed_when_not_required(self) -> None:
        d = self.repo.slot_dir()
        self.assertIsNone(self._guard("create-env", self._slot(d), environ={},
                                      require_deny_list=False))

    def test_a_protected_vm_is_refused_even_when_owned(self) -> None:
        d = self.repo.slot_dir()
        self._owned(d, PROTECTED)
        environ = {"BOSH_PROTECTED_DIRECTOR_CIDS": f"100,{PROTECTED}"}
        for require in (True, False):
            message = self._guard("delete-env", self._slot(d), environ=environ,
                                  require_deny_list=require)
            self.assertIsNotNone(message)
            self.assertIn("BOSH_PROTECTED_DIRECTOR_CIDS protects", message)
            self.assertNotIn(PROTECTED, message.replace(str(d), ""))

    def test_an_unlisted_vm_is_allowed(self) -> None:
        d = self.repo.slot_dir()
        self._owned(d, "4001")
        self.assertIsNone(self._guard("delete-env", self._slot(d)))

    def test_the_main_directors_current_vm_is_denied_too(self) -> None:
        # The variable still names the VM the main Director had before
        # create-env rebuilt it. The main state names the new one.
        _write_state(self.main / "manifests" / "bosh" / "state.json", "3901", "main")
        d = self.repo.slot_dir()
        self._owned(d, "3901")
        message = self._guard("delete-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("same Director VM", message)
        self.assertIn("BOSH_PROTECTED_DIRECTOR_CIDS doesn't list that VM", message)
        self.assertIn("new VMID", message)
        self.assertNotIn("3901", message.replace(str(d), ""))

    def test_a_listed_main_vm_gives_no_stale_list_hint(self) -> None:
        _write_state(self.main / "manifests" / "bosh" / "state.json", PROTECTED, "main")
        d = self.repo.slot_dir()
        self._owned(d, PROTECTED)
        message = self._guard("delete-env", self._slot(d))
        self.assertIn("BOSH_PROTECTED_DIRECTOR_CIDS protects", message)
        self.assertNotIn("out of date", message)

    def test_the_main_vm_is_denied_without_a_deny_list(self) -> None:
        _write_state(self.repo.default_dir / "state.json", "3901", "main")
        d = self.repo.slot_dir()
        self._owned(d, "3901")
        message = self._guard("delete-env", self._slot(d), environ={}, require_deny_list=False)
        self.assertIn("same Director VM", message)
        self.assertNotIn("out of date", message)


MAIN_CREDS = (
    "admin_password: main-admin-secret\n"
    "director_ssl:\n  ca: main-ca-pem\n  certificate: main-cert-pem\n"
    "  private_key: main-key-secret\n"
)
SLOT_CREDS = (
    "admin_password: slot-admin-secret\n"
    "director_ssl:\n  ca: slot-ca-pem\n  certificate: slot-cert-pem\n"
    "  private_key: slot-key-secret\n"
)


class CopiedCredsTest(_GuardBase):
    """A creds.yml copied from a protected slot is refused, state or no state."""

    def test_a_copied_creds_file_is_refused(self) -> None:
        for label, protected_dir in (("main", self.main / "manifests" / "bosh"),
                                     ("default", self.repo.default_dir)):
            with self.subTest(label):
                (protected_dir / "creds.yml").write_text(MAIN_CREDS)
                d = self.repo.slot_dir(label)
                (d / "creds.yml").write_text(MAIN_CREDS)
                for require in (True, False):
                    message = self._guard("run a Director command", self._slot(d),
                                          require_deny_list=require)
                    self.assertIsNotNone(message, label)
                    self.assertIn("same CA certificates", message)
                    self.assertNotIn("main-admin-secret", message)
                    self.assertNotIn("main-key-secret", message)
                (protected_dir / "creds.yml").unlink()

    def test_one_shared_certificate_is_enough(self) -> None:
        (self.repo.default_dir / "creds.yml").write_text(MAIN_CREDS)
        shared = {
            "director certificate": SLOT_CREDS.replace("slot-cert-pem", "main-cert-pem"),
            "director ca": SLOT_CREDS.replace("slot-ca-pem", "main-ca-pem"),
            "default ca": SLOT_CREDS + "default_ca:\n  certificate: main-default-ca\n",
        }
        (self.repo.default_dir / "creds.yml").write_text(
            MAIN_CREDS + "default_ca:\n  certificate: main-default-ca\n")
        for label, creds in shared.items():
            with self.subTest(label):
                d = self.repo.slot_dir(label.replace(" ", "-"))
                (d / "creds.yml").write_text(creds)
                self.assertIn("same CA certificates", self._guard("create-env", self._slot(d)))

    def test_a_shared_admin_password_alone_is_not_a_copy(self) -> None:
        # The comparison never reads the password, so only a shared public
        # certificate makes a copy.
        (self.repo.default_dir / "creds.yml").write_text(MAIN_CREDS)
        d = self.repo.slot_dir()
        (d / "creds.yml").write_text(
            SLOT_CREDS.replace("slot-admin-secret", "main-admin-secret"))
        self.assertIsNone(self._guard("create-env", self._slot(d)))

    def test_the_comparison_holds_only_certificate_digests(self) -> None:
        creds = Path(self.repo.tmp.name) / "creds.yml"
        creds.write_text(MAIN_CREDS)
        digests = _slot._creds_digests(creds)
        self.assertEqual(digests, {hashlib.sha256(v.encode()).hexdigest()
                                   for v in ("main-ca-pem", "main-cert-pem")})
        for secret in ("main-admin-secret", "main-key-secret"):
            self.assertNotIn(hashlib.sha256(secret.encode()).hexdigest(), digests)
        for keys in _slot._CREDS_FINGERPRINT:
            self.assertNotIn(keys[0], ("admin_password", "private_key"))
        self.assertIsNone(_slot._creds_digests(Path(self.repo.tmp.name) / "absent.yml"))

    def test_creds_that_are_not_utf8_are_refused(self) -> None:
        bad = b"admin_password: \xff\xfe-planted-secret\n"
        d = self.repo.slot_dir()
        (d / "creds.yml").write_bytes(bad)
        message = self._guard("create-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("is not valid UTF-8 text", message)
        self.assertIn("no way to check", message)
        self.assertNotIn("planted-secret", message)
        (d / "creds.yml").write_text(SLOT_CREDS)
        (self.main / "manifests" / "bosh" / "creds.yml").write_bytes(bad)
        message = self._guard("create-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("is not valid UTF-8 text", message)
        self.assertIn("no way to check", message)
        self.assertNotIn("planted-secret", message)
        with self.assertRaises(_slot.SlotError):
            _slot._read_creds(self.main / "manifests" / "bosh" / "creds.yml")

    def test_the_slots_own_creds_are_allowed(self) -> None:
        (self.repo.default_dir / "creds.yml").write_text(MAIN_CREDS)
        d = self.repo.slot_dir()
        (d / "creds.yml").write_text(SLOT_CREDS)
        self.assertIsNone(self._guard("create-env", self._slot(d)))

    def test_creds_with_an_invalid_value_are_refused_without_a_traceback(self) -> None:
        d = self.repo.slot_dir()
        for label, text in (("a date that doesn't exist", "admin_password: x\nwhen: 2026-13-45\n"),
                            ("nesting too deep", "k: " + "[" * 20000 + "\n")):
            with self.subTest(label):
                (d / "creds.yml").write_text(text)
                message = self._guard("create-env", self._slot(d))
                self.assertIsNotNone(message)
                self.assertIn("is not valid YAML", message)
                self.assertIn("no way to check", message)
                with self.assertRaises(_slot.SlotError) as cm:
                    _slot._read_creds(d / "creds.yml")
                self.assertIsNone(cm.exception.__cause__)

    def test_unreadable_creds_are_refused_without_quoting_them(self) -> None:
        d = self.repo.slot_dir()
        (d / "creds.yml").write_text("admin_password: [unclosed-secret\n")
        message = self._guard("create-env", self._slot(d))
        self.assertIn("is not valid YAML", message)
        self.assertNotIn("unclosed-secret", message)
        (d / "creds.yml").write_text(SLOT_CREDS)
        (self.main / "manifests" / "bosh" / "creds.yml").write_text("- a\n- list\n")
        message = self._guard("create-env", self._slot(d))
        self.assertIn("is not a YAML mapping", message)
        self.assertIn("no way to check", message)


class ExplicitPathTest(_GuardBase):
    """An extra --state or --vars-store must stay inside the slot."""

    def test_finds_both_spellings(self) -> None:
        self.assertEqual(
            _slot.path_arguments(["-n", "--state=a.json", "--vars-store", "c.yml", "--state"]),
            [("--state", "a.json"), ("--vars-store", "c.yml"), ("--state", "")])
        self.assertEqual(_slot.path_arguments(["--statefile=x", "-d", "cf"]), [])

    def _problem(self, args: "list[str]", d: Path) -> "str | None":
        return _slot.explicit_path_problem(args, self._slot(d), self.main, self.repo.root)

    def test_the_main_state_is_refused(self) -> None:
        d = self.repo.slot_dir()
        main_state = self.main / "manifests" / "bosh" / "state.json"
        for args in (["--state", str(main_state)], [f"--state={self.repo.default_dir}/state.json"],
                     ["--state=manifests/bosh/state.json"],
                     [f"--vars-store={self.main}/manifests/bosh/creds.yml"]):
            with self.subTest(args=args):
                self.assertIn("main Director's files", self._problem(args, d))

    def test_a_path_outside_the_slot_is_refused(self) -> None:
        d = self.repo.slot_dir()
        other = self.repo.slot_dir("other")
        for args in (["--state", str(other / "state.json")], [f"--vars-store={other}/creds.yml"],
                     [f"--state={d}/../other/state.json"], ["--state=state.json"]):
            with self.subTest(args=args):
                self.assertIn("outside the Director slot", self._problem(args, d))

    def test_a_link_inside_the_slot_to_the_main_state_is_refused(self) -> None:
        d = self.repo.slot_dir()
        main_state = self.main / "manifests" / "bosh" / "state.json"
        _write_state(main_state, PROTECTED, "main")
        (d / "elsewhere.json").symlink_to(main_state)
        self.assertIn("main Director's files", self._problem([f"--state={d}/elsewhere.json"], d))

    def test_a_flag_without_a_path_is_refused(self) -> None:
        self.assertIn("has no path after it", self._problem(["--state"], self.repo.slot_dir()))
        self.assertIn("has no path after it", self._problem(["--state="], self.repo.slot_dir()))

    def test_paths_inside_the_slot_are_allowed(self) -> None:
        d = self.repo.slot_dir()
        self.assertIsNone(self._problem(
            [f"--state={d}/state.json", "--vars-store", str(d / "creds.yml"), "-n"], d))
        self.assertIsNone(self._problem(["deployments"], d))


class UnreadableProtectedStateTest(_GuardBase):
    def test_unreadable_main_state_is_refused(self) -> None:
        (self.main / "manifests" / "bosh" / "state.json").write_text("{truncated")
        message = self._guard("create-env", self._slot(self.repo.slot_dir()))
        self.assertIsNotNone(message)
        self.assertIn("is not valid JSON", message)
        self.assertIn("no way to check", message)

    def test_unreadable_default_state_is_refused(self) -> None:
        (self.repo.default_dir / "state.json").write_text("[1, 2]")
        message = self._guard("create-env", self._slot(self.repo.slot_dir()))
        self.assertIsNotNone(message)
        self.assertIn("is not a JSON object", message)

    def test_a_state_that_is_not_utf8_is_refused(self) -> None:
        bad = b'{"director_id": "\xff\xfe-planted"}'
        d = self.repo.slot_dir()
        (d / "state.json").write_bytes(bad)
        message = self._guard("delete-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("is not valid UTF-8 text", message)
        self.assertNotIn("planted", message)
        with self.assertRaises(_slot.SlotError):
            _slot.read_state(d / "state.json")
        with self.assertRaises(_slot.SlotError):
            _slot.current_vm_cid(d / "state.json")

    def test_a_protected_state_that_is_not_utf8_is_refused(self) -> None:
        bad = b'{"director_id": "\xff\xfe-planted"}'
        for label, state in (("main", self.main / "manifests" / "bosh" / "state.json"),
                             ("default", self.repo.default_dir / "state.json")):
            with self.subTest(label):
                state.write_bytes(bad)
                message = self._guard("create-env", self._slot(self.repo.slot_dir(label)))
                self.assertIsNotNone(message)
                self.assertIn("is not valid UTF-8 text", message)
                self.assertIn("no way to check", message)
                state.unlink()

    def test_a_slot_yml_that_is_not_utf8_is_an_error(self) -> None:
        d = self.repo.slot_dir()
        (d / "slot.yml").write_bytes(b"internal_ip: \xff\xfe\n")
        with self.assertRaises(_slot.SlotError) as cm:
            _ = self._slot(d).alias
        self.assertIn("not valid UTF-8 text", str(cm.exception))


class SlotYmlInvalidValueTest(_GuardBase):
    def test_a_slot_yml_with_an_invalid_value_is_an_error_not_a_traceback(self) -> None:
        d = self.repo.slot_dir()
        for label, text in (("a date that doesn't exist", "internal_ip: 192.0.2.12\nwhen: 2026-13-45\n"),
                            ("nesting too deep", "k: " + "[" * 20000 + "\n")):
            with self.subTest(label):
                (d / "slot.yml").write_text(text)
                with self.assertRaises(_slot.SlotError) as cm:
                    self._slot(d).values()
                self.assertIn(str(d / "slot.yml"), str(cm.exception))
                self.assertIn("Fix the file or remove it", str(cm.exception))
                self.assertIsNone(cm.exception.__cause__)


class CheckoutDefaultSlotTest(_GuardBase):
    """Any checkout's manifests/bosh is refused, not just this one's."""

    def _checkout(self, name: str, git_file: bool) -> Path:
        root = Path(self.repo.tmp.name) / name
        bosh = root / "manifests" / "bosh"
        bosh.mkdir(parents=True)
        if git_file:
            (root / ".git").write_text("gitdir: /elsewhere\n")
        else:
            (root / ".git").mkdir()
        return bosh

    def test_another_clone_is_refused(self) -> None:
        message = self._guard("create-env", self._slot(self._checkout("clone", False)))
        self.assertIsNotNone(message)
        self.assertIn("checkout's manifests/bosh directory", message)

    def test_another_worktree_is_refused(self) -> None:
        message = self._guard("delete-env", self._slot(self._checkout("wt", True)))
        self.assertIsNotNone(message)
        self.assertIn("checkout's manifests/bosh directory", message)

    def test_a_symlink_to_another_checkout_is_refused(self) -> None:
        link = Path(self.repo.tmp.name) / "certification-link"
        link.symlink_to(self._checkout("clone", False))
        self.assertIsNotNone(self._guard("delete-env", self._slot(link)))

    def test_a_directory_with_the_tracked_files_is_refused(self) -> None:
        d = self.repo.slot_dir("copied-bosh")
        (d / "cpi.yml").write_text("[]\n")
        (d / "cloud-config.yml").write_text("{}\n")
        message = self._guard("create-env", self._slot(d))
        self.assertIsNotNone(message)
        self.assertIn("checkout's manifests/bosh directory", message)

    def test_manifests_bosh_outside_a_checkout_is_allowed(self) -> None:
        d = Path(self.repo.tmp.name) / "plain" / "manifests" / "bosh"
        d.mkdir(parents=True)
        self.assertFalse(_slot.checkout_default_slot(d))
        self.assertIsNone(self._guard("create-env", self._slot(d)))


class ConfigProblemTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = FakeRepo()
        self.addCleanup(self.repo.close)
        self.dir = self.repo.slot_dir()
        self.slot = _slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(self.dir)})

    def _problem(self, slot_yml: str, **facts) -> "str | None":
        (self.dir / "slot.yml").write_text(slot_yml)
        return _slot.config_problem(self.slot, _slot.EnvFacts(**{**FACTS.__dict__, **facts}))

    def _with(self, **values: str) -> str:
        data = {"internal_ip": "192.0.2.12", "bosh_alias": "pve-cert",
                "pve_create_env_deployment": "create-env-cert", **values}
        return "".join(f"{k}: {v}\n" for k, v in data.items() if v is not None)

    def test_default_slot_needs_no_slot_yml(self) -> None:
        self.assertIsNone(_slot.config_problem(_slot.resolve(self.repo.root, {}), _slot.EnvFacts()))

    def test_missing_slot_yml(self) -> None:
        problem = _slot.config_problem(self.slot, FACTS)
        self.assertIn("slot.yml is missing", problem)

    def test_missing_internal_ip(self) -> None:
        self.assertIn("sets no internal_ip", self._problem("bosh_alias: pve-cert\n"))

    def test_same_ip_as_the_default_slot(self) -> None:
        self.assertIn("default slot's Director IP",
                      self._problem(self._with(internal_ip="192.0.2.10")))

    def test_unreadable_gateway_fails_closed(self) -> None:
        for ip in ("192.0.2.1", "192.0.2.12"):
            with self.subTest(ip=ip):
                problem = self._problem(self._with(internal_ip=ip), gateway="")
                self.assertIsNotNone(problem)
                self.assertIn("cannot read the env's gateway", problem)

    def test_unreadable_default_ip_fails_closed(self) -> None:
        self.assertIn("cannot read the default slot's Director IP",
                      self._problem(self._with(), default_ip=""))

    def test_own_ip_is_fine(self) -> None:
        self.assertIsNone(self._problem(self._with()))

    def test_main_alias_is_refused(self) -> None:
        self.assertIn("bosh_alias to 'pve'", self._problem(self._with(bosh_alias="pve")))

    def test_omitted_alias_is_fine(self) -> None:
        self.assertIsNone(self._problem(self._with(bosh_alias=None)))

    def test_missing_deployment_is_refused(self) -> None:
        self.assertIn("sets no pve_create_env_deployment",
                      self._problem(self._with(pve_create_env_deployment=None)))

    def test_main_deployment_is_refused(self) -> None:
        self.assertIn("main Director's value",
                      self._problem(self._with(pve_create_env_deployment="create-env")))

    def test_unreadable_main_deployment_fails_closed(self) -> None:
        self.assertIn("cannot read the main Director's pve_create_env_deployment",
                      self._problem(self._with(), deployment=""))

    def test_ip_outside_the_reserved_band_is_refused(self) -> None:
        self.assertIn("outside the env's reserved band",
                      self._problem(self._with(internal_ip="192.0.2.150")))

    def test_ip_outside_the_slots_own_reserved_band_is_refused(self) -> None:
        yml = self._with() + "cpitest_reserved:\n- 192.0.2.1-192.0.2.11\n"
        self.assertIn("outside the env's reserved band", self._problem(yml))

    def test_missing_reserved_band_fails_closed(self) -> None:
        self.assertIn("cannot read cpitest_reserved", self._problem(self._with(), reserved=None))

    def test_gateway_is_refused(self) -> None:
        self.assertIn("internal_gw", self._problem(self._with(internal_ip="192.0.2.1")))

    def test_artifacts_vm_is_refused(self) -> None:
        self.assertIn("artifacts VM", self._problem(self._with(internal_ip="192.0.2.11")))

    def test_not_an_address_is_refused(self) -> None:
        self.assertIn("not an IP address", self._problem(self._with(internal_ip="director")))


class EnvFactsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = FakeRepo()
        self.addCleanup(self.repo.close)
        self.env_dir = self.repo.root / "manifests" / "envs" / "pve-cpi"
        self.env_dir.mkdir(parents=True)

    def test_env_layer_wins_over_base_vars(self) -> None:
        (self.repo.default_dir / "vars.yml").write_text(
            "internal_ip: 10.0.0.10\ninternal_gw: 10.0.0.1\npve_create_env_deployment: create-env\n")
        (self.env_dir / "vars.yml").write_text(
            "internal_ip: 10.1.0.10\npve_cpi_reserved:\n- 10.1.0.1-10.1.0.19\n")
        (self.env_dir / "artifacts.yml").write_text("artifacts_vm_ip: 10.1.0.11\n")
        facts = _slot.env_facts(self.repo.root, "pve-cpi")
        self.assertEqual(facts.default_ip, "10.1.0.10")
        self.assertEqual(facts.gateway, "10.0.0.1")
        self.assertEqual(facts.deployment, "create-env")
        self.assertEqual(facts.reserved, ["10.1.0.1-10.1.0.19"])
        self.assertEqual(facts.reserved_key, "pve_cpi_reserved")
        self.assertEqual(facts.artifacts_ip, "10.1.0.11")

    def test_missing_vars_leave_values_empty(self) -> None:
        facts = _slot.env_facts(self.repo.root, "pve-cpi")
        self.assertEqual((facts.default_ip, facts.deployment, facts.reserved), ("", "", None))

    def test_checkout_without_vars_reads_the_example(self) -> None:
        (self.repo.default_dir / "vars.yml.example").write_text("pve_create_env_deployment: create-env\n")
        self.assertEqual(_slot.env_facts(self.repo.root, "pve-cpi").deployment, "create-env")

    def test_vars_win_over_the_example(self) -> None:
        (self.repo.default_dir / "vars.yml.example").write_text("pve_create_env_deployment: create-env\n")
        (self.repo.default_dir / "vars.yml").write_text("pve_create_env_deployment: create-env-main\n")
        self.assertEqual(_slot.env_facts(self.repo.root, "pve-cpi").deployment, "create-env-main")


def _git(cwd: Path, *argv: str) -> None:
    env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
           "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"}
    subprocess.run(["git", "-C", str(cwd), *argv], check=True, env=env, capture_output=True)


@unittest.skipUnless(shutil.which("git"), "git is not on PATH")
class MainCheckoutRootTest(unittest.TestCase):
    def test_a_linked_worktree_names_its_main_checkout(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            main = Path(tmp) / "main"
            main.mkdir()
            _git(main, "init", "-q")
            _git(main, "commit", "-q", "--allow-empty", "-m", "init")
            _git(main, "worktree", "add", "-q", str(Path(tmp) / "wt"))
            found = _slot.main_checkout_root(Path(tmp) / "wt")
            self.assertEqual(os.path.realpath(found), os.path.realpath(main))
            self.assertEqual(os.path.realpath(_slot.main_checkout_root(main)),
                             os.path.realpath(main))

    def test_a_separate_git_dir_names_the_checkout(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            main = Path(tmp) / "main"
            _git(Path(tmp), "init", "-q", f"--separate-git-dir={Path(tmp) / 'store'}", str(main))
            found = _slot.main_checkout_root(main)
            self.assertIsNotNone(found)
            self.assertEqual(os.path.realpath(found), os.path.realpath(main))

    def test_outside_git_is_none(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            self.assertIsNone(_slot.main_checkout_root(Path(tmp)))

    def test_a_broken_git_link_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / ".git").write_text(f"gitdir: {Path(tmp) / 'missing'}\n")
            with self.assertRaises(_slot.SlotError):
                _slot.main_checkout_root(Path(tmp))
            repo = Path(tmp)
            slot_dir = repo / "slots" / "certification"
            slot_dir.mkdir(parents=True)
            slot = _slot.resolve(repo, {"BOSH_STATE_DIR": str(slot_dir)})
            message = _slot.guard_repo("create-env", slot, repo, DENY)
            self.assertIsNotNone(message)
            self.assertIn("can't be ruled out", message)


def _load_bosh_script(environ: dict, repo: "SyntheticRepo | None" = None) -> object:
    """Load scripts/bosh as a module with the given slot env vars.

    With repo, every path the module derives from its own location is
    rebased onto that synthetic root, so the guard reads no real state.
    """
    path = str(SCRIPTS / "bosh")
    with mock.patch.dict(os.environ, environ):
        for key in ("BOSH_STATE_DIR",):
            if key not in environ:
                os.environ.pop(key, None)
        loader = _ilm.SourceFileLoader("bosh_script_under_test", path)
        spec = _ilu.spec_from_file_location("bosh_script_under_test", path, loader=loader)
        module = _ilu.module_from_spec(spec)
        loader.exec_module(module)
    if repo is not None:
        repo.rebase_module(module)
    return module


def _never(*_a, **_kw):
    raise AssertionError("a refused command reached the bosh CLI")


class _SyntheticRoot(unittest.TestCase):
    """Runs each test against a synthetic repository root.

    The shared _integration module is rebased onto it for the test, so
    neither its guard nor its env facts read this checkout's files, and the
    root has no .git, so the guard finds no main checkout through it.
    """

    def setUp(self) -> None:
        self.repo = SyntheticRepo()
        self.addCleanup(self.repo.close)
        rebase = self.repo.rebase_shared(_integration)
        rebase.__enter__()
        self.addCleanup(rebase.__exit__, None, None, None)


class BoshScriptSlotTest(_SyntheticRoot):
    """scripts/bosh honors BOSH_STATE_DIR and refuses the default slot."""

    def setUp(self) -> None:
        super().setUp()
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.slot_dir = Path(self.tmp.name) / "certification"
        self.slot_dir.mkdir()
        (self.slot_dir / "slot.yml").write_text(GOOD_SLOT_YML)

    def _load(self, environ: dict) -> object:
        """scripts/bosh on the synthetic root, with the env facts the tests use."""
        mod = _load_bosh_script(environ, self.repo)
        mod.slot_env_facts = lambda: FACTS
        return mod

    def _refusing(self, environ: dict) -> object:
        mod = self._load({**environ, "BOSH_REFUSE_DEFAULT_SLOT": "1"})
        mod.run = _never
        mod._delete_all_deployments = _never
        mod.cpi_release_path = _never
        mod.bosh_int = _never
        mod.slot_env_facts = lambda: FACTS
        return mod

    def _slot_module(self, run) -> object:
        """scripts/bosh in the test slot, with create-env's inputs stubbed out."""
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        mod.slot_env_facts = lambda: FACTS
        mod.cpi_release_path = lambda: Path(self.tmp.name) / "release.tgz"
        mod._cpi_release_name = lambda _p: "bosh-proxmox-cpi"
        mod._light_stemcell_create_env_vars = lambda: []
        mod.bosh_deployment_dir = lambda: Path(self.tmp.name)
        mod.compiled_ops_layer = lambda: []
        mod.run = run
        return mod

    def test_teardown_refuses_the_default_slot(self) -> None:
        mod = self._refusing({})
        with _environ(BOSH_REFUSE_DEFAULT_SLOT="1", **DENY):
            with self.assertRaises(SystemExit) as cm:
                mod.cmd_teardown([])
        self.assertEqual(cm.exception.code, 2)

    def test_create_env_refuses_the_default_slot(self) -> None:
        mod = self._refusing({})
        with _environ(BOSH_REFUSE_DEFAULT_SLOT="1", **DENY):
            with self.assertRaises(SystemExit) as cm:
                mod.cmd_create_env([])
        self.assertEqual(cm.exception.code, 2)

    def test_alias_env_and_check_vars_refuse_the_default_slot(self) -> None:
        mod = self._refusing({})
        for command in (mod.cmd_alias_env, mod.cmd_check_vars):
            with _environ(BOSH_REFUSE_DEFAULT_SLOT="1", **DENY):
                with self.assertRaises(SystemExit) as cm:
                    command([])
            self.assertEqual(cm.exception.code, 2, command.__name__)

    def test_refuse_mode_needs_a_deny_list(self) -> None:
        mod = self._refusing({"BOSH_STATE_DIR": str(self.slot_dir)})
        with _environ(BOSH_REFUSE_DEFAULT_SLOT="1"):
            with self.assertRaises(SystemExit) as cm:
                mod.cmd_create_env([])
        self.assertEqual(cm.exception.code, 2)

    def test_slot_paths_alias_and_name(self) -> None:
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        self.assertEqual(mod.STATE, self.slot_dir / "state.json")
        self.assertEqual(mod.CREDS, self.slot_dir / "creds.yml")
        self.assertEqual(mod.BOSH_ALIAS, "pve-cert")
        self.assertEqual(mod.internal_ip(), "192.0.2.12")

    def test_default_slot_is_unchanged(self) -> None:
        mod = self._load({})
        self.assertEqual(mod.STATE, mod.REPO_ROOT / "manifests" / "bosh" / "state.json")
        self.assertEqual(mod.CREDS, mod.REPO_ROOT / "manifests" / "bosh" / "creds.yml")
        self.assertEqual(mod.BOSH_ALIAS, "pve")

    def _director_name_argv(self, mod: object) -> list:
        """The create-env argv the module would run, with its inputs stubbed."""
        captured: list = []
        mod.cpi_release_path = lambda: Path(self.tmp.name) / "release.tgz"
        mod._cpi_release_name = lambda _p: "bosh-proxmox-cpi"
        mod._light_stemcell_create_env_vars = lambda: []
        mod.bosh_deployment_dir = lambda: Path(self.tmp.name)
        mod.compiled_ops_layer = lambda: []
        mod.run = lambda *argv, **_kw: captured.append(argv) or 0
        with _environ():
            mod.cmd_create_env([])
        return list(captured[0])

    def test_director_name_falls_back_when_no_vars_file_sets_it(self) -> None:
        mod = self._load({})
        with _environ():
            self.assertEqual(mod.director_name_vars(), ["-v", "director_name=ocfp-mgmt"])
        slot_mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        (self.slot_dir / "slot.yml").write_text(
            "internal_ip: 192.0.2.12\npve_create_env_deployment: create-env-cert\n")
        with _environ():
            self.assertEqual(slot_mod.director_name_vars(), ["-v", "director_name=ocfp-mgmt"])

    def test_director_name_comes_from_the_base_vars_file(self) -> None:
        (self.repo.default_dir / "vars.yml").write_text(
            "internal_gw: 172.31.0.1\ndirector_name: lab-director\n")
        mod = self._load({})
        with _environ():
            self.assertEqual(mod.director_name_vars(), [])
            argv = self._director_name_argv(mod)
        self.assertFalse(any(str(a).startswith("director_name=") for a in argv))
        self.assertEqual(argv.count(str(mod.VARS)), 1)

    def test_director_name_comes_from_the_env_vars_file(self) -> None:
        env_vars = self.repo.root / "manifests" / "envs" / "cpitest" / "vars.yml"
        env_vars.write_text("director_name: env-director\n")
        mod = self._load({})
        with _environ():
            self.assertEqual(mod.director_name_vars(), [])

    def test_director_name_comes_from_slot_yml(self) -> None:
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        with _environ():
            self.assertEqual(mod.director_name_vars(), [])

    def test_the_fallback_name_is_the_only_director_name_flag(self) -> None:
        mod = self._load({})
        argv = self._director_name_argv(mod)
        pairs = [(argv[i], argv[i + 1]) for i in range(len(argv) - 1) if argv[i] == "-v"]
        self.assertEqual([v for _f, v in pairs if v.startswith("director_name=")],
                         ["director_name=ocfp-mgmt"])

    def test_layered_var_takes_the_last_file_that_sets_it(self) -> None:
        mod = self._load({})
        base = Path(self.tmp.name) / "base.yml"
        env = Path(self.tmp.name) / "env.yml"
        top = Path(self.tmp.name) / "top.yml"
        gone = Path(self.tmp.name) / "gone.yml"
        base.write_text("director_name: base\n")
        env.write_text("director_name: env\n")
        top.write_text("other: 1\n")
        self.assertEqual(mod.layered_var("director_name", [gone, base]), "base")
        self.assertEqual(mod.layered_var("director_name", [base, env]), "env")
        self.assertEqual(mod.layered_var("director_name", [base, env, top]), "env")
        self.assertIsNone(mod.layered_var("director_name", [gone, top]))
        self.assertIsNone(mod.layered_var("absent", [base, env]))

    def test_layered_var_ignores_a_file_that_is_not_a_mapping(self) -> None:
        mod = self._load({})
        listy = Path(self.tmp.name) / "list.yml"
        listy.write_text("- director_name\n")
        self.assertIsNone(mod.layered_var("director_name", [listy]))

    def test_layered_var_stops_on_a_file_it_cannot_parse(self) -> None:
        mod = self._load({})
        broken = Path(self.tmp.name) / "broken.yml"
        broken.write_text("director_name: [unclosed\n")
        with self.assertRaises(SystemExit) as cm:
            mod.layered_var("director_name", [broken])
        self.assertIn("Fix the file", str(cm.exception))
        self.assertRegex(str(cm.exception), re.escape(f"{broken} at line ") + "[0-9]+")

    def test_layered_var_does_not_quote_the_line_it_cannot_parse(self) -> None:
        mod = self._load({})
        broken = Path(self.tmp.name) / "broken.yml"
        broken.write_text("other: fine\nadmin_password: [hunter2-planted-secret\n")
        with self.assertRaises(SystemExit) as cm:
            mod.layered_var("director_name", [broken])
        message = str(cm.exception)
        self.assertNotIn("hunter2-planted-secret", message)
        self.assertNotIn("admin_password", message)
        self.assertIn(str(broken), message)
        self.assertRegex(message, r"at line [0-9]+")

    def test_layered_var_stops_on_a_value_yaml_cannot_load(self) -> None:
        mod = self._load({})
        broken = Path(self.tmp.name) / "bad-date.yml"
        broken.write_text("rotated: 2026-13-45\n")
        with self.assertRaises(SystemExit) as cm:
            mod.layered_var("director_name", [broken])
        message = str(cm.exception)
        self.assertIn(str(broken), message)
        self.assertIn("Fix the file", message)
        self.assertNotIn("2026-13-45", message)

    def _layer_files(self, base: str = "", env: str = "") -> Path:
        """Write the base and env vars files, and return the env file's path."""
        (self.repo.default_dir / "vars.yml").write_text(base)
        env_vars = self.repo.root / "manifests" / "envs" / "cpitest" / "vars.yml"
        env_vars.write_text(env)
        return env_vars

    def test_layered_var_stops_on_a_file_that_is_not_utf8(self) -> None:
        mod = self._load({})
        latin = Path(self.tmp.name) / "latin.yml"
        latin.write_bytes(b"\xff\xfed\x00i\x00r\x00")
        with self.assertRaises(SystemExit) as cm:
            mod.layered_var("director_name", [latin])
        self.assertIn(str(latin), str(cm.exception))
        self.assertIn("Fix the file", str(cm.exception))

    def test_layered_var_stops_on_a_blank_or_null_value(self) -> None:
        mod = self._load({})
        base = Path(self.tmp.name) / "base.yml"
        top = Path(self.tmp.name) / "top.yml"
        base.write_text("director_name: base\n")
        for text in ("director_name:\n", "director_name: null\n", "director_name: ''\n",
                     "director_name: '  '\n"):
            with self.subTest(text=text):
                top.write_text(text + "other: 1\n")
                for files in ([base, top], [top]):
                    with self.assertRaises(SystemExit) as cm:
                        mod.layered_var("director_name", files)
                    self.assertIn(str(top), str(cm.exception))
                    self.assertIn("Fix the file", str(cm.exception))

    def test_a_blank_name_in_a_higher_layer_stops_over_a_set_lower_layer(self) -> None:
        for blank in ("director_name:\n", "director_name: ''\n", "director_name: null\n"):
            with self.subTest(blank=blank):
                env_vars = self._layer_files("director_name: base\n", blank)
                mod = self._load({})
                with _environ(), self.assertRaises(SystemExit) as cm:
                    mod.director_name_vars()
                self.assertIn(str(env_vars), str(cm.exception))
                self.assertIn("Fix the file", str(cm.exception))
        self._layer_files("director_name: base\n", "director_name: env\n")
        slot_yml = self.slot_dir / "slot.yml"
        slot_yml.write_text(GOOD_SLOT_YML.replace("director_name: cert\n", "director_name:\n"))
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        with _environ(), self.assertRaises(SystemExit) as cm:
            mod.director_name_vars()
        self.assertIn(str(slot_yml), str(cm.exception))

    def test_vars_layer_files_go_base_then_env_then_slot(self) -> None:
        env_vars = self._layer_files()
        mod = self._load({})
        with _environ():
            self.assertEqual(mod.vars_layer_files(), [mod.VARS, env_vars])
        slot_mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        with _environ():
            self.assertEqual(slot_mod.vars_layer_files(),
                             [slot_mod.VARS, env_vars, self.slot_dir / "slot.yml"])

    def test_a_higher_layer_name_wins_over_a_lower_one(self) -> None:
        env_vars = self._layer_files("director_name: base\n", "director_name: env\n")
        slot_yml = self.slot_dir / "slot.yml"
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        with _environ():
            self.assertEqual(mod.layered_var("director_name", mod.vars_layer_files()), "cert")
            argv = self._director_name_argv(mod)
        ls = [argv[i + 1] for i in range(len(argv) - 1) if argv[i] == "-l"]
        self.assertEqual(ls, [str(mod.VARS), str(env_vars), str(slot_yml)])
        self.assertFalse(any(str(a).startswith("director_name=") for a in argv))
        # With no name in slot.yml the env's name wins over the base file's.
        slot_yml.write_text(GOOD_SLOT_YML.replace("director_name: cert\n", ""))
        with _environ():
            self.assertEqual(mod.layered_var("director_name", mod.vars_layer_files()), "env")
        env_vars.write_text("other: 1\n")
        with _environ():
            self.assertEqual(mod.layered_var("director_name", mod.vars_layer_files()), "base")

    def test_teardown_stops_on_an_unreadable_vars_file_before_deleting_anything(self) -> None:
        _write_state(self.slot_dir / "state.json", "4001", "director-7")
        (self.repo.default_dir / "vars.yml").write_bytes(b"\xff\xfedirector_name")
        deleted: list = []
        mod = self._slot_module(_never)
        self.assertTrue(_slot.write_owner(mod.SLOT, None, {}))
        mod._delete_all_deployments = lambda: deleted.append(True)
        with _environ(), self.assertRaises(SystemExit) as cm:
            mod.cmd_teardown([])
        self.assertIn("Fix the file", str(cm.exception))
        self.assertEqual(deleted, [])

    def test_teardown_of_an_empty_dedicated_slot_is_a_no_op(self) -> None:
        mod = self._refusing({"BOSH_STATE_DIR": str(self.slot_dir)})
        with _environ(BOSH_REFUSE_DEFAULT_SLOT="1", BOSH_STATE_DIR=str(self.slot_dir), **DENY):
            self.assertEqual(mod.cmd_teardown([]), 0)

    def test_a_slot_on_the_default_ip_is_refused(self) -> None:
        mod = self._refusing({"BOSH_STATE_DIR": str(self.slot_dir)})
        mod.slot_env_facts = lambda: _slot.EnvFacts(**{**FACTS.__dict__, "default_ip": "192.0.2.12"})
        with _environ():
            with self.assertRaises(SystemExit) as cm:
                mod.cmd_create_env([])
        self.assertEqual(cm.exception.code, 2)

    def test_net_down_refuses_a_non_default_slot(self) -> None:
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        mod._sdn_params = _never
        self.assertEqual(mod.cmd_net_down([]), 2)

    def test_director_env_uses_the_slot_alias(self) -> None:
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        (self.slot_dir / "creds.yml").write_text("{}\n")
        mod.bosh_int = lambda path, json_path: f"{Path(path).name}:{json_path}"
        with _environ():
            env = mod.director_env()
        self.assertEqual(env["BOSH_ENVIRONMENT"], "pve-cert")
        self.assertEqual(env["BOSH_CLIENT_SECRET"], "creds.yml:/admin_password")

    def test_create_env_records_the_slot_owner(self) -> None:
        def run(*argv, **_kw):
            self.assertEqual(argv[:2], ("bosh", "create-env"))
            _write_state(self.slot_dir / "state.json", "4001", "director-7")
            return 0
        mod = self._slot_module(run)
        with _environ():
            self.assertEqual(mod.cmd_create_env([]), 0)
        record = json.loads((self.slot_dir / "slot-owner.json").read_text())
        self.assertEqual(record, _record("director-7", "4001"))
        # The upgrade leg's second create-env replaces the VM and keeps going.
        def upgrade(*_argv, **_kw):
            _write_state(self.slot_dir / "state.json", "4002", "director-7")
            return 0
        mod.run = upgrade
        with _environ(BOSH_REFUSE_DEFAULT_SLOT="1", **DENY):
            self.assertEqual(mod.cmd_create_env([]), 0)
            mod.run = _never
            mod.bosh_int = lambda *_a: "x"
            mod._delete_all_deployments = lambda: None
            self.assertIsNone(mod.slot_refusal("delete-env"))
        record = json.loads((self.slot_dir / "slot-owner.json").read_text())
        self.assertEqual(record["current_vm_cid"], "4002")

    def test_a_failed_create_env_still_records_the_vm_it_built(self) -> None:
        def run(*_argv, **_kw):
            _write_state(self.slot_dir / "state.json", "4001")
            return 1
        mod = self._slot_module(run)
        with _environ():
            self.assertEqual(mod.cmd_create_env([]), 1)
        self.assertTrue((self.slot_dir / "slot-owner.json").exists())

    def test_create_env_fails_when_the_owner_record_cannot_be_written(self) -> None:
        def run(*_argv, **_kw):
            _write_state(self.slot_dir / "state.json", "4001")
            return 0
        mod = self._slot_module(run)
        with _environ(), mock.patch.object(_slot, "write_owner",
                                           side_effect=_slot.SlotError("cannot write it")):
            rc, err = self._stderr(lambda: mod.cmd_create_env([]))
        self.assertEqual(rc, 1)
        self.assertIn("cannot write it", err)
        _assert_record_time_steps(self, err)

    def _stderr(self, call) -> "tuple[object, str]":
        """call() with stderr captured; returns (result, what it printed)."""
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            result = call()
        return result, err.getvalue()

    def test_create_env_does_not_record_a_state_swapped_in_mid_run(self) -> None:
        # While create-env runs, another process puts a state naming a listed
        # Director VM in the slot. The owner record must not bless it.
        def run(*_argv, **_kw):
            _write_state(self.slot_dir / "state.json", PROTECTED, "other-director")
            return 0
        mod = self._slot_module(run)
        with _environ(**DENY):
            rc, err = self._stderr(lambda: mod.cmd_create_env([]))
        self.assertEqual(rc, 1)
        self.assertIn("will not record", err)
        _assert_record_time_steps(self, err)
        self.assertFalse((self.slot_dir / "slot-owner.json").exists())
        # The next command refuses the state, so nothing can act on it.
        with _environ(**DENY):
            self.assertIsNotNone(mod.slot_refusal("delete-env"))

    def test_teardown_does_not_record_a_state_swapped_in_mid_run(self) -> None:
        _write_state(self.slot_dir / "state.json", "4001", "director-7")

        def delete_env(*_argv, **_kw):
            _write_state(self.slot_dir / "state.json", PROTECTED, "other-director")
            return 0
        mod = self._slot_module(delete_env)
        self.assertTrue(_slot.write_owner(mod.SLOT, None, {}))
        before = (self.slot_dir / "slot-owner.json").read_text()
        mod._delete_all_deployments = lambda: None
        with _environ(**DENY):
            rc, err = self._stderr(lambda: mod.cmd_teardown([]))
        self.assertEqual(rc, 1)
        self.assertIn("will not record", err)
        _assert_record_time_steps(self, err)
        self.assertEqual((self.slot_dir / "slot-owner.json").read_text(), before)

    def test_a_state_that_is_not_utf8_is_refused_without_a_traceback(self) -> None:
        (self.slot_dir / "state.json").write_bytes(b'{"director_id": "\xff\xfe-planted"}')
        mod = self._slot_module(_never)
        mod._delete_all_deployments = _never
        err = io.StringIO()
        with _environ(), contextlib.redirect_stderr(err):
            with self.assertRaises(SystemExit) as cm:
                mod.cmd_teardown([])
        self.assertEqual(cm.exception.code, 2)
        self.assertIn("is not valid UTF-8 text", err.getvalue())
        self.assertNotIn("planted", err.getvalue())

    def test_creds_that_are_not_utf8_are_refused_without_a_traceback(self) -> None:
        (self.slot_dir / "creds.yml").write_bytes(b"admin_password: \xff\xfe-planted\n")
        mod = self._slot_module(_never)
        err = io.StringIO()
        with _environ(), contextlib.redirect_stderr(err):
            with self.assertRaises(SystemExit) as cm:
                mod.cmd_create_env([])
        self.assertEqual(cm.exception.code, 2)
        self.assertIn("is not valid UTF-8 text", err.getvalue())
        self.assertNotIn("planted", err.getvalue())

    def test_a_copied_state_is_refused_without_refuse_mode(self) -> None:
        _write_state(self.slot_dir / "state.json", "4001")
        mod = self._slot_module(_never)
        mod._delete_all_deployments = _never
        for command in (mod.cmd_create_env, mod.cmd_teardown, mod.cmd_alias_env):
            with _environ():
                with self.assertRaises(SystemExit) as cm:
                    command([])
            self.assertEqual(cm.exception.code, 2, command.__name__)
        self.assertFalse((self.slot_dir / "slot-owner.json").exists())

    def test_director_env_refuses_the_default_slot_in_refuse_mode(self) -> None:
        mod = self._refusing({})
        for make_env in (mod.director_env, mod.credhub_env):
            with _environ(BOSH_REFUSE_DEFAULT_SLOT="1", **DENY):
                with self.assertRaises(SystemExit) as cm:
                    make_env()
            self.assertEqual(cm.exception.code, 2, make_env.__name__)

    def test_passthrough_refuses_the_default_slot_in_refuse_mode(self) -> None:
        mod = self._refusing({})
        with _environ(BOSH_REFUSE_DEFAULT_SLOT="1", **DENY):
            with self.assertRaises(SystemExit) as cm:
                mod.main(["deployments"])
        self.assertEqual(cm.exception.code, 2)

    def test_director_env_refuses_a_copied_state(self) -> None:
        _write_state(self.slot_dir / "state.json", "4001")
        (self.slot_dir / "creds.yml").write_text("{}\n")
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        mod.bosh_int = _never
        with _environ():
            with self.assertRaises(SystemExit) as cm:
                mod.director_env()
        self.assertEqual(cm.exception.code, 2)

    def test_teardown_records_what_delete_env_left(self) -> None:
        _write_state(self.slot_dir / "state.json", "4001", "director-7", current_disk_id="disk-1",
                     disks=[{"id": "disk-1", "cid": "vm-disk-1"}],
                     stemcells=[{"id": "sc-1", "cid": "stemcell-1"}])

        def delete_env(*argv, **_kw):
            self.assertEqual(argv[:2], ("bosh", "delete-env"))
            # It deleted the VM and then failed, which leaves the disk.
            (self.slot_dir / "state.json").write_text(json.dumps(_disk_only("director-7")))
            return 1
        mod = self._slot_module(delete_env)
        self.assertTrue(_slot.write_owner(mod.SLOT, None, {}))
        mod._delete_all_deployments = lambda: None
        with _environ():
            self.assertEqual(mod.cmd_teardown([]), 1)
            self.assertIsNone(mod.slot_refusal("delete-env"))
        record = json.loads((self.slot_dir / "slot-owner.json").read_text())
        self.assertEqual(record, _record("director-7", "", "disk-1", ["vm-disk-1"], ["stemcell-1"]))

    def test_passthrough_runs_the_slot_yml_checks(self) -> None:
        (self.slot_dir / "slot.yml").write_text(
            GOOD_SLOT_YML.replace("bosh_alias: pve-cert", "bosh_alias: pve"))
        (self.slot_dir / "creds.yml").write_text(SLOT_CREDS)
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        mod.run = _never
        mod.bosh_int = lambda *_a: "x"
        for argv in (["deployments"], ["-d", "cf", "delete-deployment"], ["login"],
                     ["credhub", "find"]):
            with self.subTest(argv=argv), _environ():
                with self.assertRaises(SystemExit) as cm:
                    mod.main(argv)
                self.assertEqual(cm.exception.code, 2)

    def test_passthrough_refuses_a_copied_creds_file(self) -> None:
        (self.repo.default_dir / "creds.yml").write_text(MAIN_CREDS)
        (self.slot_dir / "creds.yml").write_text(MAIN_CREDS)
        mod = self._load({"BOSH_STATE_DIR": str(self.slot_dir)})
        mod.run = _never
        mod.bosh_int = _never
        for argv in (["deployments"], ["-d", "cf", "delete-deployment"]):
            with self.subTest(argv=argv), _environ():
                with self.assertRaises(SystemExit) as cm:
                    mod.main(argv)
                self.assertEqual(cm.exception.code, 2)

    def test_an_extra_state_or_vars_store_outside_the_slot_is_refused(self) -> None:
        main_state = self.repo.default_dir / "state.json"
        _write_state(main_state, PROTECTED, "main")
        _write_state(self.slot_dir / "state.json", "4001")
        mod = self._slot_module(_never)
        self.assertTrue(_slot.write_owner(mod.SLOT, None, {}))
        mod._delete_all_deployments = _never
        mod.bosh_int = _never
        outside = Path(self.tmp.name) / "elsewhere.json"
        for argv in (["teardown", f"--state={main_state}"],
                     ["delete-env", "--state", str(main_state)],
                     ["create-env", f"--vars-store={self.repo.default_dir}/creds.yml"],
                     ["create-env", "--state=manifests/bosh/state.json"],
                     ["-n", "delete-env", f"--state={outside}"],
                     ["int", "manifest.yml", f"--vars-store={outside}"],
                     ["teardown", "--state"]):
            for refuse in ({}, {"BOSH_REFUSE_DEFAULT_SLOT": "1", **DENY}):
                with self.subTest(argv=argv, refuse=bool(refuse)), _environ(**refuse):
                    with self.assertRaises(SystemExit) as cm:
                        mod.main(argv)
                    self.assertEqual(cm.exception.code, 2)
        for command in (mod.cmd_teardown, mod.cmd_create_env):
            with _environ(), self.assertRaises(SystemExit) as cm:
                command([f"--state={main_state}"])
            self.assertEqual(cm.exception.code, 2, command.__name__)

    def test_an_extra_state_inside_the_slot_is_passed_on(self) -> None:
        captured: list = []
        mod = self._slot_module(lambda *argv, **_kw: captured.append(argv) or 0)
        flag = f"--state={self.slot_dir}/state.json"
        with _environ():
            self.assertEqual(mod.main(["create-env", flag]), 0)
        self.assertEqual(captured[0][-1], flag)

    def test_the_default_slot_passes_extra_flags_on_unchanged(self) -> None:
        captured: list = []
        mod = self._load({})
        mod.run = lambda *argv, **_kw: captured.append(argv) or 0
        mod.director_env = lambda: {}
        with _environ():
            self.assertEqual(mod.main(["-n", "delete-env", "--state=/elsewhere/state.json"]), 0)
        self.assertEqual(captured[0][-1], "--state=/elsewhere/state.json")

    def test_default_slot_without_refuse_mode_is_unchanged(self) -> None:
        mod = self._load({})
        mod.bosh_int = lambda path, json_path: json_path
        creds = mod.CREDS
        with _environ(), mock.patch.object(type(creds), "exists", return_value=True):
            env = mod.director_env()
        self.assertEqual(env["BOSH_ENVIRONMENT"], "pve")


@unittest.skipUnless(shutil.which("git"), "git is not on PATH")
class LinkedWorktreeLookupTest(unittest.TestCase):
    """scripts/bosh finds the main checkout through a real git repository.

    The synthetic root is a linked worktree of a synthetic main checkout,
    which is where a developer's worktree stands. The only copy of the main
    Director's state is in the main checkout, so a command refuses it only
    when scripts/bosh hands the guard the right root to look it up from.
    """

    def setUp(self) -> None:
        self.repo = SyntheticRepo(git=True)
        self.addCleanup(self.repo.close)
        rebase = self.repo.rebase_shared(_integration)
        rebase.__enter__()
        self.addCleanup(rebase.__exit__, None, None, None)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.slot_dir = Path(self.tmp.name) / "certification"
        self.slot_dir.mkdir()
        (self.slot_dir / "slot.yml").write_text(GOOD_SLOT_YML)
        self.main_state = self.repo.plant_main_state("main-director", "3001")

    def _bosh(self, run=_never) -> object:
        mod = _load_bosh_script({"BOSH_STATE_DIR": str(self.slot_dir)}, self.repo)
        mod.slot_env_facts = lambda: FACTS
        mod.cpi_release_path = lambda: Path(self.tmp.name) / "release.tgz"
        mod._cpi_release_name = lambda _p: "bosh-proxmox-cpi"
        mod._light_stemcell_create_env_vars = lambda: []
        mod.bosh_deployment_dir = lambda: Path(self.tmp.name)
        mod.compiled_ops_layer = lambda: []
        mod._delete_all_deployments = _never
        mod.bosh_int = _never
        mod.run = run
        return mod

    def test_the_synthetic_root_is_a_linked_worktree(self) -> None:
        self.assertTrue((self.repo.root / ".git").is_file())
        found = _slot.main_checkout_root(self.repo.root)
        self.assertEqual(os.path.realpath(found), os.path.realpath(self.repo.main))
        self.assertFalse((self.repo.default_dir / "state.json").exists())

    def test_teardown_refuses_a_copy_of_the_main_state(self) -> None:
        shutil.copyfile(self.main_state, self.slot_dir / "state.json")
        _plant_owner(_slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(self.slot_dir)}))
        mod = self._bosh()
        err = io.StringIO()
        with _environ(**DENY), contextlib.redirect_stderr(err):
            with self.assertRaises(SystemExit) as cm:
                mod.cmd_teardown([])
        self.assertEqual(cm.exception.code, 2)
        self.assertIn("records the same Director VM as", err.getvalue())
        self.assertIn(os.path.realpath(self.main_state), err.getvalue())
        self.assertIn("doesn't list that VM", err.getvalue())

    def test_a_state_of_its_own_passes(self) -> None:
        _write_state(self.slot_dir / "state.json", "4001", "own-director")
        _plant_owner(_slot.resolve(self.repo.root, {"BOSH_STATE_DIR": str(self.slot_dir)}))
        mod = self._bosh()
        with _environ(**DENY):
            self.assertIsNone(mod.slot_refusal("delete-env"))

    def test_create_env_does_not_record_a_copy_of_the_main_state(self) -> None:
        def run(*_argv, **_kw):
            shutil.copyfile(self.main_state, self.slot_dir / "state.json")
            return 0
        mod = self._bosh(run)
        err = io.StringIO()
        with _environ(**DENY), contextlib.redirect_stderr(err):
            rc = mod.cmd_create_env([])
        self.assertEqual(rc, 1)
        self.assertIn("will not record", err.getvalue())
        self.assertIn("found the same Director VM as", err.getvalue())
        self.assertFalse((self.slot_dir / "slot-owner.json").exists())

    def test_create_env_records_a_state_of_its_own(self) -> None:
        def run(*_argv, **_kw):
            _write_state(self.slot_dir / "state.json", "4001", "own-director")
            return 0
        mod = self._bosh(run)
        with _environ(**DENY):
            self.assertEqual(mod.cmd_create_env([]), 0)
        record = json.loads((self.slot_dir / "slot-owner.json").read_text())
        self.assertEqual(record, _record("own-director", "4001"))

    def test_a_broken_git_link_stops_the_owner_record(self) -> None:
        def run(*_argv, **_kw):
            _write_state(self.slot_dir / "state.json", "4001", "own-director")
            return 0
        mod = self._bosh(run)
        with _environ(**DENY):
            self.assertEqual(mod.cmd_create_env([]), 0)
            (self.slot_dir / "slot-owner.json").unlink()
            err = io.StringIO()
            with mock.patch.object(_slot, "main_checkout_root",
                                   side_effect=_slot.SlotError("git broke")), \
                    contextlib.redirect_stderr(err):
                self.assertEqual(mod.record_slot_owner(0), 1)
        self.assertIn("no owner record was written", err.getvalue())
        self.assertFalse((self.slot_dir / "slot-owner.json").exists())


class IntegrationDirectorEnvTest(_SyntheticRoot):
    """The harnesses' shared director env follows the slot too."""

    CFG = {"bosh_creds": "manifests/bosh/creds.yml", "tier2": {"bosh_env_alias": "pve"}}

    def _env(self) -> dict:
        with mock.patch.object(_integration, "bosh_int",
                               side_effect=lambda f, p, dry_run=False: f"{f}{p}"):
            return _integration.director_env(self.CFG)

    def test_default_slot_uses_the_config(self) -> None:
        with _environ():
            os.environ.pop("BOSH_STATE_DIR", None)
            env = self._env()
        self.assertEqual(env["BOSH_ENVIRONMENT"], "pve")
        self.assertEqual(env["BOSH_CLIENT_SECRET"], "manifests/bosh/creds.yml/admin_password")

    # Valid against the cpitest env bundle the synthetic root copies.
    SLOT_YML = ("internal_ip: 172.31.0.12\nbosh_alias: pve-cert\n"
                "pve_create_env_deployment: create-env-cert\n")

    def test_a_named_slot_overrides_creds_and_alias(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "slot.yml").write_text(self.SLOT_YML)
            with _environ(BOSH_STATE_DIR=tmp):
                env = self._env()
        self.assertEqual(env["BOSH_ENVIRONMENT"], "pve-cert")
        self.assertEqual(env["BOSH_CLIENT_SECRET"], f"{tmp}/creds.yml/admin_password")

    def test_a_slot_yml_on_the_main_alias_exits(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "slot.yml").write_text(
                self.SLOT_YML.replace("bosh_alias: pve-cert", "bosh_alias: pve"))
            with _environ(BOSH_STATE_DIR=tmp):
                with mock.patch.object(_integration, "bosh_int", side_effect=_never):
                    with self.assertRaises(SystemExit) as cm:
                        _integration.director_env(self.CFG)
        self.assertIn("bosh_alias to 'pve'", str(cm.exception.code))

    def test_default_slot_in_refuse_mode_exits(self) -> None:
        with _environ(BOSH_REFUSE_DEFAULT_SLOT="1", **DENY):
            os.environ.pop("BOSH_STATE_DIR", None)
            with mock.patch.object(_integration, "bosh_int", side_effect=_never):
                with self.assertRaises(SystemExit) as cm:
                    _integration.director_env(self.CFG)
        self.assertIn("refusing to run a Director command", str(cm.exception.code))
        self.assertIn("default slot", str(cm.exception.code))

    def test_a_copied_state_exits(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            _write_state(Path(tmp) / "state.json", "4001")
            with _environ(BOSH_STATE_DIR=tmp):
                with mock.patch.object(_integration, "bosh_int", side_effect=_never):
                    with self.assertRaises(SystemExit) as cm:
                        _integration.director_env(self.CFG)
        self.assertIn("was not created in this slot", str(cm.exception.code))


REPO = SCRIPTS.parent
CPITEST_VARS = REPO / "manifests" / "envs" / "cpitest" / "vars.yml"
SLOT_TEMPLATE = REPO / "ci" / "certification-slot.yml"
SCHEDULED_CONFIG = REPO / "ci" / "integration.scheduled.yml"
RESERVED_PATH = "/networks/name=default/subnets/0/reserved"
# The certification Director's half of the cpitest dynamic range. The main
# Director's dynamic band starts at .20 and BOSH places the lowest free
# address first, so the certification slot keeps to the top of the range.
CERT_BAND = ("172.31.0.150", "172.31.0.189")


def _yaml(path: Path) -> dict:
    import yaml
    return yaml.safe_load(path.read_text()) or {}


def _addresses(entries: "list[str]") -> "set[int]":
    """Every address a list of reserved or static entries covers."""
    import ipaddress
    out: set[int] = set()
    for entry in entries:
        text = str(entry).strip()
        if "-" in text:
            lo, _, hi = text.partition("-")
            first, last = int(ipaddress.ip_address(lo.strip())), int(ipaddress.ip_address(hi.strip()))
        elif "/" in text:
            net = ipaddress.ip_network(text, strict=False)
            first, last = int(net.network_address), int(net.broadcast_address)
        else:
            first = last = int(ipaddress.ip_address(text))
        out.update(range(first, last + 1))
    return out


def _band(lo: str, hi: str) -> "set[int]":
    return _addresses([f"{lo}-{hi}"])


def _dynamic(cidr: str, gateway: str, reserved: "list[str]", static: "list[str]") -> "set[int]":
    """The addresses a BOSH manual network can place dynamically."""
    import ipaddress
    net = ipaddress.ip_network(cidr)
    usable = set(range(int(net.network_address) + 1, int(net.broadcast_address)))
    return usable - _addresses([gateway]) - _addresses(reserved) - _addresses(static)


class CertificationBandTest(unittest.TestCase):
    """The certification slot and the scheduled BATS run stay in .150-.189."""

    def setUp(self) -> None:
        self.env = _yaml(CPITEST_VARS)
        self.template = _yaml(SLOT_TEMPLATE)

    def test_template_keeps_every_env_reserved_entry(self) -> None:
        # slot.yml replaces the env's cpitest_reserved for the slot, so a
        # reserved entry the env adds later must be copied here too.
        missing = [e for e in self.env["cpitest_reserved"]
                   if e not in self.template.get("cpitest_reserved", [])]
        self.assertEqual(missing, [], "ci/certification-slot.yml dropped env reserved entries")

    def test_slot_dynamic_band_is_the_top_of_the_range(self) -> None:
        dynamic = _dynamic(
            self.env["internal_cidr"], self.env["internal_gw"],
            self.template.get("cpitest_reserved", []), [],
        )
        self.assertEqual(dynamic, _band(*CERT_BAND))

    def test_main_dynamic_band_is_unchanged(self) -> None:
        dynamic = _dynamic(
            self.env["internal_cidr"], self.env["internal_gw"],
            self.env["cpitest_reserved"], self.env.get("cpitest_static", []),
        )
        self.assertEqual(dynamic, _band("172.31.0.20", "172.31.0.199") - _addresses(["172.31.0.50"]))

    def test_scheduled_bats_stays_in_the_certification_band(self) -> None:
        bats = _yaml(SCHEDULED_CONFIG)["bats"]
        static = _addresses(bats["network_static"])
        dynamic = _dynamic(
            self.env["internal_cidr"], self.env["internal_gw"],
            bats["network_reserved"], bats["network_static"],
        )
        footprint = static | dynamic
        self.assertTrue(footprint, "the scheduled BATS network has no addresses")
        self.assertLessEqual(footprint, _band(*CERT_BAND))
        for key in ("static_ip", "second_static_ip"):
            self.assertIn(next(iter(_addresses([bats[key]]))), static, key)

    def test_scheduled_bats_bands_pass_the_bats_checks(self) -> None:
        bats_mod = _load_script("bats")
        bats = _yaml(SCHEDULED_CONFIG)["bats"]
        bats_mod._check_subnet_consistency(
            self.env["internal_cidr"], self.env["internal_gw"],
            [str(r) for r in bats["network_reserved"]],
            [str(r) for r in bats["network_static"]],
            str(bats["static_ip"]), "bats", "",
        )


def _load_script(name: str) -> object:
    path = str(SCRIPTS / name)
    loader = _ilm.SourceFileLoader(f"{name}_script_under_test", path)
    spec = _ilu.spec_from_file_location(f"{name}_script_under_test", path, loader=loader)
    module = _ilu.module_from_spec(spec)
    loader.exec_module(module)
    return module


class CloudConfigRenderTest(unittest.TestCase):
    """`scripts/bosh ucc` renders the slot's band and leaves the main one alone.

    The ucc argv is captured with run() patched out, then rendered offline with
    `bosh int` (no Director, no network) against the tracked cloud config, the
    cpitest ops and vars, and an empty stand-in for the untracked vars file.
    """

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.vars = Path(self.tmp.name) / "vars.yml"
        self.vars.write_text("{}\n")
        self.slot_dir = Path(self.tmp.name) / "certification"
        self.slot_dir.mkdir()
        (self.slot_dir / "slot.yml").write_text(
            SLOT_TEMPLATE.read_text().replace("((certification_director_ip))", "192.0.2.12")
        )

    def _ucc_argv(self, environ: dict) -> "list[str]":
        mod = _load_bosh_script(environ)
        captured: list[list[str]] = []
        mod.run = lambda *argv, **_kw: captured.append(list(argv)) or 0
        mod.director_env = lambda: {}
        mod.VARS = self.vars
        with mock.patch.dict(os.environ, {"BOSH_PVE_ENV": "cpitest"}):
            self.assertEqual(mod.cmd_ucc([]), 0)
        self.assertEqual(len(captured), 1)
        return captured[0]

    def test_default_slot_argv_is_the_main_directors(self) -> None:
        argv = self._ucc_argv({})
        envs = REPO / "manifests" / "envs" / "cpitest"
        network_ops = ["-o", str(envs / "network.yml")] if (envs / "network.yml").exists() else []
        self.assertEqual(argv, [
            "bosh", "-e", "pve", "-n", "update-cloud-config",
            str(REPO / "manifests" / "bosh" / "cloud-config.yml"),
            *network_ops,
            "-o", str(envs / "cc-reserved.yml"),
            "-l", str(self.vars),
            "-l", str(envs / "vars.yml"),
        ])

    def _render_reserved(self, argv: "list[str]") -> "list[str]":
        if shutil.which("bosh") is None:
            self.skipTest("bosh CLI not installed")
        self.assertEqual(argv[3:5], ["-n", "update-cloud-config"])
        proc = subprocess.run(
            ["bosh", "int", *argv[5:], "--path", RESERVED_PATH],
            capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)
        import yaml
        return yaml.safe_load(proc.stdout)

    def test_main_render_reserves_only_the_env_bands(self) -> None:
        reserved = self._render_reserved(self._ucc_argv({}))
        self.assertEqual(reserved, _yaml(CPITEST_VARS)["cpitest_reserved"])

    def test_slot_render_reserves_the_main_directors_band(self) -> None:
        argv = self._ucc_argv({"BOSH_STATE_DIR": str(self.slot_dir)})
        self.assertEqual(argv[2], "pve-certification")
        reserved = self._render_reserved(argv)
        self.assertEqual(reserved, _yaml(SLOT_TEMPLATE)["cpitest_reserved"])
        env = _yaml(CPITEST_VARS)
        self.assertEqual(
            _dynamic(env["internal_cidr"], env["internal_gw"], reserved, []),
            _band(*CERT_BAND),
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
