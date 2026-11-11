#!/usr/bin/env bash
# _test_erb_render.sh — render jobs/pve_cpi/templates/cpi.json.erb under each
# case below and assert the resulting JSON has the expected shape. The first
# three cases cover the credential combinations:
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
#        1 = an assertion failed (including a missing v0.9.5 tag when CI is set)
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
if (fixture = ENV['ERB_REFUSALS_FIXTURE'])
  # Render once per fixture entry: layer the entry's config members onto the
  # base pve map, and require the render to raise with exactly the entry's
  # message. The members carry the same names as the pve.* properties.
  base = Marshal.load(Marshal.dump($props))
  cases = JSON.parse(File.read(fixture, encoding: 'UTF-8'))
  failures = 0
  cases.each do |c|
    $props = Marshal.load(Marshal.dump(base))
    c.fetch('config').each { |k, v| $props['pve'][k] = v }
    got = begin
      erb.result(binding)
      nil
    rescue RuntimeError => e
      e.message.dup.force_encoding(Encoding::UTF_8)
    end
    next if got == c.fetch('message')
    failures += 1
    $stderr.puts "FAIL [case15-fixture: #{c.fetch('name')}]"
    $stderr.puts "       expected: #{c.fetch('message').inspect}"
    $stderr.puts "       got:      #{got.nil? ? 'a successful render' : got.inspect}"
  end
  puts "#{cases.length - failures} of #{cases.length} refusals matched"
  exit(failures.zero? ? 0 : 1)
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
    "vm_disk_format" => "", "log_level" => "info",
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

# vm_disk_format follows the omit-when-empty idiom. Empty (the BASE_PROPS
# value and the spec default) leaves the key out so the CPI picks the format
# from each storage's type, and a set value such as raw reaches cpi.json as is.
echo "Case 11: vm_disk_format"
assert_json_lacks_key "case11-empty" "$JSON_1" vm_disk_format || FAILED=$((FAILED+1))
PROPS_11R="$(printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
  h = eval(STDIN.read)
  h["pve"]["vm_disk_format"] = "raw"
  print h.inspect
')"
JSON_11R="$(render "$PROPS_11R")"
assert_json_valid      "case11-raw" "$JSON_11R"                    || FAILED=$((FAILED+1))
assert_json_key_equals "case11-raw" "$JSON_11R" vm_disk_format raw || FAILED=$((FAILED+1))
PROPS_11U="$(printf '%s' "$(build_props 'secret-pw' '')" | ruby -e '
  h = eval(STDIN.read)
  h["pve"].delete("vm_disk_format")
  print h.inspect
')"
JSON_11U="$(render "$PROPS_11U")"
assert_json_valid     "case11-unset" "$JSON_11U"                || FAILED=$((FAILED+1))
assert_json_lacks_key "case11-unset" "$JSON_11U" vm_disk_format || FAILED=$((FAILED+1))
SPEC_VM_DISK_FORMAT="$(ruby -ryaml -e '
  spec = YAML.load_file(ARGV[0])
  print spec.fetch("properties").fetch("pve.vm_disk_format").fetch("default")
' "${REPO_ROOT}/jobs/pve_cpi/spec")"
if [ -n "$SPEC_VM_DISK_FORMAT" ]; then
  echo "FAIL [case11-spec]: pve.vm_disk_format spec default is $(printf %q "$SPEC_VM_DISK_FORMAT"), expected empty" >&2
  FAILED=$((FAILED+1))
fi

