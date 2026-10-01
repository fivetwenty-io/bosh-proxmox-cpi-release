// drive_delete_pending_internal_test.go holds the rows for slot deletes that
// PVE could only record as pending. On a running VM whose hotplug setting
// lacks disk, or whose guest still holds the device, a drive delete stays
// pending and the guest keeps the disk, while the config endpoint no longer
// shows the slot. Every row runs on a fake with the pending model, which holds
// such a delete, reports it through the pending endpoint, and applies a
// revert.
//
// Several rows swap the parker pool sweep seam, so none of them may call
// t.Parallel.
package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// pendingRowContext is a request context whose retry loops don't sleep, so a
// busy delete runs its whole budget at once.
func pendingRowContext() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// requireRetriable and requirePermanent check the class the Director reads.
func requireRetriable(t *testing.T, err error, where string) {
	t.Helper()
	if err == nil || !okToRetryCPIError(err) {
		t.Fatalf("%s: err = %v, want a retriable CPI error", where, err)
	}
}

func requirePermanent(t *testing.T, err error, where string) {
	t.Helper()
	if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) {
		t.Fatalf("%s: err = %v, want a non-retriable CPI error", where, err)
	}
}

// requireText checks that err names every fragment and none of the banned ones.
func requireText(t *testing.T, err error, where string, want []string, banned ...string) {
	t.Helper()
	for _, fragment := range want {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("%s: error %q doesn't name %q", where, err, fragment)
		}
	}
	for _, fragment := range banned {
		if strings.Contains(err.Error(), fragment) {
			t.Errorf("%s: error %q says %q", where, err, fragment)
		}
	}
}

// requireNoParkerSlot checks that no parker in configs has a disk on a bus slot.
func requireNoParkerSlot(t *testing.T, configs map[int]map[string]any, where string) {
	t.Helper()
	for vmid, cfg := range configs {
		tags, _ := cfg["tags"].(string)
		if !tagsContain(tags, pve.ParkerTag) {
			continue
		}
		if disks := qemu.ParseDisks(cfg); len(disks) != 0 {
			t.Fatalf("%s: parker %d holds %v, want nothing attached to a parker", where, vmid, disks)
		}
	}
}

// pendingTransferVolid is the disk pendingTransferClient puts on VM 700.
const pendingTransferVolid = "data:vm-700-disk-1"

// pendingTransferClient is transferFunnelClient with VM 700 running under the
// pending model, holding pendingTransferVolid.
func pendingTransferClient() *idFakeClient {
	c := transferFunnelClient(pendingTransferVolid)
	c.pending = newFakePendingModel()
	c.pending.run(700)
	return c
}

// requireStillAttached checks that VM 700 holds volid on scsi1 in both views.
func requireStillAttached(t *testing.T, c *idFakeClient, volid, where string) {
	t.Helper()
	value, _ := c.configs[700]["scsi1"].(string)
	if !strings.HasPrefix(value, volid+",") {
		t.Fatalf("%s: VM 700 scsi1 = %q, want %s back on the slot", where, value, volid)
	}
	if c.pending.pendingDelete(700, "scsi1") {
		t.Fatalf("%s: the delete of scsi1 is still pending, want it reverted", where)
	}
}

// TestDetachStableID_BusyGuestRevertsThePendingDelete covers a source that
// runs with disk hotplug while its guest holds the device. Every delete
// records a pending delete and fails busy until the retries run out. detach_disk
// reverts the pending delete, attaches nothing to a parker, and fails
// retriably naming the VM and the slot, and a retried detach_disk does the
// same.
func TestDetachStableID_BusyGuestRevertsThePendingDelete(t *testing.T) {
	const volid = pendingTransferVolid
	c := pendingTransferClient()
	c.pending.busy[700] = true
	deps := transferFunnelDeps(c)
	diskCID := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})

	for _, attempt := range []string{"first detach", "retried detach"} {
		err := handleDetachStableID(pendingRowContext(), deps, "700", 700, resolveTransferDisk(t, deps, diskCID))
		requireRetriable(t, err, attempt)
		requireText(t, err, attempt, []string{"VM 700", "scsi1", "still holds the disk"})
		requireStillAttached(t, c, volid, attempt)
		requireNoParkerSlot(t, c.configs, attempt)
	}
}

// TestDetachStableID_HotplugLacksDiskRefuses covers a running source whose
// hotplug setting lacks disk. The delete succeeds and stays pending.
// detach_disk reverts it, attaches nothing to a parker, and fails with a
// non-retriable error that names the VM's hotplug setting. Afterwards the slot
// is still attached in both views.
func TestDetachStableID_HotplugLacksDiskRefuses(t *testing.T) {
	const volid = pendingTransferVolid
	c := pendingTransferClient()
	c.configs[700]["hotplug"] = "network,usb"
	deps := transferFunnelDeps(c)
	diskCID := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})

	err := handleDetachStableID(pendingRowContext(), deps, "700", 700, resolveTransferDisk(t, deps, diskCID))
	requirePermanent(t, err, "detach")
	requireText(t, err, "detach", []string{"VM 700", `"network,usb"`, "scsi1", "doesn't include disk"})
	requireStillAttached(t, c, volid, "detach")
	requireNoParkerSlot(t, c.configs, "detach")
}

// TestDetachStableID_UnconfirmedRevertIsRetriable covers a revert that fails,
// and one whose read afterwards still shows the pending delete. detach_disk
// attaches nothing and fails retriably naming the pending delete.
func TestDetachStableID_UnconfirmedRevertIsRetriable(t *testing.T) {
	const volid = pendingTransferVolid
	for name, setup := range map[string]func(*fakePendingModel){
		"the revert fails":                     func(m *fakePendingModel) { m.revertErr = errUnconfirmedRevert },
		"the delete is still pending after it": func(m *fakePendingModel) { m.revertKeepsPending = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := pendingTransferClient()
			c.configs[700]["hotplug"] = "network"
			setup(c.pending)
			deps := transferFunnelDeps(c)
			diskCID := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})

			err := handleDetachStableID(pendingRowContext(), deps, "700", 700, resolveTransferDisk(t, deps, diskCID))
			requireRetriable(t, err, name)
			requireText(t, err, name, []string{"VM 700", "slot scsi1", "is pending", "couldn't confirm"})
			if !c.pending.pendingDelete(700, "scsi1") {
				t.Fatal("the fake lost the pending delete the revert never cleared")
			}
			requireNoParkerSlot(t, c.configs, name)
		})
	}
}

