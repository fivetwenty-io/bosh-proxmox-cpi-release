package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const gateTestCommand = "sudo -u vcap /var/vcap/packages/pve_cpi/bin/cpi storage-journal audit --summary --config /var/vcap/jobs/pve_cpi/config/cpi.json"

func gateTestReport() StorageAllocationAudit {
	report := StorageAllocationAudit{Complete: false, VMScanComplete: false}
	report.addConflict("remote allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9 (VM 4626) is outside recorded mutation targets: observed on pvupvecf102, recorded pvupvecf101 (node_mismatch)", "VM 4626 on pvupvecf102, recorded pvupvecf101 (node_mismatch), allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9")
	report.addConflict("remote allocation 2a4b0f7c-1c2d-4e5f-8a9b-0c1d2e3f4a5b (VM 7014) is outside recorded mutation targets: observed on pvupvecf103, recorded pvupvecf102 (node_mismatch)", "VM 7014 on pvupvecf103, recorded pvupvecf102 (node_mismatch), allocation 2a4b0f7c-1c2d-4e5f-8a9b-0c1d2e3f4a5b")
	markVMScanIncomplete(&report, "some cluster nodes could not be inspected: pvupvecf104 (reported offline by /cluster/status)")
	report.Issues = append(report.Issues, `storage "nas" on node "pvupvecf101" could not be inspected: context deadline exceeded`)
	return report
}

func TestStorageAuditGateErrorAdmitsPassingReports(t *testing.T) {
	deps := Deps{}
	clean := StorageAllocationAudit{Complete: true, VMScanComplete: true}
	if err := storageAuditGateError(context.Background(), deps, "delete_disk", clean, storageAuditGateAll); err != nil {
		t.Fatalf("clean report refused: %v", err)
	}
	storageOnly := StorageAllocationAudit{Complete: false, VMScanComplete: true, Issues: []string{"storage outage"}}
	if err := storageAuditGateError(context.Background(), deps, "create_vm", storageOnly, storageAuditGateVMScan|storageAuditGateConflicts); err != nil {
		t.Fatalf("VM gate refused a storage-only issue it tolerates: %v", err)
	}
	if err := storageAuditGateError(context.Background(), deps, "delete_vm", storageOnly, storageAuditGateComplete); err == nil {
		t.Fatal("complete gate admitted an incomplete audit")
	}
}

func TestStorageAuditGateErrorListsOnlyTheFailedGate(t *testing.T) {
	report := gateTestReport()
	for _, tc := range []struct {
		name    string
		gate    storageAuditGate
		lead    string
		include []string
		exclude []string
	}{
		{"VM scan and conflicts", storageAuditGateVMScan | storageAuditGateConflicts,
			"create_vm refused: 2 audit conflicts, 1 VM-scan issue; VM 4626 on pvupvecf102",
			[]string{"VM 7014 on pvupvecf103", "pvupvecf104"}, []string{`storage "nas"`}},
		{"VM scan only", storageAuditGateVMScan,
			"create_vm refused: 1 VM-scan issue; some cluster nodes could not be inspected: pvupvecf104",
			nil, []string{"VM 4626", `storage "nas"`}},
		{"conflicts only", storageAuditGateConflicts,
			"create_vm refused: 2 audit conflicts; VM 4626 on pvupvecf102, recorded pvupvecf101 (node_mismatch), allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9; VM 7014",
			nil, []string{"pvupvecf104", `storage "nas"`}},
		{"complete", storageAuditGateComplete,
			"create_vm refused: 2 audit conflicts, 2 audit issues; VM 4626 on pvupvecf102",
			[]string{"VM 7014", "and 1 more"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := storageAuditGateError(context.Background(), Deps{StorageAuditCommand: gateTestCommand}, "create_vm", report, tc.gate)
			if err == nil {
				t.Fatal("failing report admitted")
			}
			message := err.Error()
			if !strings.HasPrefix(message, tc.lead) {
				t.Fatalf("message = %q, want prefix %q", message, tc.lead)
			}
			for _, want := range tc.include {
				if !strings.Contains(message, want) {
					t.Fatalf("message = %q, want it to name %q", message, want)
				}
			}
			for _, unwanted := range tc.exclude {
				if strings.Contains(message, unwanted) {
					t.Fatalf("message = %q names %q, which that gate tolerates", message, unwanted)
				}
			}
			if !strings.HasSuffix(message, "; run '"+gateTestCommand+"' for the full report") {
				t.Fatalf("message = %q, want the audit command last", message)
			}
			if !cpierrors.IsType(err, cpierrors.TypeCloud) {
				t.Fatalf("gate error is not a CloudError: %T", err)
			}
		})
	}
}

