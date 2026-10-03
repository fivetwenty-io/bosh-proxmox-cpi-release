package handlers

// A disk that an earlier release parked, attached to another VM, and parked
// again can leave its allocation entry, attached-disk note, and overlay on the
// first VM, 777, with nothing left that points at 777. delete_disk deletes the
// volume and then refuses at its completion audit, which counts 777's entry as
// the disk's. delete_disk now removes this disk's own notes from each VM that
// holds the volume nowhere, once the audit finds the VM carrying them. It does
// so in one write pinned to the digest of its read, before the audit decides
// the disk's fate. These rows run that shape through the real handlers on the
// flow fake.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// reattached is a disk that was parked, attached to 888, and parked again,
// after an earlier release left its notes on 777. The parker's record names
// 888, so nothing that points at 777 is left.
type reattached struct {
	s  *strandedDisk
	id string
	w  *tailWrites
	// volume is the name the disk has on the parker. onEight is the name the
	// disk had on 888, which 777 never knew it under, so no observed move took
	// the disk off 777 under that name.
	volume, onEight string
	// before is 777's notes once the leftovers are in place, and rest is
	// everything else in 777's configuration at that point.
	before map[string]string
	rest   string
	// writes counts the configuration writes to 777.
	writes int
}

// buildReattached builds a reattached disk. stuck runs a first delete_disk
// whose completion audit loses its visibility once the volume is deleted,
// which is how the record comes to sit in reconciliation_required with its
// volume gone and 777's leftovers in place.
func buildReattached(t *testing.T, stuck bool) *reattached {
	t.Helper()
	captureParkerPoolSweep(t)
	s, id, w := buildTailDisk(t)
	ctx := context.Background()
	if err := detachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("park the disk off 777: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	if err := attachDiskAt(t, ctx, s.deps, "888", s.cid); err != nil {
		t.Fatalf("attach the parked disk to 888: %v", err)
	}
	var onEight string
	for _, held := range s.serialHolders() {
		onEight = held
	}
	if onEight == "" || onEight == s.stranded {
		t.Fatalf("888 holds the disk as %q, want a name other than the birth name %s", onEight, s.stranded)
	}
	// An earlier release left the entry, the attached-disk note, and the
	// overlay on 777 when it parked the disk, and the attach to 888 ran under
	// that release too. The attach under this release would have removed them,
	// so they go in once it is done.
	s.seedSourceEntry(t, 777, id)
	pve.UpdateAttachedDiskCID(ctx, s.client, nil, "n1", 777, s.token, s.cid)
	if err := pve.SetVMDiskOptOverlay(ctx, s.client, "n1", 777, s.token, map[string]string{"cache": "none"}); err != nil {
		t.Fatal(err)
	}
	if err := detachDiskAt(t, ctx, s.deps, "888", s.cid); err != nil {
		t.Fatalf("park the disk off 888: %v", err)
	}
	// The attach and the park each moved the disk under a new name, so the
	// parker holds it under neither the name 777 knew nor the name 888 knew.
	parker := s.requireSingleHolder(t, true)
	var volume string
	for _, held := range s.serialHolders() {
		volume = held
	}
	if volume == s.stranded {
		t.Fatalf("the parker holds the disk as %s, want the name the park gave it", volume)
	}
	if source, found, err := pve.ReadParkerSourceVMID(ctx, s.client, "n1", parker, volume, s.token); err != nil || found && source == 777 {
		t.Fatalf("the parker's record names source %d (found %t, %v), want nothing pointing at 777", source, found, err)
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
	r := &reattached{s: s, id: id, w: w, volume: volume, onEight: onEight}
	if stuck {
		s.client.visibilityErrAfterDelete = errors.New("visibility lost")
		if err := deleteDiskAt(t, ctx, s.deps, s.cid); err == nil {
			t.Fatal("delete_disk without audit visibility after the deletion succeeded, want a refusal")
		}
		s.client.visibilityErrAfterDelete, s.client.visibilityErr = nil, nil
		if s.client.state.volumes[volume] != nil {
			t.Fatalf("the refused delete_disk left %s", volume)
		}
		s.requireRecord(t, id, aj.ReconciliationRequired)
	}
	r.capture(t)
	for _, note := range []string{"bosh_disk_allocations/" + s.token, "bosh_attached_disks/" + s.token, pve.DiskOptOverlaysSentinelKey + "/" + s.token} {
		if _, found := r.before[note]; !found {
			t.Fatalf("777 carries %v, want the leftover %s", r.before, note)
		}
	}
	s.client.afterConfigWrite = func(vmid int) {
		if vmid == 777 {
			r.writes++
		}
	}
	return r
}

// capture records 777's notes and the rest of its configuration as they are
// now.
func (r *reattached) capture(t *testing.T) {
	t.Helper()
	cfg := r.s.client.state.configs[777]
	r.before, r.rest = descriptionNotes(t, cfg), configWithoutNotes(cfg)
}

func (r *reattached) deleteDisk(t *testing.T, ctx context.Context) error {
	t.Helper()
	return deleteDiskAt(t, ctx, r.s.deps, r.s.cid)
}

// requireDeleted checks that the record ended Deleted, that 777 lost exactly
// the disk's allocation entry, attached-disk note, and overlay, and that
// every other note and the rest of 777's configuration stayed as they were.
func (r *reattached) requireDeleted(t *testing.T) {
	t.Helper()
	r.s.requireRecord(t, r.id, aj.Deleted)
	r.s.requireSourceClean(t)
	cfg := r.s.client.state.configs[777]
	notes := descriptionNotes(t, cfg)
	gone := map[string]bool{
		"bosh_disk_allocations/" + r.s.token:             true,
		"bosh_attached_disks/" + r.s.token:               true,
		pve.DiskOptOverlaysSentinelKey + "/" + r.s.token: true,
	}
	for note, was := range r.before {
		if now, found := notes[note]; !gone[note] && now != was {
			t.Fatalf("777's %s = %q (found %t), want %s left as it was", note, now, found, was)
		}
	}
	if len(notes) != len(r.before)-len(gone) {
		t.Fatalf("777's notes went from %v to %v, want only %v gone", r.before, notes, gone)
	}
	if rest := configWithoutNotes(cfg); rest != r.rest {
		t.Fatalf("777's config changed beyond its notes:\nbefore %s\nafter  %s", r.rest, rest)
	}
}

// requireUntouched checks that 777's notes and configuration are as they were
// and that nothing wrote to 777.
func (r *reattached) requireUntouched(t *testing.T) {
	t.Helper()
	cfg := r.s.client.state.configs[777]
	if notes := descriptionNotes(t, cfg); fmt.Sprint(notes) != fmt.Sprint(r.before) {
		t.Fatalf("777's notes went from %v to %v, want them left as they were", r.before, notes)
	}
	if rest := configWithoutNotes(cfg); rest != r.rest {
		t.Fatalf("777's config changed:\nbefore %s\nafter  %s", r.rest, rest)
	}
	if r.writes != 0 {
		t.Fatalf("777 was written %d times, want none", r.writes)
	}
}

// TestDeleteDiskReattachedLeftoversHealOnRetry covers a record stuck the way an
// earlier release leaves it, with no parker record pointing at 777. The
// Director's retry removes 777's leftovers in one write and ends Deleted.
func TestDeleteDiskReattachedLeftoversHealOnRetry(t *testing.T) {
	r := buildReattached(t, true)
	if err := r.deleteDisk(t, context.Background()); err != nil {
		t.Fatalf("retried delete_disk with the disk's leftovers on 777: %v", err)
	}
	r.requireDeleted(t)
	if r.writes != 1 {
		t.Fatalf("the retry wrote 777 %d times, want once", r.writes)
	}
}

// TestDeleteDiskReattachedLeftoversHealOnFirstPass covers the park off 888
// followed by delete_disk. The first call deletes the volume, removes 777's
// leftovers, and ends Deleted.
func TestDeleteDiskReattachedLeftoversHealOnFirstPass(t *testing.T) {
	r := buildReattached(t, false)
	if err := r.deleteDisk(t, context.Background()); err != nil {
		t.Fatalf("delete_disk with the disk's leftovers on 777: %v", err)
	}
	if r.s.client.state.volumes[r.volume] != nil {
		t.Fatalf("%s survived delete_disk", r.volume)
	}
	r.requireDeleted(t)
	if r.writes != 1 {
		t.Fatalf("delete_disk wrote 777 %d times, want once", r.writes)
	}
}

// TestDeleteDiskReattachedHolderKeepsLeftovers covers a 777 that still holds
// the disk, whether on an unused entry, on a bus slot, or as a drive that
// carries the disk's serial. The bus slot names the volume under the name the
// disk had on 888, which isn't its birth name, so the delete reaches the heal
// and the heal's own hold check refuses it. A slot under the birth name would
// stop at the identity check before the heal ran. The retry writes nothing to
// 777, refuses with the completion audit's text, and leaves the record in
// reconciliation_required.
func TestDeleteDiskReattachedHolderKeepsLeftovers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal string
		hold    func(r *reattached) (key, value string)
	}{
		{name: "unused entry", refusal: "still names its volume", hold: func(r *reattached) (string, string) { return "unused1", r.s.stranded }},
		{name: "bus slot", refusal: "artifact or provenance remains; reconciliation required", hold: func(r *reattached) (string, string) { return "scsi4", r.onEight + ",size=1G" }},
		{name: "drive with the disk's serial", refusal: "conflicts with journal; audit required", hold: func(r *reattached) (string, string) {
			storage, _, err := pve.ParseDiskCID(r.s.stranded)
			if err != nil {
				t.Fatal(err)
			}
			return "scsi5", storage + ":777/vm-777-disk-5.raw,serial=" + r.s.token + ",size=1G"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildReattached(t, true)
			key, value := tc.hold(r)
			r.s.client.state.configs[777][key] = value
			r.capture(t)
			err := r.deleteDisk(t, context.Background())
			if err == nil {
				t.Fatal("retried delete_disk while 777 holds the disk succeeded, want a refusal")
			}
			if _, birth := pve.IsDiskBirthNameHeld(err); birth {
				t.Fatalf("delete_disk stopped at the birth-name check, so it never reached the heal: %v", err)
			}
			if !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("delete_disk's refusal is %q, want it to contain %q", err, tc.refusal)
			}
			r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
			r.requireUntouched(t)
		})
	}
}

