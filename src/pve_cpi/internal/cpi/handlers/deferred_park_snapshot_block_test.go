package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// buildDeferredPark strands a disk named for 777 the way a detach_disk that
// a snapshot defers leaves it. The disk is parked and attached back so 777
// owns its name, and then a detach meets PVE's snapshot refusal of the move,
// which succeeds with the volume on 777's unused entry and the parker's
// transfer record unfinished. The refusal stays in place for the caller.
// managed picks a journal-managed disk over one without a journal, and it
// returns the allocation's ID, which is empty for the latter.
func buildDeferredPark(t *testing.T, managed bool) (*strandedDisk, string) {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	recorder := &legacyDestroyRecorder{}
	deps.PVE = legacyDestroyPVE{lifecycleFlowPVE: client, recorder: recorder}
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	deps.Config.DetachedDiskStrategy = "parked"
	s := &strandedDisk{client: client, recorder: recorder, journal: journal, managed: managed}
	if managed {
		record, err := journal.Inspect(id)
		if err != nil {
			t.Fatal(err)
		}
		s.cid, s.token = cid, record.DiskToken
	} else {
		id = ""
		original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
		delete(client.state.volumes, original)
		deps.Config.StoragePlacementNamespace = ""
		deps.Config.StorageAllocationJournalDir = ""
		token, err := pve.GenerateDiskStableID()
		if err != nil {
			t.Fatal(err)
		}
		volume := "a:124/vm-124-disk-0.raw"
		encoded, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw"})
		if err != nil {
			t.Fatal(err)
		}
		client.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
		client.state.configs[777]["scsi1"] = volume + ",serial=" + token + ",size=5G"
		s.cid, s.token = encoded, token
	}
	s.deps = deps
	ctx := context.Background()
	if err := detachDiskAt(t, ctx, deps, "777", s.cid); err != nil {
		t.Fatalf("park the disk: %v", err)
	}
	if err := attachDiskAt(t, ctx, deps, "777", s.cid); err != nil {
		t.Fatalf("attach it back to 777: %v", err)
	}
	client.moveErr = moveSnapshotRefusal()
	if err := detachDiskAt(t, ctx, deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk that the snapshot defers: %v", err)
	}
	for _, volume := range pve.FindUnusedDiskEntries(client.state.configs[777]) {
		s.stranded = volume
	}
	if s.stranded == "" || s.intentParker() == 0 {
		t.Fatalf("no deferred park: unused=%q parker=%d", s.stranded, s.intentParker())
	}
	if managed {
		s.requireRecord(t, id, aj.ReadyToReturn)
	}
	// 777's snapshot is what PVE refuses the move for, so the listing names it.
	client.vmSnapshots = map[int][]map[string]any{777: {{"name": "current"}, {"name": "pre-upgrade"}}}
	recorder.submissions = nil
	return s, id
}

// requireSnapshotBlocked fails unless err is the permanent snapshot refusal
// for op, naming the snapshot and 777's unused entry and asking only for the
// snapshot's deletion and a retry of op.
func (s *strandedDisk) requireSnapshotBlocked(t *testing.T, op string, err error, snapshot string) {
	t.Helper()
	var typed *cpierrors.Error
	if !errors.As(err, &typed) {
		t.Fatalf("%s while the snapshot blocks the park: err = %v, want a typed CPI error", op, err)
	}
	if typed.Type() != cpierrors.TypeSnapshotBlocked || typed.OkToRetry() {
		t.Fatalf("%s: type %s retry %t, want %s and no retry: %v", op, typed.Type(), typed.OkToRetry(), cpierrors.TypeSnapshotBlocked, err)
	}
	text := err.Error()
	for _, want := range []string{
		op + ": ",
		"unused0=" + s.stranded,
		snapshot,
		"then retry " + op,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("%s refusal %q doesn't say %q", op, text, want)
		}
	}
	if strings.Contains(strings.ToLower(text), "remove the") {
		t.Fatalf("%s refusal %q suggests removing something by hand", op, text)
	}
}

// deferredParkCall is one call a deferred park can meet. target is the VM an
// attach_disk names, and it is empty for delete_disk.
type deferredParkCall struct {
	name, op, target string
}

// run sends the call for s's disk.
func (c deferredParkCall) run(t *testing.T, s *strandedDisk) error {
	t.Helper()
	if c.op == "delete_disk" {
		return deleteDiskAt(t, context.Background(), s.deps, s.cid)
	}
	return attachDiskAt(t, context.Background(), s.deps, c.target, s.cid)
}

