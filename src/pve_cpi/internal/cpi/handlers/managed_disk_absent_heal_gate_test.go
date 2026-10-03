package handlers

// These rows hold delete_disk's absent-disk heal to what the completion audit
// saw. The heal removes a disk's leftover notes only when the audit found the
// disk absent. The one allowance is an old name, meaning a name the disk had
// before an observed move renamed it. Every VM also keeps its notes while any
// step of the record's active attempt is unsettled, or while any VM holds the
// disk. A read that fails at the heal ends the call retriable.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// TestDeleteDiskStaleNoteSnapshotOnReusedName covers the renamed shape with a
// snapshot of 777 that names the old name, which PVE has since given to the
// hand-attached volume. That name doesn't hold the disk on 777 when the
// snapshot has it on a bus slot, so the retry heals and ends Deleted. Two other
// snapshots still hold the disk. One has a drive that carries the disk's
// serial, and the other has the old name on an unused entry, as 777's own
// config can. Those retries write nothing and refuse.
func TestDeleteDiskStaleNoteSnapshotOnReusedName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		drive  func(s *staleNote) string
		healed bool
	}{
		{name: "the reused old name", key: "scsi3", drive: func(s *staleNote) string { return s.f.volume + ",size=1G" }, healed: true},
		{name: "a drive with the disk's serial", key: "scsi3", drive: func(s *staleNote) string {
			return s.f.volume + ",serial=" + s.key + ",size=1G"
		}},
		{name: "the reused old name on an unused entry", key: "unused0", drive: func(s *staleNote) string { return s.f.volume }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStaleNote(t, false, false)
			s.f.client.vmSnapshots = map[int][]map[string]any{777: {{"name": "before-upgrade"}}}
			s.f.client.snapshotConfigs = map[int]map[string]map[string]any{777: {"before-upgrade": {tc.key: tc.drive(s)}}}
			err := s.deleteDisk(t)
			if tc.healed {
				if err != nil {
					t.Fatalf("retried delete_disk with a snapshot naming the reused old name: %v", err)
				}
				s.requireDeleted(t)
				if s.writes != 1 {
					t.Fatalf("the retry wrote 777 %d times, want once", s.writes)
				}
				return
			}
			if err == nil {
				t.Fatalf("retried delete_disk with %s in a snapshot succeeded, want a refusal", tc.name)
			}
			s.requireState(t, aj.ReconciliationRequired)
			if s.writes != 0 {
				t.Fatalf("777 was written %d times, want none", s.writes)
			}
		})
	}
}

// TestDeleteDiskReattachedHolderElsewhereKeepsLeftovers is 999 holding a
// volume under the name the attach gave the disk on 888, which the park
// renamed it off, either on a slot that 999's own entry for the disk names or
// as a drive that carries the disk's serial. 999 isn't a VM the disk was
// renamed off, so the audit saw the disk held. The retry writes nothing to any
// VM, refuses, and leaves 777's leftovers and 999's config and volume as they
// were.
func TestDeleteDiskReattachedHolderElsewhereKeepsLeftovers(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold func(t *testing.T, r *reattached, volume string)
	}{
		{name: "slot its own entry names", hold: func(t *testing.T, r *reattached, volume string) {
			r.s.client.state.configs[999]["scsi1"] = volume + ",size=1G"
			entry := r.s.sourceEntry(t, r.id)
			entry.Volid = volume
			if err := pve.WriteDiskAllocationProvenance(context.Background(), r.s.client, "n1", 999, r.s.token, entry); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "drive with the disk's serial", hold: func(_ *testing.T, r *reattached, volume string) {
			r.s.client.state.configs[999]["scsi1"] = volume + ",serial=" + r.s.token + ",size=1G"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildReattached(t, true)
			held := r.renamedOff888(t)
			r.s.client.state.volumes[held] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
			r.s.client.state.configs[999] = map[string]any{"name": "w999", "digest": "1"}
			tc.hold(t, r, held)
			before := fmt.Sprint(r.s.client.state.configs[999])
			if err := r.deleteDisk(t, context.Background()); err == nil {
				t.Fatal("retried delete_disk while 999 holds the disk succeeded, want a refusal")
			}
			r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
			r.requireUntouched(t)
			if after := fmt.Sprint(r.s.client.state.configs[999]); after != before {
				t.Fatalf("999's config went from %s to %s, want it left as it was", before, after)
			}
			if r.s.client.state.volumes[held] == nil {
				t.Fatalf("delete_disk destroyed 999's volume %s", held)
			}
		})
	}
}

