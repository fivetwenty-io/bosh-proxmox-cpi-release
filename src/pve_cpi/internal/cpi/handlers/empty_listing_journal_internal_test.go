// Tests for what the allocation journal is allowed to say about an empty
// content listing: whose records count, which node they have to be on, and
// which states mean a volume was ever created.
package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	journalCountStorage  = "nfs-images"
	journalCountNode     = "pve-01"
	journalCountPeerNode = "pve-02"
	journalCountVolume   = "nfs-images:604/vm-604-disk-0.qcow2"
	journalCountOther    = "nfs-images:700/vm-700-disk-0.qcow2"
)

// sharedJournalProbe is the question asked about a shared storage, where a
// record from any node is about the one tree every node sees.
func sharedJournalProbe() pve.EmptyListingProbe {
	return pve.EmptyListingProbe{
		Node:       journalCountNode,
		Storage:    journalCountStorage,
		Volume:     journalCountVolume,
		Info:       pve.StorageInfo{Name: journalCountStorage, Type: pve.StorageTypeNFS},
		Classified: true,
	}
}

// localJournalProbe is the same question about a node-local storage, where only
// records on the probed node describe the tree that node listed.
func localJournalProbe(node string) pve.EmptyListingProbe {
	probe := sharedJournalProbe()
	probe.Node = node
	probe.Info = pve.StorageInfo{Name: journalCountStorage, Type: pve.StorageTypeDir, IsMountpoint: true}
	return probe
}

// observedStep is a step whose create call went to PVE and whose volume was
// seen on storage, which is the state the count is built from.
func observedStep(id, node, volume string) aj.Step {
	return aj.Step{
		ID:     id,
		Kind:   "create_volume",
		State:  aj.Observed,
		Target: aj.Target{Node: node, Storage: journalCountStorage, IntendedVolume: volume},
		VolIDs: []string{volume},
	}
}

// liveRecord wraps steps in a record that is not terminal, so the count reads
// it rather than skipping it as a completed lifecycle.
func liveRecord(id string, steps ...aj.Step) aj.Record {
	return aj.Record{ID: id, Kind: "disk", State: aj.Observed, Steps: steps}
}

// TestJournalVolumesOnStorage_SharedStorage_CountsEveryNode is the shared
// reading: one export is one tree, so a volume the journal put on it from any
// node is a volume the listing should have carried.
func TestJournalVolumesOnStorage_SharedStorage_CountsEveryNode(t *testing.T) {
	t.Parallel()

	records := []aj.Record{liveRecord("other", observedStep("s1", journalCountPeerNode, journalCountOther))}
	if got := journalVolumesOnStorage(records, sharedJournalProbe()); got != 1 {
		t.Errorf("a record from another node describes the same shared tree, want 1, got %d", got)
	}
}

// TestJournalVolumesOnStorage_LocalStorage_IgnoresOtherNodes is the defect the
// node filter closed. Every node has its own dir storage under the same name,
// so a volume recorded on one node's copy says nothing about another node's.
func TestJournalVolumesOnStorage_LocalStorage_IgnoresOtherNodes(t *testing.T) {
	t.Parallel()

	records := []aj.Record{liveRecord("other", observedStep("s1", journalCountPeerNode, journalCountOther))}
	if got := journalVolumesOnStorage(records, localJournalProbe(journalCountNode)); got != 0 {
		t.Errorf("another node's local storage is another tree, want 0, got %d", got)
	}
}

// TestJournalVolumesOnStorage_LocalStorage_CountsTheProbedNode is the other
// half: a record on the node we probed is about the tree that node listed.
func TestJournalVolumesOnStorage_LocalStorage_CountsTheProbedNode(t *testing.T) {
	t.Parallel()

	records := []aj.Record{liveRecord("other", observedStep("s1", journalCountNode, journalCountOther))}
	if got := journalVolumesOnStorage(records, localJournalProbe(journalCountNode)); got != 1 {
		t.Errorf("a record on the probed node contradicts that node's empty listing, want 1, got %d", got)
	}
}

// TestJournalVolumesOnStorage_StepWithoutANode_CountsAnywhere keeps an older
// record readable. A step that recorded no node cannot be placed, and dropping
// it would discard the evidence rather than scope it.
func TestJournalVolumesOnStorage_StepWithoutANode_CountsAnywhere(t *testing.T) {
	t.Parallel()

	records := []aj.Record{liveRecord("other", observedStep("s1", "", journalCountOther))}
	if got := journalVolumesOnStorage(records, localJournalProbe(journalCountNode)); got != 1 {
		t.Errorf("a step that named no node is in scope everywhere, want 1, got %d", got)
	}
}

