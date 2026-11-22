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
	// DrainUndecided is a started HA VM whose fate the drain can't decide
	// from what it read, such as a VM on a storage it can't classify. The
	// fates gate fails while any VM is undecided, so the drain stops nothing.
	DrainUndecided DrainFate = "undecided"
)

// drainVMFate is the fate of one VM of this Director on the drained node.
type drainVMFate struct {
	VMID       int
	Name       string
	Deployment string
	Fate       DrainFate
	// Reason says why a VM stays, has no HA resource, isn't started, or
	// can't be decided.
	Reason string
	// Targets lists the nodes that can take a VM that leaves, sorted.
	Targets []string
	// softPin names the soft node-affinity rule of a VM that leaves when the
	// rule puts only the drained node in its highest priority class. PVE HA
	// should move such a VM to the rule's next class, and a VM it keeps on the
	// node through the settle timeout is one the drain stops.
	softPin string
	facts   *rebalanceVMFacts
}

// drainInactiveStates are the HA manager states of a service that holds no
// node for a resource affinity rule, because PVE counts only active services.
var drainInactiveStates = []string{haRequestStopped, "disabled", "ignored"}

// decideDrainFates decides, for each VM of this Director on node, whether PVE
// HA can move it off the node, from PVE's own rules and the cluster's
// placement facts only. It never reads the CPI's anti-affinity group layout,
// so it holds for any rules PVE applies, whoever wrote them. The fates come in
// VMID order.
func decideDrainFates(cfg *config.CPIConfig, profile *ha.Profile, snap *rebalanceSnapshot, node string,
) []drainVMFate {
	d := drainDecider{cfg: cfg, profile: profile, snap: snap, node: node, where: map[string]string{},
		rules: drainAppliedRules(snap.rules), undecided: map[string]bool{}}
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
		case pve.HARequestedState(f.res.State) != haRawStateStarted:
			fate.Fate = DrainNotStarted
			fate.Reason = "its HA resource requests " + pve.HARequestedState(f.res.State)
			if stoppedBy, ok := ha.DrainStopped(f.res.Comment); ok {
				fate.Reason += ", and the drain of " + stoppedBy + " stopped it"
			}
		default:
			var undecided bool
			fate.Targets, fate.Reason, undecided = d.targets(f)
			if undecided {
				fate.Fate = DrainUndecided
				d.undecided[haResourceSid(f.vmid)] = true
			} else {
				sets[haResourceSid(f.vmid)] = fate.Targets
			}
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
			out[i].softPin = d.softPinOn(haResourceSid(out[i].VMID))
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
	// undecided holds the service IDs of the VMs whose fate the drain can't
	// decide.
	undecided map[string]bool
}

// softPinOn returns the ID of an applied soft node-affinity rule for sid whose
// highest priority class holds the drained node and no other node, or "".
func (d *drainDecider) softPinOn(sid string) string {
	for i := range d.rules {
		if r := &d.rules[i]; drainSoftPinHolds(r, sid, d.node) {
			return r.ID
		}
	}
	return ""
}

// drainSoftPinHolds reports whether r is an enabled soft node-affinity rule
// for sid whose highest priority class holds node alone.
func drainSoftPinHolds(r *ha.Rule, sid, node string) bool {
	if r.Disabled || r.Strict || r.Type != ha.RuleTypeNodeAffinity || !r.HasResource(sid) {
		return false
	}
	top, topNodes, ok := 0, 0, false
	for _, prio := range r.Nodes {
		switch {
		case !ok || prio > top:
			top, topNodes, ok = prio, 1, true
		case prio == top:
			topNodes++
		}
	}
	prio, named := r.Nodes[node]
	return ok && topNodes == 1 && named && prio == top
}

// drainAppliedRules returns the rules PVE applies: enabled, and with no
// feasibility error.
func drainAppliedRules(set *ha.RuleSet) []ha.Rule {
	if set == nil {
		return nil
	}
	var out []ha.Rule
	for i := range set.Rules {
		if r := &set.Rules[i]; !r.Disabled && len(r.Errors) == 0 {
			out = append(out, *r)
		}
	}
	return out
}

// takers returns the nodes other than the drained one that are online and can
// take work from PVE HA, sorted. A node that only its notes keep out of
// placement counts, because the HA manager doesn't read node notes and moves
// VMs to it.
func (d *drainDecider) takers() []string {
	var out []string
	for i := range d.profile.Nodes {
		n := &d.profile.Nodes[i]
		if n.Name == d.node || !n.Online {
			continue
		}
		if !haTakesWork(healthEntry(d.profile.Health, n.Name)) {
			continue
		}
		out = append(out, n.Name)
	}
	slices.Sort(out)
	return out
}

// targets returns the nodes that can take the VM, and the reason when there is
// none. Each step narrows the set, and the first step that empties it names
// the reason. true means the drain can't decide the VM's fate, and the reason
// says why.
func (d *drainDecider) targets(f *rebalanceVMFacts) ([]string, string, bool) {
	sid := haResourceSid(f.vmid)
	set := slices.Clone(d.candidates)
	if len(set) == 0 {
		return nil, "no other node is online, out of maintenance, and able to take work", false
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
			return nil, "its strict node-affinity rule " + r.ID + " names no other node that can take it", false
		}
	}
	set, why, undecided := d.storageTargets(f, set)
	if undecided || len(set) == 0 {
		return nil, why, undecided
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
			"rules: " + rebalanceDetail(held), false
	}
	return set, "", false
}

