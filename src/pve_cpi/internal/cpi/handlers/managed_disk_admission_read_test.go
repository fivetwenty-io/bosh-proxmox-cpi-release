package handlers

// These tests cover a read that the lifecycle guard makes to admit a mutation
// and that fails, rather than answering. Nothing has been sent when it fails,
// so the guard refuses that one mutation and stays usable. The operation's
// later writes still go through their own admission, and a request that ended
// during the read gets the same refusal as one that ended before admission
// began. A read that answers with something that disagrees with the record
// still locks the guard.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// admissionReadFault answers one read badly once it is armed. It picks the
// storage listing, which is the first read the lifecycle guard makes to admit
// any mutation, or, with identity set, the read of the cluster's identity,
// which is the second.
type admissionReadFault struct {
	identity bool
	mu       sync.Mutex
	armed    bool
	fired    int
	// answer decides how the armed listing goes. It returns the error the
	// listing fails with, and reports disagree when the listing must instead
	// answer with every storage pointed at another backing.
	answer func(ctx context.Context) (disagree bool, err error)
}

func (f *admissionReadFault) arm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fired == 0 {
		f.armed = true
	}
}

func (f *admissionReadFault) take() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed {
		return false
	}
	f.armed = false
	f.fired++
	return true
}

func (f *admissionReadFault) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

// readFaultPVE is the lock-contention flow cluster with the reads that
// admissionReadFault can answer badly.
type readFaultPVE struct {
	contendedFlowPVE
	fault *admissionReadFault
}

func (c readFaultPVE) ClusterStorage() clusterstorage.Service {
	return listingFaultStorage{Service: c.contendedFlowPVE.ClusterStorage(), fault: c.fault}
}

func (c readFaultPVE) Nodes() nodes.Service {
	return identityFaultNodes{Service: c.contendedFlowPVE.Nodes(), fault: c.fault}
}

type identityFaultNodes struct {
	nodes.Service
	fault *admissionReadFault
}

func (n identityFaultNodes) ListCertificatesInfo(ctx context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
	if n.fault.identity && n.fault.take() {
		if _, err := n.fault.answer(ctx); err != nil {
			return nil, err
		}
	}
	return n.Service.ListCertificatesInfo(ctx, node)
}

type listingFaultStorage struct {
	clusterstorage.Service
	fault *admissionReadFault
}

func (s listingFaultStorage) ListStorage(ctx context.Context, p *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	if s.fault.identity || !s.fault.take() {
		return s.Service.ListStorage(ctx, p)
	}
	disagree, err := s.fault.answer(ctx)
	if err != nil {
		return nil, err
	}
	listing, err := s.Service.ListStorage(ctx, p)
	if err != nil || !disagree {
		return listing, err
	}
	return movedStorageDefinitions(listing)
}

// armAtWindowRelease arms the fault on the parker window's release, after the
// disk has landed on 777. The release reads its sentinel first and then
// deletes it, so the first sentinel read after the move is the release's own,
// and the next storage listing is the guard's first read to admit the delete.
func armAtWindowRelease(disk *parkedFlowDisk, locks *lockContention, fault *admissionReadFault) {
	var mu sync.Mutex
	moved := false
	disk.client.afterMove = func() {
		mu.Lock()
		defer mu.Unlock()
		moved = true
	}
	locks.plainRead = func(context.Context, string) error {
		mu.Lock()
		defer mu.Unlock()
		if moved {
			fault.arm()
		}
		return nil
	}
	disk.deps.PVE = readFaultPVE{contendedFlowPVE: disk.deps.PVE.(contendedFlowPVE), fault: fault}
}

// requireHolderProvenance checks that 777 records the disk's ownership under
// the volume it now holds the disk as, and the Director's disk CID beside it.
func requireHolderProvenance(t *testing.T, disk *parkedFlowDisk) {
	t.Helper()
	cfg := disk.client.state.configs[777]
	description := pve.DescriptionFromConfig(cfg)
	entries, err := pve.ParseDiskAllocationProvenance(description)
	if err != nil {
		t.Fatalf("777's provenance does not parse: %v", err)
	}
	var held []string
	for _, entry := range entries {
		if entry.AllocationID == disk.id {
			held = append(held, entry.Volid)
		}
	}
	if len(held) != 1 {
		t.Fatalf("777 records the disk's ownership %d times (%v), want once", len(held), held)
	}
	if !lifecycleConfigHasVolume(cfg, held[0]) {
		t.Fatalf("777's provenance names %s, which 777 doesn't hold", held[0])
	}
	recorded := false
	for _, cid := range pve.GetAttachedDiskCIDs(description) {
		recorded = recorded || cid == disk.cid
	}
	if !recorded {
		t.Fatalf("777 does not record the Director's disk CID %s", disk.cid)
	}
}

