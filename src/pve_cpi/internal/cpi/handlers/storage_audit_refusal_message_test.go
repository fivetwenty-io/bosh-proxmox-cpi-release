package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// gateTestRunbookHeading is the runbook section every audit refusal points
// at, and gateTestRunbook is the clause that points there.
const (
	gateTestRunbookHeading = "An operation fails with an allocation audit refusal"
	gateTestRunbook        = `see "` + gateTestRunbookHeading + `" in docs/troubleshooting.md of bosh-proxmox-cpi-release`
)

// TestStorageAuditRunbookHeadingExists pins the heading the refusals quote to
// the runbook, so renaming the section breaks this test before it breaks the
// pointer an operator follows.
func TestStorageAuditRunbookHeadingExists(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "#") && strings.TrimSpace(strings.TrimLeft(line, "#")) == gateTestRunbookHeading {
			return
		}
	}
	t.Fatalf("docs/troubleshooting.md has no heading %q", gateTestRunbookHeading)
}

// TestStorageAuditTargetMissBriefNamesItsAllocation pins that the short form of
// a target miss names its allocation after the reason, for a VM and for a
// volume, while the first 75 characters the Director keeps stay as they were.
func TestStorageAuditTargetMissBriefNamesItsAllocation(t *testing.T) {
	f := newMoveFixture(t)
	f.rootStorage = "local"
	f.diskStorage = "local"
	f.deps.StorageAuditCommand = gateTestCommand
	report := f.audit()
	err := storageAuditGateError(context.Background(), f.deps, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts)
	if err == nil {
		t.Fatal("moved VM with node-local volumes admitted")
	}
	message := err.Error()
	for _, want := range []string{
		"VM 123 on pve2, recorded pve1 (node_mismatch), allocation " + f.vm.ID + "; not a move: volume local:123/vm-123-disk-0.qcow2 is node-local",
		"volume " + f.pdisk + " on pve2, recorded pve1 (node_local_elsewhere), allocation " + f.disk.ID,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message = %q, want it to contain %q", message, want)
		}
	}

	root := newMoveFixture(t)
	root.rootStorage = "local"
	rootReport := root.audit()
	lead := storageAuditGateError(context.Background(), root.deps, "create_vm", rootReport, storageAuditGateVMScan|storageAuditGateConflicts).Error()
	const before = "create_vm refused: 1 audit conflict; VM 123 on pve2, recorded pve1 (node_mismatch); not a move"
	if lead[:75] != before[:75] {
		t.Fatalf("first 75 characters = %q, want %q", lead[:75], before[:75])
	}
}

