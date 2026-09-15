package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

func createManagedVM(ctx context.Context, deps Deps, args []json.RawMessage, parsed *createVMParsedArgs, selection *StoragePlacementSelection) (result any, retErr error) {
	defer func() {
		if retErr != nil {
			deps.recordStoragePlacement(ctx, selection, "rejection")
		}
	}()
	nodes, err := clusterNodeNames(ctx, deps)
	if err != nil {
		return nil, err
	}
	journal, err := openStorageAllocationJournal(ctx, deps, nodes)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, journal.Close()) }()
	record, found, err := journal.InspectVMContext(ctx, parsed.agentID)
	if err != nil {
		return nil, err
	}
	if found {
		return resumeManagedVM(ctx, deps, parsed, selection, journal, record, args, continueManagedVM)
	}
	// The admission scan carries the journal's records, but the first
	// placement does not read them here. The planning callback below receives
	// a listing taken under the index lock, which is strictly fresher and is
	// the only read that closes the window against a concurrent create.
	if err := admitStorageVMAllocation(ctx, deps, journal, nodes, parsed.agentID); err != nil {
		return nil, err
	}
	prepared, err := observeManagedVMPlan(ctx, deps, parsed, selection)
	if err != nil {
		return nil, managedVMPlanCPIError(err)
	}
	// Resolve the storage tier out here. The callback runs under a cluster
	// visible lock and must not reach PVE for one.
	tier, err := managedVMTierResolver(ctx, deps, parsed, selection)
	if err != nil {
		return nil, err
	}
	placement := &managedVMFirstPlacement{deps: deps, parsed: parsed, selection: selection,
		prepared: prepared, tier: tier, args: args}
	handle, err := journal.AcquireVMPlanned(ctx, parsed.agentID, func(id string, siblings []aj.Record) (aj.Intent, error) {
		return placement.plan(ctx, id, siblings)
	})
	if err != nil {
		return resumeConflictingManagedVM(ctx, deps, parsed, selection, journal, args, err)
	}
	if handle.Resumed {
		existing := handle.Record()
		if err := handle.Close(); err != nil {
			return nil, err
		}
		return resumeManagedVM(ctx, deps, parsed, selection, journal, existing, args, continueManagedVM)
	}
	defer func() { retErr = errors.Join(retErr, handle.Close()) }()
	// A new generation is written only when the planning callback returns an
	// intent, so the shape is always here. Say so rather than dereference it.
	if placement.shape == nil {
		return nil, cpierrors.Cloud("allocation %s was acquired without a frozen VM shape", handle.Record().ID)
	}
	m, err := newManagedVMAllocation(deps, parsed, placement.shape, prepared, handle)
	if err != nil {
		return nil, err
	}
	return runManagedVMWithRetries(ctx, deps, journal, parsed, selection, m, nil)
}

// managedVMFirstPlacement carries what one first placement needs across the
// journal's planning callback. Everything on it was observed before the index
// lock was taken, and shape is the one result the callback leaves behind for
// the caller.
type managedVMFirstPlacement struct {
	deps      Deps
	parsed    *createVMParsedArgs
	selection *StoragePlacementSelection
	prepared  *managedVMPlan
	tier      vmStorageTierFn
	args      []json.RawMessage
	shape     *createVMShape
}

