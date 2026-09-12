package handlers

import (
	"context"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	"math"
	"slices"
)

// managedDiskLifecycleCapacity admits new bytes at one actual historical target.
// It restores frozen domain membership and limits without resolving current sets.
func managedDiskLifecycleCapacity(ctx context.Context, deps Deps, record aj.Record, node, storage, backing string, bytes uint64) ([]inv.ChargeRecord, error) {
	return managedDiskLifecycleCapacityFrom(ctx, deps, record, node, storage, backing, bytes, inv.PVESource{Client: deps.PVE})
}

func managedDiskLifecycleCapacityFrom(ctx context.Context, deps Deps, record aj.Record, node, storage, backing string, bytes uint64, source inv.Source) ([]inv.ChargeRecord, error) {
	if bytes == 0 || bytes > math.MaxInt64 {
		return nil, fmt.Errorf("lifecycle capacity charge outside supported range")
	}
	if deps.Config == nil {
		return nil, fmt.Errorf("lifecycle capacity requires configuration")
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return nil, err
	}
	policy := deps.Config.CloneStoragePlacement()
	policy.StorageSets = nil
	policy.RootStorageSet = ""
	policy.EphemeralStorageSet = ""
	policy.PersistentStorageSet = ""
	policy.StorageCapacityDomains = map[string]config.StorageCapacityDomain{}
	for name, members := range plan.CapacityDomains {
		policy.StorageCapacityDomains[name] = config.StorageCapacityDomain{Members: slices.Clone(members)}
	}
	collector, err := inv.NewCollector(source, inv.Options{})
	if err != nil {
		return nil, err
	}
	snapshot, err := collector.Discover(ctx, &policy, inv.Request{Nodes: []string{node}, CompanionStorageIDs: []string{storage}})
	if err != nil {
		return nil, err
	}
	definition, ok := snapshot.Definition(storage)
	if !ok || definition.BackingKey() != backing {
		return nil, fmt.Errorf("lifecycle target backing changed before admission")
	}
	for _, members := range plan.CapacityDomains {
		if !slices.Contains(members, storage) {
			continue
		}
		for _, id := range members {
			prior, had := plan.Definitions[id]
			current, found := snapshot.Definition(id)
			if !had || !found || prior.BackingKey() != current.BackingKey() {
				return nil, fmt.Errorf("frozen capacity domain backing changed")
			}
		}
	}
	current, err := currentStoragePlanCapacity(&StoragePlacementSelection{Policy: deps.Config})
	if err != nil {
		return nil, err
	}
	pair, ok := snapshot.Pair(node, storage)
	if !ok || pair.BackingKey != backing {
		return nil, fmt.Errorf("lifecycle target observation is unavailable")
	}
	limits := current.ExtraLimits
	for chargeIndex := range plan.Charges {
		charge := &plan.Charges[chargeIndex]
		if charge.Charge.StorageID == storage || charge.CapacityKey == pair.CapacityKey {
			limits, err = inv.CombineLimits(limits, charge.Charge.Limits)
			if err != nil {
				return nil, err
			}
		}
	}
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		for _, charge := range step.Charges {
			if charge.OutstandingBytes != 0 {
				return nil, fmt.Errorf("unsettled allocation charge requires reconciliation before new lifecycle capacity")
			}
		}
	}
	ledger, err := inv.NewLedger().WithPlanned(snapshot, inv.Charge{ID: fmt.Sprintf("lifecycle-%s-%d", record.ID, len(record.Steps)), Role: "persistent_lifecycle", Node: node, StorageID: storage, Bytes: bytes, Limits: limits})
	if err != nil {
		return nil, err
	}
	return ledger.Records(), nil
}