// TestDeleteDiskReattachedKeepsOtherDisksNotes adds two things to 777. One is
// another disk's notes, filed under that disk's volid. The other is an
// attached-disk note that maps this disk's CID to a volume 777 holds on a slot.
// The retry removes only this disk's own notes, and every other note comes
// through unchanged.
func TestDeleteDiskReattachedKeepsOtherDisksNotes(t *testing.T) {
	r := buildReattached(t, true)
	ctx := context.Background()
	storage, _, err := pve.ParseDiskCID(r.s.stranded)
	if err != nil {
		t.Fatal(err)
	}
	otherVolume := storage + ":777/vm-777-disk-6.raw"
	heldVolume := storage + ":777/vm-777-disk-7.raw"
	for _, volume := range []string{otherVolume, heldVolume} {
		r.s.client.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	}
	r.s.client.state.configs[777]["scsi6"] = otherVolume + ",size=1G"
	r.s.client.state.configs[777]["scsi7"] = heldVolume + ",size=1G"
	pve.UpdateAttachedDiskCID(ctx, r.s.client, nil, "n1", 777, otherVolume, "other-disk-cid")
	if err := pve.SetVMDiskOptOverlay(ctx, r.s.client, "n1", 777, otherVolume, map[string]string{"cache": "writeback"}); err != nil {
		t.Fatal(err)
	}
	pve.UpdateAttachedDiskCID(ctx, r.s.client, nil, "n1", 777, heldVolume, r.s.cid)
	r.capture(t)
	for _, note := range []string{"bosh_attached_disks/" + otherVolume, pve.DiskOptOverlaysSentinelKey + "/" + otherVolume, "bosh_attached_disks/" + heldVolume} {
		if _, found := r.before[note]; !found {
			t.Fatalf("777 carries %v, want %s", r.before, note)
		}
	}
	if err := r.deleteDisk(t, ctx); err != nil {
		t.Fatalf("retried delete_disk with another disk's notes on 777: %v", err)
	}
	r.requireDeleted(t)
	for _, volume := range []string{otherVolume, heldVolume} {
		if r.s.client.state.volumes[volume] == nil {
			t.Fatalf("delete_disk destroyed %s", volume)
		}
	}
}

