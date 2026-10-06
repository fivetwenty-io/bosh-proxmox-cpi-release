package handlers

// These tests cover delete_vm on a VM that holds a journal-managed disk whose
// provenance entry was never written, because attach_disk moved the disk onto
// the VM and its holder write then failed. The VM's notes carry neither the
// entry nor the disk's CID. The Director calls delete_vm without a
// detach_disk first on a scale-in, a plain delete-deployment, and a recreate
// of a VM whose agent doesn't answer, so delete_vm has to preserve the disk
// itself. It takes the disk's CID from the allocation journal, and the disk's
// lifecycle writes the missing entry under the disk's allocation lock before
// the disk moves to its parker. A disk the journal doesn't know keeps the
// refusal it had.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// unrecordedHolderSlot returns the slot on VM 777 that carries the disk's
// serial.
func unrecordedHolderSlot(t *testing.T, disk *parkedFlowDisk, serial string) string {
	t.Helper()
	for key, value := range qemu.ParseDisks(disk.client.state.configs[777]) {
		if got, ok := pve.StableIDFromDriveOptStr(value); ok && got == serial {
			return key
		}
	}
	t.Fatalf("777 carries no slot with serial %s: %v", serial, disk.client.state.configs[777])
	return ""
}

// diskToken returns the stable token in the disk's CID.
func diskToken(t *testing.T, disk *parkedFlowDisk) string {
	t.Helper()
	_, meta, err := pve.ParseEncodedDiskCID(disk.cid)
	if err != nil || meta == nil || meta.ID == "" {
		t.Fatalf("disk CID %s carries no stable token (%v)", disk.cid, err)
	}
	return meta.ID
}

// requireUntouched fails unless VM 777, the parker, and the disk's record are
// exactly as they were, and nothing wrote to 777's notes.
func requireUntouched(t *testing.T, disk *parkedFlowDisk, writes *holderWrites, holder, parker map[string]any, record aj.Record) {
	t.Helper()
	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("delete_vm wrote 777's notes %d times, want never", descriptions)
	}
	if got := disk.client.state.configs[777]; !reflect.DeepEqual(got, holder) {
		t.Fatalf("777 changed to %v, want %v", got, holder)
	}
	if got := disk.client.state.configs[disk.parker]; !reflect.DeepEqual(got, parker) {
		t.Fatalf("parker %d changed to %v, want %v", disk.parker, got, parker)
	}
	if got := disk.record(t); !reflect.DeepEqual(got, record) {
		t.Fatalf("the disk's record changed to %+v, want %+v", got, record)
	}
}

// requireUnsettledStepRefusal fails unless err is the heal's refusal for a
// holder that lacks the disk's entry while step, which no readback settles,
// is unsettled in the disk's record. That refusal names the step and the
// guide section for the investigation, and it is not retriable.
func requireUnsettledStepRefusal(t *testing.T, what string, err error, step string) {
	t.Helper()
	if !isHolderNotRecorded(err) {
		t.Fatalf("%s err = %v, want the heal's refusal for a missing provenance entry", what, err)
	}
	requirePermanent(t, err, what)
	requireText(t, err, what, []string{"without its provenance entry", step, "which no readback settles", `"Choose the command for the record"`}, "retry the operation", "requires reconciliation")
}

