package handlers

import (
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// ISO imgdel tasks identify the storage, unlike image tasks whose ID also
// identifies the owner VM. Require the exact earlier observed ISO separately.
func cleanupSubmittedVMISODelete(step aj.Step, record aj.Record) bool {
	target := step.Target
	if record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || (step.State != aj.Submitted && step.State != aj.Planned) || step.Kind != "vm.delete.volume" || len(step.Charges) != 0 || len(step.VolIDs) != 0 || target.External || target.VMID != 0 || target.Node == "" || target.Storage == "" || target.Backing == "" {
		return false
	}
	parts := strings.Split(step.UPID, ":")
	if step.State == aj.Planned && step.UPID != "" {
		return false
	}
	if step.State == aj.Submitted && (len(parts) != 9 || parts[1] != target.Node || parts[5] != "imgdel" || parts[6] != target.Storage) {
		return false
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return false
	}
	iso, found := managedVMRoleTarget(plan, storageRoleISO)
	if !found || iso.StorageID != target.Storage || iso.BackingKey != target.Backing || iso.Mechanism != "upload" {
		return false
	}
	definition, ok := plan.Definitions[target.Storage]
	if !ok || definition.BackingKey() != target.Backing {
		return false
	}
	return cleanupISODeletionHasPriorOwnership(step, record, iso, definition)
}

func cleanupISODeletionHasPriorOwnership(step aj.Step, record aj.Record, iso StoragePlanTarget, definition pve.StorageInfo) bool {
	target := step.Target
	for index := range record.Steps {
		birth := &record.Steps[index]
		if birth.ID == step.ID {
			break
		}
		if birth.Target.Node == target.Node && birth.Target.Storage == target.Storage && birth.Target.Backing == target.Backing && birth.Target.IntendedVolume == target.IntendedVolume && cleanupPriorISOUploadOwnership(record, birth.Target) {
			return true
		}
	}
	if step.State != aj.Submitted {
		return false
	}
	for i := range record.Steps {
		old := &record.Steps[i]
		if old.ID == step.ID {
			break
		}
		if old.Attempt != record.ActiveAttempt() || old.State != aj.Observed || old.Target.External || old.Target.VMID <= 0 || old.Target.Storage != target.Storage || old.Target.Backing != target.Backing || old.Target.IntendedVolume != target.IntendedVolume || !strings.HasPrefix(old.Kind, "vm.iso.") {
			continue
		}
		if old.Target.Node != iso.Node {
			continue
		}
		if old.Target.Node != target.Node && !definition.IsShared() {
			continue
		}
		if target.IntendedVolume != fmt.Sprintf("%s:iso/vm-%d-config.iso", target.Storage, old.Target.VMID) {
			continue
		}
		for _, volume := range old.VolIDs {
			if volume == target.IntendedVolume {
				return true
			}
		}
	}
	return false
}

func cleanupVMDeletedOwnership(record aj.Record, report StorageAllocationAudit) (aj.Verification, error) {
	_, vmid, _, err := managedVMDisposalIdentity(record, report)
	if err != nil {
		return aj.Verification{}, err
	}
	return managedVMDispositionProof(report, record, vmid, nil)
}

func cleanupHasVMDeletion(record aj.Record, settlement *cleanupSettlement) bool {
	return record.Kind == "vm" && settlement != nil && settlement.CompletedDeletion != nil
}

// A lost image-delete response can only be reconciled after independent complete
// absence. The UUID birth and retained pre-destroy ownership identify its intent;
// no second delete is submitted for a volume that is still present.
func cleanupPendingVMEphemeralDelete(step aj.Step, record aj.Record) bool {
	target := step.Target
	if record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || step.Kind != "vm.delete.volume" || step.State != aj.Planned && step.State != aj.Submitted || len(step.Charges) != 0 || len(step.VolIDs) != 0 || len(step.Parameters) != 0 || target.External || target.VMID != 0 || target.Node == "" || target.Storage == "" || target.Backing == "" {
		return false
	}
	locator, id, ok := pve.ParseManagedEphemeralVolumeID(target.IntendedVolume)
	if !ok || id != record.ID || locator != pve.AllocationNamespaceLocator(record.Namespace) {
		return false
	}
	vmid, ok := cleanupVolumeVMID(target.IntendedVolume)
	if !ok {
		return false
	}
	if step.State == aj.Planned {
		if step.UPID != "" {
			return false
		}
	} else {
		parts := strings.Split(step.UPID, ":")
		if len(parts) != 9 || parts[1] != target.Node || parts[5] != "imgdel" || parts[6] != fmt.Sprintf("%d@%s", vmid, target.Storage) {
			return false
		}
	}
	for index := range record.Steps {
		birth := &record.Steps[index]
		if birth.ID == step.ID {
			break
		}
		if !cleanupPendingVMAllocation(*birth, record) || birth.Kind != "vm."+managedVMCallCreateVolume || birth.Target.IntendedVolume != target.IntendedVolume || birth.Target.Node != target.Node || birth.Target.Storage != target.Storage || birth.Target.Backing != target.Backing {
			continue
		}
		if cleanupPriorVMVolumeOwnership(record, *birth, target.IntendedVolume) {
			return true
		}
	}
	return false
}
