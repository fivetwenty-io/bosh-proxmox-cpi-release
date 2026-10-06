#!/bin/sh
# Refuse AI attribution in commit messages.
#
# Commit messages in this repository never name an AI tool as an author:
# no co-author trailer, no noreply address, and no "generated with" footer
# that names Claude or Anthropic. A human co-author line passes.
#
# Usage:
#   scripts/_attribution_check.sh [<commit>]   # every commit reachable from <commit>, default HEAD
#   scripts/_attribution_check.sh msg <file>   # one message file (the commit-msg hook)
#
# The history mode walks every reachable commit, so like the linear-history
# check it needs the full commit graph and passes vacuously on the commits a
# shallow clone cannot see.

set -eu

pattern='co-authored-by:.*(claude|anthropic)|noreply@anthropic\.com|generated (with|by) \[?claude'

if [ "${1:-}" = "msg" ]; then
    # Comment lines are stripped by git before the commit is written.
    if grep -v '^#' "$2" | grep -qiE "$pattern"; then
        echo "attribution: the commit message names an AI tool as an author; remove that line" >&2
        exit 1
    fi
    exit 0
fi

tip="${1:-HEAD}"

found=$(git log -i -E --grep="$pattern" --format='%h %s' "$tip")
[ -z "$found" ] && exit 0

count=$(printf '%s\n' "$found" | wc -l | tr -d ' ')
echo "attribution: $count commit(s) reachable from $tip carry AI attribution:" >&2
printf '%s\n' "$found" | head -20 | sed 's/^/  /' >&2
[ "$count" -gt 20 ] && echo "  ..." >&2
echo "attribution: reword those commits to drop the attribution line" >&2
exit 1
