package handlers

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// rebalanceSnapshot is one fail-closed read of the cluster: the nodes, every
// guest, this Director's HA VMs with their configs and HA resources, the HA
// rules, the HA manager's service states, and the replication jobs.
type rebalanceSnapshot struct {
	nodes  []harebalance.Node
	guests []harebalance.Guest
	vms    []rebalanceVMFacts
	rules  *ha.RuleSet
	states map[string]string
	jobs   []ha.ReplicaJobState
}

// rebalanceVMFacts is one VM of this Director as the snapshot read it.
type rebalanceVMFacts struct {
	vmid int
	name string
	node string
	tags string
	mem  int64
	cfg  map[string]any
	// res is the VM's HA resource, nil when it has none.
	res *sdkcluster.GetHaResourcesResponse
}

// rebalanceVMPlan is what a move of one VM needs beyond the planner's VM: the
// pin it was decided from and, for each target whose move rewrites the pin,
// the decision and the pin the CPI writes there.
type rebalanceVMPlan struct {
	facts *rebalanceVMFacts
	pin   *ha.Rule
	// rewrites maps a target to the pin a move there writes.
	rewrites map[string]rebalancePinChange
}

// rebalancePinChange is the pin a move to a target writes, and the decision
// it was built from.
type rebalancePinChange struct {
	d   *ha.VMDecision
	pin *ha.Rule
}

// rebalanceModelBuilder turns a snapshot into the planner's model.
type rebalanceModelBuilder struct {
	deps     Deps
	owner    string
	profile  *ha.Profile
	snap     *rebalanceSnapshot
	excluded map[int]string
	now      time.Time
}

// build returns the model, the plan of each VM the planner may move, and the
// number of DLB members it found.
func (b *rebalanceModelBuilder) build() (harebalance.Model, map[int]*rebalanceVMPlan, int) {
	model := harebalance.Model{Nodes: b.modelNodes(), Guests: slices.Clone(b.snap.guests),
		Rules: rebalanceRules(b.snap.rules), HeadroomMode: b.deps.Config.HAHeadroomValue()}
	plans := map[int]*rebalanceVMPlan{}
	dlb := 0
	for i := range b.snap.vms {
		v, plan := b.vm(&b.snap.vms[i])
		if v.Skip == harebalance.SkipDLB {
			dlb++
		}
		model.VMs = append(model.VMs, v)
		if plan != nil {
			plans[v.VMID] = plan
		}
	}
	return model, plans, dlb
}

// modelNodes marks the nodes the profile's health shows in maintenance, or
// unable to take work, as in maintenance.
func (b *rebalanceModelBuilder) modelNodes() []harebalance.Node {
	out := slices.Clone(b.snap.nodes)
	for i := range out {
		var health *pve.NodeHealthReport
		if b.profile != nil {
			health = b.profile.Health
		}
		if e := healthEntry(health, out[i].Name); e != nil && (e.Maintenance || e.DeadOrFenced || !e.Placeable) {
			out[i].Maintenance = true
		}
	}
	return out
}