// TestAllocationAuditSharedVolumeBriefNamesItsAllocations pins that the short
// form of a shared volume names the allocations a record or a holder's disk
// provenance attributes to it, and adds nothing when only the volume's name
// makes it ours.
func TestAllocationAuditSharedVolumeBriefNamesItsAllocations(t *testing.T) {
	a := moveDefinition(t, moveDefinitions["a"])
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) (volume string, ours map[string]any, records []aj.Record, allocation string)
	}{
		{name: "a record names the volume", build: func(t *testing.T) (string, map[string]any, []aj.Record, string) {
			const volume = "a:500/vm-500-disk-1.qcow2"
			record := moveDiskRecord(t, "pve1", volume, map[string]pve.StorageInfo{"a": a})
			return volume, map[string]any{}, []aj.Record{record}, record.ID
		}},
		{name: "our disk provenance names the volume", build: func(t *testing.T) (string, map[string]any, []aj.Record, string) {
			const volume = "a:500/vm-500-disk-1.qcow2"
			provenance := pve.DiskAllocationProvenance{Version: 1, AllocationID: moveAllocationID(t), AllocationNamespace: "director", Volid: volume, Node: "pve1", Backing: a.BackingKey()}
			return volume, map[string]any{"description": moveDescription(t, moveMarker(t, moveAllocationID(t), "agent"), provenance)}, nil, provenance.AllocationID
		}},
		{name: "only the name carries our locator", build: func(t *testing.T) (string, map[string]any, []aj.Record, string) {
			return sharedReferenceAllocationVolume(t, "a"), map[string]any{}, nil, ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volume, ours, records, allocation := tc.build(t)
			ours["scsi1"] = volume
			_, _, report := sharedReferenceCluster(t, map[int]string{500: "pve1", 501: "pve3"}, map[int]map[string]any{
				500: ours,
				501: {"scsi0": "a:501/vm-501-disk-0.qcow2", "scsi1": volume},
			}, records)
			message := storageAuditGateError(context.Background(), Deps{}, "create_vm", report, storageAuditGateConflicts).Error()
			subjects := "volume " + volume + " is attached to 2 VMs: VM 500 on pve1, VM 501 on pve3"
			want := subjects + ";"
			if allocation != "" {
				want = subjects + ", allocation " + allocation + ";"
			}
			if !strings.Contains(message, want) {
				t.Fatalf("message = %q, want it to contain %q", message, want)
			}
		})
	}

	// On node-local storage the same volid on two nodes names two volumes, so
	// the conflict for the copy on pve1 names only the allocations that pve1's
	// holders and a record's pve1 step tie to it.
	t.Run("node-local copy on another node", func(t *testing.T) {
		const volume = "local:500/vm-500-disk-1.qcow2"
		local := map[string]pve.StorageInfo{"local": moveDefinition(t, moveDefinitions["local"])}
		here := moveDiskRecord(t, "pve1", volume, local)
		there := moveDiskRecord(t, "pve2", volume, local)
		elsewhere := pve.DiskAllocationProvenance{Version: 1, AllocationID: moveAllocationID(t), AllocationNamespace: "director", Volid: volume, Node: "pve2", Backing: local["local"].BackingKey()}
		_, _, report := sharedReferenceCluster(t, map[int]string{500: "pve1", 501: "pve1", 502: "pve2"}, map[int]map[string]any{
			500: {"scsi1": volume},
			501: {"scsi1": volume},
			502: {"scsi1": volume, "description": moveDescription(t, moveMarker(t, moveAllocationID(t), "agent"), elsewhere)},
		}, []aj.Record{here, there})
		subjects := "volume " + volume + " is attached to 2 VMs: VM 500 on pve1, VM 501 on pve1"
		var brief string
		for _, conflict := range report.Conflicts {
			if strings.HasPrefix(report.brief(conflict), subjects) {
				brief = report.brief(conflict)
			}
		}
		if want := subjects + ", allocation " + here.ID; brief != want {
			t.Fatalf("brief = %q, want %q; it must not name %s or %s, which belong to the copy on pve2", brief, want, there.ID, elsewhere.AllocationID)
		}
	})
}

// gateTestConflicts adds one conflict per brief, in the order given, as the
// sorted report would hold them.
func gateTestConflicts(report *StorageAllocationAudit, briefs ...string) {
	for index, brief := range briefs {
		report.addConflict("conflict "+string(rune('a'+index))+": "+brief, brief)
	}
}

// TestStorageAuditGateErrorRemainderNamesEachKind pins that the remainder
// counts each kind it left out, so a hidden VM-scan issue shows.
func TestStorageAuditGateErrorRemainderNamesEachKind(t *testing.T) {
	report := StorageAllocationAudit{}
	gateTestConflicts(&report, "VM 101 on pve2, recorded pve1 (node_mismatch)", "VM 102 on pve2, recorded pve1 (node_mismatch)", "VM 103 on pve2, recorded pve1 (node_mismatch)", "VM 104 on pve2, recorded pve1 (node_mismatch)")
	markVMScanIncomplete(&report, "some cluster nodes could not be inspected: pve3 (reported offline by /cluster/status)")
	message := storageAuditGateError(context.Background(), Deps{}, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts).Error()
	if !strings.Contains(message, "; and 1 more audit conflict (VM 104), 1 more VM-scan issue; ") {
		t.Fatalf("message = %q, want the remainder split by kind", message)
	}
}

