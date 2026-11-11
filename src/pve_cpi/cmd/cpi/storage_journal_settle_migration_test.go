package main

import (
	"bytes"
	"maps"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

const settleMigrationTestUPID = "UPID:pve:0000A1B2:0001C3D4:6734ABCD:qmigrate:90144:root@pam:"

// settleMigrationArgs is a complete settle-migration command line for
// allocation id, without the --config flag the fixture adds.
func settleMigrationArgs(id string) []string {
	return []string{
		"--allocation-id", id, "--decision-id", "shield-settle", "--expected-cid", "pvd-disk",
		"--step-id", "attempt-0-step-40", "--task-upid", settleMigrationTestUPID,
		"--previous-writer-fenced", "--remote-tasks-settled",
	}
}

// withoutFlag drops flag, and the value that follows it unless it is a
// boolean attestation, from args.
func withoutFlag(args []string, flag string) []string {
	var kept []string
	for i := 0; i < len(args); i++ {
		if args[i] != flag {
			kept = append(kept, args[i])
			continue
		}
		if flag != "--previous-writer-fenced" && flag != "--remote-tasks-settled" {
			i++
		}
	}
	return kept
}

// TestStorageJournalSettleMigrationRequiresEveryFlag refuses a settle-migration
// command line that lacks any of its flags or carries a flag that belongs to
// another action, and refuses --step-id and --task-upid on every other
// action, before the CLI reads its configuration or the journal.
func TestStorageJournalSettleMigrationRequiresEveryFlag(t *testing.T) {
	f := newStorageJournalFixture(t)
	_, id := journalFixtureVM(t, f)
	before := journalFixtureSnapshot(t, f.directory)
	full := settleMigrationArgs(id)
	cases := map[string][]string{}
	for _, flag := range []string{
		"--allocation-id", "--decision-id", "--expected-cid", "--step-id", "--task-upid",
		"--previous-writer-fenced", "--remote-tasks-settled",
	} {
		cases["without "+flag] = withoutFlag(full, flag)
	}
	cases["blank step"] = append(withoutFlag(full, "--step-id"), "--step-id", " ")
	cases["padded step"] = append(withoutFlag(full, "--step-id"), "--step-id", "attempt-0-step-40 ")
	cases["long step"] = append(withoutFlag(full, "--step-id"), "--step-id", strings.Repeat("s", 257))
	cases["UPID with a space"] = append(withoutFlag(full, "--task-upid"), "--task-upid", settleMigrationTestUPID+" x")
	cases["long UPID"] = append(withoutFlag(full, "--task-upid"), "--task-upid", strings.Repeat("U", 1025))
	cases["summary"] = append(append([]string(nil), full...), "--summary")
	cases["recovered task step"] = append(append([]string(nil), full...), "--recovered-task-step", "attempt-0-step-40")
	cases["recovered task UPID"] = append(append([]string(nil), full...), "--recovered-task-upid", settleMigrationTestUPID)
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, out := f.run("settle-migration", args...)
			if code != 2 || !strings.Contains(out, "settle-migration requires") {
				t.Fatalf("settle-migration %v = %d %q, want the flag refusal", args, code, out)
			}
		})
	}
	for _, action := range []string{"adopt", "finalize-cleanup", "cleanup", "audit"} {
		for _, flag := range []string{"--step-id", "--task-upid"} {
			args := []string{"--allocation-id", id, "--decision-id", "d", "--expected-cid", "pvd-disk", flag, "x"}
			if code, out := f.run(action, args...); code != 2 {
				t.Errorf("%s accepted %s: %d %q", action, flag, code, out)
			}
		}
	}
	if !reflect.DeepEqual(before, journalFixtureSnapshot(t, f.directory)) {
		t.Fatal("a flag refusal changed the journal")
	}
}

// TestStorageJournalSettleMigrationNamesTheHandlersRefusal runs a complete
// settle-migration command against a VM record, which the handler refuses,
// and prints the handler's reason.
func TestStorageJournalSettleMigrationNamesTheHandlersRefusal(t *testing.T) {
	f := newStorageJournalFixture(t)
	_, id := journalFixtureVM(t, f)
	before := journalFixtureSnapshot(t, f.directory)
	code, out := f.run("settle-migration", settleMigrationArgs(id)...)
	want := "allocation decision refused (identity_or_audit_evidence: settle-migration requires a disk record in " +
		"reconciliation_required with the exact CID"
	if code != 1 || !strings.Contains(out, want) {
		t.Fatalf("settle-migration of a VM record = %d %q, want the handler's reason", code, out)
	}
	if !reflect.DeepEqual(before, journalFixtureSnapshot(t, f.directory)) {
		t.Fatal("the refusal changed the journal")
	}
}