# The pve.ha properties have no spec default, so the template renders an "ha"
# block only from keys the operator set. A key set to false or to an empty
# string still counts as set, because the Go config tells those apart from an
# absent key.
echo "Case 12: pve.ha block"
props_with_ha() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -rjson -e '
    h = eval(STDIN.read)
    h["pve"]["ha"] = JSON.parse(ARGV[0])
    print h.inspect
  ' "$1"
}
assert_ha_block() {
  local label="$1"; local json="$2"; local want="$3"
  local got
  got="$(printf '%s' "$json" | ruby -rjson -e 'print JSON.generate(JSON.parse(STDIN.read).fetch("ha", "<absent>"))')"
  if [ "$got" != "$want" ]; then
    echo "FAIL [${label}]: ha block is ${got}, expected ${want}" >&2
    FAILED=$((FAILED+1))
  fi
}
assert_json_lacks_key "case12-unset" "$JSON_1" ha || FAILED=$((FAILED+1))
JSON_12E="$(render "$(props_with_ha '{}')")"
assert_json_valid     "case12-empty" "$JSON_12E"     || FAILED=$((FAILED+1))
assert_json_lacks_key "case12-empty" "$JSON_12E" ha  || FAILED=$((FAILED+1))
JSON_12M="$(render "$(props_with_ha '{"mode":"auto"}')")"
assert_json_valid "case12-mode" "$JSON_12M" || FAILED=$((FAILED+1))
assert_ha_block   "case12-mode" "$JSON_12M" '{"mode":"auto"}'
JSON_12F="$(render "$(props_with_ha '{"mode":"managed","storage_topology":"mixed","local_policy":"none","anti_affinity":"on","anti_affinity_overflow":"split","pin_az":"on","pin_az_strict":false,"headroom":"enforce","dlb":{"enabled":true,"az_name":"balance"},"motion_wait_sec":900,"replication":{"schedule":"*/15","rate_mbps":50}}')")"
assert_json_valid "case12-full" "$JSON_12F" || FAILED=$((FAILED+1))
assert_ha_block   "case12-full" "$JSON_12F" '{"mode":"managed","storage_topology":"mixed","local_policy":"none","anti_affinity":"on","anti_affinity_overflow":"split","pin_az":"on","pin_az_strict":false,"headroom":"enforce","motion_wait_sec":900,"dlb":{"enabled":true,"az_name":"balance"},"replication":{"schedule":"*/15","rate_mbps":50}}'
# Keys set to false, or to an empty string, are still set and still render.
JSON_12S="$(render "$(props_with_ha '{"mode":false,"pin_az_strict":false,"dlb":{"enabled":false,"az_name":""}}')")"
assert_json_valid "case12-set-falsy" "$JSON_12S" || FAILED=$((FAILED+1))
assert_ha_block   "case12-set-falsy" "$JSON_12S" '{"mode":false,"pin_az_strict":false,"dlb":{"enabled":false,"az_name":""}}'
# A quoted decimal integer renders as a number.
JSON_12Q="$(render "$(props_with_ha '{"motion_wait_sec":"900","replication":{"rate_mbps":" 25 "}}')")"
assert_ha_block "case12-quoted-int" "$JSON_12Q" '{"motion_wait_sec":900,"replication":{"rate_mbps":25}}'
# A value that is not a decimal integer passes through, so the Go decoder
# rejects it instead of the template turning it into 0.
JSON_12B="$(render "$(props_with_ha '{"motion_wait_sec":"soon"}')")"
assert_ha_block "case12-bad-int" "$JSON_12B" '{"motion_wait_sec":"soon"}'
# Sub-block keys render only when set.
JSON_12D="$(render "$(props_with_ha '{"dlb":{"enabled":true}}')")"
assert_ha_block "case12-dlb-only" "$JSON_12D" '{"dlb":{"enabled":true}}'
JSON_12R="$(render "$(props_with_ha '{"replication":{"schedule":"mon..fri 22:00"}}')")"
assert_ha_block "case12-replication-only" "$JSON_12R" '{"replication":{"schedule":"mon..fri 22:00"}}'
# Every pve.ha property the spec declares is one the template reads, and the
# spec declares no default for any of them.
SPEC_HA_DEFAULTS="$(ruby -ryaml -e '
  spec = YAML.load_file(ARGV[0])
  props = spec.fetch("properties").select { |k, _| k.start_with?("pve.ha.") }
  puts props.count
  puts props.select { |_, v| v.key?("default") }.keys.join(",")
' "${REPO_ROOT}/jobs/pve_cpi/spec")"
if [ "$(printf '%s' "$SPEC_HA_DEFAULTS" | sed -n 1p)" != "13" ] || [ -n "$(printf '%s' "$SPEC_HA_DEFAULTS" | sed -n 2p)" ]; then
  echo "FAIL [case12-spec]: expected 13 pve.ha properties with no default, got: ${SPEC_HA_DEFAULTS}" >&2
  FAILED=$((FAILED+1))
fi

# The removed placement properties stay in the spec with no default so that a
# deployment which sets one is caught. Each one, set to true, false, or a
# string, makes the render raise with the key and its replacement, and the Go
# table in removed_keys.go carries the same text.
echo "Case 13: removed placement properties"
props_with_placement() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -rjson -e '
    h = eval(STDIN.read)
    h["pve"]["placement"] = JSON.parse(ARGV[0])
    print h.inspect
  ' "$1"
}
assert_render_raises() {
  local label="$1"; local placement_json="$2"; shift 2
  local out rc=0
  out="$(render "$(props_with_placement "$placement_json")" 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "FAIL [${label}]: expected the render to raise, it succeeded" >&2
    FAILED=$((FAILED+1))
    return
  fi
  local needle
  for needle in "$@"; do
    if ! printf '%s' "$out" | grep -qF -- "$needle"; then
      echo "FAIL [${label}]: the error does not contain: ${needle}" >&2
      echo "       output was: ${out}" >&2
      FAILED=$((FAILED+1))
    fi
  done
}
REMOVED_KEYS_TSV="$(cat <<'KEYS'
{"anti_affinity":{"use_ha_rules":VAL}}|pve.placement.anti_affinity.use_ha_rules|use pve.ha.anti_affinity: on with pve.ha.mode: auto
{"anti_affinity":{"strict":VAL}}|pve.placement.anti_affinity.strict|delete it; resource-affinity rules are always strict and nothing replaces this key
{"pin_az_via_ha_rules":VAL}|pve.placement.pin_az_via_ha_rules|use pve.ha.pin_az: on with pve.ha.mode: auto
{"pin_az_strict":VAL}|pve.placement.pin_az_strict|use pve.ha.pin_az_strict
{"dlb":{"enabled":VAL}}|pve.placement.dlb.enabled|use pve.ha.dlb.enabled with pve.ha.mode: auto
{"dlb":{"az_name":VAL}}|pve.placement.dlb.az_name|use pve.ha.dlb.az_name
{"dlb":{"manage_cluster_crs":VAL}}|pve.placement.dlb.manage_cluster_crs|delete it; the CPI no longer writes the cluster CRS mode, so set the CRS mode in PVE
{"dlb":{"require_shared_storage":VAL}}|pve.placement.dlb.require_shared_storage|delete it; a VM with a node-local volume gets no DLB movement, because local_policy none skips it and hold and replicate pin it to its node
{"exclude_maintenance_nodes":VAL}|pve.placement.exclude_maintenance_nodes|delete it; the node health check always runs
KEYS
)"
while IFS='|' read -r shape name replacement; do
  for val in true false '"yes"'; do
    assert_render_raises "case13-${name}-${val}" "${shape//VAL/$val}" "$name" "$replacement"
  done
