package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// An NFS export that mounts but serves the wrong tree lists nothing, and the
// allow-list reads that empty listing as proof every volume on the storage is
// gone. These tests drive the three sources that contradict it, through the
// handlers that actually ask: delete_disk, whose refusal must stand, and
// has_disk, whose answer bosh cck acts on.

const (
	// corroborationStorage is an nfs storage, so it classifies shared and the
	// disk handlers take the single-probe path rather than the node sweep.
	corroborationStorage = "nfs-images"
	// corroborationVolid is the volume under test: nothing in the cluster
	// references it, which is the free-floating state the anchor proof runs in.
	corroborationVolid = "nfs-images:604/vm-604-disk-0.qcow2"
	// corroborationOtherVolid is another volume on the same storage, held by
	// an ordinary VM. Its presence in a config is what the config-reference
	// corroborator counts.
	corroborationOtherVolid = "nfs-images:700/vm-700-disk-0.qcow2"
	// corroborationJournalVolid is a volume the allocation journal recorded on
	// the storage and never recorded deleting.
	corroborationJournalVolid   = "nfs-images:800/vm-800-disk-0.qcow2"
	corroborationProbeNode      = "lab-corroborate-0"
	corroborationParkerVMID     = 90604
	corroborationOtherVMID      = 700
	corroborationStableID       = "bpd-00112233aabbccdd"
	corroborationJournalNS      = "corroboration"
	corroborationStatusUsedGiB  = 2 << 30
	corroborationStatusTotalGiB = 40 << 30
)

// corroborationSetup is the shape of one empty-listing scenario: which sources
// know of volumes the listing should have carried.
type corroborationSetup struct {
	// otherVMVolid, when set, puts an ordinary VM in the cluster whose config
	// references that volume, so the holder scan counts a reference on the
	// storage.
	otherVMVolid string
	// journalVolid, when set, enrolls a journal in a temp directory carrying
	// one live allocation of that volume on the storage, observed on storage.
	journalVolid string
	// plannedVolid, when set, enrolls the same journal with the allocation
	// stopped at planned: the intent is recorded and no create call was ever
	// made, so the intended volume names nothing that exists.
	plannedVolid string
	// statusUsed and statusActive script PVE's own status for the storage.
	// The zero value is an active storage reporting nothing in use, which
	// contradicts nothing.
	statusUsed   int64
	statusActive bool
}

// corroborationFixture is one wired scenario plus the counters and the log the
// assertions read.
type corroborationFixture struct {
	deps        handlers.Deps
	observer    *log.Observer
	client      *mockPVEClient
	deleteCalls *int
}

// statusCalls reports how many times the storage-status read was made, which is
// the cost the ordering is meant to avoid when a cheaper source already spoke.
func (f corroborationFixture) statusCalls() int { return f.client.storageStatusCalls }

// newCorroborationFixture wires a delete_disk/has_disk environment on nfs where
// the point probe cannot answer (PVE's "no format" 500) and the content listing
// comes back empty, which is the exact state a wrong export produces.
func newCorroborationFixture(t *testing.T, setup corroborationSetup) corroborationFixture {
	t.Helper()
	deleteCalls := 0
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, volume string) (bool, error) {
			return false, nfsNoFormat(volume)
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == corroborationOtherVMID {
				return map[string]any{
					"tags":  "bosh-cpi",
					"scsi0": setup.otherVMVolid + ",size=10G",
				}, nil
			}
			// The parker survives holding nothing, which is the state the
			// anchor refusal is written for.
			return map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}, nil
		},
	}
	nodesSvc := &mockNodesService{
		listStorageContentFn: func(
			_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams,
		) (*sdknodes.ListStorageContentResponse, error) {
			return storageContentListing(), nil
		},
	}
	base := &mockPVEClient{
		storageSvc:        storageSvc,
		qemuSvc:           qemuSvc,
		nodesSvc:          nodesSvc,
		clusterSvc:        corroborationClusterSvc(setup.otherVMVolid != ""),
		clusterStorageSvc: &mockClusterStorage{storageName: corroborationStorage, storageType: "nfs"},
		storageStatusFn: func(_ context.Context, _, _ string) (*sdknodes.ListStorageStatusResponse, error) {
			return storageStatusResponse(setup.statusActive, setup.statusUsed, corroborationStatusTotalGiB), nil
		},
	}
	client := &visiblePVEClient{mockPVEClient: base}
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	cfg := &config.CPIConfig{
		Node:                     corroborationProbeNode,
		DiskStorage:              corroborationStorage,
		DetachedDiskStrategy:     "parked",
		DiskDeleteStateGuard:     "off",
		ParkedDiskVMIDRangeStart: 90000,
		ParkedDiskVMIDRangeEnd:   90999,
	}
	if setup.journalVolid != "" {
		cfg.StorageAllocationJournalDir = enrolledJournalWithAllocation(t, setup.journalVolid)
		cfg.StoragePlacementNamespace = corroborationJournalNS
	}
	if setup.plannedVolid != "" {
		directory := enrollCorroborationJournal(t)
		recordPlannedCorroborationAllocation(t, directory, setup.plannedVolid)
		cfg.StorageAllocationJournalDir = directory
		cfg.StoragePlacementNamespace = corroborationJournalNS
	}
	deps := handlers.Deps{
		Config:   cfg,
		PVE:      client,
		Resolver: liveBackendResolver(client, corroborationProbeNode),
		Logger:   logger,
	}
	return corroborationFixture{deps: deps, observer: observer, client: base, deleteCalls: &deleteCalls}
}

