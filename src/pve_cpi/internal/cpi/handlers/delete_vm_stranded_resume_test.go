package handlers

// delete_vm moves a stable-ID disk it finds on an active slot to a parker
// before it destroys the VM. When that move fails after the slot delete, the
// volume sits on the VM's unused entry with no serial, and the parker's
// transfer record is the only thing that names it. These tests build that
// state through delete_vm itself and check what the next delete_vm does: it
// finishes the move from the record, and it keeps refusing whenever no record
// proves whose the volume is.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// deleteStrandedEpoch is when the failing delete_vm runs.
var deleteStrandedEpoch = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// buildLegacyDeleteFixture puts one legacy stable-ID disk, named for 777, on
// 777's scsi1 and returns the state before any delete_vm runs. renamed parks
// the disk and attaches it back first, so the volume carries a name the move
// gave it; local puts every volume on node-local storage. fast turns on the
// fast delete path.
func buildLegacyDeleteFixture(t *testing.T, renamed, local, fast bool) *strandedDisk {
	t.Helper()
	deps, client, journal, _, _ := lifecycleFlowFixture(t)
	if local {
		client.localStorage = true
		client.volumeNodes = map[string]string{}
	}
	recorder := &legacyDestroyRecorder{}
	deps.PVE = legacyDestroyPVE{lifecycleFlowPVE: client, recorder: recorder}
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	if fast {
		enabled := true
		deps.Config.FastPathDelete = &enabled
	}
	original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	delete(client.state.volumes, original)

	token, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	volume := "a:777/vm-777-disk-0.raw"
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	if local {
		client.volumeNodes[volume] = "n1"
		deps.Resolver = pve.NewBackendResolver(deps.PVE, nil, "n1")
	}
	client.state.configs[777]["scsi1"] = volume + ",serial=" + token + ",size=5G"
	s := &strandedDisk{deps: deps, client: client, recorder: recorder, journal: journal, cid: cid, token: token}

	if renamed {
		at := atProvenanceTime(deleteStrandedEpoch)
		if err := detachDiskAt(t, at, deps, "777", cid); err != nil {
			t.Fatalf("park the disk: %v", err)
		}
		if err := attachDiskAt(t, at, deps, "777", cid); err != nil {
			t.Fatalf("attach it back to 777: %v", err)
		}
	}
	return s
}

// buildDeleteStrandedDisk runs a delete_vm whose move to the parker fails
// after the slot delete, and checks that it left the volume on 777's unused
// entry with the parker's record naming it.
func buildDeleteStrandedDisk(t *testing.T, renamed, local, fast bool) *strandedDisk {
	t.Helper()
	s := buildLegacyDeleteFixture(t, renamed, local, fast)
	s.client.moveErr = errors.New("move_disk: storage migration failed")
	if err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch), s.deps); err == nil {
		t.Fatal("the delete_vm whose move fails reported success")
	}
	s.client.moveErr = nil
	for _, volume := range pve.FindUnusedDiskEntries(s.client.state.configs[777]) {
		s.stranded = volume
	}
	if s.stranded == "" {
		t.Fatalf("the failed delete_vm left no unused entry on 777: %v", s.client.state.configs[777])
	}
	if len(s.serialHolders()) != 0 {
		t.Fatalf("the failed delete_vm left the serial on %v, want it on no slot", s.serialHolders())
	}
	if s.intentParker() == 0 {
		t.Fatal("the failed delete_vm left no transfer record on any parker")
	}
	if len(s.recorder.guestDestroys()) != 0 {
		t.Fatal("the failed delete_vm destroyed 777")
	}
	s.recorder.submissions = nil
	return s
}