// TestStorageAuditGateErrorLeadsWithinDirectorTaskResult pins the ordering
// the Director's task list depends on: it keeps only about 75 characters of
// a CPI error, so the count and the first VM must come first.
func TestStorageAuditGateErrorLeadsWithinDirectorTaskResult(t *testing.T) {
	err := storageAuditGateError(context.Background(), Deps{}, "create_vm", gateTestReport(), storageAuditGateConflicts)
	if err == nil {
		t.Fatal("conflicting report admitted")
	}
	visible := err.Error()[:75]
	if !strings.Contains(visible, "2 audit conflicts") || !strings.Contains(visible, "VM 4626") {
		t.Fatalf("first 75 characters %q lack the count and the first VM", visible)
	}
}

func TestStorageAuditGateErrorCapsFindings(t *testing.T) {
	report := StorageAllocationAudit{}
	for _, vmid := range []string{"101", "102", "103", "104", "105"} {
		report.addConflict("remote allocation x (VM "+vmid+") is outside recorded mutation targets", "VM "+vmid+" on pve2")
	}
	message := storageAuditGateError(context.Background(), Deps{}, "delete_vm", report, storageAuditGateConflicts).Error()
	if !strings.Contains(message, "VM 101 on pve2; VM 102 on pve2; VM 103 on pve2; and 2 more audit conflicts (VM 104, VM 105);") {
		t.Fatalf("message = %q, want three findings and a remainder that names the rest", message)
	}
	if strings.Contains(message, "VM 104 on pve2") {
		t.Fatalf("message = %q lists more than three findings", message)
	}
}

func TestStorageAuditGateErrorFallsBackWithoutCommand(t *testing.T) {
	message := storageAuditGateError(context.Background(), Deps{}, "delete_disk", gateTestReport(), storageAuditGateAll).Error()
	if !strings.HasSuffix(message, "; run storage-journal audit --summary as the journal owner on the host that runs this CPI") {
		t.Fatalf("message = %q, want the fallback hint", message)
	}
}

func TestStorageAuditGateErrorLogsTheFullReport(t *testing.T) {
	logger, observed := log.NewObservedLogger(slog.LevelDebug)
	report := gateTestReport()
	report.Issues = append(report.Issues, "echoed header PVEAPIToken=root@pam!cpi=gate-secret")
	err := storageAuditGateError(context.Background(), Deps{Logger: logger}, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts)
	if err == nil {
		t.Fatal("failing report admitted")
	}
	entries := observed.All()
	if len(entries) != 1 || entries[0].Level != slog.LevelError {
		t.Fatalf("want exactly one Error entry, got %+v", entries)
	}
	attrs := entries[0].Attrs
	for key, want := range map[string]any{"operation": "create_vm", "vm_scan_complete": false, "complete": false, "conflict_count": int64(2), "vm_scan_issue_count": int64(1), "issue_count": int64(3)} {
		if attrs[key] != want {
			t.Fatalf("log field %s = %#v, want %#v (all: %v)", key, attrs[key], want, attrs)
		}
	}
	for key, want := range map[string]string{"conflicts": "(VM 7014)", "vm_scan_issues": "pvupvecf104", "issues": `storage "nas"`} {
		if value, _ := attrs[key].(string); !strings.Contains(value, want) {
			t.Fatalf("log field %s = %q, want it to contain %q", key, value, want)
		}
	}
	if value, _ := attrs["issues"].(string); strings.Contains(value, "gate-secret") {
		t.Fatalf("logged issues were not scrubbed: %q", value)
	}
	// The full report goes to the log; the error itself still caps at three.
	if strings.Contains(err.Error(), "gate-secret") {
		t.Fatal("gate error carried an unscrubbed finding")
	}
}

func TestStorageAllocationDecisionFailureAppendsGateSummary(t *testing.T) {
	gate := storageAuditGateError(context.Background(), Deps{StorageAuditCommand: gateTestCommand}, "allocation disposition", gateTestReport(), storageAuditGateConflicts)
	got := StorageAllocationDecisionFailure(storageDecisionSourceError(gate))
	if !strings.HasPrefix(got, "identity_or_audit_evidence: 2 audit conflicts; VM 4626 on pvupvecf102") {
		t.Fatalf("decision failure = %q, want the class and the gate summary", got)
	}
	if strings.Contains(got, "storage-journal audit --summary") {
		t.Fatalf("decision failure = %q repeats the audit command to the CLI that ran it", got)
	}
	staged := StorageAllocationDecisionFailure(storageCleanupFailure("historical_audit", gate))
	if !strings.HasPrefix(staged, "cleanup_historical_audit: 2 audit conflicts;") {
		t.Fatalf("staged decision failure = %q, want the stage and the gate summary", staged)
	}
	// An error the CPI did not type is described, never echoed.
	if got := StorageAllocationDecisionFailure(errors.New("password=private")); got != "identity_or_audit_evidence: unclassified error" {
		t.Fatalf("plain failure = %q, want the class and the unclassified description", got)
	}
	api := StorageAllocationDecisionFailure(storageDecisionSourceError(fmt.Errorf("read failed: %w", sdkerrors.ParseAPIError(500, []byte(`{"message":"storage 'nas' is not online"}`)))))
	if api != "identity_or_audit_evidence: HTTP 500: storage 'nas' is not online" {
		t.Fatalf("API failure = %q, want its classified description", api)
	}
	if got := StorageAllocationDecisionFailure(nil); got != "identity_or_audit_evidence" {
		t.Fatalf("nil failure = %q, want the bare class", got)
	}
}