// renamedOff888 returns the name the attach gave the disk on 888, which the
// park renamed it off.
func (r *reattached) renamedOff888(t *testing.T) string {
	t.Helper()
	record, err := r.s.journal.Inspect(r.id)
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State == aj.Observed && step.Target.VMID == 888 && strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") && len(step.VolIDs) > 1 {
			name = step.VolIDs[0]
		}
	}
	if name == "" || name == r.volume {
		t.Fatalf("the record has no observed move off 888 under its own name: %+v", record.Steps)
	}
	return name
}

// TestAbsentDiskLeftoverVMsListingNode covers a volume listed under a name that
// the disk had on n1 before an observed move renamed it, while 777's leftover
// entry still names the volume. On n1 the listing is the same volume, so the
// heal has to decide what to do with 777's notes. On n2's local storage the
// listing is another volume, which the audit counted as the disk's, so no VM
// is a candidate.
func TestAbsentDiskLeftoverVMsListingNode(t *testing.T) {
	const old, landed = "a:777/vm-777-disk-0.raw", "a:999/vm-999-disk-0.raw"
	record := aj.Record{ID: "0b5f3c2e-4d6a-4f1b-9c8d-7e6f5a4b3c2d", Kind: allocationKindDisk, Steps: []aj.Step{{
		ID:     "move",
		Kind:   "lifecycle_detach_disk_Nodes_CreateQemuMoveDisk",
		Target: aj.Target{Node: "n1", Storage: "a", VMID: 777, IntendedVolume: old},
		State:  aj.Observed,
		VolIDs: []string{old, landed},
	}}}
	for _, tc := range []struct {
		node string
		want []int
	}{
		{node: "n1", want: []int{777}},
		{node: "n2"},
	} {
		t.Run(tc.node, func(t *testing.T) {
			report := StorageAllocationAudit{Complete: true, VMScanComplete: true, Evidence: []StorageAllocationEvidence{
				{AllocationID: record.ID, Kind: allocationKindDisk, Node: "n1", VMID: 777, VolumeID: old},
				{AllocationID: record.ID, Kind: allocationKindDisk, Node: tc.node, VolumeID: old},
			}}
			vmids, _ := absentDiskLeftoverVMs(report, record, absentDiskRenamedSources(record))
			if fmt.Sprint(vmids) != fmt.Sprint(tc.want) {
				t.Fatalf("leftover VMs with the listing on %s = %v, want %v", tc.node, vmids, tc.want)
			}
		})
	}
}

// TestDeleteDiskReattachedListedNameNoEntryNames covers a volume listed under
// the name the attach gave the disk on 888, before the park renamed the disk
// again. 888 carries no leftover entry that names that name, so the listing
// counts as the disk's as far as the audit can tell. The retry writes nothing
// to 777 and refuses.
func TestDeleteDiskReattachedListedNameNoEntryNames(t *testing.T) {
	r := buildReattached(t, true)
	listed := r.renamedOff888(t)
	r.s.client.state.volumes[listed] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	if err := r.deleteDisk(t, context.Background()); err == nil {
		t.Fatalf("retried delete_disk with %s listed and no entry naming it succeeded, want a refusal", listed)
	}
	if record, _ := r.s.journal.Inspect(r.id); record.State == aj.Deleted {
		t.Fatal("the record ended Deleted with a volume under one of its names listed")
	}
	r.requireUntouched(t)
}