// TestStorageAuditGateErrorListsOneFindingPerSubjectFirst covers one VM
// moved with a node-local root and a node-local persistent disk. Its three
// conflicts used to fill every slot, which hid a second moved VM. The slots
// now take one finding per VM or volume first, in the report's order, and
// the remainder names the VM whose conflict it left out.
func TestStorageAuditGateErrorListsOneFindingPerSubjectFirst(t *testing.T) {
	const (
		diskID = "439a4e5d-edb5-4b79-a20e-43f8d1330b0b"
		vmID   = "2dd1c81a-4117-4896-875f-f3d8f721c3c5"
	)
	held := "VM 123 on pve2 holds disk allocation " + diskID + ", provenance names pve1; not a move: volume local:123/vm-123-disk-2.qcow2 is node-local"
	moved := "VM 123 on pve2, recorded pve1 (node_mismatch), allocation " + vmID + "; not a move: volume local:123/vm-123-disk-0.qcow2 is node-local"
	volume := "volume local:123/vm-123-disk-2.qcow2 on pve2, recorded pve1 (node_local_elsewhere), allocation " + diskID
	other := "VM 7015 on pve3, recorded pve1 (node_mismatch), allocation 5b0e2f4c-9d1a-4c3e-8f7a-6b5c4d3e2f1a"
	report := StorageAllocationAudit{}
	gateTestConflicts(&report, held, moved, volume, other)
	message := storageAuditGateError(context.Background(), Deps{}, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts).Error()
	want := "create_vm refused: 4 audit conflicts; " + held + "; " + volume + "; " + other + "; and 1 more audit conflict (VM 123); "
	if !strings.HasPrefix(message, want) {
		t.Fatalf("message = %q, want prefix %q", message, want)
	}
}

// TestStorageAuditGateErrorNamesEveryHiddenSubject covers a patch window
// that moved five VMs. The refusal used to name three of them, chosen by
// allocation UUID, and then "and 2 more". The remainder now names the rest.
func TestStorageAuditGateErrorNamesEveryHiddenSubject(t *testing.T) {
	report := StorageAllocationAudit{}
	gateTestConflicts(&report,
		"VM 4626 on pvupvecf102, recorded pvupvecf101 (node_mismatch), allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9",
		"VM 7014 on pvupvecf103, recorded pvupvecf102 (node_mismatch), allocation 2a4b0f7c-1c2d-4e5f-8a9b-0c1d2e3f4a5b",
		"VM 7016 on pvupvecf103, recorded pvupvecf101 (node_mismatch), allocation 3c5d7e9f-2b4a-4c6e-8d0f-1a2b3c4d5e6f",
		"VM 7015 on pvupvecf102, recorded pvupvecf103 (node_mismatch), allocation 4d6e8f0a-3c5b-4d7f-9e1a-2b3c4d5e6f7a",
		"VM 7018 on pvupvecf101, recorded pvupvecf102 (node_mismatch), allocation 5e7f9a1b-4d6c-4e8a-8f2b-3c4d5e6f7a8b")
	markVMScanIncomplete(&report, "some cluster nodes could not be inspected: pvupvecf104 (reported offline by /cluster/status)")
	message := storageAuditGateError(context.Background(), Deps{}, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts).Error()
	if !strings.Contains(message, "; and 2 more audit conflicts (VM 7015, VM 7018), 1 more VM-scan issue; ") {
		t.Fatalf("message = %q, want every hidden VM named", message)
	}
}