// deleteVMAt runs delete_vm on VM 777 at the provenance time ctx carries.
func deleteVMAt(t *testing.T, ctx context.Context, deps Deps) error {
	t.Helper()
	_, err := HandleDeleteVM(deps).Handle(ctx, []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	return err
}

// dropTransferRecord removes the disk's record from its parker, the way an
// operator's edit of the parker's description would.
func (s *strandedDisk) dropTransferRecord(t *testing.T) {
	t.Helper()
	parker := s.intentParker()
	cfg := s.client.state.configs[parker]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var disks map[string]json.RawMessage
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	delete(disks, s.token)
	if len(disks) == 0 {
		delete(raw, "bosh_parked_disks")
	} else {
		encoded, err := json.Marshal(disks)
		if err != nil {
			t.Fatal(err)
		}
		raw["bosh_parked_disks"] = encoded
	}
	desc, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = desc
	if s.intentParker() != 0 {
		t.Fatal("the record is still on a parker")
	}
}

// requireRefusedKeepingVolume fails unless err is the unused-slot refusal,
// nothing was destroyed, and volume still exists and is still named by 777's
// unused entry.
func (s *strandedDisk) requireRefusedKeepingVolume(t *testing.T, err error, volume string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "persistent volumes still attached as unused slots") {
		t.Fatalf("delete_vm = %v, want the unused-slot refusal", err)
	}
	if !strings.Contains(err.Error(), volume) {
		t.Fatalf("the refusal %q doesn't name %s", err.Error(), volume)
	}
	if len(s.recorder.submissions) != 0 {
		t.Fatalf("delete_vm submitted %d destroys", len(s.recorder.submissions))
	}
	if s.client.state.volumes[volume] == nil {
		t.Fatalf("%s no longer exists", volume)
	}
	if refs := strings.Join(s.references(volume), " "); !strings.Contains(refs, "777.unused") {
		t.Fatalf("%s left 777's unused entry: %v", volume, refs)
	}
}

// TestDeleteVMRetryFinishesAStrandedTransfer covers a delete_vm whose move
// failed after the slot delete. Before the change every shape refused for
// good, because no slot carried the serial and the guard refused the unused
// entry. Now the retry finishes the move from the parker's record, parks the
// disk on one serial-carrying slot, and destroys the VM without its volume.
func TestDeleteVMRetryFinishesAStrandedTransfer(t *testing.T) {
	for _, fast := range []bool{false, true} {
		for _, shape := range strandedShapes {
			t.Run(fmt.Sprintf("fast=%t/%s", fast, shape.name), func(t *testing.T) {
				s := buildDeleteStrandedDisk(t, shape.renamed, shape.local, fast)
				later := atProvenanceTime(deleteStrandedEpoch.Add(time.Hour))
				if err := deleteVMAt(t, later, s.deps); err != nil {
					t.Fatalf("the retried delete_vm failed: %v", err)
				}
				destroys := s.recorder.guestDestroys()
				if len(destroys) != 1 {
					t.Fatalf("the retried delete_vm submitted %d destroys of 777, want 1", len(destroys))
				}
				for key, value := range destroys[0].configs[777] {
					if isDiskOptionKey(key) {
						t.Fatalf("VM 777 was destroyed while %s still named %v", key, value)
					}
				}
				s.requireSingleHolder(t, true)
				if s.client.state.volumes[s.stranded] != nil && len(s.references(s.stranded)) != 0 {
					t.Fatalf("%s is still referenced by %v", s.stranded, s.references(s.stranded))
				}

				s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
				if err := attachDiskAt(t, later, s.deps, "888", s.cid); err != nil {
					t.Fatalf("attach_disk(888) after the retry: %v", err)
				}
				if vmid := s.requireSingleHolder(t, false); vmid != 888 {
					t.Fatalf("the disk landed on VM %d, want 888", vmid)
				}
			})
		}
	}
}

// TestDeleteVMRetryWithoutTheRecordKeepsRefusing covers the same stranded
// state with the parker's record gone, once because the record was removed
// and once because the parker itself was. Nothing then proves the volume is
// the disk, so the retry refuses and leaves the volume on 777.
func TestDeleteVMRetryWithoutTheRecordKeepsRefusing(t *testing.T) {
	for _, gone := range []string{"record", "parker"} {
		for _, shape := range strandedShapes {
			t.Run(gone+"/"+shape.name, func(t *testing.T) {
				s := buildDeleteStrandedDisk(t, shape.renamed, shape.local, false)
				switch gone {
				case "record":
					s.dropTransferRecord(t)
				case "parker":
					delete(s.client.state.configs, s.intentParker())
				}
				err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch.Add(time.Hour)), s.deps)
				s.requireRefusedKeepingVolume(t, err, s.stranded)
			})
		}
	}
}

