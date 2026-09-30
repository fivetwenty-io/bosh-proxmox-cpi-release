package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// These tests cover the lifecycle paths that consult an audit's observed
// moves. Each moves a VM between nodes behind the CPI's back, the way a
// rolling patch cycle does, and checks that the path accepts the move on
// shared storage and refuses it when node-local storage is involved.

// TestManagedVMDeleteRetainsEphemeralOfVMMovedOnSharedStorage moves VM 777
// from n1, where its ephemeral disk was recorded, to n2 on shared storage.
// The delete audit accepts the move, so retention accepts the recorded step.
func TestManagedVMDeleteRetainsEphemeralOfVMMovedOnSharedStorage(t *testing.T) {
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	client.vmNodes = map[int]string{777: "n2"}
	before, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete of a VM moved on shared storage refused: %v", err)
	}
	after, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != aj.VMDeletedRetained || client.moves != 1 || client.state.configs[777] != nil {
		t.Fatalf("moved retain delete incomplete: state=%s moves=%d", after.State, client.moves)
	}
	target, err := managedVMRetainedTarget(after, strings.Split(retainedVolume(t, client), ",")[0], 777)
	if err != nil || target.Node != "n2" {
		t.Fatalf("retained target = %+v, %v; want the parker on n2", target, err)
	}
	// The recorded step keeps its node. The move is not written back.
	if !reflect.DeepEqual(after.Steps[0], before.Steps[0]) {
		t.Fatalf("recorded step rewritten:\nbefore %+v\nafter  %+v", before.Steps[0], after.Steps[0])
	}
}

// TestManagedVMDeleteRefusesEphemeralMovedWithLocalStorage moves VM 777 and
// its node-local ephemeral disk to n2. The delete audit refuses the move, so
// delete_vm stops before any mutation and leaves the journal as it was.
func TestManagedVMDeleteRefusesEphemeralMovedWithLocalStorage(t *testing.T) {
	deps, client, journal, vmID, _ := retainDeleteFixture(t, func(c *lifecycleFlowPVE) {
		c.localStorage = true
		c.volumeNodes = map[string]string{}
	})
	client.vmNodes = map[int]string{777: "n2"}
	for volume := range client.volumeNodes {
		client.volumeNodes[volume] = "n2"
	}
	files := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	if message := directorMessage(err); !strings.HasPrefix(message, "VM cleanup refused") || !strings.Contains(message, "not a move: volume a:777/vm-777-ephemeral-0.raw is node-local") {
		t.Fatalf("node-local move not refused by the delete audit: %q", message)
	}
	if client.moves != 0 || client.state.configs[777] == nil {
		t.Fatalf("refused delete mutated PVE: moves=%d", client.moves)
	}
	if !reflect.DeepEqual(files, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("refused delete changed the journal")
	}
	if after, err := journal.Inspect(vmID); err != nil || after.State != aj.ReadyToReturn {
		t.Fatalf("refused delete changed the record: %+v %v", after.State, err)
	}
}

// TestManagedVMEphemeralRetentionRequiresRecordedNodeWithoutMove pins that
// retention still refuses a step recorded on another node when the caller's
// audit accepted no move.
func TestManagedVMEphemeralRetentionRequiresRecordedNodeWithoutMove(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	handle, err := journal.AcquireVM(context.Background(), "vm-agent", record.Intent)
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
	client.vmNodes = map[int]string{777: "n2"}
	if _, err := retainManagedEphemeralForVMDelete(context.Background(), deps, handle, "n2", 777, volume, false); err == nil || !strings.Contains(err.Error(), "exact recorded VM volume ownership") {
		t.Fatalf("step on another node accepted without a move: %v", err)
	}
	if client.moves != 0 {
		t.Fatal("refused retention mutated PVE")
	}
}

