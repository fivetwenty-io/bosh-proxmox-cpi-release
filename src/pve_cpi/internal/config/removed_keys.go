package config

import (
	"fmt"
	"sort"
	"strings"
)

// removedKey describes one job property that this release removed. The spec
// still declares each one, with no default, so that a deployment which sets it
// is detectable instead of being ignored by BOSH.
type removedKey struct {
	// path is the key's position inside the pve.placement block, which is how
	// a cpi-config entry spells it under pve_placement.
	path []string
	// replacement tells the operator what to write instead.
	replacement string
}

// specName returns the job spec property name, such as
// pve.placement.dlb.enabled.
func (k removedKey) specName() string {
	return "pve.placement." + strings.Join(k.path, ".")
}

// removedKeys lists every removed property. The same list, with the same
// replacement text, is in jobs/pve_cpi/templates/cpi.json.erb, which guards the
// job properties, and scripts/_test_erb_render.sh checks that the two agree.
var removedKeys = []removedKey{
	{[]string{"anti_affinity", "use_ha_rules"},
		`use pve.ha.anti_affinity: on with pve.ha.mode: auto`},
	{[]string{"anti_affinity", "strict"},
		`delete it; resource-affinity rules are always strict and nothing replaces this key`},
	{[]string{"pin_az_via_ha_rules"},
		`use pve.ha.pin_az: on with pve.ha.mode: auto`},
	{[]string{"pin_az_strict"},
		`use pve.ha.pin_az_strict`},
	{[]string{"dlb", "enabled"},
		`use pve.ha.dlb.enabled with pve.ha.mode: auto`},
	{[]string{"dlb", "az_name"},
		`use pve.ha.dlb.az_name`},
	{[]string{"dlb", "manage_cluster_crs"},
		`delete it; the CPI no longer writes the cluster CRS mode, so set the CRS mode in PVE`},
	{[]string{"dlb", "require_shared_storage"},
		`delete it; a VM with a node-local volume is never registered for DLB`},
	{[]string{"exclude_maintenance_nodes"},
		`delete it; the node health check always runs`},
}

// removedKeyError builds the one error that names every removed key found,
// each with its replacement. found holds indexes into removedKeys, in any order.
func removedKeyError(where string, found []int) error {
	sort.Ints(found)
	lines := make([]string, 0, len(found))
	for _, i := range found {
		lines = append(lines, fmt.Sprintf("%s: %s", removedKeys[i].specName(), removedKeys[i].replacement))
	}
	return fmt.Errorf("%s sets properties that were removed, and the CPI refuses to run with a feature "+
		"silently switched off; %s", where, strings.Join(lines, "; "))
}

// placementHasKey reports whether the block holds path as nested maps or as a
// dotted key at any split point.
func placementHasKey(block map[string]any, path []string) bool {
	if len(path) == 0 {
		return false
	}
	for split := len(path); split >= 1; split-- {
		head := strings.Join(path[:split], ".")
		v, ok := block[head]
		if !ok {
			continue
		}
		if split == len(path) {
			return true
		}
		if sub, isMap := v.(map[string]any); isMap && placementHasKey(sub, path[split:]) {
			return true
		}
	}
	return false
}

// checkRemovedContextKeys looks for removed keys in the flattened request
// context of a cpi-config entry and returns one error that names every one it
// finds. It runs before any override is applied, and so before the strict decode
// of the pve_placement block, which means an entry that still carries an old key
// gets the replacement named instead of a bare "unknown field".
//
// It reads three spellings. The pve_placement block holds a removed key in the
// nested spelling (a dlb map holding enabled) or as a dotted key inside the
// block ("dlb.enabled"). A spec name written as a flat key reaches the CPI as
// pve_placement.dlb.enabled or as pve_placement_dlb_enabled, and neither is a
// supported override, so without this check the CPI would report the key as
// unsupported and ignore it, which is the silent switch-off the removal must
// not allow. A key set to false, to null, or to a string counts as set. A
// pve_placement value that is not an object is left to the decode to reject.
func checkRemovedContextKeys(extra map[string]any) error {
	block, _ := extra[contextOverridePrefix+"placement"].(map[string]any)
	var found []int
	for i, k := range removedKeys {
		joined := strings.Join(k.path, ".")
		_, hasDotted := extra[contextOverridePrefix+"placement."+joined]
		_, hasFlat := extra[contextOverridePrefix+"placement_"+strings.Join(k.path, "_")]
		if hasDotted || hasFlat || placementHasKey(block, k.path) {
			found = append(found, i)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return removedKeyError("the cpi-config entry", found)
}
