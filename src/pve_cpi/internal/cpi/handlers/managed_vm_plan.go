package handlers

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/configdrive"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

type managedVMPlan struct {
	iterator  *StoragePlanIterator
	selection *StoragePlacementSelection
	collector *inv.Collector
	inventory *inv.Snapshot
	plan      *StorageAllocationPlan
	ledger    inv.Ledger
}

func managedVMClusterNodes(ctx context.Context, deps Deps) ([]string, error) {
	response, err := deps.PVE.Nodes().ListNodes(ctx)
	if err != nil || response == nil || len(*response) == 0 {
		return nil, cpierrors.Cloud("managed VM cluster node observation failed")
	}
	var names []string
	for _, raw := range *response {
		var row struct {
			Node string `json:"node"`
		}
		if json.Unmarshal(raw, &row) != nil || row.Node == "" {
			return nil, cpierrors.Cloud("managed VM cluster node observation malformed")
		}
		names = append(names, row.Node)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// prepareManagedVMPlan performs only observations. Existing-generation lookup
// and admission belong before this function; no VMID is allocated here.
func prepareManagedVMPlan(ctx context.Context, deps Deps, parsed *createVMParsedArgs, selection *StoragePlacementSelection) (*managedVMPlan, error) {
	groups, err := ObserveStoragePlanCompute(ctx, deps, parsed, nil)
	if err != nil {
		return nil, err
	}
	discovery := inv.Request{}
	if selection.FuturePersistentSet != "" {
		discovery.SetNames = append(discovery.SetNames, selection.FuturePersistentSet)
	}
	for _, g := range groups {
		discovery.Nodes = append(discovery.Nodes, g.Nodes...)
		discovery.Nodes = append(discovery.Nodes, g.HANodes...)
	}
	slices.Sort(discovery.Nodes)
	discovery.Nodes = slices.Compact(discovery.Nodes)
	existing, err := ObserveStorageExistingVolumes(ctx, deps, parsed.diskCIDs)
	if err != nil {
		return nil, err
	}
	for _, disk := range existing {
		discovery.CompanionStorageIDs = append(discovery.CompanionStorageIDs, disk.StorageID)
	}
	for _, role := range []*StorageRoleSelection{selection.Root, selection.Ephemeral} {
		if role == nil {
			continue
		}
		if role.SetName != "" {
			discovery.SetNames = append(discovery.SetNames, role.SetName)
		}
		if role.BoundaryName != "" {
			discovery.SetNames = append(discovery.SetNames, role.BoundaryName)
		}
		switch role.Kind {
		case storageSelectorPool:
			discovery.CompanionStorageIDs = append(discovery.CompanionStorageIDs, role.Value)
		case storageSelectorTier:
			discovery.TierNames = append(discovery.TierNames, role.Value)
		}
	}
	sources, err := observeManagedVMRootSources(ctx, deps, parsed, discovery.Nodes)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		discovery.CompanionStorageIDs = append(discovery.CompanionStorageIDs, source.StorageID)
		for _, aux := range source.AuxiliaryVolumes {
			discovery.CompanionStorageIDs = append(discovery.CompanionStorageIDs, aux.StorageID)
		}
	}
	isoBytes := uint64(0)
	if deps.Config.AgentMode != config.AgentModeNoAgent {
		isoBytes = configdrive.AllocationBytes()
		if iso := deps.Config.OriginalISOStorage(); iso != "" && (iso != "local" || !deps.Config.ISOStorageFollowVMStorageEnabled()) {
			discovery.CompanionStorageIDs = append(discovery.CompanionStorageIDs, iso)
		}
	}
	collector, err := inv.NewCollector(inv.PVESource{Client: deps.PVE}, inv.Options{})
	if err != nil {
		return nil, err
	}
	snapshot, err := collector.Discover(ctx, selection.Policy, discovery)
	if err != nil {
		return nil, err
	}
	observed := &managedVMPlan{selection: selection, collector: collector, inventory: snapshot}
	namespace := deps.Config.StoragePlacementNamespace
	if namespace == "" && !selection.SetManaged {
		namespace = "future-access-preflight"
	}
	request, err := ConfigureStorageVMPlanRequest(deps, parsed, StoragePlanRequest{Selection: selection, Inventory: snapshot, Groups: groups, Namespace: namespace, AllocationKey: parsed.agentID, Sources: sources, Existing: existing, SearchBudget: managedVMSearchBudget(snapshot, groups)}, isoBytes)
	if err != nil {
		return observed, err
	}
	request.OnCandidateRejected = deps.storageCandidateRejectionObserver(ctx, selection)
	iterator, err := NewStoragePlanIterator(request)
	if err != nil {
		return observed, err
	}
	observed.iterator = iterator
	plan, err := iterator.Next(ctx)
	if err != nil {
		return observed, err
	}
	ledger := inv.NewLedger()
	for i := range plan.Charges {
		charge := &plan.Charges[i]
		ledger, err = ledger.WithPlanned(snapshot, charge.Charge)
		if err != nil {
			return nil, err
		}
	}
	return &managedVMPlan{iterator: iterator, selection: selection, collector: collector, inventory: snapshot, plan: plan, ledger: ledger}, nil
}

func managedVMSearchBudget(snapshot *inv.Snapshot, groups []StoragePlanNodeGroup) int {
	// Finite upper bound on nodes times root/ephemeral memberships. The iterator
	// remains lazy and does not allocate the Cartesian product.
	nodes := 0
	for _, g := range groups {
		nodes += len(g.Nodes)
	}
	count := len(snapshot.Nodes())
	// A defensive finite CPU budget also bounds unusually broad regex manifests.
	return max(1, min(100000, max(nodes, count)*1024))
}

func observeManagedVMRootSources(ctx context.Context, deps Deps, parsed *createVMParsedArgs, nodes []string) ([]StorageRootSource, error) {
	var sources []StorageRootSource
	if resolveStemcellStrategy(deps.Config, parsed) == config.StemcellStrategyTemplate {
		if sha, ok := extractSHA8FromParsed(parsed); ok {
			refs, err := pve.FindTemplatesBySHATagClusterTolerant(ctx, deps.PVE, sha)
			if err != nil {
				return nil, cpierrors.Cloud("managed VM template inventory unavailable")
			}
			for _, ref := range refs {
				_, rootKey, err := resolveTemplateDiskStorage(ctx, deps, ref.Node, ref.VMID)
				if err != nil {
					return nil, err
				}
				if rootKey != rootDiskKey(deps.Config) {
					return nil, cpierrors.Cloud("managed VM template root bus differs from requested bus")
				}
				source, err := ObserveStorageTemplateRoot(ctx, deps, ref.Node, int(ref.VMID), rootKey)
				if err != nil {
					return nil, err
				}
				sources = append(sources, source)
			}
		}
	}
	// Direct import is a real pre-observed alternative; it is never discovered
	// after a clone submission fails. Inaccessible node copies are excluded.
	for _, node := range nodes {
		source, err := ObserveStorageRootSource(ctx, deps.PVE.Nodes(), node, parsed.rawVolid, 0)
		if err == nil {
			sources = append(sources, source)
		}
	}
	if len(sources) == 0 {
		return nil, cpierrors.Cloud("managed VM has no readable compatible stemcell source")
	}
	return sources, nil
}

func managedVMRoleTarget(plan *StorageAllocationPlan, role string) (StoragePlanTarget, bool) {
	if plan != nil {
		for i := range plan.Targets {
			target := &plan.Targets[i]
			if target.Role == role {
				return *target, true
			}
		}
	}
	return StoragePlanTarget{}, false
}
