package handlers

// These tests cover the holder's provenance write after an attach moves a
// managed disk off its parker and onto VM 777. When the write stops before
// anything is sent, because a read the guard makes to admit it fails or the
// request ends, the attach tries it again and then fails retriably rather than
// demanding an audit. A later call's identity check then proves the disk is
// 777's and writes the missing provenance itself, so the disk is never left
// needing an audit only because one write never went out.

import (
	"context"
	"encoding/json"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// provenanceWriteFault fails the storage listing while the attach's own
// provenance write, pve.WriteDiskAllocationProvenance, is on the stack. The
// match carries the call's opening parenthesis, so the heal's write through
// pve.WriteDiskAllocationProvenanceOnto never matches it. The listing is the
// first read the lifecycle guard makes to admit the write's configuration
// update, so a failure there stops the write before it is sent. It fails up
// to limit times, or every time when limit is negative, and fail decides the
// error.
type provenanceWriteFault struct {
	mu    sync.Mutex
	limit int
	fired int
	fail  func(ctx context.Context) error
}

func (f *provenanceWriteFault) take() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limit >= 0 && f.fired >= f.limit {
		return false
	}
	f.fired++
	return true
}

func (f *provenanceWriteFault) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

// clear stops the fault from firing again.
func (f *provenanceWriteFault) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limit = f.fired
}

// provenanceFaultPVE is the lock-contention flow cluster with a storage
// listing that provenanceWriteFault can fail.
type provenanceFaultPVE struct {
	contendedFlowPVE
	fault *provenanceWriteFault
}

func (c provenanceFaultPVE) ClusterStorage() clusterstorage.Service {
	return provenanceFaultStorage{Service: c.contendedFlowPVE.ClusterStorage(), fault: c.fault}
}

type provenanceFaultStorage struct {
	clusterstorage.Service
	fault *provenanceWriteFault
}

func (s provenanceFaultStorage) ListStorage(ctx context.Context, p *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	if strings.Contains(string(debug.Stack()), "pve.WriteDiskAllocationProvenance(") && s.fault.take() {
		return nil, s.fault.fail(ctx)
	}
	return s.Service.ListStorage(ctx, p)
}

// withProvenanceFault installs fault on disk's cluster.
func withProvenanceFault(disk *parkedFlowDisk, fault *provenanceWriteFault) {
	disk.deps.PVE = provenanceFaultPVE{contendedFlowPVE: disk.deps.PVE.(contendedFlowPVE), fault: fault}
}

// noBackoff keeps the provenance write's retries from waiting.
func noBackoff(ctx context.Context) context.Context {
	return pve.WithTestBackoff(ctx, func(int) time.Duration { return 0 })
}

// holderProvenanceEntries returns 777's provenance entries for the disk.
func holderProvenanceEntries(t *testing.T, disk *parkedFlowDisk) []pve.DiskAllocationProvenance {
	t.Helper()
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(disk.client.state.configs[777]))
	if err != nil {
		t.Fatalf("777's provenance does not parse: %v", err)
	}
	var held []pve.DiskAllocationProvenance
	for _, entry := range entries {
		if entry.AllocationID == disk.id {
			held = append(held, entry)
		}
	}
	return held
}

// requireNoAudit fails when err is the audit refusal, which the provenance
// write must not give for a write it never sent.
func requireNoAudit(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), "audit required") {
		t.Fatalf("err = %v, want no audit demanded for a write that was never sent", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want it retriable", err)
	}
}

// requireHealedOnRetry runs the attach again with no fault and checks that it
// succeeds, writes 777's provenance for the disk, returns the allocation, and
// leaves the disk resolvable.
func requireHealedOnRetry(t *testing.T, disk *parkedFlowDisk) {
	t.Helper()
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the retried attach failed: %v", err)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "retried", disk.record(t))
	if _, err := resolveDiskForOp(t.Context(), disk.deps, "attach_disk", disk.cid, "", nil); err != nil {
		t.Fatalf("the disk does not resolve after the retry: %v", err)
	}
}

// TestManagedAttachProvenanceAdmissionFailureRetriesTheWrite fails the guard's
// first read to admit the holder's provenance write once, with a PVE 500.
// Nothing was sent, so the attach tries the write again and succeeds in the
// same call, and the allocation comes back.
func TestManagedAttachProvenanceAdmissionFailureRetriesTheWrite(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	fault := &provenanceWriteFault{limit: 1, fail: func(context.Context) error { return errAdmissionReadRefused }}
	withProvenanceFault(disk, fault)

	if err := disk.attach(noBackoff(t.Context())); err != nil {
		t.Fatalf("attach_disk failed after one provenance admission read failed: %v", err)
	}
	if n := fault.count(); n != 1 {
		t.Fatalf("the provenance admission read failed %d times, want once", n)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "attached", disk.record(t))
}