// corroborationClusterSvc lists the parker, and optionally the ordinary VM
// whose config the reference count comes from. Both sit on the probe node, so
// the derived per-node guest listing the holder scan reads carries them.
func corroborationClusterSvc(withOtherVM bool) *mockClusterSvc {
	return &mockClusterSvc{
		listResourcesFn: func(
			_ context.Context, _ *sdkclusterapi.ListResourcesParams,
		) (*sdkclusterapi.ListResourcesResponse, error) {
			resp := sdkclusterapi.ListResourcesResponse{}
			parker, _ := json.Marshal(map[string]any{
				"vmid": corroborationParkerVMID, "node": corroborationProbeNode, "type": "qemu",
			})
			resp = append(resp, parker)
			if withOtherVM {
				other, _ := json.Marshal(map[string]any{
					"vmid": corroborationOtherVMID, "node": corroborationProbeNode, "type": "qemu",
				})
				resp = append(resp, other)
			}
			return &resp, nil
		},
	}
}

// corroborationJournalClusterID is the enrollment's recorded cluster, which the
// corroborator opens the journal against without asking the live cluster.
const corroborationJournalClusterID = "cluster"

// enrolledJournalWithAllocation enrolls a journal in a private temp directory
// and records one disk allocation whose step targets the storage under test, in
// a state that is not terminal. That is the shape the corroborator reads as "we
// allocated this and never recorded deleting it".
func enrolledJournalWithAllocation(t *testing.T, volume string) string {
	t.Helper()
	directory := enrollCorroborationJournal(t)
	recordCorroborationAllocation(t, directory, volume)
	return directory
}

// enrollCorroborationJournal creates the enrolled journal and closes it, so a
// later record can be added and so nothing holds the directory open while the
// corroborator reads it.
func enrollCorroborationJournal(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	// The journal refuses a directory any other user can reach, and the test
	// temp directory is group- and world-readable by default.
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("tighten journal directory: %v", err)
	}
	journal, err := aj.Initialize(t.Context(), directory, corroborationJournalNS, aj.Enrollment{
		ClusterID:               corroborationJournalClusterID,
		AuthorityID:             "authority",
		AuditID:                 "corroboration-fixture-audit",
		CompleteHistoricalAudit: true,
		PreviousWriterFenced:    true,
	})
	if err != nil {
		t.Fatalf("initialize allocation journal: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close allocation journal: %v", err)
	}
	return directory
}

// recordCorroborationAllocation adds one live disk allocation naming volume on
// the storage under test, with its step carried to observed: the create call
// went to PVE and the volume was seen on storage. That is the state the
// corroborator counts, because it is the only one in which a volume the listing
// should have carried exists.
func recordCorroborationAllocation(t *testing.T, directory, volume string) {
	t.Helper()
	recordCorroborationStep(t, directory, volume, aj.Observed)
}

// recordPlannedCorroborationAllocation stops one step earlier: the intent is
// persisted and no create call has been made, so the intended volume names
// nothing that exists.
func recordPlannedCorroborationAllocation(t *testing.T, directory, volume string) {
	t.Helper()
	recordCorroborationStep(t, directory, volume, aj.Planned)
}

