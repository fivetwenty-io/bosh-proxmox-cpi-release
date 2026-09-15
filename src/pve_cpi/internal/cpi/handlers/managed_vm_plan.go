package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/configdrive"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
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
	// request is the ranking input the observation half assembled. The ranking
	// half sets the sibling fields on it and builds the iterator from it, so a
	// caller holding the journal index lock can rank without observing.
	request StoragePlanRequest
	// snapshotStart is the clock reading taken immediately before discovery
	// began, which decides whether a sibling's acquired bytes still count.
	snapshotStart time.Time
	// group is the sanitized deployment and instance group pair this
	// allocation belongs to, empty when env names no instance group.
	group string
}

// managedVMStorageGroup builds the storage anti-affinity group string, the
// sanitized deployment and instance group pair that decides which allocations
// count as siblings. It is empty whenever the instance group is empty, which
// is the common create-env shape.
//
// Each half is sanitized before the two are joined, never after, because
// sanitizeTagValue rewrites the separating slash to a dash.
//
// This never comes from antiAffinityGroupTag. That helper returns the empty
// string whenever node anti-affinity is switched off, and storage
// anti-affinity has to be independent of the node setting.
func managedVMStorageGroup(cfg *config.CPIConfig, env map[string]any) string {
	_, rawDeployment, job := poolTemplateTokensFromEnv(cfg, env)
	deployment, group := sanitizeTagValue(rawDeployment), sanitizeTagValue(job)
	if deployment == "" || group == "" {
		return ""
	}
	return "deployment--" + deployment + "/instance-group--" + group
}

// managedVMAntiAffinityScope reads the storage anti-affinity scope of the root
// role. A selection with no root role takes the package default, which is the
// value a declared set with no anti_affinity block also reports.
func managedVMAntiAffinityScope(selection *StoragePlacementSelection) string {
	if selection == nil || selection.Root == nil {
		return config.DefaultStorageAntiAffinityScope
	}
	return antiAffinityScope(*selection.Root)
}

// applyManagedVMSiblings folds the journal's records into the ranking request,
// so a peer that started moments earlier is charged against this placement and
// a share that already holds one of our instance group is ranked behind one
// that does not. self is our own allocation identifier and never charges
// against us.
func applyManagedVMSiblings(observed *managedVMPlan, records []aj.Record, self string) error {
	member, domain, counts, err := siblingCharges(records, self, observed.group,
		managedVMAntiAffinityScope(observed.selection), observed.snapshotStart)
	if err != nil {
		return err
	}
	observed.request.SiblingMemberBytes = member
	observed.request.SiblingDomainBytes = domain
	observed.request.SiblingGroupCounts = counts
	return nil
}

