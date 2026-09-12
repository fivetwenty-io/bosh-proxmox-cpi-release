package handlers

import (
	"crypto/sha256"
	"fmt"
	"strconv"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// StorageRecoveredTaskEvidence is retained operator evidence from the original
// forwarding boundary. It links a response to its exact request and journal
// intent; PVE task status and artifact ownership are still observed independently.
// This is not a signed PVE attestation and does not replace writer fencing.
type StorageRecoveredTaskEvidence struct {
	Version         int            `json:"version"`
	Namespace       string         `json:"namespace"`
	AllocationID    string         `json:"allocation_id"`
	Step            aj.Step        `json:"step"`
	Method          string         `json:"method"`
	Path            string         `json:"path"`
	RequestIdentity map[string]any `json:"request_identity"`
	ResponseStatus  int            `json:"response_status"`
	UPID            string         `json:"upid"`
}

func validateRecoveredTaskReceipt(record aj.Record, step aj.Step, decision StorageAllocationDecision) error {
	receipt := decision.RecoveredTaskEvidence
	if receipt == nil {
		return fmt.Errorf("recovered task receipt is required")
	}
	expectedHash, err := aj.Fingerprint(step)
	if err != nil {
		return err
	}
	actualHash, err := aj.Fingerprint(receipt.Step)
	if err != nil {
		return err
	}
	if receipt.Version != 1 || receipt.Namespace != record.Namespace || receipt.AllocationID != record.ID || actualHash != expectedHash || receipt.UPID != decision.RecoveredTaskUPID || receipt.Method != "POST" || receipt.ResponseStatus < 200 || receipt.ResponseStatus >= 300 {
		return fmt.Errorf("recovered task lacks exact retained request/response evidence")
	}
	if err := validateRecoveredRequestScalars(step.Kind, receipt.RequestIdentity); err != nil {
		return err
	}
	request := receipt.RequestIdentity
	target := step.Target
	expected := ""
	switch step.Kind {
	case "vm." + managedVMCallUpload:
		expected = "/nodes/" + target.Node + "/storage/" + target.Storage + "/upload"
		if request["content"] != "iso" || request["filename"] != fmt.Sprintf("vm-%d-config.iso", target.VMID) {
			return fmt.Errorf("recovered upload request differs from intended ISO")
		}
	case "vm." + managedVMCallCreate, "vm." + managedVMCallClone:
		marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: record.Namespace, AllocationID: record.ID, AgentSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(record.AgentID)))})
		if err != nil || request[pveConfigKeyDescription] != marker {
			return fmt.Errorf("recovered allocation request lacks full allocation marker")
		}
		expected = "/nodes/" + target.Node + "/qemu"
		identityKey := "vmid"
		if step.Kind == "vm."+managedVMCallClone {
			var err error
			expected, err = validateRecoveredCloneRequest(record, target, request)
			if err != nil {
				return err
			}
			identityKey = "newid"
		}
		if fmt.Sprint(request[identityKey]) != strconv.Itoa(target.VMID) {
			return fmt.Errorf("recovered allocation destination differs")
		}
	default:
		return fmt.Errorf("recovered task request kind unsupported")
	}
	if receipt.Path != expected {
		return fmt.Errorf("recovered task request endpoint differs")
	}
	return nil
}

func validateRecoveredRequestScalars(kind string, request map[string]any) error {
	allowed := map[string]bool{"vmid": true, pveConfigKeyDescription: true}
	if kind == "vm."+managedVMCallClone {
		allowed = map[string]bool{"newid": true, pveConfigKeyDescription: true, "target": true, "storage": true, "full": true, "format": true}
	}
	if kind == "vm."+managedVMCallUpload {
		allowed = map[string]bool{"content": true, "filename": true}
	}
	for key, value := range request {
		switch value.(type) {
		case string, int, float64:
		case bool:
			if key != "full" {
				return fmt.Errorf("boolean request identity only valid for clone mode")
			}
		default:
			return fmt.Errorf("recovered request identity must contain bounded scalars")
		}
		if !allowed[key] || len(fmt.Sprint(value)) > 8192 {
			return fmt.Errorf("recovered task request identity is unsupported")
		}
	}
	return nil
}

func validateRecoveredCloneRequest(record aj.Record, target aj.Target, request map[string]any) (string, error) {
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return "", err
	}
	root, ok := managedVMRoleTarget(plan, storageRoleRoot)
	if !ok || root.Source == nil {
		return "", fmt.Errorf("recovered clone source unavailable")
	}
	expected := "/nodes/" + root.Source.Node + "/qemu/" + strconv.Itoa(root.Source.TemplateVMID) + "/clone"
	full := fmt.Sprint(request["full"])
	if root.Mechanism == storageMechanismFullClone && full != "true" && full != "1" || root.Mechanism == "linked_clone" && full != "false" && full != "0" {
		return "", fmt.Errorf("recovered clone mode differs")
	}
	if root.Mechanism == storageMechanismFullClone && (plan.VMExecution == nil || request["format"] != plan.VMExecution.DiskFormat) {
		return "", fmt.Errorf("recovered clone format differs")
	}
	if root.Mechanism == "linked_clone" {
		if _, exists := request["format"]; exists {
			return "", fmt.Errorf("linked clone receipt contains format override")
		}
	}
	if root.Mechanism == storageMechanismFullClone && request["storage"] != target.Storage {
		return "", fmt.Errorf("recovered clone storage differs")
	}
	if root.Mechanism == "linked_clone" {
		if _, exists := request["storage"]; exists {
			return "", fmt.Errorf("linked clone receipt contains storage override")
		}
	}
	node := root.Source.Node
	if actual, exists := request["target"]; exists {
		node = fmt.Sprint(actual)
	}
	if node != target.Node {
		return "", fmt.Errorf("recovered clone destination node differs")
	}
	return expected, nil
}
