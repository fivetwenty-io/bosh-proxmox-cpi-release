package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// CleanupStorageAllocation performs explicitly requested physical cleanup using
// the allocation's retained authority. Unknown submissions require settlement
// before this operation; resource absence alone never settles them.
func CleanupStorageAllocation(ctx context.Context, deps Deps, journal *aj.Journal, nodes []string, decision StorageAllocationDecision) (result aj.Record, retErr error) {
	phase := "input"
	defer func() {
		if retErr != nil {
			retErr = storageCleanupFailure(phase, retErr)
		}
	}()
	defer func() {
		outcome := "rejected"
		if retErr == nil {
			outcome = "cleaned"
		}
		deps.recordStorageReconciliation(ctx, outcome)
	}()
	if ctx == nil || deps.Config == nil || deps.PVE == nil || journal == nil || decision.Action != "cleanup" || strings.TrimSpace(decision.DecisionID) == "" || len(decision.DecisionID) > 256 || strings.ContainsAny(decision.DecisionID, "\r\n\x00") {
		return result, fmt.Errorf("cleanup requires journal authority and a bounded nonsecret decision reference")
	}
	phase = "authority_read"
	if _, err := journal.Inspect(decision.AllocationID); err != nil {
		return result, storageDecisionSourceError(err)
	}
	phase = "authority_lock"
	handle, err := journal.Acquire(ctx, decision.AllocationID)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	defer func() {
		if handle != nil {
			retErr = errors.Join(retErr, storageDecisionSourceError(handle.Close()))
		}
	}()
	record := handle.Record()
	if record.Namespace != deps.Config.StoragePlacementNamespace {
		return result, fmt.Errorf("cleanup namespace differs from journal authority")
	}
	phase = "cluster_identity"
	identity, err := pve.ObserveStorageClusterIdentity(ctx, deps.PVE.Nodes(), nodes)
	if err != nil || identity.ID() != record.ClusterID {
		return result, fmt.Errorf("cleanup requires verified live cluster continuity")
	}
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return result, fmt.Errorf("allocation already has a terminal disposition")
	}
	phase = "pending_mutation_settlement"
	ctx, settlement, err := admitStorageCleanupSettlement(ctx, deps, record, decision)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	phase = "historical_audit"
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	if !report.Complete || !report.VMScanComplete || len(report.Conflicts) > 0 || len(report.Issues) > 0 {
		return result, fmt.Errorf("cleanup requires complete conflict-free historical visibility")
	}
	var ownership aj.Verification
	phase = "disk_ownership"
	ownership, err = storageCleanupDiskOwnership(ctx, deps, record, report, settlement)
	if err != nil {
		return result, err
	}

	phase = "pending_ownership"
	ownership, err = storageCleanupPendingOwnership(ctx, deps, journal, record, settlement, ownership)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	phase = "persist_admission"
	id, payload, err := aj.VerificationEvidence(map[string]any{allocationEvidenceOperationField: "explicit_cleanup_admission", "decision": decision, "record_updated_at": record.UpdatedAt, "started_at": report.StartedAt, "completed_at": report.CompletedAt, "evidence": report.Evidence, "targeted_ownership": ownership, "cleanup_settlement": settlement})
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	admission := aj.Verification{EvidenceID: id, EvidenceJSON: payload, Complete: true, OwnershipVerified: ownership.OwnershipVerified, AbsenceVerified: ownership.AbsenceVerified, ArtifactDispositionVerified: ownership.ArtifactDispositionVerified}
	if cleanupCanFinalizeDirectly(record, settlement) {
		for _, evidence := range report.Evidence {
			if evidence.AllocationID == record.ID {
				return result, fmt.Errorf("unsubmitted allocation has unexplained provenance")
			}
		}
		admission.AbsenceVerified = true
		admission.ArtifactDispositionVerified = true
		record.State = aj.Cleaned
		record.Reason = ""
		record.Verifications = append(record.Verifications, admission)
		if err = handle.Save(record); err != nil {
			return result, storageDecisionSourceError(err)
		}
		return handle.Record(), nil
	}
	// VM cleanup independently proves ownership under this same handle. Persist
	// its operator request without claiming ownership from inventory alone.
	if record.Kind == "vm" && record.State != aj.VMDeletedRetained && record.State != aj.ReconciliationRequired {
		record.State = aj.ReconciliationRequired
		record.Reason = "explicit VM cleanup requested"
	}
	if record.Kind == "vm" && record.State == aj.VMDeletedRetained {
		admission, err = retainedCleanupDecisionAdmission(ctx, deps, record, report, decision)
		if err != nil {
			return result, storageDecisionSourceError(err)
		}
	}

	record.Verifications = append(record.Verifications, admission)
	if err = handle.Save(record); err != nil {
		return result, storageDecisionSourceError(err)
	}
	phase = "resource_cleanup"
	return executeStorageAllocationCleanup(ctx, deps, journal, handle, record)

}