// TestRemoveAbsentDiskNotesActiveStepUnsettled covers a record whose active
// attempt still has a step with no outcome when the heal runs, next to a
// closed attempt's create step that stays planned for good. No handler path
// gets here, because delete_disk refuses an unsettled step of the active
// attempt before it deletes the volume, so the row calls the heal directly
// with a record built by hand. The heal writes nothing to 777 and names the
// active attempt's step rather than the closed one. Without that step, the
// same call removes 777's leftovers in one write, so the step is what stopped
// it. One subtest puts the unsettled step after the attempt's observed steps,
// and the other puts it before them, so a guard that looks only at the last
// step, or stops at the first observed step it finds from the end, fails the
// second.
func TestRemoveAbsentDiskNotesActiveStepUnsettled(t *testing.T) {
	for _, tc := range []struct {
		name         string
		at           func(steps []aj.Step) int
		lastObserved bool
	}{
		{name: "the unsettled step is the last step", at: func(steps []aj.Step) int { return len(steps) }},
		{name: "observed steps follow the unsettled step", at: func([]aj.Step) int { return 1 }, lastObserved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildReattached(t, true)
			ctx := context.Background()
			bare, meta, err := decodeDiskCID(ctx, r.s.deps, "test", r.s.cid)
			if err != nil {
				t.Fatal(err)
			}
			rd, err := resolveDiskForOp(ctx, r.s.deps, "test", r.s.cid, bare, meta)
			if err != nil {
				t.Fatal(err)
			}
			record, err := r.s.journal.Inspect(r.id)
			if err != nil {
				t.Fatal(err)
			}
			report, err := AuditStorageAllocations(ctx, r.s.deps, r.s.journal, []string{"n1"})
			if err != nil {
				t.Fatal(err)
			}
			closed, abandoned := withRejectedFirstAttempt(t, r.id, record)
			interrupted := aj.Step{}
			for i := range closed.Steps {
				if step := closed.Steps[i]; step.State == aj.Observed && strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") {
					interrupted = aj.Step{ID: "interrupted-attach", Kind: step.Kind, Attempt: closed.ActiveAttempt(), Target: step.Target, State: aj.Planned}
				}
			}
			if interrupted.ID == "" {
				t.Fatalf("the record has no observed move to copy: %+v", closed.Steps)
			}
			open := closed
			open.Steps = slices.Insert(slices.Clone(closed.Steps), tc.at(closed.Steps), interrupted)
			if last := open.Steps[len(open.Steps)-1]; (last.State == aj.Observed) != tc.lastObserved {
				t.Fatalf("the last step is %+v, but the subtest wants its observed state to be %t", last, tc.lastObserved)
			}
			removed, step, err := removeAbsentDiskNotes(ctx, r.s.deps, rd, open, report)
			if err != nil || removed || step == nil || step.ID != interrupted.ID {
				t.Fatalf("heal with step %s of the active attempt unsettled = removed %t, step %+v, %v; want nothing removed and step %s named rather than %s",
					interrupted.ID, removed, step, err, interrupted.ID, abandoned.ID)
			}
			r.requireUntouched(t)
			removed, step, err = removeAbsentDiskNotes(ctx, r.s.deps, rd, closed, report)
			if err != nil || !removed || step != nil {
				t.Fatalf("heal with only the closed attempt's step planned = removed %t, step %+v, %v; want 777's leftovers removed", removed, step, err)
			}
			if r.writes != 1 {
				t.Fatalf("777 was written %d times, want once", r.writes)
			}
		})
	}
}

// TestDeleteDiskReattachedSnapshotOnOneLeftoverKeepsAll covers two VMs that
// carry the disk's leftovers, 777 and 778, where only 778 has a snapshot that
// names the disk. The audit doesn't read snapshots, so the heal finds that
// hold only when it reads 778. It reads every VM before it writes to any, so
// 777 keeps its notes as well, and the delete refuses with nothing written.
// Once 778's snapshot is gone, the retry clears both VMs and ends Deleted.
func TestDeleteDiskReattachedSnapshotOnOneLeftoverKeepsAll(t *testing.T) {
	r := buildReattached(t, true)
	ctx := context.Background()
	r.s.seedSourceEntry(t, 778, r.id)
	r.s.client.vmSnapshots = map[int][]map[string]any{778: {{"name": "before-upgrade"}}}
	r.s.client.snapshotConfigs = map[int]map[string]map[string]any{778: {"before-upgrade": {"scsi4": r.volume + ",size=1G"}}}
	r.capture(t)
	before778 := pve.DescriptionFromConfig(r.s.client.state.configs[778])
	writes778 := 0
	r.s.client.afterConfigWrite = func(vmid int) {
		switch vmid {
		case 777:
			r.writes++
		case 778:
			writes778++
		}
	}
	if err := r.deleteDisk(t, ctx); err == nil {
		t.Fatal("retried delete_disk while a snapshot of 778 names the disk succeeded, want a refusal")
	}
	r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
	r.requireUntouched(t)
	if after := pve.DescriptionFromConfig(r.s.client.state.configs[778]); writes778 != 0 || after != before778 {
		t.Fatalf("778 was written %d times and its description went from %s to %s, want it left as it was", writes778, before778, after)
	}
	r.s.client.vmSnapshots, r.s.client.snapshotConfigs = nil, nil
	if err := r.deleteDisk(t, ctx); err != nil {
		t.Fatalf("retried delete_disk once 778's snapshot is gone: %v", err)
	}
	r.requireDeleted(t)
	if r.writes != 1 || writes778 != 1 || r.s.hasEntry(778) {
		t.Fatalf("the retry wrote 777 %d times and 778 %d times, and 778 still carries the entry: %t; want one write each and no entry",
			r.writes, writes778, r.s.hasEntry(778))
	}
}

