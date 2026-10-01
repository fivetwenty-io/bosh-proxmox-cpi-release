"""Decide whether a release tag may be published, where it comes from, and whether it is Latest.

A patch tag vX.Y.Z whose Z is above zero comes from the release/X.Y branch
whenever that branch exists. It must be a final tag, and its commit must be on
that branch, whether or not the commit is also on main. That is how we ship a
fix for an older minor line after main has moved on, and it stops main's newer
work from going out under the older line's version. Every other tag must
point at a commit on main. That covers every tag whose Z is zero, and every
patch tag or prerelease for a line that has no release branch. A tag that
breaks these rules is refused, and the message says which rule it broke and
how to fix the tag.

A release is marked Latest only when its tag is the highest final version tag
in the repository, so a patch for an older line never takes the badge from a
newer release. Prereleases are never Latest, so they don't count as higher.

The release workflow runs this from the tagged checkout, and
scripts/_release_tag_test.py covers each rule against throwaway repositories.
"""
from __future__ import annotations

import argparse
from dataclasses import dataclass
from pathlib import Path
import re
import subprocess

TAG_PATTERN = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.]+))?")


class TagRefused(ValueError):
    """The tag breaks a release rule, and the message says how to fix it."""


@dataclass(frozen=True)
class Version:
    major: int
    minor: int
    patch: int
    prerelease: str

    @property
    def text(self) -> str:
        suffix = f"-{self.prerelease}" if self.prerelease else ""
        return f"{self.major}.{self.minor}.{self.patch}{suffix}"

    @property
    def line(self) -> str:
        return f"{self.major}.{self.minor}"

    @property
    def branch(self) -> str:
        return f"release/{self.line}"

    @property
    def triple(self) -> tuple[int, int, int]:
        return (self.major, self.minor, self.patch)


def parse_tag(tag: str) -> Version:
    match = TAG_PATTERN.fullmatch(tag)
    if not match:
        raise TagRefused(f"tag '{tag}' is not vX.Y.Z or vX.Y.Z-<prerelease>")
    major, minor, patch, prerelease = match.groups()
    return Version(int(major), int(minor), int(patch), prerelease or "")


def is_latest(version: Version, tags: list[str]) -> bool:
    """Report whether no final version tag in the repository is higher than this one."""
    if version.prerelease:
        return False
    for tag in tags:
        try:
            other = parse_tag(tag)
        except TagRefused:
            continue
        if not other.prerelease and other.triple > version.triple:
            return False
    return True


class GitRepo:
    """The git questions the release rule asks, answered from a checkout and its remote."""

    def __init__(self, path: Path, remote: str = "origin") -> None:
        self.path = path
        self.remote = remote

    def _git(self, *args: str, allowed: tuple[int, ...] = (0,)) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(["git", *args], cwd=self.path, capture_output=True, text=True, check=False)
        if result.returncode not in allowed:
            detail = result.stderr.strip() or result.stdout.strip()
            raise RuntimeError(f"git {' '.join(args)} exited {result.returncode}: {detail}")
        return result

    def fetch(self, branch: str) -> None:
        self._git("fetch", "--no-tags", self.remote, f"+refs/heads/{branch}:refs/remotes/{self.remote}/{branch}")

    def has_branch(self, branch: str) -> bool:
        # ls-remote exits 2 when the branch does not exist, which tells a
        # missing branch apart from a remote we cannot reach.
        result = self._git("ls-remote", "--exit-code", "--heads", self.remote, f"refs/heads/{branch}", allowed=(0, 2))
        return result.returncode == 0

    def contains(self, branch: str, sha: str) -> bool:
        result = self._git("merge-base", "--is-ancestor", sha, f"refs/remotes/{self.remote}/{branch}", allowed=(0, 1))
        return result.returncode == 0

    def release_branches_containing(self, sha: str) -> list[str]:
        prefix = f"refs/remotes/{self.remote}/"
        result = self._git("for-each-ref", "--contains", sha, "--format=%(refname)", f"{prefix}release/")
        return sorted(line.removeprefix(prefix) for line in result.stdout.splitlines() if line)

    def tags(self) -> list[str]:
        return self._git("tag", "--list").stdout.split()

    def nearest_final_tag_before(self, sha: str) -> str:
        result = self._git(
            "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", "--exclude", "*-*", f"{sha}^",
            allowed=(0, 128),
        )
        return result.stdout.strip() if result.returncode == 0 else ""


