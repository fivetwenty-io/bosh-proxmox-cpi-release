package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// journalPathFault fails every write of allocation id that settles step, as
// a journal on a full filesystem would, and leaves every other write alone.
// The flag it returns reports whether such a write was attempted.
func journalPathFault(ctx context.Context, id, step string) (context.Context, *bool) {
	fired := new(bool)
	return aj.WithSaveFaultForTest(ctx, func(next aj.Record) error {
		for i := range next.Steps {
			if next.ID == id && next.Steps[i].ID == step && next.Steps[i].State == aj.Observed {
				*fired = true
				return &fs.PathError{Op: "rename", Path: "/var/vcap/store/pve_cpi/journal/allocation-" + id + ".json", Err: syscall.ENOSPC}
			}
		}
		return nil
	}), fired
}

// TestStorageAllocationDecisionFailureNamesRefusedSettlementSteps makes the
// journal refuse a settlement write and reads the CLI's text for it. The text
// names the step the write was settling, its kind, and the journal's own
// class of failure, where it used to say "unclassified error". A lock step and
// a parker protection step both name themselves.
func TestStorageAllocationDecisionFailureNamesRefusedSettlementSteps(t *testing.T) {
	t.Run("a lock step the journal could not write", func(t *testing.T) {
		deps, _, journal, id, step := crashedLockStepDisk(t)
		ctx, _ := journalPathFault(t.Context(), id, step)
		_, err := CleanupStorageAllocation(ctx, deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: id, DecisionID: "refused-settlement"})
		want := "cleanup_lock_step_settlement: the journal refused to save the lock settlement of step " + step +
			" (lifecycle_attach_disk_Pool_CreatePool): rename /var/vcap/store/pve_cpi/journal/allocation-" + id + ".json: no space left on device"
		if got := StorageAllocationDecisionFailure(err); got != want {
			t.Fatalf("CLI text = %q, want %q", got, want)
		}
	})
	t.Run("a lock step on a closed handle", func(t *testing.T) {
		deps, _, journal, id, step := crashedLockStepDisk(t)
		handle, err := journal.Acquire(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = settlePlannedLockSteps(t.Context(), deps.PVE, handle)
		want := "identity_or_audit_evidence: the journal refused to save the lock settlement of step " + step +
			" (lifecycle_attach_disk_Pool_CreatePool): journal allocation handle is closed"
		if got := StorageAllocationDecisionFailure(storageDecisionSourceError(err)); got != want {
			t.Fatalf("CLI text = %q, want %q", got, want)
		}
	})
	t.Run("a parker protection step", func(t *testing.T) {
		c := newCutOffRestore(t, 200*time.Millisecond)
		step := c.restoreStep(t)
		c.setProtection(true)
		ctx := aj.WithSaveFaultForTest(c.ctx, func(next aj.Record) error {
			for i := range next.Steps {
				if next.Steps[i].ID == step.ID && next.Steps[i].State == aj.Observed {
					return aj.ErrReconciliationRequired
				}
			}
			return nil
		})
		_, err := ApplyStorageAllocationDecision(ctx, c.deps, c.journal, []string{"n1"},
			StorageAllocationDecision{Action: "adopt", AllocationID: c.id, ExpectedCID: c.cid, DecisionID: "refused-protection-settlement"})
		want := "identity_or_audit_evidence: the journal refused to save the parker protection settlement of step " + step.ID +
			" (lifecycle_attach_disk_Nodes_UpdateQemuConfig): allocation requires reconciliation"
		if got := StorageAllocationDecisionFailure(err); got != want {
			t.Fatalf("CLI text = %q, want %q", got, want)
		}
	})
}

