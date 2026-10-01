package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// listingFailureOutcome is what one run of listingFailureAdmissions saw.
type listingFailureOutcome struct {
	deps                 Deps
	createVM, createDisk error
	report               StorageAllocationAudit
	allocationID         string
}

// listingFailureAdmissions puts resumeVMFixture's VM 123 on node in a
// three-node cluster whose shared storage "a" lists both of the VM's volumes
// on every node, applies tweak, and runs create_vm admission, create_disk
// admission, and a plain audit against the result.
func listingFailureAdmissions(t *testing.T, node string, tweak func(*resumeVMClient)) listingFailureOutcome {
	t.Helper()
	deps, j, c, record, _ := resumeVMFixture(t)
	moveResumeVM(c)
	nodes := []string{"pve1", "pve2", "pve3"}
	c.clusterRead.members = nodes
	c.volumes.nodes = nodes
	c.nodesRead.guestNodes[123] = node
	volumes := []string{"a:123/vm-123-disk-0.qcow2", "a:123/vm-123-disk-1.qcow2"}
	c.nodesRead.volumesByNode = map[string][]string{"pve1": volumes, "pve2": volumes, "pve3": volumes}
	if tweak != nil {
		tweak(c)
	}
	outcome := listingFailureOutcome{deps: deps, allocationID: record.ID}
	outcome.createVM = admitStorageVMAllocation(t.Context(), deps, j, nodes, "another-agent")
	_, outcome.createDisk = admitStorageAllocation(t.Context(), deps, j, nodes)
	report, err := AuditStorageAllocations(t.Context(), deps, j, nodes)
	if err != nil {
		t.Fatal(err)
	}
	outcome.report = report
	return outcome
}

// failListingsOn fails every storage listing on node with err.
func failListingsOn(node string, err error) func(*resumeVMClient) {
	return func(c *resumeVMClient) { c.nodesRead.failureByNode = map[string]error{node: err} }
}

// TestCreateAdmissionToleratesAFailedListingOnAMovedVMsNode covers a VM that
// the cluster operator moved to pve2 on shared storage, where pve2's listing
// of that storage times out or answers that the storage is not online during
// the audit. A failed listing proves neither that the volume is there nor
// that it is gone, so create_vm and create_disk go ahead the way they do for
// a failed listing on any other node. The audit stays incomplete, which keeps
// delete_vm and delete_disk refused, and it accepts no move.
func TestCreateAdmissionToleratesAFailedListingOnAMovedVMsNode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		node  string
		tweak func(*resumeVMClient)
	}{
		{name: "listing times out on a node that holds no moved VM", node: "pve1", tweak: failListingsOn("pve2", context.DeadlineExceeded)},
		{name: "listing times out on the moved VM's node", node: "pve2", tweak: failListingsOn("pve2", context.DeadlineExceeded)},
		{name: "storage is not online on the moved VM's node", node: "pve2", tweak: failListingsOn("pve2", sdkerrors.ParseAPIError(500, []byte(`{"message":"storage 'a' is not online"}`)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := listingFailureAdmissions(t, tc.node, tc.tweak)
			createVM, createDisk, report, allocationID := outcome.createVM, outcome.createDisk, outcome.report, outcome.allocationID
			if createVM != nil || createDisk != nil {
				t.Fatalf("a failed listing blocked admission: create_vm=%v create_disk=%v", createVM, createDisk)
			}
			if len(report.Conflicts) != 0 {
				t.Fatalf("a failed listing raised conflicts: %s", strings.Join(report.Conflicts, "\n"))
			}
			if report.Complete || !report.VMScanComplete || len(report.VMScanIssues) != 0 {
				t.Fatalf("complete=%v vm_scan_complete=%v vm_scan_issues=%q", report.Complete, report.VMScanComplete, report.VMScanIssues)
			}
			if !findingWith(report.Issues, `storage "a" on node "pve2" could not be inspected`, "") {
				t.Fatalf("the failed listing left no issue: %q", report.Issues)
			}
			if report.observedMove("vm", allocationID, 123, "pve2") {
				t.Fatal("a failed listing proved a move")
			}
			if tc.node == "pve2" && !findingWith(report.Issues, "remote allocation "+allocationID+" (VM 123)", "move undecided (listing_failed)") {
				t.Fatalf("the undecided move left no issue: %q", report.Issues)
			}
			if storageAuditGateError(context.Background(), outcome.deps, "delete_vm", report, storageAuditGateAll) == nil {
				t.Fatal("an incomplete audit admitted a delete")
			}
		})
	}
}