// TestRetainedEphemeralCleanupStaysStrictWhenItsParkerMoved pins the one
// parker reader that does not follow a move. A retention parker's entry
// carries no allocation ID, so the audit's move rules never see it, and
// cleanup of the retained volume keeps requiring the recorded parker node.
func TestRetainedEphemeralCleanupStaysStrictWhenItsParkerMoved(t *testing.T) {
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	retained, err := journal.Inspect(vmID)
	if err != nil || retained.State != aj.VMDeletedRetained {
		t.Fatalf("retain delete: %+v %v", retained.State, err)
	}
	volume := strings.Split(retainedVolume(t, client), ",")[0]
	target, err := managedVMRetainedTarget(retained, volume, 777)
	if err != nil || target.Node != "n1" {
		t.Fatalf("retained target = %+v, %v", target, err)
	}
	client.vmNodes = map[int]string{target.VMID: "n2"}
	_, err = CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1", "n2"}, cleanupAttestedDecision(vmID))
	if err == nil {
		t.Fatal("retained cleanup followed a moved retention parker")
	}
	// The runbook quotes this class for a moved retention parker.
	if failure := StorageAllocationDecisionFailure(err); !strings.HasPrefix(failure, "cleanup_resource_cleanup: ") {
		t.Fatalf("decision failure = %q", failure)
	}
	if client.state.volumes[volume] == nil {
		t.Fatal("refused retained cleanup deleted the volume")
	}
}

// directorMessage returns the message the dispatcher sends the Director for
// err, which is the innermost CPI error's text.
func directorMessage(err error) string {
	var cloud *cpierrors.Error
	if !errors.As(err, &cloud) {
		return ""
	}
	return cloud.Error()
}

// retainedVolume returns the volume the parker holds after a retain delete.
func retainedVolume(t *testing.T, client *lifecycleFlowPVE) string {
	t.Helper()
	for vmid, cfg := range client.state.configs {
		if vmid == 777 {
			continue
		}
		if value, ok := cfg["scsi0"].(string); ok {
			return value
		}
	}
	t.Fatal("no parker holds the retained volume")
	return ""
}

// moveResumeVM moves VM 123 of resumeVMFixture from pve1, where its record
// was written, to pve2 on the shared NFS storage "a", and has the cluster
// list both nodes.
func moveResumeVM(c *resumeVMClient) {
	twoNodeCluster(c.allocationAuditClient)
	c.volumes.nodes = []string{"pve1", "pve2"}
	c.clusterRead.guestNodes = c.nodesRead.guestNodes
	c.nodesRead.guestNodes[123] = "pve2"
	volumes := []string{"a:123/vm-123-disk-0.qcow2", "a:123/vm-123-disk-1.qcow2"}
	c.nodesRead.volumesByNode = map[string][]string{"pve1": volumes, "pve2": volumes}
}

func TestObserveManagedVMRecordAcceptsVMMovedOnSharedStorage(t *testing.T) {
	deps, j, c, record, _ := resumeVMFixture(t)
	moveResumeVM(c)
	files := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	observed, err := observeManagedVMRecord(t.Context(), deps, j, record)
	if err != nil {
		t.Fatalf("VM moved on shared storage refused: %v", err)
	}
	if observed.Node != "pve2" || observed.VMID != 123 || !observed.Verification.OwnershipVerified || !strings.Contains(observed.Verification.EvidenceJSON, `"node":"pve2"`) {
		t.Fatalf("observation = %+v", observed)
	}
	if !reflect.DeepEqual(files, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("observing a moved VM changed the journal")
	}
}

