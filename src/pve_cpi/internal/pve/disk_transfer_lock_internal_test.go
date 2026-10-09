// disk_transfer_lock_internal_test.go covers the guards that keep two
// detach-side transfers of one disk from acting on each other's work: the
// per-disk transfer lock, the in-window check for a disk that is already
// parked, the intent write that refuses a finished record, and the proof a
// released-source attach needs before it attaches by config edit.
package pve

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// gatedLockPools is the in-memory lock pool store with a hook that runs
// before each create and outside the store's mutex, so a test can hold one
// lock holder back until a second contender has tried the same lock.
type gatedLockPools struct {
	*fakeLockPools
	beforeCreate func(poolID string)
}

func (g *gatedLockPools) CreatePool(ctx context.Context, poolID, comment string) error {
	if g.beforeCreate != nil {
		g.beforeCreate(poolID)
	}
	return g.fakeLockPools.CreatePool(ctx, poolID, comment)
}

// transferSourceAndParker is the usual transfer fixture: VM 700 holds the
// disk on scsi1 under its birth name, and parker 90000 is empty.
func transferSourceAndParker() *scanFakeClient {
	return newScanFakeClient(map[int]map[string]any{
		700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
		},
	})
}

// mutationEvents returns the events after from that change a disk's
// placement or a parker's records.
func mutationEvents(events []string, from int) []string {
	var out []string
	for _, e := range events[from:] {
		for _, prefix := range []string{"description:", "move:", "config-delete:", "pending-delete:", "attach:", "detach:", "destroy:"} {
			if strings.HasPrefix(e, prefix) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// TestTransferDiskToParker_ConcurrentTransfersOfOneDiskMoveItOnce runs two
// transfers of one disk at the same time, both resolved under the disk's
// birth name the way two detach_disk calls the Director doesn't serialize
// resolve it. The first holds the per-disk lock while the second waits on
// it. Exactly one moves the disk, and the other returns the landed name
// without writing anything, so the finished record stays as the mover left it.
func TestTransferDiskToParker_ConcurrentTransfersOfOneDiskMoveItOnce(t *testing.T) {
	t.Parallel()
	c := transferSourceAndParker()
	diskLock := ClusterLockPoolName(diskTransferLockName(transferStableID))
	parkerLock := ClusterLockPoolName("vm-90000")

	var diskCreates atomic.Int32
	contended := make(chan struct{})
	var contendedOnce sync.Once
	var firstWindow atomic.Bool
	var timedOut atomic.Bool
	pools := &gatedLockPools{fakeLockPools: newFakeLockPools()}
	pools.beforeCreate = func(poolID string) {
		switch poolID {
		case diskLock:
			if diskCreates.Add(1) >= 2 {
				contendedOnce.Do(func() { close(contended) })
			}
		case parkerLock:
			// The first transfer to reach its parker window already holds
			// the disk lock. It waits there until the second transfer has
			// tried that lock, so the two really overlap.
			if firstWindow.CompareAndSwap(false, true) {
				select {
				case <-contended:
				case <-time.After(10 * time.Second):
					timedOut.Store(true)
				}
			}
		}
	}
	// The mover's release of the disk lock marks where the second transfer
	// starts acting, so every event after it is the second transfer's.
	releasedAt := -1
	pools.deleteHook = func(_ context.Context, id, _ string) {
		if id != diskLock || releasedAt >= 0 {
			return
		}
		c.mu.Lock()
		releasedAt = len(c.events)
		c.mu.Unlock()
	}
	c.pools = pools

	ctx := WithClusterLockPollForTest(context.Background(), 5*time.Millisecond)
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	var wg sync.WaitGroup
	landed := make([]string, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			landed[i], errs[i] = TransferDiskToParker(ctx, c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
		})
	}
	wg.Wait()

	if timedOut.Load() {
		t.Fatal("the second transfer never tried the per-disk lock while the first held it")
	}
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("transfer %d: %v", i, errs[i])
		}
	}
	if landed[0] != landed[1] || !strings.HasPrefix(landed[0], "data:vm-90000-disk-") {
		t.Fatalf("landed = %v, want both transfers to report the one parked volume", landed)
	}

	c.mu.Lock()
	events := append([]string(nil), c.events...)
	c.mu.Unlock()
	moves, deletes := 0, 0
	for _, e := range events {
		if strings.HasPrefix(e, "move:") {
			moves++
		}
		if strings.HasPrefix(e, "config-delete:700:") {
			deletes++
		}
	}
	if moves != 1 || deletes != 1 {
		t.Fatalf("moves=%d source deletes=%d, want exactly one of each; events=%v", moves, deletes, events)
	}
	if releasedAt < 0 {
		t.Fatalf("the per-disk lock was never released; pool calls=%v", pools.calls)
	}
	if after := mutationEvents(events, releasedAt); len(after) != 0 {
		t.Fatalf("the second transfer changed state after the mover finished: %v", after)
	}
	entry, ok := c.parkedEntries(t)[transferStableID]
	if !ok || entry.Volid != landed[0] {
		t.Fatalf("parker record = %+v (found %v), want the mover's finished record naming %q", entry, ok, landed[0])
	}
	pools.mu.Lock()
	leftover := len(pools.pools)
	pools.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("lock pools left behind: %v", pools.pools)
	}
}

