package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

func TestStorageJournalSummaryIsRejectedOutsideAudit(t *testing.T) {
	for _, command := range [][]string{
		{"initialize", "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"},
		{"recover-authority", "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced"},
		{"recover-index", "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"},
		{"resolve-missing-vm", "--config", "/unopened", "--authority-id", "writer", "--previous-writer-fenced", "--agent-id", "agent", "--allocation-id", "allocation", "--index-first-crash-confirmed"},
		{"adopt", "--config", "/unopened", "--allocation-id", "allocation", "--decision-id", "ticket", "--expected-cid", "cid"},
		{"finalize-cleanup", "--config", "/unopened", "--allocation-id", "allocation", "--decision-id", "ticket"},
		{"cleanup", "--config", "/unopened", "--allocation-id", "allocation", "--decision-id", "ticket"},
	} {
		t.Run(command[0], func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := runStorageJournal(command, &out, &stderr, runOptions{}); code == 2 {
				// Without --summary the same flags pass validation and fail
				// later, on the unopened config, with exit code 1.
				t.Fatalf("valid flags refused: %s", stderr.String())
			}
			stderr.Reset()
			code := runStorageJournal(append(command, "--summary"), &out, &stderr, runOptions{})
			if code != 2 || !strings.Contains(stderr.String(), "usage: cpi storage-journal ") || !strings.Contains(stderr.String(), "--summary applies only to audit and audit-enrollment") {
				t.Fatalf("--summary accepted for %s: %d %q", command[0], code, stderr.String())
			}
		})
	}
	for _, action := range []string{"audit", "audit-enrollment"} {
		var out, stderr bytes.Buffer
		if code := runStorageJournal([]string{action, "--config", "/unopened", "--summary"}, &out, &stderr, runOptions{}); code == 2 {
			t.Fatalf("--summary refused for %s: %s", action, stderr.String())
		}
	}
}

func TestStorageJournalAuditSummaryCleanAudit(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour)
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{storageJournalAuditTestRecord("returned-1", aj.ReadyToReturn, created, created)},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, nil, true, true); code != 0 {
		t.Fatalf("clean audit exited %d: %s", code, stderr.String())
	}
	want := "audit: complete=true vm_scan_complete=true generation_index_healthy=true cluster_continuity=true records=1\n" +
		"charging: none\n"
	if out.String() != want {
		t.Fatalf("summary = %q, want %q", out.String(), want)
	}
}

func TestStorageJournalAuditSummaryFieldCase(t *testing.T) {
	now := time.Now().UTC()
	vmScanIssue := "some cluster nodes could not be inspected: pvupvecf104 (reported offline by /cluster/status)"
	report := handlers.StorageAllocationAudit{
		Complete: false, VMScanComplete: false,
		Conflicts: []string{
			"disk ownership provenance disagrees with actual holder node: disk allocation 9c2e (volume nas:vm-4626-disk-1) held by VM 4626 on pvupvecf102, provenance names pvupvecf101",
			"remote allocation 180f7d1e (VM 4626) is outside recorded mutation targets: observed on pvupvecf102, recorded pvupvecf101 (node_mismatch)",
			"remote allocation 2a4b0f7c (VM 7014) is outside recorded mutation targets: observed on pvupvecf103, recorded pvupvecf102 (node_mismatch)\nwith an injected line",
		},
		Issues:                  []string{vmScanIssue, `storage "nas" on node "pvupvecf101" could not be inspected: context deadline exceeded`},
		VMScanIssues:            []string{vmScanIssue},
		SkippedDisabledStorages: []string{"archive", "old-nfs"},
		Records: []aj.Record{
			storageJournalAuditTestRecord("planned-1", aj.Planned, now.Add(-2*time.Hour), now),
			storageJournalAuditTestRecord("returned-1", aj.ReadyToReturn, now, now),
		},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, nil, true, true); code != 1 {
		t.Fatalf("incomplete audit exited %d", code)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []string{
		"audit: complete=false vm_scan_complete=false generation_index_healthy=true cluster_continuity=true records=2",
		"conflict: " + report.Conflicts[0],
		"conflict: " + report.Conflicts[1],
		"conflict: remote allocation 2a4b0f7c (VM 7014) is outside recorded mutation targets: observed on pvupvecf103, recorded pvupvecf102 (node_mismatch) with an injected line",
		"vm-scan issue: " + vmScanIssue,
		`issue: storage "nas" on node "pvupvecf101" could not be inspected: context deadline exceeded`,
		"runbook: " + storageJournalTestRunbook,
		"record: id=planned-1 kind=vm state=planned charging=true cid=none reason=none",
	}
	if len(lines) != len(want)+2 {
		t.Fatalf("summary has %d lines, want %d: %q", len(lines), len(want)+2, out.String())
	}
	for i, line := range want {
		if lines[i] != line {
			t.Fatalf("line %d = %q, want %q", i, lines[i], line)
		}
	}
	if !strings.HasPrefix(lines[len(want)], "charging: 1 record, oldest planned-1 opened 2h0m") {
		t.Fatalf("charging line = %q", lines[len(want)])
	}
	if lines[len(want)+1] != "skipped disabled storages: archive, old-nfs" {
		t.Fatalf("skipped storage line = %q", lines[len(want)+1])
	}
}

func TestStorageJournalAuditSummaryNamesAnUnhealthyIndex(t *testing.T) {
	report := handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, aj.ErrCorrupt, false, true); code != 1 {
		t.Fatalf("unhealthy index exited %d", code)
	}
	want := "audit: complete=true vm_scan_complete=true generation_index_healthy=false cluster_continuity=false records=0\n" +
		"generation index: generation index invalid or unavailable; record listing does not establish healthy authority\n" +
		"charging: none\n"
	if out.String() != want {
		t.Fatalf("summary = %q, want %q", out.String(), want)
	}
}

