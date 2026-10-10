#!/bin/sh
# Run the CI workflows' checks locally, in the workflows' own container image.
#
# `make ci` calls this. It mirrors what CI runs on a push:
#
#   - the Linear history job's checks, which are the merge-commit check and the
#     AI-attribution check, both run against the pushed shas;
#   - the Check job's `make check REQUIRE_TOOLS=1`;
#   - the Security job's `make security REQUIRE_TOOLS=1`, but only when the
#     pushed range touches Go source, go.mod, go.sum, vendored code, a
#     package.json or package-lock.json, a Dockerfile, the security workflow,
#     or the root Makefile. Set CI_SECURITY=1 to force the scans and CI_SECURITY=0 to skip them.
#
# The image reference comes from .github/workflows/ci.yml, so there is one
# source of truth (scripts/_ci_image_check.sh fails when the other workflows
# drift from it). We build a thin local layer on that digest that adds only
# what the workflow steps install before they run make: python3, PyYAML, ruby,
# and the pinned versions of staticcheck, golangci-lint, govulncheck, gosec,
# and trivy, each read from the workflow that installs it.
#
# The Go module cache, the Go build cache, and the trivy database live in one
# named Docker volume, never in a host directory. `docker volume rm
# bosh-proxmox-cpi-ci-cache` starts them cold.
#
# The pushed range arrives in CI_RANGES, one "<remote sha> <local sha>" pair
# per line, which is the pre-push hook's stdin. With no pairs we compare HEAD
# with its upstream, or with origin/main when it has none.
#
# The worktree is mounted as it stands, so make check and the security scans
# test the checked-out tree, uncommitted changes included, while the history
# checks cover the pushed commits.
#
# POSIX sh only, because the container's /bin/sh is dash.

set -eu

die() {
	echo "ci: $*" >&2
	exit 1
}

root=$(git rev-parse --show-toplevel) || die "not inside a git checkout"
cd "$root"

if ! command -v docker >/dev/null 2>&1; then
	die "docker is not installed; make ci runs CI's checks inside CI's container image (SKIP_CHECKS=1 git push bypasses the hook)"
fi
if ! docker info >/dev/null 2>&1; then
	die "the Docker daemon is not running; start Docker, then rerun make ci (SKIP_CHECKS=1 git push bypasses the hook)"
fi

sh scripts/_ci_image_check.sh || exit 1
image=$(sh scripts/_ci_image_check.sh --print)

ci_yml=.github/workflows/ci.yml
sec_yml=.github/workflows/security.yml

# version FILE REGEX prints the version after @ on the go install line that
# matches REGEX.
version() {
	sed -n "s|.*go install $2@\\(v[^[:space:]]*\\).*|\\1|p" "$1" | head -1
}