// healReadAt runs retry once with no fault and returns a count of vmid's
// config reads. The count stops at the last read before retry's first write to
// vmid, which is the read the removal builds its write from. It counts only
// the reads made while counted reports true, and a nil counted counts them
// all. Some lookups read a VM a varying number of times before the volume is
// deleted, so a call that deletes the volume counts only the reads after that.
func healReadAt(t *testing.T, client *lifecycleFlowPVE, vmid int, counted func() bool, retry func() error) int {
	t.Helper()
	reads, at := 0, 0
	client.onConfigRead = func(read int) error {
		if read == vmid && (counted == nil || counted()) {
			reads++
		}
		return nil
	}
	client.beforeConfigWrite = func(written int) {
		if written == vmid && at == 0 {
			at = reads
		}
	}
	if err := retry(); err != nil {
		t.Fatalf("the recording retry: %v", err)
	}
	client.onConfigRead, client.beforeConfigWrite = nil, nil
	if at == 0 {
		t.Fatalf("the recording retry wrote nothing to %d", vmid)
	}
	return at
}

// onReadAt runs hook on the at-th counted config read of vmid from now on,
// counting the way healReadAt does, and makes that read fail with the error
// hook returns.
func onReadAt(client *lifecycleFlowPVE, vmid, at int, counted func() bool, hook func() error) {
	reads := 0
	client.onConfigRead = func(read int) error {
		if read != vmid || counted != nil && !counted() {
			return nil
		}
		reads++
		if reads == at {
			return hook()
		}
		return nil
	}
}

// afterDelete returns a function that reports true once client has deleted a
// volume since afterDelete was called, so only reads after the deletion count.
func afterDelete(client *lifecycleFlowPVE) func() bool {
	base := client.deletes
	return func() bool { return client.deletes > base }
}

// failReadAt makes 777's at-th config read from now on fail with err.
func failReadAt(client *lifecycleFlowPVE, at int, err error) {
	onReadAt(client, 777, at, nil, func() error { return err })
}

