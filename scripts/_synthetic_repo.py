"""A throwaway repository root for the slot and certify tests.

scripts/bosh, scripts/certify, and the harness modules find the main
Director's state.json, creds.yml, and vars.yml from their own location. A test
that loads them as they are reads this checkout's Director files, and from a
linked worktree the guard also reads the main checkout's. So these tests
rebase every module they load onto a temporary root instead, in-process with
mock.patch, and the scripts themselves keep no switch that moves the root or
turns the guard's reads off.

install_read_guard() backs that up. It makes any open() of those files, and
any subprocess whose argv names one, raise ForbiddenRead before the file is
read. ForbiddenRead derives from BaseException, so an `except Exception`
handler in the code under test can't swallow it and turn a blocked read into
a passing test. scripts/_synthetic_repo_test.py proves the guard trips.

A SyntheticRepo built with git=True is a real git repository, so the tests
can run the main-checkout lookup that scripts/bosh and scripts/certify use.
"""

from __future__ import annotations

import contextlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from unittest import mock

REAL_ROOT = Path(__file__).resolve().parent.parent
SECRET_FILES = ("state.json", "creds.yml", "vars.yml")
# Tracked inputs the certify dry run reads. None of them holds a secret.
COPIED_FILES = (
    "ci/integration.scheduled.yml",
    "manifests/envs/cpitest/vars.yml",
    "manifests/envs/cpitest/artifacts.yml",
    "manifests/envs/cpitest/cc-reserved.yml",
    "manifests/certification/cloud-config-ops.yml",
    "manifests/certification/cloud-config-cpi-ops.yml",
    "manifests/bosh/cloud-config.yml",
)
# The synthetic manifests/bosh/vars.yml. The env bundle's vars.yml, copied
# above, supplies the Director IP, gateway, and reserved bands on top of it.
SYNTHETIC_VARS = "pve_create_env_deployment: create-env\ninternal_gw: 172.31.0.1\n"

_guard_installed = False
# The real paths the audit hook refuses. install_read_guard fills it, and the
# hook reads it on every event, so a self-test can aim the hook at a planted
# file.
FORBIDDEN: "set[str]" = set()


class ForbiddenRead(BaseException):
    """A test opened, or handed a subprocess, a real Director file.

    It derives from BaseException so that a broad `except Exception` in the
    code under test can't swallow it. unittest still records it as a failed
    test.
    """


class GuardSetupError(RuntimeError):
    """The read guard can't tell which files it must protect."""


