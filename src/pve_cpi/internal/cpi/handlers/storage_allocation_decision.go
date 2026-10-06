package handlers

import (
	"context"
	"errors"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	allocationKindDisk               = "disk"
	allocationEvidenceIDField        = "allocation_id"
	allocationEvidenceOperationField = "operation"
	decisionActionAdopt              = "adopt"
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
			if decision.Action == decisionActionAdopt {
				outcome = "adopted"
			} else {
				outcome = "cleaned"
			}
		}
		deps.recordStorageReconciliation(ctx, outcome)
	}()
	if ctx == nil || deps.Config == nil || deps.PVE == nil || journal == nil || strings.TrimSpace(decision.DecisionID) == "" || len(decision.DecisionID) > 256 || strings.ContainsAny(decision.DecisionID, "\r\n\x00") {
		return result, storageRefusal("a journal and bounded nonsecret decision reference are required")
	}
	if decision.Action != decisionActionAdopt && decision.Action != "finalize-cleanup" {
		return result, storageRefusal("unsupported allocation decision")
	}
	denied := storageRefusal("allocation decisions cannot mutate PVE")
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
		return result, storageRefusal("allocation decision namespace differs from journal authority")
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, deps.PVE.Nodes(), nodes)
	if err != nil || identity.ID() != record.ClusterID {
		return result, storageRefusal("allocation decision requires verified live cluster continuity")
	}

	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return result, storageRefusal("allocation already has a terminal disposition")
	}
	// A lock step an earlier request left planned is settled by readback
	// first. The write touches only the journal, never PVE.
	gaps, err := settlePlannedLockSteps(ctx, deps, handle)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	record = handle.Record()
	// An attested adopt then plans to settle a planned configuration write on
	// the disk's holder from a readback of the slot it left. The checks below
	// run against the settled view, and adopt saves the settlement only once
	// they pass, so a refusal leaves the journal as it was.
	settlement, adoptReasons := planAdoptConfigSettlement(ctx, deps, record, decision)
	view := settlement.view(record)
	if text := unsettledDecisionText(view, gaps, adoptReasons); text != "" {
		return result, storageRefusal("allocation has unsettled mutation evidence; " + text + "; reconcile that step before disposition")
	}
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	if err := storageAuditGateError(ctx, deps, "allocation disposition", report, storageAuditGateAll); err != nil {
		return result, err
	}
	var ownership aj.Verification
	var settled []adoptSettledConfigStep
	if decision.Action == decisionActionAdopt {
		ownership, settled, err = observeAdoptOwnership(ctx, deps, journal, record, view, decision, settlement)
		if err != nil {
			return result, err
		}
	} else {
		for _, evidence := range report.Evidence {
			if evidence.AllocationID == record.ID {
				return result, storageRefusal("allocation resources or provenance remain; cleanup cannot be finalized")
			}
		}
	}
	// Omit Records: their retained verification history must not recursively grow
	// each new audit. The immutable record remains protected by this handle lock.
	fields := map[string]any{
		"decision": decision, "namespace": record.Namespace, "record_updated_at": record.UpdatedAt,
		"started_at": report.StartedAt, "completed_at": report.CompletedAt,
		"complete": report.Complete, "vm_scan_complete": report.VMScanComplete,
		"evidence": report.Evidence, "ownership": ownership,
	}
	if len(settled) > 0 {
		fields["settled_configuration_steps"] = settled
	}
	id, body, err := aj.VerificationEvidence(fields)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	proof := aj.Verification{EvidenceID: id, EvidenceJSON: body, Complete: true}
	if decision.Action == decisionActionAdopt {
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
	return saveAllocationDecision(ctx, deps, handle, settlement, record, proof)
}

// observeAdoptOwnership checks that adopt accepts view, the record as the
// settlement leaves it, and observes the disk's ownership. A disk the
// Director already holds keeps its CID when a later lifecycle leaves it in
// reconciliation_required, so adoption accepts that shape too. The
// observation proves the disk is where its record says, with no transfer in
// flight. It returns the readback evidence for the configuration writes that
// adopt settled, whether in this decision or in an earlier attested adopt
// that stopped before it saved the adoption.
func observeAdoptOwnership(ctx context.Context, deps Deps, journal *aj.Journal, record, view aj.Record, decision StorageAllocationDecision, settlement *adoptSettlement) (aj.Verification, []adoptSettledConfigStep, error) {
	if !adoptableRecord(view, decision) {
		return aj.Verification{}, nil, storageRefusal("adoption requires a ready_to_return record, or a disk in reconciliation_required, with the exact CID")
	}
	observeCtx, settled, err := adoptOwnershipContext(ctx, deps, view, decision, settlement)
	if err != nil {
		return aj.Verification{}, nil, err
	}
	if settlement != nil {
		settled = settlement.steps
	}
	ownership, err := observeAllocationDecisionOwnership(observeCtx, deps, journal, record)
	if err != nil {
		return aj.Verification{}, nil, err
	}
	return ownership, settled, nil
}

// saveAllocationDecision saves record, which carries the decision's state,
// with proof appended and its reason cleared. When adopt settled
// configuration writes, it saves that settlement on its own first, because
// the journal admits adopted only from reconciliation_required or
// ready_to_return, and a record a stopped disk call left planned reaches
// reconciliation_required only through it.
func saveAllocationDecision(ctx context.Context, deps Deps, handle *aj.Handle, settlement *adoptSettlement, record aj.Record, proof aj.Verification) (aj.Record, error) {
	if settlement != nil {
		if err := saveAdoptSettlement(ctx, deps, handle, settlement); err != nil {
			return aj.Record{}, err
		}
		state := record.State
		record = handle.Record()
		record.State = state
	}
	record.Verifications = append(record.Verifications, proof)
	record.Reason = ""
	if err := handle.Save(record); err != nil {
		if settlement != nil {
			return aj.Record{}, adoptionAfterSettlementSaveError(settlement, err)
		}
		return aj.Record{}, storageDecisionSourceError(err)
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
			return aj.Verification{}, storageRefusal("disk ownership or transfer disposition is unresolved")
		}
		storage, volume, err := pve.ParseDiskCID(rd.volid)
		if err != nil {
			return aj.Verification{}, storageDecisionSourceError(err)
		}
		info, err := deps.PVE.Nodes().GetStorageContent(ctx, rd.allocation.provenance.Node, storage, volume)
		if err != nil || info == nil || info.Size <= 0 {
			return aj.Verification{}, storageRefusal("adoption disk cannot be observed at its exact physical target")
		}
		ownership, err = managedDiskOwnershipProof(rd)
		if err != nil {
			return aj.Verification{}, storageDecisionSourceError(err)
		}
	}
	return ownership, nil
}
