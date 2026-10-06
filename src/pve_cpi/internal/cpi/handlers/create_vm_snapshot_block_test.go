package handlers

import (
	"context"
	"errors"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// requireCreateVMSnapshotBlocked fails unless err is the permanent snapshot
// refusal op gives for s's disk, naming the snapshot and 777's unused entry
// and asking for create_vm to run again.
func requireCreateVMSnapshotBlocked(t *testing.T, s *strandedDisk, op string, err error) {
	t.Helper()
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeSnapshotBlocked || typed.OkToRetry() {
		t.Fatalf("%s while the snapshot blocks the park: %v, want a permanent %s", op, err, cpierrors.TypeSnapshotBlocked)
	}
	requireText(t, err, op,
		[]string{op + ": ", "unused0=" + s.stranded, "pre-upgrade", "then retry create_vm"}, "remove the", "then retry "+op+" ")
}

// TestDeferredParkSnapshotBlocksCreateVMWithoutJournal covers create_vm's
// attach on a CPI that runs without a journal, of a disk whose park a
// snapshot deferred. The attach resumes the park first, and PVE refuses the
// move. The attach refuses permanently with the snapshot and the unused entry
// named, asks for create_vm to run again, and moves nothing.
func TestDeferredParkSnapshotBlocksCreateVMWithoutJournal(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _ := buildDeferredPark(t, false)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	before, deletes := s.snapshot(), s.client.deletes
	ctx := context.Background()
	err := attachPersistentDisks(ctx, s.deps, s.deps.Log(ctx), &createVMParsedArgs{diskCIDs: []string{s.cid}}, &createVMShape{node: "n1"}, 888)
	requireCreateVMSnapshotBlocked(t, s, "create_vm", err)
	s.requireUnchanged(t, before, deletes)
}

// TestDeferredParkSnapshotBlocksCreateVMLegacyAttach covers create_vm's
// attach of a disk without a journal under a journal-managed VM, whose park a
// snapshot deferred. The VM's record already holds the handoff step create_vm
// plans before the attach. The attach resumes the park, PVE refuses the move,
// and the guard reads both VMs back unchanged. The attach refuses permanently
// and asks for create_vm to run again, and it marks its error as one that
// left the disk unchanged, so create_vm settles the handoff and rolls its VM
// back without holding the VM's allocation for reconciliation.
func TestDeferredParkSnapshotBlocksCreateVMLegacyAttach(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _ := buildDeferredPark(t, false)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	ctx := context.Background()
	handle := snapshotBlockVMHandle(t, s, 888)
	if _, err := storageMutationIntent(handle, "vm.persistent.example", aj.Target{Node: "n1", VMID: 888}, nil); err != nil {
		t.Fatal(err)
	}
	bare, meta, err := decodeDiskCID(ctx, s.deps, "create_vm", s.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(ctx, s.deps, "create_vm", s.cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if rd.allocation != nil {
		t.Fatal("the disk resolved with an allocation, want one without a journal")
	}
	before, deletes := s.snapshot(), s.client.deletes
	_, err = attachExistingDiskToManagedVM(ctx, s.deps, handle, rd, "n1", 888)
	requireCreateVMSnapshotBlocked(t, s, "create_vm.attach_existing", err)
	if !isDiskReturnedUnchanged(err) {
		t.Fatalf("create_vm's legacy attach = %v, want it marked as returning the disk unchanged", err)
	}
	s.requireUnchanged(t, before, deletes)
	if record := handle.Record(); record.State == aj.ReconciliationRequired {
		t.Fatalf("a settled snapshot refusal demanded reconciliation of the VM: %s", record.Reason)
	}
}

// snapshotBlockVMHandle is a journal-managed VM allocation for vmid in s's
// journal, with its root observed, the way create_vm holds it when it
// attaches the persistent disks.
func snapshotBlockVMHandle(t *testing.T, s *strandedDisk, vmid int) *aj.Handle {
	t.Helper()
	records, err := s.journal.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("the journal holds no record to take an intent from")
	}
	handle, err := s.journal.AcquireVM(t.Context(), "vm-agent", records[0].Intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	})
	step, err := storageMutationIntent(handle, "vm_create", aj.Target{Node: "n1", VMID: vmid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, nil, false); err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State = aj.Observed
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	return handle
}

// TestCreateVMSnapshotBlockSkipsTheFallback covers a journal-managed create_vm
// that may fall back to another placement, whose persistent disk refuses
// permanently because a snapshot blocks its deferred park and comes back
// unchanged. The snapshot sits on the VM that holds the disk, so another
// placement can't clear it. create_vm builds one VM, rolls it back, closes
// the generation, and hands the refusal back without a fallback attempt.
func TestCreateVMSnapshotBlockSkipsTheFallback(t *testing.T) {
	// Stays serial: it swaps the package variable attachExistingDiskForVM.
	locks := newLockContention(t)
	deps, client, journal, cid, _ := createVMDiskFixture(t, locks, false)
	locks.reset()
	fallback := 1
	deps.Config.Placement.FallbackMax = &fallback
	blocked := cpierrors.SnapshotBlocked("create_vm.attach_disk: can't finish the deferred park of disk %s from VM 777; Delete snapshot pre-upgrade, then retry create_vm", cid)
	withDiskAttachOutcome(t, &diskReturnedAfterSnapshotRefusal{err: blocked})

	_, err := createVM(t.Context(), deps, createVMArgs(t, cid))
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeSnapshotBlocked || typed.OkToRetry() {
		t.Fatalf("create_vm while a snapshot blocks its disk = %v, want the permanent %s", err, cpierrors.TypeSnapshotBlocked)
	}
	if client.creates != 1 || client.destroys != 1 {
		t.Fatalf("create_vm built %d VM(s) and destroyed %d, want one of each and no fallback attempt", client.creates, client.destroys)
	}
	if _, found, err := journal.InspectVM("disk-agent"); err != nil || found {
		t.Fatalf("the rollback left a VM generation for the retry to resume: found=%t err=%v", found, err)
	}
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		if records[i].Kind != "vm" {
			continue
		}
		if records[i].State != aj.Deleted {
			t.Fatalf("the rolled-back VM generation is %s (reason %q), want %s", records[i].State, records[i].Reason, aj.Deleted)
		}
		if len(records[i].Attempts) > 1 {
			t.Fatalf("the VM generation recorded %d attempts, want one", len(records[i].Attempts))
		}
		assertStepsObserved(t, "VM", records[i])
	}
}

// TestCreateVMSnapshotRefusalWithRetriableTextSkipsTheFallback covers a
// settled refusal that keeps PVE's own retriable text and carries the marker
// without the permanent class, which is what a deferred park without a source
// VM returns. The snapshot still sits on the VM that holds the disk, so
// create_vm builds one VM, rolls it back, and makes no fallback attempt.
func TestCreateVMSnapshotRefusalWithRetriableTextSkipsTheFallback(t *testing.T) {
	// Stays serial: it swaps the package variable attachExistingDiskForVM.
	locks := newLockContention(t)
	deps, client, _, cid, _ := createVMDiskFixture(t, locks, false)
	locks.reset()
	fallback := 1
	deps.Config.Placement.FallbackMax = &fallback
	raw := cpierrors.WrapAs(moveSnapshotRefusal(), cpierrors.TypeRetriableCloud, "create_vm.attach_disk: resume interrupted transfer of disk "+cid)
	withDiskAttachOutcome(t, &diskReturnedAfterSnapshotRefusal{err: raw})

	_, err := createVM(t.Context(), deps, createVMArgs(t, cid))
	if err == nil {
		t.Fatal("create_vm succeeded while a snapshot blocks its disk")
	}
	if client.creates != 1 {
		t.Fatalf("create_vm built %d VMs, want one and no fallback attempt", client.creates)
	}
}
