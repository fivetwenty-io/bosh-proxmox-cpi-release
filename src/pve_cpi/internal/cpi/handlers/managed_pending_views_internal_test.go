// managed_pending_views_internal_test.go holds the rows for the journal-managed
// paths that read both of PVE's views. A crash or a kill leaves a stopped VM
// with a slot whose delete is pending, which the config endpoint hides while
// destroy_vm frees owned drives from the current config, and a running guest
// keeps a volume on such a slot. Each row runs on the flow fake with the
// pending model.
//
// Several rows swap the parker pool sweep seam, so none of them may call
// t.Parallel.
package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// flowDiskToken returns the stable-ID serial on VM 777's scsi1 in a flow
// fixture.
func flowDiskToken(t *testing.T, client *lifecycleFlowPVE) string {
	t.Helper()
	value, _ := client.state.configs[777]["scsi1"].(string)
	token, ok := pve.StableIDFromDriveOptStr(value)
	if !ok {
		t.Fatalf("setup: VM 777 scsi1 = %q carries no serial", value)
	}
	return token
}

// TestManagedDeleteVM_PreservesADiskWhoseSlotDeleteIsPending covers a stopped
// managed VM whose journal-managed persistent disk sits on a slot whose delete
// a crash left pending. The preservation reads both views, so the disk is
// still a candidate, its slot delete applies at once on the stopped VM, and the
// disk ends on one parker slot with its serial and its own name, the way it
// would without the pending delete. Neither view of 777 names it afterwards.
func TestManagedDeleteVM_PreservesADiskWhoseSlotDeleteIsPending(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	token := flowDiskToken(t, client)
	volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	client.state.configs[777]["scsi0"] = "a:777/vm-777-disk-0.raw,size=10G"
	owned := map[string]bool{"a:777/vm-777-disk-0.raw": true}
	client.pending = newFakePendingModel()
	client.pending.holdDelete(777, client.state.configs[777], "scsi1")

	if err := detachManagedPersistentForVMDelete(pendingRowContext(), deps, "n1", 777, owned, nil); err != nil {
		t.Fatalf("preservation with a pending-deleted slot: %v", err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	// The managed volume carries the disk band's VMID, which 777 doesn't own,
	// so the preservation parks it by config edit and no move runs.
	if record.State != aj.ReadyToReturn || client.moves != 0 {
		t.Fatalf("preservation: state=%s moves=%d, want the disk returned by config edit with no move", record.State, client.moves)
	}
	requireConfigEditPark(t, client, token, volume)
	if _, present := client.state.configs[777]["scsi1"]; present || client.pending.pendingDelete(777, "scsi1") {
		t.Fatalf("VM 777 after the preservation = %v, want scsi1 gone with nothing pending", client.state.configs[777])
	}
	requireOneParkerCarrier(t, client, token)
}

// TestManagedDeleteVM_PreservationConfirmSeesAPendingDeletedSlot covers the
// read that confirms only owned volumes remain once the preservation is done.
// Something put a foreign volume on scsi3 while the preservation ran, and its
// delete is pending. The confirm reads both views and refuses, so the destroy
// never goes out with that volume still on the VM. The foreign volume appears
// when the parker takes the disk, a write every preservation of this disk
// makes, because a managed disk parks by config edit and no move runs.
func TestManagedDeleteVM_PreservationConfirmSeesAPendingDeletedSlot(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	token := flowDiskToken(t, client)
	client.state.configs[777]["scsi0"] = "a:777/vm-777-disk-0.raw,size=10G"
	owned := map[string]bool{"a:777/vm-777-disk-0.raw": true}
	client.pending = newFakePendingModel()
	planted := false
	client.afterConfigWrite = func(vmid int) {
		if planted || vmid == 777 || len(parkerSlotsCarrying(client, vmid, token)) == 0 {
			return
		}
		planted = true
		cfg := client.state.configs[777]
		cfg["scsi3"] = "a:9003/vm-9003-disk-0.raw,size=1G"
		client.pending.holdDelete(777, cfg, "scsi3")
	}

	err := detachManagedPersistentForVMDelete(pendingRowContext(), deps, "n1", 777, owned, nil)
	if !planted {
		t.Fatal("the parker attach never ran, so the foreign volume was never planted")
	}
	if err == nil || !strings.Contains(err.Error(), "VM still references a volume outside its recorded allocation") {
		t.Fatalf("confirm read = %v, want the refusal for the foreign volume on the pending-deleted slot", err)
	}
}

// TestManagedDeleteVM_UnlinksALegacyDiskWhoseSlotDeleteIsPending covers a
// legacy persistent disk, named for another VMID and recorded under its CID, on
// a slot of a stopped managed VM whose delete a crash left pending. The
// preservation finds it, the unlink's delete applies at once, and the volume
// is left free-floating with no slot of the VM naming it in either view.
func TestManagedDeleteVM_UnlinksALegacyDiskWhoseSlotDeleteIsPending(t *testing.T) {
	ctx := pendingRowContext()
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	volume := "a:9777/vm-9777-disk-9.raw"
	cid, err := pve.EncodeDiskCID(volume, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = client.state.volumes[old]
	delete(client.state.volumes, old)
	client.state.configs[777]["scsi1"] = volume + ",size=5G"
	client.foreignUnlink = true
	pve.UpdateAttachedDiskCID(ctx, client, deps.Log(ctx), "n1", 777, volume, cid)
	client.pending = newFakePendingModel()
	client.pending.holdDelete(777, client.state.configs[777], "scsi1")
	handle, err := journal.AcquireVM(ctx, "vm-agent", record.Intent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	step, err := storageMutationIntent(handle, "vm_create", aj.Target{Node: "n1", VMID: 777}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, nil, false); err != nil {
		t.Fatal(err)
	}

	if err := detachManagedPersistentForVMDelete(ctx, deps, "n1", 777, nil, handle); err != nil {
		t.Fatalf("unlink of a legacy disk on a pending-deleted slot: %v", err)
	}
	if client.state.volumes[volume] == nil {
		t.Fatal("the preservation deleted the legacy volume")
	}
	if client.pending.pendingDelete(777, "scsi1") {
		t.Fatal("scsi1's delete is still pending, want it applied")
	}
	for key, raw := range client.state.configs[777] {
		if text, _ := raw.(string); strings.Split(text, ",")[0] == volume {
			t.Fatalf("VM 777 %s still names %s, want it free-floating", key, volume)
		}
	}
}

// TestManagedDeleteVM_DestroyVerificationSeesAPendingDeletedSlot covers the
// check just before the destroy. The retained ephemeral volume, with its
// stable-ID serial, sits on a slot whose delete a crash left pending on the
// stopped VM. The destroy would take it, so the check reads both views and
// refuses with the stable-ID text.
func TestManagedDeleteVM_DestroyVerificationSeesAPendingDeletedSlot(t *testing.T) {
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	record, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	client.state.configs[777]["scsi1"] = volume + ",serial=" + managedVMRetentionToken(vmID) + ",size=5G"
	client.pending = newFakePendingModel()
	client.pending.holdDelete(777, client.state.configs[777], "scsi1")

	err = verifyManagedVMDestroyDevices(pendingRowContext(), deps, record, "n1", 777, map[string]bool{volume: true})
	if err == nil || !strings.Contains(err.Error(), "scsi1, which carries a stable-ID serial") {
		t.Fatalf("destroy verification = %v, want the stable-ID refusal for scsi1", err)
	}
}

// TestManagedCleanup_VolumeOnAPendingDeletedSlotIsStillReferenced covers the
// reference scan before a cleanup deletes a volume. A running guest still has
// the volume on a slot whose delete is pending, which the config endpoint
// hides, so the scan reads both views and refuses.
func TestManagedCleanup_VolumeOnAPendingDeletedSlotIsStillReferenced(t *testing.T) {
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	record, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	client.pending = newFakePendingModel()
	client.pending.run(777)
	client.pending.holdDelete(777, client.state.configs[777], "scsi1")
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		t.Fatal(err)
	}
	definition := plan.Definitions["a"]
	target := aj.Target{Node: "n1", Storage: "a", Backing: definition.BackingKey(), IntendedVolume: volume}

	present, err := managedVMVerifyCleanupVolume(pendingRowContext(), deps, target, definition, true)
	if err == nil || !strings.Contains(err.Error(), "cleanup volume remains referenced by a guest") {
		t.Fatalf("cleanup volume check = %v, %v, want the refusal for the volume the running guest still has", present, err)
	}
}

// TestCleanupAllocation_ArtifactOnAPendingDeletedSlotIsStillReferenced covers
// the reference scan for a pending allocation's artifacts. Another running VM
// still has the artifact on a slot whose delete is pending, so the scan reads
// both views and refuses.
func TestCleanupAllocation_ArtifactOnAPendingDeletedSlotIsStillReferenced(t *testing.T) {
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	artifact := "a:888/vm-888-disk-0.raw"
	client.state.configs[888] = map[string]any{"name": "other", "digest": "1", "scsi1": artifact + ",size=1G"}
	client.pending = newFakePendingModel()
	client.pending.run(888)
	client.pending.holdDelete(888, client.state.configs[888], "scsi1")

	err := verifyCleanupAllocationReferences(pendingRowContext(), deps, "n1", 777, pve.StorageInfo{Name: "a"}, []string{artifact})
	if err == nil || !strings.Contains(err.Error(), "pending allocation artifact is referenced by another VM") {
		t.Fatalf("reference scan = %v, want the refusal for the artifact VM 888 still has", err)
	}
}

// TestStorageAudit_PendingDeletedSlotStillHoldsTheVolume covers the audit's
// holder collection. A journal-managed disk sits on a running guest's slot
// whose delete is pending, and the audit reports that guest as its holder,
// rather than taking the volume as unattached.
func TestStorageAudit_PendingDeletedSlotStillHoldsTheVolume(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	client.pending = newFakePendingModel()
	client.pending.run(777)
	client.pending.holdDelete(777, client.state.configs[777], "scsi1")

	report, err := AuditStorageAllocations(pendingRowContext(), deps, journal, []string{"n1", "n2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range report.Evidence {
		if evidence.AllocationID == id && evidence.VMID == 777 && evidence.Node == "n1" {
			return
		}
	}
	t.Fatalf("audit evidence = %+v, want VM 777 on n1 as the holder of allocation %s", report.Evidence, id)
}

// TestManagedDeleteVM_RetentionSeesItsSerialOnAPendingDeletedSlot covers the
// read before delete_vm's stop that decides whether a deletion keeps the
// ephemeral volume. The retention serial sits on a slot whose delete a crash
// left pending, and the record shows no retention step. The read takes both
// views, so the serial still counts, and delete_vm keeps the volume and the VM
// the way it does when the slot isn't pending, rather than destroying both.
func TestManagedDeleteVM_RetentionSeesItsSerialOnAPendingDeletedSlot(t *testing.T) {
	for name, pending := range map[string]bool{"slot not pending": false, "slot delete pending": true} {
		t.Run(name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			deps, client, _, vmID, _ := retainDeleteFixture(t)
			delete(client.state.configs[777], "tags")
			volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
			client.state.configs[777]["scsi1"] = volume + ",serial=" + managedVMRetentionToken(vmID) + ",size=5G"
			client.pending = newFakePendingModel()
			if pending {
				client.pending.holdDelete(777, client.state.configs[777], "scsi1")
			}

			_, err := HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
			if err == nil {
				t.Fatal("delete_vm destroyed a VM whose ephemeral volume carries its retention serial")
			}
			if client.state.configs[777] == nil || client.state.volumes[volume] == nil {
				t.Fatalf("delete_vm took the VM or the volume (VM present=%t, volume present=%t)", client.state.configs[777] != nil, client.state.volumes[volume] != nil)
			}
		})
	}
}

// TestManagedEphemeralRetention_FindsAPendingDeletedSlot covers the managed
// ephemeral retention on a stopped VM whose ephemeral slot a crash left with a
// pending delete. The retention reads both views, so the slot is still the
// volume's, its serial write cancels the pending delete, and the volume is
// retained on a parker.
func TestManagedEphemeralRetention_FindsAPendingDeletedSlot(t *testing.T) {
	captureParkerPoolSweep(t)
	ctx := pendingRowContext()
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	volume := "a:777/vm-777-disk-9.raw"
	client.state.volumes[volume] = client.state.volumes[old]
	delete(client.state.volumes, old)
	client.state.configs[777]["scsi1"] = volume + ",size=5G"
	client.pending = newFakePendingModel()
	client.pending.holdDelete(777, client.state.configs[777], "scsi1")
	handle, err := journal.AcquireVM(ctx, "vm-agent", record.Intent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	step, err := storageMutationIntent(handle, "vm_ephemeral", aj.Target{Node: "n1", VMID: 777, Storage: "a", Backing: "nfs://nas/a", IntendedVolume: volume}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, []string{volume}, false); err != nil {
		t.Fatal(err)
	}

	retained, err := retainManagedEphemeralForVMDelete(ctx, deps, handle, "n1", 777, volume, false)
	if err != nil {
		t.Fatalf("retention of a volume on a pending-deleted slot: %v", err)
	}
	if retained == volume || client.state.volumes[retained] == nil || client.moves != 1 {
		t.Fatalf("retention: landed %s moves=%d, want the volume moved to a parker once", retained, client.moves)
	}
	if _, present := client.state.configs[777]["scsi1"]; present || client.pending.pendingDelete(777, "scsi1") {
		t.Fatalf("VM 777 after the retention = %v, want scsi1 gone with nothing pending", client.state.configs[777])
	}
}

// replacementDelete is what replacementDeleteFixture builds: the fixture's
// deps, client, and journal, the VM allocation's ID, and scsi2's two volumes.
type replacementDelete struct {
	deps             Deps
	client           *lifecycleFlowPVE
	journal          *aj.Journal
	vmID             string
	current, pending string
}

// replacementDeleteFixture is retainDeleteFixture with a pending drive
// replacement on scsi2. Its current drive and its pending value are both legacy
// persistent disks named for other VMIDs, and the VM's description records
// the pending one's CID, so once the change applies, the preservation unlinks
// that disk and the destroy frees neither.
func replacementDeleteFixture(t *testing.T) replacementDelete {
	t.Helper()
	ctx := pendingRowContext()
	var f replacementDelete
	f.deps, f.client, f.journal, f.vmID, _ = retainDeleteFixture(t)
	source := strings.Split(f.client.state.configs[777]["scsi1"].(string), ",")[0]
	f.current, f.pending = "a:9778/vm-9778-disk-8.raw", "a:9777/vm-9777-disk-9.raw"
	for _, volume := range []string{f.current, f.pending} {
		info := *f.client.state.volumes[source]
		f.client.state.volumes[volume] = &info
	}
	cid, err := pve.EncodeDiskCID(f.pending, nil)
	if err != nil {
		t.Fatal(err)
	}
	pve.UpdateAttachedDiskCID(ctx, f.client, f.deps.Log(ctx), "n1", 777, f.pending, cid)
	// The legacy disk is named for another VM, so its slot delete leaves no
	// unused entry, while the retained ephemeral volume's transfer still
	// finds its own.
	f.client.unlinkedVolumes = map[string]bool{f.pending: true}
	f.client.state.configs[777]["scsi2"] = f.current + ",size=5G"
	f.client.pending = newFakePendingModel()
	f.client.pending.holdReplacement(777, f.client.state.configs[777], "scsi2", f.pending+",size=5G")
	return f
}

// TestManagedDeleteVM_PendingDriveReplacementAppliedByTheStop covers a running
// managed VM whose scsi2 has a pending drive replacement. delete_vm's stop
// applies the change, so the reads after it see an ordinary slot, the
// preservation unlinks the disk the change put there, and the destroy goes out
// with neither volume freed.
func TestManagedDeleteVM_PendingDriveReplacementAppliedByTheStop(t *testing.T) {
	captureParkerPoolSweep(t)
	f := replacementDeleteFixture(t)
	deps, client, journal, vmID, current, pending := f.deps, f.client, f.journal, f.vmID, f.current, f.pending
	client.pending.run(777)

	if _, err := HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_vm of a running VM with a pending drive replacement: %v", err)
	}
	if client.state.configs[777] != nil {
		t.Fatal("VM 777 wasn't destroyed")
	}
	for _, volume := range []string{current, pending} {
		if client.state.volumes[volume] == nil {
			t.Fatalf("the destroy freed %s", volume)
		}
	}
	record, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.VMDeletedRetained {
		t.Fatalf("record state = %s, want %s", record.State, aj.VMDeletedRetained)
	}
}

// TestManagedDeleteVM_PendingDriveReplacementOnAStoppedVMIsRefused covers a
// crash that left the same replacement on a stopped managed VM, which only its
// next start applies. delete_vm refuses retriably at the preservation's first
// read, naming both volumes and saying when PVE applies the change. It destroys
// nothing, leaves the slot as it found it, and hands the allocation back ready
// for the retry rather than requiring reconciliation. Once the change applies,
// the next delete_vm finishes with neither volume freed.
func TestManagedDeleteVM_PendingDriveReplacementOnAStoppedVMIsRefused(t *testing.T) {
	captureParkerPoolSweep(t)
	f := replacementDeleteFixture(t)
	deps, client, journal, vmID, current, pending := f.deps, f.client, f.journal, f.vmID, f.current, f.pending
	args := []json.RawMessage{planJSON(t, "777")}

	_, err := HandleDeleteVM(deps).Handle(pendingRowContext(), args, jsonrpc.Context{})
	requireRetriable(t, err, "delete_vm")
	requireText(t, err, "delete_vm", []string{"VM 777", "scsi2", current, pending, "next clean stop",
		"next start when the VM is already stopped", "Nothing was destroyed"}, "revert")
	if client.state.configs[777] == nil {
		t.Fatal("VM 777 was destroyed while scsi2 named two volumes")
	}
	if value, _ := client.state.configs[777]["scsi2"].(string); !strings.HasPrefix(value, pending+",") {
		t.Fatalf("VM 777 scsi2 = %q, want its pending value left in place", value)
	}
	if held, _ := client.pending.replaced[777]["scsi2"].(string); !strings.HasPrefix(held, current+",") {
		t.Fatalf("VM 777 scsi2's current drive = %q, want %s left in place", held, current)
	}
	record, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the refusal left the record requiring reconciliation: %+v", record)
	}

	// The VM's next start applies the change before the guest boots, and it
	// is stopped again by the time the Director retries.
	client.pending.completeStop(777, client.state.configs[777])
	if _, err := HandleDeleteVM(deps).Handle(pendingRowContext(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("retried delete_vm after the change applied: %v", err)
	}
	if client.state.configs[777] != nil {
		t.Fatal("the retried delete_vm didn't destroy VM 777")
	}
	for _, volume := range []string{current, pending} {
		if client.state.volumes[volume] == nil {
			t.Fatalf("the destroy freed %s", volume)
		}
	}
}

// TestManagedDeleteVM_ReplacementAfterTheFirstReadNeedsReconciliation covers a
// replacement that appears on the legacy disk's slot after the preservation's
// first read, so the unlink's read is the one that refuses. The preservation
// joins that refusal with its own uncertainty, which has already marked the
// record for reconciliation. delete_vm must not hand the Director the
// retriable refusal first on a record that will refuse the retry, so the
// reconciliation error comes first and the record requires reconciliation.
func TestManagedDeleteVM_ReplacementAfterTheFirstReadNeedsReconciliation(t *testing.T) {
	f := replacementDeleteFixture(t)
	deps, client, journal, vmID, pending := f.deps, f.client, f.journal, f.vmID, f.pending
	delete(client.pending.replaced, 777)
	third := "a:9779/vm-9779-disk-7.raw"
	info := *client.state.volumes[pending]
	client.state.volumes[third] = &info
	client.onContentRead = func(volume string) {
		if volume != pending {
			return
		}
		client.onContentRead = nil
		client.pending.holdReplacement(777, client.state.configs[777], "scsi2", third+",size=5G")
	}

	_, err := HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	if err == nil || !strings.HasPrefix(err.Error(), "allocation "+vmID+" requires reconciliation") {
		t.Fatalf("delete_vm = %v, want the reconciliation error first", err)
	}
	if okToRetryCPIError(err) {
		t.Fatalf("delete_vm = %v, want the Director to read the non-retriable reconciliation error", err)
	}
	if client.state.configs[777] == nil {
		t.Fatal("VM 777 was destroyed")
	}
	record, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("record state = %s, want %s", record.State, aj.ReconciliationRequired)
	}
}

// requireManagedDetachUntouched checks that a managed detach_disk sent no
// config write, no move, no slot delete, and no revert since generation was
// read, and that the disk's record went back the way the Director left it.
func requireManagedDetachUntouched(t *testing.T, client *lifecycleFlowPVE, journal *aj.Journal, id string, generation, moves, deletes, reverts int, where string) {
	t.Helper()
	if client.generation != generation || client.moves != moves {
		t.Fatalf("%s: %d config writes and %d moves went out, want none", where, client.generation-generation, client.moves-moves)
	}
	if len(client.pending.deleteCalls) != deletes || len(client.pending.reverts) != reverts {
		t.Fatalf("%s: deletes %v and reverts %v, want none new", where, client.pending.deleteCalls, client.pending.reverts)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReadyToReturn {
		t.Fatalf("%s: record state = %s, want %s, returned clean", where, record.State, aj.ReadyToReturn)
	}
}

// TestManagedDetachDisk_ResumeLeavesAFoundPendingDeleteAndReturnsClean covers
// a journal-managed detach_disk whose earlier attempt wrote its intent record
// and reverted a busy delete, after which a crash or an operator left the
// slot's delete pending on the running source. The source is missing from the
// listings, so the resolver returns the intent and the resume finds the
// pending delete. It leaves the delete alone, and the operation fails
// retriably with no write of any kind and returns the disk clean.
func TestManagedDetachDisk_ResumeLeavesAFoundPendingDeleteAndReturnsClean(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	client.pending = newFakePendingModel()
	client.pending.run(777)
	client.pending.busy[777] = true
	args := []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}
	if _, err := HandleDetachDisk(deps).Handle(pendingRowContext(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("setup: the busy detach succeeded")
	}
	client.pending.busy[777] = false
	client.pending.holdDelete(777, client.state.configs[777], "scsi1")
	client.unlisted = map[int]bool{777: true}
	generation, moves, deletes, reverts := client.generation, client.moves, len(client.pending.deleteCalls), len(client.pending.reverts)

	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), args, jsonrpc.Context{})
	requireRetriable(t, err, "detach through the resume")
	requireText(t, err, "detach through the resume", []string{"earlier attempt", "slot scsi1", "VM 777", "next clean stop"})
	requireManagedDetachUntouched(t, client, journal, id, generation, moves, deletes, reverts, "detach through the resume")
	if !client.pending.pendingDelete(777, "scsi1") {
		t.Fatal("the resume cleared the pending delete it found")
	}
}

// TestManagedDetachDisk_ResumeRefusesAReplacedSourceAndReturnsClean covers
// the resume route for a slot whose pending value names another volume. An
// earlier attempt wrote its intent record and reverted a busy delete, and the
// slot then gained the pending change. The source is missing from the
// listings, so the resolver returns the intent and the resume finds the
// shape. The operation fails retriably with no write of any kind and returns
// the disk clean.
func TestManagedDetachDisk_ResumeRefusesAReplacedSourceAndReturnsClean(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	other := "a:9002/vm-9002-disk-0.raw"
	current := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	info := *client.state.volumes[current]
	client.state.volumes[other] = &info
	client.pending = newFakePendingModel()
	client.pending.run(777)
	client.pending.busy[777] = true
	args := []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}
	if _, err := HandleDetachDisk(deps).Handle(pendingRowContext(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("setup: the busy detach succeeded")
	}
	client.pending.busy[777] = false
	client.pending.holdReplacement(777, client.state.configs[777], "scsi1", other+",size=5G")
	client.unlisted = map[int]bool{777: true}
	generation, moves, deletes, reverts := client.generation, client.moves, len(client.pending.deleteCalls), len(client.pending.reverts)

	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), args, jsonrpc.Context{})
	requireRetriable(t, err, "detach through the resume")
	requireText(t, err, "detach through the resume", []string{"slot scsi1", "VM 777", "pending change from one volume to another"})
	requireManagedDetachUntouched(t, client, journal, id, generation, moves, deletes, reverts, "detach through the resume")
	if held, _ := client.pending.replaced[777]["scsi1"].(string); !strings.HasPrefix(held, current+",") {
		t.Fatalf("VM 777 scsi1's current drive = %q, want %s left in place", held, current)
	}
}

