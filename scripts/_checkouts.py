"""Resolve external upstream-repo checkouts (bosh-deployment, cf-deployment,
bosh-acceptance-tests).

An explicit environment override (BOSH_DEPLOYMENT_DIR / CF_DEPLOYMENT /
BATS_DIR) always
wins and must point at an existing directory — use it to test against a local
fork or a pinned revision. Without an override, a shallow clone of the
upstream repository is maintained under .deps/ at the repo root: cloned on
first use, fast-forwarded to upstream HEAD on later uses, and reused as-is
(with a warning) when upstream cannot be reached. A refresh marker keeps
repeated invocations within one e2e run from re-fetching every time
(CHECKOUTS_REFRESH_SECONDS overrides the window; 0 forces a fetch).
"""

from __future__ import annotations

import os
import subprocess
import sys
import time
from functools import lru_cache
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
DEPS_DIR = REPO_ROOT / ".deps"

# name -> (environment override variable, upstream clone URL)
UPSTREAMS: dict[str, tuple[str, str]] = {
    "bosh-deployment": (
        "BOSH_DEPLOYMENT_DIR",
        "https://github.com/cloudfoundry/bosh-deployment.git",
    ),
    "cf-deployment": (
        "CF_DEPLOYMENT",
        "https://github.com/cloudfoundry/cf-deployment.git",
    ),
    "bosh-acceptance-tests": (
        "BATS_DIR",
        "https://github.com/cloudfoundry/bosh-acceptance-tests.git",
    ),
}

# Skip the upstream fetch when the last successful one is younger than this.
_REFRESH_SECONDS = int(os.environ.get("CHECKOUTS_REFRESH_SECONDS", "900") or "900")


def _git(dest: Path, *args: str, timeout: int = 600) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["git", "-C", str(dest), *args],
        capture_output=True, text=True, timeout=timeout,
    )


def _note(msg: str) -> None:
    print(f"    checkouts: {msg}", file=sys.stderr)


def _default_branch(dest: Path) -> "str | None":
    """The clone's default branch (from origin/HEAD, which git clone records)."""
    proc = _git(dest, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
    if proc.returncode != 0:
        return None
    ref = proc.stdout.strip()  # e.g. "origin/main"
    return ref.split("/", 1)[1] if "/" in ref else None


def _marker(name: str) -> Path:
    return DEPS_DIR / f".{name}.last-fetch"


def _refresh(name: str, dest: Path) -> None:
    """Fast-forward an existing clone to upstream HEAD; tolerate being offline."""
    marker = _marker(name)
    if (
        _REFRESH_SECONDS > 0
        and marker.exists()
        and time.time() - marker.stat().st_mtime < _REFRESH_SECONDS
    ):
        return
    branch = _default_branch(dest)
    if branch is None:
        _note(f"{name}: cannot determine upstream branch; using clone as-is")
        return
    fetch = _git(dest, "fetch", "--depth", "1", "origin", branch)
    if fetch.returncode != 0:
        detail = (fetch.stderr.strip().splitlines() or ["fetch failed"])[-1]
        _note(f"{name}: upstream unreachable ({detail}); using cached clone")
        return
    reset = _git(dest, "reset", "--hard", "FETCH_HEAD")
    if reset.returncode != 0:
        raise SystemExit(
            f"checkouts: refreshing {dest} failed: {reset.stderr.strip()}\n"
            f"Remove the directory to re-clone, or set "
            f"{UPSTREAMS[name][0]} to a checkout of your own."
        )
    head = _git(dest, "rev-parse", "--short", "HEAD").stdout.strip()
    _note(f"{name}: refreshed to upstream {branch} ({head})")
    marker.touch()


def _clone(name: str, url: str, dest: Path) -> None:
    DEPS_DIR.mkdir(exist_ok=True)
    _note(f"{name}: cloning {url} (shallow) into {dest}")
    proc = subprocess.run(
        ["git", "clone", "--depth", "1", url, str(dest)],
        capture_output=True, text=True, timeout=1800,
    )
    if proc.returncode != 0:
        detail = (proc.stderr.strip().splitlines() or ["clone failed"])[-1]
        raise SystemExit(
            f"checkouts: cloning {url} failed: {detail}\n"
            f"Set {UPSTREAMS[name][0]} to an existing checkout to skip cloning."
        )
    _marker(name).touch()


@lru_cache(maxsize=None)
def checkout_dir(name: str) -> Path:
    """The directory holding checkout `name`, cloning/refreshing as needed.

    Cached per process so one invocation fetches at most once per repo.
    Raises SystemExit with an actionable message when an explicit override
    points nowhere or a required initial clone fails.
    """
    env_var, url = UPSTREAMS[name]
    override = os.environ.get(env_var, "").strip()
    if override:
        path = Path(override).expanduser()
        if not path.is_dir():
            raise SystemExit(
                f"checkouts: {env_var}={override} is not a directory; "
                f"unset it to use an automatic {DEPS_DIR / name} clone"
            )
        return path
    dest = DEPS_DIR / name
    if (dest / ".git").is_dir():
        _refresh(name, dest)
    else:
        _clone(name, url, dest)
    return dest