// recordCorroborationStep writes one disk allocation whose single step targets
// the storage under test and reached state. The journal insists a step first
// appear as planned, so anything past that is a second save.
func recordCorroborationStep(t *testing.T, directory, volume string, state aj.State) {
	t.Helper()
	journal, err := aj.Open(directory, corroborationJournalNS, corroborationJournalClusterID)
	if err != nil {
		t.Fatalf("open allocation journal: %v", err)
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Errorf("close allocation journal: %v", closeErr)
		}
	}()
	fingerprint, err := aj.Fingerprint(map[string]string{"fixture": "corroboration"})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatalf("allocation id: %v", err)
	}
	handle, err := journal.CreateDisk(t.Context(), id, aj.Intent{
		IntentFingerprint: fingerprint,
		PolicyFingerprint: fingerprint,
		PlanVersion:       1,
		Plan:              json.RawMessage(`{"version":1}`),
	})
	if err != nil {
		t.Fatalf("create disk allocation: %v", err)
	}
	defer func() {
		if closeErr := handle.Close(); closeErr != nil {
			t.Errorf("close allocation handle: %v", closeErr)
		}
	}()
	record := handle.Record()
	record.Steps = append(record.Steps, aj.Step{
		ID:     "create-volume",
		Kind:   "create_volume",
		State:  aj.Planned,
		Target: aj.Target{Node: corroborationProbeNode, Storage: corroborationStorage, IntendedVolume: volume},
	})
	if err := handle.Save(record); err != nil {
		t.Fatalf("record allocation step: %v", err)
	}
	if state == aj.Planned {
		return
	}
	record = handle.Record()
	record.Steps[len(record.Steps)-1].State = state
	record.Steps[len(record.Steps)-1].VolIDs = []string{volume}
	if err := handle.Save(record); err != nil {
		t.Fatalf("carry allocation step to %s: %v", state, err)
	}
}

// deleteCorroborationDisk runs delete_disk against the promised-anchor CID for
// the volume under test.
//
// The fixture's logger goes into the context the call carries, the way cmd/cpi
// wires it per request, so a warning the proof writes through the context
// reaches the observer rather than the nop logger.
func deleteCorroborationDisk(t *testing.T, fixture corroborationFixture) error {
	t.Helper()
	cid := mustEncodeDiskCID(t, corroborationVolid, &pve.DiskCIDMeta{ID: corroborationStableID, Anchor: true})
	h := handlers.HandleDeleteDisk(fixture.deps)
	ctx := log.IntoContext(context.Background(), fixture.deps.Logger)
	_, err := h.Handle(ctx, []json.RawMessage{marshal(cid)}, jsonrpc.Context{})
	return err
}

// unprovenAbsenceReason returns the reason logged with the warning that says
// the refusal is standing on an absence nobody could establish. That warning is
// where the corroboration's own wording reaches an operator on this path: the
// error delete_disk returns is the anchor refusal it was already going to
// return, and the contradiction explains why it still stands.
func unprovenAbsenceReason(observer *log.Observer) string {
	for _, entry := range observer.All() {
		if entry.Level != log.LevelWarn || !strings.Contains(entry.Message, "absence could not be proven") {
			continue
		}
		if reason, ok := entry.Attrs["error"].(string); ok {
			return reason
		}
	}
	return ""
}

// TestDeleteDisk_EmptyListing_ContradictedByConfigs is the wrong-export shape
// with a witness already in hand: another VM in the cluster still references a
// volume on the storage that just listed nothing, so the listing is not
// describing the tree the volumes live on.
func TestDeleteDisk_EmptyListing_ContradictedByConfigs(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{
		otherVMVolid: corroborationOtherVolid,
		statusActive: true,
	})
	err := deleteCorroborationDisk(t, fixture)
	if err == nil {
		t.Fatal("an empty listing the cluster's configs contradict must not be read as a completed delete")
	}
	reason := unprovenAbsenceReason(fixture.observer)
	if !strings.Contains(reason, pve.CorroborationSourceConfigs) {
		t.Errorf("the operator must be told which source contradicted the listing, got %q", reason)
	}
	if !strings.Contains(reason, "1 volume on the storage is referenced by VM configs") {
		t.Errorf("the refusal must name what it saw, got %q", reason)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", *fixture.deleteCalls)
	}
	if calls := fixture.statusCalls(); calls != 0 {
		t.Errorf("the config references settled it, so no status read is owed, got %d", calls)
	}
}