var errUnconfirmedRevert = cpierrors.Retriable("revert refused by the fake")

// TestDetachStableID_StoppedSourceParksAsBefore is the control. On a stopped
// source the delete applies at once, and the transfer parks the disk as it
// always has.
func TestDetachStableID_StoppedSourceParksAsBefore(t *testing.T) {
	captureParkerPoolSweep(t)
	const volid = pendingTransferVolid
	c := pendingTransferClient()
	c.configs[700]["hotplug"] = "network"
	c.pending.stop(700)
	deps := transferFunnelDeps(c)
	diskCID := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})

	if err := handleDetachStableID(pendingRowContext(), deps, "700", 700, resolveTransferDisk(t, deps, diskCID)); err != nil {
		t.Fatalf("detach of a stopped source: %v", err)
	}
	if _, present := c.configs[700]["scsi1"]; present || c.pending.pendingDelete(700, "scsi1") {
		t.Fatalf("source after the detach = %v, want scsi1 gone with nothing pending", c.configs[700])
	}
	parked := false
	for _, value := range qemu.ParseDisks(c.configs[90000]) {
		if serial, ok := pve.StableIDFromDriveOptStr(value); ok && serial == idTestToken {
			parked = true
		}
	}
	if !parked {
		t.Fatalf("parker 90000 = %v, want the disk parked with its serial", c.configs[90000])
	}
	if len(c.pending.reverts) != 0 {
		t.Fatalf("reverts on a stopped source = %v, want none", c.pending.reverts)
	}
}

// legacyPendingVolume is a legacy persistent disk named for a disk-band VMID.
const legacyPendingVolumeName = "9001/vm-9001-disk-0.raw"

// legacyPendingFixture is the flow fake with its journal turned off and the
// journal-managed disk replaced by a legacy persistent disk on VM 777's slot,
// which runs under the pending model with a hotplug setting that lacks disk.
func legacyPendingFixture(t *testing.T, slot, serial string) (Deps, *lifecycleFlowPVE, string, string) {
	t.Helper()
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	storage := strings.Split(original, ":")[0]
	volume := storage + ":" + legacyPendingVolumeName
	client.state.volumes[volume] = client.state.volumes[original]
	delete(client.state.volumes, original)
	delete(client.state.configs[777], "scsi1")
	value := volume + ",size=5G"
	var meta *pve.DiskCIDMeta
	if serial != "" {
		value = volume + ",serial=" + serial + ",size=5G"
		meta = &pve.DiskCIDMeta{ID: serial}
	}
	client.state.configs[777][slot] = value
	client.state.configs[777]["hotplug"] = "network,usb"
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	deps.Logger = log.NewNopLogger()
	client.pending = newFakePendingModel()
	client.pending.run(777)
	cid, err := pve.EncodeDiskCID(volume, meta)
	if err != nil {
		t.Fatal(err)
	}
	return deps, client, volume, cid
}

// requireLegacyStillAttached checks that VM 777 holds volume on slot in both
// views, and that no other guest names it.
func requireLegacyStillAttached(t *testing.T, client *lifecycleFlowPVE, volume, slot, where string) {
	t.Helper()
	value, _ := client.state.configs[777][slot].(string)
	if !strings.HasPrefix(value, volume+",") {
		t.Fatalf("%s: VM 777 %s = %q, want %s back on the slot", where, slot, value, volume)
	}
	if client.pending.pendingDelete(777, slot) {
		t.Fatalf("%s: the delete of %s is still pending, want it reverted", where, slot)
	}
	for vmid, cfg := range client.state.configs {
		for key, raw := range cfg {
			text, _ := raw.(string)
			if strings.Split(text, ",")[0] == volume && (vmid != 777 || key != slot) {
				t.Fatalf("%s: VM %d %s names %s as well", where, vmid, key, volume)
			}
		}
	}
}

// TestDetachDisk_LegacyHotplugLacksDiskReverts covers the legacy detach on a
// running VM whose hotplug setting lacks disk. The delete succeeds with the
// delete left pending. detach_disk reverts it, attaches nothing to a parker,
// and fails with the non-retriable hotplug error.
func TestDetachDisk_LegacyHotplugLacksDiskReverts(t *testing.T) {
	deps, client, volume, cid := legacyPendingFixture(t, "scsi2", "")
	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	requirePermanent(t, err, "legacy detach")
	requireText(t, err, "legacy detach", []string{"VM 777", `"network,usb"`, "scsi2"})
	requireLegacyStillAttached(t, client, volume, "scsi2", "legacy detach")
}

// TestDetachDisk_LegacyRetryAfterAPendingDeleteWasLeftBehind covers a pending
// delete that a crash or an earlier release already left on the slot. The
// retried detach_disk sees the slot as attached in the current view, doesn't
// take the already-detached branch, and handles the pending delete the same
// way.
func TestDetachDisk_LegacyRetryAfterAPendingDeleteWasLeftBehind(t *testing.T) {
	deps, client, volume, cid := legacyPendingFixture(t, "scsi2", "")
	client.pending.holdDelete(777, client.state.configs[777], "scsi2")
	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	requirePermanent(t, err, "retried legacy detach")
	requireText(t, err, "retried legacy detach", []string{"VM 777", `"network,usb"`, "scsi2"})
	requireLegacyStillAttached(t, client, volume, "scsi2", "retried legacy detach")
}