// TestDeleteVMRetryLeavesAnUnclaimedUnusedVolume covers unused volumes no
// record names. On its own such a volume keeps today's refusal. Next to a
// stranded transfer, the retry finishes the transfer, and it still refuses
// over the unclaimed volume and leaves it where it is. Before the change the
// second row also refused, but over both volumes, with the disk not parked.
func TestDeleteVMRetryLeavesAnUnclaimedUnusedVolume(t *testing.T) {
	const unclaimed = "a:777/vm-777-disk-9.raw"
	addUnclaimed := func(s *strandedDisk) {
		s.client.state.volumes[unclaimed] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
		if s.client.volumeNodes != nil {
			s.client.volumeNodes[unclaimed] = "n1"
		}
		s.client.state.configs[777]["unused7"] = unclaimed
	}

	t.Run("alone", func(t *testing.T) {
		s := buildLegacyDeleteFixture(t, false, false, false)
		delete(s.client.state.configs[777], "scsi1")
		addUnclaimed(s)
		err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch), s.deps)
		s.requireRefusedKeepingVolume(t, err, unclaimed)
	})

	t.Run("beside a stranded transfer", func(t *testing.T) {
		s := buildDeleteStrandedDisk(t, true, false, false)
		addUnclaimed(s)
		err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch.Add(time.Hour)), s.deps)
		s.requireRefusedKeepingVolume(t, err, unclaimed)
		if strings.Contains(err.Error(), s.stranded) {
			t.Fatalf("the refusal %q still names the stranded volume %s", err.Error(), s.stranded)
		}
		s.requireSingleHolder(t, true)
	})
}

// TestDeleteVMFailureBeforeTheSlotDeleteKeepsTheDisk covers a transfer that
// fails before the slot delete, here because the parker's record can't be
// written. The disk stays on its slot with its serial, nothing is destroyed,
// and the refusal says what a retry does. The retry then transfers the disk
// as it always has.
func TestDeleteVMFailureBeforeTheSlotDeleteKeepsTheDisk(t *testing.T) {
	s := buildLegacyDeleteFixture(t, true, false, false)
	s.client.descriptionErr = errors.New("description write failed")
	err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch), s.deps)
	s.client.descriptionErr = nil
	if err == nil {
		t.Fatal("delete_vm reported success while the record write failed")
	}
	want := "nothing was destroyed. A delete_vm retry transfers the disk again while it is still on its slot"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("delete_vm = %q\nwant it to contain %q", err.Error(), want)
	}
	if len(s.recorder.submissions) != 0 {
		t.Fatalf("delete_vm submitted %d destroys", len(s.recorder.submissions))
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 777 {
		t.Fatalf("the disk moved to VM %d, want it still on 777", vmid)
	}
	if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
		t.Fatalf("777 holds unused entries %v after a failure before the slot delete", unused)
	}

	if err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch.Add(time.Hour)), s.deps); err != nil {
		t.Fatalf("the retried delete_vm failed: %v", err)
	}
	if len(s.recorder.guestDestroys()) != 1 {
		t.Fatalf("the retried delete_vm submitted %d destroys of 777, want 1", len(s.recorder.guestDestroys()))
	}
	s.requireSingleHolder(t, true)
}

// TestDeleteVMFinishesATransferADetachStranded covers the same stranded state
// left by a detach_disk instead, whose record carries the Director's CID. The
// delete finishes the transfer from that record too, and the disk attaches
// elsewhere afterwards. Before the change every shape refused.
func TestDeleteVMFinishesATransferADetachStranded(t *testing.T) {
	for _, shape := range strandedShapes {
		t.Run(shape.name, func(t *testing.T) {
			s := buildStrandedDisk(t, false, shape.renamed, shape.local)
			later := atProvenanceTime(strandedEpoch.Add(3 * time.Hour))
			if err := deleteVMAt(t, later, s.deps); err != nil {
				t.Fatalf("delete_vm(777): %v", err)
			}
			for _, destroy := range s.recorder.guestDestroys() {
				for key, value := range destroy.configs[777] {
					if isDiskOptionKey(key) {
						t.Fatalf("VM 777 was destroyed while %s still named %v", key, value)
					}
				}
			}
			s.requireSingleHolder(t, true)
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			if err := attachDiskAt(t, later, s.deps, "888", s.cid); err != nil {
				t.Fatalf("attach_disk(888): %v", err)
			}
			if vmid := s.requireSingleHolder(t, false); vmid != 888 {
				t.Fatalf("the disk landed on VM %d, want 888", vmid)
			}
		})
	}
}

