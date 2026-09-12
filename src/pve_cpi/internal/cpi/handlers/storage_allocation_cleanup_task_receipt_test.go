package handlers

import (
	"crypto/sha256"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"strconv"
	"testing"
)

func recoveredTaskTestEvidence(t *testing.T, record aj.Record, decision StorageAllocationDecision) *StorageRecoveredTaskEvidence {
	t.Helper()
	var step aj.Step
	for index := range record.Steps {
		candidate := &record.Steps[index]
		if candidate.ID == decision.RecoveredTaskStep {
			step = *candidate
		}
	}
	request := map[string]any{}
	path := "/nodes/" + step.Target.Node + "/qemu"
	switch step.Kind {
	case "vm." + managedVMCallUpload:
		path = "/nodes/" + step.Target.Node + "/storage/" + step.Target.Storage + "/upload"
		request["content"] = "iso"
		request["filename"] = fmt.Sprintf("vm-%d-config.iso", step.Target.VMID)
	default:
		marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: record.Namespace, AllocationID: record.ID, AgentSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(record.AgentID)))})
		if err != nil {
			t.Fatal(err)
		}
		request["description"] = marker
		if step.Kind == "vm."+managedVMCallClone {
			plan, err := activeStorageAllocationPlan(record)
			if err != nil {
				t.Fatal(err)
			}
			root, _ := managedVMRoleTarget(plan, storageRoleRoot)
			path = "/nodes/" + root.Source.Node + "/qemu/" + strconv.Itoa(root.Source.TemplateVMID) + "/clone"
			if root.Mechanism == "full_clone" {
				request["storage"] = step.Target.Storage
				request["format"] = plan.VMExecution.DiskFormat
			}
			request["full"] = root.Mechanism == "full_clone"
			request["newid"] = step.Target.VMID
			request["target"] = step.Target.Node
		} else {
			request["vmid"] = step.Target.VMID
		}
	}
	return &StorageRecoveredTaskEvidence{Version: 1, Namespace: record.Namespace, AllocationID: record.ID, Step: step, Method: "POST", Path: path, RequestIdentity: request, ResponseStatus: 200, UPID: decision.RecoveredTaskUPID}
}

func TestRecoveredCloneReceiptRefusesAnotherDestinationFromSameTemplate(t *testing.T) {
	_, _, _, record, decision := unknownVMAllocationFixture(t, storageRoleRoot, "full_clone")
	for _, mode := range []string{"missing", "other destination", "wrong marker", "wrong path", "wrong storage", "wrong task", "nested value", "other step", "wrong namespace", "wrong mode", "wrong format"} {
		t.Run(mode, func(t *testing.T) {
			changed := decision
			changed.RecoveredTaskEvidence = recoveredTaskTestEvidence(t, record, decision)
			receipt := changed.RecoveredTaskEvidence
			switch mode {
			case "missing":
				changed.RecoveredTaskEvidence = nil
			case "other destination":
				receipt.RequestIdentity["newid"] = 124
			case "wrong marker":
				receipt.RequestIdentity["description"] = "foreign"
			case "wrong path":
				receipt.Path = "/nodes/pve1/qemu/999/clone"
			case "wrong storage":
				receipt.RequestIdentity["storage"] = "other"
			case "wrong task":
				receipt.UPID += "other"
			case "nested value":
				receipt.RequestIdentity["storage"] = map[string]any{"credentials": "hidden"}
			case "other step":
				receipt.Step.ID = "other"
			case "wrong mode":
				receipt.RequestIdentity["full"] = false
			case "wrong format":
				receipt.RequestIdentity["format"] = "raw"
			case "wrong namespace":
				receipt.Namespace = "other"
			}
			if _, err := cleanupRecoveredTask(record, changed); err == nil {
				t.Fatal("ambiguous original response admitted")
			}
		})
	}
}