// requireBlocked sends the call while the snapshot blocks the park and fails
// unless it refuses permanently, changes nothing, leaves the volume on 777's
// unused entry alone, and leaves a managed record ready_to_return.
func (c deferredParkCall) requireBlocked(t *testing.T, s *strandedDisk, id string) {
	t.Helper()
	before, deletes := s.snapshot(), s.client.deletes
	s.requireSnapshotBlocked(t, c.op, c.run(t, s), "pre-upgrade")
	s.requireUnchanged(t, before, deletes)
	if refs := s.references(s.stranded); len(refs) != 1 || refs[0] != "777.unused0" {
		t.Fatalf("%s is referenced by %v, want only 777.unused0", s.stranded, refs)
	}
	if id != "" {
		s.requireRecord(t, id, aj.ReadyToReturn)
	}
}

// requireConverged sends the call once the snapshot is gone and fails unless
// it finishes the park and then attaches the disk to the target alone, or
// deletes it.
func (c deferredParkCall) requireConverged(t *testing.T, s *strandedDisk, id string) {
	t.Helper()
	if err := c.run(t, s); err != nil {
		t.Fatalf("%s after the snapshot was deleted: %v", c.name, err)
	}
	if refs := s.references(s.stranded); len(refs) != 0 {
		t.Fatalf("%s still names %s after the park finished", strings.Join(refs, ", "), s.stranded)
	}
	holders := s.serialHolders()
	if c.op == "delete_disk" {
		if len(holders) != 0 {
			t.Fatalf("delete_disk left the disk on %v", holders)
		}
		if id != "" {
			s.requireRecord(t, id, aj.Deleted)
		}
		return
	}
	if len(holders) != 1 {
		t.Fatalf("attach_disk left the disk on %v, want one slot on %s", holders, c.target)
	}
	for slot := range holders {
		if !strings.HasPrefix(slot, c.target+".") {
			t.Fatalf("attach_disk put the disk on %s, want %s", slot, c.target)
		}
	}
	if id != "" {
		s.requireRecord(t, id, aj.ReadyToReturn)
	}
}

// TestDeferredParkSnapshotBlocksAttachAndDelete covers attach_disk and
// delete_disk on a disk whose park a snapshot deferred, while the snapshot
// still exists and after it is gone. While it blocks, each call refuses
// permanently with the snapshot and the unused entry named, moves nothing, and
// returns a managed disk's allocation as ready_to_return, and a second call
// answers the same way. Once the snapshot is deleted, each call finishes the
// park and then does its own work.
func TestDeferredParkSnapshotBlocksAttachAndDelete(t *testing.T) {
	calls := []deferredParkCall{
		{name: "attach_disk to 888", op: "attach_disk", target: "888"},
		{name: "attach_disk to 777", op: "attach_disk", target: "777"},
		{name: "delete_disk", op: "delete_disk"},
	}
	for _, managed := range []bool{false, true} {
		for _, call := range calls {
			kind := "without journal/"
			if managed {
				kind = "managed/"
			}
			t.Run(kind+call.name, func(t *testing.T) {
				captureParkerPoolSweep(t)
				s, id := buildDeferredPark(t, managed)
				if call.target == "888" {
					s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
				}
				call.requireBlocked(t, s, id)
				call.requireBlocked(t, s, id)
				s.client.moveErr = nil
				s.client.vmSnapshots = nil
				call.requireConverged(t, s, id)
			})
		}
	}
}

// TestDeferredParkSnapshotBlockedWithoutListing covers a snapshot listing
// that fails while PVE refuses the move. The refusal keeps its permanent class
// and still names the unused entry, and it asks for the VM's snapshot to be
// deleted without naming it.
func TestDeferredParkSnapshotBlockedWithoutListing(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _ := buildDeferredPark(t, false)
	s.client.snapshotErr = errors.New("snapshot listing unavailable")
	before, deletes := s.snapshot(), s.client.deletes
	err := attachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	s.requireSnapshotBlocked(t, "attach_disk", err, "Delete the VM's snapshot that references the volume")
	s.requireUnchanged(t, before, deletes)
}