def _main_checkout(root: Path) -> "Path | None":
    """The main worktree git names for root, or None when root has no .git.

    Raises GuardSetupError when root has a .git and git can't name its main
    worktree. Dropping the main checkout's files from the forbidden set would
    leave them readable, so the whole test run stops instead.
    """
    if not (root / ".git").exists():
        return None
    try:
        proc = subprocess.run(
            ["git", "-C", str(root), "worktree", "list", "--porcelain"],
            capture_output=True, text=True, timeout=30,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise GuardSetupError(
            f"git could not list the worktrees of {root} ({exc}), so the main "
            "checkout's Director files can't be added to the read guard"
        ) from exc
    if proc.returncode != 0:
        raise GuardSetupError(
            f"`git worktree list` failed in {root} (exit {proc.returncode}), so "
            "the main checkout's Director files can't be added to the read guard"
        )
    for line in proc.stdout.splitlines():
        if line.startswith("worktree "):
            return Path(line[len("worktree "):])
    raise GuardSetupError(
        f"`git worktree list` named no worktree in {root}, so the main "
        "checkout's Director files can't be added to the read guard"
    )


def forbidden_files() -> "set[str]":
    """The real paths of every Director secret file the tests must not read."""
    dirs = [REAL_ROOT / "manifests" / "bosh"]
    main = _main_checkout(REAL_ROOT)
    if main is not None:
        dirs.append(main / "manifests" / "bosh")
    return {os.path.realpath(d / name) for d in dirs for name in SECRET_FILES}


def install_read_guard() -> None:
    """Raise ForbiddenRead for a real Director file a test opens or passes on.

    An audit hook can't be removed, so this installs one per process and
    leaves it in place for the whole run. Raises GuardSetupError, which stops
    the run, when git can't say where the main checkout is.
    """
    global _guard_installed
    if _guard_installed:
        return
    FORBIDDEN.update(forbidden_files())

    def hook(event: str, args: tuple) -> None:
        if event == "open" and args and isinstance(args[0], (str, bytes, os.PathLike)):
            path = os.path.realpath(os.fsdecode(args[0]))
            if path in FORBIDDEN:
                raise ForbiddenRead(f"a test opened the real Director file {path}")
        elif event == "subprocess.Popen" and len(args) > 1 and args[1]:
            for arg in args[1]:
                if not isinstance(arg, (str, bytes, os.PathLike)):
                    continue
                text = os.fsdecode(arg)
                for path in FORBIDDEN:
                    if path in text or path in os.path.realpath(text):
                        raise ForbiddenRead(
                            f"a test handed a subprocess the real Director file {path}")

    sys.addaudithook(hook)
    _guard_installed = True


def _git(cwd: Path, *argv: str) -> str:
    """Run git in cwd against a synthetic repository, ignoring the user's config."""
    env = {**os.environ, "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_SYSTEM": os.devnull,
           "GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "test@example.com",
           "GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "test@example.com"}
    proc = subprocess.run(["git", "-C", str(cwd), *argv], env=env, capture_output=True,
                          text=True, check=False)
    if proc.returncode != 0:
        raise RuntimeError(f"git {argv[0]} failed in {cwd}: {proc.stderr.strip()}")
    return proc.stdout


class SyntheticRepo:
    """A temporary repository root with tracked inputs and synthetic vars.

    By default it has no .git, so the guard finds no main checkout through it,
    and its manifests/bosh/ holds no state.json or creds.yml until a test
    writes one. With git=True, `main` is a real git repository that commits
    the tracked inputs, and `root` is a linked worktree of it, so
    _slot.main_checkout_root(root) names `main` the way it does for a
    developer's linked worktree. Everything in both is synthetic.
    """

    def __init__(self, vars_yml: str = SYNTHETIC_VARS, git: bool = False) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.base = Path(self._tmp.name)
        self.main: "Path | None" = None
        if git:
            self.main = self.base / "main"
            self._build(self.main, vars_yml)
            _git(self.main, "init", "-q")
            _git(self.main, "add", "-A")
            _git(self.main, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "synthetic")
            _git(self.main, "worktree", "add", "-q", "--detach", str(self.base / "repo"))
            self.root = self.base / "repo"
            self.default_dir = self.root / "manifests" / "bosh"
            (self.default_dir / "vars.yml").write_text(vars_yml)
        else:
            self.root = self.base / "repo"
            self.default_dir = self.root / "manifests" / "bosh"
            self._build(self.root, vars_yml)

    @staticmethod
    def _build(root: Path, vars_yml: str) -> None:
        """Copy the tracked inputs into root and give it a synthetic vars.yml."""
        default_dir = root / "manifests" / "bosh"
        default_dir.mkdir(parents=True)
        for rel in COPIED_FILES:
            source = REAL_ROOT / rel
            if source.exists():
                target = root / rel
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(source, target)
        (default_dir / "vars.yml").write_text(vars_yml)

    def plant_main_state(self, director_id: str, vm_cid: str, **extra: object) -> Path:
        """Write a synthetic state.json into the main checkout's manifests/bosh/."""
        if self.main is None:
            raise RuntimeError("plant_main_state needs SyntheticRepo(git=True)")
        path = self.main / "manifests" / "bosh" / "state.json"
        path.write_text(json.dumps(
            {"director_id": director_id, "current_vm_cid": vm_cid, **extra}))
        return path

    def close(self) -> None:
        self._tmp.cleanup()

    def rebased(self, value: object) -> object:
        """value with REAL_ROOT swapped for this root, when it lies under it."""
        import _slot
        if isinstance(value, _slot.Slot):
            return _slot.Slot(dir=self.rebased(value.dir),
                              default_dir=self.root / "manifests" / "bosh")
        if isinstance(value, Path):
            try:
                return self.root / value.relative_to(REAL_ROOT)
            except ValueError:
                return value
        if isinstance(value, str) and value.startswith(str(REAL_ROOT) + os.sep):
            return str(self.root) + value[len(str(REAL_ROOT)):]
        return value

    def rebase_module(self, module: object) -> None:
        """Point a throwaway module's root-derived globals at this root."""
        for name, value in list(vars(module).items()):
            if name.startswith("__"):
                continue
            new = self.rebased(value)
            if new is not value:
                setattr(module, name, new)

    @contextlib.contextmanager
    def rebase_shared(self, *modules: object):
        """Rebase shared modules for the duration of a with block."""
        with contextlib.ExitStack() as stack:
            for module in modules:
                for name, value in list(vars(module).items()):
                    if name.startswith("__"):
                        continue
                    new = self.rebased(value)
                    if new is not value:
                        stack.enter_context(mock.patch.object(module, name, new))
            yield self
