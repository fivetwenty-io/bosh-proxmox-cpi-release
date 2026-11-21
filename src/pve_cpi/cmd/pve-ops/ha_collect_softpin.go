package main

import (
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// softPinFindings advises on each of this Director's soft pins that leaves out
// a configured node while its VM is in an enabled resource-affinity rule. PVE
// gives the nodes such a pin leaves out a lower priority, and the CRM applies
// the resource-affinity rule only to the nodes of the highest priority, so the
// pin acts as a strict one whenever group members run on every node it names.
// A maintenance drain then has nowhere to move the VM and leaves it running on
// the node. The CPI writes such a pin no more, and the next anti-affinity plan
// of the group or the next attach_disk on the VM rewrites an old one.
func (c *haCollector) softPinFindings() {
	if c.off() || !c.rulesOK {
		return
	}
	configured := make([]string, 0, len(c.p.Nodes))
	for i := range c.p.Nodes {
		configured = append(configured, c.p.Nodes[i].Name)
	}
	for i := range c.rules {
		pin := &c.rules[i]
		if !strings.HasPrefix(pin.ID, haPinRulePrefix) || !ruleMarked(pin) || c.foreign(pin.Comment) ||
			!c.reportsRule(pin) || !ha.SoftPinNamesSomeNodes(pin, configured) {
			continue
		}
		for _, sid := range pin.Resources {
			groups := c.resourceAffinityRulesOf(sid)
			if len(groups) == 0 {
				continue
			}
			c.rep.advise(haKindSoftPinAffinity, sid,
				"the pin %s is not strict and names only %s, and %s is in the resource-affinity rule %s. "+
					"PVE prefers the nodes the pin names over every other node and applies the resource-affinity "+
					"rule only after that, so when group members run on %s the CRM cannot move the VM, and a node "+
					"maintenance leaves it running on the node. The next create_vm in the group or the next "+
					"attach_disk on the VM rewrites the pin to name every node",
				pin.ID, strings.Join(pin.NodeNames(), ", "), sid, strings.Join(groups, ", "),
				strings.Join(pin.NodeNames(), ", "))
		}
	}
}

// resourceAffinityRulesOf returns the enabled resource-affinity rules that
// name sid, sorted.
func (c *haCollector) resourceAffinityRulesOf(sid string) []string {
	var out []string
	for i := range c.rules {
		r := &c.rules[i]
		if r.Type == ha.RuleTypeResourceAffinity && !r.Disabled && r.HasResource(sid) {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}
