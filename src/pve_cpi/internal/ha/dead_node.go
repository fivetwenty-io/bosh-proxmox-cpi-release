package ha

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// This file holds the pure parts of the dead-node delete completion. A delete
// of an HA-managed VM whose node PVE has fenced cannot read the VM's config,
// because PVE answers a config read on the node that owns the VM, and that
// node is gone. Once the recovery grace window expires the CPI moves the
// config off the dead node by narrowing the VM's node-affinity pin to the
// nodes that can finish the delete, and lets the CRM's recovery carry the
// config there. The functions below decide which nodes those are and plan the
// rule writes; the handler runs them under the rules lock.

// DeadNodeSourceKey is the key of the token the completion adds to the comment
// of the VM's HA resource and of its pin. Its value names the dead node the
// config left, so the delete that runs on the destination can check that node
// reaches every volume the config names, with each node-local volume counted
// as living on the dead node.
const DeadNodeSourceKey = "dead-node-from"

// DeadNodeReach is the answer of DeletionDestinations.
type DeadNodeReach struct {
	// Source is the dead node the VM's config is on.
	Source string
	// Pinned says the VM's strict pin bounded Admissible. Without one, every
	// member the recorded storages reach is admissible.
	Pinned bool
	// Admissible holds the members the VM's pin admits, Source included when
	// the pin names it, sorted.
	Admissible []string
	// Reach holds the members of Admissible other than Source, sorted.
	Reach []string
	// Destinations holds the members of Reach that the health report shows
	// placeable, sorted. Only these may receive the config.
	Destinations []string
	// Unplaceable holds the members of Reach that are not placeable, sorted,
	// such as a node with a stale LRM timestamp.
	Unplaceable []string
	// Recorded holds the storages the pin's volume record names, sorted.
	Recorded []string
	// Replica names the partner whose verified replica of the VM's node-local
	// volumes the pin records, empty when the pin records none.
	Replica string
}

// PinnedToSource reports whether the VM's strict pin admits the dead node
// alone, which is the pin of a hold VM, of a VM with a node-local volume, or
// of a VM whose hard constraints leave one node.
func (r DeadNodeReach) PinnedToSource() bool {
	return r.Pinned && len(r.Reach) == 0
}

