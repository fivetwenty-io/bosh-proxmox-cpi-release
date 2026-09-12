package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// cleanupSettlementKey is request-local. Persisted evidence never grants a later
// invocation permission to bypass fresh task and ownership observations.
type cleanupSettlementKey struct{}
type cleanupSettlement struct {
	CompletedVMDeletions  []aj.Step                         `json:"completed_vm_deletions,omitempty"`
	CompletedHAPurges     []aj.Step                         `json:"completed_ha_purges,omitempty"`
	PendingVMVolumes      []string                          `json:"pending_vm_volumes,omitempty"`
	PendingVMAllocation   *aj.Step                          `json:"pending_vm_allocation,omitempty"`
	RecoveredTask         *aj.Step                          `json:"recovered_task,omitempty"`
	AllocationID          string                            `json:"allocation_id"`
	Attempt               int                               `json:"attempt"`
	Steps                 map[string]string                 `json:"steps"`
	TaskObservation       pve.StorageTaskSettlementEvidence `json:"tasks"`
	SharedPoolOnly        string                            `json:"shared_pool_only,omitempty"`
	CompletedUpload       *aj.Target                        `json:"completed_upload,omitempty"`
	UnknownDiskAllocation bool                              `json:"unknown_disk_allocation,omitempty"`
	CompletedDeletion     *aj.Target                        `json:"completed_deletion,omitempty"`
}

func admitStorageCleanupSettlement(ctx context.Context, deps Deps, record aj.Record, decision StorageAllocationDecision) (context.Context, *cleanupSettlement, error) {
	recovered, err := cleanupRecoveredTask(record, decision)
	if err != nil {
		return ctx, nil, err
	}
	proof := &cleanupSettlement{AllocationID: record.ID, Attempt: record.ActiveAttempt(), Steps: map[string]string{}, RecoveredTask: recovered}
	var upids []string
	for i := range record.Steps {
		original := &record.Steps[i]
		effective := *original
		if recovered != nil && recovered.ID == original.ID {
			effective = *recovered
		}
		step := &effective
		if step.State == aj.Observed || storageDecisionClosedAttemptStepSettled(record, *step) {
			continue
		}
		if step.UPID != "" {
			upids = append(upids, step.UPID)
		}
		if cleanupPendingDiskDelete(*step, record) || cleanupSubmittedVMISODelete(*step, record) || cleanupPendingVMEphemeralDelete(*step, record) {
			if proof.CompletedDeletion != nil {
				return ctx, nil, fmt.Errorf("cleanup requires a unique completed deletion")
			}
			target := step.Target
			proof.CompletedDeletion = &target
		} else if target, ok := cleanupSubmittedISOUpload(*step, record); ok {
			if proof.CompletedUpload != nil {
				return ctx, nil, fmt.Errorf("cleanup requires a unique completed ISO upload")
			}
			proof.CompletedUpload = &target
		} else if pool, ok := cleanupOnlySharedPool(record); ok {
			proof.SharedPoolOnly = pool
		} else {
			switch {
			case cleanupPendingHAPurge(*step, record):
				proof.CompletedHAPurges = append(proof.CompletedHAPurges, *step)
			case cleanupPendingVMDeletion(*step, record):
				proof.CompletedVMDeletions = append(proof.CompletedVMDeletions, *step)
			case cleanupPendingVMAllocation(*step, record):
				if proof.PendingVMAllocation != nil {
					return ctx, nil, fmt.Errorf("cleanup requires a unique pending VM allocation")
				}
				pending := *step
				proof.PendingVMAllocation = &pending
			case cleanupUnknownDiskAllocation(*step, record):
				proof.UnknownDiskAllocation = true
			case !cleanupConfigStep(*step, record):
				return ctx, nil, fmt.Errorf("cleanup refuses unresolved allocation or asynchronous mutation")
			}
		}
		hash, err := aj.Fingerprint(*original)
		if err != nil {
			return ctx, nil, err
		}
		proof.Steps[step.ID] = hash
	}
	if len(proof.Steps) == 0 {
		return ctx, nil, nil
	}
	if !decision.PreviousWriterFenced || !decision.RemoteTasksSettled || strings.TrimSpace(decision.AuthorityID) == "" {
		return ctx, nil, fmt.Errorf("pending mutation cleanup requires explicit writer fencing and independently settled remote tasks")
	}
	nodes, err := managedVMClusterNodes(ctx, deps)
	if err != nil {
		return ctx, nil, err
	}
	var rootTasks []string
	if proof.PendingVMAllocation != nil && cleanupRootTaskIdentity(*proof.PendingVMAllocation, record) {
		rootTasks = append(rootTasks, proof.PendingVMAllocation.UPID)
	}
	proof.TaskObservation, err = pve.ObserveStorageCleanupAllocationTasks(ctx, deps.PVE, nodes, upids, rootTasks)
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, cleanupSettlementKey{}, proof), proof, nil
}

