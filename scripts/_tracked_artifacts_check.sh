#!/bin/sh
# Refuse AI coding-agent working state in the tracked tree.
#
# Agents may keep scratch directories, plans, session journals, and
# work-preservation bundles inside the checkout, but none of it is part of
# the product and none of it is ever committed. .gitignore keeps the known
# names out of `git add`; this script is the backstop for a forced add, a
# renamed directory, or a new tool nobody has ignored yet.
#
# Usage:
#   scripts/_tracked_artifacts_check.sh           # audit every tracked path (make check, CI)
#   scripts/_tracked_artifacts_check.sh staged    # audit the index (pre-commit hook)
#
# The staged mode also refuses any staged path that .gitignore matches (a
# forced add) and any staged blob over MAX_BLOB_BYTES. Set
# ALLOW_LARGE_FILES=1 to commit a large blob on purpose.

set -eu

mode="${1:-tracked}"
max_blob_bytes="${MAX_BLOB_BYTES:-5242880}"
vendor_prefix='src/pve_cpi/vendor/'

# Only the repository's own .gitignore files decide what counts as ignored.
# A developer's global excludes file would otherwise make the verdict differ
# from machine to machine and from CI.
git() { command git -c core.excludesFile=/dev/null "$@"; }

# Tracked paths that .gitignore matches on purpose. The lab environment
# vars.yml files carry lab-only credentials by policy and are tracked even
# though the generic vars.yml rule ignores every other copy.
ignored_allowlist='^manifests/envs/[^/]+/vars\.yml$'

# Path patterns, matched case-insensitively against the repo-relative path.
# Vendored upstream files are exempt: several Go modules ship their own
# AGENTS.md or CLAUDE.md and those belong to the module, not to us.
patterns='(^|/)\.(codex|claude|cursor|aider|gemini|copilot|windsurf|cline|roo|opencode|crush|agents?|superpowers)(/|$)
(^|/)[^/]*work-preservation(/|$)
(^|/)superpowers(/|$)
(^|/)graphify-out(/|$)
(^|/)(CLAUDE|AGENTS|GEMINI)\.md$
(^|/)\.mcp\.json$
(^|/)\.aider[^/]*$'

fail=0

report() {
    echo "tracked-artifacts: $1" >&2
    fail=1
}

case "$mode" in
    tracked)
        paths=$(git ls-files)
        ;;
    staged)
        paths=$(git diff --cached --name-only --diff-filter=ACR)
        ;;
    *)
        echo "usage: $0 [tracked|staged]" >&2
        exit 2
        ;;
esac

[ -z "$paths" ] && exit 0

# 1. Agent-tool names anywhere in the path.
hits=$(printf '%s\n' "$paths" | grep -v "^$vendor_prefix" | grep -E -i "$(printf '%s' "$patterns" | paste -sd '|' -)" || true)
if [ -n "$hits" ]; then
    report "AI agent working state must never be committed:"
    printf '%s\n' "$hits" | sed 's/^/  /' >&2
fi

# 2. Paths that .gitignore matches. In tracked mode that means someone
#    forced an ignored file in; in staged mode it means `git add -f`.
if [ "$mode" = tracked ]; then
    ignored=$(git ls-files -ci --exclude-standard)
else
    ignored=$(printf '%s\n' "$paths" | git check-ignore --no-index --stdin || true)
fi
ignored=$(printf '%s\n' "$ignored" | grep -v "^$vendor_prefix" | grep -E -v "$ignored_allowlist" || true)
if [ -n "$ignored" ]; then
    report "paths matched by .gitignore are in the index (forced add?):"
    printf '%s\n' "$ignored" | sed 's/^/  /' >&2
fi

# 3. Large staged blobs. Bundles, tarballs, and compiled binaries are how
#    agent state usually arrives; the repo's real large objects live in the
#    blobstore, never in git.
if [ "$mode" = staged ] && [ -z "${ALLOW_LARGE_FILES:-}" ]; then
    large=$(printf '%s\n' "$paths" | while IFS= read -r p; do
        size=$(git cat-file -s ":$p" 2>/dev/null || echo 0)
        [ "$size" -gt "$max_blob_bytes" ] && printf '%s (%s bytes)\n' "$p" "$size"
    done || true)
    if [ -n "$large" ]; then
        report "staged files exceed ${max_blob_bytes} bytes (ALLOW_LARGE_FILES=1 to override):"
        printf '%s\n' "$large" | sed 's/^/  /' >&2
    fi
fi

exit $fail