// TestStorageAllocationDecisionFailureNamesCPIRefusals pins that a refusal the
// CPI wrote itself reaches the storage-journal CLI with its reason, wherever
// it sits in the chain, while backend text beside it stays out.
func TestStorageAllocationDecisionFailureNamesCPIRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"fixed refusal", storageDecisionSourceError(storageRefusal("pending stop lacks exact VM ownership")),
			"identity_or_audit_evidence: pending stop lacks exact VM ownership"},
		{"pending allocation moved", storageCleanupFailure("pending_mutation_settlement", pendingVMAllocationMoved(aj.Record{ID: "a1"}, aj.Step{Target: aj.Target{Node: "pve1", VMID: 123}}, "pve2", 123)),
			"cleanup_pending_mutation_settlement: pending VM allocation a1 moved from exact target: VM 123 observed on pve2, recorded VM 123 on pve1"},
		{"readback outside recorded nodes", storageDecisionSourceError(gateTestReadback().admitLocation(StorageAllocationAudit{})),
			"identity_or_audit_evidence: actual VM is outside recorded nodes: VM 123 on pve2, recorded pve1, and the audit accepted no move"},
		{"joined with an uncertain outcome", errors.Join(storageRefusal("VM stop not observed"), errors.New("Authorization: PVEAPIToken=root@pam!cpi=leaked")),
			"identity_or_audit_evidence: VM stop not observed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StorageAllocationDecisionFailure(tc.err)
			if got != tc.want {
				t.Fatalf("decision failure = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "leaked") {
				t.Fatalf("decision failure leaked backend text: %q", got)
			}
		})
	}
	// The Director still reads the readback refusal exactly as before.
	readback := gateTestReadback().admitLocation(StorageAllocationAudit{})
	if message := directorMessage(readback); message != "allocation a1 requires reconciliation: actual VM is outside recorded nodes: VM 123 on pve2, recorded pve1, and the audit accepted no move" {
		t.Fatalf("Director message = %q", message)
	}
}

// gateTestReadback is a readback of VM 123, recorded on pve1, found on pve2.
func gateTestReadback() *managedVMRecordReadback {
	record := aj.Record{ID: "a1", Steps: []aj.Step{{Target: aj.Target{Node: "pve1", VMID: 123}}}}
	return &managedVMRecordReadback{record: record, vmid: 123, node: "pve2"}
}

// TestStorageAuditGateHintQuotesACommandThatQuotesItself sets the command
// off in double quotes when one of its arguments is single-quoted, so the
// text between the quotes still pastes into a shell.
func TestStorageAuditGateHintQuotesACommandThatQuotesItself(t *testing.T) {
	command := "/home/op/cpi storage-journal audit --summary --config '/home/op/a b/cpi.json'"
	err := storageAuditGateError(context.Background(), Deps{StorageAuditCommand: command}, "create_vm", gateTestReport(), storageAuditGateConflicts)
	if err == nil || !strings.HasSuffix(err.Error(), `; run "`+command+`" for the full report`) {
		t.Fatalf("gate error = %v, want the command in double quotes", err)
	}
	err = storageAuditGateError(context.Background(), Deps{StorageAuditCommand: gateTestCommand}, "create_vm", gateTestReport(), storageAuditGateConflicts)
	if err == nil || !strings.HasSuffix(err.Error(), "; run '"+gateTestCommand+"' for the full report") {
		t.Fatalf("gate error = %v, want the command in single quotes", err)
	}
}

func TestAdmissionRefusalsNameTheirFindings(t *testing.T) {
	deps, j, c := auditFixture(t)
	deps.StorageAuditCommand = gateTestCommand
	c.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
	err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent")
	if err == nil || !strings.HasPrefix(err.Error(), "create_vm refused: 1 audit conflict; VM 123 on pve1 carries unknown allocation "+findingAllocationID) {
		t.Fatalf("VM admission error = %v", err)
	}

	diskDeps, diskJournal, diskClient := auditFixture(t)
	diskClient.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
	_, err = admitStorageAllocation(context.Background(), diskDeps, diskJournal, []string{"pve1"})
	if err == nil || !strings.HasPrefix(err.Error(), "storage allocation admission refused: 1 audit conflict; VM 123 on pve1") {
		t.Fatalf("disk admission error = %v", err)
	}
}
