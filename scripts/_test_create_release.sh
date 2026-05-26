#!/usr/bin/env bash
# _test_create_release.sh — self-contained smoke test for the secret guard
# embedded in scripts/create-release.
#
# Extracts ONLY the guard logic (no bosh CLI, no make, no real git tags) into a
# standalone function, then exercises it against three scenarios:
#
#   1. creds.yml staged              → must exit non-zero (guard fires)
#   2. creds.yml staged + ALLOW=1   → must exit 0      (bypass with banner)
#   3. release-notes.yml staged      → must exit 0      (no pattern match)
#
# Usage: bash scripts/_test_create_release.sh
# Exit:  0 = all assertions passed, non-zero = failure

set -euo pipefail

# Capture the real script directory NOW, before we cd into the tmp repo.
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CREATE_RELEASE_SCRIPT="${SELF_DIR}/create-release"

# ---- helpers -----------------------------------------------------------------

RED='\033[1;31m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
RESET='\033[0m'

PASS=0
FAIL=0

assert_exit() {
  local label="$1"
  local expected_exit="$2"
  local actual_exit="$3"
  if [[ "${actual_exit}" == "${expected_exit}" ]]; then
    echo -e "${GREEN}PASS${RESET} ${label} (exit=${actual_exit})"
    PASS=$(( PASS + 1 ))
  else
    echo -e "${RED}FAIL${RESET} ${label} — expected exit ${expected_exit}, got ${actual_exit}"
    FAIL=$(( FAIL + 1 ))
  fi
}

assert_output_contains() {
  local label="$1"
  local needle="$2"
  local haystack="$3"
  if echo "${haystack}" | grep -qF "${needle}"; then
    echo -e "${GREEN}PASS${RESET} ${label} (output contains '${needle}')"
    PASS=$(( PASS + 1 ))
  else
    echo -e "${RED}FAIL${RESET} ${label} — output did not contain '${needle}'"
    echo "  actual output: ${haystack}"
    FAIL=$(( FAIL + 1 ))
  fi
}

# ---- secret guard function (extracted verbatim from create-release logic) ----
#
# Parameters: $1 = ALLOW_SECRET_COMMIT value (0 or 1)
#             stdin not used; function reads STAGED_SECRETS from caller scope.
run_guard() {
  local allow="${1:-0}"
  local staged_secrets="$2"

  if [[ -n "${staged_secrets}" ]]; then
    if [[ "${allow}" == "1" ]]; then
      echo "WARNING: ALLOW_SECRET_COMMIT=1 — bypassing secret guard."
      echo "Files: ${staged_secrets}"
      return 0
    else
      echo "ERROR: Secret guard blocked the commit." >&2
      echo "Staged credential files: ${staged_secrets}" >&2
      echo "Re-run with ALLOW_SECRET_COMMIT=1 to bypass." >&2
      return 1
    fi
  fi
  return 0
}

# ---- tmp git repo setup -------------------------------------------------------

TMPDIR_BASE="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_BASE}"' EXIT

TESTREPO="${TMPDIR_BASE}/testrepo"
mkdir -p "${TESTREPO}"
cd "${TESTREPO}"
git init -q
git config user.email "test@example.com"
git config user.name "Test"

# Create an initial commit so HEAD exists (required for git diff --cached).
touch README
git add README
git commit -q -m "init"

# ---- scenario helpers --------------------------------------------------------

stage_file() {
  local filename="$1"
  local content="${2:-# fake content — not a real credential}"
  mkdir -p "$(dirname "${filename}")"
  echo "${content}" > "${filename}"
  git add "${filename}"
}

get_staged_secrets() {
  git diff --cached --name-only | grep -E '(creds|vars|secret)\.yml$' || true
}

reset_index() {
  # Unstage everything without touching working tree.
  git reset -q HEAD -- . 2>/dev/null || true
  # Remove any untracked staged files left in working tree.
  git clean -qf
}

# ---- test 1: creds.yml staged → guard must fire (exit non-zero) --------------

echo ""
echo "==> Test 1: creds.yml staged, no bypass flag"

stage_file "manifests/bosh/creds.yml"
STAGED="$(get_staged_secrets)"

set +e
output="$(run_guard "0" "${STAGED}" 2>&1)"
exit_code=$?
set -e

assert_exit "T1 exit non-zero" "1" "${exit_code}"
assert_output_contains "T1 error message present" "Secret guard blocked" "${output}"

reset_index

# ---- test 2: creds.yml staged + ALLOW_SECRET_COMMIT=1 → must exit 0 ---------

echo ""
echo "==> Test 2: creds.yml staged, ALLOW_SECRET_COMMIT=1"

stage_file "manifests/bosh/creds.yml"
STAGED="$(get_staged_secrets)"

set +e
output="$(run_guard "1" "${STAGED}" 2>&1)"
exit_code=$?
set -e

assert_exit "T2 exit 0 (bypass)" "0" "${exit_code}"
assert_output_contains "T2 bypass banner present" "ALLOW_SECRET_COMMIT=1" "${output}"

reset_index

# ---- test 3: vars.yml staged → guard must fire ------------------------------

echo ""
echo "==> Test 3: vars.yml staged, no bypass flag"

stage_file "manifests/bosh/vars.yml"
STAGED="$(get_staged_secrets)"

set +e
output="$(run_guard "0" "${STAGED}" 2>&1)"
exit_code=$?
set -e

assert_exit "T3 exit non-zero (vars.yml)" "1" "${exit_code}"
assert_output_contains "T3 error message present" "Secret guard blocked" "${output}"

reset_index

# ---- test 4: release-notes.yml staged → guard must NOT fire -----------------

echo ""
echo "==> Test 4: release-notes.yml staged (no pattern match)"

stage_file "release-notes.yml"
STAGED="$(get_staged_secrets)"

set +e
output="$(run_guard "0" "${STAGED}" 2>&1)"
exit_code=$?
set -e

assert_exit "T4 exit 0 (no match)" "0" "${exit_code}"

reset_index

# ---- test 5: secret.yml staged → guard must fire ----------------------------

echo ""
echo "==> Test 5: secret.yml staged (pattern match on 'secret')"

stage_file "config/secret.yml"
STAGED="$(get_staged_secrets)"

set +e
output="$(run_guard "0" "${STAGED}" 2>&1)"
exit_code=$?
set -e

assert_exit "T5 exit non-zero (secret.yml)" "1" "${exit_code}"

reset_index

# ---- test 6: syntax check scripts/create-release ----------------------------

echo ""
echo "==> Test 6: bash -n syntax check on scripts/create-release"

set +e
bash -n "${CREATE_RELEASE_SCRIPT}" 2>&1
syntax_exit=$?
set -e

assert_exit "T6 bash -n exit 0" "0" "${syntax_exit}"

# ---- summary -----------------------------------------------------------------

echo ""
echo "============================================================"
echo "Results: ${PASS} passed, ${FAIL} failed"
echo "============================================================"

if [[ "${FAIL}" -gt 0 ]]; then
  exit 1
fi

exit 0
