package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodesapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// These tests cover the window in which delete_vm has taken a moved VM's
// record out of ready_to_return. The delete admission accepted the move and
// retained it in its evidence, so the audit keeps reading the VM as moved
// until the guest is gone. Each test runs the same steps on an unmoved VM as
// the control and requires the moved VM to behave exactly like it.

// admissionVerification is the evidence a delete admission retains, naming
// the moves its audit accepted.
func admissionVerification(t *testing.T, record aj.Record, ownership bool, moves ...StorageAllocationMove) aj.Verification {
	t.Helper()
	evidence := struct {
		ObservedMoves []StorageAllocationMove `json:"observed_moves,omitempty"`
		Facts         map[string]any          `json:"facts"`
	}{moves, map[string]any{allocationEvidenceOperationField: managedVMCleanupAdmissionOperation, allocationEvidenceIDField: record.ID}}
	raw, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	return aj.Verification{EvidenceID: "admission", Complete: true, OwnershipVerified: ownership, EvidenceJSON: string(raw)}
}

// TestAllocationAuditKeepsTheDeleteAdmissionsMove drives the VM rule's
// deletion clause on the move fixture, where VM 123 was recorded on pve1 and
// now runs on pve2. Only a deleting record whose latest ownership-verified
// admission named this VMID and node keeps the move, and every live
// condition of the rule still applies to it.
func TestAllocationAuditKeepsTheDeleteAdmissionsMove(t *testing.T) {
	vmMove := func(f *moveFixture, vmid int, node string) StorageAllocationMove {
		return StorageAllocationMove{AllocationID: f.vm.ID, Kind: "vm", VMID: vmid, RecordedNodes: []string{"pve1"}, ObservedNode: node}
	}
	for _, tc := range []struct {
		name     string
		state    aj.State
		unlisted bool
		admit    func(t *testing.T, f *moveFixture) []aj.Verification
		refusal  string
	}{
		{name: "reconciliation after the admission accepted pve2", state: aj.ReconciliationRequired,
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, true, vmMove(f, 123, "pve2"))}
			}},
		{name: "observed while the delete runs", state: aj.Observed,
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, true, vmMove(f, 123, "pve2"))}
			}},
		{name: "planned while a delete step is open", state: aj.Planned,
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, true, vmMove(f, 123, "pve2"))}
			}},
		{name: "no admission", state: aj.ReconciliationRequired, refusal: "the record is in state reconciliation_required, not ready_to_return or adopted",
			admit: func(*testing.T, *moveFixture) []aj.Verification { return nil }},
		{name: "admission named another node", state: aj.ReconciliationRequired, refusal: "the record is in state reconciliation_required",
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, true, vmMove(f, 123, "pve3"))}
			}},
		{name: "admission named another VMID", state: aj.ReconciliationRequired, refusal: "the record is in state reconciliation_required",
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, true, vmMove(f, 124, "pve2"))}
			}},
		{name: "admission without verified ownership", state: aj.ReconciliationRequired, refusal: "the record is in state reconciliation_required",
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, false, vmMove(f, 123, "pve2"))}
			}},
		{name: "a later admission accepted no move", state: aj.ReconciliationRequired, refusal: "the record is in state reconciliation_required",
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, true, vmMove(f, 123, "pve2")), admissionVerification(t, f.vm, true)}
			}},
		{name: "submitted records stay strict", state: aj.Submitted, refusal: "the record is in state submitted, not ready_to_return or adopted",
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, true, vmMove(f, 123, "pve2"))}
			}},
		{name: "the live storage checks still apply", state: aj.ReconciliationRequired, unlisted: true, refusal: `volume a:123/vm-123-disk-1.qcow2 was not listed on storage "a" on pve2`,
			admit: func(t *testing.T, f *moveFixture) []aj.Verification {
				return []aj.Verification{admissionVerification(t, f.vm, true, vmMove(f, 123, "pve2"))}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoveFixture(t)
			if tc.unlisted {
				f.unlisted = map[string]bool{"a:123/vm-123-disk-1.qcow2": true}
			}
			records := f.build()
			records[0].State = tc.state
			records[0].Verifications = tc.admit(t, f)
			report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			moved := report.observedMove("vm", f.vm.ID, 123, "pve2")
			if tc.refusal == "" {
				if !moved || len(report.Conflicts) != 0 {
					t.Fatalf("admitted move refused:\n%s", strings.Join(report.Conflicts, "\n"))
				}
				return
			}
			if moved || !findingWith(report.Conflicts, "remote allocation "+f.vm.ID+" (VM 123)", "; not accepted as a move because "+tc.refusal) {
				t.Fatalf("move not refused with %q:\n%s", tc.refusal, strings.Join(report.Conflicts, "\n"))
			}
		})
	}
}

