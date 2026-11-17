package main

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// softPin returns a pin on vm:100 that is not strict and names nodes.
func softPin(comment string, nodes ...string) ha.Rule {
	r := pinRule(100, comment, nodes...)
	r.Strict = false
	return r
}

// antiAffinity returns a marked negative resource-affinity rule over sids.
func antiAffinity(sids ...string) ha.Rule {
	return ha.Rule{ID: "bosh-aa-0123456789ab", Type: ha.RuleTypeResourceAffinity, Affinity: ha.AffinityNegative,
		Comment: haMarker, Resources: sids}
}

// TestCollectSoftSingleNodePinOnAntiAffinityMemberAdvises reports this
// Director's soft pin that names only some nodes on a VM in an anti-affinity
// rule, because PVE treats the left-out nodes as a lower priority it never
// falls back to once the resource-affinity rule removes the named ones.
func TestCollectSoftSingleNodePinOnAntiAffinityMemberAdvises(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.rules = []ha.Rule{softPin(haMarker, "n1"), antiAffinity("vm:100", "vm:101")}
	r := runCollect(t, f, haCfg("auto", "none"))
	want(t, r, haKindSoftPinAffinity, haSeverityAdvice, "vm:100", "bosh-na-100", "names only n1",
		"bosh-aa-0123456789ab", "rewrites the pin to name every node")
}

// TestCollectSoftPinAdviceSkipsPinsThatAreSafe reports nothing for a pin that
// is strict, names every node, belongs to another Director, is disabled, or
// sits on a VM in no resource-affinity rule.
func TestCollectSoftPinAdviceSkipsPinsThatAreSafe(t *testing.T) {
	t.Parallel()
	foreign := "bosh-cpi:v1 owner=99999999-2222-3333-4444-555555555555"
	disabledPin := softPin(haMarker, "n1")
	disabledPin.Disabled = true
	disabledGroup := antiAffinity("vm:100", "vm:101")
	disabledGroup.Disabled = true
	for name, rules := range map[string][]ha.Rule{
		"strict":         {pinRule(100, haMarker, "n1"), antiAffinity("vm:100", "vm:101")},
		"every node":     {softPin(haMarker, "n1", "n2", "n3"), antiAffinity("vm:100", "vm:101")},
		"foreign":        {softPin(foreign, "n1"), antiAffinity("vm:100", "vm:101")},
		"disabled pin":   {disabledPin, antiAffinity("vm:100", "vm:101")},
		"disabled group": {softPin(haMarker, "n1"), disabledGroup},
		"no group":       {softPin(haMarker, "n1")},
		"other member":   {softPin(haMarker, "n1"), antiAffinity("vm:101", "vm:102")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			f.rules = rules
			wantNone(t, runCollect(t, f, haCfg("auto", "none")), haKindSoftPinAffinity)
		})
	}
}