func retainedCleanupDecisionAdmission(ctx context.Context, deps Deps, record aj.Record, report StorageAllocationAudit, decision StorageAllocationDecision) (aj.Verification, error) {
	var retained aj.VMRetentionEvidence
	for i := len(record.Verifications) - 1; i >= 0; i-- {
		v := record.Verifications[i]
		if v.VMAbsenceVerified && v.ArtifactDispositionVerified && !v.AbsenceVerified {
			if err := json.Unmarshal([]byte(v.EvidenceJSON), &retained); err != nil {
				return aj.Verification{}, err
			}
			break
		}
	}
	if retained.VMID <= 0 {
		return aj.Verification{}, fmt.Errorf("retained VM disposition identity unavailable")
	}
	location, err := pve.FindVMAuthoritative(ctx, deps.PVE, retained.VMID)
	if err != nil || location.Found {
		return aj.Verification{}, fmt.Errorf("retained cleanup requires absent original VM identity")
	}
	var present []aj.Target
	for _, target := range retained.RetainedArtifacts {
		storage, bare, e := pve.ParseDiskCID(target.IntendedVolume)
		if e != nil {
			return aj.Verification{}, e
		}
		exists, e := managedVolumePresent(ctx, deps, target.Node, target.IntendedVolume)
		if e != nil {
			return aj.Verification{}, e
		}
		if !exists {
			continue
		}
		info, e := deps.PVE.Nodes().GetStorageContent(ctx, target.Node, storage, bare)
		if pve.IsNotFound(e) {
			continue
		}
		if e != nil || info == nil || info.Size <= 0 {
			return aj.Verification{}, fmt.Errorf("retained artifact exact readback unavailable")
		}
		present = append(present, target)
	}
	retained.RetainedArtifacts = present
	id, payload, err := aj.VerificationEvidence(struct {
		aj.VMRetentionEvidence
		Decision    StorageAllocationDecision   `json:"decision"`
		StartedAt   any                         `json:"started_at"`
		CompletedAt any                         `json:"completed_at"`
		Evidence    []StorageAllocationEvidence `json:"evidence"`
	}{retained, decision, report.StartedAt, report.CompletedAt, report.Evidence})
	if err != nil {
		return aj.Verification{}, err
	}
	return aj.Verification{EvidenceID: id, EvidenceJSON: payload, Complete: true, VMAbsenceVerified: len(present) > 0, AbsenceVerified: len(present) == 0, ArtifactDispositionVerified: true}, nil
}

func storageCleanupDiskOwnership(ctx context.Context, deps Deps, record aj.Record, report StorageAllocationAudit, settlement *cleanupSettlement) (aj.Verification, error) {
	if cleanupHasVMDeletion(record, settlement) {
		return cleanupVMDeletedOwnership(record, report)
	}
	var ownership aj.Verification
	if record.Kind == allocationKindDisk && len(record.Steps) > 0 {
		present := false
		for _, evidence := range report.Evidence {
			present = present || evidence.AllocationID == record.ID
		}
		if present {
			rd, e := resolveManagedDiskForCleanup(ctx, deps, record)
			if e != nil {
				return aj.Verification{}, storageDecisionSourceError(e)
			}
			if rd.allocation == nil || rd.allocation.record.ID != record.ID || rd.intent != nil {
				return aj.Verification{}, fmt.Errorf("disk cleanup identity or transfer remains unresolved")
			}
			if rd.holder != nil && !rd.holder.IsParker {
				return aj.Verification{}, fmt.Errorf("disk cleanup requires managed detach before deleting an attached disk")
			}
			ownership, e = managedDiskOwnershipProof(rd)
			if e != nil {
				return aj.Verification{}, storageDecisionSourceError(e)
			}
		} else {
			ownership = aj.Verification{Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}
		}
	}

	return ownership, nil
}

func executeStorageAllocationCleanup(ctx context.Context, deps Deps, journal *aj.Journal, handle *aj.Handle, record aj.Record) (result aj.Record, retErr error) {
	var err error
	if record.Kind == "vm" {
		proof, e := cleanupManagedVMAttempt(ctx, deps, journal, handle)
		if e != nil {
			return result, storageDecisionSourceError(e)
		}
		if proof.Complete && proof.VMAbsenceVerified && proof.ArtifactDispositionVerified && !proof.AbsenceVerified && proof.EvidenceJSON != "" {
			retained := handle.Record()
			retained.State = aj.VMDeletedRetained
			retained.Reason = ""
			retained.Verifications = append(retained.Verifications, proof)
			if e = handle.Save(retained); e != nil {
				return result, storageDecisionSourceError(e)
			}
			proof, e = cleanupManagedVMAttempt(ctx, deps, journal, handle)
			if e != nil {
				return result, storageDecisionSourceError(e)
			}
		}
		if !proof.Complete || !proof.AbsenceVerified || !proof.ArtifactDispositionVerified || proof.EvidenceJSON == "" {
			return result, fmt.Errorf("VM cleanup did not prove durable complete disposition")
		}
		record = handle.Record()
		record.State = aj.Cleaned
		record.Reason = ""
		record.Verifications = append(record.Verifications, proof)
		if err = handle.Save(record); err != nil {
			return result, storageDecisionSourceError(err)
		}
		return handle.Record(), nil
	}
	proof, err := cleanupManagedDiskAllocation(ctx, deps, journal, handle)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	if !proof.Complete || !proof.AbsenceVerified || !proof.ArtifactDispositionVerified || proof.EvidenceJSON == "" {
		return result, fmt.Errorf("disk cleanup did not prove durable complete disposition")
	}
	record = handle.Record()
	record.State = aj.Cleaned
	record.Reason = ""
	record.Verifications = append(record.Verifications, proof)
	if err = handle.Save(record); err != nil {
		return result, storageDecisionSourceError(err)
	}
	return handle.Record(), nil
}