// TestTransferDiskToParker_StaleSecondCallSucceedsWithoutTouchingTheDisk is
// the field sequence with the lock already serializing the two calls. The
// second call starts after the first has parked the disk and still carries
// the birth name. It finds the disk's serial on the parker inside its
// window, so it writes no intent, deletes nothing, attaches nothing, and
// returns the landed name.
func TestTransferDiskToParker_StaleSecondCallSucceedsWithoutTouchingTheDisk(t *testing.T) {
	t.Parallel()
	c := transferSourceAndParker()
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	first, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	if err != nil {
		t.Fatalf("first transfer: %v", err)
	}
	before := c.parkedEntries(t)[transferStableID]
	mark := len(c.events)

	second, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	if err != nil {
		t.Fatalf("second transfer: %v", err)
	}
	if second != first {
		t.Fatalf("second transfer landed = %q, want the first one's %q", second, first)
	}
	if after := mutationEvents(c.events, mark); len(after) != 0 {
		t.Fatalf("the second transfer changed state: %v", after)
	}
	if after := c.parkedEntries(t)[transferStableID]; !reflect.DeepEqual(after, before) {
		t.Fatalf("finished record changed from %+v to %+v", before, after)
	}
}

// TestTransferDiskToParker_DiskParkedOnAnotherParkerIsSuccess covers a stale
// call whose disk landed on a different parker than the one its window
// opens. That parker carries no serial of the disk, and the source names the
// volume nowhere, so the window takes back the intent it wrote and reports
// the disk gone. The re-resolve finds the disk on the other parker and the
// transfer succeeds there without moving anything.
func TestTransferDiskToParker_DiskParkedOnAnotherParkerIsSuccess(t *testing.T) {
	t.Parallel()
	const parked = "data:vm-90001-disk-0"
	desc := `<!--BOSH:{"bosh_parked_disks":{"` + transferStableID + `":{"disk_cid":"pvd-test","parked_at":"t",` +
		`"node":"pve1","volid":"` + parked + `","slot":"scsi0","source_vm_cid":"700"}}}-->`
	c := newScanFakeClient(map[int]map[string]any{
		700: {},
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
		},
		90001: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"scsi0":         parked + ",serial=" + transferStableID,
			"description":   desc,
		},
	})
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	if err != nil {
		t.Fatalf("TransferDiskToParker: %v", err)
	}
	if landed != parked {
		t.Fatalf("landed = %q, want the volume already parked on 90001", landed)
	}
	for _, e := range c.events {
		for _, prefix := range []string{"move:", "config-delete:", "attach:", "description:90001"} {
			if strings.HasPrefix(e, prefix) {
				t.Fatalf("the transfer acted on the disk or its record: %v", c.events)
			}
		}
	}
	if got, _ := c.configs[90001]["description"].(string); got != desc {
		t.Fatalf("the other parker's record changed: %q", got)
	}
	if entries := c.parkedEntries(t); len(entries) != 0 {
		t.Fatalf("parker 90000 kept a record of a disk it never received: %+v", entries)
	}
}