// TestStorageJournalSettleMigrationRequiresClusterContinuity refuses to
// settle a migration against a cluster whose identity differs from the one
// the journal enrolled.
func TestStorageJournalSettleMigrationRequiresClusterContinuity(t *testing.T) {
	f := newStorageJournalFixture(t)
	_, id := journalFixtureVM(t, f)
	f.fingerprint.Store(true)
	code, out := f.run("settle-migration", settleMigrationArgs(id)...)
	if code != 1 || !strings.Contains(out, "allocation decision requires verified cluster continuity") {
		t.Fatalf("settle-migration on another cluster = %d %q, want the continuity refusal", code, out)
	}
}

// TestStorageJournalUsageNamesSettleMigration lists the action and its flags
// in the usage and the flag help.
func TestStorageJournalUsageNamesSettleMigration(t *testing.T) {
	if !strings.Contains(storageJournalUsage, "|settle-migration") {
		t.Fatalf("usage %q does not name settle-migration", storageJournalUsage)
	}
	var out, stderr bytes.Buffer
	if code := runStorageJournal([]string{"settle-migration", "--help"}, &out, &stderr, runOptions{}); code != 0 {
		t.Fatalf("settle-migration --help = %d", code)
	}
	for _, want := range []string{"-step-id", "-task-upid", "qmigrate"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("settle-migration help lacks %s: %s", want, stderr.String())
		}
	}
}

// settledMigrationRecord is a disk record whose migrate step the handler
// settled: the step is observed, and the record carries the evidence the
// handler appended for it.
func settledMigrationRecord(t *testing.T) aj.Record {
	t.Helper()
	evidence := handlers.MigrationSettlementEvidence{
		Scope: "migration_settlement", AllocationID: "alloc-1", DecisionID: "shield-settle",
		StepID: "attempt-0-step-40", Kind: "lifecycle_attach_disk_Nodes_CreateQemuMigrate",
		UPID: settleMigrationTestUPID, TaskStatus: "stopped", ExitStatus: "OK", SourceNode: "pve",
		TargetNode: "pve2", VMID: 90144, Slot: "scsi2", Volume: "local-lvm:vm-90144-disk-1",
		Serial: "pvd-serial", ObservedAt: "2026-11-11T00:00:00Z",
	}
	id, raw, err := aj.VerificationEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	return aj.Record{
		ID: "alloc-1", Kind: "disk", State: aj.ReconciliationRequired, CID: "pvd-disk", DiskToken: "pvd-serial",
		Steps: []aj.Step{{
			ID: "attempt-0-step-40", Kind: evidence.Kind, State: aj.Observed, UPID: settleMigrationTestUPID,
			Target: aj.Target{Node: "pve2", VMID: 90144, IntendedVolume: evidence.Volume},
		}},
		Verifications: []aj.Verification{{EvidenceID: id, EvidenceJSON: raw, Complete: true}},
	}
}

// TestSettleMigrationSummaryPrintsTheEvidence builds the settle-migration
// output from a settled record. It names the step, the task and its outcome,
// the VM, both nodes, the slot, the volume, and the disk's serial.
func TestSettleMigrationSummaryPrintsTheEvidence(t *testing.T) {
	summary, err := settleMigrationSummary(settledMigrationRecord(t), "attempt-0-step-40")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"allocation_id": "alloc-1", "state": "reconciliation_required", "cid": "pvd-disk",
		"decision_id": "shield-settle", "step_id": "attempt-0-step-40", "upid": settleMigrationTestUPID,
		"task_status": "stopped", "exit_status": "OK", "vmid": "90144", "source_node": "pve",
		"target_node": "pve2", "slot": "scsi2", "volume": "local-lvm:vm-90144-disk-1", "serial": "pvd-serial",
	}
	if !maps.Equal(summary, want) {
		t.Fatalf("settle-migration summary = %v, want %v", summary, want)
	}
}

// TestSettleMigrationSummaryRefusesARecordWithoutItsEvidence refuses to
// print a success the record cannot back: a record with no evidence for the
// step, a step the record lacks, and evidence that names another target.
func TestSettleMigrationSummaryRefusesARecordWithoutItsEvidence(t *testing.T) {
	noEvidence := settledMigrationRecord(t)
	noEvidence.Verifications = nil
	otherTarget := settledMigrationRecord(t)
	otherTarget.Steps[0].Target.Node = "pve3"
	for name, tc := range map[string]struct {
		record aj.Record
		step   string
	}{
		"no evidence":  {noEvidence, "attempt-0-step-40"},
		"another step": {settledMigrationRecord(t), "attempt-0-step-41"},
		"other target": {otherTarget, "attempt-0-step-40"},
	} {
		t.Run(name, func(t *testing.T) {
			if summary, err := settleMigrationSummary(tc.record, tc.step); err == nil {
				t.Fatalf("settleMigrationSummary = %v, nil; want an error", summary)
			}
		})
	}
}