// deleteWindowFixture is retainDeleteFixture with VM 777 moved from n1 to n2
// on shared storage when moved is set.
func deleteWindowFixture(t *testing.T, moved bool) (Deps, *lifecycleFlowPVE, *aj.Journal, string) {
	t.Helper()
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	if moved {
		client.vmNodes = map[int]string{777: "n2"}
	}
	return deps, client, journal, vmID
}

// deleteWindowOutcome is what one run of a window scenario observed. IDs are
// replaced so that a moved run and its control compare equal.
type deleteWindowOutcome struct {
	FirstDelete  string
	Admission    string
	Conflicts    []string
	RetryDelete  string
	FinalState   aj.State
	GuestRemains bool
}

func deleteWindowText(err error, vmID string) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), vmID, "<vm>")
}

type statusFailQEMU struct {
	lifecycleFlowQEMU
	fail *bool
}

func (q statusFailQEMU) Status(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if *q.fail {
		return nil, errors.New("transient status read failure")
	}
	return q.lifecycleFlowQEMU.Status(ctx, node, vmid)
}

// statusFailPVE fails every VM status read while fail is set.
type statusFailPVE struct {
	*lifecycleFlowPVE
	fail *bool
}

func (c statusFailPVE) QEMU() qemu.Service {
	return statusFailQEMU{lifecycleFlowQEMU: c.lifecycleFlowPVE.QEMU().(lifecycleFlowQEMU), fail: c.fail}
}

// TestDeleteOfMovedVMSurvivesATransientStatusFailure fails the status read
// that delete_vm makes after its admission and before any step on the VM's
// node. Another deployment's create_vm must still be admitted, and the
// Director's retry must finish the delete, just as for an unmoved VM.
func TestDeleteOfMovedVMSurvivesATransientStatusFailure(t *testing.T) {
	run := func(t *testing.T, moved bool) deleteWindowOutcome {
		deps, client, journal, vmID := deleteWindowFixture(t, moved)
		fail := true
		deps.PVE = statusFailPVE{lifecycleFlowPVE: client, fail: &fail}
		var outcome deleteWindowOutcome
		_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
		outcome.FirstDelete = deleteWindowText(errors.Unwrap(err), vmID)
		outcome.Admission = deleteWindowText(admitStorageVMAllocation(context.Background(), deps, journal, []string{"n1", "n2"}, "other-agent"), vmID)
		fail = false
		_, err = HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
		outcome.RetryDelete = deleteWindowText(err, vmID)
		record, err := journal.Inspect(vmID)
		if err != nil {
			t.Fatal(err)
		}
		outcome.FinalState, outcome.GuestRemains = record.State, client.state.configs[777] != nil
		return outcome
	}
	control, moved := run(t, false), run(t, true)
	if control.FirstDelete == "" || !strings.Contains(control.FirstDelete, "transient status read failure") || control.Admission != "" || control.RetryDelete != "" || control.FinalState != aj.VMDeletedRetained || control.GuestRemains {
		t.Fatalf("unmoved control changed: %+v", control)
	}
	if !reflect.DeepEqual(moved, control) {
		t.Fatalf("moved VM diverged from its control:\nmoved   %+v\ncontrol %+v", moved, control)
	}
}