done <<< "$REMOVED_KEYS_TSV"
# Several keys at once produce one error that names all of them.
assert_render_raises "case13-several" \
  '{"exclude_maintenance_nodes":false,"pin_az_via_ha_rules":true,"dlb":{"enabled":true,"manage_cluster_crs":false}}' \
  "pve.placement.exclude_maintenance_nodes" "pve.placement.pin_az_via_ha_rules" \
  "pve.placement.dlb.enabled" "pve.placement.dlb.manage_cluster_crs"
# None of the removed keys set: placement still renders its supported keys, and
# the removed keys never appear in the output.
JSON_13="$(render "$(props_with_placement '{"anti_affinity":{"enabled":true},"az_map":{"z1":["pve1"]}}')")"
assert_json_valid "case13-clean" "$JSON_13" || FAILED=$((FAILED+1))
if [ "$(printf '%s' "$JSON_13" | ruby -rjson -e 'print JSON.generate(JSON.parse(STDIN.read).fetch("placement"))')" != '{"az_map":{"z1":["pve1"]},"anti_affinity":{"enabled":true}}' ]; then
  echo "FAIL [case13-clean]: placement block is not the supported keys only" >&2
  FAILED=$((FAILED+1))
fi
assert_json_lacks_key "case13-unset" "$JSON_1" placement || FAILED=$((FAILED+1))
# The spec keeps all nine with no default and a REMOVED description, and the
# pve.ha count stays 13.
SPEC_REMOVED="$(ruby -ryaml -e '
  spec = YAML.load_file(ARGV[0])
  names = %w[pve.placement.anti_affinity.use_ha_rules pve.placement.anti_affinity.strict
    pve.placement.pin_az_via_ha_rules pve.placement.pin_az_strict pve.placement.dlb.enabled
    pve.placement.dlb.az_name pve.placement.dlb.manage_cluster_crs
    pve.placement.dlb.require_shared_storage pve.placement.exclude_maintenance_nodes]
  props = spec.fetch("properties")
  bad = names.reject { |n| props.key?(n) && !props[n].key?("default") && props[n]["description"].to_s.include?("REMOVED") }
  print bad.join(",")
' "${REPO_ROOT}/jobs/pve_cpi/spec")"
if [ -n "$SPEC_REMOVED" ]; then
  echo "FAIL [case13-spec]: removed keys missing, carrying a default, or not marked REMOVED: ${SPEC_REMOVED}" >&2
  FAILED=$((FAILED+1))
fi

# pve.endpoints follows the omit-when-empty idiom, and pve.host is optional once
# the list supplies an endpoint. A manifest that sets neither fails at render
# time, which keeps the deploy-time failure the required pve.host gave before.
echo "Case 14: pve.endpoints and optional pve.host"
# props_with_pve merges a JSON object into the base pve map. A null value
# deletes that key, so {"host":null} models a manifest that never sets pve.host.
props_with_pve() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -rjson -e '
    h = eval(STDIN.read)
    JSON.parse(ARGV[0]).each { |k, v| v.nil? ? h["pve"].delete(k) : h["pve"][k] = v }
    print h.inspect
  ' "$1"
}
# props_with_pve_nil sets each key in a JSON array to nil in the base pve map,
# which is how a lookup sees an optional property that a manifest left unset
# and a BOSH renderer reports as nil. props_with_pve deletes the key instead.
# An optional second argument is a JSON object of values to set first.
props_with_pve_nil() {
  printf '%s' "$(build_props 'secret-pw' '')" | ruby -rjson -e '
    h = eval(STDIN.read)
    JSON.parse(ARGV[1].to_s.empty? ? "{}" : ARGV[1]).each { |k, v| h["pve"][k] = v }
    JSON.parse(ARGV[0]).each { |k| h["pve"][k] = nil }
    print h.inspect
  ' "$1" "${2:-}"
}
assert_pve_render_raises() {
  local label="$1"; local pve_json="$2"; shift 2
  local out rc=0
  out="$(render "$(props_with_pve "$pve_json")" 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "FAIL [${label}]: expected the render to raise, it succeeded" >&2
    FAILED=$((FAILED+1))
    return
  fi
  local needle
  for needle in "$@"; do
    if ! printf '%s' "$out" | grep -qF -- "$needle"; then
      echo "FAIL [${label}]: the error does not contain: ${needle}" >&2
      echo "       output was: ${out}" >&2
      FAILED=$((FAILED+1))
    fi
  done
}
# Compact JSON of one rendered key, or <absent>.
json_value() {
  printf '%s' "$1" | ruby -rjson -e 'print JSON.generate(JSON.parse(STDIN.read).fetch(ARGV[0], "<absent>"))' "$2"
}
assert_json_value() {
  local label="$1"; local json="$2"; local key="$3"; local want="$4"
  local got
  got="$(json_value "$json" "$key")"
  if [ "$got" != "$want" ]; then
    echo "FAIL [${label}]: key \"${key}\" is ${got}, expected ${want}" >&2
    echo "       JSON was: ${json}" >&2
    FAILED=$((FAILED+1))
  fi
}