// TestDeleteDiskReattachedSourceReadFails covers a retry whose read of 777
// fails. That is the read the removal would build its write from. The call
// fails retriable, writes nothing to 777, and leaves the record in
// reconciliation_required, and a rerun ends Deleted.
func TestDeleteDiskReattachedSourceReadFails(t *testing.T) {
	recording := buildReattached(t, true)
	at := healReadAt(t, recording.s.client, 777, nil, func() error { return recording.deleteDisk(t, context.Background()) })

	r := buildReattached(t, true)
	failReadAt(r.s.client, at, errors.New("connection reset reading 777"))
	err := r.deleteDisk(t, context.Background())
	requireRetriable(t, err, "delete_disk whose read of 777 failed")
	r.s.client.onConfigRead = nil
	r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
	r.requireUntouched(t)
	if err := r.deleteDisk(t, context.Background()); err != nil {
		t.Fatalf("rerun of delete_disk after the failed read: %v", err)
	}
	r.requireDeleted(t)
}

// TestDeleteDiskStaleNoteNotFoundWhileVMExists covers the renamed shape where
// the read of 777 answers 404 while 777 still exists, which a read can do when
// it races a migration. A 404 leaves the audit that chose 777 out of date, as
// any failed read does, so the call fails retriable and writes nothing, and a
// rerun ends Deleted.
func TestDeleteDiskStaleNoteNotFoundWhileVMExists(t *testing.T) {
	recording := newStaleNote(t, false, false)
	at := healReadAt(t, recording.f.client, 777, nil, func() error { return recording.deleteDisk(t) })

	s := newStaleNote(t, false, false)
	failReadAt(s.f.client, at, sdkerrors.ParseAPIError(404, []byte(`{"message":"VM not found"}`)))
	err := s.deleteDisk(t)
	requireRetriable(t, err, "delete_disk whose read of 777 answered 404 while 777 exists")
	s.f.client.onConfigRead = nil
	s.requireState(t, aj.ReconciliationRequired)
	if s.writes != 0 {
		t.Fatalf("777 was written %d times, want none", s.writes)
	}
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("rerun of delete_disk after the 404: %v", err)
	}
	s.requireDeleted(t)
}

// TestDeleteDiskStaleNoteMigratedVM covers the renamed shape with 777 migrated
// to n2 after its notes were written, so the entry still names n1. The fake
// answers 404 for 777 on n1, so the retry ends Deleted only when it reads 777
// on n2 and reads its write back there.
func TestDeleteDiskStaleNoteMigratedVM(t *testing.T) {
	s := newStaleNote(t, false, false)
	if s.f.client.vmNodes == nil {
		s.f.client.vmNodes = map[int]string{}
	}
	s.f.client.vmNodes[777] = "n2"
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("retried delete_disk with 777 on n2: %v", err)
	}
	s.requireDeleted(t)
	if s.writes != 1 {
		t.Fatalf("the retry wrote 777 %d times, want once", s.writes)
	}
}

// TestDeleteDiskReattachedForeignEntryStays covers an entry on 777 under the
// disk's stable ID whose backing or allocation ID differs from the record's
// own. The entry isn't provably this disk's, so the retry writes nothing to 777
// and leaves the entry exactly as it was.
func TestDeleteDiskReattachedForeignEntryStays(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(entry *pve.DiskAllocationProvenance)
	}{
		{name: "backing", change: func(entry *pve.DiskAllocationProvenance) { entry.Backing = "dir:/mnt/elsewhere" }},
		{name: "allocation ID", change: func(entry *pve.DiskAllocationProvenance) {
			entry.AllocationID = "0b5f3c2e-4d6a-4f1b-9c8d-7e6f5a4b3c2d"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildReattached(t, true)
			entry := r.s.sourceEntry(t, r.id)
			tc.change(&entry)
			if entry.AllocationID != r.id {
				// The writer refuses to change an entry's allocation, so we
				// rewrite the description the way another tool might.
				cfg := r.s.client.state.configs[777]
				description := pve.DescriptionFromConfig(cfg)
				field := `"allocation_id":"` + r.id + `"`
				if strings.Count(description, field) != 1 {
					t.Fatalf("777's description carries %s %d times, want once", field, strings.Count(description, field))
				}
				cfg["description"] = strings.Replace(description, field, `"allocation_id":"`+entry.AllocationID+`"`, 1)
			} else if err := pve.WriteDiskAllocationProvenance(context.Background(), r.s.client, "n1", 777, r.s.token, entry); err != nil {
				t.Fatal(err)
			}
			r.capture(t)
			r.writes = 0
			_ = r.deleteDisk(t, context.Background())
			r.requireUntouched(t)
			entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(r.s.client.state.configs[777]))
			if err != nil {
				t.Fatal(err)
			}
			if entries[r.s.token] != entry {
				t.Fatalf("777's entry = %+v, want %+v left in place", entries[r.s.token], entry)
			}
		})
	}
}

