package handlers

import (
	"context"
	"encoding/json"
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

// admitStorageCleanupSettlement admits the open steps of record that this
// cleanup can settle by fresh observation. gaps are the reasons lock and
// protection settlement left steps planned, and a refusal of a step it cannot
// admit names that step with its own reason. A refusal for missing
// attestations names the first step it admitted and the storage-journal flags
// the decision lacks.
func admitStorageCleanupSettlement(ctx context.Context, deps Deps, record aj.Record, decision StorageAllocationDecision, gaps map[string]error) (context.Context, *cleanupSettlement, error) {
	recovered, err := cleanupRecoveredTask(record, decision)
	if err != nil {
		return ctx, nil, err
	}
	proof := &cleanupSettlement{AllocationID: record.ID, Attempt: record.ActiveAttempt(), Steps: map[string]string{}, RecoveredTask: recovered}
	var upids []string
	admitted := ""
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
				return ctx, nil, storageRefusal("cleanup requires a unique completed deletion")
			}
			target := step.Target
			proof.CompletedDeletion = &target
		} else if target, ok := cleanupSubmittedISOUpload(*step, record); ok {
			if proof.CompletedUpload != nil {
				return ctx, nil, storageRefusal("cleanup requires a unique completed ISO upload")
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
					return ctx, nil, storageRefusal("cleanup requires a unique pending VM allocation")
				}
				pending := *step
				proof.PendingVMAllocation = &pending
			case cleanupUnknownDiskAllocation(*step, record):
				proof.UnknownDiskAllocation = true
			case cleanupPersistentHandoffStep(*step, record):
			case !cleanupConfigStep(*step, record):
				return ctx, nil, storageRefusal("cleanup refuses unresolved allocation or asynchronous mutation; " + unsettledStepName(*original) + unsettledStepReason(*original, gaps))
			}
		}
		hash, err := aj.Fingerprint(*original)
		if err != nil {
			return ctx, nil, err
		}
		proof.Steps[step.ID] = hash
		if admitted == "" {
			admitted = unsettledStepName(*original)
		}
	}
	if len(proof.Steps) == 0 {
		return ctx, nil, nil
	}
	if missing := cleanupMissingAttestations(decision); len(missing) > 0 {
		return ctx, nil, storageRefusal("pending mutation cleanup requires explicit writer fencing and independently settled remote tasks; " + admitted + "; rerun with " + joinWithOxfordComma(missing))
	}
	nodes, err := clusterNodeNames(ctx, deps)
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

// cleanupMissingAttestations names the storage-journal flags that a cleanup
// of pending mutations needs and decision lacks, always in the same order. The
// names are the CLI's flags in cmd/cpi/storage_journal.go, because that CLI is
// the only production caller of CleanupStorageAllocation.
func cleanupMissingAttestations(decision StorageAllocationDecision) []string {
	var missing []string
	if !decision.PreviousWriterFenced {
		missing = append(missing, "--previous-writer-fenced")
	}
	if !decision.RemoteTasksSettled {
		missing = append(missing, "--remote-tasks-settled")
	}
	if strings.TrimSpace(decision.AuthorityID) == "" {
		missing = append(missing, "--authority-id")
	}
	return missing
}

// joinWithOxfordComma joins items as a list in prose: "a", "a and b", or "a,
// b, and c".
func joinWithOxfordComma(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}

// plannedVMConfigStep reports whether step is a planned step of record's
// active attempt that has no task, charges nothing, records no volume, and
// targets a VM on a node, which is the shape of every configuration write
// cleanup or adopt settles. It doesn't look at the step's kind.
func plannedVMConfigStep(step aj.Step, record aj.Record) bool {
	return step.Attempt == record.ActiveAttempt() && step.State == aj.Planned && step.UPID == "" && len(step.Charges) == 0 && len(step.VolIDs) == 0 && step.Target.VMID > 0 && step.Target.Node != "" && !step.Target.External
}