func cleanupConfigStep(step aj.Step, record aj.Record) bool {
	if step.Attempt != record.ActiveAttempt() || step.State != aj.Planned || step.UPID != "" || len(step.Charges) != 0 || len(step.VolIDs) != 0 || step.Target.VMID <= 0 || step.Target.Node == "" || step.Target.External {
		return false
	}
	switch record.Kind {
	case "vm":
		if step.Kind == "vm.Nodes.UpdateQemuConfig" {
			return true
		}
		if step.Target.Storage != "" || step.Target.Backing != "" || step.Target.IntendedVolume != "" {
			return false
		}
		switch step.Kind {
		case "vm.Cluster.CreateHaResources", "vm.Cluster.UpdateHaResources":
			return len(step.Parameters) == 0
		case "vm.Cluster.CreateHaRules":
			var fields map[string]any
			if json.Unmarshal(step.Parameters, &fields) != nil || fields["version"] != float64(1) || fields["kind"] != managedVMHARuleKind {
				return false
			}
			resources, ok := fields["resources"].(string)
			if !ok {
				return false
			}
			rule, ok := fields["rule"].(string)
			if !ok || strings.TrimSpace(rule) == "" {
				return false
			}
			for _, resource := range strings.Split(resources, ",") {
				if resource == haResourceSid(step.Target.VMID) {
					return true
				}
			}
		}
		return false
	case "disk":
		return step.Kind == "park_Nodes_UpdateQemuConfig" || step.Kind == "lifecycle_detach_disk_Nodes_UpdateQemuConfig" || step.Kind == "lifecycle_delete_disk_Nodes_UpdateQemuConfig"
	default:
		return false
	}
}

// storageCleanupSettled permits only the exact pending configuration, ISO upload,
// recorded deletion, or sole shared-pool intent
// independently admitted by this explicit cleanup invocation. New failed cleanup
// steps, ordinary lifecycle calls and allocation replay retain strict refusal.
func storageCleanupSettled(ctx context.Context, record aj.Record) error {
	proof, _ := ctx.Value(cleanupSettlementKey{}).(*cleanupSettlement)
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt != record.ActiveAttempt() || step.State == aj.Observed {
			continue
		}
		if proof == nil || proof.AllocationID != record.ID || proof.Attempt != record.ActiveAttempt() {
			return fmt.Errorf("cleanup has unresolved mutation evidence")
		}
		hash, err := aj.Fingerprint(*step)
		if err != nil || proof.Steps[step.ID] != hash {
			return fmt.Errorf("cleanup has new or changed unresolved mutation evidence")
		}
	}
	return nil
}