# Unset: the base manifest renders as before, with no endpoints key.
assert_json_lacks_key "case14-unset" "$JSON_1" endpoints || FAILED=$((FAILED+1))
assert_json_key_equals "case14-unset" "$JSON_1" host pve.example.com || FAILED=$((FAILED+1))

# Empty list: the key is left out.
JSON_14E="$(render "$(props_with_pve '{"endpoints":[]}')")"
assert_json_valid     "case14-empty" "$JSON_14E"           || FAILED=$((FAILED+1))
assert_json_lacks_key "case14-empty" "$JSON_14E" endpoints || FAILED=$((FAILED+1))
assert_json_key_equals "case14-empty" "$JSON_14E" host pve.example.com || FAILED=$((FAILED+1))

# A list alongside pve.host: both reach cpi.json, and the order is preserved.
JSON_14L="$(render "$(props_with_pve '{"endpoints":["pve.example.com","pve2.example.com:9000","[fd00::3]:8006"]}')")"
assert_json_valid "case14-list" "$JSON_14L" || FAILED=$((FAILED+1))
assert_json_value "case14-list" "$JSON_14L" endpoints '["pve.example.com","pve2.example.com:9000","[fd00::3]:8006"]'
assert_json_key_equals "case14-list" "$JSON_14L" host pve.example.com || FAILED=$((FAILED+1))

# A list with no pve.host renders an empty host and leaves the first entry to
# the CPI, whether pve.host is absent or an explicit empty string.
for host_json in '{"host":null,"endpoints":["a.example.com:9000","b.example.com"]}' \
                 '{"host":"","endpoints":["a.example.com:9000","b.example.com"]}'; do
  JSON_14H="$(render "$(props_with_pve "$host_json")")"
  assert_json_valid "case14-hostless" "$JSON_14H" || FAILED=$((FAILED+1))
  assert_json_value "case14-hostless" "$JSON_14H" endpoints '["a.example.com:9000","b.example.com"]'
  assert_json_key_equals "case14-hostless" "$JSON_14H" host "" || FAILED=$((FAILED+1))
done

# A blank pve.host counts as unset: with a list it renders an empty host, and
# the CPI takes the first entry; without one the render raises.
JSON_14B="$(render "$(props_with_pve '{"host":"  ","endpoints":["a.example.com:9000"]}')")"
assert_json_valid "case14-blank-host" "$JSON_14B" || FAILED=$((FAILED+1))
assert_json_key_equals "case14-blank-host" "$JSON_14B" host "" || FAILED=$((FAILED+1))
assert_pve_render_raises "case14-blank-host-alone" '{"host":"  "}' "pve.host or pve.endpoints is required"

# The list must be an array of strings; the CPI's JSON parser rejects anything
# else at run time, so the render raises at deploy time instead.
assert_pve_render_raises "case14-scalar" '{"endpoints":"a.example.com"}' "pve.endpoints must be a list of strings"
assert_pve_render_raises "case14-scalar-empty" '{"endpoints":""}' "pve.endpoints must be a list of strings"
assert_pve_render_raises "case14-map" '{"endpoints":{"a":"b"}}' "pve.endpoints must be a list of strings"
assert_pve_render_raises "case14-nonstring-entry" '{"endpoints":["a.example.com",8006]}' "pve.endpoints must be a list of strings"
assert_pve_render_raises "case14-null-entry" '{"endpoints":["a.example.com",null]}' "pve.endpoints must be a list of strings"

# Neither property set: the render raises and names both.
assert_pve_render_raises "case14-neither-unset" '{"host":null}' "pve.host or pve.endpoints is required"
assert_pve_render_raises "case14-neither-empty" '{"host":"","endpoints":[]}' "pve.host or pve.endpoints is required"
assert_pve_render_raises "case14-neither-null-list" '{"host":null,"endpoints":null}' "pve.host or pve.endpoints is required"