// plan ranks, shapes, and freezes one placement while the journal holds its
// index lock, and returns the intent the new record carries. It observes
// nothing: the siblings come from the journal's own read under that lock, the
// inventory snapshot was frozen before it, and so was the storage tier. See
// aj.VMPlanFunc for the contract the callback runs under.
func (p *managedVMFirstPlacement) plan(ctx context.Context, id string, siblings []aj.Record) (aj.Intent, error) {
	if err := applyManagedVMSiblings(p.prepared, siblings, id); err != nil {
		return aj.Intent{}, err
	}
	if err := rankManagedVMPlan(ctx, p.deps, p.parsed, p.prepared); err != nil {
		return aj.Intent{}, managedVMPlanCPIError(err)
	}
	p.parsed.storagePlan = p.prepared.plan
	p.parsed.storageSelection = p.selection
	// The frozen plan names the root storage and its type, so the shape build
	// needs no cluster storage listing of its own.
	shape, err := buildVMShapeForNode(ctx, p.deps, p.parsed, p.prepared.plan.Node, p.tier)
	if err != nil {
		return aj.Intent{}, err
	}
	p.prepared.plan.VMExecution, err = freezeManagedVMExecution(p.deps.Config, shape)
	if err != nil {
		return aj.Intent{}, err
	}
	intent, err := storageJournalIntent("create_vm", p.args, p.selection, p.prepared.inventory, p.prepared.plan)
	if err != nil {
		return aj.Intent{}, err
	}
	p.shape = shape
	return intent, nil
}

// resumeConflictingManagedVM converges on a generation that appeared after this
// call looked the agent up and before it acquired. The planning entry point has
// no caller intent to compare, so the journal reports that generation as a
// conflict rather than resuming it, and the resume path owns the comparison
// instead: it re-acquires with this call's caller fingerprint and refuses a
// generation whose caller intent differs. Any other acquisition error, and a
// conflict with no generation behind it, is returned unchanged.
func resumeConflictingManagedVM(ctx context.Context, deps Deps, parsed *createVMParsedArgs,
	selection *StoragePlacementSelection, journal *aj.Journal, args []json.RawMessage, cause error) (any, error) {
	if !errors.Is(cause, aj.ErrConflict) {
		return nil, cause
	}
	record, found, err := journal.InspectVMContext(ctx, parsed.agentID)
	if err != nil || !found {
		return nil, errors.Join(cause, err)
	}
	return resumeManagedVM(ctx, deps, parsed, selection, journal, record, args, continueManagedVM)
}

