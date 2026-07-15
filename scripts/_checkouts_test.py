#!/usr/bin/env python3
"""Unit tests for scripts/_checkouts.py.

Run with:
    python3 scripts/_checkouts_test.py

Uses only stdlib; no network access — git interactions are stubbed.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _checkouts  # noqa: E402


def _completed(rc: int = 0, stdout: str = "", stderr: str = "") -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess(args=[], returncode=rc, stdout=stdout, stderr=stderr)


class CheckoutsBase(unittest.TestCase):
    def setUp(self) -> None:
        _checkouts.checkout_dir.cache_clear()
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        self.deps = self.tmp / ".deps"
        self._deps_patch = mock.patch.object(_checkouts, "DEPS_DIR", self.deps)
        self._deps_patch.start()
        # Isolate from the operator's real environment overrides.
        self._env_patch = mock.patch.dict(os.environ)
        self._env_patch.start()
        os.environ.pop("BOSH_DEPLOYMENT_DIR", None)
        os.environ.pop("CF_DEPLOYMENT", None)

    def tearDown(self) -> None:
        self._env_patch.stop()
        self._deps_patch.stop()
        self._tmp.cleanup()
        _checkouts.checkout_dir.cache_clear()

    def make_clone(self, name: str) -> Path:
        dest = self.deps / name
        (dest / ".git").mkdir(parents=True)
        return dest


class TestOverride(CheckoutsBase):
    def test_existing_override_wins(self) -> None:
        override = self.tmp / "my-bosh-deployment"
        override.mkdir()
        os.environ["BOSH_DEPLOYMENT_DIR"] = str(override)
        with mock.patch.object(_checkouts, "_clone") as clone, \
                mock.patch.object(_checkouts, "_refresh") as refresh:
            got = _checkouts.checkout_dir("bosh-deployment")
        self.assertEqual(got, override)
        clone.assert_not_called()
        refresh.assert_not_called()

    def test_missing_override_fails_fast(self) -> None:
        os.environ["BOSH_DEPLOYMENT_DIR"] = str(self.tmp / "nope")
        with self.assertRaises(SystemExit) as ctx:
            _checkouts.checkout_dir("bosh-deployment")
        self.assertIn("BOSH_DEPLOYMENT_DIR", str(ctx.exception))

    def test_blank_override_is_ignored(self) -> None:
        os.environ["CF_DEPLOYMENT"] = "   "
        dest = self.make_clone("cf-deployment")
        with mock.patch.object(_checkouts, "_refresh"):
            got = _checkouts.checkout_dir("cf-deployment")
        self.assertEqual(got, dest)


class TestCloneAndRefresh(CheckoutsBase):
    def test_absent_clone_is_created(self) -> None:
        with mock.patch.object(_checkouts, "_clone") as clone:
            got = _checkouts.checkout_dir("bosh-deployment")
        clone.assert_called_once()
        self.assertEqual(got, self.deps / "bosh-deployment")

    def test_existing_clone_is_refreshed_not_recloned(self) -> None:
        dest = self.make_clone("bosh-deployment")
        with mock.patch.object(_checkouts, "_clone") as clone, \
                mock.patch.object(_checkouts, "_refresh") as refresh:
            got = _checkouts.checkout_dir("bosh-deployment")
        clone.assert_not_called()
        refresh.assert_called_once_with("bosh-deployment", dest)
        self.assertEqual(got, dest)

    def test_result_is_cached_per_process(self) -> None:
        self.make_clone("bosh-deployment")
        with mock.patch.object(_checkouts, "_refresh") as refresh:
            _checkouts.checkout_dir("bosh-deployment")
            _checkouts.checkout_dir("bosh-deployment")
        refresh.assert_called_once()

    def test_failed_clone_raises_actionable_error(self) -> None:
        with mock.patch.object(_checkouts.subprocess, "run",
                               return_value=_completed(128, stderr="fatal: unable to access")):
            with self.assertRaises(SystemExit) as ctx:
                _checkouts.checkout_dir("bosh-deployment")
        self.assertIn("BOSH_DEPLOYMENT_DIR", str(ctx.exception))


class TestRefresh(CheckoutsBase):
    def test_fresh_marker_skips_fetch(self) -> None:
        dest = self.make_clone("bosh-deployment")
        marker = _checkouts._marker("bosh-deployment")
        marker.touch()
        with mock.patch.object(_checkouts, "_git") as git:
            _checkouts._refresh("bosh-deployment", dest)
        git.assert_not_called()

    def test_stale_marker_fetches_and_resets(self) -> None:
        dest = self.make_clone("bosh-deployment")
        marker = _checkouts._marker("bosh-deployment")
        marker.touch()
        stale = time.time() - _checkouts._REFRESH_SECONDS - 60
        os.utime(marker, (stale, stale))
        calls: list[tuple[str, ...]] = []

        def fake_git(dest_arg: Path, *args: str, **_kw) -> subprocess.CompletedProcess:
            calls.append(args)
            if args[0] == "symbolic-ref":
                return _completed(stdout="origin/master\n")
            if args[0] == "rev-parse":
                return _completed(stdout="abc1234\n")
            return _completed()

        with mock.patch.object(_checkouts, "_git", side_effect=fake_git):
            _checkouts._refresh("bosh-deployment", dest)
        ops = [c[0] for c in calls]
        self.assertIn("fetch", ops)
        self.assertIn("reset", ops)
        self.assertGreater(marker.stat().st_mtime, stale)

    def test_offline_fetch_keeps_cached_clone(self) -> None:
        dest = self.make_clone("bosh-deployment")

        def fake_git(dest_arg: Path, *args: str, **_kw) -> subprocess.CompletedProcess:
            if args[0] == "symbolic-ref":
                return _completed(stdout="origin/master\n")
            if args[0] == "fetch":
                return _completed(128, stderr="fatal: unable to access remote")
            return _completed()

        with mock.patch.object(_checkouts, "_git", side_effect=fake_git):
            _checkouts._refresh("bosh-deployment", dest)  # must not raise
        got = _checkouts.checkout_dir("bosh-deployment")
        self.assertEqual(got, dest)

    def test_unknown_branch_uses_clone_as_is(self) -> None:
        dest = self.make_clone("bosh-deployment")

        def fake_git(dest_arg: Path, *args: str, **_kw) -> subprocess.CompletedProcess:
            if args[0] == "symbolic-ref":
                return _completed(1, stderr="fatal: ref refs/remotes/origin/HEAD is not a symbolic ref")
            self.fail(f"unexpected git call: {args}")

        with mock.patch.object(_checkouts, "_git", side_effect=fake_git):
            _checkouts._refresh("bosh-deployment", dest)  # must not raise


if __name__ == "__main__":
    unittest.main(verbosity=2)