// TestDeleteDisk_RefusesAHolderWithAPendingDelete covers a disk whose holder's
// slot has a pending delete, so the disk's CID otherwise looks free.
// delete_disk refuses it with a non-retriable error that names the VM, the
// node, and the slot, says to stop the VM and rerun the clean-up, and doesn't
// suggest a revert. It deletes nothing.
func TestDeleteDisk_RefusesAHolderWithAPendingDelete(t *testing.T) {
	deps, client, volume, cid := legacyPendingFixture(t, "scsi2", "")
	client.pending.holdDelete(777, client.state.configs[777], "scsi2")
	_, err := HandleDeleteDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{})
	requirePermanent(t, err, "delete_disk")
	requireText(t, err, "delete_disk", []string{"VM 777", "node n1", "slot scsi2", "stopping VM 777", "rerun the clean-up", "Nothing was deleted"}, "revert")
	if client.state.volumes[volume] == nil || client.deletes != 0 {
		t.Fatalf("delete_disk deleted the volume of a disk the running guest still has (deletes=%d)", client.deletes)
	}
	if !client.pending.pendingDelete(777, "scsi2") {
		t.Fatal("delete_disk touched the pending delete")
	}
}

// TestAttachDisk_LegacySCSI0MigrationRevertsAPendingDelete covers attach_disk's
// move of a legacy scsi0 attachment on a running VM whose hotplug setting
// lacks disk. The scsi0 delete stays pending, the helper reverts it, the attach
// adds no second slot, and attach_disk fails with the non-retriable hotplug
// error.
func TestAttachDisk_LegacySCSI0MigrationRevertsAPendingDelete(t *testing.T) {
	deps, client, volume, cid := legacyPendingFixture(t, "scsi0", "")
	_, err := HandleAttachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	requirePermanent(t, err, "attach_disk")
	requireText(t, err, "attach_disk", []string{"VM 777", `"network,usb"`, "scsi0"})
	requireLegacyStillAttached(t, client, volume, "scsi0", "attach_disk")
}

// legacyFastDeleteFixture is legacyPendingFixture for a legacy delete_vm on
// the fast path, with a destroy recorder.
func legacyFastDeleteFixture(t *testing.T, serial string) (Deps, *lifecycleFlowPVE, *legacyDestroyRecorder, string) {
	t.Helper()
	deps, client, volume, _ := legacyPendingFixture(t, "scsi2", serial)
	fast := true
	deps.Config.FastPathDelete = &fast
	deps.Config.DetachedDiskStrategy = "parked"
	recorder := &legacyDestroyRecorder{}
	deps.PVE = legacyDestroyPVE{lifecycleFlowPVE: client, recorder: recorder}
	deps.Agent = legacyDestroyAgent{}
	return deps, client, recorder, volume
}

// requireRefusedBeforeDestroy checks a delete_vm that failed on a pending
// delete. Nothing was destroyed, the VM keeps its bosh-deleting tag, and the
// disk is back on its slot.
func requireRefusedBeforeDestroy(t *testing.T, client *lifecycleFlowPVE, recorder *legacyDestroyRecorder, volume, where string) {
	t.Helper()
	if destroys := recorder.guestDestroys(); len(destroys) != 0 {
		t.Fatalf("%s: VM 777 was destroyed %d times while the delete of its disk was pending", where, len(destroys))
	}
	tags, _ := client.state.configs[777]["tags"].(string)
	if !tagsContain(tags, tagDeletingVM) {
		t.Fatalf("%s: VM 777 tags = %q, want it to keep %s", where, tags, tagDeletingVM)
	}
	requireLegacyStillAttached(t, client, volume, "scsi2", where)
}