# The recommended rollback manifest sets both pve.host and pve.endpoints. The
# 0.9.5 template does not know pve.endpoints, and it has to render that
# manifest anyway, with the host it reads. A developer checkout without tags
# skips this case; with CI set, a missing tag fails the script so the check
# cannot go quiet.
OLD_ERB="$(mktemp "${TMPDIR:-/tmp}/cpi-0.9.5.XXXXXX")"
trap 'rm -f "$OLD_ERB"' EXIT
if git -C "$REPO_ROOT" show v0.9.5:jobs/pve_cpi/templates/cpi.json.erb > "$OLD_ERB" 2>/dev/null; then
  JSON_14R="$(ERB_PATH="$OLD_ERB" render "$(props_with_pve '{"endpoints":["pve.example.com","pve2.example.com"]}')")"
  assert_json_valid      "case14-rollback" "$JSON_14R"                     || FAILED=$((FAILED+1))
  assert_json_key_equals "case14-rollback" "$JSON_14R" host pve.example.com || FAILED=$((FAILED+1))
  assert_json_lacks_key  "case14-rollback" "$JSON_14R" endpoints           || FAILED=$((FAILED+1))
elif [ -n "${CI:-}" ]; then
  echo "FAIL [case14-rollback]: tag v0.9.5 is not in this checkout, and CI requires it (fetch with tags)" >&2
  FAILED=$((FAILED+1))
else
  echo "SKIP [case14-rollback]: tag v0.9.5 is not in this checkout (fetch tags to run it)" >&2
fi

# pve.endpoints is in the job spec with no default, and it is not on the retired
# list, so cpi_config entries may carry it.
SPEC_ENDPOINTS="$(ruby -ryaml -e '
  props = YAML.load_file(ARGV[0]).fetch("properties")
  print(props.key?("pve.endpoints") && !props["pve.endpoints"].key?("default") ? "ok" : "bad")
' "${REPO_ROOT}/jobs/pve_cpi/spec")"
if [ "$SPEC_ENDPOINTS" != "ok" ]; then
  echo "FAIL [case14-spec]: pve.endpoints missing from the job spec or carrying a default" >&2
  FAILED=$((FAILED+1))
fi

# --- Case 15: pve.api_endpoint_discovery and pve.endpoint_cache_dir ------------
# Neither property has a spec default, so an untouched manifest renders neither
# key. The mode accepts only the lowercase names and the two booleans that YAML
# 1.1 makes of a bare on or off, and every refusal uses the CPI config's text.
# The base manifest is password auth with verify_ssl true.
echo "Case 15: pve.api_endpoint_discovery and pve.endpoint_cache_dir"
assert_json_lacks_key "case15-unset" "$JSON_1" api_endpoint_discovery || FAILED=$((FAILED+1))
assert_json_lacks_key "case15-unset" "$JSON_1" endpoint_cache_dir     || FAILED=$((FAILED+1))

# A property that the lookup does not find is unset.
JSON_15A="$(render "$(props_with_pve '{"api_endpoint_discovery":null,"endpoint_cache_dir":null}')")"
assert_json_lacks_key "case15-absent" "$JSON_15A" api_endpoint_discovery || FAILED=$((FAILED+1))
assert_json_lacks_key "case15-absent" "$JSON_15A" endpoint_cache_dir     || FAILED=$((FAILED+1))

# A property that the lookup finds with a nil value is unset too, as it is in a
# BOSH manifest. This reaches the nil guards inside the if_p blocks.
JSON_15N="$(render "$(props_with_pve_nil '["api_endpoint_discovery","endpoint_cache_dir"]')")"
assert_json_valid     "case15-nil" "$JSON_15N"                          || FAILED=$((FAILED+1))
assert_json_lacks_key "case15-nil" "$JSON_15N" api_endpoint_discovery   || FAILED=$((FAILED+1))
assert_json_lacks_key "case15-nil" "$JSON_15N" endpoint_cache_dir       || FAILED=$((FAILED+1))

# A nil verify_ssl reads as verified, so on renders with a password. A nil
# journal directory does not stop a cache directory from rendering.
JSON_15NV="$(render "$(props_with_pve_nil '["verify_ssl"]' '{"api_endpoint_discovery":"on"}')")"
assert_json_key_equals "case15-nil-verify" "$JSON_15NV" api_endpoint_discovery on || FAILED=$((FAILED+1))
JSON_15NJ="$(render "$(props_with_pve_nil '["storage_allocation_journal_dir"]' '{"endpoint_cache_dir":"/var/cache/ep"}')")"
assert_json_key_equals "case15-nil-journal" "$JSON_15NJ" endpoint_cache_dir /var/cache/ep || FAILED=$((FAILED+1))