func cleanupConfigStep(step aj.Step, record aj.Record) bool {
	if !plannedVMConfigStep(step, record) {
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
		// Cleanup settles these on the operator's attestations and a fresh
		// observation of the disk's owner, and neither depends on which call
		// planned the write. attach_disk's own config writes, and the detach
		// tail it runs first, can leave such a step just as detach_disk and
		// delete_disk can. A lifecycle write that turns a parker's
		// protection back on is the exception. The write settles only once
		// the parker reads back protected, so cleanup declines it here and
		// lets the protection settlement refuse it with its own reason. A
		// park_ step never records the protection parameters, so it can't
		// be one.
		switch step.Kind {
		case "park_Nodes_UpdateQemuConfig":
			return true
		case "lifecycle_attach_disk_Nodes_UpdateQemuConfig", "lifecycle_detach_disk_Nodes_UpdateQemuConfig", "lifecycle_delete_disk_Nodes_UpdateQemuConfig":
			return !IsParkerProtectionStep(record, step)
		}
		return false
	default:
		return false
	}
}

// storageCleanupSettled is storageOperationSettled for explicit cleanup and
// delete_vm, whose refusals name cleanup.
func storageCleanupSettled(ctx context.Context, record aj.Record) error {
	return storageOperationSettled(ctx, "cleanup", record, nil)
}

// storageCleanupSettledAfter is storageCleanupSettled for delete_vm, which has
// just run settlePlannedLockSteps on record. gaps is what settlement returned,
// and the refusal adds why settlement left the step it names planned, the way
// adopt's refusal does.
func storageCleanupSettledAfter(ctx context.Context, record aj.Record, gaps map[string]error) error {
	if gaps == nil {
		gaps = map[string]error{}
	}
	return storageOperationSettled(ctx, "cleanup", record, gaps)
}

// storageOperationSettled permits only the exact pending configuration, ISO
// upload, recorded deletion, or sole shared-pool intent independently admitted
// by this explicit cleanup invocation. New failed cleanup steps, ordinary
// lifecycle calls and allocation replay retain strict refusal. operation names
// the call in the refusal, such as attach_disk for an ordinary lifecycle call,
// so the Director's error says which call found the unsettled step. When gaps
// is not nil, the caller has just run settlement, and the refusal for an
// unsettled step adds the reason settlement left it planned.
func storageOperationSettled(ctx context.Context, operation string, record aj.Record, gaps map[string]error) error {
	proof, _ := ctx.Value(cleanupSettlementKey{}).(*cleanupSettlement)
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt != record.ActiveAttempt() || step.State == aj.Observed {
			continue
		}
		if proof == nil || proof.AllocationID != record.ID || proof.Attempt != record.ActiveAttempt() {
			text := unsettledStepName(*step)
			if gaps != nil {
				text += unsettledStepReason(*step, gaps)
			}
			return storageRefusal(operation + " has unresolved mutation evidence; " + text)
		}
		hash, err := aj.Fingerprint(*step)
		if err != nil || proof.Steps[step.ID] != hash {
			return storageRefusal(operation + " has new or changed unresolved mutation evidence; " + unsettledStepName(*step))
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
		return aj.Verification{}, storageRefusal("pending configuration cleanup requires independently observed owned artifacts")
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
		return storageRefusal("completed deletion target backing cannot be verified")
	}
	present, err := managedVolumePresent(ctx, deps, target.Node, target.IntendedVolume)
	if err != nil || present {
		return storageRefusal("completed deletion target absence cannot be verified")
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
		return aj.Verification{}, storageRefusal("completed deletion requires complete historical artifact absence")
	}
	if record.Kind == "vm" {
		plan, err := activeStorageAllocationPlan(record)
		if err != nil {
			return aj.Verification{}, err
		}
		definition, ok := plan.Definitions[settlement.CompletedDeletion.Storage]
		if !ok {
			return aj.Verification{}, storageRefusal("completed VM deletion lacks frozen definition")
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