// TestManagedDetachDisk_ReplacedSlotReturnsClean covers a journal-managed
// detach_disk of a disk whose slot carries a pending value naming another
// volume. The transfer refuses before it creates a parker or writes an intent
// record, so the operation fails retriably with no write of any kind and
// returns the disk clean.
func TestManagedDetachDisk_ReplacedSlotReturnsClean(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	other := "a:9002/vm-9002-disk-0.raw"
	current := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	info := *client.state.volumes[current]
	client.state.volumes[other] = &info
	client.pending = newFakePendingModel()
	client.pending.run(777)
	client.pending.holdReplacement(777, client.state.configs[777], "scsi1", other+",size=5G")
	generation, moves := client.generation, client.moves

	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	requireRetriable(t, err, "detach of a replaced slot")
	requireText(t, err, "detach of a replaced slot", []string{"slot scsi1", "VM 777", "pending change from one volume to another"})
	requireManagedDetachUntouched(t, client, journal, id, generation, moves, 0, 0, "detach of a replaced slot")
	if held, _ := client.pending.replaced[777]["scsi1"].(string); !strings.HasPrefix(held, current+",") {
		t.Fatalf("VM 777 scsi1's current drive = %q, want %s left in place", held, current)
	}
}

// TestManagedDetachDisk_ReplacementAfterAnAdmittedWriteNeedsReconciliation is
// the negative row. The replacement appears after the transfer's first config
// write, which the guard admitted as a disk mutation, so the slot delete's
// refusal doesn't prove the disk is where the operation found it. The record
// requires reconciliation.
func TestManagedDetachDisk_ReplacementAfterAnAdmittedWriteNeedsReconciliation(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	other := "a:9002/vm-9002-disk-0.raw"
	current := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	info := *client.state.volumes[current]
	client.state.volumes[other] = &info
	client.pending = newFakePendingModel()
	client.pending.run(777)
	client.afterConfigWrite = func(vmid int) {
		if vmid == 777 {
			return
		}
		client.afterConfigWrite = nil
		client.pending.holdReplacement(777, client.state.configs[777], "scsi1", other+",size=5G")
	}

	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("detach of a slot replaced mid-transfer succeeded")
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("record state = %s, want %s after an admitted write", record.State, aj.ReconciliationRequired)
	}
}