// DeletionDestinations returns the nodes a fenced VM's config may move to so
// that its delete can complete there. The dead node's config cannot be read,
// so the VM's volumes are known only from the record the CPI keeps of them
// while the node is down: the VM's node-affinity pin, pinID, and the volume
// record its comment carries (VolumeRecordKey). The CPI writes that pin for
// every VM it registers, from the VM's eligibility decision, so a strict pin
// names the VM's hard admissible set, which is the access set of its volumes
// cut down by its other hard constraints, and a non-strict pin means every
// configured node reaches them. The record names the storages of those
// volumes, and the answer is also bounded by their reach in storages, with a
// node-local storage counted as living on source. A VM with a node-local
// volume therefore reaches source alone, unless its strict pin records a
// verified replica (ReplicaPartnerKey) on a partner the pin also names and
// every recorded node-local storage is a ZFS pool. The CPI writes that token
// only with a pin it widened after a sync it verified for every node-local
// volume, and removes it before any volume change, so while it stands the
// partner's reach of those volumes counts as well, and Replica names the
// partner. The token is a claim, not proof: the caller confirms the job that
// keeps the copy before it acts on a destination the replica adds, and the
// delete on that destination proves each copy is on the node. A disabled pin, or one PVE reports
// in error, still records the set, because neither moves a volume.
//
// The answer is an error, which the caller turns into a deferral, when the VM
// has no pin or its pin carries no volume record, because such a VM was
// registered before the CPI recorded its volumes and nothing says where they
// are. It is also an error when source is not a member, when the pin is not
// this Director's node-affinity pin of sid alone, when a strict pin names no
// node, when a strict pin leaves out source without the dead-node token
// naming source, because such a pin is not a record of the reach of the
// volumes on source, and when a recorded storage is not in storages.
//
// The destinations are the admitted members other than source that placeable
// accepts, which is a fresh LRM timestamp, online, quorate, and not in
// maintenance. The failed source's own state never counts, because it is
// never a destination. The delete on the destination checks every volume
// again against the config it then reads.
func DeletionDestinations(
	source string, members []string, set *RuleSet, sid, pinID string, owned func(*Rule) bool,
	placeable func(string) bool, storages pve.StorageInfoGetter,
) (DeadNodeReach, error) {
	out := DeadNodeReach{Source: source}
	switch {
	case strings.TrimSpace(source) == "":
		return out, errors.New("deletion destinations: the dead node is not named")
	case !slices.Contains(members, source):
		return out, fmt.Errorf("deletion destinations: dead node %s is not a cluster member", source)
	case placeable == nil:
		return out, errors.New("deletion destinations: no node health answer was given")
	case set == nil:
		return out, errors.New("deletion destinations: the HA rules were not read")
	case storages == nil:
		return out, errors.New("deletion destinations: the storage index was not read")
	case strings.TrimSpace(sid) == "" || strings.TrimSpace(pinID) == "":
		return out, errors.New("deletion destinations: the HA resource or its pin is not named")
	}
	pin := set.Find(pinID)
	switch {
	case pin == nil:
		return out, fmt.Errorf("deletion destinations: %s has no pin %q, so the CPI holds no record of where its "+
			"volumes are", sid, pinID)
	case pin.Type != RuleTypeNodeAffinity || pin.Affinity == AffinityNegative:
		return out, fmt.Errorf("deletion destinations: rule %q is not a node-affinity pin", pinID)
	case owned == nil || !owned(pin):
		return out, fmt.Errorf("deletion destinations: pin %q does not carry this Director's ownership marker",
			pinID)
	case !slices.Equal(pin.Resources, []string{sid}):
		return out, fmt.Errorf("deletion destinations: pin %q holds %s, not %s alone", pinID,
			pin.ResourcesParam(), sid)
	case pin.Strict && len(pin.Nodes) == 0:
		return out, fmt.Errorf("deletion destinations: strict pin %q names no node", pinID)
	}
	recorded, ok := VolumeRecord(pin.Comment)
	if !ok {
		return out, fmt.Errorf("deletion destinations: pin %q carries no %s record of the VM's volumes, so its "+
			"nodes say nothing about where they are", pinID, VolumeRecordKey)
	}
	out.Recorded = recorded
	vols := make([]pve.AccessVolume, 0, len(recorded))
	for _, storage := range recorded {
		vols = append(vols, pve.AccessVolume{Volume: storage + ":" + sid, HomeNode: source})
	}
	// VolumeAccessSet reads the in-memory index the caller passes, so the
	// background context never waits.
	reachable, err := pve.VolumeAccessSet(context.Background(), storages, members, vols)
	if err != nil {
		return out, fmt.Errorf("deletion destinations: the reach of the recorded storages %s: %w",
			strings.Join(recorded, ","), err)
	}
	partner, fromReplica, err := replicaReach(pin, source, sid, members, recorded, storages)
	if err != nil {
		return out, err
	}
	out.Replica = partner
	for _, n := range fromReplica {
		if !slices.Contains(reachable, n) {
			reachable = append(reachable, n)
		}
	}
	admissible := slices.Clone(reachable)
	if pin.Strict {
		if _, named := pin.Nodes[source]; !named {
			if from, ok := DeadNodeSource(pin.Comment); !ok || from != source {
				return out, fmt.Errorf("deletion destinations: strict pin %q does not name dead node %s, "+
					"which holds the VM, so it is no record of the reach of the VM's volumes", pinID, source)
			}
		}
		out.Pinned = true
		admissible = slices.DeleteFunc(admissible, func(n string) bool { _, ok := pin.Nodes[n]; return !ok })
	}
	for _, m := range admissible {
		if strings.TrimSpace(m) == "" || slices.Contains(out.Admissible, m) {
			continue
		}
		out.Admissible = append(out.Admissible, m)
		if m == source {
			continue
		}
		out.Reach = append(out.Reach, m)
		if placeable(m) {
			out.Destinations = append(out.Destinations, m)
		} else {
			out.Unplaceable = append(out.Unplaceable, m)
		}
	}
	sort.Strings(out.Admissible)
	sort.Strings(out.Reach)
	sort.Strings(out.Destinations)
	sort.Strings(out.Unplaceable)
	return out, nil
}