// TestStorageAuditGateErrorNamesTheRunbook pins the runbook clause, just
// before the command or its fallback, so the pasteable command stays last.
func TestStorageAuditGateErrorNamesTheRunbook(t *testing.T) {
	withCommand := storageAuditGateError(context.Background(), Deps{StorageAuditCommand: gateTestCommand}, "create_vm", gateTestReport(), storageAuditGateConflicts).Error()
	if !strings.HasSuffix(withCommand, "; "+gateTestRunbook+"; run '"+gateTestCommand+"' for the full report") {
		t.Fatalf("message = %q, want the runbook before the command", withCommand)
	}
	fallback := storageAuditGateError(context.Background(), Deps{}, "delete_disk", gateTestReport(), storageAuditGateAll).Error()
	if !strings.HasSuffix(fallback, "; "+gateTestRunbook+"; "+storageAuditGateFallbackHint) {
		t.Fatalf("message = %q, want the runbook before the fallback hint", fallback)
	}
}

// TestStorageAllocationDecisionFailureNamesTheRunbookForAuditRefusals pins
// that the storage-journal CLI points at the runbook when an audit gate
// refused the decision, and only then.
func TestStorageAllocationDecisionFailureNamesTheRunbookForAuditRefusals(t *testing.T) {
	gate := storageAuditGateError(context.Background(), Deps{StorageAuditCommand: gateTestCommand}, "allocation disposition", gateTestReport(), storageAuditGateConflicts)
	for _, err := range []error{storageDecisionSourceError(gate), storageCleanupFailure("historical_audit", gate)} {
		got := StorageAllocationDecisionFailure(err)
		if !strings.HasSuffix(got, "; "+gateTestRunbook) {
			t.Fatalf("decision failure = %q, want the runbook last", got)
		}
		if strings.Contains(got, "storage-journal audit --summary") {
			t.Fatalf("decision failure = %q repeats the audit command", got)
		}
	}
	for _, err := range []error{
		storageDecisionSourceError(storageRefusal("pending stop lacks exact VM ownership")),
		storageDecisionSourceError(errors.New("password=private")),
	} {
		if got := StorageAllocationDecisionFailure(err); strings.Contains(got, "troubleshooting.md") {
			t.Fatalf("decision failure = %q points a refusal that no audit raised at the audit runbook", got)
		}
	}
}

// TestCreateDiskRefusalUnderAnOfflineNodeNamesTheNode covers a VM moved on
// shared storage while another node is offline. create_disk admission gates
// on conflicts only, so it never lists the VM-scan issue that names the node,
// and its move refusal has to name the node itself.
func TestCreateDiskRefusalUnderAnOfflineNodeNamesTheNode(t *testing.T) {
	deps, j, c, record, _ := resumeVMFixture(t)
	moveResumeVM(c)
	c.clusterRead.members = []string{"pve1", "pve2", "pve3"}
	c.volumes.nodes = []string{"pve1", "pve2", "pve3"}
	volumes := []string{"a:123/vm-123-disk-0.qcow2", "a:123/vm-123-disk-1.qcow2"}
	c.nodesRead.volumesByNode = map[string][]string{"pve1": volumes, "pve2": volumes, "pve3": volumes}
	c.clusterRead.status = []map[string]any{{"type": "cluster", "quorate": 1}, {"type": "node", "name": "pve1", "online": 1}, {"type": "node", "name": "pve2", "online": 1}, {"type": "node", "name": "pve3", "online": 0}}
	c.nodesRead.failureByNode = map[string]error{"pve3": sdkerrors.ParseAPIError(595, []byte(`{"message":"no route to host"}`))}
	_, err := admitStorageAllocation(t.Context(), deps, j, []string{"pve1", "pve2", "pve3"})
	if err == nil {
		t.Fatal("create_disk admitted a move it could not decide")
	}
	want := "VM 123 on pve2, recorded pve1 (node_mismatch), allocation " + record.ID + "; not a move: the VM scan is incomplete because pve3 is reported offline, so another sighting could be hidden"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("create_disk refusal = %q, want it to contain %q", err, want)
	}
}