// TestDeferredParkSnapshotRefusalReturnsRecord covers the calls on a
// journal-managed disk whose park a snapshot deferred that resume the park
// before they act and keep their retriable answer when PVE refuses the move.
// The guard settled the move as refused, so each call changes nothing and the
// allocation goes back as ready_to_return rather than waiting for
// reconciliation. Once the snapshot is deleted, update_disk finishes the park.
func TestDeferredParkSnapshotRefusalReturnsRecord(t *testing.T) {
	calls := []struct {
		op  string
		run func(t *testing.T, s *strandedDisk) error
	}{
		{"update_disk", func(t *testing.T, s *strandedDisk) error {
			return callHandler(t, HandleUpdateDisk(s.deps), s.cid, map[string]any{"cache": "writeback"})
		}},
		{"resize_disk", func(t *testing.T, s *strandedDisk) error {
			return callHandler(t, HandleResizeDisk(s.deps), s.cid, 6144)
		}},
		{"snapshot_disk", func(t *testing.T, s *strandedDisk) error {
			return callHandler(t, HandleSnapshotDisk(s.deps), s.cid, map[string]any{})
		}},
		{"set_disk_metadata", func(t *testing.T, s *strandedDisk) error {
			return callHandler(t, HandleSetDiskMetadata(s.deps), s.cid, map[string]any{"director": "x"})
		}},
	}
	for _, call := range calls {
		t.Run(call.op, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id := buildDeferredPark(t, true)
			before, deletes := s.snapshot(), s.client.deletes
			err := call.run(t, s)
			if !pve.IsMoveDiskSnapshotRefusal(err) {
				t.Fatalf("%s while the snapshot blocks the park: %v, want PVE's snapshot refusal", call.op, err)
			}
			requireRetriable(t, err, call.op+" while the snapshot blocks the park")
			s.requireUnchanged(t, before, deletes)
			s.requireRecord(t, id, aj.ReadyToReturn)
			if call.op != "update_disk" {
				return
			}

			s.client.moveErr = nil
			s.client.vmSnapshots = nil
			if err := call.run(t, s); err != nil {
				t.Fatalf("update_disk after the snapshot was deleted: %v", err)
			}
			if refs := s.references(s.stranded); len(refs) != 0 {
				t.Fatalf("%s still names %s after the park finished", strings.Join(refs, ", "), s.stranded)
			}
			s.requireSingleHolder(t, true)
			s.requireRecord(t, id, aj.ReadyToReturn)
		})
	}
}

// TestDeferredParkSnapshotRefusalUnreadReceiverNeedsReconciliation covers a
// refused move whose second readback fails. PVE refuses the move onto the
// parker for the snapshot, and the guard reads the source back, but its read
// of the parker fails, so nothing shows that the receiving slot stayed empty.
// The move stays uncertain, the allocation waits at reconciliation_required,
// and the call's error doesn't say the disk came back unchanged.
func TestDeferredParkSnapshotRefusalUnreadReceiverNeedsReconciliation(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id := buildDeferredPark(t, true)
	parker := s.intentParker()
	s.client.moveErr = nil
	s.client.moveSnapshotRefusal = true
	refused, failed := false, 0
	s.client.beforeMoveCheck = func(int, *nodes.CreateQemuMoveDiskParams) { refused = true }
	s.client.onConfigRead = func(vmid int) error {
		if refused && vmid == parker && failed == 0 {
			failed++
			return errors.New("connection reset by peer")
		}
		return nil
	}
	err := callHandler(t, HandleUpdateDisk(s.deps), s.cid, map[string]any{"cache": "writeback"})
	if err == nil {
		t.Fatal("update_disk succeeded although PVE refused the move")
	}
	if failed != 1 {
		t.Fatalf("the parker's readback failed %d times, want once", failed)
	}
	if isDiskReturnedUnchanged(err) {
		t.Fatalf("update_disk = %v, marked as returned unchanged although the receiver went unread", err)
	}
	s.requireRecord(t, id, aj.ReconciliationRequired)
}