func TestStorageJournalEnrollmentSummary(t *testing.T) {
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Evidence: []handlers.StorageAllocationEvidence{
			{AllocationID: "a1", Kind: "vm", Node: "pve1", VMID: 4626},
			{AllocationID: "a1", Kind: "volume", Node: "pve1", VMID: 4626, VolumeID: "nas:vm-4626-disk-0"},
		},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalEnrollment(&out, &stderr, report, true); code != 0 {
		t.Fatalf("complete enrollment audit exited %d", code)
	}
	want := "audit-enrollment: complete=true vm_scan_complete=true evidence=2\n" +
		"provenance: allocation a1 (vm) on pve1, VM 4626\n" +
		"provenance: allocation a1 (volume) on pve1, VM 4626, volume nas:vm-4626-disk-0\n"
	if out.String() != want {
		t.Fatalf("summary = %q, want %q", out.String(), want)
	}
	out.Reset()
	incomplete := handlers.StorageAllocationAudit{Complete: false, VMScanComplete: false, Issues: []string{"visibility unproven"}, VMScanIssues: []string{"visibility unproven"}}
	if code := writeStorageJournalEnrollment(&out, &stderr, incomplete, true); code != 1 {
		t.Fatalf("incomplete enrollment audit exited %d", code)
	}
	if out.String() != "audit-enrollment: complete=false vm_scan_complete=false evidence=0\nvm-scan issue: visibility unproven\n" {
		t.Fatalf("incomplete enrollment summary = %q", out.String())
	}
}

// One fixture runs in both modes, so the text summary cannot drift from the
// JSON report's exit-code rule.
func TestStorageJournalSummaryAndJSONShareExitCodes(t *testing.T) {
	f := newStorageJournalFixture(t)
	compare := func(stage string, actions ...string) {
		t.Helper()
		for _, action := range actions {
			jsonCode, jsonOut := f.run(action)
			textCode, textOut := f.run(action, "--summary")
			if jsonCode != textCode {
				t.Fatalf("%s %s: JSON exited %d, summary exited %d\n%s\n%s", stage, action, jsonCode, textCode, jsonOut, textOut)
			}
			if !strings.HasPrefix(jsonOut, "{") || strings.HasPrefix(textOut, "{") || !strings.HasPrefix(textOut, action+": complete=") {
				t.Fatalf("%s %s: output modes not honored\n%s\n%s", stage, action, jsonOut, textOut)
			}
		}
	}
	compare("healthy", "audit", "audit-enrollment")
	if code, out := f.run("audit", "--summary"); code != 0 {
		t.Fatalf("healthy summary exited %d: %s", code, out)
	}
	journalFixtureVM(t, f)
	f.restricted.Store(true)
	compare("restricted", "audit", "audit-enrollment")
	if code, out := f.run("audit", "--summary"); code != 1 || !strings.Contains(out, "vm-scan issue: ") {
		t.Fatalf("restricted summary exited %d: %s", code, out)
	}
	f.restricted.Store(false)
	if err := os.Remove(journalFixtureFile(t, f.directory, "index.json")); err != nil {
		t.Fatal(err)
	}
	compare("index lost", "audit")
	if code, out := f.run("audit", "--summary"); code != 1 || !strings.Contains(out, "generation_index_healthy=false") {
		t.Fatalf("index-lost summary exited %d: %s", code, out)
	}
}