// TestStorageAuditMoveRefusalNamesTheUninspectedNodes pins the bare reason a
// detach_disk refusal reads when no finding names the allocation, for an
// offline node and for a node whose guest listing failed, with and without a
// verdict that explains it.
func TestStorageAuditMoveRefusalNamesTheUninspectedNodes(t *testing.T) {
	f := newMoveFixture(t)
	f.offline = "pve3"
	if got := storageAuditMoveRefusal(f.audit(), "no-such-allocation"); got != "the VM scan is incomplete because pve3 is reported offline" {
		t.Fatalf("offline node refusal = %q", got)
	}
	for _, tc := range []struct {
		name    string
		failure error
		want    string
	}{
		{"verdict", sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed"}`)), "the VM scan is incomplete because guests could not be listed on pve2 (HTTP 403: Permission check failed)"},
		{"transport", errors.New("opaque listing secret-password"), "the VM scan is incomplete because guests could not be listed on pve2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, c := auditFixture(t)
			twoNodeCluster(c)
			c.nodesRead.qemuFailure = map[string]error{"pve2": tc.failure}
			report, err := auditStorageAllocationRecords(t.Context(), deps, nil, nil, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			if got := storageAuditMoveRefusal(report, "no-such-allocation"); got != tc.want {
				t.Fatalf("enumeration refusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// timedOutVisibilityClient fails the audit visibility proof the way a
// permissions read that timed out does.
type timedOutVisibilityClient struct{ *allocationAuditClient }

func (*timedOutVisibilityClient) StorageAuditVisibility(context.Context) error {
	return &pve.AuditVisibilityReadError{Read: "effective permissions", Path: "/access", Cause: context.DeadlineExceeded}
}

// TestCreateVMRefusedForUnprovenVisibilityKeepsItsErrorType pins that a
// create_vm refused because a visibility read timed out reaches the Director
// as the same non-retriable CloudError it always did. The visibility error
// exposes its cause, and nothing may read that as a transient fault.
func TestCreateVMRefusedForUnprovenVisibilityKeepsItsErrorType(t *testing.T) {
	deps, j, c := auditFixture(t)
	deps.PVE = &timedOutVisibilityClient{c}
	err := admitStorageVMAllocation(t.Context(), deps, j, []string{"pve1"}, "agent")
	if err == nil {
		t.Fatal("create_vm admitted an audit whose visibility is unproven")
	}
	var refusal *cpierrors.Error
	if !errors.As(err, &refusal) || refusal.Type() != cpierrors.TypeCloud || refusal.OkToRetry() || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("refusal %T %v is not a non-retriable CloudError", err, err)
	}
	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithTransientClassifier(pve.IsTransientTransport))
	if err := d.Register("create_vm", cpi.HandlerFunc(func(context.Context, []json.RawMessage, jsonrpc.Context) (any, error) { return nil, err })); err != nil {
		t.Fatal(err)
	}
	body := d.Handle(context.Background(), &jsonrpc.Request{Method: "create_vm"}).Error
	if body == nil || body.Type != string(cpierrors.TypeCloud) || body.OkToRetry {
		t.Fatalf("Director received %+v, want a CloudError with ok_to_retry false", body)
	}
}

// TestUnprovenVisibilityIssueNamesTheFailedRead pins the VM-scan issue a
// timed-out visibility read raises, which used to say "unclassified error".
func TestUnprovenVisibilityIssueNamesTheFailedRead(t *testing.T) {
	deps, j, c := auditFixture(t)
	deps.PVE = &timedOutVisibilityClient{c}
	err := admitStorageVMAllocation(t.Context(), deps, j, []string{"pve1"}, "agent")
	want := "cluster-wide VM and storage audit visibility is unproven: could not read effective permissions at /access (context deadline exceeded)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("create_vm refusal = %v, want it to contain %q", err, want)
	}
}