// TestDeleteDiskReattachedWriteOutcomeUnknown covers a removal whose answer
// never comes back. In the first row PVE never got the write, and in the second the
// write landed and its readback failed. Either way the call fails retriable
// and leaves the record in reconciliation_required, and a rerun ends Deleted
// with 777 written exactly once.
func TestDeleteDiskReattachedWriteOutcomeUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		landed bool
	}{
		{name: "write not applied"},
		{name: "write applied and its readback lost", landed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildReattached(t, true)
			client := r.s.client
			lost := errors.New("connection reset by peer")
			if tc.landed {
				client.afterConfigWrite = func(vmid int) {
					if vmid != 777 {
						return
					}
					r.writes++
					client.onConfigRead = func(read int) error {
						if read == 777 {
							return lost
						}
						return nil
					}
				}
			} else {
				client.descriptionErr = lost
			}
			err := r.deleteDisk(t, context.Background())
			requireRetriable(t, err, "delete_disk whose removal lost its answer")
			client.descriptionErr, client.onConfigRead = nil, nil
			r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
			if carries := r.s.hasEntry(777); carries == tc.landed {
				t.Fatalf("after the lost answer 777 carries the entry = %t, want %t", carries, !tc.landed)
			}
			if err := r.deleteDisk(t, context.Background()); err != nil {
				t.Fatalf("rerun of delete_disk: %v", err)
			}
			r.requireDeleted(t)
			if r.writes != 1 {
				t.Fatalf("777 was written %d times, want once", r.writes)
			}
		})
	}
}

// TestDeleteDiskReattachedCrashBeforeAudit covers a call that stops after the
// removal landed and before the completion audit could finish. An audit that
// loses its visibility stands in for that crash. A rerun finds the leftovers
// gone, writes nothing, and ends Deleted.
func TestDeleteDiskReattachedCrashBeforeAudit(t *testing.T) {
	r := buildReattached(t, true)
	client := r.s.client
	client.afterConfigWrite = func(vmid int) {
		if vmid == 777 {
			r.writes++
			client.visibilityErr = errors.New("visibility lost")
		}
	}
	if err := r.deleteDisk(t, context.Background()); err == nil {
		t.Fatal("delete_disk whose audit lost its visibility succeeded, want a refusal")
	}
	client.visibilityErr = nil
	r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
	if r.s.hasEntry(777) {
		t.Fatal("the removal didn't land before the audit")
	}
	if err := r.deleteDisk(t, context.Background()); err != nil {
		t.Fatalf("rerun of delete_disk after the audit stopped: %v", err)
	}
	r.requireDeleted(t)
	if r.writes != 1 {
		t.Fatalf("777 was written %d times, want once", r.writes)
	}
}

// TestDeleteDiskReattachedRaceKeepsOtherRemoval covers a retry that races a
// second disk's tail on 777. A first, recording run counts 777's reads and
// writes. Then the second disk's tail removes its own entry from 777 from
// another goroutine, at one of three moments. The first is right after the
// read our removal is built from, the second is as our removal arrives, and the
// third is after our removal lands. Wherever the second disk's removal lands,
// it must survive. When it lands before our removal reaches PVE, PVE refuses
// our removal's digest, so the call fails retriable and a rerun ends Deleted.
func TestDeleteDiskReattachedRaceKeepsOtherRemoval(t *testing.T) {
	var events []tailEvent
	recording := buildReattached(t, true)
	recording.s.seedOtherTailDisk(t)
	recordCtx := withTailHook(context.Background(), func(event tailEvent) { events = append(events, event) })
	if err := recording.deleteDisk(t, recordCtx); err != nil {
		t.Fatalf("retry with no race: %v", err)
	}
	write := -1
	for i, event := range events {
		if event == tailWrite {
			write = i
			break
		}
	}
	if write < 1 || events[write-1] != tailRead || write+1 >= len(events) || events[write+1] != tailWritten {
		t.Fatalf("777's events %v don't show the removal built from a read and landing", events)
	}
	for _, race := range []struct {
		name string
		at   int
	}{
		{name: "after the read the removal is built from", at: write - 1},
		{name: "as the removal arrives", at: write},
		{name: "after the removal lands", at: write + 1},
	} {
		t.Run(race.name, func(t *testing.T) {
			r := buildReattached(t, true)
			otherToken := r.s.seedOtherTailDisk(t)
			seen, raced := 0, false
			var raceErr error
			ctx := withTailHook(context.Background(), func(event tailEvent) {
				at := seen
				seen++
				if at != race.at {
					return
				}
				raced = true
				if event != events[race.at] {
					raceErr = fmt.Errorf("event %d is a %s, want a %s", race.at, event, events[race.at])
					return
				}
				raceErr = r.s.removeOtherTailEntry(otherToken)
			})
			err := r.deleteDisk(t, ctx)
			if !raced || raceErr != nil {
				t.Fatalf("the second disk's tail ran %t: %v", raced, raceErr)
			}
			if allocationEntryNamed(pve.DescriptionFromConfig(r.s.client.state.configs[777]), otherToken) {
				t.Fatal("777 carries the second disk's allocation entry again after its tail removed it")
			}
			if events[race.at] == tailWritten {
				if err != nil {
					t.Fatalf("retry whose removal landed before the second disk's tail: %v", err)
				}
			} else {
				requireRetriable(t, err, "retry that met the second disk's tail")
				if r.w.conflicts == 0 {
					t.Fatal("PVE refused no removal after the second disk's tail moved the digest")
				}
				if err := r.deleteDisk(t, context.Background()); err != nil {
					t.Fatalf("rerun of delete_disk after the race: %v", err)
				}
			}
			if allocationEntryNamed(pve.DescriptionFromConfig(r.s.client.state.configs[777]), otherToken) {
				t.Fatal("777 carries the second disk's allocation entry again")
			}
			r.s.requireRecord(t, r.id, aj.Deleted)
			r.s.requireSourceClean(t)
		})
	}
}