// TestJournalVolumesOnStorage_UnclassifiedStorage_ReadsOnlyTheProbedNode pins
// the conservative reading. A storage nobody could classify may be local, so
// evidence from elsewhere in the cluster cannot be weighed against it.
func TestJournalVolumesOnStorage_UnclassifiedStorage_ReadsOnlyTheProbedNode(t *testing.T) {
	t.Parallel()

	probe := pve.EmptyListingProbe{
		Node: journalCountNode, Storage: journalCountStorage, Volume: journalCountVolume,
	}
	records := []aj.Record{liveRecord("other", observedStep("s1", journalCountPeerNode, journalCountOther))}
	if got := journalVolumesOnStorage(records, probe); got != 0 {
		t.Errorf("a storage nobody classified cannot be assumed shared, want 0, got %d", got)
	}
}

// TestJournalVolumesOnStorage_PlannedStepIsNotCounted is the state rule. The
// journal persists a step's intent before it submits the create call, so an
// intended volume on a planned step names something that was never created.
func TestJournalVolumesOnStorage_PlannedStepIsNotCounted(t *testing.T) {
	t.Parallel()

	planned := aj.Step{
		ID:     "planned",
		Kind:   "create_volume",
		State:  aj.Planned,
		Target: aj.Target{Node: journalCountNode, Storage: journalCountStorage, IntendedVolume: journalCountOther},
	}
	records := []aj.Record{liveRecord("other", planned)}
	if got := journalVolumesOnStorage(records, sharedJournalProbe()); got != 0 {
		t.Errorf("a volume no create call was ever made for contradicts nothing, want 0, got %d", got)
	}
}

// TestJournalVolumesOnStorage_SubmittedStepIsCounted is the other side of that
// line. From submitted onward the create call has gone to PVE, and a volume may
// exist whatever the outcome was, so it counts.
func TestJournalVolumesOnStorage_SubmittedStepIsCounted(t *testing.T) {
	t.Parallel()

	submitted := aj.Step{
		ID:     "submitted",
		Kind:   "create_volume",
		State:  aj.Submitted,
		UPID:   "UPID:pve-01:0000A1B2:00000000:00000000:imgcreate::root@pam:",
		Target: aj.Target{Node: journalCountNode, Storage: journalCountStorage, IntendedVolume: journalCountOther},
	}
	records := []aj.Record{liveRecord("other", submitted)}
	if got := journalVolumesOnStorage(records, sharedJournalProbe()); got != 1 {
		t.Errorf("a create call that went to PVE may have made a volume, want 1, got %d", got)
	}
}

// TestJournalVolumesOnStorage_RenamedVolumeExcludesItsWholeRecord is the parker
// reassignment. move_disk renames the volume, so the birth volid the journal
// recorded and the name the proof carries never meet by string comparison, and
// the record that holds both is the allocation whose outcome the caller is
// settling.
func TestJournalVolumesOnStorage_RenamedVolumeExcludesItsWholeRecord(t *testing.T) {
	t.Parallel()

	records := []aj.Record{liveRecord("renamed",
		observedStep("birth", journalCountNode, journalCountVolume),
		observedStep("reassigned", journalCountNode, "nfs-images:90604/vm-90604-disk-0.qcow2"),
	)}
	if got := journalVolumesOnStorage(records, sharedJournalProbe()); got != 0 {
		t.Errorf("the volume's own record may not be evidence against its absence, want 0, got %d", got)
	}
}

// TestJournalVolumesOnStorage_RetainedEphemeralISOExcluded is the same rule
// from the cleanup side. A VM whose deletion retained its ephemeral disk keeps
// one record naming that disk and the config ISO beside it, and counting the
// ISO would refuse every retained cleanup the journal ever recorded.
func TestJournalVolumesOnStorage_RetainedEphemeralISOExcluded(t *testing.T) {
	t.Parallel()

	retained := aj.Record{
		ID:    "retained",
		Kind:  "vm",
		State: aj.VMDeletedRetained,
		Steps: []aj.Step{
			observedStep("ephemeral", journalCountNode, journalCountVolume),
			observedStep("iso", journalCountNode, "nfs-images:604/vm-604-cloudinit.iso"),
		},
	}
	if got := journalVolumesOnStorage([]aj.Record{retained}, sharedJournalProbe()); got != 0 {
		t.Errorf("the retained record belongs to the volume under proof, want 0, got %d", got)
	}
}

// TestJournalVolumesOnStorage_OtherRecordsStillCount keeps the source useful.
// Excluding the volume's own allocation must not exclude the allocations beside
// it, which are what this source exists to report.
func TestJournalVolumesOnStorage_OtherRecordsStillCount(t *testing.T) {
	t.Parallel()

	records := []aj.Record{
		liveRecord("own", observedStep("birth", journalCountNode, journalCountVolume)),
		liveRecord("other", observedStep("s1", journalCountNode, journalCountOther)),
	}
	if got := journalVolumesOnStorage(records, sharedJournalProbe()); got != 1 {
		t.Errorf("another allocation's volume is exactly the evidence wanted, want 1, got %d", got)
	}
}