# Each mode as a string, and the booleans mapped back to the mode names.
for pair in 'auto:auto' 'on:on' 'off:off'; do
  mode="${pair%%:*}"
  JSON_15M="$(render "$(props_with_pve "{\"api_endpoint_discovery\":\"${mode}\"}")")"
  assert_json_valid      "case15-${mode}" "$JSON_15M"                           || FAILED=$((FAILED+1))
  assert_json_key_equals "case15-${mode}" "$JSON_15M" api_endpoint_discovery "$mode" || FAILED=$((FAILED+1))
done
JSON_15T="$(render "$(props_with_pve '{"api_endpoint_discovery":true}')")"
assert_json_value "case15-true" "$JSON_15T" api_endpoint_discovery '"on"'
JSON_15F="$(render "$(props_with_pve '{"api_endpoint_discovery":false}')")"
assert_json_value "case15-false" "$JSON_15F" api_endpoint_discovery '"off"'

# Anything else is refused with the config's message: wrong case, padding, the
# string forms of the booleans, and an empty string.
for bad in 'ON' 'On' ' on' 'on ' 'true' 'false' 'yes' 'enabled' ''; do
  assert_pve_render_raises "case15-bad-[${bad}]" "{\"api_endpoint_discovery\":\"${bad}\"}" \
    "api_endpoint_discovery must be auto, on, or off, got \"${bad}\""
done

# A value that is neither a string nor a boolean is refused, and the echo of it
# is cut at 64 bytes.
assert_pve_render_raises "case15-int" '{"api_endpoint_discovery":1}' \
  "api_endpoint_discovery: expected a string or a boolean, got 1"
assert_pve_render_raises "case15-list" '{"api_endpoint_discovery":["on"]}' \
  'api_endpoint_discovery: expected a string or a boolean, got ["on"]'
LONG_15="$(printf 'x%.0s' $(seq 1 100))"
assert_pve_render_raises "case15-long" "{\"api_endpoint_discovery\":[\"${LONG_15}\"]}" \
  "got [\"$(printf 'x%.0s' $(seq 1 62))..."

# on with password auth and verify_ssl false is refused, whether it is written
# as the string or as a bare boolean. The base manifest holds a password and no
# token.
PW_NOVERIFY_ERR='api_endpoint_discovery "on" cannot be used with password auth while verify_ssl is false; use an API token (pve.api_token) or turn on verification (pve.verify_ssl: true)'
assert_pve_render_raises "case15-pw-noverify" '{"verify_ssl":false,"api_endpoint_discovery":"on"}' "$PW_NOVERIFY_ERR"
assert_pve_render_raises "case15-pw-noverify-bool" '{"verify_ssl":false,"api_endpoint_discovery":true}' "$PW_NOVERIFY_ERR"

# The other modes and the verified case still render under the same auth.
for mode in auto off; do
  JSON_15P="$(render "$(props_with_pve "{\"verify_ssl\":false,\"api_endpoint_discovery\":\"${mode}\"}")")"
  assert_json_key_equals "case15-pw-noverify-${mode}" "$JSON_15P" api_endpoint_discovery "$mode" || FAILED=$((FAILED+1))
done
JSON_15V="$(render "$(props_with_pve '{"verify_ssl":true,"api_endpoint_discovery":"on"}')")"
assert_json_key_equals "case15-pw-verify" "$JSON_15V" api_endpoint_discovery on || FAILED=$((FAILED+1))

# A configured token wins over a password, as the CPI decides, so on renders
# with verify_ssl false whether the password is a placeholder beside the token
# or empty.
JSON_15K="$(render "$(props_with_pve '{"api_token":"root@pam!cpi=abc-123","verify_ssl":false,"api_endpoint_discovery":"on"}')")"
assert_json_valid      "case15-token-wins" "$JSON_15K"                          || FAILED=$((FAILED+1))
assert_json_key_equals "case15-token-wins" "$JSON_15K" api_endpoint_discovery on || FAILED=$((FAILED+1))
JSON_15K2="$(render "$(props_with_pve '{"password":"","api_token":"root@pam!cpi=abc-123","verify_ssl":false,"api_endpoint_discovery":true}')")"
assert_json_key_equals "case15-token-only" "$JSON_15K2" api_endpoint_discovery on || FAILED=$((FAILED+1))
# The header form of a real token is a token, as the CPI reads it, so on renders
# with verify_ssl false beside a password. A value that is only the prefix is no
# token, and the fixture case "mode on with a token that is only the header
# prefix and no verification" asserts the refusal.
JSON_15K3="$(render "$(props_with_pve '{"api_token":"PVEAPIToken=root@pam!cpi=abc-123","verify_ssl":false,"api_endpoint_discovery":"on"}')")"
assert_json_valid      "case15-prefixed-token" "$JSON_15K3"                          || FAILED=$((FAILED+1))
assert_json_key_equals "case15-prefixed-token" "$JSON_15K3" api_endpoint_discovery on || FAILED=$((FAILED+1))

