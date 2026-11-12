// parker_lock_wait_test.go covers how long an attach_disk, detach_disk, or
// delete_disk of a disk without an allocation journal record waits for its
// parker's lock. Every disk on one parker shares that lock, so a clean-up that
// deletes many parked orphans at once queues them all behind it.
package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// parkerLockHolderFor is how long the other request holds the parker's lock
// in the tests that wait it out. It is longer than the lock's old 15-second
// default wait and well inside its TTL.
const parkerLockHolderFor = 20 * time.Second

// assertWaitedOutHolder fails the test unless the call succeeded after the
// holder left.
func assertWaitedOutHolder(t *testing.T, call string, err error, waited time.Duration) {
	t.Helper()
	if ttl := pve.ParkerProtectionLockTTLNow(); parkerLockHolderFor >= ttl {
		t.Fatalf("the holder's %v must be shorter than the lock's TTL of %v", parkerLockHolderFor, ttl)
	}
	if err != nil {
		t.Fatalf("%s gave up on the parker's lock after %v instead of waiting out the holder: %v",
			call, waited, err)
	}
	if waited < parkerLockHolderFor {
		t.Fatalf("%s took the lock after %v, before the holder left at %v", call, waited, parkerLockHolderFor)
	}
}

// parkedDeleteUnderHeldLock runs one delete_disk of a parked disk with no
// journal record while another request holds the parker's lock for holdFor,
// or for good when holdFor is zero. It returns whether the parker's detach
// ran, how long the delete spent waiting on the lock's clock, and the
// handler's error.
func parkedDeleteUnderHeldLock(t *testing.T, holdFor time.Duration) (bool, time.Duration, error) {
	t.Helper()
	return parkedDeleteWithDeadline(t, holdFor, 0)
}

// parkedDeleteWithDeadline is parkedDeleteUnderHeldLock on a request context
// whose deadline sits deadlineIn after the lock clock's start, or on a context
// with no deadline when deadlineIn is zero. The lock clock starts at the real
// time of the call and only moves when the acquire sleeps, so a deadline set on
// the real context from the clock's start is a real instant a minute or so
// ahead that no test run reaches, and the acquire still compares it against the
// fake clock the way it does in production.
func parkedDeleteWithDeadline(t *testing.T, holdFor, deadlineIn time.Duration) (bool, time.Duration, error) {
	t.Helper()
	pools, clk, ctx := handlers.HeldParkerLock(holdFor, reparkParkerVMID)
	if deadlineIn > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, clk.Start().Add(deadlineIn))
		defer cancel()
	}
	detached := false
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid != reparkParkerVMID {
				return map[string]any{}, nil
			}
			cfg := map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}
			if !detached {
				cfg["scsi0"] = reparkVolid + ",serial=" + reparkStableID + ",size=10G"
			}
			return cfg, nil
		},
		detachDiskFn: func(_ context.Context, _ string, _ int, _ string) error {
			detached = true
			return nil
		},
	}
	deps := reparkDeps(&mockPVEClient{
		storageSvc: &mockStorageService{}, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc(), poolsSvc: pools,
	})
	_, err := handlers.HandleDeleteDisk(deps).Handle(ctx,
		[]json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{})
	return detached, clk.Elapsed(), err
}

// TestHandleDeleteDisk_ParkedNoJournal_WaitsOutALongHolder is the clean-up
// case. Another delete holds the parker's lock for parkerLockHolderFor. A
// delete of a disk without a journal record must wait its turn and then delete
// the disk, as a journal-managed delete always has.
func TestHandleDeleteDisk_ParkedNoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	detached, waited, err := parkedDeleteUnderHeldLock(t, parkerLockHolderFor)
	assertWaitedOutHolder(t, "delete_disk", err, waited)
	if !detached {
		t.Fatal("delete_disk returned success without detaching the disk from its parker")
	}
}

