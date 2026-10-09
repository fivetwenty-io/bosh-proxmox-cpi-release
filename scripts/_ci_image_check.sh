#!/bin/sh
# Fail when the workflows disagree about the digest-pinned build image.
#
# The build jobs in .github/workflows/*.yml run in one golang container, and
# `make ci` runs the same image locally, reading the reference from ci.yml. A
# workflow that moves to a new digest while another stays behind would make a
# local pass stop predicting a CI pass, so we compare every golang `image:`
# line. Workflows that run a different image, such as the certification
# toolbox, are not build jobs and are left alone.
#
# Usage:
#   scripts/_ci_image_check.sh           # check, exit 1 on drift
#   scripts/_ci_image_check.sh --print   # check, then print the pinned reference

set -eu

dir=${CI_WORKFLOW_DIR:-.github/workflows}

images() {
	sed -n 's/^[[:space:]]*image:[[:space:]]*\(golang:[^[:space:]#]*\).*/\1/p' "$@"
}

refs=$(images "$dir"/*.yml | sort -u)
count=$(printf '%s\n' "$refs" | grep -c . || true)

if [ "$count" -ne 1 ]; then
	echo "ci-image: the workflows pin $count different build images, want exactly one:" >&2
	for f in "$dir"/*.yml; do
		images "$f" | sed "s|^|  $f: |" >&2
	done
	exit 1
fi

case "$refs" in
golang:*@sha256:*) ;;
*)
	echo "ci-image: '$refs' is not a digest-pinned golang image" >&2
	exit 1
	;;
esac

[ "${1:-}" = "--print" ] && printf '%s\n' "$refs"
exit 0