// TestRefusedSettlementSaveDescribesADurabilityFailure makes the journal
// refuse a lock settlement write with a durability failure around a full
// filesystem. The CLI describes the journal's error exactly as it describes
// that error everywhere else, durability lead included.
func TestRefusedSettlementSaveDescribesADurabilityFailure(t *testing.T) {
	deps, _, journal, id, step := crashedLockStepDisk(t)
	path := "/var/vcap/store/pve_cpi/journal/.tmp-allocation-" + id + ".json"
	ctx := aj.WithSaveFaultForTest(t.Context(), func(next aj.Record) error {
		for i := range next.Steps {
			if next.Steps[i].ID == step && next.Steps[i].State == aj.Observed {
				return &aj.DurabilityError{Err: &fs.PathError{Op: "rename", Path: path, Err: syscall.ENOSPC}}
			}
		}
		return nil
	})
	_, err := CleanupStorageAllocation(ctx, deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: id, DecisionID: "durability-settlement"})
	want := "cleanup_lock_step_settlement: the journal refused to save the lock settlement of step " + step +
		" (lifecycle_attach_disk_Pool_CreatePool): journal durability failure; reconcile before mutation (rename " + path + ": no space left on device)"
	if got := StorageAllocationDecisionFailure(err); got != want {
		t.Fatalf("CLI text = %q, want %q", got, want)
	}
}

// TestRefusedProtectionSettlementNamesItsSteps is the protection settler's
// side of TestRefusedSettlementNamesItsSteps. The journal refuses the write
// that settles a parker protection restore, and the error names the step it
// was settling and keeps the journal's own error in the chain.
func TestRefusedProtectionSettlementNamesItsSteps(t *testing.T) {
	c := newCutOffRestore(t, 200*time.Millisecond)
	step := c.restoreStep(t)
	c.setProtection(true)
	handle, err := c.journal.Acquire(c.ctx, c.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = settlePlannedLockSteps(c.ctx, c.deps.PVE, handle)
	if !errors.Is(err, aj.ErrClosed) || !strings.Contains(err.Error(), "settling parker protection step "+step.ID+" (lifecycle_attach_disk_Nodes_UpdateQemuConfig)") {
		t.Fatalf("the refused protection settlement did not name its step: %v", err)
	}
}

// TestRefusedSettlementSaveKeepsTheDirectorsGenericLine sends a delete_vm
// whose settlement write the journal refuses through the production
// dispatcher. The CLI names the step, but the Director must keep the generic
// evidence line for a journal write that failed, so no journal path reaches
// it, and the error keeps its type and retry flag.
func TestRefusedSettlementSaveKeepsTheDirectorsGenericLine(t *testing.T) {
	deps, client, journal, vmID := deleteWindowFixture(t, false)
	planned := plantVMLockStep(t, journal, vmID)
	deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: newLockContention(t)}
	d := cpi.NewDispatcher(log.NewNopLogger())
	if err := d.Register("delete_vm", HandleDeleteVM(deps)); err != nil {
		t.Fatal(err)
	}
	ctx, fired := journalPathFault(context.Background(), vmID, planned)
	body := d.Handle(ctx, &jsonrpc.Request{Method: "delete_vm", Arguments: []json.RawMessage{planJSON(t, "777")}}).Error
	if !*fired {
		t.Fatal("delete_vm never wrote the settlement of its lock step")
	}
	if body == nil || body.Type != string(cpierrors.TypeCloud) || body.OkToRetry || body.Message != "allocation decision could not verify or persist evidence" {
		t.Fatalf("Director received %+v, want the generic evidence line", body)
	}
}

