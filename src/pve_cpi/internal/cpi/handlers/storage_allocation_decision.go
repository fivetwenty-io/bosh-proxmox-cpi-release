package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	allocationKindDisk               = "disk"
	allocationEvidenceIDField        = "allocation_id"
	allocationEvidenceOperationField = "operation"
)

// StorageAllocationDecision records an explicit operator disposition. DecisionID
// is a nonsecret audit reference; it is never a substitute for live evidence.
type StorageAllocationDecision struct {
	PreviousWriterFenced  bool                          `json:"previous_writer_fenced,omitempty"`
	RemoteTasksSettled    bool                          `json:"remote_tasks_settled,omitempty"`
	AuthorityID           string                        `json:"authority_id,omitempty"`
	Action                string                        `json:"action"`
	AllocationID          string                        `json:"allocation_id"`
	ExpectedCID           string                        `json:"expected_cid,omitempty"`
	RecoveredTaskEvidence *StorageRecoveredTaskEvidence `json:"recovered_task_evidence,omitempty"`
	RecoveredTaskStep     string                        `json:"recovered_task_step,omitempty"`
	RecoveredTaskUPID     string                        `json:"recovered_task_upid,omitempty"`
	DecisionID            string                        `json:"decision_id"`
}

// ApplyStorageAllocationDecision observes PVE without changing it. Cleanup is
// finalized only after all recorded mutations settled and all artifacts vanished.
func ApplyStorageAllocationDecision(ctx context.Context, deps Deps, journal *aj.Journal, nodes []string, decision StorageAllocationDecision) (result aj.Record, retErr error) {
	defer func() {
		outcome := "rejected"
		if retErr == nil {
			if decision.Action == "adopt" {
				outcome = "adopted"
			} else {
				outcome = "cleaned"
			}
		}
		deps.recordStorageReconciliation(ctx, outcome)
	}()
	if ctx == nil || deps.Config == nil || deps.PVE == nil || journal == nil || strings.TrimSpace(decision.DecisionID) == "" || len(decision.DecisionID) > 256 || strings.ContainsAny(decision.DecisionID, "\r\n\x00") {
		return result, fmt.Errorf("a journal and bounded nonsecret decision reference are required")
	}
	if decision.Action != "adopt" && decision.Action != "finalize-cleanup" {
		return result, fmt.Errorf("unsupported allocation decision")
	}
	denied := fmt.Errorf("allocation decisions cannot mutate PVE")
	guard, err := NewManagedAllocationGuard(deps.PVE, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "", denied },
		After:  func(context.Context, ManagedAllocationMutation, string, any) error { return denied },
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error { return denied },
	})
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	deps.PVE = guard.Client()
	// Inspect before requesting ownership so an absent or corrupt record does
	// not create a new lock file. Acquire repeats the read under ownership.
	if _, err := journal.Inspect(decision.AllocationID); err != nil {
		return result, storageDecisionSourceError(err)
	}
	handle, err := journal.Acquire(ctx, decision.AllocationID)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	defer func() { retErr = errors.Join(retErr, storageDecisionSourceError(handle.Close())) }()
	record := handle.Record()
	if record.Namespace != deps.Config.StoragePlacementNamespace {
		return result, fmt.Errorf("allocation decision namespace differs from journal authority")
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, deps.PVE.Nodes(), nodes)
	if err != nil || identity.ID() != record.ClusterID {
		return result, fmt.Errorf("allocation decision requires verified live cluster continuity")
	}

	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return result, fmt.Errorf("allocation already has a terminal disposition")
	}
	for stepIndex := range record.Steps {
		step := record.Steps[stepIndex]
		if step.State != aj.Observed && !storageDecisionClosedAttemptStepSettled(record, step) {
			return result, fmt.Errorf("allocation has unsettled mutation evidence; reconcile task completion before disposition")
		}
	}
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	if !report.Complete || !report.VMScanComplete || len(report.Issues) != 0 || len(report.Conflicts) != 0 {
		return result, fmt.Errorf("allocation disposition requires a complete conflict-free historical audit")
	}
	var ownership aj.Verification
	if decision.Action == "adopt" {
		if record.State != aj.ReadyToReturn || record.CID == "" || record.CID != decision.ExpectedCID {
			return result, fmt.Errorf("adoption requires the exact ready-to-return CID")
		}
		ownership, err = observeAllocationDecisionOwnership(ctx, deps, journal, record)
		if err != nil {
			return result, err
		}
	} else {
		for _, evidence := range report.Evidence {
			if evidence.AllocationID == record.ID {
				return result, fmt.Errorf("allocation resources or provenance remain; cleanup cannot be finalized")
			}
		}
	}
	// Omit Records: their retained verification history must not recursively grow
	// each new audit. The immutable record remains protected by this handle lock.
	id, body, err := aj.VerificationEvidence(map[string]any{
		"decision": decision, "namespace": record.Namespace, "record_updated_at": record.UpdatedAt,
		"started_at": report.StartedAt, "completed_at": report.CompletedAt,
		"complete": report.Complete, "vm_scan_complete": report.VMScanComplete,
		"evidence": report.Evidence, "ownership": ownership,
	})
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	proof := aj.Verification{EvidenceID: id, EvidenceJSON: body, Complete: true}
	if decision.Action == "adopt" {
		proof.OwnershipVerified = true
		record.State = aj.Adopted
	} else {
		proof.AbsenceVerified = true
		proof.ArtifactDispositionVerified = true
		record.State = aj.Cleaned
	}
	if guard.Err() != nil {
		return result, denied
	}
	record.Verifications = append(record.Verifications, proof)
	record.Reason = ""
	if err = handle.Save(record); err != nil {
		return result, storageDecisionSourceError(err)
	}
	return handle.Record(), nil
}

