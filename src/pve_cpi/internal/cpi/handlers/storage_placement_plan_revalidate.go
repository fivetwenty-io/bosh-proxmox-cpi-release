package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

// StoragePlanInfrastructurePolicy preserves infrastructure storage and HA reachability rules.
type StoragePlanInfrastructurePolicy struct {
	OriginalISOStorage        string
	FollowRoot, RequireShared bool
}

// RevalidateStorageAllocationPlan holds the recorded identities. currentInventory
// resolves today's boundaries; originalInventory is the frozen original discovery.
// The executor supplies its live immutable ledger, including completion proofs.
func RevalidateStorageAllocationPlan(ctx context.Context, collector *inv.Collector, originalInventory, currentInventory *inv.Snapshot, current *StoragePlacementSelection, plan *StorageAllocationPlan, ledger inv.Ledger, clock func() time.Time, infrastructure ...StoragePlanInfrastructurePolicy) (*inv.Snapshot, inv.Ledger, error) {
	fail := func(err error) (*inv.Snapshot, inv.Ledger, error) {
		return originalInventory, ledger, planError(StoragePlanReconciliation, "recorded allocation %s: %v", plan.AllocationKey, err)
	}
	if plan == nil || collector == nil || originalInventory == nil || currentInventory == nil || current == nil {
		return originalInventory, ledger, planError(StoragePlanConfiguration, "revalidation inputs are required")
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if clock == nil {
		clock = time.Now
	}
	var ids []string
	for name, members := range plan.CapacityDomains {
		ids = append(ids, members...)
		currentDomain, ok := current.Policy.StorageCapacityDomains[name]
		oldMembers := slices.Clone(members)
		newMembers := slices.Clone(currentDomain.Members)
		slices.Sort(oldMembers)
		slices.Sort(newMembers)
		if !ok || !slices.Equal(oldMembers, newMembers) {
			return fail(fmt.Errorf("capacity domain %s policy changed", name))
		}
	}
	for index := range plan.Targets {
		targetIDs, err := validateRecordedStorageTarget(originalInventory, currentInventory, current, plan, &plan.Targets[index], infrastructure)
		if err != nil {
			return fail(err)
		}
		ids = append(ids, targetIDs...)
	}

	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		recorded, ok := plan.Definitions[id]
		currentDefinition, found := originalInventory.Definition(id)
		if !ok || !found || !inv.SameDefinitionSafety(recorded, currentDefinition) {
			return fail(fmt.Errorf("recorded definition for %s differs from recovery inventory", id))
		}
	}
	if err := collector.RevalidateDefinitions(ctx, originalInventory, ids); err != nil {
		return fail(err)
	}
	snapshot := originalInventory
	if !snapshot.Fresh(clock()) {
		var err error
		snapshot, err = collector.Refresh(ctx, snapshot)
		if err != nil {
			return fail(err)
		}
	}
	capacityRequest, err := currentStoragePlanCapacity(current)
	if err != nil {
		return fail(err)
	}
	refreshed, err := ledger.Refresh(snapshot, clock())
	if err != nil {
		return fail(err)
	}
	for index := range plan.Targets {
		if err := revalidateStorageTargetCapacity(snapshot, current, plan, &plan.Targets[index], refreshed, capacityRequest); err != nil {
			return fail(err)
		}
	}

	return snapshot, refreshed, nil
}

// PreflightStorageDeployment is stricter than per-create feasibility: every
// supplied deployment/HA node must reach every frozen member and infrastructure
// target. It performs no requests, allocation, or membership discovery.
func PreflightStorageDeployment(ctx context.Context, snapshot *inv.Snapshot, nodes, setNames, infrastructureIDs []string, clocks ...func() time.Time) error {
	if snapshot == nil || len(nodes) == 0 {
		return planError(StoragePlanConfiguration, "deployment inventory and nodes are required")
	}
	clock := time.Now
	if len(clocks) > 1 {
		return planError(StoragePlanConfiguration, "one preflight clock is supported")
	}
	if len(clocks) == 1 && clocks[0] != nil {
		clock = clocks[0]
	}
	if !snapshot.Fresh(clock()) {
		return planError(StoragePlanObservation, "deployment inventory is stale")
	}
	ids := slices.Clone(infrastructureIDs)
	for _, set := range setNames {
		members, ok := snapshot.Members(set)
		if !ok {
			return planError(StoragePlanConfiguration, "preflight set %s was not discovered", set)
		}
		ids = append(ids, members...)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, node := range nodes {
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return planError(StoragePlanObservation, "%v", err)
			}
			p, ok := snapshot.Pair(node, id)
			if !ok || p.Reason != "" {
				return planError(StoragePlanObservation, "deployment target %s unavailable on %s", id, node)
			}
		}
	}
	return nil
}

func currentStoragePlanCapacity(current *StoragePlacementSelection) (StoragePlanRequest, error) {
	if current.Policy == nil {
		return StoragePlanRequest{}, planError(StoragePlanConfiguration, "current capacity policy required")
	}
	if current.Root != nil {
		raw, err := json.Marshal(current.CloudProperties)
		if err != nil {
			return StoragePlanRequest{}, err
		}
		var cp createVMCloudProps
		if err := json.Unmarshal(raw, &cp); err != nil {
			return StoragePlanRequest{}, err
		}
		return ConfigureStorageVMPlanRequest(Deps{Config: current.Policy}, &createVMParsedArgs{cloudProps: cp, cloudPropsMap: current.CloudProperties}, StoragePlanRequest{}, 0)
	}
	limits := inv.Limits{MaxUtilizationPct: current.Policy.MaxUtilizationPctValue()}
	if current.Policy.ReserveStorageHeadroomEnabled() {
		mb := current.Policy.StorageHeadroomMBValue()
		if mb < 0 || uint64(mb) > math.MaxUint64/(1<<20) {
			return StoragePlanRequest{}, planError(StoragePlanConfiguration, "current headroom overflow")
		}
		limits.ReserveBytes = uint64(mb) * (1 << 20)
	}
	return StoragePlanRequest{ExtraLimits: limits}, nil
}