// TestHandleDeleteDisk_ParkedNoJournal_WaitIsBoundedByTheTTL keeps the wait
// bounded. A holder that never leaves still times the delete out once the
// lock's TTL has passed, with the retriable lock timeout and the disk left in
// place.
func TestHandleDeleteDisk_ParkedNoJournal_WaitIsBoundedByTheTTL(t *testing.T) {
	t.Parallel()
	detached, waited, err := parkedDeleteUnderHeldLock(t, 0)
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("delete_disk against a holder that never leaves = %v, want the cluster lock timeout", err)
	}
	if detached {
		t.Fatal("delete_disk detached the disk without holding the parker's lock")
	}
	ttl := pve.ParkerProtectionLockTTLNow()
	if waited < ttl || waited > ttl+time.Minute {
		t.Fatalf("delete_disk waited %v for the parker's lock, want about its TTL of %v", waited, ttl)
	}
}

// TestHandleAttachDisk_ParkedNoJournal_WaitsOutALongHolder is the attach of a
// parked disk without a journal record while another request holds the
// parker's lock for parkerLockHolderFor. The unpark must wait its turn, and the
// attach then completes.
func TestHandleAttachDisk_ParkedNoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	const bareCID = parkedVolid
	// The parker (90000) holds the disk until the unpark detaches its slot. The
	// target VM 100 is empty until the attach, and then holds the disk.
	var qemuSvc *attachQEMUService
	qemuSvc = &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			switch {
			case vmid == 90000 && len(qemuSvc.detachCalls) == 0:
				return map[string]any{parkerSlot: bareCID, "tags": "bosh-parker", "name": "bosh-parker-90000"}, nil
			case vmid == 90000:
				return map[string]any{"tags": "bosh-parker", "name": "bosh-parker-90000"}, nil
			case qemuSvc.attachLastVolid != "":
				return map[string]any{"scsi1": bareCID}, nil
			default:
				return map[string]any{}, nil
			}
		},
	}
	pools, clk, ctx := handlers.HeldParkerLock(parkerLockHolderFor, 90000)
	deps := handlers.Deps{
		Config: parkedCfg(),
		PVE:    &mockPVEClient{qemuSvc: qemuSvc, clusterSvc: parkerClusterSvc(), poolsSvc: pools},
		Agent:  &captureAgent{},
		Logger: log.NewNopLogger(),
	}
	_, err := handlers.HandleAttachDisk(deps).Handle(ctx, attachArgs(t, "100", bareCID), jsonrpc.Context{})
	assertWaitedOutHolder(t, "attach_disk", err, clk.Elapsed())
	if len(qemuSvc.detachCalls) != 1 || qemuSvc.detachCalls[0] != parkerSlot {
		t.Fatalf("expected the unpark to detach %q from the parker; got %v", parkerSlot, qemuSvc.detachCalls)
	}
	if qemuSvc.attachLastVolid != bareCID {
		t.Fatalf("attach volid = %q, want %q", qemuSvc.attachLastVolid, bareCID)
	}
}

// TestHandleDetachDisk_ParkedNoJournal_WaitsOutALongHolder is the detach of a
// disk without a journal record into a parker whose lock another request holds
// for parkerLockHolderFor. The park must wait its turn and then attach the
// disk to the parker.
func TestHandleDetachDisk_ParkedNoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	const volid = "local-lvm:vm-9001-disk-0"
	var parkedVolid string
	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: 90000,
		sourceCfg:       map[string]any{diskSlot: volid},
		parkerCfg:       map[string]any{"tags": "bosh-parker"},
		attachDiskFn: func(_ context.Context, _ string, _ int, vol, _ string, _ *qemu.AttachOpts) (string, error) {
			parkedVolid = vol
			return "", nil
		},
	}
	// The detach creates a fresh parker, so another request holds the lock of
	// every parker in the band.
	band := make([]int, 0, 1000)
	for vmid := 90000; vmid <= 90999; vmid++ {
		band = append(band, vmid)
	}
	pools, clk, ctx := handlers.HeldParkerLock(parkerLockHolderFor, band...)
	deps := detachDepsParked(qemuSvc, parkerEmptyClusterSvc())
	deps.PVE.(*mockPVEClient).poolsSvc = pools
	_, err := handlers.HandleDetachDisk(deps).Handle(ctx, detachArgs(t, "100", volid), jsonrpc.Context{})
	assertWaitedOutHolder(t, "detach_disk", err, clk.Elapsed())
	if parkedVolid != volid {
		t.Fatalf("park attach volid = %q, want %q", parkedVolid, volid)
	}
}

