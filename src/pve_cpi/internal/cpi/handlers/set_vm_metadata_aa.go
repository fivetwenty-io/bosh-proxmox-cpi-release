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
	return applyAntiAffinityMembership(ctx, deps, ident, vmid, aaJoinOpts{index: index, checkSelf: true}, logger)
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