// TestManagedAttachProvenanceAdmissionKeepsFailingHealsOnRetry fails every
// read the guard makes to admit the holder's provenance write. The attach
// tries the write managedHolderProvenanceAttempts times and then fails with a
// retriable error that names the VM and volume, not an audit. The disk has
// moved, so the allocation needs reconciliation. The retry's identity check
// finds 777 holding the renamed disk without provenance, proves the disk is
// 777's, writes the provenance, and the attach completes.
func TestManagedAttachProvenanceAdmissionKeepsFailingHealsOnRetry(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	fault := &provenanceWriteFault{limit: -1, fail: func(context.Context) error { return errAdmissionReadRefused }}
	withProvenanceFault(disk, fault)

	err := disk.attach(noBackoff(t.Context()))
	if err == nil {
		t.Fatal("attach_disk succeeded although no provenance write was admitted")
	}
	requireNoAudit(t, err)
	if !strings.Contains(err.Error(), "was not written") || !strings.Contains(err.Error(), "VM 777") {
		t.Fatalf("err = %v, want it to say the provenance was not written for VM 777", err)
	}
	if n := fault.count(); n != managedHolderProvenanceAttempts {
		t.Fatalf("the provenance write was tried %d times, want %d", n, managedHolderProvenanceAttempts)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although no write was sent: %v", held)
	}
	if record := disk.record(t); record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state = %s, want %s after the disk moved", record.State, aj.ReconciliationRequired)
	}
	parkerNotes := pve.DescriptionFromConfig(disk.client.state.configs[disk.parker])
	if _, found, err := pve.FindDiskAllocationProvenance(parkerNotes, diskStableIDFromCID(t, disk.cid)); err != nil || !found {
		t.Fatalf("the parker's leftover entry for the disk is gone (found %v, err %v), want it left for the retry to accept", found, err)
	}

	fault.clear()
	requireHealedOnRetry(t, disk)
}

// TestManagedAttachEndedDuringProvenanceAdmissionHealsOnRetry ends the request
// while the guard reads the storage listing to admit the holder's provenance
// write. The write is not sent and not tried again, and the attach fails with
// a retriable error rather than an audit. The retry writes the provenance.
func TestManagedAttachEndedDuringProvenanceAdmissionHealsOnRetry(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &provenanceWriteFault{limit: 1, fail: func(readCtx context.Context) error {
		cancel()
		return context.Canceled
	}}
	withProvenanceFault(disk, fault)

	err := disk.attach(noBackoff(ctx))
	if err == nil {
		t.Fatal("attach_disk succeeded although the request ended before the provenance write")
	}
	requireNoAudit(t, err)
	if !strings.Contains(err.Error(), "request ended before the write was sent") {
		t.Fatalf("err = %v, want it to say the request ended before the write was sent", err)
	}
	if n := fault.count(); n != 1 {
		t.Fatalf("the provenance admission read ran %d times, want once", n)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although no write was sent: %v", held)
	}

	requireHealedOnRetry(t, disk)
}

// TestManagedAttachEndedAsTheMoveLandsHealsOnRetry ends the request just as
// the move onto 777 lands, so every write after the move is refused on the
// ended request. The provenance write is one of them, and the attach fails
// with a retriable error rather than an audit. The retry writes the
// provenance.
func TestManagedAttachEndedAsTheMoveLandsHealsOnRetry(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	disk.client.afterMove = cancel

	err := disk.attach(ctx)
	if err == nil {
		t.Fatal("attach_disk succeeded although the request ended as the move landed")
	}
	requireNoAudit(t, err)
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although the request ended before the write: %v", held)
	}

	disk.client.afterMove = nil
	requireHealedOnRetry(t, disk)
}

// TestManagedAttachRetryRefusesAnotherClaimOnTheDisk is the boundary of the
// heal. After the provenance write never went out, VM 778 carries provenance
// for the disk's key. The retry can't prove the disk is 777's alone, so it
// keeps the audit refusal, names 778, and writes nothing onto 777.
func TestManagedAttachRetryRefusesAnotherClaimOnTheDisk(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	fault := &provenanceWriteFault{limit: -1, fail: func(context.Context) error { return errAdmissionReadRefused }}
	withProvenanceFault(disk, fault)
	if err := disk.attach(noBackoff(t.Context())); err == nil {
		t.Fatal("attach_disk succeeded although no provenance write was admitted")
	}
	fault.clear()

	key := diskStableIDFromCID(t, disk.cid)
	claim := pve.DiskAllocationProvenance{Version: 1, AllocationID: disk.id, AllocationNamespace: disk.deps.Config.StoragePlacementNamespace, Volid: "a:778/vm-778-disk-0.raw", Node: "n1", Backing: "claimed-elsewhere"}
	encoded, err := json.Marshal(map[string]pve.DiskAllocationProvenance{key: claim})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_disk_allocations": encoded})
	if err != nil {
		t.Fatal(err)
	}
	disk.client.state.configs[778] = map[string]any{"name": "other", "description": notes, "digest": "778"}

	err = disk.attach(t.Context())
	if err == nil {
		t.Fatal("the retried attach succeeded although VM 778 claims the disk")
	}
	if !strings.Contains(err.Error(), renamedHolderAudit) || !strings.Contains(err.Error(), "VM 778") {
		t.Fatalf("err = %v, want the audit refusal naming VM 778", err)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although another VM claims it: %v", held)
	}
}