// TestManagedAttachReleaseAdmissionReadFailureKeepsTheGuard moves a parked
// disk onto 777, and the guard's first read to admit the release of the
// parker window's sentinel gets a PVE 500. The release is not sent, and that
// costs only the sentinel waiting out its TTL. The guard stays usable, so the
// attach still writes 777's ownership record and the disk CID for the disk
// under its new name, and the allocation is returned. A later call then finds
// the provenance it needs and is not refused for an audit.
func TestManagedAttachReleaseAdmissionReadFailureKeepsTheGuard(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	fault := &admissionReadFault{answer: func(context.Context) (bool, error) { return false, errAdmissionReadRefused }}
	armAtWindowRelease(disk, locks, fault)

	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("attach_disk failed after the release's admission read failed: %v", err)
	}
	if n := fault.count(); n != 1 {
		t.Fatalf("the release's admission read failed %d times, want once", n)
	}
	if n := sentinelCount(locks); n != 1 {
		t.Fatalf("%d sentinels stand, want the window's own left for its TTL because its release was not sent", n)
	}
	assertReturnedRecord(t, "attached", disk.record(t))
	requireHolderProvenance(t, disk)
	if _, err := resolveDiskForOp(t.Context(), disk.deps, "attach_disk", disk.cid, "", nil); err != nil {
		t.Fatalf("the disk under its new name does not resolve: %v", err)
	}
}

// TestManagedAttachReleaseAdmissionAnswerDisagreesLocksTheGuard is the
// boundary of the test above. The release's admission listing answers, and
// every storage in it names another backing. That check finished and found
// the record contradicted, so the guard still locks itself, nothing after it
// is written, and the allocation is left for reconciliation.
func TestManagedAttachReleaseAdmissionAnswerDisagreesLocksTheGuard(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	fault := &admissionReadFault{answer: func(context.Context) (bool, error) { return true, nil }}
	armAtWindowRelease(disk, locks, fault)

	err := disk.attach(t.Context())
	if err == nil {
		t.Fatal("attach_disk succeeded although the release's admission found the record contradicted")
	}
	if n := fault.count(); n != 1 {
		t.Fatalf("the release's admission listing disagreed %d times, want once", n)
	}
	if !strings.Contains(err.Error(), "managed allocation blocked before Pool.DeletePool") {
		t.Fatalf("err = %v, want the guard locked before the release", err)
	}
	if record := disk.record(t); record.State != aj.ReconciliationRequired {
		t.Fatalf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
	}
}

// TestManagedAttachEndedDuringLockCreateAdmissionReturnsTheAllocation ends the
// request while the guard reads the cluster's identity to admit the parker
// window's sentinel create, which is the attach's first mutation. The guard
// has already checked that the request was live when it began. The create
// is not sent, the window never runs, and the guard is not locked. The attach
// fails with the same retriable error as a request that ended before the
// create, and the allocation is returned as it was.
func TestManagedAttachEndedDuringLockCreateAdmissionReturnsTheAllocation(t *testing.T) {
	t.Parallel()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &admissionReadFault{identity: true, answer: func(readCtx context.Context) (bool, error) {
		cancel()
		return false, readCtx.Err()
	}}
	flow := disk.deps.PVE.(contendedFlowPVE)
	// The attach lists 777's snapshots right before the window, so the next
	// read of the cluster's identity is the guard's, admitting the sentinel
	// create.
	flow.onSnapshots = func(vmid int) {
		if vmid == 777 {
			fault.arm()
		}
	}
	disk.deps.PVE = readFaultPVE{contendedFlowPVE: flow, fault: fault}

	err := disk.attach(ctx)
	if n := fault.count(); n != 1 {
		t.Fatalf("the request ended during %d admission reads, want one", n)
	}
	assertCleanCancellation(t, disk, err, errManagedRequestEnded)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to carry the request's own context error", err)
	}
	if strings.Contains(err.Error(), "reconciliation") {
		t.Fatalf("err = %v, want the ended request's error, not a reconciliation refusal", err)
	}
	if n := sentinelCount(locks); n != 0 {
		t.Fatalf("a sentinel was created on the ended request: %v", locks.pools)
	}
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the attach after the ended request failed: %v", err)
	}
	assertReturnedRecord(t, "attached", disk.record(t))
}