// TestDeleteDisk_EmptyListing_ContradictedByJournal covers the storage no
// config references any more: the CPI's own record of what it allocated there,
// and never recorded deleting, is what is left to contradict the listing.
func TestDeleteDisk_EmptyListing_ContradictedByJournal(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{
		journalVolid: corroborationJournalVolid,
		statusActive: true,
	})
	err := deleteCorroborationDisk(t, fixture)
	if err == nil {
		t.Fatal("an empty listing the allocation journal contradicts must not be read as a completed delete")
	}
	reason := unprovenAbsenceReason(fixture.observer)
	if !strings.Contains(reason, pve.CorroborationSourceJournal) {
		t.Errorf("the operator must be told which source contradicted the listing, got %q", reason)
	}
	if !strings.Contains(reason, "1 volume allocated on the storage has no recorded delete") {
		t.Errorf("the refusal must name what it saw, got %q", reason)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", *fixture.deleteCalls)
	}
}

// TestDeleteDisk_EmptyListing_JournalStopsTheStatusRead pins the cost order:
// the status read is the only corroborator that spends an API call, and a
// contradiction the records already carry must settle the question without it.
func TestDeleteDisk_EmptyListing_JournalStopsTheStatusRead(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{
		journalVolid: corroborationJournalVolid,
		statusActive: true,
	})
	if err := deleteCorroborationDisk(t, fixture); err == nil {
		t.Fatal("the journal contradiction must stand")
	}
	if calls := fixture.statusCalls(); calls != 0 {
		t.Errorf("a contradiction from the records owes PVE no status read, got %d", calls)
	}
}

// TestDeleteDisk_EmptyListing_UsedBytesAreAdvisory pins the one source that
// may not contradict. PVE answers the used figure from a statfs of the whole
// filesystem behind the storage, while the listing covers only the storage's
// configured content types, so an export shared with a backup storage reports
// its dumps against an images storage that is honestly empty. The delete stays
// idempotent and the operator gets a warning to go look at the export.
func TestDeleteDisk_EmptyListing_UsedBytesAreAdvisory(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{
		statusActive: true,
		statusUsed:   corroborationStatusUsedGiB,
	})
	if err := deleteCorroborationDisk(t, fixture); err != nil {
		t.Fatalf("a used figure is not a listing and must not refuse the delete, got %v", err)
	}
	warned := false
	for _, entry := range fixture.observer.All() {
		if entry.Level == log.LevelWarn && strings.Contains(entry.Message, "reports bytes in use") {
			warned = true
			if got := entry.Attrs["storage"]; got != corroborationStorage {
				t.Errorf("the warning must name the storage, got %v", got)
			}
			if got := entry.Attrs["used_bytes"]; got != int64(corroborationStatusUsedGiB) {
				t.Errorf("the warning must carry the figure PVE reported, got %v", got)
			}
		}
	}
	if !warned {
		t.Error("the operator must still be told the storage reports bytes against an empty listing")
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", *fixture.deleteCalls)
	}
	if calls := fixture.statusCalls(); calls != 1 {
		t.Errorf("the status is read once per probe, got %d", calls)
	}
}

// TestDeleteDisk_EmptyListing_InactiveStorage_IsUnproven keeps the half of the
// status read that is still evidence: a storage PVE reports inactive on the
// node cannot have proved anything absent, so the anchor refusal stands.
func TestDeleteDisk_EmptyListing_InactiveStorage_IsUnproven(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{statusActive: false})
	err := deleteCorroborationDisk(t, fixture)
	if err == nil {
		t.Fatal("an empty listing from an inactive storage must not be read as a completed delete")
	}
	reason := unprovenAbsenceReason(fixture.observer)
	if !strings.Contains(reason, pve.CorroborationSourceStorageStatus) {
		t.Errorf("the operator must be told which source contradicted the listing, got %q", reason)
	}
	if !strings.Contains(reason, "not active") {
		t.Errorf("the refusal must name what it saw, got %q", reason)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", *fixture.deleteCalls)
	}
}

// TestDeleteDisk_EmptyListing_NothingContradicts keeps the behavior the
// corroboration must not take away: a genuinely empty storage, with no
// references, no journal and nothing in use, still proves the volume gone and
// delete_disk still returns the idempotent success the Director expects.
func TestDeleteDisk_EmptyListing_NothingContradicts(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{statusActive: true})
	if err := deleteCorroborationDisk(t, fixture); err != nil {
		t.Fatalf("an uncontradicted empty listing must stay a completed delete, got %v", err)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", *fixture.deleteCalls)
	}
}