def _where_else(git: GitRepo, sha: str) -> str:
    branches = git.release_branches_containing(sha)
    if not branches:
        return ""
    tags = " or ".join(f"v{branch.removeprefix('release/')}.N" for branch in branches)
    return f"The commit is on {' and '.join(branches)}, so a tag for it would be {tags}.\n"


def release_source(tag: str, version: Version, sha: str, git: GitRepo) -> str:
    """Return the branch this tag releases from, or raise TagRefused naming the broken rule."""
    branch = version.branch
    # Once a line has its own release branch, its patches come only from
    # there, even when the commit is also on main, so main's newer work never
    # ships under the older line's version.
    if version.patch > 0 and git.has_branch(branch):
        if version.prerelease:
            raise TagRefused(
                f"tag '{tag}' is a prerelease, and {branch} exists, so a patch for {version.line}\n"
                "comes from that branch as a final tag.\n"
                f"Delete the tag, put the fix on {branch}, and tag it "
                f"v{version.major}.{version.minor}.{version.patch}."
            )
        git.fetch(branch)
        if git.contains(branch, sha):
            return branch
        git.fetch("main")
        on_main = ""
        if git.contains("main", sha):
            on_main = f"The commit is on main, and main's newer work must not ship as {version.text}.\n"
        raise TagRefused(
            f"tag '{tag}' points at {sha}, which is not on {branch}.\n"
            f"{branch} exists, so a patch for {version.line} comes from that branch.\n"
            f"{on_main}{_where_else(git, sha)}"
            f"Delete the tag, cherry-pick the fix onto {branch} with -x, and tag that commit."
        )
    git.fetch("main")
    if git.contains("main", sha):
        return "main"
    if version.prerelease or version.patch == 0:
        raise TagRefused(
            f"tag '{tag}' points at {sha}, which is not on main.\n"
            "Only a final patch tag (vX.Y.Z with Z above zero) may come from a release branch,\n"
            "so this tag must point at main. Delete the tag, land the commit on main, and tag again."
        )
    raise TagRefused(
        f"tag '{tag}' points at {sha}, which is not on main,\n"
        f"and there is no {branch} branch to release it from.\n"
        f"{_where_else(git, sha)}"
        f"Delete the tag, then either land the commit on main or cut {branch}\n"
        f"from v{version.line}.0 with the fix on it, and tag again."
    )


def release_plan(tag: str, sha: str, git: GitRepo) -> dict[str, str]:
    version = parse_tag(tag)
    source = release_source(tag, version, sha, git)
    return {
        "version": version.text,
        "prerelease": "true" if version.prerelease else "false",
        "source": source,
        "latest": "true" if is_latest(version, git.tags()) else "false",
        # A release from a branch starts its generated notes at the nearest
        # final tag behind it there, rather than at whichever release GitHub
        # would pick, which may be a newer one from main.
        "notes_start_tag": "" if source == "main" else git.nearest_final_tag_before(sha),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--tag", required=True, help="the pushed tag name, such as v0.8.1")
    parser.add_argument("--sha", required=True, help="the commit the tag points at")
    parser.add_argument("--remote", default="origin")
    parser.add_argument("--repo", type=Path, default=Path("."))
    parser.add_argument("--github-output", type=Path, help="append the plan here as key=value lines")
    args = parser.parse_args()
    try:
        plan = release_plan(args.tag, args.sha, GitRepo(args.repo, args.remote))
    except (TagRefused, RuntimeError) as error:
        parser.exit(1, f"ERROR: {error}\n")
    print(f"Releasing {args.tag} from {plan['source']}, Latest {plan['latest']}")
    if args.github_output:
        with args.github_output.open("a", encoding="utf-8") as stream:
            for key, value in plan.items():
                stream.write(f"{key}={value}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