// TestDeleteVMFastPath_LegacyForeignDiskPendingDelete covers the fast path
// with a legacy foreign disk on a VM that's still running with hotplug lacking
// disk. The detach's delete is reverted, delete_vm fails retriably, no
// DeleteQemu goes out, and the VM keeps its bosh-deleting tag. Once the VM
// reports stopped, the retried delete_vm leaves the disk free-floating, and the
// destroy goes out.
func TestDeleteVMFastPath_LegacyForeignDiskPendingDelete(t *testing.T) {
	deps, client, recorder, volume := legacyFastDeleteFixture(t, "")
	args := []json.RawMessage{planJSON(t, "777")}

	_, err := HandleDeleteVM(deps).Handle(pendingRowContext(), args, jsonrpc.Context{})
	requireRetriable(t, err, "fast-path delete_vm")
	requireText(t, err, "fast-path delete_vm", []string{"VM 777", "slot scsi2", "stop is still in progress", "next delete_vm finishes the detach"}, "running again")
	requireRefusedBeforeDestroy(t, client, recorder, volume, "fast-path delete_vm")

	client.pending.stop(777)
	if _, err := HandleDeleteVM(deps).Handle(pendingRowContext(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("retried fast-path delete_vm after the VM stopped: %v", err)
	}
	if destroys := recorder.guestDestroys(); len(destroys) != 1 {
		t.Fatalf("VM 777 was destroyed %d times, want once", len(destroys))
	}
	if client.state.volumes[volume] == nil {
		t.Fatal("the destroy took the legacy foreign volume")
	}
	for vmid, cfg := range client.state.configs {
		for key, raw := range cfg {
			if text, _ := raw.(string); strings.Split(text, ",")[0] == volume {
				t.Fatalf("VM %d %s still names %s, want it free-floating", vmid, key, volume)
			}
		}
	}
}

// TestDeleteVMFastPath_StableIDForeignDiskPendingDelete covers the fast path
// with a stable-ID foreign disk on the same running VM. The transfer's typed
// error reaches delete_vm through TransferDiskToParker, and delete_vm fails
// retriably the same way. Once the VM reports stopped, the retry parks the
// disk.
func TestDeleteVMFastPath_StableIDForeignDiskPendingDelete(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, recorder, volume := legacyFastDeleteFixture(t, idTestToken)
	args := []json.RawMessage{planJSON(t, "777")}

	_, err := HandleDeleteVM(deps).Handle(pendingRowContext(), args, jsonrpc.Context{})
	requireRetriable(t, err, "fast-path delete_vm")
	requireText(t, err, "fast-path delete_vm", []string{"VM 777", "slot scsi2", "stop is still in progress"})
	requireRefusedBeforeDestroy(t, client, recorder, volume, "fast-path delete_vm")
	requireNoParkerSlot(t, client.state.configs, "fast-path delete_vm")

	client.pending.stop(777)
	if _, err := HandleDeleteVM(deps).Handle(pendingRowContext(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("retried fast-path delete_vm after the VM stopped: %v", err)
	}
	if destroys := recorder.guestDestroys(); len(destroys) != 1 {
		t.Fatalf("VM 777 was destroyed %d times, want once", len(destroys))
	}
	carriers := 0
	for vmid, cfg := range client.state.configs {
		tags, _ := cfg["tags"].(string)
		for _, value := range qemu.ParseDisks(cfg) {
			if serial, ok := pve.StableIDFromDriveOptStr(value); ok && serial == idTestToken {
				if !tagsContain(tags, pve.ParkerTag) {
					t.Fatalf("VM %d carries the disk's serial and isn't a parker", vmid)
				}
				carriers++
			}
		}
	}
	if carriers != 1 {
		t.Fatalf("%d slots carry the disk's serial after the retry, want one parker slot", carriers)
	}
}

// TestDeleteVMSyncPath_VMRunningAgainSaysSo covers the synchronous path, which
// waits for the stop before it detaches anything. A VM that's running when the
// detach runs was started again by something else, so the text says that, and
// that the next delete_vm stops it and finishes the detach.
func TestDeleteVMSyncPath_VMRunningAgainSaysSo(t *testing.T) {
	deps, client, recorder, volume := legacyFastDeleteFixture(t, "")
	slow := false
	deps.Config.FastPathDelete = &slow
	// The awaited stop completes, and then something such as HA starts the
	// VM again before the detach runs.
	client.pending.restartAfterStop = map[int]bool{777: true}

	_, err := HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	requireRetriable(t, err, "sync delete_vm")
	requireText(t, err, "sync delete_vm", []string{"VM 777", "slot scsi2", "running again", "next delete_vm stops the VM"}, "still in progress")
	if destroys := recorder.guestDestroys(); len(destroys) != 0 {
		t.Fatalf("VM 777 was destroyed %d times while the delete of its disk was pending", len(destroys))
	}
	requireLegacyStillAttached(t, client, volume, "scsi2", "sync delete_vm")
}

// managedPendingSteps returns the active attempt's config-write steps that
// target VM 777.
func managedPendingSteps(record aj.Record) []aj.Step {
	var out []aj.Step
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() && step.Target.VMID == 777 && strings.HasSuffix(step.Kind, "_Nodes_UpdateQemuConfig") {
			out = append(out, *step)
		}
	}
	return out
}

// requireManagedPendingSettled checks a journal-managed operation whose slot
// delete stayed pending and was reverted. The record isn't left uncertain,
// every step is observed, and of the config writes on VM 777 exactly
// wantSettled, the deletes, are settled with no volume, so the pending delete
// was never recorded as an observed delete, while the revert after them names
// the volume.
func requireManagedPendingSettled(t *testing.T, journal *aj.Journal, id, volume, where string, wantSettled int) {
	t.Helper()
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("%s: the record was left uncertain: %+v", where, record)
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() && step.State != aj.Observed {
			t.Fatalf("%s: step %s (%s) is %s, want every step observed", where, step.ID, step.Kind, step.State)
		}
	}
	writes := managedPendingSteps(record)
	settled := 0
	for i := range writes {
		if len(writes[i].VolIDs) == 0 {
			settled++
		}
	}
	if settled != wantSettled || len(writes) != wantSettled+1 {
		t.Fatalf("%s: config writes on VM 777 = %+v, want %d deletes settled with no volume and the revert after them", where, writes, wantSettled)
	}
	last := writes[len(writes)-1]
	if len(last.VolIDs) != 1 || last.VolIDs[0] != volume {
		t.Fatalf("%s: the revert step = %+v, want it observed naming %s", where, last, volume)
	}
}

// TestManagedDetachDisk_PendingDeleteRevertAdmitted covers a managed legacy
// detach through managedDetachDisk with the delete left pending. The revert is
// admitted by the guard, the allocation isn't poisoned, and the pending delete
// isn't recorded as an observed delete.
func TestManagedDetachDisk_PendingDeleteRevertAdmitted(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	client.state.configs[777]["hotplug"] = "network,usb"
	client.pending = newFakePendingModel()
	client.pending.run(777)
	ctx := pendingRowContext()
	bare, meta, err := decodeDiskCID(ctx, deps, "detach_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(ctx, deps, "detach_disk", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	managed, lifecycle, err := managedDiskOperation(ctx, deps, rd, "detach_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("managed lifecycle: %v", err)
	}

	detachErr := detachDriveSlot(ctx, managed, "n1", 777, "scsi1", volume)
	pending, ok := pve.IsDriveDeletePending(detachErr)
	if !ok || pending.Reason != pve.DriveDeletePendingHotplug {
		t.Fatalf("managed detach = %v, want the reverted hotplug pending delete", detachErr)
	}
	if guardErr := lifecycle.guard.Err(); guardErr != nil {
		t.Fatalf("the guard was poisoned by the delete or its revert: %v", guardErr)
	}
	mapped := driveDeletePendingDiskError("detach_disk", detachErr)
	requirePermanent(t, mapped, "managed detach")
	if finishErr := lifecycle.finish(ctx, mapped, false); finishErr == nil || !strings.Contains(finishErr.Error(), `"network,usb"`) {
		t.Fatalf("finish = %v, want the hotplug refusal back unchanged", finishErr)
	}
	if value, _ := client.state.configs[777]["scsi1"].(string); !strings.HasPrefix(value, volume+",") || client.pending.pendingDelete(777, "scsi1") {
		t.Fatalf("VM 777 scsi1 = %q pending=%v, want the disk back with nothing pending", value, client.pending.pendingDelete(777, "scsi1"))
	}
	requireManagedPendingSettled(t, journal, id, volume, "managed detach", 1)
}