// TestDeleteVMLeavesAManagedStrandedTransfer covers a journal-managed disk in
// the stranded state. Its resume needs the allocation's own context, which
// delete_vm doesn't have, so the delete keeps refusing, the volume stays on
// 777, and the record stays for the settlement.
func TestDeleteVMLeavesAManagedStrandedTransfer(t *testing.T) {
	s := buildStrandedDisk(t, true, true, false)
	logger, observed := log.NewObservedLogger(slog.LevelDebug)
	s.deps.Logger = logger
	err := deleteVMAt(t, atProvenanceTime(strandedEpoch.Add(3*time.Hour)), s.deps)
	if err == nil {
		t.Fatal("delete_vm destroyed a VM that holds a managed disk's stranded volume")
	}
	// The unused-slot refusal, and the skip warning for the managed record,
	// show that the managed-allocation branch is what declined the resume.
	// Any other branch would log another reason, and a resume that ran and
	// failed would return its own error instead.
	if !strings.Contains(err.Error(), "persistent volumes still attached as unused slots") {
		t.Fatalf("delete_vm = %v, want the unused-slot refusal", err)
	}
	skipped := 0
	for _, e := range observed.All() {
		if strings.Contains(e.Message, "can't finish") {
			if reason := fmt.Sprint(e.Attrs["reason"]); reason != "the record belongs to a managed allocation" {
				t.Fatalf("the resume was skipped for %q, want the managed allocation", reason)
			}
			skipped++
		}
	}
	if skipped != 1 {
		t.Fatalf("%d skip warnings, want 1", skipped)
	}
	for _, e := range observed.All() {
		if strings.Contains(e.Message, "finishing it before destroy") {
			t.Fatal("delete_vm started a resume of a managed disk's transfer")
		}
	}
	if len(s.recorder.guestDestroys()) != 0 {
		t.Fatalf("delete_vm submitted %d destroys of 777", len(s.recorder.guestDestroys()))
	}
	if s.client.state.volumes[s.stranded] == nil || !strings.Contains(strings.Join(s.references(s.stranded), " "), "777.unused") {
		t.Fatalf("the volume %s left 777's unused entry: present=%t refs=%v", s.stranded, s.client.state.volumes[s.stranded] != nil, s.references(s.stranded))
	}
	if s.intentParker() == 0 {
		t.Fatal("the transfer record was collected")
	}
}

// TestDeleteVMRetryStoppedBySnapshotAsksForTheSnapshotsGone covers a stranded
// transfer whose move PVE refuses because a snapshot of the VM still names the
// volume. Only an operator can clear that, so the retry fails for good with
// the class detach_disk gives a snapshot block, names the snapshot to delete,
// destroys nothing, and leaves the volume and the record where they are.
func TestDeleteVMRetryStoppedBySnapshotAsksForTheSnapshotsGone(t *testing.T) {
	for _, fast := range []bool{false, true} {
		for _, listed := range []bool{true, false} {
			t.Run(fmt.Sprintf("fast=%t/listed=%t", fast, listed), func(t *testing.T) {
				s := buildDeleteStrandedDisk(t, true, false, fast)
				s.client.moveErr = errors.New("Can't move disk used by a snapshot to another VM")
				if listed {
					s.client.snapshots = []map[string]any{{"name": "pre-upgrade"}}
				} else {
					s.client.snapshotErr = errors.New("snapshot listing unavailable")
				}
				err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch.Add(time.Hour)), s.deps)
				requireSnapshotBlock(t, err, s, listed)
			})
		}
	}
}

// TestDeleteVMRetryStoppedBySnapshotWithNoneListed covers PVE's snapshot
// refusal of the move while the VM's snapshot listing answers with none. The
// refusal stays permanent and destroys nothing, and since no snapshot can be
// named, it asks for the VM's configuration to be checked for another
// reference to the volume rather than for a snapshot's deletion.
func TestDeleteVMRetryStoppedBySnapshotWithNoneListed(t *testing.T) {
	s := buildDeleteStrandedDisk(t, true, false, false)
	s.client.moveErr = errors.New("Can't move disk used by a snapshot to another VM")
	s.client.snapshots = nil
	err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch.Add(time.Hour)), s.deps)
	requireSnapshotBlock(t, err, s, false)
	requireText(t, err, "delete_vm with no snapshot listed",
		[]string{"in use by a snapshot", "lists no snapshot", "nothing was destroyed",
			"Check the VM's configuration for another reference to the volume, then retry delete_vm"},
		"Delete the VM's snapshot")
}

