package handlers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// fingerprints returns the fingerprint of each named record.
func (f *protectionPendingFlow) fingerprints(t *testing.T, ids ...string) []string {
	t.Helper()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		record, err := f.parked.journal.Inspect(id)
		if err != nil {
			t.Fatal(err)
		}
		fp, err := aj.Fingerprint(record)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fp)
	}
	return out
}

// boundSlot names the slot on vmid that holds the parked disk's volume, or ""
// when none does.
func (f *protectionPendingFlow) boundSlot(t *testing.T, vmid int) string {
	t.Helper()
	cfg, ok := f.parked.client.state.configs[vmid]
	if !ok {
		return ""
	}
	bare, meta, err := decodeDiskCID(t.Context(), f.parked.deps, "test", f.parked.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(t.Context(), f.parked.deps, "test", f.parked.cid, bare, meta)
	if err != nil {
		t.Fatalf("resolving the parked disk: %v", err)
	}
	for key := range cfg {
		value, _ := pve.ConfigString(cfg, key)
		if isDiskOptionKey(key) && strings.Split(value, ",")[0] == rd.volid {
			return key
		}
	}
	return ""
}

// TestExhaustedTriesRecoverWithPlainCleanup covers a create_vm whose every
// try ran out with the parker still unprotected. The Director never got the
// VM's CID, so no delete_vm comes for it, and the next deploy runs under a new
// agent ID. A plain cleanup of the VM record is the way out. While protection
// is off, it is refused with the qm set command and changes nothing. Once
// protection is back, it preserves the bound disk through the disk's own
// lifecycle, whose admission settles the restore step, and removes the VM, and
// a create_vm under a new agent ID then attaches the disk.
//
// The create attaches only the parked disk. With a second disk on the same
// parker, cleanup would preserve that disk first, and that preservation's own
// window puts the parker's protection back before the parked disk's turn, so
// the refusal this test pins would never be reached.
func TestExhaustedTriesRecoverWithPlainCleanup(t *testing.T) {
	flow := newProtectionPendingFlow(t, false)
	disks, err := json.Marshal([]string{flow.parked.cid})
	if err != nil {
		t.Fatal(err)
	}
	flow.args[4] = disks
	generation, vmid := flow.cutOff(t)
	flow.setParkerProtection(false)
	cleanupDeps := attestedCleanupDeps(flow.parked.deps)
	plain := StorageAllocationDecision{Action: "cleanup", AllocationID: generation.ID, DecisionID: "exhausted-tries"}

	slot := flow.boundSlot(t, vmid)
	if slot == "" {
		t.Fatalf("the parked disk is not bound to VM %d before cleanup", vmid)
	}
	diskBefore := flow.fingerprints(t, flow.parked.id)[0]
	vmStepsBefore, err := aj.Fingerprint(generation.Steps)
	if err != nil {
		t.Fatal(err)
	}
	_, err = CleanupStorageAllocation(flow.ctx, cleanupDeps, flow.parked.journal, []string{"n1"}, plain)
	// The CLI prints the refusal's class and then its reason, which is where
	// the settler's text lands.
	want := fmt.Sprintf("run qm set %d --protection 1", flow.parked.parker)
	if err == nil || !strings.Contains(StorageAllocationDecisionFailure(err), want) {
		t.Fatalf("plain cleanup with protection off = %v (%s), want a refusal containing %q", err, StorageAllocationDecisionFailure(err), want)
	}
	t.Logf("refusal: %v (%s)", err, StorageAllocationDecisionFailure(err))
	if _, exists := flow.parked.client.state.configs[vmid]; !exists || flow.boundSlot(t, vmid) != slot {
		t.Fatalf("the refused cleanup touched VM %d or its binding at %s", vmid, slot)
	}
	if flow.fingerprints(t, flow.parked.id)[0] != diskBefore {
		t.Fatal("the refused cleanup changed the disk's record")
	}
	// The VM record keeps every step it had and gains none. The cleanup's
	// own admission still marks it as needing reconciliation, which the
	// cleanup below then closes.
	kept := flow.generation(t)
	vmStepsAfter, err := aj.Fingerprint(kept.Steps)
	if err != nil {
		t.Fatal(err)
	}
	if vmStepsAfter != vmStepsBefore {
		t.Fatalf("the refused cleanup changed the VM record's steps: %d before, %d after", len(generation.Steps), len(kept.Steps))
	}
	if kept.State != aj.ReconciliationRequired {
		t.Fatalf("the VM record after the refused cleanup is %s (reason %q), want %s", kept.State, kept.Reason, aj.ReconciliationRequired)
	}

	flow.setParkerProtection(true)
	cleaned, err := CleanupStorageAllocation(flow.ctx, cleanupDeps, flow.parked.journal, []string{"n1"}, plain)
	if err != nil {
		t.Fatalf("plain cleanup after protection was put back: %v (%s)", err, StorageAllocationDecisionFailure(err))
	}
	if cleaned.State != aj.Cleaned {
		t.Fatalf("cleanup left the VM record %s", cleaned.State)
	}
	if _, exists := flow.parked.client.state.configs[vmid]; exists {
		t.Fatalf("cleanup left VM %d on PVE", vmid)
	}
	assertReturnedRecord(t, "parked disk", flow.parked.record(t))
	if holder := flow.holderOf(t, flow.parked.cid); holder == nil || !holder.IsParker || holder.VMID != flow.parked.parker {
		t.Fatalf("the disk is not on its parker after cleanup: %+v", holder)
	}

	args := append([]json.RawMessage(nil), flow.args...)
	args[0] = json.RawMessage(`"exhausted-tries-next-deploy"`)
	result, err := createVM(flow.ctx, flow.parked.deps, args)
	if err != nil {
		t.Fatalf("create_vm under a new agent ID failed: %v", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("unexpected create_vm result %v", result)
	}
	fresh, err := strconv.Atoi(fmt.Sprint(values[0]))
	if err != nil || fresh == vmid {
		t.Fatalf("create_vm under a new agent ID returned %v, want a fresh VM", values[0])
	}
	if holder := flow.holderOf(t, flow.parked.cid); holder == nil || holder.VMID != fresh {
		t.Fatalf("the disk is not attached to the fresh VM %d: %+v", fresh, holder)
	}
}