// replicaReach returns the partner of a verified replica that the strict pin
// records and names, with the reach of the recorded storages counted from
// that partner. A pin with no such partner returns an empty answer, and so
// does a pin whose record names a node-local storage that is not a ZFS pool,
// because replication copies no volume there and the token cannot speak for
// it. The token is the pin's claim alone: the caller confirms the job that
// keeps the copy before it counts the partner.
func replicaReach(
	pin *Rule, source, sid string, members, recorded []string, storages pve.StorageInfoGetter,
) (string, []string, error) {
	partner, ok := ReplicaPartner(pin.Comment)
	if !ok || !pin.Strict || partner == source || !slices.Contains(members, partner) {
		return "", nil, nil
	}
	if _, named := pin.Nodes[partner]; !named {
		return "", nil, nil
	}
	for _, storage := range recorded {
		info, err := storages.Get(context.Background(), storage)
		if err != nil {
			return "", nil, fmt.Errorf("deletion destinations: recorded storage %s: %w", storage, err)
		}
		if !info.IsShared() && info.Type != pve.StorageTypeZFSPool {
			return "", nil, nil
		}
	}
	replicas := make([]pve.AccessVolume, 0, len(recorded))
	for _, storage := range recorded {
		replicas = append(replicas, pve.AccessVolume{Volume: storage + ":" + sid, HomeNode: partner})
	}
	// VolumeAccessSet reads the in-memory index the caller passes, so the
	// background context never waits.
	fromReplica, err := pve.VolumeAccessSet(context.Background(), storages, members, replicas)
	if err != nil {
		return "", nil, fmt.Errorf("deletion destinations: the reach of the replica on %s: %w", partner, err)
	}
	return partner, fromReplica, nil
}

// StorageIndex serves VolumeAccessSet from one read of the storage index, so
// every volume of a config is classified from the same answer.
type StorageIndex map[string]pve.StorageInfo

// NewStorageIndex indexes storages by name. An entry with no name and a name
// that appears twice are errors, because two entries could disagree on the
// reach of the storage.
func NewStorageIndex(storages []pve.StorageInfo) (StorageIndex, error) {
	index := make(StorageIndex, len(storages))
	for i := range storages {
		info := &storages[i]
		if strings.TrimSpace(info.Name) == "" {
			return nil, errors.New("the storage index holds an entry with no name")
		}
		if _, dup := index[info.Name]; dup {
			return nil, fmt.Errorf("the storage index names storage %s twice", info.Name)
		}
		index[info.Name] = *info
	}
	return index, nil
}

// Get returns the named storage, and an error when the index does not hold it.
func (idx StorageIndex) Get(_ context.Context, name string) (pve.StorageInfo, error) {
	info, ok := idx[name]
	if !ok {
		return pve.StorageInfo{}, fmt.Errorf("storage %s is not in the storage index", name)
	}
	return info, nil
}

// AllowedNodes returns the members the CRM's recovery may place sid on under
// set: the nodes of an enabled, error-free strict node-affinity rule that holds
// sid, or every member when no such rule exists, because a non-strict rule only
// orders the nodes. Resource-affinity rules are left out, so the answer is the
// widest set the recovery could choose from once they are gone. The result is
// sorted.
func AllowedNodes(set *RuleSet, sid string, members []string) []string {
	out := slices.Clone(members)
	if set != nil {
		for i := range set.Rules {
			r := &set.Rules[i]
			if !IsConfinement(r) || r.Disabled || len(r.Errors) > 0 || !r.HasResource(sid) {
				continue
			}
			out = slices.DeleteFunc(out, func(n string) bool { _, ok := r.Nodes[n]; return !ok })
		}
	}
	sort.Strings(out)
	return out
}