// TestDeferredParkSnapshotBlocksCreateVM covers create_vm's attach of a
// journal-managed disk whose park a snapshot deferred. The attach resumes the
// park first, and PVE refuses the move. The attach refuses permanently with
// the snapshot and the unused entry named and asks for create_vm to run
// again. It moves nothing, returns the allocation as ready_to_return, and
// marks its error as one that left the disk unchanged, so create_vm rolls its
// VM back without holding the disk's allocation for reconciliation.
func TestDeferredParkSnapshotBlocksCreateVM(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id := buildDeferredPark(t, true)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	bare, meta, err := decodeDiskCID(context.Background(), s.deps, "create_vm", s.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), s.deps, "create_vm", s.cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	before, deletes := s.snapshot(), s.client.deletes
	_, err = attachManagedPersistentDisk(context.Background(), s.deps, "888", "n1", 888, rd)
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeSnapshotBlocked || typed.OkToRetry() {
		t.Fatalf("create_vm's attach while the snapshot blocks the park: %v, want a permanent %s", err, cpierrors.TypeSnapshotBlocked)
	}
	requireText(t, err, "create_vm's attach",
		[]string{"create_vm.attach_disk: ", "unused0=" + s.stranded, "pre-upgrade", "then retry create_vm"}, "remove the")
	if !isDiskReturnedUnchanged(err) {
		t.Fatalf("create_vm's attach = %v, want it marked as returning the disk unchanged", err)
	}
	s.requireUnchanged(t, before, deletes)
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// TestDeferredParkSnapshotBlockedWithEmptyListing covers PVE's refusal of the
// move while the source VM lists no snapshot. Something else in the VM's
// configuration still names the volume, so the refusal says that PVE reports
// the volume in use by a snapshot, that the VM lists none, and that the VM's
// configuration needs checking for another reference.
func TestDeferredParkSnapshotBlockedWithEmptyListing(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _ := buildDeferredPark(t, false)
	s.client.vmSnapshots = map[int][]map[string]any{777: {{"name": "current"}}}
	before, deletes := s.snapshot(), s.client.deletes
	err := attachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	s.requireSnapshotBlocked(t, "attach_disk", err, "lists no snapshot")
	requireText(t, err, "attach_disk with an empty snapshot listing",
		[]string{"PVE reports unused entry unused0=" + s.stranded + " in use by a snapshot", "Check the VM's configuration for another reference"},
		"Delete the VM's snapshot", "Delete snapshot")
	s.requireUnchanged(t, before, deletes)
}

// TestDeferredParkSnapshotRefusedByTheCPI covers a deferred park whose source
// VM no longer names the volume while a snapshot of the VM still does. The
// resume would park the released volume by a config edit, and the CPI checks
// the snapshots first and declines without sending anything to PVE. The
// refusal says that the CPI declined, names the snapshot, and doesn't blame
// PVE for a move it never saw.
func TestDeferredParkSnapshotRefusedByTheCPI(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _ := buildDeferredPark(t, false)
	delete(s.client.state.configs[777], "unused0")
	s.client.snapshotConfigs = map[int]map[string]map[string]any{
		777: {"pre-upgrade": {"name": "w777", "scsi1": s.stranded + ",size=5G"}},
	}
	moves := s.client.moveCalls
	err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeSnapshotBlocked || typed.OkToRetry() {
		t.Fatalf("attach_disk while a snapshot names the released volume: %v, want a permanent %s", err, cpierrors.TypeSnapshotBlocked)
	}
	requireText(t, err, "attach_disk refused by the CPI",
		[]string{"the CPI found that the VM's snapshot pre-upgrade names volume " + s.stranded, "declined to park it before sending PVE anything",
			"Delete snapshot pre-upgrade", "then retry attach_disk"},
		"PVE won't move", "PVE reports")
	if s.client.moveCalls != moves {
		t.Fatalf("attach_disk sent %d move(s) although the CPI declined the park", s.client.moveCalls-moves)
	}
}

// TestDeferredParkSnapshotBlockAfterAppliedPendingDelete covers attach_disk's
// resume on a stopped source whose delete of the disk's slot was pending. The
// resume applies the delete, which leaves the volume on an unused entry, and
// then PVE refuses the move for a snapshot. The refusal says the pending
// delete of the slot was applied and that the volume stayed on the unused
// entry, because the source no longer holds the slot it had.
func TestDeferredParkSnapshotBlockAfterAppliedPendingDelete(t *testing.T) {
	captureParkerPoolSweep(t)
	deps, c, diskCID := pendingResumeFixture(t)
	c.pending.stop(700)
	c.moveErr, c.keepMoveErr = moveSnapshotRefusal(), true
	_, err := resumeTransfer(pendingRowContext(), deps, "attach_disk", resolveTransferDisk(t, deps, diskCID), false)
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeSnapshotBlocked || typed.OkToRetry() {
		t.Fatalf("attach_disk's resume after the applied delete: %v, want a permanent %s", err, cpierrors.TypeSnapshotBlocked)
	}
	requireText(t, err, "attach_disk's resume after the applied delete",
		[]string{"attach_disk: can't finish the deferred park", "unused0=" + pendingTransferVolid,
			"The resume applied the pending delete of slot scsi1 first, and the volume stayed on the unused entry that delete left.",
			"then retry attach_disk"})
	if got, _ := c.configs[700]["unused0"].(string); got != pendingTransferVolid {
		t.Fatalf("700's unused0 = %q, want %s", got, pendingTransferVolid)
	}
}

// TestDeferredParkSnapshotRefusalCompletionFails covers a refused move onto
// the parker that the guard settled while the audit that returns the
// allocation fails. PVE refuses the move for the snapshot, the guard reads
// both VMs back unchanged, and then a configuration read the completion makes
// fails. Nothing proves the disk's ownership afresh, so the allocation waits
// at reconciliation_required, the call's error doesn't say the disk came back
// unchanged, and the reconciliation leads over update_disk's retriable
// refusal. attach_disk's permanent refusal keeps its place in front.
func TestDeferredParkSnapshotRefusalCompletionFails(t *testing.T) {
	calls := []struct {
		op string
		// failAt is the configuration read after the refusal that fails.
		// The guard's readback makes the first two, and attach_disk's
		// refusal text makes the third.
		failAt  int
		run     func(t *testing.T, s *strandedDisk) error
		permits func(t *testing.T, err error)
	}{
		{"update_disk", 3, func(t *testing.T, s *strandedDisk) error {
			return callHandler(t, HandleUpdateDisk(s.deps), s.cid, map[string]any{"cache": "writeback"})
		}, func(t *testing.T, err error) {
			requirePermanent(t, err, "update_disk whose completion failed")
			if cpierrors.IsType(err, cpierrors.TypeSnapshotBlocked) {
				t.Fatalf("update_disk = %v, want the reconciliation to lead", err)
			}
		}},
		{"attach_disk", 4, func(t *testing.T, s *strandedDisk) error {
			return attachDiskAt(t, context.Background(), s.deps, "777", s.cid)
		}, func(t *testing.T, err error) {
			var typed *cpierrors.Error
			if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeSnapshotBlocked || typed.OkToRetry() {
				t.Fatalf("attach_disk whose completion failed = %v, want the permanent %s in front", err, cpierrors.TypeSnapshotBlocked)
			}
		}},
	}
	for _, call := range calls {
		t.Run(call.op, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id := buildDeferredPark(t, true)
			s.client.moveErr = nil
			s.client.moveSnapshotRefusal = true
			refused, reads := false, 0
			s.client.beforeMoveCheck = func(int, *nodes.CreateQemuMoveDiskParams) { refused = true }
			s.client.onConfigRead = func(int) error {
				if !refused {
					return nil
				}
				reads++
				if reads == call.failAt {
					return errors.New("connection reset by peer")
				}
				return nil
			}
			err := call.run(t, s)
			if err == nil {
				t.Fatalf("%s succeeded although PVE refused the move", call.op)
			}
			requireText(t, err, call.op+" whose completion failed",
				[]string{"completion audit after a snapshot refused the move to a parker failed"})
			if isDiskReturnedUnchanged(err) {
				t.Fatalf("%s = %v, marked as returned unchanged although its completion failed", call.op, err)
			}
			call.permits(t, err)
			s.requireRecord(t, id, aj.ReconciliationRequired)
		})
	}
}