func TestObserveManagedVMRecordRefusesUnacceptedMoves(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		mutate       func(t *testing.T, j *aj.Journal, c *resumeVMClient, record *aj.Record)
	}{
		{"volume missing from the new node's listing", "not a move: volume a:123/vm-123-disk-0.qcow2 was not listed on storage \"a\" on pve2", func(_ *testing.T, _ *aj.Journal, c *resumeVMClient, _ *aj.Record) {
			c.nodesRead.volumesByNode["pve2"] = nil
		}},
		{"record outside the returned states", "not a move: the record is in state reconciliation_required", func(t *testing.T, j *aj.Journal, _ *resumeVMClient, record *aj.Record) {
			h, err := j.Acquire(t.Context(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			r := h.Record()
			r.State = aj.ReconciliationRequired
			r.Reason = "operator hold"
			if err := h.Save(r); err != nil {
				t.Fatal(err)
			}
			*record = h.Record()
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, j, c, record, _ := resumeVMFixture(t)
			moveResumeVM(c)
			tc.mutate(t, j, c, &record)
			files := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			_, err := observeManagedVMRecord(t.Context(), deps, j, record)
			if message := directorMessage(err); !strings.HasPrefix(message, "VM allocation readback refused") || !strings.Contains(message, tc.reason) {
				t.Fatalf("unaccepted move not refused with its reason: %q", message)
			}
			if !reflect.DeepEqual(files, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("refused observation changed the journal")
			}
		})
	}
}

// TestAdmitLocationRequiresRecordedNodeOrObservedMove pins the readback's
// node rule on its own: a recorded node passes, another node passes only
// when the audit lists this allocation's move to it.
func TestAdmitLocationRequiresRecordedNodeOrObservedMove(t *testing.T) {
	_, _, _, record, _ := resumeVMFixture(t)
	readback := managedVMRecordReadback{record: record, vmid: 123, node: "pve1"}
	if err := readback.admitLocation(StorageAllocationAudit{}); err != nil {
		t.Fatalf("recorded node refused: %v", err)
	}
	readback.node = "pve2"
	err := readback.admitLocation(StorageAllocationAudit{})
	if err == nil || !strings.Contains(err.Error(), "actual VM is outside recorded nodes: VM 123 on pve2, recorded pve1, and the audit accepted no move") {
		t.Fatalf("unrecorded node without a move: %v", err)
	}
	other := StorageAllocationAudit{ObservedMoves: []StorageAllocationMove{{AllocationID: "another-allocation", Kind: "vm", VMID: 123, ObservedNode: "pve2"}}}
	if err := readback.admitLocation(other); err == nil {
		t.Fatal("another allocation's move admitted this VM")
	}
	moved := StorageAllocationAudit{ObservedMoves: []StorageAllocationMove{{AllocationID: record.ID, Kind: "vm", VMID: 123, RecordedNodes: []string{"pve1"}, ObservedNode: "pve2"}}}
	if err := readback.admitLocation(moved); err != nil {
		t.Fatalf("accepted move refused: %v", err)
	}
}

// TestManagedVMResumeReturnsCIDOfVMMovedOnSharedStorage is a Director retry
// of create_vm after the VM moved. Resume returns the CID and appends only
// the ownership proof it always writes; no step records the move.
func TestManagedVMResumeReturnsCIDOfVMMovedOnSharedStorage(t *testing.T) {
	deps, j, c, record, args := resumeVMFixture(t)
	moveResumeVM(c)
	result, err := resumeManagedVM(t.Context(), deps, &createVMParsedArgs{agentID: "agent"}, nil, j, record, args, nil)
	response, ok := result.([]any)
	if err != nil || !ok || len(response) != 2 || response[0] != "123" {
		t.Fatalf("resume of a moved VM = %v, %v", result, err)
	}
	after, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Steps, record.Steps) || after.State != record.State {
		t.Fatal("resume recorded the move in the journal")
	}
	if len(after.Verifications) != 1 || !after.Verifications[0].OwnershipVerified || !strings.Contains(after.Verifications[0].EvidenceJSON, `"node":"pve2"`) {
		t.Fatalf("ownership proof = %+v", after.Verifications)
	}
	if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
		t.Fatal("resume mutated PVE")
	}
}

func TestAllocationDecisionAdoptsVMMovedOnSharedStorage(t *testing.T) {
	deps, j, c, record, _ := resumeVMFixture(t)
	moveResumeVM(c)
	next, err := ApplyStorageAllocationDecision(t.Context(), deps, j, []string{"pve1", "pve2"}, StorageAllocationDecision{Action: "adopt", AllocationID: record.ID, ExpectedCID: record.CID, DecisionID: "moved-adoption"})
	if err != nil {
		t.Fatalf("adoption of a VM moved on shared storage refused: %v", err)
	}
	if next.State != aj.Adopted || !reflect.DeepEqual(next.Steps, record.Steps) {
		t.Fatalf("adoption = %s, steps changed %v", next.State, !reflect.DeepEqual(next.Steps, record.Steps))
	}
	if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
		t.Fatal("adoption mutated PVE")
	}
}