// plantVMLockStep leaves VM 777's record the way the VM guard leaves one after
// a pool call it could not classify: in reconciliation_required with a planned
// vm.Pool.CreatePool. It returns the step's ID.
func plantVMLockStep(t *testing.T, journal *aj.Journal, vmID string) string {
	t.Helper()
	handle, err := journal.Acquire(t.Context(), vmID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	record := handle.Record()
	record.State = aj.ReconciliationRequired
	record.Reason = "VM Pool call"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	id, err := storageMutationIntent(handle, "vm.Pool.CreatePool", aj.Target{Node: "n1", VMID: 777}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestDeleteVMRefusalSaysWhyItsLockStepStayedPlanned fails the sentinel read
// that would settle a VM's planned lock step and runs delete_vm through the
// production dispatcher. The Director's refusal names the step and says why
// settlement left it planned.
func TestDeleteVMRefusalSaysWhyItsLockStepStayedPlanned(t *testing.T) {
	deps, client, journal, vmID := deleteWindowFixture(t, false)
	planned := plantVMLockStep(t, journal, vmID)
	locks := newLockContention(t)
	locks.readErr = poolVerdictError("permission check failed for /pool/bosh-lock-vm-777 (Pool.Audit)")
	deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks}
	d := cpi.NewDispatcher(log.NewNopLogger())
	if err := d.Register("delete_vm", HandleDeleteVM(deps)); err != nil {
		t.Fatal(err)
	}
	body := d.Handle(context.Background(), &jsonrpc.Request{Method: "delete_vm", Arguments: []json.RawMessage{planJSON(t, "777")}}).Error
	want := "cleanup has unresolved mutation evidence; step " + planned + " (vm.Pool.CreatePool) is planned; its lock sentinel could not be settled because "
	if body == nil || body.Type != string(cpierrors.TypeCloud) || body.OkToRetry || !strings.HasPrefix(body.Message, want) {
		t.Fatalf("Director received %+v, want a CloudError starting %q", body, want)
	}
	t.Logf("refusal: %s", body.Message)
}

// TestCleanupRefusalNamesTheStepItRefused runs plain cleanup on records whose
// admission refuses a step it cannot settle. The refusal names that step and
// its state. When an unadmitted step comes ahead of a lock step that
// settlement left planned, the refusal names the step it refused on, not the
// lock step.
func TestCleanupRefusalNamesTheStepItRefused(t *testing.T) {
	const head = "cleanup_pending_mutation_settlement: cleanup refuses unresolved allocation or asynchronous mutation; "
	t.Run("a submitted step", func(t *testing.T) {
		var submitted string
		deps, _, journal, id, _ := lifecycleFlowFixtureWith(t, false, false, func(h *aj.Handle) {
			birth := h.Record().Steps[0].Target
			var err error
			submitted, err = storageMutationIntent(h, "lifecycle_resize_disk_Nodes_ResizeQemuDisk", aj.Target{Node: birth.Node, Storage: birth.Storage, Backing: birth.Backing, VMID: 777, IntendedVolume: birth.IntendedVolume}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := storageMutationSubmitted(h, submitted, "UPID:n1:00001234:00005678:65000000:resize:777:root@pam:"); err != nil {
				t.Fatal(err)
			}
		})
		_, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: id, DecisionID: "submitted-step"})
		want := head + "step " + submitted + " (lifecycle_resize_disk_Nodes_ResizeQemuDisk) is submitted"
		if got := StorageAllocationDecisionFailure(err); got != want {
			t.Fatalf("CLI text = %q, want %q", got, want)
		}
	})
	t.Run("an unadmitted step ahead of an unsettled lock step", func(t *testing.T) {
		var attach, lock string
		deps, client, journal, id, _ := lifecycleFlowFixtureWith(t, false, false, func(h *aj.Handle) {
			birth := h.Record().Steps[0].Target
			target := aj.Target{Node: birth.Node, Storage: birth.Storage, Backing: birth.Backing, VMID: 777, IntendedVolume: birth.IntendedVolume}
			var err error
			if attach, err = storageMutationIntent(h, "lifecycle_attach_disk_QEMU_AttachDisk", target, nil); err != nil {
				t.Fatal(err)
			}
			if lock, err = storageMutationIntent(h, "lifecycle_attach_disk_Pool_CreatePool", target, nil); err != nil {
				t.Fatal(err)
			}
		})
		locks := newLockContention(t)
		locks.readErr = poolVerdictError("permission check failed (Pool.Audit)")
		deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks}
		_, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: id, DecisionID: "unadmitted-step"})
		want := head + "step " + attach + " (lifecycle_attach_disk_QEMU_AttachDisk) is planned"
		if got := StorageAllocationDecisionFailure(err); got != want || strings.Contains(got, lock) {
			t.Fatalf("CLI text = %q, want %q without lock step %s", got, want, lock)
		}
	})
}