// TestManagedDetach_StableIDPendingDeleteSettlesCleanly covers a managed
// detach whose delete stays pending. The revert is admitted by the guard, the
// observation records it, the allocation isn't poisoned, and the record isn't
// left uncertain. The pending delete is never recorded as an observed delete.
func TestManagedDetach_StableIDPendingDeleteSettlesCleanly(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	client.state.configs[777]["hotplug"] = "network,usb"
	client.pending = newFakePendingModel()
	client.pending.run(777)
	deps.Config.DetachedDiskStrategy = "parked"

	_, err := HandleDetachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	requirePermanent(t, err, "managed detach")
	requireText(t, err, "managed detach", []string{"VM 777", `"network,usb"`, "scsi1"})
	if value, _ := client.state.configs[777]["scsi1"].(string); !strings.HasPrefix(value, volume+",") || client.pending.pendingDelete(777, "scsi1") {
		t.Fatalf("VM 777 scsi1 = %q, want the disk back with nothing pending", value)
	}
	requireNoParkerSlot(t, client.state.configs, "managed detach")
	requireManagedPendingSettled(t, journal, id, volume, "managed detach", 1)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReadyToReturn {
		t.Fatalf("record state = %s, want %s, the way the operation found it", record.State, aj.ReadyToReturn)
	}
}

// TestPendingModelServesTheDeleteFlag pins the flow fake's pending endpoint to
// PVE's shape for a held delete, so the rows above test the CPI and not a fake
// that reports something PVE never sends.
func TestPendingModelServesTheDeleteFlag(t *testing.T) {
	model := newFakePendingModel()
	model.run(777)
	cfg := map[string]any{"digest": "1", "hotplug": "network", "scsi1": "a:9001/vm-9001-disk-0.raw,size=5G"}
	slot := "scsi1"
	handled, err := model.update(777, cfg, &nodes.UpdateQemuConfigParams{Delete: &slot})
	if !handled || err != nil {
		t.Fatalf("hotplug-lacking delete: handled=%v err=%v, want it held with the PUT succeeding", handled, err)
	}
	resp := model.response(777, cfg)
	items := make([]map[string]any, 0, len(*resp))
	for _, raw := range *resp {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	found := false
	for _, item := range items {
		if item["key"] == "scsi1" {
			found = item["value"] == "a:9001/vm-9001-disk-0.raw,size=5G" && item["delete"] == float64(1) && item["pending"] == nil
		}
	}
	if !found {
		t.Fatalf("pending response = %v, want scsi1 with its current value and delete 1", items)
	}
	model.busy[777] = true
	cfg["hotplug"] = "network,disk"
	cfg["scsi2"] = "a:9002/vm-9002-disk-0.raw"
	slot2 := "scsi2"
	if handled, err := model.update(777, cfg, &nodes.UpdateQemuConfigParams{Delete: &slot2}); !handled || err == nil || !pve.IsHotUnplugBusy(err) {
		t.Fatalf("busy delete: handled=%v err=%v, want it held with PVE's busy error", handled, err)
	}
	revert := "scsi1,scsi2"
	if handled, err := model.update(777, cfg, &nodes.UpdateQemuConfigParams{Revert: &revert}); !handled || err != nil {
		t.Fatalf("revert: handled=%v err=%v", handled, err)
	}
	if model.pendingDelete(777, "scsi1") || model.pendingDelete(777, "scsi2") || cfg["scsi1"] == nil || cfg["scsi2"] == nil {
		t.Fatalf("after the revert cfg=%v, want both slots back with nothing pending", cfg)
	}
}

// pendingResumeFixture leaves the state a resume meets when an earlier
// attempt's delete stayed pending. A busy transfer writes its intent record on
// the parker and reverts its delete, and then a crash, an earlier release, or
// an operator leaves the slot's delete pending again. The source is missing
// from the guest listings, so the identity scan misses it and the resolver
// returns the intent.
func pendingResumeFixture(t *testing.T) (Deps, *idFakeClient, string) {
	t.Helper()
	const volid = pendingTransferVolid
	c := pendingTransferClient()
	c.pending.busy[700] = true
	deps := transferFunnelDeps(c)
	diskCID := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
	if err := handleDetachStableID(pendingRowContext(), deps, "700", 700, resolveTransferDisk(t, deps, diskCID)); err == nil {
		t.Fatal("setup: the busy transfer succeeded")
	}
	c.pending.busy[700] = false
	c.pending.holdDelete(700, c.configs[700], "scsi1")
	c.unlisted = map[int]bool{700: true}
	if rd := resolveTransferDisk(t, deps, diskCID); rd.intent == nil {
		t.Fatalf("setup: the resolver found no intent: %+v", rd)
	}
	return deps, c, diskCID
}

// requireLeftAlone checks that the resume sent no revert and no delete, and
// left the pending delete and the parker as it found them.
func requireLeftAlone(t *testing.T, c *idFakeClient, reverts, deletes int, where string) {
	t.Helper()
	if len(c.pending.reverts) != reverts {
		t.Fatalf("%s: reverts went from %d to %v, want zero new revert calls", where, reverts, c.pending.reverts)
	}
	if len(c.pending.deleteCalls) != deletes {
		t.Fatalf("%s: deletes went from %d to %v, want none sent", where, deletes, c.pending.deleteCalls)
	}
	if !c.pending.pendingDelete(700, "scsi1") {
		t.Fatalf("%s: the pending delete is gone, want it left alone", where)
	}
	requireNoParkerSlot(t, c.configs, where)
}

// TestDetachDisk_ResumeLeavesAFoundPendingDeleteAlone covers detach_disk
// through the resume window. The resume finds the source's delete pending,
// leaves it alone, and attaches nothing, and detach_disk fails retriably,
// because the transfer finishes once the VM stops.
func TestDetachDisk_ResumeLeavesAFoundPendingDeleteAlone(t *testing.T) {
	deps, c, diskCID := pendingResumeFixture(t)
	reverts, deletes := len(c.pending.reverts), len(c.pending.deleteCalls)

	err := handleDetachStableID(pendingRowContext(), deps, "700", 700, resolveTransferDisk(t, deps, diskCID))
	requireRetriable(t, err, "detach through the resume")
	requireText(t, err, "detach through the resume",
		[]string{"earlier attempt", "slot scsi1", "VM 700", "once the VM stops"}, "reverted", "revert it")
	requireLeftAlone(t, c, reverts, deletes, "detach through the resume")
}

// TestDeleteDisk_ResumeLeavesAFoundPendingDeleteAlone covers delete_disk
// through the resume window. It gets the same non-retriable refusal a holder
// with a pending delete gets, whichever route finds the shape, and nothing is
// reverted or deleted.
func TestDeleteDisk_ResumeLeavesAFoundPendingDeleteAlone(t *testing.T) {
	deps, c, diskCID := pendingResumeFixture(t)
	reverts, deletes := len(c.pending.reverts), len(c.pending.deleteCalls)

	_, err := HandleDeleteDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, diskCID)}, jsonrpc.Context{})
	requirePermanent(t, err, "delete_disk through the resume")
	if want := pendingDeleteRefusal(diskCID, 700, "pve1", "scsi1").Error(); err.Error() != want {
		t.Fatalf("delete_disk through the resume = %q, want the holder refusal %q", err, want)
	}
	requireLeftAlone(t, c, reverts, deletes, "delete_disk through the resume")
	if len(c.destroyed) != 0 {
		t.Fatalf("delete_disk destroyed %v", c.destroyed)
	}
}