// vm classifies one VM in the order of section 5 of the design and, for a
// candidate, fills its targets, its AZ, and the pin each target needs.
func (b *rebalanceModelBuilder) vm(f *rebalanceVMFacts) (harebalance.VM, *rebalanceVMPlan) {
	v := harebalance.VM{VMID: f.vmid, Name: f.name, Node: f.node, MemBytes: f.mem}
	skip := func(kind harebalance.SkipKind, reason string) (harebalance.VM, *rebalanceVMPlan) {
		v.Skip, v.SkipReason = kind, reason
		return v, nil
	}
	sid := haResourceSid(f.vmid)
	pin := b.snap.rules.Find(naRuleNameFor(f.vmid))
	if why, ok := b.excluded[f.vmid]; ok {
		return skip(harebalance.SkipRefused, why)
	}
	if f.res == nil {
		return skip(harebalance.SkipLocalDisk, "it has no HA resource, as a VM with a node-local disk has none, "+
			"so PVE HA can't move it")
	}
	if why := b.replicateReason(f, pin); why != "" {
		return skip(harebalance.SkipReplicate, why)
	}
	if node, ok := ha.RebalancePending(f.res.Comment); ok {
		return skip(harebalance.SkipRebalancePending, "it carries the mark of an earlier rebalance that moves it "+
			"to "+node+", which a run with --apply finishes first")
	}
	if ha.FailbackHeld(f.res.Comment, b.now) {
		return skip(harebalance.SkipFailbackHeld, "its HA resource carries a fresh failback hold, so an attach "+
			"or a narrowing is moving it")
	}
	if ha.NarrowPending(f.res.State, f.res.Comment) {
		return skip(harebalance.SkipNarrowing, "the narrowing barrier holds its HA resource while a CPI call "+
			"changes it")
	}
	if requested, raw := pve.HARequestedState(f.res.State), b.snap.states[sid]; requested != "started" ||
		raw != "started" {
		return skip(harebalance.SkipNotStarted, fmt.Sprintf("its HA resource requests %s and the HA manager "+
			"reports it as %s, and only a started VM moves", orNothing(requested), orNothing(raw)))
	}
	if lock, _ := pve.ConfigString(f.cfg, "lock"); strings.TrimSpace(lock) != "" {
		return skip(harebalance.SkipLocked, "it holds the PVE lock "+strconv.Quote(lock)+", so a task is changing it")
	}
	if b.profile.ResourceKeys().InDLB(f.res.Comment, f.res.AutoRebalance) {
		return skip(harebalance.SkipDLB, "it is a DLB member, and PVE's CRS load balancer places it")
	}
	if inPositiveAffinityRule(b.snap.rules, sid) {
		return skip(harebalance.SkipPositiveAffinity, "it is in a positive resource affinity rule, so PVE would "+
			"move its partners with it")
	}
	if pin == nil || pin.Type != ha.RuleTypeNodeAffinity || pin.Disabled ||
		haOwnershipOf(pin.Comment, b.owner) != haOwnedByUs {
		return skip(harebalance.SkipUndecided, "it has no enabled pin of this Director")
	}
	if names := pin.NodeNames(); pin.Strict && len(names) == 1 {
		return skip(harebalance.SkipHold, "it is a hold VM pinned to "+names[0]+" alone")
	}
	if top, pending := b.failbackPending(f, pin); pending {
		return skip(harebalance.SkipFailbackPending, "its failback is on, and PVE HA returns it to "+top+
			" on its own")
	}
	return b.candidate(v, f, pin)
}

// candidate decides a VM that passed every exclusion and fills its targets,
// AZ, and pins.
func (b *rebalanceModelBuilder) candidate(v harebalance.VM, f *rebalanceVMFacts, pin *ha.Rule,
) (harebalance.VM, *rebalanceVMPlan) {
	az, azNodes := rebalanceAZ(b.deps, pin)
	keys := b.profile.ResourceKeys()
	in := pinWidenInput(b.deps, keys, f.vmid, f.node, f.cfg, f.res)
	in.AZ, in.AZNodes = az, azNodes
	d, err := b.profile.DecideVM(in)
	switch {
	case err != nil:
		v.Skip, v.SkipReason = harebalance.SkipUndecided, "its HA decision failed: "+err.Error()
		return v, nil
	case !d.Registers():
		v.Skip, v.SkipReason = harebalance.SkipUndecided, "its HA decision does not register it: "+d.Reason
		return v, nil
	case d.Policy == ha.PolicyHold:
		v.Skip, v.SkipReason = harebalance.SkipHold, "its HA decision gives it the hold policy"
		return v, nil
	case d.Policy == ha.PolicyReplicate:
		v.Skip, v.SkipReason = harebalance.SkipReplicate, "its HA decision gives it the replicate policy"
		return v, nil
	case d.Policy != ha.PolicyShared:
		v.Skip, v.SkipReason = harebalance.SkipLocalDisk, "its HA decision gives it no shared policy: "+d.Reason
		return v, nil
	}
	targets := intersectNodes(pin.NodeNames(), d.Admissible)
	if len(d.CPUDomain) > 0 {
		targets = intersectNodes(targets, d.CPUDomain)
	}
	v.Targets, v.Hard, v.AZ, v.AZName = targets, slices.Clone(d.Hard), azNodes, az
	plan := &rebalanceVMPlan{facts: f, pin: pin, rewrites: map[string]rebalancePinChange{}}
	placed, flat := pinPlaced(pin)
	for _, t := range targets {
		if t == f.node || flat || t == placed {
			continue
		}
		change, refused := b.rewrite(f, pin, t, az, azNodes)
		if refused != "" {
			if v.PinRefused == nil {
				v.PinRefused = map[string]string{}
			}
			v.PinRefused[t] = refused
			continue
		}
		if change == nil {
			continue
		}
		if v.Rewrite == nil {
			v.Rewrite = map[string]bool{}
		}
		v.Rewrite[t] = true
		plan.rewrites[t] = *change
	}
	return v, plan
}

