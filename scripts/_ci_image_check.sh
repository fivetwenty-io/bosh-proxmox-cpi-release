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
# The Concourse tasks in ci/tasks/*.yml pin the same golang digest in a
# `digest:` line, and every staticcheck and golangci-lint pin must agree across
# the workflows and the tasks.
#
# Usage:
#   scripts/_ci_image_check.sh           # check, exit 1 on drift
#   scripts/_ci_image_check.sh --print   # check, then print the pinned reference

set -eu

dir=${CI_WORKFLOW_DIR:-.github/workflows}
tasks=${CI_TASK_DIR:-ci/tasks}

images() {
	sed -n "s/^[[:space:]-]*image:[[:space:]]*[\"']\{0,1\}\(golang:[^[:space:]#\"']*\).*/\1/p" "$@"
}

# Every uncommented line that mentions golang: must have matched the parser
# above. A line it missed, such as a differently shaped key, would otherwise
# slip past the drift check.
for f in "$dir"/*.yml; do
	total=$(grep -Ev '^[[:space:]]*#' "$f" | grep -c 'golang:' || true)
	parsed=$(images "$f" | grep -c . || true)
	if [ "$total" -ne "$parsed" ]; then
		echo "ci-image: $f has a golang: image line that the checker could not parse:" >&2
		grep -Ev '^[[:space:]]*#' "$f" | grep 'golang:' >&2
		exit 1
	fi
done

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

want_digest=${refs#*@}

# task_digests FILE prints the digest: value of a task whose image_resource is
# the golang repository. Tasks on other images, such as ubuntu, print nothing.
task_digests() {
	awk '
		/^[[:space:]]*repository:[[:space:]]*["'"'"']?golang["'"'"']?[[:space:]]*$/ { g = 1; next }
		g && /^[[:space:]]*digest:/ {
			v = $0
			sub(/^[[:space:]]*digest:[[:space:]]*["'"'"']?/, "", v)
			sub(/["'"'"']?[[:space:]]*(#.*)?$/, "", v)
			print v
			g = 0
		}
	' "$1"
}

for f in "$tasks"/*.yml; do
	[ -f "$f" ] || continue
	if grep -Eq '^[[:space:]]*repository:[[:space:]]*["'"'"']?golang["'"'"']?[[:space:]]*$' "$f"; then
		got=$(task_digests "$f")
		if [ "$got" != "$want_digest" ]; then
			echo "ci-image: $f pins golang digest '${got:-none}', but ci.yml pins '$want_digest'" >&2
			exit 1
		fi
	fi
done

# Every staticcheck and golangci-lint version pin must match across the
# workflows and the tasks.
for tool in staticcheck golangci-lint; do
	pins=$(grep -Eho "cmd/$tool@[^[:space:]\"']+" "$dir"/*.yml "$tasks"/*.yml 2>/dev/null | sort -u)
	n=$(printf '%s\n' "$pins" | grep -c . || true)
	if [ "$n" -gt 1 ]; then
		echo "ci-image: $tool is pinned to $n different versions, want one:" >&2
		grep -Hn "cmd/$tool@" "$dir"/*.yml "$tasks"/*.yml >&2
		exit 1
	fi
done

[ "${1:-}" = "--print" ] && printf '%s\n' "$refs"
exit 0