// TestHealUnrecordedHolderRefusesWithoutProof checks each proof the heal makes
// from the holder alone. A parker holder, a slot without the disk's serial,
// and a volume named for another VM each keep the audit refusal, before the
// heal reads anything else or writes anything.
func TestHealUnrecordedHolderRefusesWithoutProof(t *testing.T) {
	t.Parallel()
	const stableID = "bpd-0123456789abcdef"
	volid := "a:777/vm-777-disk-1.raw"
	slot := volid + ",serial=" + stableID
	cases := map[string]struct {
		holder pve.DiskHolder
		volid  string
		cfg    map[string]any
		want   string
	}{
		"parker holder":     {holder: pve.DiskHolder{Found: true, VMID: 777, Node: "n1", IsParker: true}, volid: volid, cfg: map[string]any{"scsi1": slot}, want: "is a parker"},
		"parker tags":       {holder: pve.DiskHolder{Found: true, VMID: 777, Node: "n1", Tags: "bosh-parker"}, volid: volid, cfg: map[string]any{"scsi1": slot}, want: "is a parker"},
		"slot lacks serial": {holder: pve.DiskHolder{Found: true, VMID: 777, Node: "n1"}, volid: volid, cfg: map[string]any{"scsi1": volid}, want: "no drive slot"},
		"other serial":      {holder: pve.DiskHolder{Found: true, VMID: 777, Node: "n1"}, volid: volid, cfg: map[string]any{"scsi1": volid + ",serial=bpd-ffffffffffffffff"}, want: "no drive slot"},
		"named for another": {holder: pve.DiskHolder{Found: true, VMID: 777, Node: "n1"}, volid: "a:778/vm-778-disk-1.raw", cfg: map[string]any{"scsi1": "a:778/vm-778-disk-1.raw,serial=" + stableID}, want: "not named for its holder"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			holder := tc.holder
			rd := resolvedDisk{diskCID: "cid", birth: "a:123/vm-123-disk-0.raw", volid: tc.volid, stableID: stableID, holder: &holder}
			err := healUnrecordedHolder(t.Context(), Deps{}, rd, aj.Record{}, false, pve.DiskAllocationProvenance{}, tc.cfg)
			if err == nil {
				t.Fatal("the heal accepted a disk it could not prove")
			}
			if !strings.Contains(err.Error(), renamedHolderAudit) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want the audit refusal saying %q", err, tc.want)
			}
			if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("err = %v, want it not retriable", err)
			}
		})
	}
}

// TestManagedAttachLockCreateAdmissionFailureReturnsTheAllocation fails the
// guard's read of the cluster's identity to admit the parker window's sentinel
// create, the attach's first mutation, with a PVE 500. The create is not sent
// and nothing else is, so the allocation goes back as it was, the same way it
// does when the request ends at that point, and the error is retriable.
func TestManagedAttachLockCreateAdmissionFailureReturnsTheAllocation(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	fault := &admissionReadFault{identity: true, answer: func(context.Context) (bool, error) { return false, errAdmissionReadRefused }}
	flow := disk.deps.PVE.(contendedFlowPVE)
	flow.onSnapshots = func(vmid int) {
		if vmid == 777 {
			fault.arm()
		}
	}
	disk.deps.PVE = readFaultPVE{contendedFlowPVE: flow, fault: fault}

	err := disk.attach(t.Context())
	if n := fault.count(); n != 1 {
		t.Fatalf("the sentinel create's admission read failed %d times, want once", n)
	}
	if !isDiskReturnedUnchanged(err) {
		t.Fatalf("err = %v, want the disk returned unchanged", err)
	}
	assertCleanCancellation(t, disk, err, errManagedAdmissionReadFailed)
	if n := sentinelCount(locks); n != 0 {
		t.Fatalf("a sentinel was created although its admission read failed: %v", locks.pools)
	}
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the attach after the failed admission read failed: %v", err)
	}
	assertReturnedRecord(t, "attached", disk.record(t))
}

// diskStableIDFromCID returns the stable ID a disk CID carries.
func diskStableIDFromCID(t *testing.T, cid string) string {
	t.Helper()
	_, meta, err := pve.ParseEncodedDiskCID(cid)
	if err != nil || meta == nil || meta.ID == "" {
		t.Fatalf("disk CID %s carries no stable ID (err %v)", cid, err)
	}
	return meta.ID
}