// rewrite returns the pin a move of the VM to target writes, nil when the pin
// stays as it is, or why the move is refused because that pin changes more
// than the priorities of the one the VM has.
func (b *rebalanceModelBuilder) rewrite(f *rebalanceVMFacts, pin *ha.Rule, target, az string, azNodes []string,
) (*rebalancePinChange, string) {
	in := pinWidenInput(b.deps, b.profile.ResourceKeys(), f.vmid, target, f.cfg, f.res)
	in.AZ, in.AZNodes = az, azNodes
	d, err := b.profile.DecideVM(in)
	if err != nil || !d.Registers() || d.Policy != ha.PolicyShared {
		return nil, "the CPI's HA decision for the VM on " + target + " does not keep the shared policy"
	}
	newPin, err := ha.DecisionPin(d, pin.ID, haIdentity{Owner: b.owner}.marker(), ha.VolumeStorages(in.Volumes),
		profileNodeNames(b.profile), profileRebalancesOnStart(b.profile), inResourceAffinityRule(b.snap.rules, d.SID))
	if err != nil {
		return nil, "the CPI can't build the pin for the VM on " + target + ": " + err.Error()
	}
	diff := ha.RuleDiff(newPin, pin)
	switch {
	case len(diff) == 0:
		return nil, ""
	case len(diff) == 1 && diff[0] == "nodes" && slices.Equal(sortedNames(newPin), sortedNames(pin)):
		return &rebalancePinChange{d: d, pin: newPin}, ""
	}
	return nil, "the pin the CPI would write for the VM on " + target + " changes more than priorities (" +
		strings.Join(diff, ", ") + ")"
}

// replicateReason returns why a VM is a replicate VM, or "".
func (b *rebalanceModelBuilder) replicateReason(f *rebalanceVMFacts, pin *ha.Rule) string {
	switch {
	case hasReplicaTag(f.tags) || configHasReplicaTag(f.cfg):
		return "it is a replicate VM, and a move would reverse its replication job"
	case pin != nil && replicaPinComment(pin.Comment):
		return "its pin carries a token of the replicate policy, and a move would reverse its replication job"
	}
	owned, foreign, err := guestReplicaJobs(b.snap.jobs, f.vmid, b.owner)
	if err != nil || owned != nil || len(foreign) > 0 {
		return "it has a replication job, and a move would reverse it"
	}
	return ""
}

// failbackPending reports whether the VM's failback is on, no hold or mark
// holds it, and the VM sits below the highest priority of the online nodes
// of its pin, so PVE HA moves it back on its own. top names that node.
func (b *rebalanceModelBuilder) failbackPending(f *rebalanceVMFacts, pin *ha.Rule) (string, bool) {
	if f.res.Failback != nil && !f.res.Failback.Bool() {
		return "", false
	}
	if ha.FailbackHeld(f.res.Comment, b.now) {
		return "", false
	}
	online := map[string]bool{}
	for i := range b.snap.nodes {
		n := b.snap.nodes[i]
		var health *pve.NodeHealthReport
		if b.profile != nil {
			health = b.profile.Health
		}
		e := healthEntry(health, n.Name)
		online[n.Name] = n.Online && (e == nil || !e.Maintenance)
	}
	best, top := -1, ""
	for _, n := range sortedNames(pin) {
		if p := pin.Nodes[n]; online[n] && p > best {
			best, top = p, n
		}
	}
	if best < 0 {
		return "", false
	}
	if cur, ok := pin.Nodes[f.node]; ok && cur >= best {
		return "", false
	}
	return top, true
}

