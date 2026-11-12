package handlers

// parker_lock_wait_callers_internal_test.go covers the parker lock callers
// that the attach, detach, and delete tests don't reach. A delete_vm that
// moves a foreign disk off the VM, a legacy ephemeral retention, and the
// resumes of an interrupted transfer that resize_disk, update_disk,
// snapshot_disk, and set_disk_metadata run all wait a whole TTL for a busy
// parker. The deferred-teardown sweep is the one caller that gives up early.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// heldPoolsIDClient is the identity rig's client with a sentinel pool store
// that another request holds, so its transfers take real parker locks.
type heldPoolsIDClient struct {
	*idFakeClient
	pools *HeldParkerPools
}

func (c heldPoolsIDClient) Pools() pve.PoolService { return c.pools }

// TestDeleteVMForeignTransfer_WaitsOutALongHolder is delete_vm moving a
// stable-ID disk that belongs to another VM onto a parker before the destroy.
// Another request holds the parker's lock for parkerLockHolderForInternal,
// which is longer than the lock's old 15-second wait. The transfer must wait
// its turn and land the disk on the parker.
func TestDeleteVMForeignTransfer_WaitsOutALongHolder(t *testing.T) {
	t.Parallel()
	c := newIDFakeClient(map[int]map[string]any{
		700: {
			"virtio0": "data:vm-700-disk-0,size=20G",
			"scsi1":   "data:vm-700-disk-1,serial=" + idTestToken + ",size=10G",
		},
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
	pools, clk, ctx := HeldParkerLock(parkerLockHolderForInternal, 90000)
	deps := idTestDeps(c)
	deps.PVE = heldPoolsIDClient{idFakeClient: c, pools: pools}

	err := detachForeignActiveDisks(ctx, deps, "pve1", "700", 700, deps.Log(ctx))
	if err != nil {
		t.Fatalf("delete_vm's transfer gave up on the parker's lock after %v instead of waiting out the holder: %v",
			clk.Elapsed(), err)
	}
	if waited := clk.Elapsed(); waited < parkerLockHolderForInternal {
		t.Fatalf("the transfer took the parker's lock after %v, before the holder left at %v",
			waited, parkerLockHolderForInternal)
	}
	if _, present := c.configs[700]["scsi1"]; present {
		t.Fatal("the disk is still on the VM being deleted")
	}
	found := false
	for _, optStr := range qemu.ParseDisks(c.configs[90000]) {
		if serial, ok := pve.StableIDFromDriveOptStr(optStr); ok && serial == idTestToken {
			found = true
		}
	}
	if !found {
		t.Fatalf("the parker does not carry the disk after the transfer: %v", c.configs[90000])
	}
}

// TestLegacyRetention_WaitsOutALongHolder is delete_vm keeping a legacy VM's
// ephemeral disk by moving it to a parker. Another request holds the lock of
// every parker in the band for parkerLockHolderForInternal. The retention
// must wait its turn, and the destroy must go out only after the volume has
// left the VM.
func TestLegacyRetention_WaitsOutALongHolder(t *testing.T) {
	const volume = "a:vm-777-ephemeral-0"
	deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
	pools, clk, ctx := HeldParkerLock(parkerLockHolderForInternal, parkerBand()...)
	deps.PVE = sentinelHeldPVE{Client: deps.PVE, held: pools}

	_, err := HandleDeleteVM(deps).Handle(ctx, []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("the retention gave up on the parker's lock after %v instead of waiting out the holder: %v",
			clk.Elapsed(), err)
	}
	if waited := clk.Elapsed(); waited < parkerLockHolderForInternal {
		t.Fatalf("the retention took the parker's lock after %v, before the holder left at %v",
			waited, parkerLockHolderForInternal)
	}
	assertLegacyRetained(t, deps, client, recorder, volume)
}

// TestSweepPreservation_GivesUpOnABusyParkerAfterTheSweepWait runs the
// deferred-teardown sweep over a queued legacy VM whose ephemeral disk is
// kept. Another request holds every parker's lock and never leaves. The
// retention runs through preserveForSweep on the sweep's context, so it must
// give up after pve.ParkerLockSweepWait rather than the whole TTL, and the VM
// must stay queued with its disk and without a destroy.
func TestSweepPreservation_GivesUpOnABusyParkerAfterTheSweepWait(t *testing.T) {
	const volume = "a:vm-777-ephemeral-0"
	deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
	client.state.configs[777]["tags"] = ownershipTag + ";" + tagRetainEphemeral + ";" + tagDeletingVM
	pools, clk, ctx := HeldParkerLock(0, parkerBand()...)
	deps.PVE = sentinelHeldPVE{Client: deps.PVE, held: pools}

	sweepDeferredTeardowns(ctx, deps, deps.Logger)

	waited, ttl := clk.Elapsed(), pve.ParkerProtectionLockTTLNow()
	if waited < pve.ParkerLockSweepWait || waited >= ttl/2 {
		t.Fatalf("the sweep waited %v for the parker's lock, want about %v and far below its TTL of %v",
			waited, pve.ParkerLockSweepWait, ttl)
	}
	if destroys := recorder.guestDestroys(); len(destroys) != 0 {
		t.Fatalf("the sweep destroyed VM 777 %d times without keeping its ephemeral disk", len(destroys))
	}
	if slot, _ := client.state.configs[777]["scsi2"].(string); !strings.HasPrefix(slot, volume+",") {
		t.Fatalf("777's scsi2 is %q, want the ephemeral volume still on it", slot)
	}
	tags, _ := pve.ConfigString(client.state.configs[777], jsonKeyTags)
	if !tagsContain(tags, tagDeletingVM) {
		t.Fatalf("777 lost its queue tag: %q", tags)
	}
}

// TestResumesOnHandlers_WaitOutALongHolder calls each handler whose disk
// resolves to an interrupted transfer, which the handler finishes before it
// acts. The disk has no allocation journal record. Another request holds the
// parker's lock for parkerLockHolderForInternal, and each resume must wait
// its turn and give the parker's slot the disk's serial back. What the
// handler does with the disk after that is not this test's concern. The
// fixture's parked disk lacks the size and the workload VM that resize_disk,
// update_disk, and snapshot_disk want, so only a lock timeout counts as a
// failure here.
func TestResumesOnHandlers_WaitOutALongHolder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(Deps) Handler
		args    []any
	}{
		{"resize_disk", HandleResizeDisk, []any{6144}},
		{"update_disk", HandleUpdateDisk, []any{map[string]any{"cache": "writeback", "size": 6144}}},
		{"snapshot_disk", HandleSnapshotDisk, []any{map[string]any{}}},
		{"set_disk_metadata", HandleSetDiskMetadata, []any{map[string]any{"director": "x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := legacyLandedBeforeSerial(t)
			pools, clk, ctx := HeldParkerLock(parkerLockHolderForInternal, s.intentParker())
			s.deps.PVE = sentinelHeldPVE{Client: s.deps.PVE, held: pools}

			raw := []json.RawMessage{planJSON(t, s.cid)}
			for _, arg := range tc.args {
				raw = append(raw, planJSON(t, arg))
			}
			_, err := tc.handler(s.deps).Handle(ctx, raw, jsonrpc.Context{})
			if errors.Is(err, pve.ErrClusterLockTimeout) {
				t.Fatalf("%s gave up on the parker's lock after %v instead of waiting out the holder: %v",
					tc.name, clk.Elapsed(), err)
			}
			if waited := clk.Elapsed(); waited < parkerLockHolderForInternal {
				t.Fatalf("%s took the parker's lock after %v, before the holder left at %v",
					tc.name, waited, parkerLockHolderForInternal)
			}
			s.requireSingleHolder(t, true)
		})
	}
}

// TestResumesOnHandlers_WaitIsBoundedByTheTTL keeps the resume's wait bounded.
// A holder that never leaves times resize_disk out with the retriable lock
// timeout once the lock's TTL has passed.
func TestResumesOnHandlers_WaitIsBoundedByTheTTL(t *testing.T) {
	s, _ := legacyLandedBeforeSerial(t)
	pools, clk, ctx := HeldParkerLock(0, s.intentParker())
	s.deps.PVE = sentinelHeldPVE{Client: s.deps.PVE, held: pools}

	_, err := HandleResizeDisk(s.deps).Handle(ctx,
		[]json.RawMessage{planJSON(t, s.cid), planJSON(t, 6144)}, jsonrpc.Context{})
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("resize_disk against a holder that never leaves = %v, want the cluster lock timeout", err)
	}
	ttl := pve.ParkerProtectionLockTTLNow()
	if waited := clk.Elapsed(); waited < ttl || waited > ttl+time.Minute {
		t.Fatalf("resize_disk waited %v for the parker's lock, want about its TTL of %v", waited, ttl)
	}
	if client := s.client; client.resizeCalls != 0 {
		t.Fatalf("resize_disk resized %d times without holding the parker's lock", client.resizeCalls)
	}
}
