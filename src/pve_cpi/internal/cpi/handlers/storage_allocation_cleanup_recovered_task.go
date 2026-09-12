package handlers

import (
	"context"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"strconv"
	"strings"
)

// A recovered response is an operator-supplied lookup key, never ownership or
// completion proof. Admission independently observes the exact live task and all
// resource identities, and retains the original unknown step without rewriting it.
func cleanupRecoveredTask(record aj.Record, decision StorageAllocationDecision) (*aj.Step, error) {
	if decision.RecoveredTaskStep == "" && decision.RecoveredTaskUPID == "" && decision.RecoveredTaskEvidence == nil {
		return nil, nil
	}
	if decision.Action != "cleanup" || !decision.PreviousWriterFenced || !decision.RemoteTasksSettled || decision.RecoveredTaskStep == "" || decision.RecoveredTaskUPID == "" {
		return nil, fmt.Errorf("recovered task requires an exact step and fenced settlement")
	}
	var found *aj.Step
	for index := range record.Steps {
		step := &record.Steps[index]
		if step.ID != decision.RecoveredTaskStep {
			continue
		}
		if found != nil || record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || step.State != aj.Planned || step.UPID != "" || step.Target.External {
			return nil, fmt.Errorf("recovered task does not identify one unknown active VM step")
		}
		if err := validateRecoveredTaskReceipt(record, *step, decision); err != nil {
			return nil, err
		}
		effective := *step
		effective.UPID = decision.RecoveredTaskUPID
		effective.State = aj.Submitted
		if _, ok := cleanupSubmittedISOUpload(effective, record); !ok && !cleanupPendingVMAllocation(effective, record) {
			return nil, fmt.Errorf("recovered task does not match a supported exact mutation")
		}
		found = &effective
	}
	if found == nil {
		return nil, fmt.Errorf("recovered task step was not found")
	}
	return found, nil
}

func cleanupEffectiveStep(ctx context.Context, step aj.Step) aj.Step {
	proof, _ := ctx.Value(cleanupSettlementKey{}).(*cleanupSettlement)
	if proof != nil && proof.RecoveredTask != nil && proof.RecoveredTask.ID == step.ID {
		return *proof.RecoveredTask
	}
	return step
}

func cleanupRootTaskIdentity(step aj.Step, record aj.Record) bool {
	parts := strings.Split(step.UPID, ":")
	if len(parts) != 9 || step.State != aj.Submitted || step.Target.VMID <= 0 {
		return false
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return false
	}
	target, ok := managedVMRoleTarget(plan, storageRoleRoot)
	if !ok || target.Source == nil {
		return false
	}
	if step.Kind == "vm."+managedVMCallCreate {
		return target.Mechanism == "import" && parts[1] == target.Node && parts[5] == "qmcreate" && parts[6] == strconv.Itoa(step.Target.VMID)
	}
	if step.Kind == "vm."+managedVMCallClone {
		return (target.Mechanism == storageMechanismFullClone || target.Mechanism == "linked_clone") && target.Source.TemplateVMID > 0 && parts[1] == target.Source.Node && parts[5] == "qmclone" && parts[6] == strconv.Itoa(target.Source.TemplateVMID)
	}
	return false
}