// rebalanceAZ reads a VM's AZ from its pin (section 7.2 of the design). The AZ
// is the nodes at priority 1, plus the priority 2 node when az_map puts it in
// the same entry. A pin with no priority 1 node takes the AZ of its priority 2
// node. A flat pin carries no AZ.
func rebalanceAZ(deps Deps, pin *ha.Rule) (string, []string) {
	placed, flat := pinPlaced(pin)
	if flat {
		return "", nil
	}
	var preferred []string
	for _, n := range sortedNames(pin) {
		if pin.Nodes[n] == ha.PinPriorityPreferred {
			preferred = append(preferred, n)
		}
	}
	if len(preferred) == 0 {
		name := soleAZOfNode(deps.Config, placed)
		if name == "" {
			return "", nil
		}
		members, _ := deps.Config.AZCandidates(name)
		return name, slices.Sorted(slices.Values(members))
	}
	name := soleAZOfNode(deps.Config, preferred[0])
	for _, n := range preferred[1:] {
		if soleAZOfNode(deps.Config, n) != name {
			name = ""
		}
	}
	nodes := preferred
	if name != "" && placed != "" && soleAZOfNode(deps.Config, placed) == name {
		nodes = append(nodes, placed)
		slices.Sort(nodes)
	}
	return name, nodes
}

// pinPlaced returns the node at the pin's priority 2, and whether every node
// of the pin has one priority, which is a flat pin.
func pinPlaced(pin *ha.Rule) (string, bool) {
	placed := ""
	prios := map[int]bool{}
	for _, n := range sortedNames(pin) {
		prios[pin.Nodes[n]] = true
		if pin.Nodes[n] == ha.PinPriorityPlaced {
			placed = n
		}
	}
	return placed, len(prios) <= 1
}

func sortedNames(pin *ha.Rule) []string {
	names := pin.NodeNames()
	slices.Sort(names)
	return names
}

// intersectNodes returns the nodes of a that b also lists, in a's order.
func intersectNodes(a, b []string) []string {
	var out []string
	for _, n := range a {
		if slices.Contains(b, n) {
			out = append(out, n)
		}
	}
	return out
}

// inPositiveAffinityRule reports whether an enabled positive resource affinity
// rule names sid.
func inPositiveAffinityRule(set *ha.RuleSet, sid string) bool {
	return positiveAffinityRule(set, sid) != ""
}

// positiveAffinityRule returns the id of the first enabled positive resource
// affinity rule that names sid, or "".
func positiveAffinityRule(set *ha.RuleSet, sid string) string {
	if set == nil {
		return ""
	}
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.Type == ha.RuleTypeResourceAffinity && r.Affinity == ha.AffinityPositive && !r.Disabled &&
			r.HasResource(sid) {
			return r.ID
		}
	}
	return ""
}

// aaGroupPattern splits a CPI anti-affinity rule's id into its group and the
// suffix of a split rule, which is -<n> for a block rule and -p<n> for a
// pattern rule.
var aaGroupPattern = regexp.MustCompile(`^(` + haRuleNamePrefix + `[0-9a-f]{12})(-p?\d+)?$`)

// rebalanceRules returns the enabled negative resource affinity rules with
// their VM members. A split rule of a CPI group belongs to the group its
// first rule names.
func rebalanceRules(set *ha.RuleSet) []harebalance.Rule {
	if set == nil {
		return nil
	}
	var out []harebalance.Rule
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.Type != ha.RuleTypeResourceAffinity || r.Affinity != ha.AffinityNegative || r.Disabled {
			continue
		}
		var members []int
		for _, sid := range r.Resources {
			if rest, ok := strings.CutPrefix(sid, "vm:"); ok {
				if vmid, err := strconv.Atoi(rest); err == nil {
					members = append(members, vmid)
				}
			}
		}
		group := r.ID
		if m := aaGroupPattern.FindStringSubmatch(r.ID); m != nil {
			group = m[1]
		}
		out = append(out, harebalance.Rule{ID: r.ID, Group: group, Members: members})
	}
	return out
}

// orNothing returns s, or "nothing" when s is empty.
func orNothing(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}