// TestDeleteVMPreservesADiskWhoseHolderEntryWasNeverWritten runs delete_vm's
// preservation on 777 while 777 holds the disk without its provenance entry
// or its CID. The disk's lifecycle writes the entry once, under the disk's
// allocation lock, and then moves the disk to its parker, which records it
// under the disk's allocation. The allocation is returned with every step
// settled, and 777 no longer holds the disk.
func TestDeleteVMPreservesADiskWhoseHolderEntryWasNeverWritten(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	token := diskToken(t, disk)
	unrecordedHolderSlot(t, disk, token)

	if err := detachManagedPersistentForVMDelete(t.Context(), disk.deps, "n1", 777, nil, nil); err != nil {
		t.Fatalf("delete_vm's preservation failed on a disk the journal knows: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
	for _, value := range qemu.ParseDisks(disk.client.state.configs[777]) {
		if serial, ok := pve.StableIDFromDriveOptStr(value); ok && serial == token {
			t.Fatalf("777 still holds the disk as %s", value)
		}
	}
	parker := disk.client.state.configs[disk.parker]
	parked := ""
	for _, value := range qemu.ParseDisks(parker) {
		if serial, ok := pve.StableIDFromDriveOptStr(value); ok && serial == token {
			parked = strings.Split(value, ",")[0]
		}
	}
	if parked == "" {
		t.Fatalf("parker %d doesn't hold the disk: %v", disk.parker, parker)
	}
	entry, found, err := pve.FindDiskAllocationProvenance(pve.DescriptionFromConfig(parker), token)
	if err != nil || !found || entry.AllocationID != disk.id || entry.Volid != parked {
		t.Fatalf("parker %d records the disk as %+v (found %v, err %v), want allocation %s on %s", disk.parker, entry, found, err, disk.id, parked)
	}
	assertReturnedRecord(t, "preserved", disk.record(t))
}

// TestDeleteVMWaitsForAnUnsettledStepBeforeHealingTheHolder leaves a step of
// the disk's record planned that no readback settles. delete_vm's
// preservation then writes nothing and returns the heal's refusal, which
// names the step and the investigation it needs and is not retriable,
// because no retry settles such a step. 777, the parker, and the record stay
// exactly as they were.
func TestDeleteVMWaitsForAnUnsettledStepBeforeHealingTheHolder(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	record := disk.record(t)
	planned := fmt.Sprintf("attempt-%d-step-99", record.ActiveAttempt())
	record.Steps = append(record.Steps, aj.Step{ID: planned, Attempt: record.ActiveAttempt(), Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", State: aj.Planned, Target: aj.Target{Node: "n1", VMID: 777}})
	rewriteJournalRecord(t, disk.deps, disk.id, record)
	record = disk.record(t)
	holder := maps.Clone(disk.client.state.configs[777])
	parker := maps.Clone(disk.client.state.configs[disk.parker])

	err := detachManagedPersistentForVMDelete(t.Context(), disk.deps, "n1", 777, nil, nil)
	requireUnsettledStepRefusal(t, "delete_vm's preservation with a planned step", err, planned)
	requireUntouched(t, disk, writes, holder, parker, record)
}

// TestDeleteVMKeepsRefusingADiskTheJournalDoesNotKnow gives 777's slot a
// serial that no allocation record names, with no provenance entry and no CID
// in 777's notes. delete_vm's preservation keeps the refusal it gave before,
// which is not retriable, and writes nothing.
func TestDeleteVMKeepsRefusingADiskTheJournalDoesNotKnow(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	token := diskToken(t, disk)
	slot := unrecordedHolderSlot(t, disk, token)
	unknown := "bpd-00000000000000aa"
	value, _ := pve.ConfigString(disk.client.state.configs[777], slot)
	disk.client.state.configs[777][slot] = strings.Replace(value, "serial="+token, "serial="+unknown, 1)
	record := disk.record(t)
	holder := maps.Clone(disk.client.state.configs[777])
	parker := maps.Clone(disk.client.state.configs[disk.parker])

	err := detachManagedPersistentForVMDelete(t.Context(), disk.deps, "n1", 777, nil, nil)
	if err == nil || err.Error() != "legacy persistent disk lacks its recorded CPI CID" {
		t.Fatalf("err = %v, want the refusal for a disk with no recorded CID", err)
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want it not retriable", err)
	}
	requireUntouched(t, disk, writes, holder, parker, record)
}

// unrecordedHolderManagedVM makes 777 a journal-managed VM that holds the
// unrecorded disk, the way create_vm leaves a VM it made. Its allocation
// record has one observed root step and is ready to return, and 777's notes
// carry the record's marker. The root volume itself is left out, because the
// fake PVE refuses to destroy a VM while any volume remains on it, and only
// the persistent disk matters here. It returns the VM record's ID.
func unrecordedHolderManagedVM(t *testing.T, disk *parkedFlowDisk) string {
	t.Helper()
	ctx := t.Context()
	prior, err := disk.journal.Inspect(disk.id)
	if err != nil {
		t.Fatal(err)
	}
	frozenPlan, err := activeStorageAllocationPlan(prior)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := managedDiskActualDefinition(ctx, disk.deps, "b")
	if err != nil {
		t.Fatal(err)
	}
	frozenPlan.Definitions["b"] = definition
	frozenPlan.AllocationKey = "vm-agent"
	intent := prior.Intent
	if intent.Plan, err = json.Marshal(frozenPlan); err != nil {
		t.Fatal(err)
	}
	handle, err := disk.journal.AcquireVM(ctx, "vm-agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	root := "b:777/vm-777-disk-0.raw"
	step, err := storageMutationIntent(handle, "vm.root.scsi0", aj.Target{Node: "n1", VMID: 777, Storage: "b", Backing: definition.BackingKey(), IntendedVolume: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, []string{root}, false); err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State = aj.ReadyToReturn
	record.CID = "777"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("vm-agent"))
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: record.Namespace, AllocationID: record.ID, AgentSHA256: hex.EncodeToString(hash[:])})
	if err != nil {
		t.Fatal(err)
	}
	desc, _ := disk.client.state.configs[777]["description"].(string)
	disk.client.state.configs[777]["description"] = marker + desc
	return record.ID
}

// deleteVM777 runs the delete_vm handler for VM 777.
func deleteVM777(t *testing.T, disk *parkedFlowDisk) error {
	t.Helper()
	_, err := HandleDeleteVM(disk.deps).Handle(t.Context(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	return err
}

// requireDeletedAndPreserved fails unless delete_vm destroyed 777 and closed
// its allocation, and the disk's allocation was returned with every step
// settled.
func requireDeletedAndPreserved(t *testing.T, disk *parkedFlowDisk, vmID string) {
	t.Helper()
	if cfg := disk.client.state.configs[777]; len(cfg) != 0 {
		t.Fatalf("delete_vm left 777 in place: %v", cfg)
	}
	vm, err := disk.journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != aj.Deleted {
		t.Fatalf("the VM's allocation is %s (reason %q), want %s", vm.State, vm.Reason, aj.Deleted)
	}
	assertReturnedRecord(t, "preserved", disk.record(t))
}

// TestDeleteVMHandlerHealsTheHolderAndDeletesTheVM runs the delete_vm handler
// on a journal-managed 777 that holds the disk without its provenance entry
// or its CID. The disk's lifecycle writes the entry once under the disk's
// allocation lock and moves the disk to its parker, and then delete_vm
// destroys 777 and closes its allocation.
func TestDeleteVMHandlerHealsTheHolderAndDeletesTheVM(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	token := diskToken(t, disk)
	vmID := unrecordedHolderManagedVM(t, disk)

	if err := deleteVM777(t, disk); err != nil {
		t.Fatalf("delete_vm failed on a VM that holds a disk the journal knows: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
	parker := disk.client.state.configs[disk.parker]
	entry, found, err := pve.FindDiskAllocationProvenance(pve.DescriptionFromConfig(parker), token)
	if err != nil || !found || entry.AllocationID != disk.id {
		t.Fatalf("parker %d records the disk as %+v (found %v, err %v), want allocation %s", disk.parker, entry, found, err, disk.id)
	}
	requireDeletedAndPreserved(t, disk, vmID)
}

// TestDeleteVMHandlerRefusesAnUnsettledStepOnEveryRetry leaves a step of the
// disk's record planned that no readback settles, and runs the delete_vm
// handler three times without touching the record. Every call returns the
// heal's refusal, which names the step and the investigation it needs and is
// not retriable, so the Director doesn't retry into the same answer. Every
// call leaves the VM's allocation observed rather than requiring
// reconciliation, writes nothing to 777's notes, and leaves the disk on 777
// with the step still planned.
func TestDeleteVMHandlerRefusesAnUnsettledStepOnEveryRetry(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	token := diskToken(t, disk)
	vmID := unrecordedHolderManagedVM(t, disk)
	record := disk.record(t)
	planned := fmt.Sprintf("attempt-%d-step-99", record.ActiveAttempt())
	record.Steps = append(record.Steps, aj.Step{ID: planned, Attempt: record.ActiveAttempt(), Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", State: aj.Planned, Target: aj.Target{Node: "n1", VMID: 777}})
	rewriteJournalRecord(t, disk.deps, disk.id, record)

	for try := 1; try <= 3; try++ {
		what := fmt.Sprintf("delete_vm try %d with a planned step", try)
		err := deleteVM777(t, disk)
		requireUnsettledStepRefusal(t, what, err, planned)
		vm, inspectErr := disk.journal.Inspect(vmID)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if vm.State != aj.Observed {
			t.Fatalf("%s left the VM's allocation %s (reason %q), want %s", what, vm.State, vm.Reason, aj.Observed)
		}
		if !strings.Contains(vm.Reason, planned) || !strings.Contains(vm.Reason, "which no readback settles") {
			t.Fatalf("%s left the VM's allocation with reason %q, want it to name step %s", what, vm.Reason, planned)
		}
		if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
			t.Fatalf("%s wrote 777's notes %d times, want never", what, descriptions)
		}
		unrecordedHolderSlot(t, disk, token)
		state := aj.State("")
		for _, step := range disk.record(t).Steps {
			if step.ID == planned {
				state = step.State
			}
		}
		if state != aj.Planned {
			t.Fatalf("%s left step %s %q, want it still planned", what, planned, state)
		}
	}
}

// TestDeleteVMHandlerRetriesAPendingProtectionRestoreAndThenDeletes leaves the
// parker's protection restore planned in the disk's record while the parker
// reads back unprotected. The settler keeps the step planned, so the
// delete_vm handler returns a retriable error that names the protection write
// rather than a reconciliation error. It leaves the VM's allocation observed,
// writes nothing to 777's notes, and leaves the disk on 777. Once the parker
// reads back protected, the next delete_vm settles the step by readback,
// writes the entry under the disk's allocation lock, preserves the disk, and
// deletes the VM.
func TestDeleteVMHandlerRetriesAPendingProtectionRestoreAndThenDeletes(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	token := diskToken(t, disk)
	vmID := unrecordedHolderManagedVM(t, disk)
	plantPlannedRestore(t, disk)
	disk.client.state.configs[disk.parker]["protection"] = 0

	err := deleteVM777(t, disk)
	what := "delete_vm with the parker's protection restore unsettled"
	requireRetriable(t, err, what)
	requireText(t, err, what, []string{"parker protection write", "retry once the parker reads back protected"}, "requires reconciliation")
	vm, inspectErr := disk.journal.Inspect(vmID)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if vm.State != aj.Observed {
		t.Fatalf("%s left the VM's allocation %s (reason %q), want %s", what, vm.State, vm.Reason, aj.Observed)
	}
	if !strings.Contains(vm.Reason, "a parker protection write") {
		t.Fatalf("%s left the VM's allocation with reason %q, want it to name the protection write", what, vm.Reason)
	}
	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("%s wrote 777's notes %d times, want never", what, descriptions)
	}
	unrecordedHolderSlot(t, disk, token)

	disk.client.state.configs[disk.parker]["protection"] = 1
	if err := deleteVM777(t, disk); err != nil {
		t.Fatalf("delete_vm retried once the parker read back protected: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
	if steps := unsettledSteps(t, disk); len(steps) != 0 {
		t.Fatalf("the record keeps unsettled steps %v after the delete", steps)
	}
	requireDeletedAndPreserved(t, disk, vmID)
}

// TestDeleteVMHealsAHolderWhoseNotesRecordTheCID gives 777's notes the disk's
// CID but no provenance entry. delete_vm's preservation writes the entry
// under the disk's allocation lock and moves the disk to its parker, as it
// does when the notes lack the CID too, instead of refusing a disk that no
// later call would heal.
func TestDeleteVMHealsAHolderWhoseNotesRecordTheCID(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	token := diskToken(t, disk)
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(disk.client.state.configs[777]))
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	recorded, err := json.Marshal(map[string]string{token: disk.cid})
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_attached_disks"] = recorded
	desc, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	disk.client.state.configs[777]["description"] = desc

	if err := detachManagedPersistentForVMDelete(t.Context(), disk.deps, "n1", 777, nil, nil); err != nil {
		t.Fatalf("delete_vm's preservation failed on a disk whose CID 777's notes record: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
	for _, value := range qemu.ParseDisks(disk.client.state.configs[777]) {
		if serial, ok := pve.StableIDFromDriveOptStr(value); ok && serial == token {
			t.Fatalf("777 still holds the disk as %s", value)
		}
	}
	assertReturnedRecord(t, "preserved", disk.record(t))
}

// TestVMCleanupFailureHandsBackAnUnrecordedHolderRefusal covers the refusals
// delete_vm's preservation returns before it changes a disk. The heal's
// retriable refusal, as the disk's lifecycle hands it back after closing the
// journal, the failed journal read that looks up the disk's CID, and a parker
// protection write that waits on the parker's lock or on the parker reading
// back protected all go back to the Director unchanged and retriable. The
// heal's refusal for a step that no readback settles goes back unchanged and
// not retriable. Each leaves the VM's allocation observed, with a reason that
// names the refusal and says what clears it. The heal's refusal joined with another
// failure still requires reconciliation.
func TestVMCleanupFailureHandsBackAnUnrecordedHolderRefusal(t *testing.T) {
	t.Parallel()
	refusal := func() error {
		rd := resolvedDisk{diskCID: "pvd-disk", volid: "b:777/vm-777-disk-1.raw", holder: &pve.DiskHolder{Found: true, Node: "n1", VMID: 777}}
		return holderNotRecordedRefusal(rd, ", and step attempt-0-step-99 (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned in the disk's record")
	}
	journalUnread := func(t *testing.T) error {
		disk, _ := unrecordedHolderDisk(t)
		matches, err := filepath.Glob(filepath.Join(disk.deps.Config.StorageAllocationJournalDir, "*", "allocation-"+disk.id+".json"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("want one record file for %s, got %v (%v)", disk.id, matches, err)
		}
		if err := os.WriteFile(matches[0], []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		cid, err := managedVMJournalDiskCID(disk.deps, 777, "b:777/vm-777-disk-1.raw", diskToken(t, disk))
		if err == nil {
			t.Fatalf("the journal lookup read a broken record and found %q", cid)
		}
		return err
	}
	unsettled := func(*testing.T) error {
		rd := resolvedDisk{diskCID: "pvd-disk", volid: "b:777/vm-777-disk-1.raw", holder: &pve.DiskHolder{Found: true, Node: "n1", VMID: 777}}
		step := aj.Step{ID: "attempt-0-step-99", Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", State: aj.Planned}
		return errors.Join(unsettledStepHolderRefusal(rd, step), nil, nil)
	}
	gap := storageRefusal("lifecycle has unresolved mutation evidence; step attempt-0-step-18 (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned; its parker protection write could not be settled because parker 90000 reads back unprotected")
	lockBusy := func(*testing.T) error {
		return errors.Join(cpierrors.WrapAs(&protectionPendingRefusal{err: gap}, cpierrors.TypeRetriableCloud, "another operation on the parker is in progress, so retry"), nil, nil)
	}
	unprotected := func(*testing.T) error {
		return protectionPendingPreservation(777, errors.Join(&protectionPendingRefusal{err: gap}, nil, nil))
	}
	resumes := "the next delete_vm or storage-journal cleanup of this record resumes it"
	for _, tc := range []struct {
		name      string
		cause     func(t *testing.T) error
		permanent bool
		reconcile bool
		reason    []string
	}{
		{name: "heal refusal", cause: func(*testing.T) error { return errors.Join(refusal(), nil, nil) }, reason: []string{"persistent disk b:777/vm-777-disk-1.raw lacks its provenance entry on VM 777", resumes}},
		{name: "journal read failure", cause: journalUnread, reason: []string{"the allocation journal couldn't be read", resumes}},
		{name: "parker lock busy", cause: lockBusy, reason: []string{"a parker protection write", resumes}},
		{name: "parker unprotected", cause: unprotected, reason: []string{"a parker protection write", "read back protected", resumes}},
		{name: "step no readback settles", cause: unsettled, permanent: true, reason: []string{"step attempt-0-step-99 (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned", "which no readback settles", "clear the cause as the error says"}},
		{name: "heal refusal joined with a failed journal close", cause: func(*testing.T) error {
			return errors.Join(refusal(), errors.New("close allocation journal: input/output error"))
		}, reconcile: true},
		{name: "parker unprotected joined with a failed journal close", cause: func(*testing.T) error {
			return protectionPendingPreservation(777, errors.Join(&protectionPendingRefusal{err: gap}, errors.New("close allocation journal: input/output error")))
		}, reconcile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := createdManagedVM(t)
			// A disposal admits the record as observed before it preserves
			// any disk, so the refusal meets an observed record.
			admitted := m.handle.Record()
			admitted.State = aj.Observed
			admitted.Reason = ""
			if err := m.handle.Save(admitted); err != nil {
				t.Fatal(err)
			}
			cause := tc.cause(t)
			err := managedVMCleanupFailure(m.handle, cause)
			state := m.handle.Record().State
			if tc.reconcile {
				requirePermanent(t, err, tc.name)
				if state != aj.ReconciliationRequired {
					t.Fatalf("%s left the VM's allocation %s, want %s", tc.name, state, aj.ReconciliationRequired)
				}
				return
			}
			if err != cause { //nolint:errorlint // The refusal must come back as the very error the preservation returned.
				t.Fatalf("%s came back as %v, want it unchanged", tc.name, err)
			}
			if tc.permanent {
				requirePermanent(t, err, tc.name)
			} else {
				requireRetriable(t, err, tc.name)
			}
			if state != aj.Observed {
				t.Fatalf("%s left the VM's allocation %s, want %s", tc.name, state, aj.Observed)
			}
			reason := m.handle.Record().Reason
			for _, want := range append([]string{"VM cleanup left the VM's persistent disks in place because "}, tc.reason...) {
				if !strings.Contains(reason, want) {
					t.Fatalf("%s left the reason %q, want it to contain %q", tc.name, reason, want)
				}
			}
		})
	}
}

// TestDeleteVMCandidateDefersATransferToTheHealGate covers the ownership
// check delete_vm's preservation makes on each disk it finds on the VM. The
// current resolver never returns a found holder with a transfer intent, so
// the disk is built by hand. A journal-managed disk with a transfer to a
// parker in flight, on a VM that carries no provenance entry for it, gets the
// heal's retriable refusal, which says the transfer is still in flight. The same disk on a VM that
// carries its entry keeps the plain refusal, and so does a disk the journal
// doesn't know.
func TestDeleteVMCandidateDefersATransferToTheHealGate(t *testing.T) {
	t.Parallel()
	volume := "b:777/vm-777-disk-1.raw"
	moving := resolvedDisk{
		diskCID: "pvd-disk", volid: volume, stableID: "bpd-00000000000000ab",
		holder:     &pve.DiskHolder{Found: true, Node: "n1", VMID: 777},
		intent:     &pve.DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "n1", Volid: volume},
		allocation: &managedDiskIdentity{},
	}
	err := managedVMCandidateOwnership(moving, "n1", 777, volume, false)
	requireNotRecorded(t, "the ownership check with a transfer in flight", err)
	requireText(t, err, "the ownership check with a transfer in flight", []string{"parker 90000", "still in flight"})

	const plain = "persistent disk has no unambiguous current ownership proof"
	if err := managedVMCandidateOwnership(moving, "n1", 777, volume, true); err == nil || err.Error() != plain {
		t.Fatalf("the ownership check with the entry recorded = %v, want %q", err, plain)
	}
	legacy := moving
	legacy.allocation = nil
	if err := managedVMCandidateOwnership(legacy, "n1", 777, volume, false); err == nil || err.Error() != plain {
		t.Fatalf("the ownership check on a disk the journal doesn't know = %v, want %q", err, plain)
	}
	settled := moving
	settled.intent = nil
	if err := managedVMCandidateOwnership(settled, "n1", 777, volume, false); err != nil {
		t.Fatalf("the ownership check refused a disk 777 holds with nothing in flight: %v", err)
	}
}