// TestDeleteOfMovedVMKeepsItsMoveAfterRetentionFails fails the retention
// transfer of a stopped VM, which leaves the record in reconciliation with
// its retention steps on the VM's node. The audit must keep reading the VM
// as moved, so create_vm is admitted, and the Director's retry and explicit
// cleanup must refuse for the same reason they refuse on an unmoved VM.
func TestDeleteOfMovedVMKeepsItsMoveAfterRetentionFails(t *testing.T) {
	run := func(t *testing.T, moved bool) (deleteWindowOutcome, StorageAllocationAudit, string) {
		deps, client, journal, vmID := deleteWindowFixture(t, moved)
		client.moveErr = errors.New("injected transfer failure")
		var outcome deleteWindowOutcome
		_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
		outcome.FirstDelete = deleteWindowText(err, vmID)
		client.moveErr = nil
		report, err := AuditStorageAllocations(context.Background(), deps, journal, []string{"n1", "n2"})
		if err != nil {
			t.Fatal(err)
		}
		outcome.Conflicts = report.Conflicts
		outcome.Admission = deleteWindowText(admitStorageVMAllocation(context.Background(), deps, journal, []string{"n1", "n2"}, "other-agent"), vmID)
		_, err = HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
		outcome.RetryDelete = deleteWindowText(err, vmID)
		_, err = CleanupStorageAllocation(context.Background(), deps, journal, []string{"n1", "n2"}, cleanupAttestedDecision(vmID))
		cleanup := StorageAllocationDecisionFailure(err)
		record, err := journal.Inspect(vmID)
		if err != nil {
			t.Fatal(err)
		}
		outcome.FinalState, outcome.GuestRemains = record.State, client.state.configs[777] != nil
		return outcome, report, strings.ReplaceAll(cleanup, vmID, "<vm>")
	}
	control, _, controlCleanup := run(t, false)
	if control.FirstDelete == "" || control.Admission != "" || len(control.Conflicts) != 0 || control.RetryDelete == "" || strings.Contains(control.RetryDelete, "refused: ") || control.FinalState != aj.ReconciliationRequired || !control.GuestRemains {
		t.Fatalf("unmoved control changed: %+v", control)
	}
	moved, report, movedCleanup := run(t, true)
	if !reflect.DeepEqual(moved, control) || movedCleanup != controlCleanup {
		t.Fatalf("moved VM diverged from its control:\nmoved   %+v %q\ncontrol %+v %q", moved, movedCleanup, control, controlCleanup)
	}
	if len(report.ObservedMoves) != 1 || report.ObservedMoves[0].Kind != "vm" || report.ObservedMoves[0].VMID != 777 || report.ObservedMoves[0].ObservedNode != "n2" {
		t.Fatalf("audit did not keep reading the VM as moved: %+v", report.ObservedMoves)
	}
}

type moveHookNodes struct {
	lifecycleFlowNodes
	hook func()
}

func (n moveHookNodes) CreateQemuMoveDisk(ctx context.Context, node string, source string, p *nodesapi.CreateQemuMoveDiskParams) (*nodesapi.CreateQemuMoveDiskResponse, error) {
	n.hook()
	return n.lifecycleFlowNodes.CreateQemuMoveDisk(ctx, node, source, p)
}

// moveHookPVE runs hook before every retention transfer.
type moveHookPVE struct {
	*lifecycleFlowPVE
	hook func()
}

func (c moveHookPVE) Nodes() nodesapi.Service {
	return moveHookNodes{lifecycleFlowNodes: c.lifecycleFlowPVE.Nodes().(lifecycleFlowNodes), hook: c.hook}
}