staticcheck_v=$(version "$ci_yml" 'honnef.co/go/tools/cmd/staticcheck')
golangci_v=$(version "$ci_yml" 'github.com/golangci/golangci-lint/v2/cmd/golangci-lint')
govulncheck_v=$(version "$sec_yml" 'golang.org/x/vuln/cmd/govulncheck')
gosec_v=$(version "$sec_yml" 'github.com/securego/gosec/v2/cmd/gosec')
trivy_v=$(sed -n 's/^[[:space:]]*version:[[:space:]]*\(v[0-9][^[:space:]]*\).*/\1/p' "$sec_yml" | head -1)
gomax=$(sed -n 's/^[[:space:]]*GOMAXPROCS:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$ci_yml" | head -1)

for v in "$staticcheck_v" "$golangci_v" "$govulncheck_v" "$gosec_v" "$trivy_v" "$gomax"; do
	[ -n "$v" ] || die "could not read a pinned tool version from the workflows"
done

# Work out what is being pushed. Each pair becomes an attribution range and a
# changed-file list for the security trigger.
zero=0000000000000000000000000000000000000000
ranges=${CI_RANGES:-}
if [ -z "$ranges" ]; then
	up=$(git rev-parse --verify --quiet '@{upstream}' 2>/dev/null || git rev-parse --verify --quiet origin/main 2>/dev/null || echo "$zero")
	ranges="$up $(git rev-parse HEAD)"
fi

attribution=""
pushed=""
touches_go=0
saw_range=0
while read -r remote_sha local_sha _; do
	[ -n "${local_sha:-}" ] || continue
	[ "$local_sha" = "$zero" ] && continue # a branch deletion pushes no commits
	base=$remote_sha
	if [ "$base" = "$zero" ] || ! git cat-file -e "$base^{commit}" 2>/dev/null; then
		base=$(git merge-base "$local_sha" origin/main 2>/dev/null || echo "")
	fi
	if [ -z "$base" ]; then
		saw_range=1
		pushed="$pushed $local_sha"
		attribution="$attribution $local_sha"
		touches_go=1 # no base to diff against, so scan
		continue
	fi
	# A ref that adds no commits, such as a tag on a commit origin/main already
	# has, pushes nothing for CI to reject.
	if [ -z "$(git rev-list -n1 "$base..$local_sha")" ]; then
		continue
	fi
	saw_range=1
	pushed="$pushed $local_sha"
	attribution="$attribution $base..$local_sha"
	if git diff --name-only "$base" "$local_sha" | grep -Eq '\.go$|(^|/)go\.(mod|sum)$|(^|/)vendor/|(^|/)package(-lock)?\.json$|(^|/)Dockerfile|^\.github/workflows/security\.yml$|^Makefile$'; then
		touches_go=1
	fi
done <<EOF
$ranges
EOF

if [ "$saw_range" -eq 0 ]; then
	echo "ci: nothing to push, so there is nothing to check"
	exit 0
fi

case "${CI_SECURITY:-auto}" in
1)
	security=1
	echo "ci: security scans forced on (CI_SECURITY=1)"
	;;
0)
	security=0
	echo "ci: security scans skipped (CI_SECURITY=0)"
	;;
auto)
	security=$touches_go
	if [ "$security" -eq 1 ]; then
		echo "ci: the pushed range changes Go source, dependencies, or the security setup, so the security scans run"
	else
		echo "ci: skipping the security scans, since the pushed range changes no Go source, dependency files, or security setup (CI_SECURITY=1 forces them)"
	fi
	;;
*) die "CI_SECURITY must be 1, 0, or auto, not '${CI_SECURITY}'" ;;
esac

