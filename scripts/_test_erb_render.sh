#!/usr/bin/env bash
# _test_erb_render.sh — render jobs/pve_cpi/templates/cpi.json.erb under three
# credential combinations and assert the resulting JSON has the expected shape:
#
#   1. password-only   → JSON includes "password" key, omits "api_token" key
#   2. api_token-only  → JSON includes "api_token" key, omits "password" key
#   3. both supplied   → JSON includes BOTH keys (validator catches it at run time)
#
# This is a TEMPLATE-LEVEL smoke check; it does NOT exercise the Go validator
# (config.Validate covers that in unit tests). It exists to prove the ERB
# `if_p`-equivalent logic (`unless empty?`) renders the documented JSON shape.
#
# Requires: ruby (any 2.x or 3.x). If ruby is absent the script exits 2 with a
# clear skip message — CI can choose to treat 2 as soft-skip.
#
# Usage: bash scripts/_test_erb_render.sh
# Exit:  0 = all renders + assertions passed
#        1 = an assertion failed
#        2 = ruby not installed (soft skip)

set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SELF_DIR}/.." && pwd)"
ERB_PATH="${REPO_ROOT}/jobs/pve_cpi/templates/cpi.json.erb"

if ! command -v ruby >/dev/null 2>&1; then
  echo "SKIP: ruby is not installed; cannot evaluate ERB template" >&2
  echo "      install ruby (>=2.7) and re-run; exit code 2 documented in script header" >&2
  exit 2
fi

if [ ! -r "${ERB_PATH}" ]; then
  echo "FAIL: cannot read ${ERB_PATH}" >&2
  exit 1
fi

# Render the template under a synthetic property-map. The BOSH job DSL exposes
# `p(name, default=nil)` and `if_p(name) { |v| ... }`. We re-implement that
# minimal surface here so the same .erb file can be rendered standalone.
render() {
  local props_ruby="$1"
  ruby - "$ERB_PATH" "$props_ruby" <<'RUBY'
require 'erb'
require 'json'

erb_path  = ARGV[0]
props_src = ARGV[1]
$props    = eval(props_src) # operator-controlled in this test only

def lookup(key)
  parts = key.split('.')
  cur = $props
  parts.each do |part|
    return [false, nil] unless cur.is_a?(Hash) && cur.key?(part)
    cur = cur[part]
  end
  [true, cur]
end

def p(name, default = :__no_default__)
  found, val = lookup(name)
  return val if found
  return default unless default == :__no_default__
  raise "property #{name} not set and no default supplied"
end

def if_p(*names)
  vals = []
  names.each do |n|
    found, v = lookup(n)
    return unless found
    vals << v
  end
  yield(*vals)
end

template = File.read(erb_path)
# ERB.new signature differs between Ruby 2.5- and 2.6+; tolerate both.
erb = begin
  ERB.new(template, trim_mode: '-')
rescue ArgumentError
  ERB.new(template, nil, '-')
end
print erb.result(binding)
RUBY
}

assert_json_has_key() {
  local label="$1"; local json="$2"; local key="$3"
  if ! printf '%s' "$json" | ruby -rjson -e 'd=JSON.parse(STDIN.read); exit(d.key?(ARGV[0]) ? 0 : 1)' "$key"; then
    echo "FAIL [${label}]: expected key \"${key}\" in JSON output" >&2
    echo "       JSON was: ${json}" >&2
    return 1
  fi
}

assert_json_lacks_key() {
  local label="$1"; local json="$2"; local key="$3"
  if printf '%s' "$json" | ruby -rjson -e 'd=JSON.parse(STDIN.read); exit(d.key?(ARGV[0]) ? 0 : 1)' "$key"; then
    echo "FAIL [${label}]: did not expect key \"${key}\" in JSON output" >&2
    echo "       JSON was: ${json}" >&2
    return 1
  fi
}

assert_json_key_equals() {
  local label="$1"; local json="$2"; local key="$3"; local want="$4"
  local got
  got="$(printf '%s' "$json" | ruby -rjson -e 'd=JSON.parse(STDIN.read); print d.fetch(ARGV[0], "<absent>")' "$key")"
  if [ "$got" != "$want" ]; then
    echo "FAIL [${label}]: key \"${key}\" is $(printf %q "$got"), expected $(printf %q "$want")" >&2
    echo "       JSON was: ${json}" >&2
    return 1
  fi
}