// TestHandleDeleteDisk_ParkedNoJournal_WaitIsClippedByTheRequestDeadline pins
// that a delete_disk whose request context carries a deadline stops waiting
// for the parker's lock shortly before that deadline instead of running on to
// the lock's full TTL. The holder never leaves and the deadline sits a minute
// after the clock's start, so the acquire must give up after about 45 seconds,
// which is that minute less the 15-second margin, with the retriable lock
// timeout and the disk left on its parker.
func TestHandleDeleteDisk_ParkedNoJournal_WaitIsClippedByTheRequestDeadline(t *testing.T) {
	t.Parallel()
	const deadlineIn = time.Minute
	detached, waited, err := parkedDeleteWithDeadline(t, 0, deadlineIn)
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("delete_disk against a holder that never leaves = %v, want the cluster lock timeout", err)
	}
	if detached {
		t.Fatal("delete_disk detached the disk without holding the parker's lock")
	}
	want := deadlineIn - pve.ClusterLockContextMargin
	ttl := pve.ParkerProtectionLockTTLNow()
	if waited < want-5*time.Second || waited >= ttl/2 {
		t.Fatalf("delete_disk waited %v for the parker's lock, want about %v and far below its TTL of %v",
			waited, want, ttl)
	}
}

// TestHandleDeleteDisk_ParkedNoJournal_NoStableID_WaitsOutALongHolder is the
// delete of a parked disk that carries no stable ID, which takes the plain
// unpark and then the storage delete instead of the owned-disk deallocation.
// Another request holds the parker's lock for parkerLockHolderFor. The unpark
// must wait its turn, and the volume is deleted after it.
func TestHandleDeleteDisk_ParkedNoJournal_NoStableID_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	pools, clk, ctx := handlers.HeldParkerLock(parkerLockHolderFor, 90000)
	deleted := false
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, _ string) error {
			deleted = true
			return nil
		},
	}
	client, detachCalls := parkerDeleteClient(t, storageSvc, 90000, testNode, nil)
	client.poolsSvc = pools
	// The parker holds the disk until the unpark detaches it.
	qemuSvc := client.qemuSvc.(*mockQEMUService)
	qemuSvc.configFn = func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		if vmid != 90000 {
			return map[string]any{}, nil
		}
		cfg := map[string]any{"tags": "bosh-parker"}
		if *detachCalls == 0 {
			cfg["scsi0"] = diskCID
		}
		return cfg, nil
	}
	_, err := handlers.HandleDeleteDisk(parkerDepsForDelete(client)).Handle(ctx,
		[]json.RawMessage{marshal(mustEncodeDiskCID(t, diskCID, nil))}, jsonrpc.Context{})
	assertWaitedOutHolder(t, "delete_disk", err, clk.Elapsed())
	if *detachCalls == 0 {
		t.Fatal("delete_disk returned success without unparking the disk")
	}
	if !deleted {
		t.Fatal("delete_disk unparked the disk but never deleted the volume")
	}
}

