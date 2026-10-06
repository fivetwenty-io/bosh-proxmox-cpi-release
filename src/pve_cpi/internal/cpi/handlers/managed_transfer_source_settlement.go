package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A managed disk's transfer to a parker writes the transfer record on the
// parker and then deletes the disk's slot on the source VM, and the lifecycle
// guard records the slot delete as planned before it sends it. When the call
// is cut off after PVE applies the delete and before the parker attaches the
// volume, the volume sits in no slot, the parker keeps the transfer record,
// and the slot delete's step stays planned. Every readmission of the disk then
// refuses on that step, so the transfer that would put the disk back on a bus
// never runs, and a delete_vm rerun finds no slot of the disk to preserve.
//
// The settler below settles that one step by reading the source back, and it
// is deliberately narrow. It runs only inside the disk's own lifecycle, under
// the disk's allocation lock, after the disk resolved to the same record, and
// the operation that admits next resumes the transfer the way
// resumeTransferIfNeeded does for any disk with a transfer in flight. The step
// settles only when every one of these holds.
//
//  1. The resolution found the parker's transfer record and no slot that
//     carries the disk's serial, and the record names the disk's serial.
//  2. The step is a planned lifecycle Nodes.UpdateQemuConfig step of the
//     active attempt of a disk record, with no UPID, no recorded parameters,
//     no charges, and no external target, and it is the record's last step.
//  3. Its target is the transfer's source VM and the transfer's volume, and the
//     step journaled before it by the same operation is the observed write of
//     the transfer record on the parker the record names. Observed writes of
//     the same operation to the source in between, such as a reverted pending
//     delete, are passed over. So the step is the source slot delete that
//     followed the record write.
//  4. A fresh read of the source, in both of PVE's views, finds no bus slot
//     naming the volume, which is what the slot delete leaves. A slot whose
//     delete is pending still names it, so the step stays planned. A source
//     that the cluster no longer finds anywhere names nothing either, and the
//     resume then attaches the volume by config edit as it does for any
//     transfer whose source is gone.
//
// A settled step is recorded observed, with no UPID and the volume appended to
// its VolIDs. The settler writes nothing to PVE, so it never adds a write PVE
// could apply twice, and a delete PVE has already applied can't apply again
// with a different result. Adopt and the storage journal's cleanup don't run
// it, because neither of them can finish the transfer afterwards.

// settlePlannedTransferSourceWrite settles the planned source slot delete of
// current's transfer when the reads above prove PVE applied it. It returns an
// error only when the settled record can't be saved. A step it leaves planned
// keeps the refusal readmission gives it.
func settlePlannedTransferSourceWrite(ctx context.Context, client pve.Client, handle *aj.Handle, current resolvedDisk) error {
	if handle == nil || current.intent == nil || current.holder != nil || current.stableID == "" {
		return nil
	}
	record := handle.Record()
	if n := len(record.Attempts); n > 0 && record.Attempts[n-1].Completion != nil {
		return nil
	}
	if record.DiskToken != current.stableID {
		return nil
	}
	index, ok := transferSourceWriteCandidate(record, *current.intent)
	if !ok {
		return nil
	}
	step := &record.Steps[index]
	logger := log.FromContext(ctx)
	if gap := transferSourceReleased(ctx, unguardedPVE(client), step.Target.Node, step.Target.VMID, step.Target.IntendedVolume); gap != "" {
		logger.Warn("planned transfer slot delete left planned",
			log.String("allocation", record.ID), log.String("step", step.ID), log.String("reason", gap))
		return nil
	}
	logger.Info("planned transfer slot delete settled by readback",
		log.String("allocation", record.ID), log.String("step", step.ID), log.Int("vmid", step.Target.VMID),
		log.String("node", step.Target.Node), log.String("volume", step.Target.IntendedVolume),
		log.Int("parker_vmid", current.intent.ParkerVMID))
	step.State = aj.Observed
	if !containsString(step.VolIDs, step.Target.IntendedVolume) {
		step.VolIDs = append(step.VolIDs, step.Target.IntendedVolume)
	}
	settled := []aj.Step{*step}
	if err := handle.Save(record); err != nil {
		return refusedSettlementSave("transfer", settled, err)
	}
	return nil
}

// transferSourceWriteCandidate returns the index of the step that items 2 and
// 3 above describe for intent, or false when the record holds no such step.
// The step records no parameters, so the match can't tell the slot delete from
// another config write the same operation makes on the source after the
// parker's record write. Closing that gap would need the step to record the
// slot key it deletes.
func transferSourceWriteCandidate(record aj.Record, intent pve.DiskTransferIntent) (int, bool) {
	source, err := strconv.Atoi(intent.SourceVMCID)
	if err != nil || source <= 0 || intent.ParkerVMID <= 0 || intent.ParkerVMID == source || intent.Volid == "" {
		return 0, false
	}
	if record.Kind != allocationKindDisk || len(record.Steps) == 0 {
		return 0, false
	}
	index := len(record.Steps) - 1
	step := record.Steps[index]
	if step.Attempt != record.ActiveAttempt() || step.State != aj.Planned || step.UPID != "" || step.Target.External {
		return 0, false
	}
	if len(step.Parameters) != 0 || len(step.Charges) != 0 {
		return 0, false
	}
	if !strings.HasPrefix(step.Kind, "lifecycle_") || !strings.HasSuffix(step.Kind, "_Nodes_UpdateQemuConfig") {
		return 0, false
	}
	if step.Target.Node == "" || step.Target.VMID != source || step.Target.IntendedVolume != intent.Volid {
		return 0, false
	}
	for i := index - 1; i >= 0; i-- {
		before := record.Steps[i]
		if before.Attempt != step.Attempt || before.Kind != step.Kind || before.State != aj.Observed {
			return 0, false
		}
		switch before.Target.VMID {
		case source:
			continue
		case intent.ParkerVMID:
			return index, true
		default:
			return 0, false
		}
	}
	return 0, false
}

// transferSourceReleased is item 4 above. It returns "" when no bus slot of
// VM vmid names volume in either view, or when the cluster no longer finds the
// VM, and otherwise the reason the step stays planned.
func transferSourceReleased(ctx context.Context, c pve.Client, node string, vmid int, volume string) string {
	views, err := pve.ReadQemuViews(ctx, c, node, vmid)
	if err != nil {
		if !pve.IsNotFound(err) && !pve.IsPmxcfsConfigMissing(err) {
			return fmt.Sprintf("the config of VM %d could not be read (%s)", vmid, pve.DescribeAuditError(err))
		}
		location, findErr := pve.FindVMAuthoritative(ctx, c, vmid)
		if findErr != nil {
			return fmt.Sprintf("VM %d has no config on node %s and the cluster could not be searched for it (%s)", vmid, node, pve.DescribeAuditError(findErr))
		}
		if !location.Found {
			return ""
		}
		if location.Node == node {
			return fmt.Sprintf("VM %d read as gone on node %s, but the cluster still finds it there", vmid, node)
		}
		if views, err = pve.ReadQemuViews(ctx, c, location.Node, vmid); err != nil {
			return fmt.Sprintf("the config of VM %d on node %s could not be read (%s)", vmid, location.Node, pve.DescribeAuditError(err))
		}
	}
	if slot, onBus := views.BusSlotNaming(volume); onBus {
		return fmt.Sprintf("%s of VM %d still names volume %s", slot, vmid, volume)
	}
	return ""
}
