package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

func beginManagedVMRetry(ctx context.Context, deps Deps, parsed *createVMParsedArgs, selection *StoragePlacementSelection, handle *aj.Handle, next *managedVMPlan, proof aj.Verification) (*managedVMAllocation, error) {
	parsed.storagePlan = next.plan
	shape, err := buildVMShapeForNode(ctx, deps, parsed, next.plan.Node)
	if err != nil {
		return nil, err
	}
	next.plan.VMExecution, err = freezeManagedVMExecution(deps.Config, shape)
	if err != nil {
		return nil, err
	}
	intent, err := storageJournalIntent("create_vm", nil, selection, next.inventory, next.plan)
	if err != nil {
		return nil, err
	}
	original := handle.Record().Intent
	intent.IntentFingerprint = original.IntentFingerprint
	previous, err := activeStorageAllocationPlan(handle.Record())
	if err != nil {
		return nil, err
	}
	if previous.VMExecution == nil || previous.VMExecution.MaxAttempts <= handle.Record().ActiveAttempt()+1 {
		return nil, fmt.Errorf("frozen VM attempt budget exhausted")
	}
	if next.plan.VMExecution.InputFingerprint != previous.VMExecution.InputFingerprint {
		return nil, fmt.Errorf("frozen VM execution inputs changed; no replacement allocated")
	}
	next.plan.VMExecution.MaxAttempts = previous.VMExecution.MaxAttempts
	intent.Plan, err = json.Marshal(next.plan)
	if err != nil {
		return nil, err
	}
	if intent.IntentFingerprint != original.IntentFingerprint || intent.PolicyFingerprint != original.PolicyFingerprint || intent.FrozenInputsFingerprint != original.FrozenInputsFingerprint {
		return nil, fmt.Errorf("retry policy or frozen membership changed; no replacement allocated")
	}
	attempt := aj.AttemptPlan{Version: aj.AttemptVersion, PolicyFingerprint: intent.PolicyFingerprint, FrozenInputsFingerprint: intent.FrozenInputsFingerprint, PlanVersion: intent.PlanVersion, Plan: intent.Plan}
	if err := handle.BeginAttempt(attempt, aj.AttemptVerification{Verification: proof, OutcomesKnown: true}); err != nil {
		return nil, err
	}
	return newManagedVMAllocation(deps, parsed, shape, next, handle)
}

func managedVMAttemptClosed(record aj.Record) bool {
	return len(record.Attempts) > 0 && record.Attempts[len(record.Attempts)-1].Completion != nil
}
func proveManagedVMAttemptAbsent(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record) (aj.Verification, error) {
	nodes, err := clusterNodeNames(ctx, deps)
	if err != nil {
		return aj.Verification{}, err
	}
	audit, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return aj.Verification{}, err
	}
	if err := storageAuditGateError(ctx, deps, "create_vm retry", audit, storageAuditGateAll); err != nil {
		return aj.Verification{}, err
	}
	for _, evidence := range audit.Evidence {
		if evidence.AllocationID == record.ID {
			return aj.Verification{}, fmt.Errorf("closed VM attempt still has remote allocation evidence")
		}
	}
	proof, err := storageAllocationVerification(audit, map[string]any{"operation": "VM retry admission", "allocation_id": record.ID})
	if err != nil {
		return proof, err
	}
	proof.AbsenceVerified = true
	proof.VMAbsenceVerified = true
	proof.ArtifactDispositionVerified = true
	return proof, nil
}
func runManagedVMWithRetries(ctx context.Context, deps Deps, journal *aj.Journal, parsed *createVMParsedArgs, selection *StoragePlacementSelection, m *managedVMAllocation, observed *managedVMObservation) (any, error) {
	for {
		result, err := m.execute(ctx, observed)
		if err == nil {
			deps.recordStoragePlacement(ctx, selection, "allocation")
			return result, nil
		}
		descriptor := m.prepared.plan.VMExecution
		if descriptor == nil || descriptor.MaxAttempts <= m.handle.Record().ActiveAttempt()+1 || deps.Config.KeepFailedVMsEnabled() {
			return nil, err
		}
		proof, cleanupErr := rollbackManagedVMAttempt(ctx, deps, journal, m.handle)
		if cleanupErr != nil {
			return nil, managedVMRollbackError(ctx, deps, m.handle, err, cleanupErr)
		}
		if cleanupErr := m.handle.CompleteAttempt(aj.AttemptVerification{Verification: proof, OutcomesKnown: true}); cleanupErr != nil {
			return nil, cleanupErr
		}
		parsed.storagePlan = nil
		parsed.storageRuntime = nil
		// The re-plan charges the same in-flight siblings a first placement
		// charges, so a fallback attempt does not pile onto a share a peer
		// claimed while this attempt was running. Our own record is excluded,
		// which leaves the retry free to return to the share it already holds.
		siblings, replanErr := journal.List()
		if replanErr != nil {
			return nil, replanErr
		}
		next, replanErr := prepareManagedVMPlan(ctx, deps, parsed, selection, siblings, m.handle.Record().ID)
		if replanErr != nil {
			return nil, managedVMPlanCPIError(replanErr)
		}
		proof, replanErr = proveManagedVMAttemptAbsent(ctx, deps, journal, m.handle.Record())
		if replanErr != nil {
			return nil, replanErr
		}
		runtime, replanErr := beginManagedVMRetry(ctx, deps, parsed, selection, m.handle, next, proof)
		if replanErr != nil {
			return nil, replanErr
		}
		m = runtime
		observed = nil
		deps.recordStoragePlacement(ctx, selection, "fallback")
	}
}

// managedVMRollbackError joins a failed rollback onto the attempt's own error.
// A rollback that failed after its disposal was admitted has left the
// generation requiring reconciliation, and that error leads, so the Director
// reads a failure it must not retry rather than the attempt's retriable one. A
// rollback refused before its disposal began has left the attempt's VM and
// generation as they were, and the attempt's error keeps its place, so a retry
// under the same agent ID resumes that generation.
func managedVMRollbackError(ctx context.Context, deps Deps, handle *aj.Handle, attemptErr, rollbackErr error) error {
	if handle.Record().State != aj.ReconciliationRequired {
		return errors.Join(attemptErr, rollbackErr)
	}
	deps.recordStorageReconciliation(ctx, "required")
	return errors.Join(rollbackErr, attemptErr)
}
