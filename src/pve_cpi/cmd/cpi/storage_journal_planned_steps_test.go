package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

const (
	plannedStepAuditHeader = "audit: complete=true vm_scan_complete=true generation_index_healthy=true cluster_continuity=true records=1\n"
	protectionStepKind     = "lifecycle_attach_disk_Nodes_UpdateQemuConfig"
	protectionOnParameters = `{"version":1,"kind":"parker_protection_on"}`
)

func plannedStepTestRecord(id string, state aj.State, steps ...aj.Step) aj.Record {
	now := time.Now().UTC()
	record := storageJournalAuditTestRecord(id, state, now, now)
	record.Steps = steps
	return record
}

func writePlannedStepAudit(t *testing.T, report handlers.StorageAllocationAudit, summary bool) (int, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := writeStorageJournalAudit(&out, &stderr, report, nil, true, summary)
	return code, out.String()
}

func TestStorageJournalAuditSummaryNamesAPlannedStep(t *testing.T) {
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{plannedStepTestRecord("returned-1", aj.ReadyToReturn,
			aj.Step{ID: "attempt-0-step-3", Kind: "lifecycle_create_vm_QEMU_Create", Target: aj.Target{Node: "n1", VMID: 4356}, State: aj.Planned})},
	}
	code, out := writePlannedStepAudit(t, report, true)
	want := plannedStepAuditHeader +
		"planned step: record=returned-1 step=attempt-0-step-3 kind=lifecycle_create_vm_QEMU_Create\n" +
		"charging: none\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, summary = %q, want %q", code, out, want)
	}
}

func TestStorageJournalAuditSummaryNamesTheParkerOfAProtectionStep(t *testing.T) {
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{plannedStepTestRecord("returned-1", aj.ReadyToReturn,
			aj.Step{ID: "attempt-0-step-4", Kind: protectionStepKind, Target: aj.Target{Node: "lab-pmx-0", VMID: 90372}, State: aj.Planned, Parameters: json.RawMessage(protectionOnParameters)})},
	}
	code, out := writePlannedStepAudit(t, report, true)
	want := plannedStepAuditHeader +
		"planned step: record=returned-1 step=attempt-0-step-4 kind=" + protectionStepKind + " parker_vmid=90372 node=lab-pmx-0\n" +
		"charging: none\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, summary = %q, want %q", code, out, want)
	}
}

func TestStorageJournalAuditSummaryLeavesOutAParkerForAnOtherwiseShapedWrite(t *testing.T) {
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{plannedStepTestRecord("returned-1", aj.ReadyToReturn,
			aj.Step{ID: "attempt-0-step-4", Kind: protectionStepKind, Target: aj.Target{Node: "n1", VMID: 90372}, State: aj.Planned},
			aj.Step{ID: "attempt-0-step-5", Kind: protectionStepKind, Target: aj.Target{Node: "n1", VMID: 90372}, State: aj.Planned, Parameters: json.RawMessage(`{"version":1,"kind":"parker_protection_on","scsi1":"x"}`)})},
	}
	_, out := writePlannedStepAudit(t, report, true)
	if strings.Contains(out, "parker_vmid=") || strings.Count(out, "planned step: ") != 2 {
		t.Fatalf("summary = %q", out)
	}
}

func TestStorageJournalAuditSummaryPrintsNoLineWithoutAPlannedStep(t *testing.T) {
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{plannedStepTestRecord("returned-1", aj.ReadyToReturn,
			aj.Step{ID: "attempt-0-step-3", Kind: protectionStepKind, Target: aj.Target{Node: "n1", VMID: 90372}, State: aj.Observed, Parameters: json.RawMessage(protectionOnParameters)},
			aj.Step{ID: "attempt-0-step-4", Kind: "lifecycle_create_vm_QEMU_Create", Target: aj.Target{Node: "n1", VMID: 4356}, State: aj.Submitted},
			aj.Step{ID: "attempt-0-step-5", Kind: "lifecycle_create_vm_QEMU_Start", Target: aj.Target{Node: "n1", VMID: 4356}, State: aj.ReconciliationRequired})},
	}
	code, out := writePlannedStepAudit(t, report, true)
	if code != 0 || out != plannedStepAuditHeader+"charging: none\n" {
		t.Fatalf("exit %d, summary = %q", code, out)
	}
}