// otherHolder is a parked disk whose entry both 777 and 778 carry. 778 names
// the volume nowhere, and no record names 778.
type otherHolder struct {
	s  *strandedDisk
	id string
	// before is 778's description once its entry is there.
	before string
	// writes counts the configuration writes to 778.
	writes int
}

func buildOtherHolder(t *testing.T) *otherHolder {
	t.Helper()
	captureParkerPoolSweep(t)
	s, id, _ := buildLatentTailDisk(t)
	s.seedSourceEntry(t, 778, id)
	o := &otherHolder{s: s, id: id}
	o.capture()
	s.client.afterConfigWrite = func(vmid int) {
		if vmid == 778 {
			o.writes++
		}
	}
	return o
}

func (o *otherHolder) capture() {
	o.before = pve.DescriptionFromConfig(o.s.client.state.configs[778])
}

// requireKept checks that the delete refused, that the record didn't end
// Deleted, and that 778 still carries its entry, unwritten.
func (o *otherHolder) requireKept(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("delete_disk succeeded while 778 may still hold the disk, want a refusal")
	}
	record, inspectErr := o.s.journal.Inspect(o.id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if record.State == aj.Deleted {
		t.Fatal("the record ended Deleted while 778 may still hold the disk")
	}
	if description := pve.DescriptionFromConfig(o.s.client.state.configs[778]); description != o.before {
		t.Fatalf("778's description went from %s to %s, want it left as it was", o.before, description)
	}
	if o.writes != 0 {
		t.Fatalf("778 was written %d times, want none", o.writes)
	}
}

// TestDeleteDiskOtherHolderNamingVolumeKeepsEntry covers a 778 that names the
// volume only in a snapshot, only as a slot whose delete is pending, or only on
// an unused entry. A rollback, the pending change, or a later attach could bring
// the volume back to 778, so 778 still holds it. The delete refuses, and
// 778's entry stays as it was.
func TestDeleteDiskOtherHolderNamingVolumeKeepsEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		name778 func(o *otherHolder)
	}{
		{name: "snapshot", name778: func(o *otherHolder) {
			o.s.client.vmSnapshots = map[int][]map[string]any{778: {{"name": "before-upgrade"}}}
			o.s.client.snapshotConfigs = map[int]map[string]map[string]any{778: {"before-upgrade": {"scsi4": o.s.stranded + ",size=1G"}}}
		}},
		{name: "pending delete", name778: func(o *otherHolder) {
			o.s.client.pending = newFakePendingModel()
			o.s.client.pending.deletes[778] = map[string]any{"scsi4": o.s.stranded + ",size=1G"}
		}},
		{name: "unused entry", name778: func(o *otherHolder) {
			o.s.client.state.configs[778]["unused4"] = o.s.stranded
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := buildOtherHolder(t)
			tc.name778(o)
			o.capture()
			o.requireKept(t, deleteDiskAt(t, context.Background(), o.s.deps, o.s.cid))
		})
	}
}