// TestWriteParkerTransferIntent_RefusesFinishedRecord pins that an intent
// write never replaces the record of a disk that has landed on the parker,
// and still replaces an intent whose disk hasn't, which is what a resumed
// transfer rewrites.
func TestWriteParkerTransferIntent_RefusesFinishedRecord(t *testing.T) {
	t.Parallel()
	record := func(volid, slot string) string {
		return `<!--BOSH:{"bosh_parked_disks":{"` + transferStableID + `":{"disk_cid":"pvd-test","parked_at":"t",` +
			`"node":"pve1","volid":"` + volid + `","slot":"` + slot + `","source_vm_cid":"700"}}}-->`
	}
	intent := parkerProvEntry{DiskCID: "pvd-test", Node: "pve1", Volid: "data:vm-700-disk-1", Slot: "scsi2", SourceVMCID: "700"}

	t.Run("finished record", func(t *testing.T) {
		t.Parallel()
		desc := record("data:vm-90000-disk-0", "scsi0")
		c := newScanFakeClient(map[int]map[string]any{
			90000: {
				cfgKeyTags:    "bosh-cpi;bosh-parker",
				"scsi0":       "data:vm-90000-disk-0,serial=" + transferStableID,
				"description": desc,
			},
		})
		err := writeParkerTransferIntent(context.Background(), c, nil, "pve1", 90000, transferStableID, intent, transferTestCfg)
		if !errors.Is(err, errParkerRecordFinished) {
			t.Fatalf("err = %v, want the finished-record refusal", err)
		}
		var finished *ParkerRecordFinishedError
		if !errors.As(err, &finished) || finished.Slot != "scsi0" || finished.Volid != "data:vm-90000-disk-0" {
			t.Fatalf("refusal = %#v, want it to name the landed slot and volume", finished)
		}
		if got, _ := c.configs[90000]["description"].(string); got != desc {
			t.Fatalf("the finished record was rewritten: %q", got)
		}
	})

	t.Run("intent in progress", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {
				cfgKeyTags:    "bosh-cpi;bosh-parker",
				"description": record("data:vm-700-disk-1", "scsi0"),
			},
		})
		if err := writeParkerTransferIntent(context.Background(), c, nil, "pve1", 90000, transferStableID, intent, transferTestCfg); err != nil {
			t.Fatalf("replacing an unfinished intent: %v", err)
		}
		if got := c.parkedEntries(t)[transferStableID]; got.Slot != "scsi2" {
			t.Fatalf("record = %+v, want the new intent's slot", got)
		}
	})
}

// TestTransferDiskToParker_ReleasedSourceAttachNeedsProof covers the
// config-edit attach a transfer uses when its slot delete left no unused
// entry. It attaches only after its own delete took the slot off the source
// and an unfiltered listing shows the volume, and every other case reports
// the disk gone and re-resolves it instead.
func TestTransferDiskToParker_ReleasedSourceAttachNeedsProof(t *testing.T) {
	t.Parallel()
	// The volume is named for VM 777 while VM 700 holds it, so PVE keeps no
	// unused entry when the slot goes, and the transfer must attach it by
	// config edit.
	const foreign = "data:vm-777-disk-1"
	fixture := func() *scanFakeClient {
		return newScanFakeClient(map[int]map[string]any{
			700: {"scsi1": foreign + ",serial=" + transferStableID + ",size=10G"},
			90000: {
				cfgKeyTags:      "bosh-cpi;bosh-parker",
				paramProtection: true,
			},
		})
	}
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}

	t.Run("own delete and a listing that shows the volume", func(t *testing.T) {
		t.Parallel()
		c := fixture()
		landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, foreign, transferTestCfg, pctx)
		if err != nil {
			t.Fatalf("TransferDiskToParker: %v", err)
		}
		if landed != foreign || c.attachCalls != 1 {
			t.Fatalf("landed=%q attaches=%d, want the volume attached once under its own name", landed, c.attachCalls)
		}
	})

	t.Run("listing that doesn't show the volume", func(t *testing.T) {
		t.Parallel()
		c := fixture()
		c.unlistedVolumes = map[string]bool{foreign: true}
		_, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, foreign, transferTestCfg, pctx)
		if err == nil {
			t.Fatal("a volume the storage doesn't list was attached")
		}
		if c.attachCalls != 0 {
			t.Fatalf("attaches = %d, want none without proof the volume exists", c.attachCalls)
		}
		var cpiErr *cpierrors.Error
		if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
			t.Fatalf("err = %v, want a retriable error from the re-resolve", err)
		}
		// The slot delete ran, so the intent record is the disk's identity
		// carrier now and stays for the resume.
		if _, ok := c.parkedEntries(t)[transferStableID]; !ok {
			t.Fatal("the intent record was dropped after the source slot was deleted")
		}
	})

	t.Run("source let the volume go before any delete", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700: {},
			90000: {
				cfgKeyTags:      "bosh-cpi;bosh-parker",
				paramProtection: true,
			},
		})
		// The volume still exists, so only the missing delete stops the
		// attach.
		c.released = map[string]bool{foreign: true}
		_, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, foreign, transferTestCfg, pctx)
		if err == nil {
			t.Fatal("a volume this transfer never released was attached")
		}
		if c.attachCalls != 0 {
			t.Fatalf("attaches = %d, want none when this transfer deleted no slot", c.attachCalls)
		}
		var cpiErr *cpierrors.Error
		if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
			t.Fatalf("err = %v, want a retriable error from the re-resolve", err)
		}
		if entries := c.parkedEntries(t); len(entries) != 0 {
			t.Fatalf("the window kept an intent for a disk it never moved: %+v", entries)
		}
	})

	t.Run("attach that PVE refuses because the volume is gone", func(t *testing.T) {
		t.Parallel()
		c := fixture()
		c.attachErr = missingVolumeAnswer("data/vm-777-disk-1")
		_, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, foreign, transferTestCfg, pctx)
		if err == nil {
			t.Fatal("a refused attach reported success")
		}
		if c.attachCalls != 1 {
			t.Fatalf("attaches = %d, want one, because a missing volume is not retried", c.attachCalls)
		}
		var cpiErr *cpierrors.Error
		if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
			t.Fatalf("err = %v, want a retriable error from the re-resolve", err)
		}
	})
}