// TestDeleteVM_StoppedVMWithALeftPendingDeleteKeepsTheVolume covers a stopped
// VM whose disk slot carries a pending delete that a crash or a kill left
// behind, because only a clean stop applies one. The config endpoint leaves
// that slot out, but PVE's destroy works from the current config, which still
// has it. delete_vm reads both views, detaches the slot, which a stopped VM
// applies at once, and only then destroys the VM, so the destroy never takes
// the volume. One row holds a legacy foreign disk and the other a stable-ID
// disk, which goes to a parker.
func TestDeleteVM_StoppedVMWithALeftPendingDeleteKeepsTheVolume(t *testing.T) {
	for name, serial := range map[string]string{"legacy foreign disk": "", "stable-ID disk": idTestToken} {
		t.Run(name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			deps, client, recorder, volume := legacyFastDeleteFixture(t, serial)
			slow := false
			deps.Config.FastPathDelete = &slow
			client.pending.stop(777)
			client.pending.holdDelete(777, client.state.configs[777], "scsi2")

			if _, err := HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
				t.Fatalf("delete_vm of a stopped VM with a left pending delete: %v", err)
			}
			destroys := recorder.guestDestroys()
			if len(destroys) != 1 {
				t.Fatalf("VM 777 was destroyed %d times, want once", len(destroys))
			}
			if client.state.volumes[volume] == nil && serial == "" {
				t.Fatalf("the destroy took the volume %s of a slot whose delete was pending", volume)
			}
			if text, _ := destroys[0].configs[777]["scsi2"].(string); text != "" {
				t.Fatalf("scsi2 still named %q when the destroy went out", text)
			}
			if serial != "" {
				requireOneParkerCarrier(t, client, serial)
			}
		})
	}
}

// TestDeleteVMFastPath_RunningVMWithALeftPendingDeleteKeepsTheVolume covers the
// fast path on a VM that's still running with hotplug lacking disk, whose
// disk slot already carries a pending delete. delete_vm sees the slot in the
// current view, the detach's delete stays pending and is reverted, and
// delete_vm fails retriably without destroying anything.
func TestDeleteVMFastPath_RunningVMWithALeftPendingDeleteKeepsTheVolume(t *testing.T) {
	deps, client, recorder, volume := legacyFastDeleteFixture(t, "")
	client.pending.holdDelete(777, client.state.configs[777], "scsi2")

	_, err := HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	requireRetriable(t, err, "fast-path delete_vm")
	if destroys := recorder.guestDestroys(); len(destroys) != 0 {
		t.Fatalf("VM 777 was destroyed %d times while its disk slot's delete was pending", len(destroys))
	}
	if client.state.volumes[volume] == nil {
		t.Fatal("the volume is gone")
	}
	requireLegacyStillAttached(t, client, volume, "scsi2", "fast-path delete_vm")
}

