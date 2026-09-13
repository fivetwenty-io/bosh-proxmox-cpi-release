#!/bin/sh
# Refuse merge commits anywhere in the history behind a commit.
#
# main is linear: every change lands by a rebase or a squash, never by a
# merge commit. GitHub enforces that on main with a ruleset; this script
# is the local and CI backstop, so a merge commit is caught by the
# pre-push hook and by `make check` before it ever reaches the server, and
# so a pull request branch that merged main into itself fails CI instead
# of being quietly linearized at merge time.
#
# Usage:
#   scripts/_linear_history_check.sh [<commit>]   # default HEAD
#
# The check walks every commit reachable from <commit>, so it needs the
# full commit graph. A shallow clone passes vacuously for the commits it
# cannot see; CI fetches the whole graph (without trees or blobs) for that
# reason.

set -eu

tip="${1:-HEAD}"

merges=$(git rev-list --merges "$tip")
[ -z "$merges" ] && exit 0

count=$(printf '%s\n' "$merges" | wc -l | tr -d ' ')
echo "linear-history: $count merge commit(s) reachable from $tip:" >&2
printf '%s\n' "$merges" | head -20 | while read -r sha; do
    git log --no-walk --format='  %h %s' "$sha" >&2
done
[ "$count" -gt 20 ] && echo "  ..." >&2
echo "linear-history: this repository keeps a linear history; rebase the branch instead (git rebase main)" >&2
exit 1