// TestDeleteDiskOtherHolderInterruptedAttachKeepsEntry covers an attach to 778
// whose move lost its answer, so the record still has that move unobserved.
// The move may yet land the disk on 778, so the delete refuses, and 778's
// entry stays as it was.
func TestDeleteDiskOtherHolderInterruptedAttachKeepsEntry(t *testing.T) {
	o := buildOtherHolder(t)
	o.s.client.loseMoveResponse = true
	if err := attachDiskAt(t, context.Background(), o.s.deps, "778", o.s.cid); err == nil {
		t.Fatal("attach_disk whose move lost its answer succeeded, want a failure")
	}
	o.s.client.loseMoveResponse = false
	record, err := o.s.journal.Inspect(o.id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(record.Steps, func(step aj.Step) bool { return step.State != aj.Observed }) {
		t.Fatal("the interrupted attach left every step observed")
	}
	o.capture()
	o.writes = 0
	o.requireKept(t, deleteDiskAt(t, context.Background(), o.s.deps, o.s.cid))
}

// TestDeleteDiskOtherHolderReadFails covers a delete whose read of 778 fails
// or comes back without a digest. That is the read the removal would build its
// write from. The call fails retriable and writes nothing to 778, and a rerun
// ends Deleted.
func TestDeleteDiskOtherHolderReadFails(t *testing.T) {
	recording := buildOtherHolder(t)
	at := healReadAt(t, recording.s.client, 778, afterDelete(recording.s.client), func() error {
		return deleteDiskAt(t, context.Background(), recording.s.deps, recording.s.cid)
	})
	for _, tc := range []struct {
		name  string
		fault func(o *otherHolder) func() error
	}{
		{name: "read fails", fault: func(*otherHolder) func() error {
			return func() error { return errors.New("connection reset reading 778") }
		}},
		{name: "read without a digest", fault: func(o *otherHolder) func() error {
			return func() error {
				delete(o.s.client.state.configs[778], "digest")
				return nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := buildOtherHolder(t)
			digest := o.s.client.state.configs[778]["digest"]
			onReadAt(o.s.client, 778, at, afterDelete(o.s.client), tc.fault(o))
			err := deleteDiskAt(t, context.Background(), o.s.deps, o.s.cid)
			o.s.client.onConfigRead = nil
			requireRetriable(t, err, "delete_disk whose read of 778 failed")
			o.s.client.state.configs[778]["digest"] = digest
			o.requireKept(t, err)
			if err := deleteDiskAt(t, context.Background(), o.s.deps, o.s.cid); err != nil {
				t.Fatalf("rerun of delete_disk after the failed read: %v", err)
			}
			if o.s.hasEntry(778) {
				t.Fatal("778 still carries the allocation's entry after the rerun")
			}
			o.s.requireRecord(t, o.id, aj.Deleted)
		})
	}
}

// TestDeleteDiskStaleNoteOtherDiskOnReusedNameStays covers another journaled
// disk that a move put on 777 under the renamed disk's old name, and that a
// config-edit attach then took to 778 under that name. 778 holds that disk and
// carries its entry, which names the old name too. That entry is keyed by the
// other disk's stable ID and names the other disk's own allocation, so the
// retry neither counts it as ours nor touches it, and ends Deleted.
func TestDeleteDiskStaleNoteOtherDiskOnReusedNameStays(t *testing.T) {
	s := newStaleNote(t, false, true)
	ctx := context.Background()
	reused := s.f.volume
	other, entry := otherDiskOnReusedName(t, s)
	s.f.client.state.configs[778] = map[string]any{"name": "w778", "digest": "1", "scsi1": reused + ",serial=" + other.token + ",size=5G"}
	if err := pve.WriteDiskAllocationProvenance(ctx, s.f.client, "n1", 778, other.token, entry); err != nil {
		t.Fatal(err)
	}
	before := s.f.client.state.configs[778]["description"]
	writes := 0
	s.f.client.afterConfigWrite = func(vmid int) {
		switch vmid {
		case 777:
			s.writes++
		case 778:
			writes++
		}
	}
	if err := s.deleteDisk(t); err != nil {
		t.Fatalf("retried delete_disk with another disk on the reused name: %v", err)
	}
	s.requireState(t, aj.Deleted)
	if description := s.f.client.state.configs[778]["description"]; description != before {
		t.Fatalf("778's description went from %v to %v, want it byte for byte", before, description)
	}
	if writes != 0 {
		t.Fatalf("778 was written %d times, want none", writes)
	}
	if s.f.client.state.volumes[reused] == nil {
		t.Fatalf("delete_disk destroyed the other disk's volume %s", reused)
	}
	if s.writes != 1 {
		t.Fatalf("the retry wrote 777 %d times, want once", s.writes)
	}
}

// otherDiskOnReusedName journals another disk that a move put on 777 under the
// renamed disk's old name, gives that disk a volume under that name, and
// returns the disk along with the allocation entry that a VM holding it
// carries.
func otherDiskOnReusedName(t *testing.T, s *staleNote) (lifecycleFlowDisk, pve.DiskAllocationProvenance) {
	t.Helper()
	ctx := context.Background()
	reused := s.f.volume
	storage, _, err := pve.ParseDiskCID(reused)
	if err != nil {
		t.Fatal(err)
	}
	backing, err := managedDiskActualBacking(ctx, s.f.deps, storage)
	if err != nil {
		t.Fatal(err)
	}
	// The plan may put the other disk on any storage of the set. The move's
	// step records the storage it landed on, which is the storage of the
	// reused name's volume.
	other := journalLifecycleFlowDisk(t, s.f.journal, s.f.client.state, false, false, func(handle *aj.Handle) {
		created := handle.Record().Steps[0].Target
		step, err := storageMutationIntent(handle, "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk", aj.Target{Node: "n1", Storage: storage, Backing: backing, VMID: 123, IntendedVolume: created.IntendedVolume}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := storageMutationObserved(handle, step, []string{created.IntendedVolume, reused}, false); err != nil {
			t.Fatal(err)
		}
		record := handle.Record()
		cid, err := pve.EncodeDiskCID(created.IntendedVolume, &pve.DiskCIDMeta{ID: record.DiskToken, Format: "raw"})
		if err != nil {
			t.Fatal(err)
		}
		record.State, record.CID = aj.ReadyToReturn, cid
		if err := handle.Save(record); err != nil {
			t.Fatal(err)
		}
	})
	delete(s.f.client.state.volumes, other.volume)
	s.f.client.state.volumes[reused] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	return other, pve.DiskAllocationProvenance{Version: 1, AllocationID: other.id, AllocationNamespace: lifecycleFlowNamespace, Volid: reused, Node: "n1", Backing: backing}
}

// TestDeleteDiskHealsOnRetryOnceHoldsSettle covers a disk whose volume is gone
// while a VM still holds it. The first call refuses and writes nothing. Once
// we settle the hold, the next call heals the leftover notes and ends Deleted
// with no recreate of the VM. The hold is a name on an unused entry, on a bus
// slot, as a drive with the disk's serial, in a snapshot, or as a pending
// delete, and each one sits on 777, the VM that carries the leftovers. The bus
// slot and the pending delete name the volume under the name the disk had on
// 888, because a slot under the birth name would stop at the identity check
// before the heal ran. A further subtest puts the hold on 778, a VM that
// carries its own entry for the disk.
func TestDeleteDiskHealsOnRetryOnceHoldsSettle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal string
		hold    func(r *reattached) func()
	}{
		{name: "unused entry", refusal: "still names its volume", hold: func(r *reattached) func() {
			r.s.client.state.configs[777]["unused1"] = r.s.stranded
			return func() { delete(r.s.client.state.configs[777], "unused1") }
		}},
		{name: "bus slot", refusal: "artifact or provenance remains; reconciliation required", hold: func(r *reattached) func() {
			r.s.client.state.configs[777]["scsi4"] = r.onEight + ",size=1G"
			return func() { delete(r.s.client.state.configs[777], "scsi4") }
		}},
		{name: "drive with the disk's serial", refusal: "conflicts with journal; audit required", hold: func(r *reattached) func() {
			storage, _, err := pve.ParseDiskCID(r.s.stranded)
			if err != nil {
				t.Fatal(err)
			}
			r.s.client.state.configs[777]["scsi5"] = storage + ":777/vm-777-disk-5.raw,serial=" + r.s.token + ",size=1G"
			return func() { delete(r.s.client.state.configs[777], "scsi5") }
		}},
		{name: "snapshot", refusal: "artifact or provenance remains; reconciliation required", hold: func(r *reattached) func() {
			r.s.client.vmSnapshots = map[int][]map[string]any{777: {{"name": "before-upgrade"}}}
			r.s.client.snapshotConfigs = map[int]map[string]map[string]any{777: {"before-upgrade": {"scsi4": r.s.stranded + ",size=1G"}}}
			return func() { r.s.client.vmSnapshots, r.s.client.snapshotConfigs = nil, nil }
		}},
		{name: "pending delete", refusal: "artifact or provenance remains; reconciliation required", hold: func(r *reattached) func() {
			r.s.client.pending = newFakePendingModel()
			r.s.client.pending.deletes[777] = map[string]any{"scsi4": r.onEight + ",size=1G"}
			return func() { delete(r.s.client.pending.deletes, 777) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildReattached(t, true)
			settle := tc.hold(r)
			r.capture(t)
			err := r.deleteDisk(t, context.Background())
			if err == nil {
				t.Fatalf("delete_disk while 777 holds the disk as %s succeeded, want a refusal", tc.name)
			}
			if _, birth := pve.IsDiskBirthNameHeld(err); birth {
				t.Fatalf("delete_disk stopped at the birth-name check, so it never reached the heal: %v", err)
			}
			if !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("delete_disk's refusal is %q, want it to contain %q", err, tc.refusal)
			}
			r.s.requireRecord(t, r.id, aj.ReconciliationRequired)
			r.requireUntouched(t)
			settle()
			r.capture(t)
			if err := r.deleteDisk(t, context.Background()); err != nil {
				t.Fatalf("retried delete_disk after we settled the hold: %v", err)
			}
			r.requireDeleted(t)
			if r.writes != 1 {
				t.Fatalf("the retry wrote 777 %d times, want once", r.writes)
			}
		})
	}
	t.Run("hold on a VM with its own entry", func(t *testing.T) {
		o := buildOtherHolder(t)
		o.s.client.state.configs[778]["unused4"] = o.s.stranded
		o.capture()
		o.requireKept(t, deleteDiskAt(t, context.Background(), o.s.deps, o.s.cid))
		delete(o.s.client.state.configs[778], "unused4")
		o.capture()
		if err := deleteDiskAt(t, context.Background(), o.s.deps, o.s.cid); err != nil {
			t.Fatalf("retried delete_disk after we settled 778's hold: %v", err)
		}
		if o.s.hasEntry(778) {
			t.Fatal("778 still carries the allocation's entry after the retry")
		}
		if o.writes != 1 {
			t.Fatalf("the retry wrote 778 %d times, want once", o.writes)
		}
		o.s.requireRecord(t, o.id, aj.Deleted)
		o.s.requireSourceClean(t)
	})
}