func TestStorageJournalAuditSummaryListsPlannedStepsInRecordOrder(t *testing.T) {
	step := func(id string) aj.Step {
		return aj.Step{ID: id, Kind: "lifecycle_create_vm_QEMU_Create", Target: aj.Target{Node: "n1", VMID: 1}, State: aj.Planned}
	}
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{
			plannedStepTestRecord("b-record", aj.ReadyToReturn, step("s1"), step("s2")),
			plannedStepTestRecord("a-record", aj.Adopted, step("s9")),
		},
	}
	_, out := writePlannedStepAudit(t, report, true)
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "planned step: ") {
			got = append(got, strings.Fields(line)[2]+" "+strings.Fields(line)[3])
		}
	}
	want := []string{"record=a-record step=s9", "record=b-record step=s1", "record=b-record step=s2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("planned step lines = %q, want %q", got, want)
	}
}

func TestStorageJournalAuditSummaryPlannedStepsKeepTheExitCode(t *testing.T) {
	records := []aj.Record{plannedStepTestRecord("returned-1", aj.ReadyToReturn,
		aj.Step{ID: "attempt-0-step-3", Kind: protectionStepKind, Target: aj.Target{Node: "n1", VMID: 90372}, State: aj.Planned, Parameters: json.RawMessage(protectionOnParameters)})}
	for _, test := range []struct {
		name           string
		report         handlers.StorageAllocationAudit
		indexErr       error
		continuity     bool
		wantExitStatus int
	}{
		{"complete", handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true, Records: records}, nil, true, 0},
		{"audit incomplete", handlers.StorageAllocationAudit{Complete: false, VMScanComplete: true, Records: records}, nil, true, 1},
		{"vm scan incomplete", handlers.StorageAllocationAudit{Complete: true, VMScanComplete: false, Records: records}, nil, true, 1},
		{"index unhealthy", handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true, Records: records}, aj.ErrCorrupt, true, 1},
		{"continuity lost", handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true, Records: records}, nil, false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var summary, jsonOut, stderr bytes.Buffer
			textCode := writeStorageJournalAudit(&summary, &stderr, test.report, test.indexErr, test.continuity, true)
			jsonCode := writeStorageJournalAudit(&jsonOut, &stderr, test.report, test.indexErr, test.continuity, false)
			if textCode != test.wantExitStatus || jsonCode != test.wantExitStatus {
				t.Fatalf("summary exited %d, JSON exited %d, want %d", textCode, jsonCode, test.wantExitStatus)
			}
			if !strings.Contains(summary.String(), "planned step: record=returned-1 ") {
				t.Fatalf("summary = %q", summary.String())
			}
		})
	}
}

// The JSON output already carries every record, so the planned step line must
// not change it. A report with a planned protection step prints the same
// fields as before, and nothing in it names a planned step or a parker.
func TestStorageJournalAuditJSONIgnoresPlannedSteps(t *testing.T) {
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{plannedStepTestRecord("returned-1", aj.ReadyToReturn,
			aj.Step{ID: "attempt-0-step-3", Kind: protectionStepKind, Target: aj.Target{Node: "n1", VMID: 90372}, State: aj.Planned, Parameters: json.RawMessage(protectionOnParameters)})},
	}
	_, out := writePlannedStepAudit(t, report, false)
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "audit,charging_summary,cluster_continuity,generation_index_healthy,records"; got != want {
		t.Fatalf("top-level fields = %s, want %s", got, want)
	}
	var records []map[string]any
	if err := json.Unmarshal(decoded["records"], &records); err != nil || len(records) != 1 {
		t.Fatalf("records = %s: %v", decoded["records"], err)
	}
	recordKeys := make([]string, 0, len(records[0]))
	for key := range records[0] {
		recordKeys = append(recordKeys, key)
	}
	sort.Strings(recordKeys)
	if got, want := strings.Join(recordKeys, ","), "CID,CreatedAt,ID,Kind,SHA256,State,UpdatedAt,charging"; got != want {
		t.Fatalf("record fields = %s, want %s", got, want)
	}
	if strings.Contains(out, "planned step") || strings.Contains(out, "parker_vmid") {
		t.Fatalf("JSON names a planned step: %s", out)
	}
}