func storageCleanupPendingOwnership(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record, settlement *cleanupSettlement, ownership aj.Verification) (aj.Verification, error) {
	if settlement == nil {
		return ownership, nil
	}
	for index := range settlement.CompletedHAPurges {
		step := &settlement.CompletedHAPurges[index]
		if err := observeCleanupHAPurge(ctx, deps, record, *step); err != nil {
			return aj.Verification{}, err
		}
	}
	for index := range settlement.CompletedVMDeletions {
		step := &settlement.CompletedVMDeletions[index]
		if err := observeCleanupVMDeletion(ctx, deps, journal, record, *step); err != nil {
			return aj.Verification{}, err
		}
	}
	if settlement.SharedPoolOnly != "" {
		return observeCleanupPoolOnlyAbsence(ctx, deps, record, settlement)
	}
	if settlement.CompletedDeletion != nil {
		return observeCompletedCleanupDeletion(ctx, deps, record, settlement, ownership)
	}
	if settlement.PendingVMAllocation != nil {
		verified, volumes, err := observeCleanupVMAllocation(ctx, deps, journal, record, *settlement.PendingVMAllocation)
		if err == nil {
			settlement.PendingVMVolumes = volumes
		}
		return verified, err
	}
	if record.Kind == "vm" && settlement.CompletedUpload != nil {
		present, err := observeCleanupUploadedISOState(ctx, deps, journal, record, *settlement.CompletedUpload)
		if err != nil {
			return aj.Verification{}, err
		}
		return aj.Verification{Complete: true, OwnershipVerified: present, AbsenceVerified: !present, ArtifactDispositionVerified: !present}, nil
	}
	if record.Kind == "vm" {
		observed, err := observeManagedVMRecord(ctx, deps, journal, record)
		if err != nil {
			return aj.Verification{}, err
		}
		ownership = observed.Verification
		if settlement.CompletedUpload != nil {
			if err := observeCleanupUploadedISO(ctx, deps, record, *settlement.CompletedUpload, observed); err != nil {
				return aj.Verification{}, err
			}
		}
	}
	if settlement.UnknownDiskAllocation && ownership.Complete && ownership.AbsenceVerified && ownership.ArtifactDispositionVerified {
		return ownership, nil
	}
	if !ownership.Complete || !ownership.OwnershipVerified {
		return aj.Verification{}, fmt.Errorf("pending configuration cleanup requires independently observed owned artifacts")
	}
	return ownership, nil
}

func cleanupPendingDiskDelete(step aj.Step, record aj.Record) bool {
	if record.Kind != "disk" || step.Attempt != record.ActiveAttempt() || (step.State != aj.Submitted && step.State != aj.Planned) || step.Kind != "lifecycle_delete_disk_Storage_DeleteVolumeAsync" || len(step.Charges) != 0 || len(step.VolIDs) != 0 || step.Target.External || step.Target.Node == "" || step.Target.Storage == "" || step.Target.Backing == "" || step.Target.IntendedVolume == "" {
		return false
	}
	parts := strings.Split(step.UPID, ":")
	vmid, validVMID := cleanupVolumeVMID(step.Target.IntendedVolume)
	if !validVMID || vmid <= 0 {
		return false
	}
	if step.State == aj.Submitted && (len(parts) != 9 || parts[1] != step.Target.Node || parts[5] != "imgdel" || parts[6] != strconv.Itoa(vmid)+"@"+step.Target.Storage) {
		return false
	}
	if step.State == aj.Planned {
		locator, id, ok := pve.ParseAllocationVolumeID(step.Target.IntendedVolume)
		if step.UPID != "" || !ok || id != record.ID || locator != pve.AllocationNamespaceLocator(record.Namespace) {
			return false
		}
	}
	storage, _, err := pve.ParseDiskCID(step.Target.IntendedVolume)
	if err != nil || storage != step.Target.Storage {
		return false
	}
	for i := range record.Steps {
		old := &record.Steps[i]
		if old.ID == step.ID {
			break
		}
		if old.Target.External || old.Target.Node != step.Target.Node || old.Target.Storage != step.Target.Storage || old.Target.Backing != step.Target.Backing {
			continue
		}
		if cleanupUnknownDiskAllocation(*old, record) && old.Target.IntendedVolume == step.Target.IntendedVolume {
			return true
		}
		if old.State != aj.Observed {
			continue
		}
		for _, volume := range old.VolIDs {
			if volume == step.Target.IntendedVolume {
				return true
			}
		}
	}
	return false
}
func observeCleanupDeletedTarget(ctx context.Context, deps Deps, target aj.Target) error {
	definition, err := managedDiskActualDefinition(ctx, deps, target.Storage)
	if err != nil || definition.BackingKey() != target.Backing {
		return fmt.Errorf("completed deletion target backing cannot be verified")
	}
	present, err := managedVolumePresent(ctx, deps, target.Node, target.IntendedVolume)
	if err != nil || present {
		return fmt.Errorf("completed deletion target absence cannot be verified")
	}
	return nil
}

func cleanupCanFinalizeDirectly(record aj.Record, settlement *cleanupSettlement) bool {
	return len(record.Steps) == 0 || settlement != nil && (settlement.CompletedDeletion != nil && record.Kind == "disk" || settlement.SharedPoolOnly != "")
}