// TestCreateAdmissionRefusesAMovedVMWhoseVolumeIsMissing is the other side
// of the same move. When pve2 lists storage "a" and the VM's root disk is not
// in that listing, the volume is missing there, and the move stays a
// conflict that blocks create_vm and create_disk.
func TestCreateAdmissionRefusesAMovedVMWhoseVolumeIsMissing(t *testing.T) {
	outcome := listingFailureAdmissions(t, "pve2", func(c *resumeVMClient) {
		c.nodesRead.volumesByNode["pve2"] = []string{"a:123/vm-123-disk-1.qcow2"}
	})
	const reason = `not a move: volume a:123/vm-123-disk-0.qcow2 was not listed on storage "a" on pve2`
	for name, err := range map[string]error{"create_vm": outcome.createVM, "create_disk": outcome.createDisk} {
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Fatalf("%s = %v, want a refusal naming the missing volume", name, err)
		}
	}
	if findingWith(outcome.report.Issues, "remote allocation ", "move undecided") {
		t.Fatalf("a successful listing left the move undecided: %q", outcome.report.Issues)
	}
}

// TestAllocationAuditLeavesAMoveUndecidedWhenItsListingFails pins the reason
// a move carries when the new node's listing of its storage failed or held a
// malformed entry. The move is neither accepted nor raised as a conflict
// that says the volume is missing. It becomes an issue with its own reason,
// listing_failed, which leaves the audit incomplete and the VM scan whole.
func TestAllocationAuditLeavesAMoveUndecidedWhenItsListingFails(t *testing.T) {
	const vmConflict = "remote allocation "
	const diskConflict = "disk ownership provenance disagrees with actual holder node"
	const undecided = `; move undecided (listing_failed): storage "a" on pve2 could not be listed, so volume `
	for _, tc := range []struct {
		name   string
		mutate func(f *moveFixture)
	}{
		{name: "failed listing on the new node", mutate: func(f *moveFixture) {
			f.c.nodesRead.failureByNode = map[string]error{"pve2": errors.New("listing failed")}
		}},
		{name: "malformed listing on the new node", mutate: func(f *moveFixture) {
			f.c.nodesRead.malformedListing = map[string]bool{"pve2/a": true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoveFixture(t)
			tc.mutate(f)
			report := f.audit()
			if len(report.Conflicts) != 0 {
				t.Fatalf("an unproven listing raised conflicts:\n%s", strings.Join(report.Conflicts, "\n"))
			}
			vmPrefix := vmConflict + f.vm.ID + " (VM 123) is outside recorded mutation targets"
			if !findingWith(report.Issues, vmPrefix, undecided+"a:123/vm-123-disk-0.qcow2 is unproven there") {
				t.Fatalf("VM move not left undecided:\n%s", strings.Join(report.Issues, "\n"))
			}
			if !findingWith(report.Issues, diskConflict, undecided+"a:123/vm-123-disk-2.qcow2 is unproven there") {
				t.Fatalf("disk move not left undecided:\n%s", strings.Join(report.Issues, "\n"))
			}
			if report.Complete || !report.VMScanComplete || len(report.VMScanIssues) != 0 {
				t.Fatalf("complete=%v vm_scan_complete=%v vm_scan_issues=%q", report.Complete, report.VMScanComplete, report.VMScanIssues)
			}
			if len(report.ObservedMoves) != 0 {
				t.Fatalf("an unproven listing accepted moves: %+v", report.ObservedMoves)
			}
			if got := storageAuditMoveRefusal(report, f.disk.ID); !strings.Contains(got, "move undecided (listing_failed)") {
				t.Fatalf("refusal = %q, want the undecided move", got)
			}
			if err := storageAuditGateError(context.Background(), f.deps, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts); err != nil {
				t.Fatalf("create_vm refused on an unproven listing: %v", err)
			}
			if err := storageAuditGateError(context.Background(), f.deps, "storage allocation admission", report, storageAuditGateConflicts); err != nil {
				t.Fatalf("create_disk refused on an unproven listing: %v", err)
			}
			if storageAuditGateError(context.Background(), f.deps, "delete_vm", report, storageAuditGateAll) == nil {
				t.Fatal("delete_vm admitted an undecided move")
			}
		})
	}
}

// TestAllocationAuditRefusesAMoveThatAFailedListingCannotRescue pins that a
// failed listing leaves a move undecided only when nothing the audit did
// read refuses it. A node-local root disk refuses the VM move whatever the
// listing says, while the persistent disk on shared storage stays undecided.
func TestAllocationAuditRefusesAMoveThatAFailedListingCannotRescue(t *testing.T) {
	f := newMoveFixture(t)
	f.rootStorage = "local"
	f.c.nodesRead.failureByNode = map[string]error{"pve2": errors.New("listing failed")}
	report := f.audit()
	vmPrefix := "remote allocation " + f.vm.ID + " (VM 123) is outside recorded mutation targets"
	if !findingWith(report.Conflicts, vmPrefix, "; not accepted as a move because volume local:123/vm-123-disk-0.qcow2 is node-local") {
		t.Fatalf("node-local VM move not refused:\n%s", strings.Join(report.Conflicts, "\n"))
	}
	if findingWith(report.Issues, vmPrefix, "move undecided") {
		t.Fatalf("a refused VM move was also left undecided:\n%s", strings.Join(report.Issues, "\n"))
	}
	if !findingWith(report.Issues, "disk ownership provenance disagrees with actual holder node", "move undecided (listing_failed)") {
		t.Fatalf("shared disk move not left undecided:\n%s", strings.Join(report.Issues, "\n"))
	}
	if storageAuditGateError(context.Background(), f.deps, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts) == nil {
		t.Fatal("create_vm admitted a node-local move")
	}
}

// TestAllocationAuditRefusesAnOverlongProvenanceNode pins the bound on the
// node a disk's provenance names. A PVE node name is one hostname label of
// at most 63 bytes, so a longer value in a VM's Notes names no node. The
// audit reports it as malformed provenance, the way it reports any other
// provenance it cannot parse, and never repeats it in observed_moves or the
// report.
func TestAllocationAuditRefusesAnOverlongProvenanceNode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		node     string
		accepted bool
	}{
		{name: "63-byte node is accepted as the recorded node", node: strings.Repeat("n", 63), accepted: true},
		{name: "64-byte node is malformed provenance", node: strings.Repeat("n", 64)},
		{name: "8000-byte node is malformed provenance", node: strings.Repeat("n", 8000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoveFixture(t)
			records := f.build()
			provenance := pve.DiskAllocationProvenance{Version: 1, AllocationID: f.disk.ID, AllocationNamespace: "director", Volid: f.pdisk, Node: tc.node, Backing: moveDefinition(t, moveDefinitions["a"]).BackingKey()}
			f.c.configs[123]["description"] = moveDescription(t, moveMarker(t, f.vm.ID, "agent"), provenance)
			report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.accepted {
				want := StorageAllocationMove{AllocationID: f.disk.ID, Kind: allocationKindDisk, VMID: 123, RecordedNodes: []string{tc.node}, ObservedNode: "pve2", Volumes: []string{f.pdisk}}
				if !slices.ContainsFunc(report.ObservedMoves, func(got StorageAllocationMove) bool { return reflect.DeepEqual(got, want) }) {
					t.Fatalf("disk move with a 63-byte node refused: moves=%+v conflicts=%q issues=%q", report.ObservedMoves, report.Conflicts, report.Issues)
				}
				return
			}
			if !findingWith(report.Issues, "VM 123 has malformed disk provenance on pve2", "invalid managed disk provenance") {
				t.Fatalf("overlong node not reported as malformed provenance: %q", report.Issues)
			}
			if report.Complete {
				t.Fatal("malformed provenance left the audit complete")
			}
			if report.observedMove(allocationKindDisk, f.disk.ID, 123, "pve2") {
				t.Fatal("a disk move with an overlong provenance node was accepted")
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), strings.Repeat("n", 64)) {
				t.Fatal("the overlong node reached the report")
			}
		})
	}
}
