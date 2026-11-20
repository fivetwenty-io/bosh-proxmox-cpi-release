package handlers

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// DrainFate is what a drain decides for one VM of this Director on the node.
type DrainFate string

// The fates of a VM on a drained node.
const (
	// DrainLeaves is a started HA VM that another node can take, so PVE HA
	// moves it during maintenance.
	DrainLeaves DrainFate = "leaves"
	// DrainStays is a started HA VM that no other node can take, so the drain
	// stops it through PVE HA.
	DrainStays DrainFate = "stays"
	// DrainNotHA is a VM with no HA resource, which maintenance never moves
	// and the drain never stops.
	DrainNotHA DrainFate = "not_ha"
	// DrainNotStarted is a VM whose HA resource does not request started, so
	// it doesn't block the node's maintenance.
	DrainNotStarted DrainFate = "not_started"
)

// drainVMFate is the fate of one VM of this Director on the drained node.
type drainVMFate struct {
	VMID       int
	Name       string
	Deployment string
	Fate       DrainFate
	// Reason says why a VM stays, has no HA resource, or isn't started.
	Reason string
	// Targets lists the nodes that can take a VM that leaves, sorted.
	Targets []string
	facts   *rebalanceVMFacts
}

// drainInactiveStates are the HA manager states of a service that holds no
// node for a resource affinity rule, because PVE counts only active services.
var drainInactiveStates = []string{"stopped", "disabled", "ignored"}

// decideDrainFates decides, for each VM of this Director on node, whether PVE
// HA can move it off the node, from PVE's own rules and the cluster's
// placement facts only. It never reads the CPI's anti-affinity group layout,
// so it holds for any rules PVE applies, whoever wrote them. The fates come in
// VMID order.
func decideDrainFates(cfg *config.CPIConfig, profile *ha.Profile, snap *rebalanceSnapshot, node string,
) []drainVMFate {
	d := drainDecider{cfg: cfg, profile: profile, snap: snap, node: node, where: map[string]string{},
		rules: drainAppliedRules(snap.rules)}
	for i := range snap.guests {
		g := &snap.guests[i]
		d.where[strconv.Itoa(g.VMID)] = g.Node
	}
	d.candidates = d.takers()
	var out []drainVMFate
	sets := map[string][]string{}
	for i := range snap.vms {
		f := &snap.vms[i]
		if f.node != node {
			continue
		}
		fate := drainVMFate{VMID: f.vmid, Name: f.name, Deployment: drainDeploymentOf(f.tags), facts: f}
		switch {
		case f.res == nil:
			fate.Fate, fate.Reason = DrainNotHA, "it has no HA resource, so maintenance won't move it, and the "+
				"drain won't stop it; it goes down when the node reboots"
		case pve.HARequestedState(f.res.State) != "started":
			fate.Fate = DrainNotStarted
			fate.Reason = "its HA resource requests " + pve.HARequestedState(f.res.State)
			if stoppedBy, ok := ha.DrainStopped(f.res.Comment); ok {
				fate.Reason += ", and the drain of " + stoppedBy + " stopped it"
			}
		default:
			fate.Targets, fate.Reason = d.targets(f)
			sets[haResourceSid(f.vmid)] = fate.Targets
		}
		out = append(out, fate)
	}
	d.joinPositiveGroups(out, sets)
	for i := range out {
		switch {
		case out[i].Fate != "":
		case len(out[i].Targets) == 0:
			out[i].Fate = DrainStays
		default:
			out[i].Fate, out[i].Reason = DrainLeaves, ""
		}
	}
	return out
}

// drainDecider holds what every VM's decision reads.
type drainDecider struct {
	cfg     *config.CPIConfig
	profile *ha.Profile
	snap    *rebalanceSnapshot
	node    string
	// where maps a guest's VMID, as text, to its node.
	where map[string]string
	// rules are the rules PVE applies.
	rules []ha.Rule
	// candidates are the nodes other than the drained one that can take work.
	candidates []string
}

// drainAppliedRules returns the rules PVE applies: enabled, and with no
// feasibility error.
func drainAppliedRules(set *ha.RuleSet) []ha.Rule {
	if set == nil {
		return nil
	}
	var out []ha.Rule
	for _, r := range set.Rules {
		if !r.Disabled && len(r.Errors) == 0 {
			out = append(out, r)
		}
	}
	return out
}

// takers returns the nodes other than the drained one that are online, out of
// maintenance, and placeable, sorted.
func (d *drainDecider) takers() []string {
	var out []string
	for _, n := range d.profile.Nodes {
		if n.Name == d.node || !n.Online {
			continue
		}
		e := healthEntry(d.profile.Health, n.Name)
		if e == nil || e.Maintenance || e.DeadOrFenced || !e.Placeable {
			continue
		}
		out = append(out, n.Name)
	}
	slices.Sort(out)
	return out
}

