package handlers

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// joinAntiAffinityAtMetadata puts the VM into the rules of its anti-affinity
// group once set_vm_metadata has written its metadata. The rules choose their
// members by instance index, and the Director names the index only here, so
// create_vm stages the VM's HA resource and joins no rule. The group is read
// from the marker on the VM's own HA resource, which create_vm wrote, so a VM
// with no HA resource, or one this Director did not mark, joins nothing. The
// index comes from the metadata, because the VM's index tag can still be
// missing from the cluster's guest list right after the write. The join puts
// the VM through the same feasibility checks as every other member, so a join
// that would make the rules conflict leaves it out instead of sending a write
// PVE refuses. A failure is logged and never fails the call, and the next plan
// of the group, or pve-ops ha --fix-anti-affinity, places the VM.
func joinAntiAffinityAtMetadata(ctx context.Context, deps Deps, vmid int, metadata map[string]any, logger *log.Logger) {
	if !antiAffinityJoinEnabled(deps.Config) || deps.RequestDirectorUUID == "" {
		return
	}
	deps = withRequestHAProfile(deps)
	rule, err := joinAntiAffinityGroupOf(ctx, deps, vmid, metadataIndex(metadata), logger)
	switch {
	case err != nil:
		logger.Warn("set_vm_metadata: the VM could not join its anti-affinity group's rules; the next write of "+
			"the group's rules, or pve-ops ha --fix-anti-affinity, places it", log.Err(err))
	case rule != "":
		logger.Info("set_vm_metadata: the VM is in its anti-affinity group's rule", log.String("rule", rule))
	}
}

// joinAntiAffinityGroupOf reads the group of the VM from its HA resource and
// joins it, returning the rule the VM was placed in, or "" when it was placed
// in none or has no group to join.
func joinAntiAffinityGroupOf(ctx context.Context, deps Deps, vmid, index int, logger *log.Logger) (string, error) {
	resources, err := listHAResources(ctx, deps.PVE.Cluster())
	if err != nil {
		return "", fmt.Errorf("anti-affinity: read the HA resources: %w", err)
	}
	comment, ok := resources[haResourceSid(vmid)]
	if !ok || haOwnershipOf(comment, deps.RequestDirectorUUID) != haOwnedByUs {
		return "", nil
	}
	m, _ := parseHaMarker(comment)
	ident := haIdentity{Owner: m.Owner, Deployment: m.Deployment, Group: m.Group}
	// The move runs outside the rules lock, which would otherwise hold every
	// sibling's join for the whole migration. The locked join plans again from
	// fresh reads, and the layout places members by index alone, so the move
	// changes no rule. Its claim lasts until the rule is written, so a sibling
	// that joins meanwhile sees this VM on its way to the new node.
	release := moveJoinerOffPartners(ctx, deps, ident, vmid, index, m.Born, logger)
	defer release()
	if rule, ok := antiAffinityJoinSettled(ctx, deps, ident, vmid, index); ok {
		return rule, nil
	}
	return applyAntiAffinityMembership(ctx, deps, ident, vmid, aaJoinOpts{index: index, checkSelf: true}, logger)
}

// antiAffinityJoinSettled plans the VM's group from a read made without the
// rules lock, and returns the rule that holds the VM when that plan writes
// nothing. The rule writer writes nothing for such a plan under the lock
// either, so the join can skip the lock, and a parallel deploy does not queue
// every set_vm_metadata call behind it. Any doubt, such as a failed read, a
// plan that would write a rule or a hold on a member's HA resource, a VM in
// no rule, a member's pin whose priorities the locked join would restore, or
// an unmarked rule from an older CPI that the locked join would migrate,
// sends the join down the locked path. The plan here records no hold itself,
// because a hold it wrote from a read made without the lock could put back
// one that a locked plan has just consumed. A group whose holds are all
// where they belong, which is the common case, still skips the lock. Without
// the rules lock there is nothing to skip, so it reports false.
func antiAffinityJoinSettled(ctx context.Context, deps Deps, ident haIdentity, vmid, index int) (string, bool) {
	if !deps.Config.ClusterLockEnabled() || !ident.valid() || ident.Owner == "" {
		return "", false
	}
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return "", false
	}
	scan, err := collectGroupMemberSids(ctx, deps, ident)
	if err != nil {
		return "", false
	}
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		return "", false
	}
	if legacy := legacyRuleName(ident); legacy != ident.ruleName() && set.Find(legacy) != nil {
		return "", false
	}
	// The locked join plans and logs again when this plan finds work, so this
	// one logs nothing.
	planner := newGroupPlanner(ctx, deps, profile, ident, scan, log.NewNopLogger())
	planner.sid, planner.index, planner.checkSelf = haResourceSid(vmid), index, true
	planner.recordHolds = nil
	ops, err := planner.plan(set)
	if err != nil || len(ops) > 0 || len(planner.pendingHolds) > 0 || planner.placedIn == "" ||
		len(planner.restoreOps(set)) > 0 {
		return "", false
	}
	return planner.placedIn, true
}

// metadataIndex returns the instance index in set_vm_metadata's metadata, or
// -1 when it holds none. The Director sends the index as a string, and a JSON
// number is read too. An index is a whole number written with no sign and no
// leading zero, the form the VM's index tag takes.
func metadataIndex(metadata map[string]any) int {
	var s string
	switch v := metadata["index"].(type) {
	case string:
		s = v
	case float64:
		if v < 0 || v != math.Trunc(v) || v > math.MaxInt32 {
			return -1
		}
		s = strconv.FormatInt(int64(v), 10)
	default:
		return -1
	}
	if n, ok := canonicalIndex(s, 9); ok {
		return n
	}
	return -1
}
