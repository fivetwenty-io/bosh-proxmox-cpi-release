package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	nodesapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// These tests cover a returned VM that moved from n1 to n2 on shared storage
// and is then disposed of by an operator's explicit cleanup, or by a
// delete_vm whose admission write fails. Each test runs the same steps on an
// unmoved VM as the control and requires the moved VM to end exactly like it.

// volumeDestroyingPVE destroys a guest together with the volumes its
// configuration references, the way qm destroy does. The flow fake's
// DeleteQemu refuses a guest that still holds a volume, because it models
// only the retention parker, so an explicit cleanup cannot finish on it
// without this.
type volumeDestroyingPVE struct{ *lifecycleFlowPVE }

func (c volumeDestroyingPVE) Nodes() nodesapi.Service {
	return volumeDestroyingNodes{lifecycleFlowNodes: c.lifecycleFlowPVE.Nodes().(lifecycleFlowNodes)}
}

type volumeDestroyingNodes struct{ lifecycleFlowNodes }

func (n volumeDestroyingNodes) DeleteQemu(ctx context.Context, node, vmidText string, p *nodesapi.DeleteQemuParams) (*nodesapi.DeleteQemuResponse, error) {
	vmid, err := strconv.Atoi(vmidText)
	if err != nil {
		return nil, err
	}
	if n.c.vmNode(vmid) == node {
		config := n.c.state.configs[vmid]
		for key, value := range config {
			if !isDiskOptionKey(key) {
				continue
			}
			text, _ := value.(string)
			delete(n.c.state.volumes, strings.Split(text, ",")[0])
			delete(config, key)
		}
		// PVE's destroy works from the current config, which still has a slot
		// whose delete is pending, so it takes that slot's volume too.
		if n.c.pending != nil {
			for _, text := range n.c.pending.dropHeld(vmid) {
				delete(n.c.state.volumes, strings.Split(text, ",")[0])
			}
		}
	}
	return n.lifecycleFlowNodes.DeleteQemu(ctx, node, vmidText, p)
}

// explicitAdmissionMoves returns the moves that record's explicit cleanup
// admission retained.
func explicitAdmissionMoves(t *testing.T, record aj.Record) []StorageAllocationMove {
	t.Helper()
	for _, verification := range record.Verifications {
		var evidence struct {
			Operation     string                  `json:"operation"`
			ObservedMoves []StorageAllocationMove `json:"observed_moves"`
		}
		if json.Unmarshal([]byte(verification.EvidenceJSON), &evidence) == nil && evidence.Operation == "explicit_cleanup_admission" {
			return evidence.ObservedMoves
		}
	}
	t.Fatal("record has no explicit cleanup admission")
	return nil
}

// explicitCleanupOutcome is what one run of the explicit cleanup scenario
// observed. IDs are replaced so that a moved run and its control compare
// equal.
type explicitCleanupOutcome struct {
	FirstCleanup   string
	CleanupState   aj.State
	Conflicts      []string
	Admission      string
	SecondCleanup  string
	DirectorDelete string
	FinalState     aj.State
	GuestRemains   bool
}