// missingVolumeAnswer is the HTTP 500 PVE answers a config PUT that names an
// LVM volume that no longer exists, as the field logs carry it.
func missingVolumeAnswer(lv string) error {
	return sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":%q}`, "no such logical volume "+lv+"\n")))
}

// TestAttachToParkerLocked_MissingVolumeIsNotRetried pins that the parker
// attach gives up on the first "no such logical volume" answer instead of
// spending its retry budget on a verdict that never changes.
func TestAttachToParkerLocked_MissingVolumeIsNotRetried(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
	})
	c.attachErr = missingVolumeAnswer("labdata/vm-6535-disk-2")
	if !IsTransientTransport(sdkerrors.ParseAPIError(500, []byte(`{"message":"internal server error"}`))) {
		t.Fatal("a plain 500 must stay transient, or this test proves nothing about the exclusion")
	}
	_, err := attachToParkerLocked(context.Background(), c, nil, "pve1", 90000, "labdata:vm-6535-disk-2", transferStableID)
	if err == nil || !IsStorageVolumeMissing(err) {
		t.Fatalf("err = %v, want the missing-volume verdict", err)
	}
	if c.attachCalls != 1 {
		t.Fatalf("attach attempts = %d, want 1", c.attachCalls)
	}
}

// TestWithDiskTransferLock covers the lock's own contract: the body runs
// between the sentinel's create and its release under the disk's key, a
// client without a pool service runs the body unserialized, and an acquire
// that times out against a live holder returns without running it.
func TestWithDiskTransferLock(t *testing.T) {
	t.Parallel()
	key := ClusterLockPoolName(diskTransferLockName(transferStableID))

	t.Run("serializes on the disk key", func(t *testing.T) {
		t.Parallel()
		pools := &recordingPoolService{}
		c := &parkerLockClient{pools: pools}
		err := withDiskTransferLock(context.Background(), c, nil, transferStableID, "transfer_in", func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("the body runs without the lock's deadline")
			}
			pools.events = append(pools.events, "body")
			return nil
		})
		if err != nil {
			t.Fatalf("withDiskTransferLock: %v", err)
		}
		want := []string{"create:" + key, "body", "delete:" + key}
		if strings.Join(pools.events, ",") != strings.Join(want, ",") {
			t.Fatalf("events = %v, want %v", pools.events, want)
		}
	})

	t.Run("no pool service", func(t *testing.T) {
		t.Parallel()
		ran := false
		err := withDiskTransferLock(context.Background(), &parkerLockClient{}, nil, transferStableID, "transfer_in", func(context.Context) error {
			ran = true
			return nil
		})
		if err != nil || !ran {
			t.Fatalf("err=%v ran=%v, want the body run without a pool service", err, ran)
		}
	})

	t.Run("live holder", func(t *testing.T) {
		t.Parallel()
		pools := newFakeLockPools()
		holder := &parkerLockClient{pools: pools}
		held := make(chan struct{})
		done := make(chan struct{})
		go func() {
			_ = withDiskTransferLock(context.Background(), holder, nil, transferStableID, "holder", func(context.Context) error {
				close(held)
				<-done
				return nil
			})
		}()
		<-held
		defer close(done)
		ctx := WithClusterLockPollForTest(WithParkerLockWait(context.Background(), 50*time.Millisecond), 5*time.Millisecond)
		ran := false
		err := withDiskTransferLock(ctx, &parkerLockClient{pools: pools}, nil, transferStableID, "contender", func(context.Context) error {
			ran = true
			return nil
		})
		if err == nil || ran {
			t.Fatalf("err=%v ran=%v, want the contender refused while the holder runs", err, ran)
		}
		if errors.Is(err, ErrClusterLockCreateRefused) {
			t.Fatalf("err = %v, want a timeout rather than a refused create", err)
		}
	})
}