// TestAdmitStorageVMAllocationLeavesJournalUnchangedForMovedVM pins that
// admission accepts another agent's moved VM without writing anything.
func TestAdmitStorageVMAllocationLeavesJournalUnchangedForMovedVM(t *testing.T) {
	deps, j, c, _, _ := resumeVMFixture(t)
	moveResumeVM(c)
	files := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	if err := admitStorageVMAllocation(t.Context(), deps, j, []string{"pve1", "pve2"}, "another-agent"); err != nil {
		t.Fatalf("create_vm admission refused a VM moved on shared storage: %v", err)
	}
	if err := admitStorageVMAllocation(t.Context(), deps, j, []string{"pve1", "pve2"}, "agent"); err == nil || !strings.Contains(err.Error(), "already has remote VM provenance") {
		t.Fatalf("the moved VM's own agent was admitted again: %v", err)
	}
	if !reflect.DeepEqual(files, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("admission changed the journal")
	}
	if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
		t.Fatal("admission mutated PVE")
	}
}

// crashDeleteOfMovedVM runs delete_vm on VM 123 after it moved to pve2 and
// loses the stop task's response, which leaves the record in reconciliation
// with a planned stop step on pve2 and create steps on pve1.
func crashDeleteOfMovedVM(t *testing.T) (Deps, *aj.Journal, *deleteManagedClient, aj.Record, aj.Step) {
	t.Helper()
	deps, j, c, record := deleteManagedFixture(t)
	moveResumeVM(c.resumeVMClient)
	c.lostStop = true
	if handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123); !handled || err == nil {
		t.Fatalf("lost stop response not reported: %v %v", handled, err)
	}
	after, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	for index := range after.Steps {
		step := after.Steps[index]
		if step.Kind == "vm.delete.stop" {
			if after.State != aj.ReconciliationRequired || step.State != aj.Planned || step.Target.Node != "pve2" {
				t.Fatalf("crashed delete left state %s and stop %+v", after.State, step)
			}
			deps.PVE = &cleanupTaskClient{Client: c}
			return deps, j, c, after, step
		}
	}
	t.Fatalf("crashed delete recorded no stop step: %+v", after.Steps)
	return deps, j, c, after, aj.Step{}
}

// TestCleanupReentersCrashedDeleteOfVMMovedOnSharedStorage links the stop
// step on pve2 to the create steps on pve1 through the move the delete
// admission accepted, and then observes the stop on the VM it reached.
func TestCleanupReentersCrashedDeleteOfVMMovedOnSharedStorage(t *testing.T) {
	deps, j, _, record, stop := crashDeleteOfMovedVM(t)
	files := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	ctx, settlement, err := admitStorageCleanupSettlement(t.Context(), deps, record, cleanupAttestedDecision(record.ID))
	if err != nil {
		t.Fatalf("crashed delete of a moved VM not admitted for cleanup: %v", err)
	}
	if settlement == nil || len(settlement.CompletedVMDeletions) != 1 || settlement.CompletedVMDeletions[0].ID != stop.ID {
		t.Fatalf("stop step not linked: %+v", settlement)
	}
	if err := observeCleanupVMDeletion(ctx, deps, j, record, stop); err != nil {
		t.Fatalf("stop of the moved VM not observed: %v", err)
	}
	if !reflect.DeepEqual(files, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("re-entry admission changed the journal")
	}
	// The link rests on the admission's retained verdict alone.
	unproven := record
	unproven.Verifications = nil
	if cleanupPendingVMDeletion(stop, unproven) {
		t.Fatal("stop step linked across nodes without an admitted move")
	}
}