// storageTargets narrows set to the nodes that hold every node-local volume of
// the VM, which are the targets of its replication jobs that are enabled and
// sound. A volume replication never copies keeps no node. true means a volume
// sits on a storage the drain can't classify, because the storage list
// couldn't be read or doesn't name it, so the drain can't tell whether the
// volume keeps the VM on the node.
func (d *drainDecider) storageTargets(f *rebalanceVMFacts, set []string) ([]string, string, bool) {
	local := false
	for _, v := range haVolumesOfConfig(f.cfg, f.node, nil) {
		storage, _, _ := strings.Cut(v.Volume, ":")
		shared, known := d.profile.StorageShared(d.cfg, storage)
		switch {
		case !known && d.profile != nil && d.profile.StoragesErr != "":
			return nil, "the storage definitions couldn't be read (" + d.profile.StoragesErr + "), so the " +
				"drain can't tell whether its volume " + v.Volume + " sits on shared storage", true
		case !known:
			return nil, "the cluster's storage list doesn't name storage " + storage + ", which holds its " +
				"volume " + v.Volume + ", so the drain can't tell whether it is shared", true
		case shared:
			continue
		}
		if v.Kind == ha.VolumeISO || v.NoReplicate {
			return nil, "its volume " + v.Volume + " sits on node-local storage " + storage + ", and " +
				"replication doesn't copy it", false
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
				"replica of it is on another node that can take it", false
		}
	}
	return set, "", false
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
// the drained node keeps only its own set. A partner whose fate the drain
// can't decide leaves the VM undecided too. It repeats until nothing changes.
func (d *drainDecider) joinPositiveGroups(fates []drainVMFate, sets map[string][]string) {
	for changed := true; changed; {
		changed = false
		for i := range fates {
			fate := &fates[i]
			sid := haResourceSid(fate.VMID)
			if mine, ok := sets[sid]; ok && len(mine) > 0 && d.narrowToPartners(fate, sid, sets) {
				changed = true
			}
		}
	}
}

// narrowToPartners narrows the set of the VM sid to the nodes the partners of
// its positive resource-affinity rules can take, and reports whether it
// changed. A set that ends empty names the rule and the partner as the reason.
func (d *drainDecider) narrowToPartners(fate *drainVMFate, sid string, sets map[string][]string) bool {
	changed := false
	for j := range d.rules {
		r := &d.rules[j]
		if r.Type != ha.RuleTypeResourceAffinity || r.Affinity != ha.AffinityPositive || !r.HasResource(sid) {
			continue
		}
		for _, partner := range r.Resources {
			if why := d.partnerUndecided(partner, sid, sets); why != "" {
				d.undecided[sid] = true
				delete(sets, sid)
				fate.Fate, fate.Targets = DrainUndecided, nil
				fate.Reason = "its positive resource-affinity rule " + r.ID + " keeps it with VM " +
					sidNumber(partner) + ", " + why
				return true
			}
			theirs, ok := d.partnerSet(partner, sid, sets)
			if !ok {
				continue
			}
			mine := sets[sid]
			narrowed := intersectNodes(mine, theirs)
			if len(narrowed) == len(mine) {
				continue
			}
			changed = true
			sets[sid], fate.Targets = narrowed, narrowed
			if len(narrowed) == 0 {
				fate.Reason = "its positive resource-affinity rule " + r.ID + " keeps it with VM " +
					sidNumber(partner) + ", and no node can take them both"
				return true
			}
		}
	}
	return changed
}

// partnerUndecided returns why the drain can't decide where a positive partner
// of sid can go, or "". That is a partner whose own fate is undecided, and an
// active partner on the drained node that is no VM the drain decides, such as
// another Director's VM, because the drain never reads its config.
func (d *drainDecider) partnerUndecided(partner, sid string, sets map[string][]string) string {
	if partner == sid {
		return ""
	}
	if d.undecided[partner] {
		return "whose fate the drain can't decide"
	}
	if _, decided := sets[partner]; decided {
		return ""
	}
	if at, active := d.activeNode(partner); active && at == d.node {
		return "which runs on node " + d.node + " and isn't a VM of this Director, so the drain can't tell " +
			"whether the two can leave together"
	}
	return ""
}

// partnerSet returns the nodes a positive partner of sid can be on: its own
// set when it is on the drained node, and its node when it is active
// elsewhere. false means the partner narrows nothing.
func (d *drainDecider) partnerSet(partner, sid string, sets map[string][]string) ([]string, bool) {
	if partner == sid {
		return nil, false
	}
	if set, here := sets[partner]; here {
		return set, true
	}
	if at, active := d.activeNode(partner); active && at != d.node {
		return []string{at}, true
	}
	return nil, false
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