// TestDeferredParkCPIRefusalOnAManagedDisk covers a journal-managed disk whose
// source VM no longer names the volume while a snapshot of the VM still does.
// The CPI declines the park itself before it sends PVE anything, so no guard
// settles a refused move, and the allocation waits at reconciliation_required
// after each call. Once the snapshot is gone, the next attach_disk finishes the
// park, attaches the disk, and returns the allocation as ready_to_return.
func TestDeferredParkCPIRefusalOnAManagedDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id := buildDeferredPark(t, true)
	delete(s.client.state.configs[777], "unused0")
	s.client.snapshotConfigs = map[int]map[string]map[string]any{
		777: {"pre-upgrade": {"name": "w777", "scsi1": s.stranded + ",size=5G"}},
	}
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	moves := s.client.moveCalls
	for attempt := 1; attempt <= 2; attempt++ {
		err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
		var typed *cpierrors.Error
		if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeSnapshotBlocked || typed.OkToRetry() {
			t.Fatalf("attach_disk %d while the snapshot names the released volume: %v, want a permanent %s", attempt, err, cpierrors.TypeSnapshotBlocked)
		}
		requireText(t, err, "attach_disk refused by the CPI",
			[]string{"declined to park it before sending PVE anything", "Delete snapshot pre-upgrade", "then retry attach_disk"})
		s.requireRecord(t, id, aj.ReconciliationRequired)
	}
	if s.client.moveCalls != moves {
		t.Fatalf("attach_disk sent %d move(s) although the CPI declined the park", s.client.moveCalls-moves)
	}

	s.client.snapshotConfigs = nil
	s.client.vmSnapshots = nil
	s.client.moveErr = nil
	call := deferredParkCall{name: "attach_disk to 888 after the snapshot was deleted", op: "attach_disk", target: "888"}
	call.requireConverged(t, s, id)
}