// TestDeleteDisk_EmptyListing_PlannedAllocationDoesNotContradict is the state
// rule on the handler path. Another allocation that recorded its intent and
// then failed before the create call names a volume PVE never made, so it is no
// evidence that the storage holds anything, and delete_disk stays idempotent.
func TestDeleteDisk_EmptyListing_PlannedAllocationDoesNotContradict(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{
		plannedVolid: corroborationJournalVolid,
		statusActive: true,
	})
	if err := deleteCorroborationDisk(t, fixture); err != nil {
		t.Fatalf("a volume no create call was ever made for must not refuse the delete, got %v", err)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", *fixture.deleteCalls)
	}
}

// TestDeleteDisk_EmptyListing_UnenrolledJournalDirectory_SaysNothing is the
// operator who set the journal directory and stopped there. The directory
// exists with the umask default of 0755, which the journal refuses to open
// because another user could reach it, and that refusal used to travel out as a
// check that did not land. A directory holding no enrollment holds no records,
// so it has nothing to say and delete_disk stays idempotent.
func TestDeleteDisk_EmptyListing_UnenrolledJournalDirectory_SaysNothing(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{statusActive: true})
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatalf("relax the journal directory: %v", err)
	}
	fixture.deps.Config.StorageAllocationJournalDir = directory
	fixture.deps.Config.StoragePlacementNamespace = corroborationJournalNS

	if err := deleteCorroborationDisk(t, fixture); err != nil {
		t.Fatalf("a directory with no enrollment holds no records to fail on, got %v", err)
	}
	if reason := unprovenAbsenceReason(fixture.observer); reason != "" {
		t.Errorf("an unenrolled directory must not read as a check that did not land, got %q", reason)
	}
}

// TestDeleteDisk_EmptyListing_EnrolledUnreadableJournal_FailsClosed is the
// other side of that rule. Once the enrollment is there, a directory the
// journal will not open is a journal whose records we failed to read, and a
// delete path may not read that as the journal saying nothing.
func TestDeleteDisk_EmptyListing_EnrolledUnreadableJournal_FailsClosed(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{
		journalVolid: corroborationJournalVolid,
		statusActive: true,
	})
	if err := os.Chmod(fixture.deps.Config.StorageAllocationJournalDir, 0o755); err != nil {
		t.Fatalf("relax the journal directory: %v", err)
	}

	if err := deleteCorroborationDisk(t, fixture); err == nil {
		t.Fatal("an enrolled journal that will not open leaves the absence unproven")
	}
	reason := unprovenAbsenceReason(fixture.observer)
	if !strings.Contains(reason, pve.CorroborationSourceJournal) {
		t.Errorf("the operator must be told which check did not land, got %q", reason)
	}
	if !strings.Contains(reason, "did not land") {
		t.Errorf("a failed read is not a source with nothing to say, got %q", reason)
	}
}

// TestHasDisk_EmptyListing_NothingContradicts is the bosh cck answer. An active
// storage with nothing in use and no journal leaves has_disk reporting false,
// which is what lets the Director's repair proceed.
func TestHasDisk_EmptyListing_NothingContradicts(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{statusActive: true})
	cid := mustEncodeDiskCID(t, corroborationVolid, &pve.DiskCIDMeta{ID: corroborationStableID, Anchor: true})
	h := handlers.HandleHasDisk(fixture.deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{marshal(cid)}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("an uncontradicted empty listing must settle has_disk, got %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("has_disk must answer a bool, got %T", result)
	}
	if exists {
		t.Error("a volume missing from a listing an nfs export served is gone")
	}
	if calls := fixture.statusCalls(); calls != 1 {
		t.Errorf("the status read is what clears the listing here, want 1 call, got %d", calls)
	}
}

// TestDeleteDisk_EmptyListing_OwnJournalRecordDoesNotContradict is the
// regression the exclusion exists for. A disk whose volume PVE already deleted
// still has an open allocation record, because recording the delete is what the
// call in flight is trying to do. Counting that record would refuse every
// completed delete on any deployment that enabled the journal.
func TestDeleteDisk_EmptyListing_OwnJournalRecordDoesNotContradict(t *testing.T) {
	t.Parallel()

	// The record carries the bare storage-relative name while the CID carries
	// the qualified volid, which is the pair the name match has to see through.
	fixture := newCorroborationFixture(t, corroborationSetup{
		journalVolid: "604/vm-604-disk-0.qcow2",
		statusActive: true,
	})
	if err := deleteCorroborationDisk(t, fixture); err != nil {
		t.Fatalf("the volume's own allocation record must not contradict its absence, got %v", err)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", *fixture.deleteCalls)
	}
}

