// create_vm_nicplan.go maps the director's networks hash onto PVE NIC slots.
//
// A BOSH network normally becomes one NIC. When the director sends
// `nic_group`, every network sharing a group value lands on the SAME NIC —
// that is how a dual-stack instance is expressed: an IPv4 network and an
// IPv6 network, same group, one interface carrying `ip=` and `ip6=` in a
// single PVE ipconfig{N}.
//
// Every part of create_vm that addresses a NIC by index (ipconfig{N},
// net{N}, ipfilter-net{N}, the firewall read-modify-write) must agree on the
// same mapping, so they all derive it from planNICs rather than recomputing a
// position from sortedNetworkNames.
package handlers

import (
	"strings"
)

// nicPlanEntry is a single PVE NIC (net{index}) plus the BOSH networks
// configured onto it, in the order they appear in sortedNetworkNames.
type nicPlanEntry struct {
	index int
	names []string
}

// primary returns the first network name on the NIC. Every entry is built
// with at least one name, so this is always safe.
func (e nicPlanEntry) primary() string {
	return e.names[0]
}

// planNICs assigns networks to NIC slots.
//
// Ordering follows sortedNetworkNames (the `default` network first, then
// alphabetical); a group takes the slot of its first member. Networks with no
// nic_group each get their own NIC, so a director that never sends nic_group
// produces exactly the one-network-per-NIC mapping this CPI has always used —
// index i is still the i-th name in sortedNetworkNames order.
func planNICs(networks map[string]createVMNetworkSpec) []nicPlanEntry {
	ordered := sortedNetworkNames(networks)
	plan := make([]nicPlanEntry, 0, len(ordered))
	slotByGroup := make(map[string]int, len(ordered))

	for _, name := range ordered {
		group := strings.TrimSpace(string(networks[name].NicGroup))
		if group != "" {
			if slot, ok := slotByGroup[group]; ok {
				plan[slot].names = append(plan[slot].names, name)
				continue
			}
			slotByGroup[group] = len(plan)
		}
		plan = append(plan, nicPlanEntry{names: []string{name}})
	}

	for i := range plan {
		plan[i].index = i
	}
	return plan
}

// planNetworkNames flattens a plan back into a NIC-ordered list of network
// names, for the callers that want every network once and do not care which
// NIC it landed on.
func planNetworkNames(plan []nicPlanEntry) []string {
	out := make([]string, 0, len(plan))
	for _, entry := range plan {
		out = append(out, entry.names...)
	}
	return out
}
