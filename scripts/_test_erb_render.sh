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
#
# An optional second argument is the job spec as JSON. When it is given, the
# template sees `spec` the way a BOSH renderer exposes it: nested objects whose
# attributes read as methods and whose missing attributes read as nil. When it
# is absent, `spec` is undefined, which is the harshest case a renderer could
# hand the template.
render() {
  local props_ruby="$1"
  local spec_json="${2:-}"
  ruby - "$ERB_PATH" "$props_ruby" "$spec_json" <<'RUBY'
require 'erb'
require 'json'

erb_path  = ARGV[0]
props_src = ARGV[1]
spec_src  = ARGV[2].to_s
$props    = eval(props_src) # operator-controlled in this test only

# SpecNode mirrors the OpenStruct a BOSH renderer wraps the spec in.
class SpecNode
  def initialize(hash)
    @hash = hash
  end

  def method_missing(name, *args)
    return super unless args.empty?
    v = @hash[name.to_s]
    v.is_a?(Hash) ? SpecNode.new(v) : v
  end

  def respond_to_missing?(name, include_private = false)
    @hash.key?(name.to_s) || super
  end
end

unless spec_src.empty?
  $spec = SpecNode.new(JSON.parse(spec_src))
  def spec
    $spec
  end
end

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

# placement.az_enforcement has no spec default: it renders only when the
# operator sets it, and an unset config carries no placement block at all.
echo "Case 6: placement.az_enforcement"
props_with_az_enforcement() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
    h = eval(STDIN.read)
    h["pve"]["placement"] = { "az_enforcement" => ARGV[0] }
    print h.inspect
  ' "$1"
}
JSON_6="$(render "$(props_with_az_enforcement strict)")"
assert_json_valid "case6" "$JSON_6" || FAILED=$((FAILED+1))
if [ "$(printf '%s' "$JSON_6" | ruby -rjson -e 'print JSON.parse(STDIN.read).fetch("placement", {}).fetch("az_enforcement", "<absent>")')" != "strict" ]; then
  echo "FAIL [case6]: placement.az_enforcement did not render as strict" >&2
  FAILED=$((FAILED+1))
fi
assert_json_lacks_key "case6-unset" "$JSON_1" placement || FAILED=$((FAILED+1))

echo "Case 7: placement.bridge_check"
props_with_bridge_check() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
    h = eval(STDIN.read)
    h["pve"]["placement"] = { "bridge_check" => ARGV[0] }
    print h.inspect
  ' "$1"
}
JSON_7="$(render "$(props_with_bridge_check enforce)")"
assert_json_valid "case7" "$JSON_7" || FAILED=$((FAILED+1))
if [ "$(printf '%s' "$JSON_7" | ruby -rjson -e 'print JSON.parse(STDIN.read).fetch("placement", {}).fetch("bridge_check", "<absent>")')" != "enforce" ]; then
  echo "FAIL [case7]: placement.bridge_check did not render as enforce" >&2
  FAILED=$((FAILED+1))
fi
assert_json_lacks_key "case7-unset" "$JSON_1" placement || FAILED=$((FAILED+1))
# With other placement keys set and bridge_check unset, the template must not
# emit bridge_check next to them.
props_with_other_placement() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
    h = eval(STDIN.read)
    h["pve"]["placement"] = { "fallback_max" => 1 }
    print h.inspect
  '
}
JSON_7B="$(render "$(props_with_other_placement)")"
assert_json_valid "case7-other" "$JSON_7B" || FAILED=$((FAILED+1))
if [ "$(printf '%s' "$JSON_7B" | ruby -rjson -e 'print JSON.parse(STDIN.read).fetch("placement", {}).fetch("bridge_check", "<absent>")')" != "<absent>" ]; then
  echo "FAIL [case7]: placement.bridge_check was emitted although it was unset" >&2
  FAILED=$((FAILED+1))