// TestHasDisk_EmptyListing_OwnJournalRecordDoesNotContradict is the same rule
// on the read path. bosh cck learns the disk is gone only if has_disk answers
// false, and the disk's own record must not stand in the way of that.
func TestHasDisk_EmptyListing_OwnJournalRecordDoesNotContradict(t *testing.T) {
	t.Parallel()

	fixture := newCorroborationFixture(t, corroborationSetup{
		journalVolid: corroborationVolid,
		statusActive: true,
	})
	cid := mustEncodeDiskCID(t, corroborationVolid, &pve.DiskCIDMeta{ID: corroborationStableID, Anchor: true})
	h := handlers.HandleHasDisk(fixture.deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{marshal(cid)}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("the volume's own allocation record must not block the answer, got %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("has_disk must answer a bool, got %T", result)
	}
	if exists {
		t.Error("a volume missing from a listing an nfs export served is gone")
	}
}

// TestCreateDisk_OrphanSweep_OwnJournalRecordDoesNotContradict covers the
// sweep that runs while its own allocation is still mid-flight. The record for
// the volume being swept is exactly the record the journal holds, so excluding
// it is what keeps the sweep able to conclude that the failed create committed
// nothing.
func TestCreateDisk_OrphanSweep_OwnJournalRecordDoesNotContradict(t *testing.T) {
	t.Parallel()

	directory := enrollCorroborationJournal(t)
	var probed string
	var recorded sync.Once
	createFailed := false
	deleteCalls := 0
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			createFailed = true
			return "", errors.New("network drop before the allocation ran")
		},
		existsFn: func(_ context.Context, _, _, volume string) (bool, error) {
			// The allocator picks the VMID, so the volume the sweep is about
			// to probe is learned here rather than predicted. Only the probe
			// that follows the failed create is the sweep's own, and that is
			// the volume whose allocation is still open in the journal.
			probed = volume
			if createFailed {
				recorded.Do(func() { recordCorroborationAllocation(t, directory, volume) })
			}
			return false, nfsNoFormat(volume)
		},
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleteCalls++
			return nil
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	nodesSvc := &mockNodesService{
		listStorageContentFn: func(
			_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams,
		) (*sdknodes.ListStorageContentResponse, error) {
			return storageContentListing(), nil
		},
	}
	client := &visiblePVEClient{mockPVEClient: &mockPVEClient{
		storageSvc: storageSvc,
		nodesSvc:   nodesSvc,
		clusterSvc: &mockClusterSvc{},
		clusterStorageSvc: &mockClusterStorage{
			storageName: corroborationStorage,
			storageType: "nfs",
			shared:      true,
		},
		storageStatusFn: func(_ context.Context, _, _ string) (*sdknodes.ListStorageStatusResponse, error) {
			return storageStatusResponse(true, 0, corroborationStatusTotalGiB), nil
		},
	}}
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:                        corroborationProbeNode,
			DiskStorage:                 corroborationStorage,
			VMDiskFormat:                "qcow2",
			DetachedDiskStrategy:        "free",
			StorageAllocationJournalDir: directory,
			StoragePlacementNamespace:   corroborationJournalNS,
		},
		PVE:    client,
		Logger: logger,
	}

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(1024),
		marshal(map[string]string{}),
	}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("the CreateVolume failure must still propagate")
	}
	if probed == "" {
		t.Fatal("the sweep must have probed the volume it was about to clean")
	}
	// Zero deletes alone would also be what a sweep that gave up looks like,
	// and giving up is exactly what a contradicted listing would produce here.
	// The warning is what tells the two apart.
	for _, entry := range observer.All() {
		if strings.Contains(entry.Message, "orphan volume existence probe failed") {
			reason, _ := entry.Attrs["error"].(string)
			t.Fatalf("the volume's own allocation record must not stop the sweep proving its absence: %s", reason)
		}
	}
	if deleteCalls != 0 {
		t.Errorf("nothing was committed, so nothing may be deleted, got %d delete calls", deleteCalls)
	}
}