// TestCleanupReentryRefusesVMMovedAgainAfterStop moves the VM back to pve1
// after the crashed stop. The stop step names pve2, so re-entry refuses.
func TestCleanupReentryRefusesVMMovedAgainAfterStop(t *testing.T) {
	deps, j, c, record, stop := crashDeleteOfMovedVM(t)
	c.nodesRead.guestNodes[123] = "pve1"
	if err := observeCleanupVMDeletion(t.Context(), deps, j, record, stop); err == nil || !strings.Contains(err.Error(), "pending stop lacks exact VM ownership") {
		t.Fatalf("VM moved again after its stop was accepted: %v", err)
	}
}

// TestCleanupPendingConfigStaysStrictForVMMovedOutsideReturnedStates covers
// the settlement caller. Its record is in reconciliation, so the audit keeps
// the move a conflict and explicit cleanup refuses without writing.
func TestCleanupPendingConfigStaysStrictForVMMovedOutsideReturnedStates(t *testing.T) {
	deps, j, c, record := deleteManagedFixture(t)
	moveResumeVM(c.resumeVMClient)
	appendCleanupPending(t, j, record.ID, "vm.Nodes.UpdateQemuConfig")
	deps.PVE = &cleanupTaskClient{Client: c}
	files := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	_, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1", "pve2"}, cleanupAttestedDecision(record.ID))
	if message := directorMessage(err); !strings.Contains(message, "not a move: the record is in state reconciliation_required") {
		t.Fatalf("moved VM outside the returned states not refused: %q", message)
	}
	if c.stopCount != 0 || c.destroyCount != 0 || !reflect.DeepEqual(files, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("refused cleanup changed PVE or the journal")
	}
}

// TestCleanupPendingVMAllocationStaysStrictWhenMoved covers the pending
// allocation caller: a VM whose CID was never returned has no reason to move.
func TestCleanupPendingVMAllocationStaysStrictWhenMoved(t *testing.T) {
	deps, j, c, record, decision := unknownVMAllocationFixture(t, storageRoleRoot)
	twoNodeCluster(c.allocationAuditClient)
	c.nodesRead.guestNodes[123] = "pve2"
	c.nodesRead.volumesByNode = map[string][]string{"pve1": {"a:123/vm-123-disk-0.qcow2"}, "pve2": {"a:123/vm-123-disk-0.qcow2"}}
	files := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	_, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1", "pve2"}, decision)
	if err == nil {
		t.Fatal("moved pending allocation cleaned up")
	}
	// The runbook quotes this refusal for a VM whose CID was never returned.
	if failure := StorageAllocationDecisionFailure(err); failure != "cleanup_historical_audit: 1 audit conflict; VM 123 on pve2, recorded pve1 (node_mismatch); not a move: the record is in state "+string(record.State)+", not ready_to_return or adopted" {
		t.Fatalf("decision failure = %q", failure)
	}
	if c.destroyCount != 0 || c.volumeDeletes != 0 || !reflect.DeepEqual(files, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("refused cleanup changed PVE or the journal")
	}
	pending := record.Steps[len(record.Steps)-1]
	if err := pendingVMAllocationMoved(record, pending, "pve2", 123); err == nil {
		t.Fatal("pending allocation check accepted a moved VM")
	}
}

// TestCleanupUploadedISOStaysStrictWhenMoved covers the ISO caller, whose
// own node check refuses a VM observed anywhere but the upload's node.
func TestCleanupUploadedISOStaysStrictWhenMoved(t *testing.T) {
	observed := &managedVMObservation{Node: "pve2", VMID: 123, Verification: aj.Verification{Complete: true, OwnershipVerified: true}}
	err := observeCleanupUploadedISO(t.Context(), Deps{}, aj.Record{}, aj.Target{Node: "pve1", VMID: 123}, observed)
	if err == nil || !strings.Contains(err.Error(), "uploaded ISO cleanup lacks exact live VM ownership") {
		t.Fatalf("ISO cleanup accepted a moved VM: %v", err)
	}
}
