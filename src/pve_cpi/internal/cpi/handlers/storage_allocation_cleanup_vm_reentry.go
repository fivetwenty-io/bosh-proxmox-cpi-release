package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// Admission never resubmits an unknown cleanup request. Its exact effect must
// already be visible before this invocation can continue with other disposal.
func cleanupPendingVMDeletion(step aj.Step, record aj.Record) bool {
	if record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || step.Target.External || len(step.Charges) != 0 || len(step.VolIDs) != 0 || step.Target.Node == "" {
		return false
	}
	if step.State != aj.Planned && step.State != aj.Submitted {
		return false
	}
	if step.State == aj.Planned && step.UPID != "" {
		return false
	}
	kind := ""
	switch step.Kind {
	case "vm.delete.stop":
		kind = "qmstop"
	case "vm.delete.destroy":
		kind = "qmdestroy"
	default:
		return false
	}
	if step.Target.VMID <= 0 || step.Target.Storage != "" || step.Target.Backing != "" || step.Target.IntendedVolume != "" || len(step.Parameters) != 0 {
		return false
	}
	moved := cleanupVMDeletionAdmittedMove(record, step.Target.VMID, step.Target.Node)
	linked := false
	for index := range record.Steps {
		prior := &record.Steps[index]
		if prior.ID == step.ID {
			break
		}
		if prior.Attempt == step.Attempt && !prior.Target.External && prior.Target.VMID == step.Target.VMID && (prior.Target.Node == step.Target.Node || moved) && !strings.HasPrefix(prior.Kind, "vm.delete.") {
			linked = true
		}
	}
	if !linked {
		return false
	}
	if step.State == aj.Submitted {
		parts := strings.Split(step.UPID, ":")
		if len(parts) != 9 || parts[1] != step.Target.Node || parts[5] != kind || parts[6] != strconv.Itoa(step.Target.VMID) {
			return false
		}
	}
	return true
}

// cleanupVMDeletionAdmittedMove reports whether the latest delete admission
// of record that verified ownership accepted a move of VM vmid to node. An
// admission that found the VM absent is skipped, and it plans no stop or
// destroy that an older move could link. The first admission audit ran while
// the record was still returned, and it retained the moves it accepted in its
// evidence. A delete in flight, or one that failed or crashed, leaves the
// record in planned, observed, or reconciliation, so crash re-entry and the
// audit's VM move rule both read the admission's verdict instead.
func cleanupVMDeletionAdmittedMove(record aj.Record, vmid int, node string) bool {
	var moves []StorageAllocationMove
	for _, verification := range record.Verifications {
		var evidence struct {
			ObservedMoves []StorageAllocationMove `json:"observed_moves"`
			Facts         struct {
				Operation    string `json:"operation"`
				AllocationID string `json:"allocation_id"`
			} `json:"facts"`
		}
		if !verification.Complete || !verification.OwnershipVerified || json.Unmarshal([]byte(verification.EvidenceJSON), &evidence) != nil {
			continue
		}
		if evidence.Facts.Operation == managedVMCleanupAdmissionOperation && evidence.Facts.AllocationID == record.ID {
			moves = evidence.ObservedMoves
		}
	}
	return slices.ContainsFunc(moves, func(move StorageAllocationMove) bool {
		return move.Kind == "vm" && move.AllocationID == record.ID && move.VMID == vmid && move.ObservedNode == node
	})
}

// observeCleanupVMDeletion requires the pending stop to have reached exactly
// the VM this record owns. The stop step names the node the delete admission
// found the VM on, so a VM that moved again since then is refused here.
func observeCleanupVMDeletion(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record, step aj.Step) error {
	nodes, err := clusterNodeNames(ctx, deps)
	if err != nil {
		return err
	}
	if step.Kind == "vm.delete.destroy" {
		// No numeric-ID adoption: both guest classes must be absent. Remaining exact
		// journal-owned volumes are checked separately by the disposal observers.
		return observePoolOnlyGuestsAbsent(ctx, deps, nodes, step.Target.VMID)
	}
	observed, err := observeManagedVMRecord(ctx, deps, journal, record)
	if err != nil {
		return err
	}
	if observed.Node != step.Target.Node || observed.VMID != step.Target.VMID {
		return storageRefusal("pending stop lacks exact VM ownership")
	}
	status, err := deps.PVE.QEMU().Status(ctx, observed.Node, observed.VMID)
	if err != nil || status["status"] != "stopped" {
		return storageRefusal("pending stop completion is not independently observed")
	}
	return nil
}
