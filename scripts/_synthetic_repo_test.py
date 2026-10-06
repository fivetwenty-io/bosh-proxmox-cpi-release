#!/usr/bin/env python3
"""Self-tests for the read guard and the git-backed synthetic repository.

Run with:
    python3 scripts/_synthetic_repo_test.py

The guard that keeps the slot and certify tests off the real Director files
is only worth having if it trips. These tests aim it at planted temporary
files, never at a real state.json, creds.yml, or vars.yml.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
import _slot  # noqa: E402
import _synthetic_repo as sr  # noqa: E402
from _synthetic_repo import SyntheticRepo, install_read_guard  # noqa: E402

install_read_guard()


class _PlantedFile(unittest.TestCase):
    """A temporary file that the guard is told to treat as a real Director file."""

    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.planted = Path(tmp.name) / "state.json"
        self.planted.write_text("{}")
        self.other = Path(tmp.name) / "other.json"
        self.other.write_text("{}")
        patch = mock.patch.object(sr, "FORBIDDEN", {os.path.realpath(self.planted)})
        patch.start()
        self.addCleanup(patch.stop)


class ReadGuardTest(_PlantedFile):
    def test_the_guard_trips_on_a_planted_read(self) -> None:
        with self.assertRaises(sr.ForbiddenRead) as cm:
            self.planted.read_text()
        self.assertIn("opened the real Director file", str(cm.exception))

    def test_the_guard_trips_on_a_read_through_a_symlink(self) -> None:
        link = self.planted.with_name("link.json")
        link.symlink_to(self.planted)
        with self.assertRaises(sr.ForbiddenRead):
            open(link).close()

    def test_the_guard_trips_on_a_subprocess_that_names_the_file(self) -> None:
        with self.assertRaises(sr.ForbiddenRead) as cm:
            subprocess.run(["cat", str(self.planted)], capture_output=True)
        self.assertIn("handed a subprocess", str(cm.exception))

    def test_other_files_stay_readable(self) -> None:
        self.assertEqual(self.other.read_text(), "{}")
        proc = subprocess.run(["cat", str(self.other)], capture_output=True, text=True)
        self.assertEqual(proc.stdout, "{}")

    def test_a_broad_handler_cannot_swallow_the_guard(self) -> None:
        self.assertFalse(issubclass(sr.ForbiddenRead, Exception))
        self.assertTrue(issubclass(sr.ForbiddenRead, BaseException))

        def swallow() -> str:
            try:
                return self.planted.read_text()
            except Exception:  # noqa: BLE001 - this is the handler under test
                return "swallowed"

        with self.assertRaises(sr.ForbiddenRead):
            swallow()

    def test_the_real_director_files_are_on_the_forbidden_list(self) -> None:
        real = sr.forbidden_files()
        for name in sr.SECRET_FILES:
            self.assertIn(os.path.realpath(sr.REAL_ROOT / "manifests" / "bosh" / name), real)


@unittest.skipUnless(shutil.which("git"), "git is not on PATH")
class GitFailureTest(unittest.TestCase):
    def test_a_root_without_git_has_no_main_checkout(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            self.assertIsNone(sr._main_checkout(Path(tmp)))

    def test_a_git_failure_stops_the_run(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            # A .git that git can't read: the listing fails, and so must the guard.
            (Path(tmp) / ".git").write_text(f"gitdir: {Path(tmp) / 'missing'}\n")
            with self.assertRaises(sr.GuardSetupError) as cm:
                sr._main_checkout(Path(tmp))
            self.assertIn("can't be added to the read guard", str(cm.exception))

    def test_git_that_cannot_run_stops_the_run(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / ".git").mkdir()
            for error in (FileNotFoundError("git"), subprocess.TimeoutExpired("git", 30)):
                with self.subTest(error=type(error).__name__):
                    with mock.patch.object(sr.subprocess, "run", side_effect=error):
                        with self.assertRaises(sr.GuardSetupError):
                            sr._main_checkout(Path(tmp))

    def test_a_listing_with_no_worktree_stops_the_run(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / ".git").mkdir()
            done = subprocess.CompletedProcess([], 0, stdout="", stderr="")
            with mock.patch.object(sr.subprocess, "run", return_value=done):
                with self.assertRaises(sr.GuardSetupError):
                    sr._main_checkout(Path(tmp))

    @unittest.skipUnless((sr.REAL_ROOT / ".git").exists(), "this tree has no .git")
    def test_forbidden_files_do_not_drop_the_main_checkout_on_a_git_failure(self) -> None:
        with mock.patch.object(sr.subprocess, "run", side_effect=OSError("no git")):
            with self.assertRaises(sr.GuardSetupError):
                sr.forbidden_files()


@unittest.skipUnless(shutil.which("git"), "git is not on PATH")
class GitBackedRepoTest(unittest.TestCase):
    def test_the_root_is_a_linked_worktree_of_the_main_checkout(self) -> None:
        repo = SyntheticRepo(git=True)
        self.addCleanup(repo.close)
        self.assertTrue((repo.root / ".git").is_file())
        found = _slot.main_checkout_root(repo.root)
        self.assertEqual(os.path.realpath(found), os.path.realpath(repo.main))
        self.assertTrue((repo.root / "manifests" / "bosh" / "cloud-config.yml").exists())
        self.assertTrue((repo.default_dir / "vars.yml").exists())

    def test_planting_a_main_state_writes_only_into_the_main_checkout(self) -> None:
        repo = SyntheticRepo(git=True)
        self.addCleanup(repo.close)
        state = repo.plant_main_state("main-director", "3001", current_disk_id="disk-1")
        self.assertEqual(state, repo.main / "manifests" / "bosh" / "state.json")
        self.assertIn('"current_vm_cid": "3001"', state.read_text())
        self.assertFalse((repo.default_dir / "state.json").exists())

    def test_the_default_repo_has_no_git_and_cannot_plant_a_main_state(self) -> None:
        repo = SyntheticRepo()
        self.addCleanup(repo.close)
        self.assertFalse((repo.root / ".git").exists())
        self.assertIsNone(_slot.main_checkout_root(repo.root))
        with self.assertRaises(RuntimeError):
            repo.plant_main_state("main-director", "3001")


if __name__ == "__main__":
    unittest.main(verbosity=2)