# The cache directory renders as written when it passes, and an empty string
# leaves it out.
JSON_15D="$(render "$(props_with_pve '{"endpoint_cache_dir":"/var/vcap/store/pve_cpi/endpoint-cache"}')")"
assert_json_key_equals "case15-dir" "$JSON_15D" endpoint_cache_dir /var/vcap/store/pve_cpi/endpoint-cache || FAILED=$((FAILED+1))
JSON_15DE="$(render "$(props_with_pve '{"endpoint_cache_dir":""}')")"
assert_json_valid     "case15-dir-empty" "$JSON_15DE"                    || FAILED=$((FAILED+1))
assert_json_lacks_key "case15-dir-empty" "$JSON_15DE" endpoint_cache_dir || FAILED=$((FAILED+1))

# Each refusal uses the config's text.
assert_pve_render_raises "case15-dir-relative" '{"endpoint_cache_dir":"cache/dir"}' \
  'endpoint_cache_dir must be an absolute path, got "cache/dir"'
assert_pve_render_raises "case15-dir-leading-space" '{"endpoint_cache_dir":" /cache"}' \
  'endpoint_cache_dir must be an absolute path, got " /cache"'
assert_pve_render_raises "case15-dir-trailing-space" '{"endpoint_cache_dir":"/cache "}' \
  'endpoint_cache_dir must not have leading or trailing whitespace, got "/cache "'
assert_pve_render_raises "case15-dir-control" '{"endpoint_cache_dir":"/ca\u0009che"}' \
  "endpoint_cache_dir must not contain control characters"
assert_pve_render_raises "case15-dir-dotdot" '{"endpoint_cache_dir":"/a/../b"}' \
  'endpoint_cache_dir must be a clean path, write "/b" instead of "/a/../b"'
assert_pve_render_raises "case15-dir-trailing-slash" '{"endpoint_cache_dir":"/a/b/"}' \
  'endpoint_cache_dir must be a clean path, write "/a/b" instead of "/a/b/"'
assert_pve_render_raises "case15-dir-double-slash" '{"endpoint_cache_dir":"//a"}' \
  'endpoint_cache_dir must be a clean path, write "/a" instead of "//a"'
assert_pve_render_raises "case15-dir-root" '{"endpoint_cache_dir":"/"}' \
  "endpoint_cache_dir must not be the root directory; choose a dedicated directory"
assert_pve_render_raises "case15-dir-nonstring" '{"endpoint_cache_dir":5}' \
  "pve.endpoint_cache_dir must be a string"

# The cache directory may not equal, sit inside, or contain the journal
# directory, by whole path elements, and a sibling of the journal is allowed.
JOURNAL_ERR_TAIL='; choose a directory that does not overlap the journal'
assert_pve_render_raises "case15-dir-equals-journal" \
  '{"storage_allocation_journal_dir":"/var/j","endpoint_cache_dir":"/var/j"}' \
  'endpoint_cache_dir "/var/j" must not equal, sit inside, or contain storage_allocation_journal_dir "/var/j"'"$JOURNAL_ERR_TAIL"
assert_pve_render_raises "case15-dir-inside-journal" \
  '{"storage_allocation_journal_dir":"/var/j","endpoint_cache_dir":"/var/j/cache"}' \
  'endpoint_cache_dir "/var/j/cache" must not equal, sit inside, or contain storage_allocation_journal_dir "/var/j"'"$JOURNAL_ERR_TAIL"
assert_pve_render_raises "case15-dir-inside-unclean-journal" \
  '{"storage_allocation_journal_dir":"/var//j/","endpoint_cache_dir":"/var/j/cache"}' \
  'must not equal, sit inside, or contain storage_allocation_journal_dir "/var//j/"'
assert_pve_render_raises "case15-dir-inside-root-journal" \
  '{"storage_allocation_journal_dir":"/","endpoint_cache_dir":"/cache"}' \
  'must not equal, sit inside, or contain storage_allocation_journal_dir "/"'
assert_pve_render_raises "case15-dir-holds-journal" \
  '{"storage_allocation_journal_dir":"/var/j/store","endpoint_cache_dir":"/var/j"}' \
  'endpoint_cache_dir "/var/j" must not equal, sit inside, or contain storage_allocation_journal_dir "/var/j/store"'"$JOURNAL_ERR_TAIL"
assert_pve_render_raises "case15-dir-holds-unclean-journal" \
  '{"storage_allocation_journal_dir":"/var//j/store/","endpoint_cache_dir":"/var/j"}' \
  'must not equal, sit inside, or contain storage_allocation_journal_dir "/var//j/store/"'
assert_pve_render_raises "case15-dir-holds-journal-deep" \
  '{"storage_allocation_journal_dir":"/var/vcap/store/journal","endpoint_cache_dir":"/var"}' \
  'endpoint_cache_dir "/var" must not equal, sit inside, or contain storage_allocation_journal_dir "/var/vcap/store/journal"'"$JOURNAL_ERR_TAIL"
assert_pve_render_raises "case15-journal-whitespace" \
  '{"storage_allocation_journal_dir":"/var/j ","endpoint_cache_dir":"/var/j/cache"}' \
  'storage_allocation_journal_dir must not have leading or trailing whitespace when endpoint_cache_dir is set, got "/var/j "'