// TestHandleCreateDisk_ParkFreshDisk_NoJournal_WaitsOutALongHolder is the park
// at the end of create_disk on the legacy path, where the new disk has no
// allocation journal record. The park creates a fresh parker, so another
// request holds the lock of every parker in the band for parkerLockHolderFor.
// The park must wait its turn, attach the disk to the parker, and let
// create_disk return the disk's CID.
func TestHandleCreateDisk_ParkFreshDisk_NoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	storageSvc := &mockStorageService{
		createVolumeFn: func(
			_ context.Context, _, storage string, _ int, _ string, vmid int, _ string,
		) (string, error) {
			return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid), nil
		},
	}
	var attachedVolid string
	qemuSvc := &parkerQEMUService{
		sourceVMID:      100,
		parkerVMIDStart: 90000,
		sourceCfg:       map[string]any{"name": "vm-100"},
		parkerCfg:       map[string]any{"tags": "bosh-parker"},
		attachDiskFn: func(_ context.Context, _ string, _ int, vol, _ string, _ *qemu.AttachOpts) (string, error) {
			attachedVolid = vol
			return "", nil
		},
	}
	band := make([]int, 0, 1000)
	for vmid := 90000; vmid <= 90999; vmid++ {
		band = append(band, vmid)
	}
	pools, clk, ctx := handlers.HeldParkerLock(parkerLockHolderFor, band...)
	deps := createDiskDepsParked(storageSvc, qemuSvc)
	deps.PVE.(*mockPVEClient).poolsSvc = pools
	result, err := handlers.HandleCreateDisk(deps).Handle(ctx, []json.RawMessage{
		marshal(1024), marshal(map[string]string{}), json.RawMessage(`null`),
	}, jsonrpc.Context{})
	assertWaitedOutHolder(t, "create_disk", err, clk.Elapsed())
	if cid, ok := result.(string); !ok || cid == "" {
		t.Fatalf("expected a non-empty disk CID, got %#v", result)
	}
	if attachedVolid == "" {
		t.Fatal("create_disk returned success without parking the disk")
	}
}

// TestCreateVM_DiskCIDs_ParkedNoJournal_WaitsOutALongHolder is the pre-attach
// of a persistent disk that create_vm receives in its disk_cids, where the
// disk is parked and has no allocation journal record. Another request holds
// the parker's lock for parkerLockHolderFor. The unpark before the attach must
// wait its turn, and create_vm then attaches the disk and succeeds.
func TestCreateVM_DiskCIDs_ParkedNoJournal_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	const parkerVMID = 90000
	var mu sync.Mutex
	detached := false
	attachedSlot := ""
	q := &vmMockQEMU{}
	q.detachDiskFn = func(_ context.Context, _ string, vmid int, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		if vmid == parkerVMID {
			detached = true
		}
		return nil
	}
	q.attachDiskFn = func(_ context.Context, _ string, _ int, _, _ string, opts *qemu.AttachOpts) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		attachedSlot = "scsi1"
		if opts != nil && opts.DiskID != "" {
			attachedSlot = opts.DiskID
		}
		return attachedSlot, nil
	}
	q.configFn = func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		if vmid == parkerVMID {
			cfg := map[string]any{"tags": "bosh-parker", "name": "bosh-parker-90000"}
			if !detached {
				cfg[parkerSlot] = parkedVolid
			}
			return cfg, nil
		}
		cfg := map[string]any{"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0"}
		if attachedSlot != "" {
			cfg[attachedSlot] = parkedVolid
		}
		return cfg, nil
	}
	c := &vmMockCluster{
		listResourcesFn: func(
			_ context.Context, _ *sdkcluster.ListResourcesParams,
		) (*sdkcluster.ListResourcesResponse, error) {
			raw, _ := json.Marshal(map[string]any{"vmid": parkerVMID, "node": "pve", "type": "qemu"})
			resp := sdkcluster.ListResourcesResponse{raw}
			return &resp, nil
		},
	}
	deps := buildVMDeps(q, &vmMockNodes{}, c, &vmMockAgent{})
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.DiskPerformance = &config.DiskPerformanceDefaults{Iothread: boolPtr(false)}
	pools, clk, ctx := handlers.HeldParkerLock(parkerLockHolderFor, parkerVMID)
	deps.PVE.(*mockPVEClient).poolsSvc = pools

	args := mkArgs("agent-1", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{mustEncodeDiskCID(t, parkedVolid, nil)}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(ctx, args, mkCtx("parked-preattach-held"))
	assertWaitedOutHolder(t, "create_vm", err, clk.Elapsed())
	mu.Lock()
	defer mu.Unlock()
	if !detached || attachedSlot == "" {
		t.Fatalf("create_vm returned success with detached=%t and attached slot %q", detached, attachedSlot)
	}
}