dockerfile=$(cat <<'DOCKERFILE'
ARG BASE
FROM ${BASE}
ARG STATICCHECK
ARG GOLANGCI
ARG GOVULNCHECK
ARG GOSEC
ARG TRIVY
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends python3 python3-yaml ruby \
 && rm -rf /var/lib/apt/lists/*
RUN GOBIN=/usr/local/bin go install honnef.co/go/tools/cmd/staticcheck@${STATICCHECK} \
 && GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI} \
 && GOBIN=/usr/local/bin go install golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK} \
 && GOBIN=/usr/local/bin go install github.com/securego/gosec/v2/cmd/gosec@${GOSEC} \
 && go clean -cache -modcache
RUN arch=$(dpkg --print-architecture) \
 && if [ "$arch" = amd64 ]; then asset=64bit; elif [ "$arch" = arm64 ]; then asset=ARM64; else echo "no trivy build for $arch" >&2; exit 1; fi \
 && curl -fsSL "https://github.com/aquasecurity/trivy/releases/download/${TRIVY}/trivy_${TRIVY#v}_Linux-${asset}.tar.gz" \
    | tar -xz -C /usr/local/bin trivy
DOCKERFILE
)

# Build the local layer once per image digest, tool-version set, and Dockerfile.
tag="bosh-proxmox-cpi-ci:$(printf '%s %s %s %s %s %s\n%s' "$image" "$staticcheck_v" "$golangci_v" "$govulncheck_v" "$gosec_v" "$trivy_v" "$dockerfile" | shasum -a 256 | cut -c1-12)"
if ! docker image inspect "$tag" >/dev/null 2>&1; then
	echo "ci: building $tag from $image (one time per digest, tool-version set, and Dockerfile)"
	printf '%s\n' "$dockerfile" | docker build --quiet -t "$tag" \
		--build-arg "BASE=$image" \
		--build-arg "STATICCHECK=$staticcheck_v" \
		--build-arg "GOLANGCI=$golangci_v" \
		--build-arg "GOVULNCHECK=$govulncheck_v" \
		--build-arg "GOSEC=$gosec_v" \
		--build-arg "TRIVY=$trivy_v" \
		- >/dev/null || die "building the local CI image failed"
fi

uid=$(id -u)
gid=$(id -g)
volume=bosh-proxmox-cpi-ci-cache
docker volume create "$volume" >/dev/null

# A fresh volume belongs to root; hand it to the invoking user so the checks
# do not write root-owned files into the worktree.
docker run --rm -u 0 -v "$volume:/cache" "$tag" \
	sh -c 'if [ "$(stat -c %u /cache)" != "$1" ]; then chown "$1:$2" /cache; fi' sh "$uid" "$gid"

# A linked worktree's .git file points at the main repository's git directory
# by absolute path, so mount that directory at the same path.
common=$(cd "$(git rev-parse --git-common-dir)" && pwd -P)
mounts="-v $root:$root"
case "$common/" in
"$root"/*) ;;
*) mounts="$mounts -v $common:$common" ;;
esac

echo "ci: running in $image (GOMAXPROCS=$gomax)"
# shellcheck disable=SC2086 # $mounts is a fixed list of -v flags
exec docker run --rm --init -u "$uid:$gid" $mounts -w "$root" \
	-v "$volume:/cache" \
	-e HOME=/cache/home -e GOPATH=/cache/go -e GOCACHE=/cache/go-build \
	-e XDG_CACHE_HOME=/cache/xdg -e GOMAXPROCS="$gomax" -e CI=true \
	-e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e "GIT_CONFIG_VALUE_0=*" \
	-e "CI_ATTRIBUTION_RANGES=$attribution" -e "CI_PUSHED_SHAS=$pushed" -e "CI_RUN_SECURITY=$security" \
	"$tag" sh -ec '
mkdir -p "$HOME" "$GOPATH" "$GOCACHE" "$XDG_CACHE_HOME"

echo "==> Toolchain: go.mod must not need a newer Go than this image"
have=$(go env GOVERSION | sed "s/^go//")
for mod in $(git ls-files "go.mod" "*/go.mod"); do
	want=$(sed -n "s/^go[[:space:]][[:space:]]*\\([0-9][0-9.]*\\).*/\\1/p" "$mod" | head -1)
	[ -n "$want" ] || continue
	newest=$(printf "%s\\n%s\\n" "$want" "$have" | sort -V | tail -1)
	if [ "$newest" != "$have" ]; then
		echo "ci: $mod needs Go $want but the pinned image has Go $have; bump the image digest first (the image sets GOTOOLCHAIN=local, while CI setup-go would install $want)" >&2
		exit 1
	fi
done

echo "==> Linear history job: refuse merge commits in the pushed commits"
for sha in $CI_PUSHED_SHAS; do
	sh scripts/_linear_history_check.sh "$sha"
done

echo "==> Linear history job: refuse AI attribution in the pushed commits"
for range in $CI_ATTRIBUTION_RANGES; do
	sh scripts/_attribution_check.sh "$range"
done

echo "==> Check job: make check REQUIRE_TOOLS=1"
make check REQUIRE_TOOLS=1

if [ "$CI_RUN_SECURITY" = "1" ]; then
	echo "==> Security job: make security REQUIRE_TOOLS=1"
	make security REQUIRE_TOOLS=1
fi

echo "ci: all CI checks passed"
'