type storageDecisionObservationError struct{ cause error }

func (e *storageDecisionObservationError) Error() string {
	return "allocation decision could not verify or persist evidence"
}
func (e *storageDecisionObservationError) Unwrap() error { return e.cause }
func storageDecisionSourceError(err error) error {
	if err == nil {
		return nil
	}
	return &storageDecisionObservationError{err}
}

func storageDecisionClosedAttemptStepSettled(record aj.Record, step aj.Step) bool {
	if step.Attempt < 0 || step.Attempt > record.ActiveAttempt() || step.Attempt >= len(record.Attempts) {
		return false
	}
	proof := record.Attempts[step.Attempt].Completion
	return proof != nil && proof.Complete && proof.AbsenceVerified && proof.ArtifactDispositionVerified && !proof.RetainedArtifacts && (proof.OutcomesKnown || proof.NoSubmissionVerified)
}

func observeAllocationDecisionOwnership(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record) (aj.Verification, error) {
	var ownership aj.Verification
	if record.Kind == "vm" {
		observed, err := observeManagedVMRecord(ctx, deps, journal, record)
		if err != nil {
			return aj.Verification{}, storageDecisionSourceError(err)
		}
		ownership = observed.Verification
	} else {
		rd, err := resolveDeleteDiskCID(ctx, deps, record.CID)
		if err != nil {
			return aj.Verification{}, storageDecisionSourceError(err)
		}
		if rd.allocation == nil || rd.allocation.record.ID != record.ID || rd.intent != nil {
			return aj.Verification{}, fmt.Errorf("disk ownership or transfer disposition is unresolved")
		}
		storage, volume, err := pve.ParseDiskCID(rd.volid)
		if err != nil {
			return aj.Verification{}, storageDecisionSourceError(err)
		}
		info, err := deps.PVE.Nodes().GetStorageContent(ctx, rd.allocation.provenance.Node, storage, volume)
		if err != nil || info == nil || info.Size <= 0 {
			return aj.Verification{}, fmt.Errorf("adoption disk cannot be observed at its exact physical target")
		}
		ownership, err = managedDiskOwnershipProof(rd)
		if err != nil {
			return aj.Verification{}, storageDecisionSourceError(err)
		}
	}
	return ownership, nil
}