// An accepted move prints after the findings, naming the kind, allocation,
// VM, recorded nodes, and observed node, so an operator can see which moves
// the audit accepted without reading JSON.
func TestStorageJournalSummaryListsObservedMoves(t *testing.T) {
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		ObservedMoves: []handlers.StorageAllocationMove{
			{AllocationID: "180f7d1e", Kind: "vm", VMID: 4626, RecordedNodes: []string{"pvupvecf101"}, ObservedNode: "pvupvecf102"},
		},
	}
	lines := storageJournalFindingLines(report)
	want := []string{"observed move: vm allocation 180f7d1e (VM 4626) moved from pvupvecf101 to pvupvecf102"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
}

func TestStorageJournalAuditSummaryOmitsHealthyRecords(t *testing.T) {
	now := time.Now().UTC()
	adopted := storageJournalAuditTestRecord("adopted-1", aj.Adopted, now, now)
	adopted.CID = "pvd-adopted"
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{
			storageJournalAuditTestRecord("returned-1", aj.ReadyToReturn, now, now),
			adopted,
			storageJournalAuditTestRecord("cleaned-1", aj.Cleaned, now, now),
			storageJournalAuditTestRecord("deleted-1", aj.Deleted, now, now),
		},
		Evidence: []handlers.StorageAllocationEvidence{{AllocationID: "adopted-1", Kind: "disk", Node: "n1", VMID: 90372, VolumeID: "nas:24192/vm-24192-disk.qcow2"}},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, nil, true, true); code != 0 {
		t.Fatalf("healthy audit exited %d: %s", code, stderr.String())
	}
	want := "audit: complete=true vm_scan_complete=true generation_index_healthy=true cluster_continuity=true records=4\n" +
		"charging: none\n"
	if out.String() != want {
		t.Fatalf("summary = %q, want %q", out.String(), want)
	}
}

// TestStorageJournalAuditSummaryNamesARecordNeedingReconciliation is the shape
// an operator reconciles by hand: a returned disk a lifecycle left in
// reconciliation_required, with the parker still holding its volume.
func TestStorageJournalAuditSummaryNamesARecordNeedingReconciliation(t *testing.T) {
	now := time.Now().UTC()
	disk := storageJournalAuditTestRecord("65a2e32a-0ec7-4dd8-bfc3-8ba70f2dfcf3", aj.ReconciliationRequired, now, now)
	disk.Kind = "disk"
	disk.CID = "pvz-H4sIAAAAAAAC_zTMXW6DMBAE4LvMs7cF8xd8m"
	disk.Reason = "outcome requires reconciliation at lifecycle attach_disk Pool.CreatePool"
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{storageJournalAuditTestRecord("returned-1", aj.ReadyToReturn, now, now), disk},
		Evidence: []handlers.StorageAllocationEvidence{
			{AllocationID: "returned-1", Kind: "vm", Node: "lab-pmx-0", VMID: 4356},
			{AllocationID: disk.ID, Kind: "disk", Node: "lab-pmx-0", VMID: 90372, VolumeID: "nfs-images:24192/vm-24192-bosh-alloc.qcow2"},
		},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, nil, true, true); code != 0 {
		t.Fatalf("audit exited %d: %s", code, stderr.String())
	}
	want := "audit: complete=true vm_scan_complete=true generation_index_healthy=true cluster_continuity=true records=2\n" +
		"record: id=65a2e32a-0ec7-4dd8-bfc3-8ba70f2dfcf3 kind=disk state=reconciliation_required charging=true cid=pvz-H4sIAAAAAAAC_zTMXW6DMBAE4LvMs7cF8xd8m reason=\"outcome requires reconciliation at lifecycle attach_disk Pool.CreatePool\"\n" +
		"evidence: allocation=65a2e32a-0ec7-4dd8-bfc3-8ba70f2dfcf3 kind=disk volume=nfs-images:24192/vm-24192-bosh-alloc.qcow2 node=lab-pmx-0 holder_vmid=90372\n"
	if !strings.HasPrefix(out.String(), want) || !strings.Contains(out.String(), "\ncharging: 1 record, oldest 65a2e32a-0ec7-4dd8-bfc3-8ba70f2dfcf3") {
		t.Fatalf("summary = %q, want it to open with %q", out.String(), want)
	}
}