// TestDeleteVM_PendingDriveReplacementIsRefused covers a slot whose pending
// value replaces its current drive, which a CPI attach can leave behind on a
// running VM. PVE's destroy frees an owned volume that either value names, so
// the destroy guards see both volumes on the slot, and delete_vm refuses
// retriably, naming both, without destroying anything.
func TestDeleteVM_PendingDriveReplacementIsRefused(t *testing.T) {
	deps, client, recorder, volume := legacyFastDeleteFixture(t, "")
	slow := false
	deps.Config.FastPathDelete = &slow
	client.pending.stop(777)
	storage := strings.Split(volume, ":")[0]
	second := storage + ":9002/vm-9002-disk-0.raw"
	client.state.volumes[second] = client.state.volumes[volume]
	client.pending.holdReplacement(777, client.state.configs[777], "scsi2", second+",size=5G")

	holding, err := pve.ReadQemuHolding(pendingRowContext(), client, "n1", 777)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{volume, second}
	if volume > second {
		want = []string{second, volume}
	}
	if got := holding.Volumes("scsi2"); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Volumes(scsi2) = %v, want both %v", got, want)
	}

	_, err = HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	requireRetriable(t, err, "delete_vm with a pending drive replacement")
	requireText(t, err, "delete_vm with a pending drive replacement", []string{"VM 777", "scsi2", volume, second, "Nothing was destroyed"})
	if destroys := recorder.guestDestroys(); len(destroys) != 0 {
		t.Fatalf("VM 777 was destroyed %d times while a slot named two volumes", len(destroys))
	}
	if client.state.volumes[volume] == nil || client.state.volumes[second] == nil {
		t.Fatal("a volume the slot names is gone")
	}
}

// TestNextFreeSCSIIndexInViews_CountsEitherViewAndPendingDeletes pins the slot
// choice every disk attach shares. A key counts as occupied when either view
// has it, which takes in a slot whose delete is pending and a slot that
// exists only as a pending add.
func TestNextFreeSCSIIndexInViews_CountsEitherViewAndPendingDeletes(t *testing.T) {
	views, err := pve.ReadQemuViews(context.Background(), &pendingRowClient{resp: nodes.ListQemuPendingResponse{
		json.RawMessage(`{"key":"scsi0","value":"a:1/vm-1-disk-0.raw"}`),
		json.RawMessage(`{"key":"scsi1","value":"a:9/vm-9-disk-0.raw","delete":1}`),
		json.RawMessage(`{"key":"scsi2","pending":"a:9/vm-9-disk-2.raw"}`),
		json.RawMessage(`{"key":"scsi3","value":"a:9/vm-9-disk-3.raw"}`),
		json.RawMessage(`{"key":"scsi5","value":"a:9/vm-9-disk-5.raw"}`),
	}}, "n1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := nextFreeSCSIIndexInViews(views); got != 4 {
		t.Fatalf("next free scsi index = %d, want 4, past the pending delete, the pending add, and scsi3", got)
	}
	if got := nextFreeSCSIIndexAtLeast(views.Applied(), 1); got != 1 {
		t.Fatalf("setup: the applied view alone gives %d, want 1, which is the slot the pending delete hides", got)
	}
}

// TestAttachDisk_ReattachRevertsThePendingDeleteOnItsOwnSlot covers attach_disk
// onto the VM that already holds the disk on a slot whose delete is pending.
// The slot choice reads both views, reverts that pending delete, confirms it
// with a pending read, and attaches on the disk's own slot. In the second
// shape the disk's slot sits above a free lower slot, and the attach still
// returns the disk's own slot and writes nothing to the lower one.
func TestAttachDisk_ReattachRevertsThePendingDeleteOnItsOwnSlot(t *testing.T) {
	for name, slot := range map[string]string{"lowest free slot": "scsi1", "above a free lower slot": "scsi3"} {
		t.Run(name, func(t *testing.T) {
			deps, client, volume, cid := legacyPendingFixture(t, slot, "")
			client.pending.holdDelete(777, client.state.configs[777], slot)

			result, err := HandleAttachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
			if err != nil {
				t.Fatalf("attach onto the disk's own pending-deleted slot: %v", err)
			}
			if hints, ok := result.(diskHints); !ok || !strings.HasSuffix(hints.Path, "drive-"+slot) {
				t.Fatalf("attach result = %+v, want the device path of %s", result, slot)
			}
			if got := len(client.pending.reverts); got != 1 {
				t.Fatalf("reverts = %v, want exactly one", client.pending.reverts)
			}
			requireLegacyStillAttached(t, client, volume, slot, "attach")
			for _, lower := range []string{"scsi1", "scsi2"} {
				if lower == slot {
					continue
				}
				if value, present := client.state.configs[777][lower]; present {
					t.Fatalf("attach wrote %v to %s, want nothing on a slot below the disk's own", value, lower)
				}
			}
		})
	}
}