// WithDeadNodeSource returns comment with the dead-node token naming source,
// replacing a token that names another node. The rest of the comment, which
// starts with the ownership marker, is kept as it was.
func WithDeadNodeSource(comment, source string) string {
	fields := strings.Fields(comment)
	kept := fields[:0]
	for _, f := range fields {
		if key, _, ok := strings.Cut(f, "="); ok && key == DeadNodeSourceKey {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(append(kept, DeadNodeSourceKey+"="+source), " ")
}

// DeadNodePinComment returns the comment of the pin the completion writes. It
// keeps the comment of the VM's current pin, cur, when owned reports that pin
// as this Director's, so tokens such as the deployment and the group survive,
// and starts from marker otherwise. Either way the dead-node token names
// source.
func DeadNodePinComment(cur *Rule, owned func(*Rule) bool, marker, source string) string {
	base := marker
	if cur != nil && owned != nil && owned(cur) && strings.TrimSpace(cur.Comment) != "" {
		base = cur.Comment
	}
	return WithDeadNodeSource(base, source)
}

// DeadNodeSource returns the dead node a comment's token names, and false when
// the comment carries no token or an empty one.
func DeadNodeSource(comment string) (string, bool) {
	for _, f := range strings.Fields(comment) {
		key, val, ok := strings.Cut(f, "=")
		if ok && key == DeadNodeSourceKey && val != "" {
			return val, true
		}
	}
	return "", false
}

// DeadNodePin is the pin the completion writes: a strict node-affinity rule
// that holds sid alone and names exactly nodes, every one at priority 0, so
// the CRM's recovery may place the config on any of them and on no other node.
func DeadNodePin(id, sid, comment string, nodes []string) Rule {
	pin := Rule{
		ID:        id,
		Type:      RuleTypeNodeAffinity,
		Affinity:  AffinityPositive,
		Strict:    true,
		Comment:   comment,
		Resources: []string{sid},
		Nodes:     make(map[string]int, len(nodes)),
	}
	for _, n := range nodes {
		pin.Nodes[n] = 0
	}
	return pin
}

// DeadNodeRuleBlockers lists why the completion may not edit the rules of sid,
// or nothing when it may. owned reports whether a rule carries this Director's
// marker. The completion writes only the pin named pinID and removes sid only
// from negative resource-affinity rules it owns, so it is blocked by:
//
//   - a rule named pinID that is not this Director's node-affinity pin of sid
//     alone, because replacing it would drop another resource's confinement or
//     edit a rule that is not ours;
//   - another node-affinity rule that holds sid, because PVE allows a resource
//     in one node-affinity rule only, and a second one would disable the pin;
//   - a positive resource-affinity rule that holds sid, because it ties the VM
//     to the nodes of other resources, which can conflict with the pin and
//     disable it;
//   - a negative resource-affinity rule that holds sid and is not ours,
//     because it can keep the CRM from recovering the VM onto a destination
//     and the CPI may not edit it.
func DeadNodeRuleBlockers(set *RuleSet, sid, pinID string, owned func(*Rule) bool) []string {
	if set == nil {
		return []string{"the HA rules were not read"}
	}
	var out []string
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.ID == pinID {
			switch {
			case r.Type != RuleTypeNodeAffinity || r.Affinity == AffinityNegative:
				out = append(out, fmt.Sprintf("rule %q is a %s %s rule, not the VM's node-affinity pin",
					r.ID, orDash(r.Affinity), r.Type))
			case owned == nil || !owned(r):
				out = append(out, fmt.Sprintf("pin %q does not carry this Director's ownership marker", r.ID))
			case !slices.Equal(r.Resources, []string{sid}):
				out = append(out, fmt.Sprintf("pin %q holds %s, not %s alone", r.ID, r.ResourcesParam(), sid))
			}
			continue
		}
		if !r.HasResource(sid) {
			continue
		}
		switch {
		case r.Type == RuleTypeNodeAffinity:
			out = append(out, fmt.Sprintf("node-affinity rule %q also holds %s", r.ID, sid))
		case r.Type != RuleTypeResourceAffinity:
			out = append(out, fmt.Sprintf("rule %q of type %s holds %s", r.ID, r.Type, sid))
		case r.Affinity != AffinityNegative:
			out = append(out, fmt.Sprintf("positive resource-affinity rule %q holds %s", r.ID, sid))
		case owned == nil || !owned(r):
			out = append(out, fmt.Sprintf("anti-affinity rule %q holds %s and does not carry this Director's "+
				"ownership marker", r.ID, sid))
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// PlanDeadNodePin is the planner of the pin write. It refuses whatever
// DeadNodeRuleBlockers names, so a rule that changed since the caller's check
// is never overwritten, and it returns no write once the pin matches want.
func PlanDeadNodePin(want Rule, owned func(*Rule) bool) Planner {
	sid := ""
	if len(want.Resources) == 1 {
		sid = want.Resources[0]
	}
	return func(set *RuleSet) ([]RuleOp, error) {
		if sid == "" || len(want.Nodes) == 0 {
			return nil, fmt.Errorf("dead-node pin %q must hold one resource and name at least one node", want.ID)
		}
		if blockers := DeadNodeRuleBlockers(set, sid, want.ID, owned); len(blockers) > 0 {
			return nil, fmt.Errorf("dead-node pin %q not written: %s", want.ID, strings.Join(blockers, "; "))
		}
		cur := set.Find(want.ID)
		switch {
		case cur == nil:
			return []RuleOp{{Kind: OpCreate, Rule: want}}, nil
		case SameRule(&want, cur):
			return nil, nil
		}
		return []RuleOp{{Kind: OpUpdate, Rule: want}}, nil
	}
}

// DeadNodePinInEffect reports whether set holds want as written, enabled, and
// with no feasibility error from PVE, and says what differs when it does not.
func DeadNodePinInEffect(set *RuleSet, want Rule) (bool, string) {
	if set == nil {
		return false, "the HA rules were not read"
	}
	got := set.Find(want.ID)
	switch {
	case got == nil:
		return false, fmt.Sprintf("pin %q is missing", want.ID)
	case got.Disabled:
		return false, fmt.Sprintf("pin %q is disabled", want.ID)
	case len(got.Errors) > 0:
		return false, fmt.Sprintf("pin %q has errors: %s", want.ID, strings.Join(got.Errors, "; "))
	}
	if diff := RuleDiff(&want, got); len(diff) > 0 {
		return false, fmt.Sprintf("pin %q differs in %s", want.ID, strings.Join(diff, ","))
	}
	return true, ""
}

// PlanAntiAffinityRelease is the planner that removes sid from every negative
// resource-affinity rule owned reports as this Director's. A rule left with
// fewer than two resources is deleted, because PVE refuses a resource-affinity
// rule with one. Any rule DeadNodeRuleBlockers would name for sid, other than
// the pin, refuses the plan.
func PlanAntiAffinityRelease(sid, pinID string, owned func(*Rule) bool) Planner {
	return func(set *RuleSet) ([]RuleOp, error) {
		if blockers := DeadNodeRuleBlockers(set, sid, pinID, owned); len(blockers) > 0 {
			return nil, fmt.Errorf("anti-affinity release of %s refused: %s", sid, strings.Join(blockers, "; "))
		}
		var ops []RuleOp
		for i := range set.Rules {
			r := &set.Rules[i]
			if r.Type != RuleTypeResourceAffinity || r.Affinity != AffinityNegative || !r.HasResource(sid) {
				continue
			}
			rest := slices.DeleteFunc(slices.Clone(r.Resources), func(s string) bool { return s == sid })
			if len(rest) < 2 {
				ops = append(ops, RuleOp{Kind: OpDelete, Rule: Rule{ID: r.ID}})
				continue
			}
			next := r.Clone()
			next.Resources = rest
			next.Errors = nil
			ops = append(ops, RuleOp{Kind: OpUpdate, Rule: next})
		}
		return ops, nil
	}
}