// TestJournalVolumesOnStorage_TerminalRecordClaimsNothing keeps the rule the
// audit index also follows: a record that recorded its delete is a completed
// lifecycle.
func TestJournalVolumesOnStorage_TerminalRecordClaimsNothing(t *testing.T) {
	t.Parallel()

	deleted := aj.Record{
		ID: "done", Kind: "disk", State: aj.Deleted,
		Steps: []aj.Step{observedStep("s1", journalCountNode, journalCountOther)},
	}
	if got := journalVolumesOnStorage([]aj.Record{deleted}, sharedJournalProbe()); got != 0 {
		t.Errorf("a recorded delete claims nothing, want 0, got %d", got)
	}
}

// TestJournalVolumesOnStorage_ExternalStepIsPreservationNotOwnership keeps the
// external marker meaning what it says: work done for another allocation.
func TestJournalVolumesOnStorage_ExternalStepIsPreservationNotOwnership(t *testing.T) {
	t.Parallel()

	step := observedStep("preserve", journalCountNode, journalCountOther)
	step.Target.External = true
	if got := journalVolumesOnStorage([]aj.Record{liveRecord("other", step)}, sharedJournalProbe()); got != 0 {
		t.Errorf("preservation work is not a claim of ownership, want 0, got %d", got)
	}
}

// TestJournalVolumesOnStorage_OtherStorageIsNotCounted keeps the storage filter
// honest, since a volid on a different storage says nothing about this listing.
func TestJournalVolumesOnStorage_OtherStorageIsNotCounted(t *testing.T) {
	t.Parallel()

	step := observedStep("elsewhere", journalCountNode, "other-nfs:700/vm-700-disk-0.qcow2")
	step.Target.Storage = "other-nfs"
	if got := journalVolumesOnStorage([]aj.Record{liveRecord("other", step)}, sharedJournalProbe()); got != 0 {
		t.Errorf("another storage's volume is not evidence here, want 0, got %d", got)
	}
}

// TestJournalCorroborator_ReadsTheJournalOnce pins the memoized read. One
// corroborator answers for every node of a cluster sweep and every slot of a
// delete_vm loop, and re-reading the journal directory for each of those would
// turn one second opinion into a read per candidate.
//
// The proof is that the second answer survives an enrollment the first read
// would now choke on: a corroborator that went back to disk would report a
// check that did not land, and this one reports what it already read.
func TestJournalCorroborator_ReadsTheJournalOnce(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	namespace := "director"
	// The journal refuses a directory any other user can reach, and the test
	// temp directory is group- and world-readable by default.
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("tighten the journal directory: %v", err)
	}
	journal, err := aj.Initialize(t.Context(), directory, namespace, aj.Enrollment{
		ClusterID:               "cluster",
		AuthorityID:             "authority",
		AuditID:                 "memoization-fixture-audit",
		CompleteHistoricalAudit: true,
		PreviousWriterFenced:    true,
	})
	if err != nil {
		t.Fatalf("initialize allocation journal: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close allocation journal: %v", err)
	}
	cfg := &config.CPIConfig{
		StorageAllocationJournalDir: directory,
		StoragePlacementNamespace:   namespace,
	}
	source := journalCorroborator(cfg)
	if _, err := source.CorroborateEmptyListing(context.Background(), sharedJournalProbe()); err != nil {
		t.Fatalf("an enrolled journal with no records has nothing to say: %v", err)
	}

	corruptEnrollment(t, directory, namespace)
	if _, err := source.CorroborateEmptyListing(context.Background(), sharedJournalProbe()); err != nil {
		t.Fatalf("the second probe must answer from the first read, got %v", err)
	}
	// A corroborator built fresh does go to disk, which is what makes the
	// assertion above about memoization rather than about the fixture.
	if _, err := journalCorroborator(cfg).CorroborateEmptyListing(
		context.Background(), sharedJournalProbe()); err == nil {
		t.Fatal("a fresh corroborator reads the broken enrollment and fails closed")
	}
}

// corruptEnrollment overwrites the authority file with content the journal
// cannot read, so any later open fails on a journal that is plainly enrolled.
func corruptEnrollment(t *testing.T, directory, namespace string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read journal directory: %v", err)
	}
	written := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		authority := filepath.Join(directory, entry.Name(), "authority.json")
		if _, statErr := os.Lstat(authority); statErr != nil {
			continue
		}
		if err := os.WriteFile(authority, []byte("not json"), 0o600); err != nil {
			t.Fatalf("overwrite authority file: %v", err)
		}
		written = true
	}
	if !written {
		t.Fatalf("no enrolled namespace found under the journal directory for %s", namespace)
	}
}