// TestAttachDisk_ReattachWithAnUnconfirmedRevertIsRetriable covers the same
// reattach when the revert can't be confirmed. attach_disk fails retriably
// with the revert_unconfirmed error and writes the disk nowhere else.
func TestAttachDisk_ReattachWithAnUnconfirmedRevertIsRetriable(t *testing.T) {
	deps, client, volume, cid := legacyPendingFixture(t, "scsi3", "")
	client.pending.holdDelete(777, client.state.configs[777], "scsi3")
	client.pending.revertKeepsPending = true

	_, err := HandleAttachDisk(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	requireRetriable(t, err, "attach with an unconfirmed revert")
	requireText(t, err, "attach with an unconfirmed revert", []string{"slot scsi3", "VM 777", "couldn't confirm"})
	if got := len(client.pending.reverts); got != 1 {
		t.Fatalf("reverts = %v, want exactly one", client.pending.reverts)
	}
	if !client.pending.pendingDelete(777, "scsi3") {
		t.Fatal("setup: the revert cleared the pending delete")
	}
	for key, raw := range client.state.configs[777] {
		if text, _ := raw.(string); strings.Split(text, ",")[0] == volume {
			t.Fatalf("attach wrote the disk to %s while its own slot's delete is pending", key)
		}
	}
}

// requireOneParkerCarrier checks that exactly one slot carries serial, that it
// sits on a parker, and that its volume still exists.
func requireOneParkerCarrier(t *testing.T, client *lifecycleFlowPVE, serial string) {
	t.Helper()
	carriers := 0
	for vmid, cfg := range client.state.configs {
		tags, _ := cfg["tags"].(string)
		for _, value := range qemu.ParseDisks(cfg) {
			got, ok := pve.StableIDFromDriveOptStr(value)
			if !ok || got != serial {
				continue
			}
			if !tagsContain(tags, pve.ParkerTag) {
				t.Fatalf("VM %d carries the disk's serial and isn't a parker", vmid)
			}
			if landed := strings.Split(value, ",")[0]; client.state.volumes[landed] == nil {
				t.Fatalf("the parked volume %s is gone", landed)
			}
			carriers++
		}
	}
	if carriers != 1 {
		t.Fatalf("%d slots carry the disk's serial, want one parker slot", carriers)
	}
}

// replacementFixture is legacyFastDeleteFixture with a second volume on
// storage and VM 777's scsi2 carrying a pending value that names it, while the
// current value still names the first. The pending volume is a foreign one,
// or a legacy volume named for VM 777 itself when owned is set.
func replacementFixture(t *testing.T, fast, owned bool) (Deps, *lifecycleFlowPVE, *legacyDestroyRecorder, string, string) {
	t.Helper()
	deps, client, recorder, volume := legacyFastDeleteFixture(t, "")
	deps.Config.FastPathDelete = &fast
	pendingVolume := strings.Split(volume, ":")[0] + ":9002/vm-9002-disk-0.raw"
	if owned {
		pendingVolume = strings.Split(volume, ":")[0] + ":777/vm-777-disk-7.raw"
	}
	client.state.volumes[pendingVolume] = client.state.volumes[volume]
	client.pending.holdReplacement(777, client.state.configs[777], "scsi2", pendingVolume+",size=5G")
	return deps, client, recorder, volume, pendingVolume
}

// requireVolumesKept checks that a destroy went out exactly once and freed
// none of the named volumes.
func requireVolumesKept(t *testing.T, client *lifecycleFlowPVE, recorder *legacyDestroyRecorder, where string, volumes ...string) {
	t.Helper()
	if destroys := recorder.guestDestroys(); len(destroys) != 1 {
		t.Fatalf("%s: VM 777 was destroyed %d times, want once", where, len(destroys))
	}
	for _, volume := range volumes {
		if client.state.volumes[volume] == nil {
			t.Fatalf("%s: the destroy freed %s", where, volume)
		}
	}
}

// TestDeleteVMSyncPath_PendingDriveReplacementAppliedByTheStop covers the sync
// path on a running VM whose scsi2 has a pending value naming a second
// foreign volume. delete_vm stops the VM, the awaited stop applies the change,
// and the destroy goes out once with neither volume freed.
func TestDeleteVMSyncPath_PendingDriveReplacementAppliedByTheStop(t *testing.T) {
	deps, client, recorder, volume, second := replacementFixture(t, false, false)

	if _, err := HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatalf("sync delete_vm of a running VM with a pending drive replacement: %v", err)
	}
	requireVolumesKept(t, client, recorder, "sync delete_vm", volume, second)
}

// TestDeleteVMFastPath_PendingDriveReplacementRefusedUntilTheStopApplies covers
// the fast path on the same VM. The first attempt issues the stop, which
// hasn't finished, and refuses retriably after it, naming both volumes. Once
// the stop has applied the change, the retry destroys the VM with neither
// volume freed.
func TestDeleteVMFastPath_PendingDriveReplacementRefusedUntilTheStopApplies(t *testing.T) {
	deps, client, recorder, volume, second := replacementFixture(t, true, false)
	args := []json.RawMessage{planJSON(t, "777")}

	_, err := HandleDeleteVM(deps).Handle(pendingRowContext(), args, jsonrpc.Context{})
	requireRetriable(t, err, "fast-path delete_vm")
	requireText(t, err, "fast-path delete_vm", []string{"VM 777", "scsi2", volume, second, "next clean stop", "Nothing was destroyed"}, "revert")
	if len(client.pending.stopRequests) != 1 {
		t.Fatalf("stop requests = %v, want the fast path's stop issued before the refusal", client.pending.stopRequests)
	}
	if destroys := recorder.guestDestroys(); len(destroys) != 0 {
		t.Fatalf("VM 777 was destroyed %d times while a slot named two volumes", len(destroys))
	}

	client.pending.completeStops(client.state.configs)
	if _, err := HandleDeleteVM(deps).Handle(pendingRowContext(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("retried fast-path delete_vm after the stop applied the change: %v", err)
	}
	requireVolumesKept(t, client, recorder, "retried fast-path delete_vm", volume, second)
}

// TestDeleteVM_OwnedLegacyVolumeOnlyAPendingValueNamesRefusesBeforeTheStop
// covers a pending value naming a legacy volume named for the VM itself,
// which the destroy would free. delete_vm refuses with the owned-legacy text
// before it stops the VM.
func TestDeleteVM_OwnedLegacyVolumeOnlyAPendingValueNamesRefusesBeforeTheStop(t *testing.T) {
	for name, fast := range map[string]bool{"sync path": false, "fast path": true} {
		t.Run(name, func(t *testing.T) {
			deps, client, recorder, _, owned := replacementFixture(t, fast, true)
			// The owned volume is a persistent disk the Director attached, so
			// the VM's description records its CID.
			cid, err := pve.EncodeDiskCID(owned, nil)
			if err != nil {
				t.Fatal(err)
			}
			recorded, err := json.Marshal(map[string]string{owned: cid})
			if err != nil {
				t.Fatal(err)
			}
			description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_attached_disks": recorded})
			if err != nil {
				t.Fatal(err)
			}
			client.state.configs[777]["description"] = description

			_, err = HandleDeleteVM(deps).Handle(pendingRowContext(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
			requirePermanent(t, err, "delete_vm")
			requireText(t, err, "delete_vm", []string{"VM 777", owned, "is named for VMID 777"})
			if len(client.pending.stopRequests) != 0 {
				t.Fatalf("stop requests = %v, want the refusal before any stop", client.pending.stopRequests)
			}
			if destroys := recorder.guestDestroys(); len(destroys) != 0 {
				t.Fatalf("VM 777 was destroyed %d times", len(destroys))
			}
		})
	}
}