// targets returns the nodes that can take the VM, and the reason when there is
// none. Each step narrows the set, and the first step that empties it names
// the reason.
func (d *drainDecider) targets(f *rebalanceVMFacts) ([]string, string) {
	sid := haResourceSid(f.vmid)
	set := slices.Clone(d.candidates)
	if len(set) == 0 {
		return nil, "no other node is online, out of maintenance, and able to take work"
	}
	for i := range d.rules {
		r := &d.rules[i]
		if r.Type != ha.RuleTypeNodeAffinity || !r.Strict || !r.HasResource(sid) {
			continue
		}
		named := r.NodeNames()
		set = slices.DeleteFunc(set, func(n string) bool {
			return slices.Contains(named, n) == (r.Affinity == ha.AffinityNegative)
		})
		if len(set) == 0 {
			return nil, "its strict node-affinity rule " + r.ID + " names no other node that can take it"
		}
	}
	set, why := d.storageTargets(f, set)
	if len(set) == 0 {
		return nil, why
	}
	var held []string
	for i := range d.rules {
		r := &d.rules[i]
		if r.Type != ha.RuleTypeResourceAffinity || r.Affinity != ha.AffinityNegative || !r.HasResource(sid) {
			continue
		}
		for _, partner := range r.Resources {
			at, ok := d.activeNode(partner)
			if partner == sid || !ok || !slices.Contains(set, at) {
				continue
			}
			set = slices.DeleteFunc(set, func(n string) bool { return n == at })
			held = append(held, fmt.Sprintf("VM %s on %s in %s", sidNumber(partner), at, r.ID))
		}
	}
	if len(set) == 0 {
		return nil, "every other node that can take it holds a partner from its negative resource-affinity " +
			"rules: " + rebalanceDetail(held)
	}
	return set, ""
}

// storageTargets narrows set to the nodes that hold every node-local volume of
// the VM: the targets of its replication jobs that are enabled and sound. A
// volume replication never copies keeps no node.
func (d *drainDecider) storageTargets(f *rebalanceVMFacts, set []string) ([]string, string) {
	local := false
	for _, v := range haVolumesOfConfig(f.cfg, f.node, nil) {
		storage, _, _ := strings.Cut(v.Volume, ":")
		if shared, known := d.profile.StorageShared(d.cfg, storage); shared && known {
			continue
		}
		if v.Kind == ha.VolumeISO || v.NoReplicate {
			return nil, "its volume " + v.Volume + " sits on node-local storage " + storage + ", and " +
				"replication doesn't copy it"
		}
		if !local {
			local = true
			var replicas []string
			for _, j := range d.snap.jobs {
				if j.Guest == f.vmid && !j.Disabled && j.Fault == "" {
					replicas = append(replicas, j.Target)
				}
			}
			set = slices.DeleteFunc(set, func(n string) bool { return !slices.Contains(replicas, n) })
		}
		if len(set) == 0 {
			return nil, "its volume " + v.Volume + " sits on node-local storage " + storage + ", and no " +
				"replica of it is on another node that can take it"
		}
	}
	return set, ""
}

// activeNode returns the node of the service sid when the HA manager counts it
// as active.
func (d *drainDecider) activeNode(sid string) (string, bool) {
	state, managed := d.snap.states[sid]
	if !managed || slices.Contains(drainInactiveStates, state) {
		return "", false
	}
	node, ok := d.where[sidNumber(sid)]
	return node, ok && node != ""
}

// joinPositiveGroups narrows each VM in a positive resource-affinity rule to
// the nodes its partners can take too, because PVE moves such a group
// together. A partner on another node keeps only that node, and a partner on
// the drained node keeps only its own set. It repeats until nothing changes.
func (d *drainDecider) joinPositiveGroups(fates []drainVMFate, sets map[string][]string) {
	for changed := true; changed; {
		changed = false
		for i := range fates {
			fate := &fates[i]
			sid := haResourceSid(fate.VMID)
			mine, ok := sets[sid]
			if !ok || len(mine) == 0 {
				continue
			}
			for j := range d.rules {
				r := &d.rules[j]
				if r.Type != ha.RuleTypeResourceAffinity || r.Affinity != ha.AffinityPositive || !r.HasResource(sid) {
					continue
				}
				for _, partner := range r.Resources {
					if partner == sid {
						continue
					}
					var theirs []string
					if set, here := sets[partner]; here {
						theirs = set
					} else if at, active := d.activeNode(partner); active && at != d.node {
						theirs = []string{at}
					} else {
						continue
					}
					narrowed := intersectNodes(mine, theirs)
					if len(narrowed) == len(mine) {
						continue
					}
					mine, changed = narrowed, true
					sets[sid], fate.Targets = mine, mine
					if len(mine) == 0 {
						fate.Reason = "its positive resource-affinity rule " + r.ID + " keeps it with VM " +
							sidNumber(partner) + ", and no node can take them both"
					}
				}
			}
		}
	}
}

// sidNumber returns the VMID of an HA service ID such as vm:101.
func sidNumber(sid string) string {
	_, n, _ := strings.Cut(sid, ":")
	return n
}

// drainDeploymentOf returns the BOSH deployment a VM's deployment-- tag names,
// or "".
func drainDeploymentOf(tags string) string {
	values := tagValues(lowerTags(parseTagsField(tags)), deploymentTagPrefix)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