fi
# vm_ownership_guard renders only when set, so the Go default ("enforce")
# decides for a manifest that says nothing.
echo "Case 8: vm_ownership_guard"
props_with_ownership_guard() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
    h = eval(STDIN.read)
    h["pve"]["vm_ownership_guard"] = ARGV[0]
    print h.inspect
  ' "$1"
}
JSON_8="$(render "$(props_with_ownership_guard warn)")"
assert_json_valid      "case8" "$JSON_8"                         || FAILED=$((FAILED+1))
assert_json_key_equals "case8" "$JSON_8" vm_ownership_guard warn || FAILED=$((FAILED+1))
assert_json_lacks_key  "case8-unset" "$JSON_1" vm_ownership_guard || FAILED=$((FAILED+1))

# vm_cid_format renders only when set, so the Go default ("stamped") decides
# for a manifest that says nothing. cluster_stamp_aliases renders only when
# the list names a stamp, and it renders as a JSON list, not a string.
echo "Case 9: vm_cid_format and cluster_stamp_aliases"
props_with_cid_settings() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -rjson -e '
    h = eval(STDIN.read)
    h["pve"]["vm_cid_format"] = ARGV[0]
    h["pve"]["cluster_stamp_aliases"] = JSON.parse(ARGV[1])
    print h.inspect
  ' "$1" "$2"
}
JSON_9="$(render "$(props_with_cid_settings bare '["0a1b2c3d","4e5f6a7b"]')")"
assert_json_valid      "case9" "$JSON_9"                    || FAILED=$((FAILED+1))
assert_json_key_equals "case9" "$JSON_9" vm_cid_format bare || FAILED=$((FAILED+1))
if [ "$(printf '%s' "$JSON_9" | ruby -rjson -e 'print JSON.generate(JSON.parse(STDIN.read).fetch("cluster_stamp_aliases", nil))')" != '["0a1b2c3d","4e5f6a7b"]' ]; then
  echo "FAIL [case9]: cluster_stamp_aliases did not render as the two-stamp list" >&2
  FAILED=$((FAILED+1))
fi
JSON_9E="$(render "$(props_with_cid_settings '' '[]')")"
assert_json_valid     "case9-empty" "$JSON_9E"                       || FAILED=$((FAILED+1))
assert_json_lacks_key "case9-empty" "$JSON_9E" vm_cid_format         || FAILED=$((FAILED+1))
assert_json_lacks_key "case9-empty" "$JSON_9E" cluster_stamp_aliases || FAILED=$((FAILED+1))
assert_json_lacks_key "case9-unset" "$JSON_1" vm_cid_format          || FAILED=$((FAILED+1))
assert_json_lacks_key "case9-unset" "$JSON_1" cluster_stamp_aliases  || FAILED=$((FAILED+1))

# release_version renders from spec.release.version, which a director supplies.
# A renderer that supplies no release, or no spec at all, leaves the key out, so
# `cpi --version` falls back to the package version.
echo "Case 10: release_version"
JSON_10="$(render "$PROPS_1" '{"name":"pve_cpi","release":{"name":"bosh-proxmox-cpi","version":"0.9.5"}}')"
assert_json_valid      "case10" "$JSON_10"                        || FAILED=$((FAILED+1))
assert_json_key_equals "case10" "$JSON_10" release_version 0.9.5  || FAILED=$((FAILED+1))
JSON_10N="$(render "$PROPS_1" '{"name":"pve_cpi"}')"
assert_json_valid     "case10-no-release" "$JSON_10N"                 || FAILED=$((FAILED+1))
assert_json_lacks_key "case10-no-release" "$JSON_10N" release_version || FAILED=$((FAILED+1))
JSON_10E="$(render "$PROPS_1" '{"name":"pve_cpi","release":{"name":"bosh-proxmox-cpi","version":""}}')"
assert_json_valid     "case10-empty" "$JSON_10E"                 || FAILED=$((FAILED+1))
assert_json_lacks_key "case10-empty" "$JSON_10E" release_version || FAILED=$((FAILED+1))
assert_json_lacks_key "case10-no-spec" "$JSON_1" release_version || FAILED=$((FAILED+1))

if [ "$FAILED" -ne 0 ]; then
  echo "RESULT: ${FAILED} assertion(s) FAILED" >&2
  exit 1
fi

echo "RESULT: all 3 cred combinations rendered and asserted clean"
exit 0