// TestCreateVMIsAdmittedWhileAMovedVMIsDeleted runs another deployment's
// create_vm admission in the middle of a retain delete of a moved VM, the way
// a deploy with max_in_flight above one recreates instances side by side.
func TestCreateVMIsAdmittedWhileAMovedVMIsDeleted(t *testing.T) {
	run := func(t *testing.T, moved bool) deleteWindowOutcome {
		deps, client, journal, vmID := deleteWindowFixture(t, moved)
		reader := deps
		reader.PVE = client
		var outcome deleteWindowOutcome
		admitted := false
		deps.PVE = moveHookPVE{lifecycleFlowPVE: client, hook: func() {
			if !admitted {
				admitted = true
				outcome.Admission = deleteWindowText(admitStorageVMAllocation(context.Background(), reader, journal, []string{"n1", "n2"}, "other-agent"), vmID)
			}
		}}
		_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
		if !admitted {
			t.Fatal("the delete made no retention transfer")
		}
		outcome.FirstDelete = deleteWindowText(err, vmID)
		record, err := journal.Inspect(vmID)
		if err != nil {
			t.Fatal(err)
		}
		outcome.FinalState, outcome.GuestRemains = record.State, client.state.configs[777] != nil
		return outcome
	}
	control, moved := run(t, false), run(t, true)
	if control.Admission != "" || control.FirstDelete != "" || control.FinalState != aj.VMDeletedRetained || control.GuestRemains {
		t.Fatalf("unmoved control changed: %+v", control)
	}
	if !reflect.DeepEqual(moved, control) {
		t.Fatalf("moved VM diverged from its control:\nmoved   %+v\ncontrol %+v", moved, control)
	}
}

// TestDeleteOfMovedVMHealsItsAttachedPersistentDisk deletes VM 777 after it
// moved from n1 to n2 on shared storage while it held a managed persistent
// disk whose provenance names n1. The delete admission accepts both moves,
// the persistent-disk detach inside the delete rewrites the provenance on n2
// before it parks the disk there, and the audit afterwards is clean.
func TestDeleteOfMovedVMHealsItsAttachedPersistentDisk(t *testing.T) {
	deps, client, journal, diskID, cid := lifecycleFlowFixture(t)
	attachMovedDisk(t, deps, client, cid)
	prior, err := journal.Inspect(diskID)
	if err != nil {
		t.Fatal(err)
	}
	persistent := heldDiskVolume(client)
	ephemeral := "a:777/vm-777-ephemeral-0.raw"
	info := *client.state.volumes[persistent]
	client.state.volumes[ephemeral] = &info
	client.state.configs[777]["scsi2"] = ephemeral + ",size=5G"
	client.state.configs[777]["tags"] = tagRetainEphemeral
	plan, err := activeStorageAllocationPlan(prior)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := managedDiskActualDefinition(context.Background(), deps, "a")
	if err != nil {
		t.Fatal(err)
	}
	plan.Definitions["a"] = definition
	plan.AllocationKey = "vm-agent"
	intent := prior.Intent
	if intent.Plan, err = json.Marshal(plan); err != nil {
		t.Fatal(err)
	}
	handle, err := journal.AcquireVM(context.Background(), "vm-agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	vmID := handle.Record().ID
	step, err := storageMutationIntent(handle, "vm.ephemeral.scsi2", aj.Target{Node: "n1", VMID: 777, Storage: "a", Backing: definition.BackingKey(), IntendedVolume: ephemeral}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, []string{ephemeral}, false); err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State, record.CID = aj.ReadyToReturn, "777"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	client.state.configs[777]["description"] = moveMarker(t, vmID, "vm-agent") + "\n" + pve.DescriptionFromConfig(client.state.configs[777])

	before, err := AuditStorageAllocations(context.Background(), deps, journal, []string{"n1", "n2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Conflicts) != 0 || !before.observedMove("vm", vmID, 777, "n2") || !before.observedMove(allocationKindDisk, diskID, 777, "n2") {
		t.Fatalf("delete admission would not accept both moves: conflicts=%q moves=%+v", before.Conflicts, before.ObservedMoves)
	}
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete of a moved VM with an attached persistent disk failed: %v", err)
	}
	after, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != aj.VMDeletedRetained || client.state.configs[777] != nil {
		t.Fatalf("delete incomplete: state=%s guest present=%v", after.State, client.state.configs[777] != nil)
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.holder == nil || !resolved.holder.IsParker || resolved.holder.Node != "n2" {
		t.Fatalf("persistent disk not parked on the VM's current node: %+v", resolved.holder)
	}
	final, err := AuditStorageAllocations(context.Background(), deps, journal, []string{"n1", "n2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Conflicts) != 0 {
		t.Fatalf("audit after the delete has conflicts: %q", final.Conflicts)
	}
}
