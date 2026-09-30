#!/bin/sh
# Runs the slow make check gates in three lanes at once.
#
# The test lane runs coverage-check, which runs the race tests and gates on
# their coverage, and its output streams live. The analysis lane runs vet,
# staticcheck, and lint, and the scripts lane runs erb-check and py-test. Each
# background lane stops at its own first failure and logs to a file, and we
# print those logs whole, in a fixed order, once every lane has finished.
# The script exits non-zero when any lane fails.
#
# The Makefile's check target calls this with MAKE set to its own $(MAKE), so
# command-line variables such as REQUIRE_TOOLS=1 reach every lane through
# MAKEFLAGS. CHECK_LANES=0 runs the same gates one after another instead, in
# the order make check used before the lanes existed.
#
# The script sticks to POSIX sh, because the CI container's /bin/sh is dash.

set -u

make_cmd=${MAKE:-make}
lanes=${CHECK_LANES:-1}

case "$lanes" in
0)
	exec "$make_cmd" --no-print-directory vet erb-check py-test staticcheck lint coverage-check
	;;
1) ;;
*)
	echo "CHECK_LANES must be 0 or 1, not '$lanes'" >&2
	exit 2
	;;
esac

tmp=$(mktemp -d "${TMPDIR:-/tmp}/check-lanes.XXXXXX") || exit 1
test_pid=
analysis_pid=
scripts_pid=

# Background jobs of a non-interactive shell ignore SIGINT, and killing a
# lane's make leaves its recipe's go or python process running. So we collect
# every descendant of a lane from one ps snapshot and signal them all.
# shellcheck disable=SC2329 # reached through finish, from the trap
kill_tree() {
	[ -n "$1" ] || return 0
	pids=$(ps -A -o pid= -o ppid= 2>/dev/null | awk -v root="$1" '
		{ parent[$1] = $2 }
		END {
			keep[root] = 1
			grew = 1
			while (grew) {
				grew = 0
				for (p in parent) {
					if (!(p in keep) && (parent[p] in keep)) {
						keep[p] = 1
						grew = 1
					}
				}
			}
			for (p in keep) print p
		}')
	[ -n "$pids" ] || pids=$1
	# shellcheck disable=SC2086 # the pid list splits on purpose
	kill -TERM $pids 2>/dev/null
	return 0
}

# shellcheck disable=SC2329 # invoked by the EXIT trap
finish() {
	status=$?
	trap - EXIT INT TERM
	kill_tree "$test_pid"
	kill_tree "$analysis_pid"
	kill_tree "$scripts_pid"
	rm -rf "$tmp"
	exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"$make_cmd" --no-print-directory vet staticcheck lint >"$tmp/analysis.log" 2>&1 &
analysis_pid=$!
"$make_cmd" --no-print-directory erb-check py-test >"$tmp/scripts.log" 2>&1 &
scripts_pid=$!

echo "==> test lane (race tests with coverage, coverage-check)"
# The test lane runs in the background too, so that a signal reaches our trap
# while we wait instead of after the tests finish.
"$make_cmd" --no-print-directory coverage-check &
test_pid=$!

wait "$test_pid"
test_rc=$?
test_pid=
wait "$analysis_pid"
analysis_rc=$?
analysis_pid=
wait "$scripts_pid"
scripts_rc=$?
scripts_pid=

echo
echo "==> analysis lane (vet, staticcheck, lint)"
cat "$tmp/analysis.log"
echo
echo "==> scripts lane (erb-check, py-test)"
cat "$tmp/scripts.log"

result() {
	if [ "$2" -eq 0 ]; then
		echo "    $1: passed"
	else
		echo "    $1: FAILED (exit $2)"
	fi
}

echo
echo "==> lane results"
result "test lane" "$test_rc"
result "analysis lane" "$analysis_rc"
result "scripts lane" "$scripts_rc"

if [ "$test_rc" -ne 0 ] || [ "$analysis_rc" -ne 0 ] || [ "$scripts_rc" -ne 0 ]; then
	exit 1
fi
exit 0