// TestStorageJournalAuditSummaryNamesChargingRecords prints every charging
// record in ID order, with its evidence sorted and a scrubbed, one-line reason.
func TestStorageJournalAuditSummaryNamesChargingRecords(t *testing.T) {
	now := time.Now().UTC()
	planned := storageJournalAuditTestRecord("b-planned", aj.Planned, now.Add(-time.Hour), now)
	observed := storageJournalAuditTestRecord("a-observed", aj.Observed, now, now)
	observed.Kind = "disk"
	observed.Reason = "lifecycle attach_disk admitted; completion pending\nPVEAPIToken=root@pam!cpi=secret-value"
	report := handlers.StorageAllocationAudit{
		Complete: true, VMScanComplete: true,
		Records: []aj.Record{planned, observed},
		Evidence: []handlers.StorageAllocationEvidence{
			{AllocationID: "a-observed", Kind: "disk", Node: "n2", VMID: 90000, VolumeID: "nas:1/b.qcow2"},
			{AllocationID: "a-observed", Kind: "disk", Node: "n1", VolumeID: "nas:1/a.qcow2"},
		},
	}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, nil, true, true); code != 0 {
		t.Fatalf("audit exited %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("summary has %d lines: %q", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[1], "record: id=a-observed kind=disk state=observed charging=true cid=none reason=\"lifecycle attach_disk admitted; completion pending ") || strings.Contains(out.String(), "secret-value") {
		t.Fatalf("record line = %q", lines[1])
	}
	if lines[2] != "evidence: allocation=a-observed kind=disk volume=nas:1/a.qcow2 node=n1 holder_vmid=none" ||
		lines[3] != "evidence: allocation=a-observed kind=disk volume=nas:1/b.qcow2 node=n2 holder_vmid=90000" {
		t.Fatalf("evidence lines = %q", lines[2:4])
	}
	if lines[4] != "record: id=b-planned kind=vm state=planned charging=true cid=none reason=none" {
		t.Fatalf("second record line = %q", lines[4])
	}
	if !strings.HasPrefix(lines[5], "charging: 2 records, oldest b-planned") {
		t.Fatalf("charging line = %q", lines[5])
	}
}

// TestStorageJournalAuditSummaryNamesAListingStop prints the observed VM
// record that delete_vm leaves when a storage's content listing stopped its
// cleanup. The reason comes from the cleanup's own save on a journal record,
// so the record line carries the whole reason the cleanup saved, with the
// storage, the node, the listing's reason, and what to rerun.
func TestStorageJournalAuditSummaryNamesAListingStop(t *testing.T) {
	f := newStorageJournalFixture(t)
	j := f.open(t)
	fingerprint := strings.Repeat("a", 64)
	plan, err := json.Marshal(map[string]any{"Version": 1, "Namespace": "director", "AllocationKey": "agent", "PolicyFingerprint": fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: fingerprint, PolicyFingerprint: fingerprint, PlanVersion: 1, Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	id := handle.Record().ID
	record := handle.Record()
	record.Steps = append(record.Steps, aj.Step{ID: "attempt-0-step-0", Kind: "Nodes.CreateQemu", Attempt: record.ActiveAttempt(), Target: aj.Target{Node: "lab-pmx-0", VMID: 4356}, State: aj.Planned})
	if err = handle.Save(record); err != nil {
		t.Fatal(err)
	}
	record.Steps[0].State = aj.Observed
	record.State, record.CID = aj.Observed, "4356"
	if err = handle.Save(record); err != nil {
		t.Fatal(err)
	}
	stop := handlers.NoteVMListingStopForTest(handle, "nfs-images", "lab-pmx-0", "listing_http_500")
	if stop == nil || !strings.Contains(stop.Error(), "could not list storage nfs-images on node lab-pmx-0 (listing_http_500)") {
		t.Fatalf("the listing stop returned %v, want the delete_vm error that names the storage", stop)
	}
	if err = handle.Close(); err != nil {
		t.Fatal(err)
	}
	vm, err := j.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
	report := handlers.StorageAllocationAudit{Complete: true, VMScanComplete: true, Records: []aj.Record{vm}}
	var out, stderr bytes.Buffer
	if code := writeStorageJournalAudit(&out, &stderr, report, nil, true, true); code != 0 {
		t.Fatalf("audit exited %d: %s", code, stderr.String())
	}
	reason := "VM cleanup stopped because storage nfs-images on node lab-pmx-0 could not be listed (listing_http_500); rerun delete_vm or storage-journal cleanup once that storage lists"
	want := "record: id=" + id + " kind=vm state=observed charging=true cid=4356 reason=\"" + reason + "\"\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("summary = %q, want the record line %q", out.String(), want)
	}
}