func cleanupVolumeVMID(volume string) (int, bool) {
	if vmid, ok := pve.EmbeddedDiskVMID(volume); ok {
		return vmid, true
	}
	if _, _, ok := pve.ParseAllocationVolumeID(volume); !ok {
		if _, _, ephemeral := pve.ParseManagedEphemeralVolumeID(volume); !ephemeral {
			return 0, false
		}
	}
	_, bare, err := pve.ParseDiskCID(volume)
	if err != nil {
		return 0, false
	}
	segments := strings.Split(bare, "/")
	name := strings.TrimPrefix(segments[len(segments)-1], "vm-")
	owner, _, ok := strings.Cut(name, "-")
	if !ok {
		return 0, false
	}
	vmid, err := strconv.Atoi(owner)
	return vmid, err == nil && vmid > 0
}

// cleanupUnknownDiskAllocation admits only the original synchronous allocation
// of an exact full-UUID birth name. It does not establish ownership or absence;
// the historical audit and disk resolver must prove those before any deletion.
func cleanupUnknownDiskAllocation(step aj.Step, record aj.Record) bool {
	if record.Kind != "disk" || step.Kind != "create_persistent_volume" || step.State != aj.Planned || step.UPID != "" || len(step.VolIDs) != 0 || step.Attempt != record.ActiveAttempt() || step.Target.External || step.Target.VMID <= 0 {
		return false
	}
	var birth struct {
		Version int    `json:"version"`
		Kind    string `json:"kind"`
		Absent  bool   `json:"absence_verified"`
	}
	if json.Unmarshal(step.Parameters, &birth) != nil || birth.Version != 1 || birth.Kind != "persistent_birth" || !birth.Absent {
		return false
	}
	allocations := 0
	first := ""
	for index := range record.Steps {
		old := &record.Steps[index]
		if old.Attempt == record.ActiveAttempt() {
			if first == "" {
				first = old.ID
			}
			if old.Kind == "create_persistent_volume" {
				allocations++
			}
		}
	}
	if allocations != 1 || first != step.ID {
		return false
	}
	locator, id, ok := pve.ParseAllocationVolumeID(step.Target.IntendedVolume)
	if !ok || id != record.ID || locator != pve.AllocationNamespaceLocator(record.Namespace) {
		return false
	}
	vmid, ok := cleanupVolumeVMID(step.Target.IntendedVolume)
	if !ok || vmid != step.Target.VMID {
		return false
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil || len(plan.Targets) != 1 {
		return false
	}
	target := plan.Targets[0]
	storage, _, err := pve.ParseDiskCID(step.Target.IntendedVolume)
	if err != nil || storage != target.StorageID || target.Role != storageRolePersistent || target.Node != step.Target.Node || target.StorageID != step.Target.Storage || target.BackingKey != step.Target.Backing || target.VirtualBytes == 0 {
		return false
	}
	return cleanupDiskBirthChargesMatch(step, plan)
}

func observeCompletedCleanupDeletion(ctx context.Context, deps Deps, record aj.Record, settlement *cleanupSettlement, ownership aj.Verification) (aj.Verification, error) {
	if !ownership.Complete || !ownership.AbsenceVerified || !ownership.ArtifactDispositionVerified {
		return aj.Verification{}, fmt.Errorf("completed deletion requires complete historical artifact absence")
	}
	if record.Kind == "vm" {
		plan, err := activeStorageAllocationPlan(record)
		if err != nil {
			return aj.Verification{}, err
		}
		definition, ok := plan.Definitions[settlement.CompletedDeletion.Storage]
		if !ok {
			return aj.Verification{}, fmt.Errorf("completed VM deletion lacks frozen definition")
		}
		if err := verifyManagedVMCleanupDefinition(ctx, deps, *settlement.CompletedDeletion, definition); err != nil {
			return aj.Verification{}, err
		}
	}
	if err := observeCleanupDeletedTarget(ctx, deps, *settlement.CompletedDeletion); err != nil {
		return aj.Verification{}, err
	}
	return ownership, nil
}

func cleanupDiskBirthChargesMatch(step aj.Step, plan *StorageAllocationPlan) bool {
	if len(step.Charges) != len(plan.Charges) || len(step.Charges) == 0 {
		return false
	}
	for i, charge := range step.Charges {
		expected := plan.Charges[i]
		if charge.PlannedBytes <= 0 || uint64(charge.PlannedBytes) != expected.Charge.Bytes || charge.AcquiredBytes != 0 || charge.OutstandingBytes != charge.PlannedBytes || charge.Backing != expected.CapacityKey || charge.Domain != expected.DomainKey {
			return false
		}
	}
	return true
}
