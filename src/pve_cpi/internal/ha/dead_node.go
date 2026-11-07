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
// of the VM's HA resource. Its value names the dead node the config left, so
// the delete that runs on the destination can check that node reaches every
// volume the config names, with each node-local volume counted as living on
// the dead node.
const DeadNodeSourceKey = "dead-node-from"

// deadNodeReachVolume is the volume name VolumeAccessSet is handed for a
// storage the dead node's VM may hold volumes on. VolumeAccessSet needs a
// storage-qualified volume ID, and only the storage part decides the reach.
const deadNodeReachVolume = "dead-node-reach"

// guestContentTypes are the PVE storage content types a QEMU config can name:
// disk images, container volumes, ISO images, and snippets. A storage that
// carries none of them, such as a backup-only one, cannot hold a volume of the
// VM.
var guestContentTypes = []string{"images", "rootdir", "iso", "snippets"}

// StorageMayHoldGuestVolume reports whether a VM on the dead node source can
// hold a volume on the storage. A storage whose node list leaves out source is
// not one PVE activates there, so the VM could not use it. A storage with no
// content list, or one that names a guest content type, may hold a volume.
// A disabled storage still counts, because disabling a storage does not move
// the volumes on it.
func StorageMayHoldGuestVolume(info pve.StorageInfo, source string) bool {
	if len(info.Nodes) > 0 && !slices.Contains(info.Nodes, source) {
		return false
	}
	content := strings.TrimSpace(info.Content)
	if content == "" {
		return true
	}
	for _, c := range strings.Split(content, ",") {
		if slices.Contains(guestContentTypes, strings.TrimSpace(c)) {
			return true
		}
	}
	return false
}

// DeadNodeReach is the answer of DeletionDestinations.
type DeadNodeReach struct {
	// Source is the dead node the VM's config is on.
	Source string
	// Reach holds the members other than Source that reach every storage the
	// VM may hold volumes on, sorted.
	Reach []string
	// Destinations holds the members of Reach that the health report shows
	// placeable, sorted. Only these may receive the config.
	Destinations []string
	// Blocking names the storages that no member other than Source reaches,
	// sorted. A node-local storage on Source and a disabled storage are among
	// them, because the VM's volumes there cannot leave the dead node.
	Blocking []string
	// Unplaceable holds the members of Reach that are not placeable, sorted,
	// such as a node with a stale LRM timestamp.
	Unplaceable []string
}

// DeletionDestinations returns the nodes a fenced VM's config may move to so
// that its delete can complete there. The dead node's config cannot be read,
// so the VM's own volume list is unknown. The answer is computed from every
// storage the VM may hold a volume on (StorageMayHoldGuestVolume) instead,
// which is a superset of the volumes the config names, so a node it returns
// reaches every volume whatever the config holds. Each storage contributes
// its reach as VolumeAccessSet computes it for a volume homed on source: a
// node-local storage reaches source alone, a disabled one reaches no node,
// and a shared one reaches its node list or every member. The destinations
// are the members other than source in the intersection that placeable
// accepts, which is a fresh LRM timestamp, online, quorate, and not in
// maintenance. The failed source's own state never counts, because it is
// never a destination.
//
// storages is the whole storage index. A duplicate storage name is an error,
// because the two entries could disagree on the reach.
func DeletionDestinations(
	ctx context.Context, source string, members []string, storages []pve.StorageInfo, placeable func(string) bool,
) (DeadNodeReach, error) {
	out := DeadNodeReach{Source: source}
	switch {
	case strings.TrimSpace(source) == "":
		return out, errors.New("deletion destinations: the dead node is not named")
	case !slices.Contains(members, source):
		return out, fmt.Errorf("deletion destinations: dead node %s is not a cluster member", source)
	case placeable == nil:
		return out, errors.New("deletion destinations: no node health answer was given")
	}
	index, err := NewStorageIndex(storages)
	if err != nil {
		return out, fmt.Errorf("deletion destinations: %w", err)
	}
	reach := make(map[string]bool, len(members))
	for _, m := range members {
		if m != "" && m != source {
			reach[m] = true
		}
	}
	names := make([]string, 0, len(index))
	for name := range index {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		info := index[name]
		if !StorageMayHoldGuestVolume(info, source) {
			continue
		}
		nodes, err := pve.VolumeAccessSet(ctx, index, members, []pve.AccessVolume{
			{Volume: name + ":" + deadNodeReachVolume, HomeNode: source},
		})
		if err != nil {
			return out, fmt.Errorf("deletion destinations: reach of storage %s: %w", name, err)
		}
		for m := range reach {
			if !slices.Contains(nodes, m) {
				delete(reach, m)
			}
		}
		if !slices.ContainsFunc(nodes, func(n string) bool { return n != source }) {
			out.Blocking = append(out.Blocking, name)
		}
	}
	for m := range reach {
		out.Reach = append(out.Reach, m)
		if placeable(m) {
			out.Destinations = append(out.Destinations, m)
		} else {
			out.Unplaceable = append(out.Unplaceable, m)
		}
	}
	sort.Strings(out.Reach)
	sort.Strings(out.Destinations)
	sort.Strings(out.Unplaceable)
	return out, nil
}

// StorageIndex serves VolumeAccessSet from one read of the storage index, so
// every storage is classified from the same answer.
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