func clusterNodeNames(ctx context.Context, deps Deps) ([]string, error) {
	response, err := deps.PVE.Nodes().ListNodes(ctx)
	if err != nil || response == nil || len(*response) == 0 {
		return nil, cpierrors.Cloud("cluster node observation failed")
	}
	var names []string
	for _, raw := range *response {
		var row struct {
			Node string `json:"node"`
		}
		if json.Unmarshal(raw, &row) != nil || row.Node == "" {
			return nil, cpierrors.Cloud("cluster node observation malformed")
		}
		names = append(names, row.Node)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// prepareManagedVMPlan observes, ranks, and freezes one placement for a caller
// that holds no journal lock. records are the journal allocations that were in
// flight when the caller read them and self is the caller's own allocation
// identifier, which never charges against itself. The first-placement path
// calls the two halves separately, so that only the ranking runs under the
// journal index lock.
//
// Existing-generation lookup and admission belong before this function; no
// VMID is allocated here.
func prepareManagedVMPlan(ctx context.Context, deps Deps, parsed *createVMParsedArgs,
	selection *StoragePlacementSelection, records []aj.Record, self string) (*managedVMPlan, error) {
	observed, err := observeManagedVMPlan(ctx, deps, parsed, selection)
	if err != nil {
		return observed, err
	}
	if err := applyManagedVMSiblings(observed, records, self); err != nil {
		return observed, err
	}
	return observed, rankManagedVMPlan(ctx, deps, parsed, observed)
}

// observeManagedVMPlan performs only observations and returns the ranking
// request they produced. It allocates no VMID, ranks nothing, and freezes
// nothing, so every PVE round trip a placement needs has been made by the time
// it returns.
func observeManagedVMPlan(ctx context.Context, deps Deps, parsed *createVMParsedArgs,
	selection *StoragePlacementSelection) (*managedVMPlan, error) {
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
	// Read the clock before discovery rather than after it. Every observation
	// the snapshot carries is stamped at or after this moment, so a sibling
	// that acquired bytes this snapshot cannot yet see is counted rather than
	// dropped, which is the direction the accounting deliberately errs in.
	snapshotStart := time.Now().UTC()
	snapshot, err := collector.Discover(ctx, selection.Policy, discovery)
	if err != nil {
		return nil, err
	}
	observed := &managedVMPlan{selection: selection, collector: collector, inventory: snapshot,
		snapshotStart: snapshotStart, group: managedVMStorageGroup(deps.Config, parsed.env)}
	namespace := deps.Config.StoragePlacementNamespace
	if namespace == "" && !selection.SetManaged {
		namespace = "future-access-preflight"
	}
	request, err := ConfigureStorageVMPlanRequest(deps, parsed, StoragePlanRequest{Selection: selection,
		Inventory: snapshot, Groups: groups, Namespace: namespace, AllocationKey: parsed.agentID,
		Sources: sources, Existing: existing, Group: observed.group,
		SearchBudget: managedVMSearchBudget(snapshot, groups)}, isoBytes)
	if err != nil {
		return observed, err
	}
	request.OnCandidateRejected = deps.storageCandidateRejectionObserver(ctx, selection)
	observed.request = request
	return observed, nil
}

// rankManagedVMPlan turns an observed request into a frozen plan. It observes
// nothing: the iterator, the ranking, and the execution ledger are in-memory
// work over the snapshot the observation half froze, which is what lets the
// first-placement path run this under the journal index lock.
func rankManagedVMPlan(ctx context.Context, deps Deps, parsed *createVMParsedArgs, observed *managedVMPlan) error {
	iterator, err := NewStoragePlanIterator(observed.request)
	if err != nil {
		return err
	}
	observed.iterator = iterator
	plan, err := iterator.Next(ctx)
	if err != nil {
		return err
	}
	// A root that falls back to a full clone under storage-set replicas
	// means the placed member has no cache template of its own. Say so on
	// the create rather than leaving the operator to read clone timings.
	// Gated on the property so a deployment without replicas is never told
	// to build them. Fires once per plan attempt.
	if root, ok := managedVMRoleTarget(plan, storageRoleRoot); ok &&
		root.Mechanism == storageMechanismFullClone && root.Source != nil &&
		root.Source.TemplateVMID > 0 && deps.Config.StemcellReplicateStorageSetEnabled() {
		sha8, _ := extractSHA8FromParsed(parsed)
		deps.Log(ctx).Warn("create_vm: no cache template on placed storage; cloning in full",
			log.String("storage", root.StorageID),
			log.String("sha8", sha8),
			log.String("hint", "run bosh upload-stemcell --fix to build the replica"))
	}
	ledger := inv.NewLedger()
	for i := range plan.Charges {
		charge := &plan.Charges[i]
		ledger, err = ledger.WithPlanned(observed.inventory, charge.Charge)
		if err != nil {
			return err
		}
	}
	observed.plan = plan
	observed.ledger = ledger
	return nil
}

// managedVMTierResolver builds the storage-tier resolver the managed create_vm
// shape build uses while the journal holds its index lock. Every tier name a
// shape build can ask for is already named in the parsed cloud properties or in
// an atomic root selector, so each one is resolved here, ahead of the lock, and
// the returned closure answers from that cache. The index lock is cluster
// visible and every create in the namespace queues behind the planning
// callback, so one live PVE query inside it would serialize the namespace on a
// round trip.
//
// A failed resolution is cached rather than returned, because the managed path
// takes its root storage from the frozen plan and never consults a tier at all.
// Surfacing the failure here would reject a create that would otherwise
// succeed, while a caller that does consult the tier still sees the real error.
//
// It returns a nil resolver when the cluster storage lister is unavailable,
// which is the permissive default buildVMShapeForNode applies on its own.
func managedVMTierResolver(ctx context.Context, deps Deps, parsed *createVMParsedArgs,
	selection *StoragePlacementSelection) (vmStorageTierFn, error) {
	if deps.PVE == nil || deps.PVE.ClusterStorage() == nil {
		return nil, nil
	}
	names, err := managedVMTierNames(parsed, selection, deps.Config)
	if err != nil {
		return nil, err
	}
	type resolution struct {
		id  string
		err error
	}
	lister, cfg := deps.PVE.ClusterStorage(), deps.Config
	resolved := make(map[string]resolution, len(names))
	for _, name := range names {
		// The VM root disk does not apply the encrypted filter, which covers
		// persistent and ephemeral disks only, so encrypted is false here.
		id, tierErr := resolveStorageTier(ctx, lister, cfg, name, false)
		resolved[name] = resolution{id: id, err: tierErr}
	}
	return func(tier string) (string, error) {
		answer, ok := resolved[tier]
		if !ok {
			return "", cpierrors.Cloud(
				"create_vm: storage tier %q was not resolved before the allocation lock", tier)
		}
		return answer.id, answer.err
	}, nil
}

// managedVMTierNames lists every storage tier a shape build can name: the one a
// resolver layer declares through cloud_properties.storage_tier, and the one an
// atomic root selector carries.
func managedVMTierNames(parsed *createVMParsedArgs, selection *StoragePlacementSelection,
	cfg *config.CPIConfig) ([]string, error) {
	resolver, err := newLayeredResolver(parsed.cloudPropsMap, cfg)
	if err != nil {
		return nil, err
	}
	var names []string
	if tier, ok := resolver.String("storage_tier"); ok && tier != "" {
		names = append(names, tier)
	}
	if selection != nil && selection.Root != nil && selection.Root.Atomic &&
		selection.Root.Kind == storageSelectorTier && selection.Root.Value != "" {
		names = append(names, selection.Root.Value)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
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
