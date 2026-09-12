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
	nodes, err := managedVMClusterNodes(ctx, deps)
	if err != nil {
		return aj.Verification{}, err
	}
	audit, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return aj.Verification{}, err
	}
	if !audit.Complete || !audit.VMScanComplete || len(audit.Issues) > 0 || len(audit.Conflicts) > 0 {
		return aj.Verification{}, fmt.Errorf("VM retry requires complete conflict-free fresh absence audit")
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
		proof, cleanupErr := cleanupManagedVMAttempt(ctx, deps, journal, m.handle)
		if cleanupErr != nil {
			return nil, errors.Join(err, cleanupErr)
		}
		if cleanupErr := m.handle.CompleteAttempt(aj.AttemptVerification{Verification: proof, OutcomesKnown: true}); cleanupErr != nil {
			return nil, cleanupErr
		}
		parsed.storagePlan = nil
		parsed.storageRuntime = nil
		next, replanErr := prepareManagedVMPlan(ctx, deps, parsed, selection)
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