func validateRecordedStorageTarget(originalInventory, currentInventory *inv.Snapshot, current *StoragePlacementSelection, plan *StorageAllocationPlan, target *StoragePlanTarget, infrastructure []StoragePlanInfrastructurePolicy) ([]string, error) {
	var ids []string
	fail := func(err error) ([]string, error) { return nil, err }
	ids = append(ids, target.StorageID)
	if currentInventory.DomainForStorage(target.StorageID) != target.DomainKey {
		return fail(fmt.Errorf("recorded target capacity-domain membership changed"))
	}
	if target.Source != nil {
		ids = append(ids, target.Source.StorageID)
		for _, aux := range target.Source.AuxiliaryVolumes {
			ids = append(ids, aux.StorageID)
		}
	}
	var role *StorageRoleSelection
	switch target.Role {
	case storageRoleRoot:
		role = current.Root
	case storageRoleEphemeral:
		role = current.Ephemeral
	case storageRolePersistent:
		role = current.Persistent
	case storageRoleISO:
		d, ok := currentInventory.Definition(target.StorageID)
		if !ok || d.Disabled || !slices.Contains(strings.Split(d.Content, ","), storageRoleISO) {
			return fail(fmt.Errorf("recorded ISO target no longer permits iso content"))
		}
		if len(infrastructure) != 1 {
			return fail(fmt.Errorf("current original ISO policy is required to revalidate recorded infrastructure"))
		}
		policy := infrastructure[0]
		if (policy.RequireShared || len(plan.HANodes) > 0) && !d.IsShared() {
			return fail(fmt.Errorf("current infrastructure policy requires shared ISO"))
		}
		permitted := policy.OriginalISOStorage == target.StorageID
		if policy.FollowRoot && (policy.OriginalISOStorage == "" || policy.OriginalISOStorage == "local") {
			for rangeIndex82 := range plan.Targets {
				if plan.Targets[rangeIndex82].Role == storageRoleRoot && plan.Targets[rangeIndex82].StorageID == target.StorageID && d.IsShared() {
					permitted = true
				}
			}
		}
		if !permitted {
			return fail(fmt.Errorf("recorded ISO target is excluded by current infrastructure policy"))
		}
	}
	if role != nil && role.BoundaryName != "" {
		members, ok := currentInventory.Members(role.BoundaryName)
		if !ok || !slices.Contains(members, target.StorageID) {
			return fail(fmt.Errorf("recorded %s target %s excluded by current boundary %s", target.Role, target.StorageID, role.BoundaryName))
		}
	}
	old, ok := originalInventory.Pair(target.Node, target.StorageID)
	if !ok || old.BackingKey != target.BackingKey || old.CapacityKey != target.CapacityKey {
		return fail(fmt.Errorf("recorded target identity differs from frozen inventory"))
	}
	return ids, nil
}

func revalidateStorageTargetCapacity(snapshot *inv.Snapshot, current *StoragePlacementSelection, plan *StorageAllocationPlan, target *StoragePlanTarget, refreshed inv.Ledger, capacityRequest StoragePlanRequest) error {
	var err error
	p, ok := snapshot.Pair(target.Node, target.StorageID)
	if !ok || p.Reason != "" {
		return (fmt.Errorf("recorded target %s unavailable", target.StorageID))
	}
	for _, node := range plan.HANodes {
		p, ok := snapshot.Pair(node, target.StorageID)
		if !ok || p.Reason != "" {
			return (fmt.Errorf("recorded target %s unavailable on permitted HA node %s", target.StorageID, node))
		}
	}
	if target.Source != nil {
		sourceIDs := []string{target.Source.StorageID}
		for _, aux := range target.Source.AuxiliaryVolumes {
			sourceIDs = append(sourceIDs, aux.StorageID)
		}
		for _, id := range sourceIDs {
			p, ok := snapshot.Pair(target.Node, id)
			if !ok || p.Reason != "" {
				return (fmt.Errorf("recorded source %s unavailable on %s", id, target.Node))
			}
		}
	}
	// Zero additional bytes validates all outstanding charges exactly once.
	limits := capacityRequest.ExtraLimits
	records := refreshed.Records()
	for index := range records {
		c := &records[index]
		if c.Charge.StorageID == target.StorageID {
			limits, err = inv.CombineLimits(limits, c.Charge.Limits)
			if err != nil {
				return (err)
			}
		}
	}
	var currentRole *StorageRoleSelection
	switch target.Role {
	case storageRoleRoot:
		currentRole = current.Root
	case storageRoleEphemeral:
		currentRole = current.Ephemeral
	case storageRolePersistent:
		currentRole = current.Persistent
	}
	if currentRole != nil {
		probe := StoragePlanIterator{req: capacityRequest}
		currentLimits, e := probe.limits(*currentRole)
		if e != nil {
			return (e)
		}
		limits, err = inv.CombineLimits(limits, currentLimits)
		if err != nil {
			return (err)
		}
	}
	if _, err = refreshed.Candidate(snapshot, target.Node, target.StorageID, 0, limits); err != nil {
		return (err)
	}
	return nil
}
