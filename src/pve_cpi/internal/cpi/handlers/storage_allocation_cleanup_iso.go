package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A submitted upload is eligible only for explicit cleanup. Neither task success
// nor the deterministic filename permits allocation replay or adoption.
func cleanupSubmittedISOUpload(step aj.Step, record aj.Record) (aj.Target, bool) {
	target := step.Target
	if record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || step.State != aj.Submitted || step.Kind != "vm."+managedVMCallUpload || len(step.VolIDs) != 0 || len(step.Charges) != 1 || target.External || target.VMID <= 0 {
		return aj.Target{}, false
	}
	parts := strings.Split(step.UPID, ":")
	// PVE starts imgcopy on the receiving API node and can SCP the upload to
	// target.Node. The exact task node is validated against fresh cluster
	// membership and its own status endpoint by cleanup settlement admission.
	if len(parts) != 9 || parts[5] != "imgcopy" || parts[6] != "" {
		return aj.Target{}, false
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return aj.Target{}, false
	}
	iso, found := managedVMRoleTarget(plan, storageRoleISO)
	if !found || iso.Mechanism != "upload" || iso.Node != target.Node || iso.StorageID != target.Storage || iso.BackingKey != target.Backing || iso.VirtualBytes == 0 || iso.ChargeBytes != iso.VirtualBytes || target.IntendedVolume != fmt.Sprintf("%s:iso/vm-%d-config.iso", target.Storage, target.VMID) {
		return aj.Target{}, false
	}
	charge := step.Charges[0]
	if charge.PlannedBytes <= 0 || uint64(charge.PlannedBytes) != iso.ChargeBytes || charge.AcquiredBytes != 0 || charge.OutstandingBytes != charge.PlannedBytes || charge.Backing != iso.CapacityKey || charge.Domain != iso.DomainKey {
		return aj.Target{}, false
	}
	return target, true
}

func observeCleanupUploadedISO(ctx context.Context, deps Deps, record aj.Record, target aj.Target, observed *managedVMObservation) error {
	if observed == nil || observed.Node != target.Node || observed.VMID != target.VMID || !observed.Verification.OwnershipVerified {
		return fmt.Errorf("uploaded ISO cleanup lacks exact live VM ownership")
	}
	var pending *aj.Step
	for i := range record.Steps {
		effective := cleanupEffectiveStep(ctx, record.Steps[i])
		candidate, ok := cleanupSubmittedISOUpload(effective, record)
		if !ok {
			continue
		}
		if candidate != target || pending != nil {
			return fmt.Errorf("uploaded ISO cleanup target is ambiguous")
		}
		pending = &effective
	}
	if pending == nil {
		return fmt.Errorf("uploaded ISO cleanup lacks original submitted intent")
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return err
	}
	definition, ok := plan.Definitions[target.Storage]
	if !ok {
		return fmt.Errorf("uploaded ISO cleanup lacks frozen definition")
	}
	if err := verifyManagedVMCleanupDefinition(ctx, deps, target, definition); err != nil {
		return err
	}
	iso, _ := managedVMRoleTarget(plan, storageRoleISO)
	content, err := pve.ObserveStorageISOContent(ctx, deps.PVE, target.Node, target.IntendedVolume, iso.VirtualBytes)
	if err != nil {
		return err
	}
	// ctime corroborates the exact completed task interval; it is not ownership
	// proof. A remote copy can finish after the second in which its worker began.
	proof, _ := ctx.Value(cleanupSettlementKey{}).(*cleanupSettlement)
	if proof == nil || !proof.TaskObservation.CorroboratesUploadTimestamp(pending.UPID, content.CTime) {
		return fmt.Errorf("uploaded ISO timestamp does not corroborate recorded task")
	}
	_, bare, err := pve.ParseDiskCID(target.IntendedVolume)
	if err != nil {
		return err
	}
	detail, err := deps.PVE.Nodes().GetStorageContent(ctx, target.Node, target.Storage, bare)
	if err != nil || detail == nil || detail.Size <= 0 || uint64(detail.Size) != iso.VirtualBytes || detail.Format != "raw" && detail.Format != "iso" {
		return fmt.Errorf("uploaded ISO exact content readback differs")
	}
	return nil
}

// A prior explicit admission preserves the full marked VM ownership observed
// before destruction. Reentry also requires fresh guest absence, all other VM
// artifacts absent, and the same exact ISO content/task timestamp when present.
func observeCleanupUploadedISOState(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record, target aj.Target) (bool, error) {
	location, err := pve.FindVMAuthoritative(ctx, deps.PVE, target.VMID)
	if err != nil {
		return false, err
	}
	if location.Found {
		observed, err := observeManagedVMRecord(ctx, deps, journal, record)
		if err != nil {
			return false, err
		}
		if err := observeCleanupUploadedISO(ctx, deps, record, target, observed); err != nil {
			return false, err
		}
		return true, nil
	}
	if !cleanupPriorISOUploadOwnership(record, target) {
		return false, fmt.Errorf("absent VM has no retained exact ISO ownership")
	}
	nodes, err := clusterNodeNames(ctx, deps)
	if err != nil {
		return false, err
	}
	if _, err = observePlannedVMAbsence(ctx, deps, record, nodes, target.VMID, target.IntendedVolume); err != nil {
		return false, err
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return false, err
	}
	definition, ok := plan.Definitions[target.Storage]
	if !ok {
		return false, fmt.Errorf("ISO backing unavailable")
	}
	present, err := managedVMVerifyCleanupVolume(ctx, deps, target, definition, true)
	if err != nil || !present {
		return present, err
	}
	// The historical marker is not recreated or written to PVE. This input only
	// carries the previously admitted identity into the unchanged content verifier.
	prior := &managedVMObservation{Node: target.Node, VMID: target.VMID, Verification: aj.Verification{Complete: true, OwnershipVerified: true}}
	if err := observeCleanupUploadedISO(ctx, deps, record, target, prior); err != nil {
		return false, err
	}
	return true, nil
}

func cleanupPriorISOUploadOwnership(record aj.Record, target aj.Target) bool {
	for _, verification := range record.Verifications {
		if !verification.Complete || !verification.OwnershipVerified {
			continue
		}
		var payload struct {
			Operation  string             `json:"operation"`
			Settlement *cleanupSettlement `json:"cleanup_settlement"`
		}
		if json.Unmarshal([]byte(verification.EvidenceJSON), &payload) != nil || payload.Operation != "explicit_cleanup_admission" || payload.Settlement == nil {
			continue
		}
		proof := payload.Settlement
		if proof.AllocationID != record.ID || proof.Attempt != record.ActiveAttempt() || proof.CompletedUpload == nil || *proof.CompletedUpload != target {
			continue
		}
		for index := range record.Steps {
			step := &record.Steps[index]
			effective := *step
			if proof.RecoveredTask != nil && proof.RecoveredTask.ID == step.ID {
				effective = *proof.RecoveredTask
			}
			intended, ok := cleanupSubmittedISOUpload(effective, record)
			if !ok || intended != target {
				continue
			}
			hash, err := aj.Fingerprint(step)
			if err == nil && proof.Steps[step.ID] == hash {
				return true
			}
		}
	}
	return false
}
