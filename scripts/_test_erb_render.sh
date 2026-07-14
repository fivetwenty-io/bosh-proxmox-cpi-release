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
    "network_bridge" => "vmbr0", "network_mode" => "sdn",
    "sdn_zone" => "", "sdn_zone_type" => "vxlan", "sdn_auto_manage_zone" => true,
    "verify_ssl" => true, "agent_mode" => "cloudinit",
    "vm_disk_format" => "qcow2", "log_level" => "info",
    "vmid_range_start" => 100, "vmid_range_end" => 5999,
    "allow_disk_ops_with_snapshots" => false,
    "require_snapshot_check_pass" => false,
    "hotplug" => "network,disk,cpu,memory", "numa" => true,
    "reboot_mode" => "soft", "reboot_timeout" => 60,
    "vm_prefix" => "", "create_env_deployment" => "create-env"
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

if [ "$FAILED" -ne 0 ]; then
  echo "RESULT: ${FAILED} assertion(s) FAILED" >&2
  exit 1
fi

echo "RESULT: all 3 cred combinations rendered and asserted clean"
exit 0