assert_json_valid() {
  local label="$1"; local json="$2"
  if ! printf '%s' "$json" | ruby -rjson -e 'JSON.parse(STDIN.read)' >/dev/null 2>&1; then
    echo "FAIL [${label}]: rendered output is not valid JSON" >&2
    echo "       output was: ${json}" >&2
    return 1
  fi
}

# Base property bag shared across all three cases. Mirrors spec defaults so the
# render does not raise "property not set". Only the auth-credential pair varies.
BASE_PROPS='{
  "pve" => {
    "host" => "pve.example.com", "port" => 8006, "user" => "root@pam",
    "realm" => "pam", "node" => "pve1",
    "vm_storage" => "local-lvm", "disk_storage" => "local-lvm",
    "stemcell_storage" => "local", "iso_storage" => "local",
    "stemcell_strategy" => "template",
    "network_bridge" => "vmbr0", "network_mode" => "sdn",
    "sdn_zone" => "", "sdn_zone_type" => "vxlan", "sdn_auto_manage_zone" => true,
    "verify_ssl" => true, "agent_mode" => "cloudinit",
    "vm_disk_format" => "qcow2", "log_level" => "info",
    "vmid_range_start" => 100, "vmid_range_end" => 5999,
    "allow_disk_ops_with_snapshots" => false,
    "require_snapshot_check_pass" => false,
    "hotplug" => "network,disk,cpu,memory", "numa" => true,
    "reboot_mode" => "soft", "reboot_timeout" => 60,
    "vm_prefix" => "", "create_env_deployment" => "create-env",
    "parker_prefix" => "", "parker_pool" => "{prefix}-parker"
  }
}'

# Helper: install a specific (password, api_token) pair into the base map.
build_props() {
  local pw="$1"; local tok="$2"
  printf '%s' "$BASE_PROPS" \
    | ruby -e '
        src = STDIN.read
        h = eval(src)
        h["pve"]["password"]  = ARGV[0]
        h["pve"]["api_token"] = ARGV[1]
        print h.inspect
      ' "$pw" "$tok"
}

# Storage-placement round-trip fixture consumed by Go integration tests.
# The normal smoke-test invocation still exercises the original three cases.
if [ "${1:-}" = "--storage-placement-fixture" ]; then
  fixture_base="$(build_props 'fixture-password' '')"
  fixture_props="$(printf '%s' "$fixture_base" | ruby -rjson -e '
    h = eval(STDIN.read)
    h["pve"].merge!(JSON.parse(ARGV[0]))
    print h.inspect
  ' "$2")"
  render "$fixture_props"
  exit 0
fi

FAILED=0

# --- Case 1: password-only ---------------------------------------------------
echo "Case 1: password-only"
PROPS_1="$(build_props 'secret-pw' '')"
JSON_1="$(render "$PROPS_1")"
assert_json_valid     "case1" "$JSON_1"           || FAILED=$((FAILED+1))
assert_json_has_key   "case1" "$JSON_1" password  || FAILED=$((FAILED+1))
assert_json_lacks_key "case1" "$JSON_1" api_token || FAILED=$((FAILED+1))

# --- Case 2: api_token-only --------------------------------------------------
echo "Case 2: api_token-only"
PROPS_2="$(build_props '' 'root@pam!cpi=abc-123')"
JSON_2="$(render "$PROPS_2")"
assert_json_valid     "case2" "$JSON_2"            || FAILED=$((FAILED+1))
assert_json_has_key   "case2" "$JSON_2" api_token  || FAILED=$((FAILED+1))
assert_json_lacks_key "case2" "$JSON_2" password   || FAILED=$((FAILED+1))

# --- Case 3: both set (validator must catch later) ---------------------------
echo "Case 3: both supplied"
PROPS_3="$(build_props 'secret-pw' 'root@pam!cpi=abc-123')"
JSON_3="$(render "$PROPS_3")"
assert_json_valid   "case3" "$JSON_3"             || FAILED=$((FAILED+1))
assert_json_has_key "case3" "$JSON_3" password    || FAILED=$((FAILED+1))
assert_json_has_key "case3" "$JSON_3" api_token   || FAILED=$((FAILED+1))

# --- Case 4: parker_prefix / parker_pool idioms ------------------------------
echo "Case 4: parker prefix/pool"

# 4a: a set parker_prefix reaches the rendered config (the omit-when-empty
# idiom passes a non-empty value through unchanged).
PROPS_4A="$(printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
  h = eval(STDIN.read)
  h["pve"]["parker_prefix"] = "cpi"
  print h.inspect