for pair in '/var/j:/var/jx' '/var/j:/var/cache' '/var/jx/sub:/var/j' '/var/j:/other/j/cache' 'relative/j:/var/j'; do
  journal="${pair%%:*}"; dir="${pair#*:}"
  JSON_15J="$(render "$(props_with_pve "{\"storage_allocation_journal_dir\":\"${journal}\",\"endpoint_cache_dir\":\"${dir}\"}")")"
  assert_json_key_equals "case15-journal-ok-${journal}-${dir}" "$JSON_15J" endpoint_cache_dir "$dir" || FAILED=$((FAILED+1))
done

# A journal directory with leading whitespace is also not absolute, so the CPI
# reports a second error after the whitespace one. The template raises the
# first, which the fixture below cannot hold because it asserts the whole text.
assert_pve_render_raises "case15-journal-leading-space" \
  '{"storage_allocation_journal_dir":" /var/j","endpoint_cache_dir":"/var/cache/ep"}' \
  'storage_allocation_journal_dir must not have leading or trailing whitespace when endpoint_cache_dir is set, got " /var/j"'

# The CPI's config tests read the same fixture and assert that config.Load
# refuses each entry with exactly this message, so a wording change on either
# side fails a test until the other side matches. Each entry's members carry the
# same names as the pve.* properties, and the template has to raise the message
# in full, including the quoted values.
REFUSALS_FIXTURE="${REPO_ROOT}/src/pve_cpi/internal/config/testdata/discovery_refusals.json"
if [ ! -r "$REFUSALS_FIXTURE" ]; then
  echo "FAIL [case15-refusals]: cannot read ${REFUSALS_FIXTURE}" >&2
  FAILED=$((FAILED+1))
elif ! REFUSALS_OUT="$(ERB_REFUSALS_FIXTURE="$REFUSALS_FIXTURE" render "$(build_props 'secret-pw' '')" 2>&1)"; then
  printf '%s\n' "$REFUSALS_OUT" >&2
  FAILED=$((FAILED+1))
fi

# The quoting helper in the template has to match Go's strconv.Quote, because
# the CPI's messages use %q. The lambda is pulled out of the template and run
# against the fixture that holds strconv.Quote's output for each class of
# character: the named escapes, other controls, Unicode whitespace, format and
# private-use characters, printable non-ASCII, hash sequences, and invalid
# UTF-8. It runs under the C locale too, where Ruby's inspect escapes every
# non-ASCII character.
QUOTE_FIXTURE="${REPO_ROOT}/src/pve_cpi/internal/config/testdata/template_quote_cases.json"
for locale in C en_US.UTF-8; do
  if ! QUOTE_OUT="$(LC_ALL="$locale" LANG="$locale" ruby -rjson - "$ERB_PATH" "$QUOTE_FIXTURE" 2>&1 <<'RUBY'
src = File.read(ARGV[0], encoding: 'UTF-8')
found = src.match(/^  go_quote = lambda do \|str\|\n.*?^  end\n/m) or abort "go_quote lambda not found in the template"
go_quote = eval(found[0].sub('go_quote = ', ''))
cases = JSON.parse(File.read(ARGV[1], encoding: 'UTF-8'))
failures = cases.reject do |c|
  go_quote.call([c.fetch('hex')].pack('H*')) == c.fetch('quoted')
end
failures.each do |c|
  got = go_quote.call([c.fetch('hex')].pack('H*'))
  warn "FAIL [case15-quote: #{c.fetch('name')}] got #{got.dump} want #{c.fetch('quoted').dump}"
end
puts "#{cases.length - failures.length} of #{cases.length} quotes matched"
exit(failures.empty? ? 0 : 1)
RUBY
)"; then
    echo "FAIL [case15-quote-${locale}]: the template's quoting differs from strconv.Quote" >&2
    printf '%s\n' "$QUOTE_OUT" >&2
    FAILED=$((FAILED+1))
  fi
done

# Both properties are in the job spec with no default, and neither is on the
# retired list that the removed-keys guard holds.
SPEC_DISCOVERY="$(ruby -ryaml -e '
  props = YAML.load_file(ARGV[0]).fetch("properties")
  ok = %w[pve.api_endpoint_discovery pve.endpoint_cache_dir].all? { |k| props.key?(k) && !props[k].key?("default") }
  print(ok ? "ok" : "bad")
' "${REPO_ROOT}/jobs/pve_cpi/spec")"
if [ "$SPEC_DISCOVERY" != "ok" ]; then
  echo "FAIL [case15-spec]: a discovery property is missing from the job spec or carries a default" >&2
  FAILED=$((FAILED+1))
fi

if [ "$FAILED" -ne 0 ]; then
  echo "RESULT: ${FAILED} assertion(s) FAILED" >&2
  exit 1
fi

echo "RESULT: every case rendered and asserted clean"
exit 0