// requireSnapshotBlock fails unless err is the permanent snapshot block that
// names the stranded volume and the snapshots to delete, and nothing was
// destroyed or moved. With listed false the snapshot names are unavailable,
// and the text still says what to delete.
func requireSnapshotBlock(t *testing.T, err error, s *strandedDisk, listed bool) {
	t.Helper()
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("delete_vm = %v, want a CPI error", err)
	}
	if cpiErr.Type() != cpierrors.TypeSnapshotBlocked || cpiErr.OkToRetry() {
		t.Fatalf("delete_vm error is %s retriable=%t, want a permanent snapshot block: %v", cpiErr.Type(), cpiErr.OkToRetry(), err)
	}
	text := err.Error()
	if !strings.Contains(text, "Delete snapshot") && !strings.Contains(text, "Delete the VM's snapshot") &&
		!strings.Contains(text, "Check the VM's configuration for another reference") {
		t.Fatalf("delete_vm = %q, want it to say what to delete or check", text)
	}
	if listed && !strings.Contains(text, "pre-upgrade") {
		t.Fatalf("delete_vm = %q, want it to name snapshot pre-upgrade", text)
	}
	if !strings.Contains(text, s.stranded) {
		t.Fatalf("delete_vm = %q, want it to name %s", text, s.stranded)
	}
	if len(s.recorder.submissions) != 0 || len(s.recorder.guestDestroys()) != 0 {
		t.Fatal("delete_vm destroyed something while a snapshot blocked the move")
	}
	if s.client.state.volumes[s.stranded] == nil || !strings.Contains(strings.Join(s.references(s.stranded), " "), "777.unused") {
		t.Fatalf("the volume %s left 777's unused entry: %v", s.stranded, s.references(s.stranded))
	}
	if s.intentParker() == 0 {
		t.Fatal("the transfer record was collected")
	}
}

// TestDeleteVMRetryFailsClosedWhenAParkerCannotBeRead covers a parker whose
// config read fails for a reason other than the parker being gone. A record
// that was never read can't count as a record that isn't there, so the retry
// returns the read failure, retriable unless PVE gave a verdict, and destroys
// nothing and moves nothing.
func TestDeleteVMRetryFailsClosedWhenAParkerCannotBeRead(t *testing.T) {
	rows := []struct {
		name      string
		readErr   error
		retriable bool
	}{
		{"transport timeout", &net.DNSError{Err: "i/o timeout", Name: "pve1", IsTimeout: true}, true},
		{"server error", sdkerrors.ParseAPIError(500, []byte(`{"message":"pmxcfs busy"}`)), true},
		{"denied grant", sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed"}`)), false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := buildDeleteStrandedDisk(t, true, false, false)
			parker := s.intentParker()
			before := s.client.state.volumes[s.stranded]
			moves := s.client.moves
			s.client.onConfigRead = func(vmid int) error {
				if vmid == parker {
					return row.readErr
				}
				return nil
			}
			err := deleteVMAt(t, atProvenanceTime(deleteStrandedEpoch.Add(time.Hour)), s.deps)
			s.client.onConfigRead = nil
			var cpiErr *cpierrors.Error
			if !errors.As(err, &cpiErr) {
				t.Fatalf("delete_vm = %v, want a CPI error", err)
			}
			if cpiErr.OkToRetry() != row.retriable {
				t.Fatalf("delete_vm retriable=%t, want %t: %v", cpiErr.OkToRetry(), row.retriable, err)
			}
			if !strings.Contains(err.Error(), "nothing was destroyed") {
				t.Fatalf("delete_vm = %q, want it to say nothing was destroyed", err.Error())
			}
			if len(s.recorder.submissions) != 0 || len(s.recorder.guestDestroys()) != 0 {
				t.Fatal("delete_vm destroyed something while a parker was unreadable")
			}
			if s.client.moves != moves || s.client.state.volumes[s.stranded] != before {
				t.Fatal("delete_vm moved the volume while a parker was unreadable")
			}
			if refs := strings.Join(s.references(s.stranded), " "); !strings.Contains(refs, "777.unused") {
				t.Fatalf("the volume %s left 777's unused entry: %v", s.stranded, refs)
			}
			if s.intentParker() == 0 {
				t.Fatal("the transfer record was collected")
			}
		})
	}
}