')"
JSON_4A="$(render "$PROPS_4A")"
assert_json_valid    "case4a" "$JSON_4A"               || FAILED=$((FAILED+1))
assert_json_has_key  "case4a" "$JSON_4A" parker_prefix || FAILED=$((FAILED+1))

# 4b: an empty parker_prefix (the BASE_PROPS default) does not reach the
# rendered config; absent and empty mean the same thing for the prefix.
assert_json_lacks_key "case4b" "$JSON_1" parker_prefix || FAILED=$((FAILED+1))

# 4c: an empty parker_pool still reaches the rendered config as an explicit
# empty string, because that is the whole "" opt-out for pool assignment
# and the always-emit idiom must not collapse it away.
PROPS_4C="$(printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
  h = eval(STDIN.read)
  h["pve"]["parker_pool"] = ""
  print h.inspect
')"
JSON_4C="$(render "$PROPS_4C")"
assert_json_valid   "case4c" "$JSON_4C"             || FAILED=$((FAILED+1))
assert_json_has_key "case4c" "$JSON_4C" parker_pool || FAILED=$((FAILED+1))

# 4d: with pve.parker_pool never set, the rendered config carries the release
# default, and that default is the same string the job spec declares. The ERB
# spells its own inline default so a stub property map still renders, so the two
# can drift apart without anything else noticing; this case is what notices.
SPEC_PARKER_POOL="$(ruby -ryaml -e '
  spec = YAML.load_file(ARGV[0])
  print spec.fetch("properties").fetch("pve.parker_pool").fetch("default")
' "${REPO_ROOT}/jobs/pve_cpi/spec")"
if [ "$SPEC_PARKER_POOL" != "{prefix}-parker" ]; then
  echo "FAIL [case4d]: the spec default for pve.parker_pool is $(printf %q "$SPEC_PARKER_POOL"), expected '{prefix}-parker'" >&2
  FAILED=$((FAILED+1))
fi
PROPS_4D="$(printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
  h = eval(STDIN.read)
  h["pve"].delete("parker_pool")
  print h.inspect
')"
JSON_4D="$(render "$PROPS_4D")"
assert_json_valid       "case4d" "$JSON_4D"                                  || FAILED=$((FAILED+1))
assert_json_key_equals  "case4d" "$JSON_4D" parker_pool "$SPEC_PARKER_POOL"  || FAILED=$((FAILED+1))

# --- Case 5: stemcell_replicate_storage_set tri-state -------------------------
# The key must round-trip both explicit values and stay absent when unset, so
# the Go side can tell "operator said false" from "operator said nothing" and
# apply its default-on resolution only to the latter.
echo "Case 5: stemcell_replicate_storage_set"
props_with_replicate_set() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
    h = eval(STDIN.read)
    h["pve"]["stemcell_replicate_storage_set"] = (ARGV[0] == "true")
    print h.inspect
  ' "$1"
}
JSON_5="$(render "$(props_with_replicate_set true)")"
assert_json_valid      "case5" "$JSON_5"                                       || FAILED=$((FAILED+1))
assert_json_key_equals "case5" "$JSON_5" stemcell_replicate_storage_set true    || FAILED=$((FAILED+1))

JSON_5F="$(render "$(props_with_replicate_set false)")"
assert_json_valid      "case5-false" "$JSON_5F"                                      || FAILED=$((FAILED+1))
assert_json_key_equals "case5-false" "$JSON_5F" stemcell_replicate_storage_set false  || FAILED=$((FAILED+1))

assert_json_lacks_key "case5-unset" "$JSON_1" stemcell_replicate_storage_set || FAILED=$((FAILED+1))

# Omitted new placement properties must not alter legacy JSON.
for key in storage_sets storage_capacity_domains ephemeral_storage_set \
  persistent_storage_set root_storage_set storage_placement_namespace \
  storage_allocation_journal_dir require_disjoint_storage_sets \
  storage_status_max_age_seconds; do
  assert_json_lacks_key "legacy-placement" "$JSON_1" "$key" || FAILED=$((FAILED+1))
done

if [ "$FAILED" -ne 0 ]; then
  echo "RESULT: ${FAILED} assertion(s) FAILED" >&2
  exit 1
fi

echo "RESULT: all 3 cred combinations rendered and asserted clean"
exit 0