// TestCleanupFencingRefusalNamesStepAndMissingFlags runs cleanup on records
// whose every open step the admission accepts, without the attestations that
// step needs. The refusal keeps its head, names the first step it admitted,
// and names the storage-journal flags the decision is missing.
func TestCleanupFencingRefusalNamesStepAndMissingFlags(t *testing.T) {
	const head = "cleanup_pending_mutation_settlement: pending mutation cleanup requires explicit writer fencing and independently settled remote tasks; step "
	t.Run("the persistent disk handoff under a plain decision", func(t *testing.T) {
		// Stays serial: it swaps the package variable attachExistingDiskForVM.
		locks := newLockContention(t)
		deps, _, journal, cid, parker := createVMDiskFixture(t, locks, true)
		locks.reset()
		plantHeldParkerLock(locks, parker)
		ctx := shortenManagedLockWait(t.Context(), testManagedLockWait)
		failPersistentHandoff(t, func(slot string, err error) (string, error) {
			if err != nil {
				return slot, errors.New(err.Error())
			}
			return slot, nil
		})
		if _, err := createVM(ctx, deps, createVMArgs(t, cid)); err == nil {
			t.Fatal("create_vm succeeded behind a held parker lock")
		}
		vm := handoffRecord(t, journal)
		var handoff aj.Step
		for i := range vm.Steps {
			if vm.Steps[i].State != aj.Observed {
				handoff = vm.Steps[i]
			}
		}
		locks.reset()
		_, err := CleanupStorageAllocation(ctx, attestedCleanupDeps(deps), journal, []string{"n1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: vm.ID, DecisionID: "plain-handoff"})
		want := head + handoff.ID + " (" + handoff.Kind + ") is planned; rerun with --previous-writer-fenced, --remote-tasks-settled, and --authority-id"
		if got := StorageAllocationDecisionFailure(err); got != want || !strings.HasPrefix(handoff.Kind, managedVMPersistentHandoffPrefix) {
			t.Fatalf("CLI text = %q, want %q", got, want)
		}
	})
	t.Run("a pending configuration missing only its authority", func(t *testing.T) {
		deps, j, _, record := deleteManagedFixture(t)
		appendCleanupPending(t, j, record.ID, "vm.Nodes.UpdateQemuConfig")
		decision := cleanupAttestedDecision(record.ID)
		decision.AuthorityID = ""
		_, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision)
		want := head + "pending-config (vm.Nodes.UpdateQemuConfig) is planned; rerun with --authority-id"
		if got := StorageAllocationDecisionFailure(err); got != want {
			t.Fatalf("CLI text = %q, want %q", got, want)
		}
	})
}

// TestChangedUnsettledEvidenceRefusalNamesItsStep pins that the refusal for a
// step that changed after cleanup admitted it, or that appeared since, names
// that step the way every other unsettled-evidence refusal does.
func TestChangedUnsettledEvidenceRefusalNamesItsStep(t *testing.T) {
	r := aj.Record{ID: "allocation", Kind: "vm", Steps: []aj.Step{{ID: "pending", Kind: "vm.Nodes.UpdateQemuConfig", State: aj.Planned}}}
	hash, err := aj.Fingerprint(r.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), cleanupSettlementKey{}, &cleanupSettlement{AllocationID: r.ID, Steps: map[string]string{"pending": hash}})
	r.Steps[0].UPID = "unknown"
	r.Steps[0].State = aj.Submitted
	if err := storageCleanupSettled(ctx, r); err == nil || err.Error() != "cleanup has new or changed unresolved mutation evidence; step pending (vm.Nodes.UpdateQemuConfig) is submitted" {
		t.Fatalf("changed step refusal = %v", err)
	}
	r.Steps[0].UPID, r.Steps[0].State = "", aj.Planned
	r.Steps = append(r.Steps, aj.Step{ID: "new", Kind: "vm.Cluster.UpdateHaResources", State: aj.Planned})
	if err := storageCleanupSettled(ctx, r); err == nil || err.Error() != "cleanup has new or changed unresolved mutation evidence; step new (vm.Cluster.UpdateHaResources) is planned" {
		t.Fatalf("new step refusal = %v", err)
	}
}