// TestAbsentDiskNoteHolderEveryClaim covers a VM that carries the disk's old
// name in more than one claim on its node, such as one in its applied config
// and one in a pending change. The VM stops counting as a holder only when every
// one of those claims has no serial, isn't a CD-ROM, and is attributed to the
// record by the VM's own entry. It doesn't stop counting when no claim sits on
// the holder's own node and VM, because then nothing attributes the name to
// the record.
func TestAbsentDiskNoteHolderEveryClaim(t *testing.T) {
	const old, landed = "a:777/vm-777-disk-0.raw", "a:999/vm-999-disk-0.raw"
	record := aj.Record{ID: "0b5f3c2e-4d6a-4f1b-9c8d-7e6f5a4b3c2d", Kind: allocationKindDisk, Steps: []aj.Step{{
		ID:     "move",
		Kind:   "lifecycle_detach_disk_Nodes_CreateQemuMoveDisk",
		Target: aj.Target{Node: "n1", Storage: "a", VMID: 777, IntendedVolume: old},
		State:  aj.Observed,
		VolIDs: []string{old, landed},
	}}}
	holder := StorageAllocationEvidence{AllocationID: record.ID, Kind: allocationKindDisk, Node: "n1", VMID: 777, VolumeID: old}
	plain := storageAuditClaim{node: "n1", vmid: 777, allocations: []string{record.ID}}
	for _, tc := range []struct {
		name   string
		claims []storageAuditClaim
		want   bool
	}{
		{name: "a second claim with no serial", claims: []storageAuditClaim{plain, plain}, want: true},
		{name: "a second claim with a serial", claims: []storageAuditClaim{plain, {node: "n1", vmid: 777, serial: "other-disk", allocations: []string{record.ID}}}},
		{name: "a second claim on a CD-ROM", claims: []storageAuditClaim{plain, {node: "n1", vmid: 777, cdrom: true, allocations: []string{record.ID}}}},
		{name: "a second claim the entry doesn't attribute", claims: []storageAuditClaim{plain, {node: "n1", vmid: 777}}},
		{name: "claims only on another node and another VM", claims: []storageAuditClaim{
			{node: "n2", vmid: 777, allocations: []string{record.ID}},
			{node: "n1", vmid: 778, allocations: []string{record.ID}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := StorageAllocationAudit{claims: map[string][]storageAuditClaim{old: tc.claims}}
			if got := absentDiskNoteHolder(report, record, holder, absentDiskRenamedSources(record)[777]); got != tc.want {
				t.Fatalf("absentDiskNoteHolder with %s = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}

// TestAbsentDiskLeftoverVMsConflictKeepsAll covers a report that carries a
// conflict. The audit counts such a report incomplete, so the heal never sees
// one marked complete, and the row builds it by hand to show that the conflict
// alone keeps every VM out of the heal. The same report with no conflict names
// VM 777.
func TestAbsentDiskLeftoverVMsConflictKeepsAll(t *testing.T) {
	record := aj.Record{ID: "6c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f", Kind: allocationKindDisk}
	report := StorageAllocationAudit{Complete: true, VMScanComplete: true, Evidence: []StorageAllocationEvidence{{
		AllocationID: record.ID, Kind: allocationKindDisk, Node: "n1", VMID: 777, VolumeID: "a:777/vm-777-disk-0.raw",
	}}}
	if vmids, _ := absentDiskLeftoverVMs(report, record, nil); !slices.Equal(vmids, []int{777}) {
		t.Fatalf("absentDiskLeftoverVMs with no conflict = %v, want [777]", vmids)
	}
	report.Conflicts = []string{"volume a:777/vm-777-disk-0.raw is referenced by more than one VM"}
	if vmids, nodes := absentDiskLeftoverVMs(report, record, nil); len(vmids) != 0 || len(nodes) != 0 {
		t.Fatalf("absentDiskLeftoverVMs with a conflict = %v %v, want none", vmids, nodes)
	}
}

// TestDeleteDiskStaleNoteOtherDiskOnSameVM covers another journaled disk on 777
// under the renamed disk's old name. That disk holds the slot with its own
// serial and carries its own entry there. The retry removes the renamed disk's
// stale notes, keeps the other disk's entry, its slot, and its volume, and ends
// Deleted.
func TestDeleteDiskStaleNoteOtherDiskOnSameVM(t *testing.T) {
	s := newStaleNote(t, false, true)
	ctx := context.Background()
	other, entry := otherDiskOnReusedName(t, s)
	s.f.client.state.configs[777]["scsi3"] = s.f.volume + ",serial=" + other.token + ",size=5G"
	if err := pve.WriteDiskAllocationProvenance(ctx, s.f.client, "n1", 777, other.token, entry); err != nil {
		t.Fatal(err)
	}
	cfg := s.f.client.state.configs[777]
	s.before, s.rest = descriptionNotes(t, cfg), configWithoutNotes(cfg)
	if _, found := s.before["bosh_disk_allocations/"+other.token]; !found {
		t.Fatalf("777 carries %v, want the other disk's entry", s.before)
	}
	s.writes = 0
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("retried delete_disk with another disk on the old name on 777: %v", err)
	}
	s.requireDeleted(t)
	if s.writes != 1 {
		t.Fatalf("the retry wrote 777 %d times, want once", s.writes)
	}
}

// TestDeleteDiskReattachedSourceVanishes covers a retry whose read of 777 finds
// 777 gone. That is the read the removal would build its write from. The audit
// that chose 777 is out of date, so the call fails retriable and writes
// nothing, and a rerun audits again and ends Deleted.
func TestDeleteDiskReattachedSourceVanishes(t *testing.T) {
	recording := buildReattached(t, true)
	at := healReadAt(t, recording.s.client, 777, nil, func() error { return recording.deleteDisk(t, context.Background()) })

	r := buildReattached(t, true)
	onReadAt(r.s.client, 777, at, nil, func() error {
		delete(r.s.client.state.configs, 777)
		return sdkerrors.ParseAPIError(500, []byte(`{"message":"Configuration file 'nodes/n1/qemu-server/777.conf' does not exist"}`))
	})
	err := r.deleteDisk(t, context.Background())
	requireRetriable(t, err, "delete_disk whose read of 777 found it gone")
	r.s.client.onConfigRead = nil
	r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
	if r.writes != 0 {
		t.Fatalf("777 was written %d times, want none", r.writes)
	}
	if err := r.deleteDisk(t, context.Background()); err != nil {
		t.Fatalf("rerun of delete_disk after 777 went: %v", err)
	}
	r.s.requireRecord(t, r.id, aj.Deleted)
}

// TestDeleteDiskReattachedMalformedView covers a retry whose read of 777 comes
// back with a pending row that isn't an object. That is the read the removal
// would build its write from. The call fails retriable, as a failed read does,
// and writes nothing, and a rerun ends Deleted.
func TestDeleteDiskReattachedMalformedView(t *testing.T) {
	recording := buildReattached(t, true)
	at := healReadAt(t, recording.s.client, 777, nil, func() error { return recording.deleteDisk(t, context.Background()) })

	r := buildReattached(t, true)
	onReadAt(r.s.client, 777, at, nil, func() error {
		r.w.malformNext = true
		return nil
	})
	err := r.deleteDisk(t, context.Background())
	requireRetriable(t, err, "delete_disk whose read of 777 came back malformed")
	r.s.client.onConfigRead = nil
	if r.w.malformNext {
		t.Fatal("the malformed view was never served")
	}
	r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
	r.requireUntouched(t)
	if err := r.deleteDisk(t, context.Background()); err != nil {
		t.Fatalf("rerun of delete_disk after the malformed view: %v", err)
	}
	r.requireDeleted(t)
}

// TestDeleteDiskOtherHolderPendingAddKeepsEntry covers a 778 that names the
// volume only as a pending add on a slot that its current config doesn't have.
// Applying the pending change puts the volume on 778, so 778 still holds it.
// The delete refuses, and 778's entry stays as it was.
func TestDeleteDiskOtherHolderPendingAddKeepsEntry(t *testing.T) {
	o := buildOtherHolder(t)
	o.s.client.pending = newFakePendingModel()
	o.s.client.pending.replaced = map[int]map[string]any{778: {"scsi4": nil}}
	o.s.client.state.configs[778]["scsi4"] = o.s.stranded + ",size=1G"
	o.capture()
	views, err := pve.ReadQemuViews(context.Background(), o.s.client, "n1", 778)
	if err != nil {
		t.Fatal(err)
	}
	if _, current := views.Current()["scsi4"]; current || len(views.SlotsNaming(o.s.stranded)) == 0 {
		t.Fatalf("778's views = current %v, holding %v, want scsi4 only as a pending add", views.Current(), views.Holding())
	}
	o.requireKept(t, deleteDiskAt(t, context.Background(), o.s.deps, o.s.cid))
}