// TestExplicitCleanupOfMovedReturnedVMFinishesLikeItsControl runs an
// operator's cleanup on the returned record of VM 777 with no delete_vm
// first. The cleanup's own audit accepts the move before the cleanup takes
// the record out of ready_to_return, and the admission it writes retains that
// move, so the disposal that follows keeps reading the VM as moved. VM 777 is
// tagged to retain its ephemeral disk, so the first cleanup keeps the volume
// and stops at vm_deleted_retained, and another deployment's create_vm must
// be admitted. The second cleanup deletes the retained volume and ends the
// record cleaned, and the Director's delete_vm must find nothing left to do,
// just as for an unmoved VM.
func TestExplicitCleanupOfMovedReturnedVMFinishesLikeItsControl(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision func(vmID string) StorageAllocationDecision
	}{
		{"attested decision", cleanupAttestedDecision},
		{"plain decision", func(vmID string) StorageAllocationDecision {
			return StorageAllocationDecision{Action: "cleanup", AllocationID: vmID, DecisionID: "incident-1234-approved-removal"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(t *testing.T, moved bool) (explicitCleanupOutcome, []StorageAllocationMove) {
				deps, client, journal, vmID := deleteWindowFixture(t, moved)
				deps.PVE = volumeDestroyingPVE{client}
				nodes := []string{"n1", "n2"}
				ctx := context.Background()
				var outcome explicitCleanupOutcome
				if _, err := CleanupStorageAllocation(ctx, deps, journal, nodes, tc.decision(vmID)); err != nil {
					outcome.FirstCleanup = strings.ReplaceAll(StorageAllocationDecisionFailure(err), vmID, "<vm>")
				}
				record, err := journal.Inspect(vmID)
				if err != nil {
					t.Fatal(err)
				}
				outcome.CleanupState = record.State
				report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
				if err != nil {
					t.Fatal(err)
				}
				for _, conflict := range report.Conflicts {
					outcome.Conflicts = append(outcome.Conflicts, strings.ReplaceAll(conflict, vmID, "<vm>"))
				}
				outcome.Admission = deleteWindowText(admitStorageVMAllocation(ctx, deps, journal, nodes, "other-agent"), vmID)
				second := tc.decision(vmID)
				second.DecisionID += "-again"
				if _, err := CleanupStorageAllocation(ctx, deps, journal, nodes, second); err != nil {
					outcome.SecondCleanup = strings.ReplaceAll(StorageAllocationDecisionFailure(err), vmID, "<vm>")
				}
				_, err = HandleDeleteVM(deps).Handle(ctx, []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
				outcome.DirectorDelete = deleteWindowText(err, vmID)
				final, err := journal.Inspect(vmID)
				if err != nil {
					t.Fatal(err)
				}
				outcome.FinalState, outcome.GuestRemains = final.State, client.state.configs[777] != nil
				return outcome, explicitAdmissionMoves(t, final)
			}
			control, controlMoves := run(t, false)
			if control.FirstCleanup != "" || control.CleanupState != aj.VMDeletedRetained || len(control.Conflicts) != 0 || control.Admission != "" || control.SecondCleanup != "" || control.DirectorDelete != "" || control.FinalState != aj.Cleaned || control.GuestRemains {
				t.Fatalf("unmoved control changed: %+v", control)
			}
			if len(controlMoves) != 0 {
				t.Fatalf("unmoved cleanup retained moves: %+v", controlMoves)
			}
			moved, movedMoves := run(t, true)
			if !reflect.DeepEqual(moved, control) {
				t.Fatalf("moved VM diverged from its control:\nmoved   %+v\ncontrol %+v", moved, control)
			}
			if len(movedMoves) != 1 || movedMoves[0].Kind != "vm" || movedMoves[0].VMID != 777 || movedMoves[0].ObservedNode != "n2" {
				t.Fatalf("cleanup admission did not retain the accepted move: %+v", movedMoves)
			}
		})
	}
}

// TestDeleteOfMovedVMSurvivesAFailedAdmissionWrite fails the journal write
// that carries the delete admission of a returned VM. The write that takes
// the record out of ready_to_return is the same write, so the failure leaves
// the record returned, the audit stays clean, another deployment's create_vm
// is admitted, and the Director's retry finishes the delete, just as for an
// unmoved VM.
func TestDeleteOfMovedVMSurvivesAFailedAdmissionWrite(t *testing.T) {
	run := func(t *testing.T, moved bool) (deleteWindowOutcome, aj.State) {
		deps, client, journal, vmID := deleteWindowFixture(t, moved)
		nodes := []string{"n1", "n2"}
		injected := errors.New("injected journal write failure")
		fired := false
		ctx := aj.WithSaveFaultForTest(context.Background(), func(next aj.Record) error {
			if fired || next.ID != vmID || len(next.Verifications) == 0 || !strings.Contains(next.Verifications[len(next.Verifications)-1].EvidenceJSON, managedVMCleanupAdmissionOperation) {
				return nil
			}
			fired = true
			return injected
		})
		var outcome deleteWindowOutcome
		_, err := HandleDeleteVM(deps).Handle(ctx, []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
		if !fired {
			t.Fatal("the delete wrote no admission")
		}
		if !errors.Is(err, injected) {
			t.Fatalf("delete did not fail on the admission write: %v", err)
		}
		outcome.FirstDelete = deleteWindowText(err, vmID)
		record, err := journal.Inspect(vmID)
		if err != nil {
			t.Fatal(err)
		}
		afterFailure := record.State
		report, err := AuditStorageAllocations(context.Background(), deps, journal, nodes)
		if err != nil {
			t.Fatal(err)
		}
		outcome.Conflicts = report.Conflicts
		outcome.Admission = deleteWindowText(admitStorageVMAllocation(context.Background(), deps, journal, nodes, "other-agent"), vmID)
		_, err = HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
		outcome.RetryDelete = deleteWindowText(err, vmID)
		final, err := journal.Inspect(vmID)
		if err != nil {
			t.Fatal(err)
		}
		outcome.FinalState, outcome.GuestRemains = final.State, client.state.configs[777] != nil
		return outcome, afterFailure
	}
	control, controlAfterFailure := run(t, false)
	if controlAfterFailure != aj.ReadyToReturn || len(control.Conflicts) != 0 || control.Admission != "" || control.RetryDelete != "" || control.FinalState != aj.VMDeletedRetained || control.GuestRemains {
		t.Fatalf("unmoved control changed: after failure %s, %+v", controlAfterFailure, control)
	}
	moved, movedAfterFailure := run(t, true)
	if movedAfterFailure != controlAfterFailure || !reflect.DeepEqual(moved, control) {
		t.Fatalf("moved VM diverged from its control:\nmoved   %s %+v\ncontrol %s %+v", movedAfterFailure, moved, controlAfterFailure, control)
	}
}