func managedVMPlanCPIError(err error) error {
	var planning *StoragePlanError
	if errors.As(err, &planning) {
		return planning.CPI()
	}
	return cpierrors.Cloud("create_vm: %s", err.Error())
}
func newManagedVMAllocation(deps Deps, parsed *createVMParsedArgs, shape *createVMShape, prepared *managedVMPlan, handle *aj.Handle) (*managedVMAllocation, error) {
	digest := sha256.Sum256([]byte(parsed.agentID))
	record := handle.Record()
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Namespace: record.Namespace, AllocationID: record.ID, AgentSHA256: hex.EncodeToString(digest[:]), Kind: "vm"})
	if err != nil {
		return nil, err
	}
	return &managedVMAllocation{deps: deps, parsed: parsed, shape: shape, prepared: prepared, handle: handle, marker: marker, volumes: map[string]string{}, chargeSteps: map[string]string{}, chargeIndices: map[string]int{}}, nil
}
func continueManagedVM(ctx context.Context, deps Deps, parsed *createVMParsedArgs, selection *StoragePlacementSelection, journal *aj.Journal, handle *aj.Handle, plan *StorageAllocationPlan, observed *managedVMObservation) (any, error) {
	if managedVMAttemptClosed(handle.Record()) {
		// Read the siblings before the re-plan. The absence proof keeps its
		// place after it, because that proof is what authorizes the retry, and
		// our own record is excluded so the retry stays free to return to the
		// share its retained artifacts already sit on.
		siblings, err := journal.List()
		if err != nil {
			return nil, err
		}
		next, err := prepareManagedVMPlan(ctx, deps, parsed, selection, siblings, handle.Record().ID)
		if err != nil {
			return nil, managedVMPlanCPIError(err)
		}
		proof, err := proveManagedVMAttemptAbsent(ctx, deps, journal, handle.Record())
		if err != nil {
			return nil, err
		}
		runtime, err := beginManagedVMRetry(ctx, deps, parsed, selection, handle, next, proof)
		if err != nil {
			return nil, err
		}
		deps.recordStoragePlacement(ctx, selection, "fallback")
		return runManagedVMWithRetries(ctx, deps, journal, parsed, selection, runtime, nil)
	}

	recorded := handle.Record()
	for i := range recorded.Steps {
		step := &recorded.Steps[i]
		if step.Attempt == recorded.ActiveAttempt() && step.Kind == "vm.keep_failed" {
			return nil, cpierrors.Cloud("allocation %s retains a failed attempt for operator inspection", handle.Record().ID)
		}
		if step.Attempt == recorded.ActiveAttempt() && step.State != aj.Observed {
			return nil, storageAllocationUncertain(handle, "unsettled recorded mutation")
		}
	}

	shape, err := plan.VMExecution.shape(deps.Config)
	if err != nil {
		return nil, cpierrors.Cloud("%s", err.Error())
	}
	if observed != nil {
		shape.node = observed.Node
	}
	parsed.storagePlan = plan
	parsed.storageSelection = selection
	prepared, err := recoverManagedVMInventory(ctx, deps, selection, plan)
	if err != nil {
		return nil, managedVMPlanCPIError(err)
	}
	m, err := newManagedVMAllocation(deps, parsed, shape, prepared, handle)
	if err != nil {
		return nil, err
	}
	if observed != nil {
		m.vmid = observed.VMID
		m.rootCreated = true
	}
	if err := m.restoreAcquiredCharges(ctx, observed); err != nil {
		return nil, err
	}
	return runManagedVMWithRetries(ctx, deps, journal, parsed, selection, m, observed)
}
func recoverManagedVMInventory(ctx context.Context, deps Deps, selection *StoragePlacementSelection, plan *StorageAllocationPlan) (*managedVMPlan, error) {
	request := inv.Request{Nodes: append([]string{plan.Node}, plan.HANodes...)}
	for id := range plan.Definitions {
		request.CompanionStorageIDs = append(request.CompanionStorageIDs, id)
	}
	for _, role := range []*StorageRoleSelection{selection.Root, selection.Ephemeral} {
		if role == nil {
			continue
		}
		if role.SetName != "" {
			request.SetNames = append(request.SetNames, role.SetName)
		}
		if role.BoundaryName != "" {
			request.SetNames = append(request.SetNames, role.BoundaryName)
		}
	}
	if selection.FuturePersistentSet != "" {
		request.SetNames = append(request.SetNames, selection.FuturePersistentSet)
	}
	slices.Sort(request.Nodes)
	request.Nodes = slices.Compact(request.Nodes)
	collector, err := inv.NewCollector(inv.PVESource{Client: deps.PVE}, inv.Options{})
	if err != nil {
		return nil, err
	}
	inventory, err := collector.Discover(ctx, selection.Policy, request)
	if err != nil {
		return nil, err
	}
	ledger := inv.NewLedger()
	return &managedVMPlan{selection: selection, collector: collector, inventory: inventory, ledger: ledger, plan: plan}, nil
}
func (m *managedVMAllocation) execute(ctx context.Context, _ *managedVMObservation) (result any, retErr error) {
	defer func() {
		if m.handle.Record().State == aj.ReconciliationRequired {
			if m.deps.Config.KeepFailedVMsEnabled() {
				retErr = errors.Join(retErr, m.retainFailedAttempt())
			}
			m.deps.recordStorageReconciliation(ctx, "required")
		}
	}()
	release, err := m.deps.Inflight.acquire(ctx, m.shape.node, m.deps.Config.MaxInflightPerNodeLimit())
	if err != nil {
		return nil, err
	}
	defer release()
	if m.vmid == 0 {
		rangeStart, _ := resolveVMIDAllocParams(m.deps.Config)
		rangeEnd := m.deps.Config.VMIDRangeEnd
		if rangeEnd == 0 {
			rangeEnd = pve.VMIDRangeVMEnd
		}
		m.vmid, err = pve.NextVMID(ctx, m.deps.PVE, pve.WithRange(rangeStart, rangeEnd))
		if err != nil {
			return nil, err
		}
	}
	if err := m.newGuard(); err != nil {
		return nil, err
	}
	m.parsed.storageRuntime = m
	guarded := m.deps
	guarded.PVE = m.guard.Client()
	if iso, ok := managedVMRoleTarget(m.prepared.plan, storageRoleISO); ok {
		cfg := m.deps.Config.WithResolvedISOStorage(iso.StorageID)
		guarded.Config = &cfg
		guarded.Agent, err = agent.NewManagedAgent(&cfg, guarded.PVE, guarded.NodeEndpoints, guarded.Log(ctx), iso.ChargeBytes, agent.ManagedAgentEvidence{ExistingISO: m.volumes[storageRoleISO], CheckPayload: m.checkAgentPayload})
		if err != nil {
			return nil, err
		}
	}
	if !m.rootCreated {
		root, _ := managedVMRoleTarget(m.prepared.plan, storageRoleRoot)
		if err := createManagedVMRoot(ctx, guarded, m.parsed, m.shape, root, m.vmid, m.marker); err != nil {
			return nil, storageAllocationUncertain(m.handle, "VM root creation")
		}
	}
	// The common post-create path owns no rollback; every nested write is
	// intercepted, and any later observation failure preserves the generation.
	result, err = finishCreatedVM(ctx, guarded, guarded.Log(ctx), m.parsed, m.shape, m.vmid, candidateVMName(m.shape.initialName, m.parsed.agentID, m.vmid))
	if err != nil {
		return nil, storageAllocationUncertain(m.handle, "VM post-create")
	}
	if err := m.guard.Err(); err != nil {
		return nil, err
	}
	if err := m.recordBindings(ctx); err != nil {
		return nil, storageAllocationUncertain(m.handle, "VM final role bindings")
	}
	record := m.handle.Record()
	record.State = aj.ReadyToReturn
	record.CID = strconv.Itoa(m.vmid)
	if err := m.handle.Save(record); err != nil {
		return nil, err
	}
	return result, nil
}
func (m *managedVMAllocation) recordBindings(ctx context.Context) error {
	cfg, err := m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
	if err != nil {
		return err
	}
	if err := m.verifyMarker(cfg); err != nil {
		return err
	}
	for i := range m.prepared.plan.Targets {
		target := &m.prepared.plan.Targets[i]
		volume := m.volumes[target.Role]
		if volume == "" {
			return fmt.Errorf("allocation role %s has no actual volume", target.Role)
		}
		if _, err = m.observeTargetVolume(ctx, *target, volume, target.VirtualBytes); err != nil {
			return err
		}
		device := ""
		for key, value := range cfg {
			drive, ok := value.(string)
			if ok && strings.Split(drive, ",")[0] == volume && managedVMVolumeDevice(key) {
				if device != "" {
					return fmt.Errorf("volume has ambiguous VM device")
				}
				device = key
			}
		}
		if device == "" {
			return fmt.Errorf("allocation role is not attached")
		}
		kind := "vm." + target.Role + "." + device
		already := false
		recorded := m.handle.Record()
		for i := range recorded.Steps {
			step := &recorded.Steps[i]
			if step.Attempt == recorded.ActiveAttempt() && step.Kind == kind && step.State == aj.Observed && len(step.VolIDs) == 1 && step.VolIDs[0] == volume {
				already = true
			}
		}
		if already {
			continue
		}
		step, err := storageMutationIntent(m.handle, kind, aj.Target{Node: m.shape.node, VMID: m.vmid, Storage: target.StorageID, Backing: target.BackingKey, IntendedVolume: volume}, nil)
		if err != nil {
			return err
		}
		if err := storageMutationObserved(m.handle, step, []string{volume}, false); err != nil {
			return err
		}
	}
	return nil
}
