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
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
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

	t.Run("a nested call for the same disk runs inside the held lock", func(t *testing.T) {
		t.Parallel()
		pools := &recordingPoolService{}
		c := &parkerLockClient{pools: pools}
		other := ClusterLockPoolName(diskTransferLockName("other-disk"))
		err := withDiskTransferLock(context.Background(), c, nil, transferStableID, "transfer_in", func(outer context.Context) error {
			outerDeadline, _ := outer.Deadline()
			if err := withDiskTransferLock(outer, c, nil, transferStableID, "transfer_resume", func(inner context.Context) error {
				if d, ok := inner.Deadline(); !ok || !d.Equal(outerDeadline) {
					t.Errorf("nested deadline = %v (set %v), want the outer holder's %v", d, ok, outerDeadline)
				}
				pools.events = append(pools.events, "nested")
				return nil
			}); err != nil {
				return err
			}
			// Another disk's lock is its own, so it is still taken.
			return withDiskTransferLock(outer, c, nil, "other-disk", "transfer_in", func(context.Context) error {
				pools.events = append(pools.events, "other")
				return nil
			})
		})
		if err != nil {
			t.Fatalf("withDiskTransferLock: %v", err)
		}
		want := []string{"create:" + key, "nested", "create:" + other, "other", "delete:" + other, "delete:" + key}
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
		// The disk lock waits out a whole holder's claim, so the contender's
		// request deadline is what ends its wait here, the way the
		// Director's request timeout ends it in the field.
		reqCtx, cancel := context.WithTimeout(context.Background(), clusterLockContextMargin+200*time.Millisecond)
		defer cancel()
		ctx := WithClusterLockPollForTest(reqCtx, 5*time.Millisecond)
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

// fakeLockClock is a lock clock whose sleeps advance its time at once, so a
// test can spend a production-sized wait without waiting for it. Each sleep
// runs onSleep with the time the clock has reached.
type fakeLockClock struct {
	base    time.Time
	offset  atomic.Int64
	onSleep func(elapsed time.Duration)
}

func (f *fakeLockClock) clock() lockClock {
	return lockClock{
		now: func() time.Time { return f.base.Add(time.Duration(f.offset.Load())) },
		sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			elapsed := time.Duration(f.offset.Add(int64(d)))
			if f.onSleep != nil {
				f.onSleep(elapsed)
			}
			return nil
		},
	}
}

// TestWithDiskTransferLock_WaitsOutAHolderPastTheParkerWait pins the per-disk
// lock's wait against the parker lock's. A holder that keeps the disk lock for
// longer than the parker lock's 15s wait is still waited out, because the disk
// lock's wait covers its whole TTL, and that TTL covers a parker window that
// waited its whole wait before it ran.
func TestWithDiskTransferLock_WaitsOutAHolderPastTheParkerWait(t *testing.T) {
	t.Parallel()
	parkerTTL, parkerWait := parkerLockTimeoutsFrom(context.Background())
	ttl, wait := diskTransferLockTimeouts(context.Background())
	if ttl != parkerTTL+parkerWait || wait != ttl+clusterLockGrace() {
		t.Fatalf("ttl=%s wait=%s, want ttl %s and wait %s", ttl, wait, parkerTTL+parkerWait, parkerTTL+parkerWait+clusterLockGrace())
	}

	key := ClusterLockPoolName(diskTransferLockName(transferStableID))
	pools := newFakeLockPools()
	clk := &fakeLockClock{base: time.Now()}
	pools.pools[key] = encodeLockComment("holder", clk.base.Add(10*time.Minute))
	const holdFor = parkerProtectionLockTimeout + 5*time.Second
	clk.onSleep = func(elapsed time.Duration) {
		if elapsed >= holdFor {
			pools.mu.Lock()
			if pools.pools[key] == encodeLockComment("holder", clk.base.Add(10*time.Minute)) {
				delete(pools.pools, key)
			}
			pools.mu.Unlock()
		}
	}
	ctx := withTestParkerLockClock(context.Background(), clk.clock())
	ran := false
	err := withDiskTransferLock(ctx, &parkerLockClient{pools: pools}, nil, transferStableID, "contender", func(context.Context) error {
		ran = true
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("err=%v ran=%v, want the contender to run once the holder let go", err, ran)
	}
	if elapsed := time.Duration(clk.offset.Load()); elapsed < holdFor {
		t.Fatalf("the contender acquired after %s, before the holder let go at %s", elapsed, holdFor)
	}
}

// TestDiskTransferLockTimeouts_CapTheManagedWait pins how long a crashed
// holder can block a disk. A managed caller stretches the parker lock's wait
// to about a whole parker TTL (WithParkerLockWait), and the disk lock's TTL
// adds only the default parker wait on top of the parker TTL, never that
// stretched wait. A waiter is bounded by that TTL plus the create grace.
func TestDiskTransferLockTimeouts_CapTheManagedWait(t *testing.T) {
	t.Parallel()
	parkerTTL := parkerProtectionLockTTLNow()
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"default wait", context.Background()},
		{"managed wait of a whole parker TTL", WithParkerLockWait(context.Background(), parkerTTL)},
		{"managed wait of an hour", WithParkerLockWait(context.Background(), time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ttl, wait := diskTransferLockTimeouts(tc.ctx)
			if want := parkerTTL + parkerProtectionLockTimeout; ttl != want {
				t.Fatalf("ttl = %s, want the parker TTL plus the default parker wait, %s", ttl, want)
			}
			if wait != ttl+clusterLockGrace() {
				t.Fatalf("wait = %s, want the TTL plus the create grace, %s", wait, ttl+clusterLockGrace())
			}
		})
	}

	t.Run("a shorter test wait is kept", func(t *testing.T) {
		t.Parallel()
		ctx := withTestParkerLockTimeouts(context.Background(), 2*time.Second, 300*time.Millisecond)
		if ttl, _ := diskTransferLockTimeouts(ctx); ttl != 2300*time.Millisecond {
			t.Fatalf("ttl = %s, want the test TTL plus the test wait, 2.3s", ttl)
		}
	})
}

// TestWithDiskTransferLock_UnserializedFallbacksAreLoud covers the two
// fallbacks that run a transfer without its per-disk lock. Each logs an error,
// because a second transfer of the disk can overlap the first, and marks the
// body's context so the transfer's own logs say it ran unserialized.
func TestWithDiskTransferLock_UnserializedFallbacksAreLoud(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		client Client
	}{
		{"no pool service", &parkerLockClient{}},
		{"create refused", &parkerLockClient{pools: &recordingPoolService{
			createErr: sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed (/pool, Pool.Allocate)"}`)),
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, obs := log.NewObservedLogger(log.LevelWarn)
			marked := false
			err := withDiskTransferLock(context.Background(), tc.client, logger, transferStableID, "transfer_in", func(ctx context.Context) error {
				marked = diskTransferLockUnserialized(ctx)
				return nil
			})
			if err != nil || !marked {
				t.Fatalf("err=%v marked=%v, want the body run and its context marked unserialized", err, marked)
			}
			entries := obs.All()
			if len(entries) != 1 || entries[0].Level != log.LevelError || entries[0].Attrs["stable_id"] != transferStableID {
				t.Fatalf("log = %+v, want one error naming the disk", entries)
			}
		})
	}

	t.Run("held lock is not marked", func(t *testing.T) {
		t.Parallel()
		err := withDiskTransferLock(context.Background(), &parkerLockClient{pools: &recordingPoolService{}}, nil, transferStableID, "transfer_in",
			func(ctx context.Context) error {
				if diskTransferLockUnserialized(ctx) {
					t.Error("a transfer that holds the lock was marked unserialized")
				}
				return nil
			})
		if err != nil {
			t.Fatalf("withDiskTransferLock: %v", err)
		}
	})
}

// TestDiskTransferWindowContext covers the budget each parker window checks
// under the per-disk lock. A window opens on a deadline a window's reserve
// before the disk deadline, a window with too little of the lock left isn't
// opened, and a transfer without a budget, or with a test-sized one, runs on
// its context unchanged.
func TestDiskTransferWindowContext(t *testing.T) {
	t.Parallel()
	now := time.Now()
	reserve := parkerWindowReserveNow()

	t.Run("no budget", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		got, cancel, err := diskTransferWindowContext(ctx, transferStableID, now)
		defer cancel()
		if err != nil || got != ctx {
			t.Fatalf("err=%v, want the context back unchanged", err)
		}
	})

	t.Run("test-sized budget", func(t *testing.T) {
		t.Parallel()
		budget := diskTransferBudgetFor(time.Second, now.Add(time.Second))
		if budget.reserve != 0 {
			t.Fatalf("reserve = %s, want none for a TTL that can't hold one", budget.reserve)
		}
		ctx := context.WithValue(context.Background(), diskTransferBudgetKey{}, budget)
		got, cancel, err := diskTransferWindowContext(ctx, transferStableID, now)
		defer cancel()
		if err != nil || got != ctx {
			t.Fatalf("err=%v, want the context back unchanged", err)
		}
	})

	t.Run("room for a window", func(t *testing.T) {
		t.Parallel()
		ttl, _ := diskTransferLockTimeouts(context.Background())
		deadline := now.Add(reserve + diskTransferWindowNeed() + 10*time.Second)
		budget := diskTransferBudgetFor(ttl, deadline)
		if budget.reserve != reserve {
			t.Fatalf("reserve = %s, want the window reserve %s for the production TTL", budget.reserve, reserve)
		}
		ctx := context.WithValue(context.Background(), diskTransferBudgetKey{}, budget)
		got, cancel, err := diskTransferWindowContext(ctx, transferStableID, now)
		if err != nil {
			t.Fatalf("diskTransferWindowContext: %v", err)
		}
		defer cancel()
		if d, ok := got.Deadline(); !ok || !d.Equal(deadline.Add(-reserve)) {
			t.Fatalf("window deadline = %v (set %v), want %v", d, ok, deadline.Add(-reserve))
		}
	})

	t.Run("too little left", func(t *testing.T) {
		t.Parallel()
		deadline := now.Add(reserve + diskTransferWindowNeed() - time.Second)
		ctx := context.WithValue(context.Background(), diskTransferBudgetKey{}, diskTransferBudget{deadline: deadline, reserve: reserve})
		_, _, err := diskTransferWindowContext(ctx, transferStableID, now)
		var cpiErr *cpierrors.Error
		if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
			t.Fatalf("err = %v, want a retriable refusal", err)
		}
	})

	t.Run("the lock hands its transfer the budget", func(t *testing.T) {
		t.Parallel()
		err := withDiskTransferLock(context.Background(), &parkerLockClient{pools: &recordingPoolService{}}, nil, transferStableID, "transfer_in",
			func(ctx context.Context) error {
				budget, ok := ctx.Value(diskTransferBudgetKey{}).(diskTransferBudget)
				deadline, hasDeadline := ctx.Deadline()
				if !ok || !hasDeadline || budget.reserve != reserve || !budget.deadline.Equal(deadline) {
					t.Errorf("budget=%+v (set %v) deadline=%v, want the reserve and the lock's deadline", budget, ok, deadline)
				}
				return nil
			})
		if err != nil {
			t.Fatalf("withDiskTransferLock: %v", err)
		}
	})
}

// listingLockPools is the in-memory lock pool store with the pool listing the
// expired-lock sweep reads.
type listingLockPools struct {
	*fakeLockPools
	listErr error
}

func (l *listingLockPools) ListPoolComments(context.Context) (map[string]string, error) {
	if l.listErr != nil {
		return nil, l.listErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string, len(l.pools))
	for id, comment := range l.pools {
		out[id] = comment
	}
	return out, nil
}

// TestSweepExpiredDiskTransferLocks covers the sweep of per-disk lock
// sentinels a crashed transfer left behind. It deletes only a per-disk
// sentinel whose claim has expired and still reads the same, and it leaves a
// live claim, a claim that expired too recently for a stealer to have taken
// it, a comment it can't parse, another lock's sentinel, a claim that changed
// after the listing, and one whose re-read ran over the steal budget.
func TestSweepExpiredDiskTransferLocks(t *testing.T) {
	t.Parallel()
	now := time.Now()
	expired := encodeLockComment("crashed", now.Add(-time.Minute))
	live := encodeLockComment("holder", now.Add(time.Minute))
	diskPool := func(id string) string { return ClusterLockPoolName(diskTransferLockName(id)) }
	fixture := func() *listingLockPools {
		pools := &listingLockPools{fakeLockPools: newFakeLockPools()}
		pools.pools[diskPool("expired")] = expired
		pools.pools[diskPool("live")] = live
		pools.pools[diskPool("garbled")] = "owner=someone exp=soon"
		pools.pools[ClusterLockPoolName("vm-90000")] = expired
		pools.pools["bosh-parkers"] = ""
		return pools
	}
	at := func(t time.Time) context.Context {
		return withTestParkerLockClock(context.Background(), lockClock{
			now:   func() time.Time { return t },
			sleep: func(context.Context, time.Duration) error { return nil },
		})
	}

	t.Run("deletes only the expired disk sentinel", func(t *testing.T) {
		t.Parallel()
		pools := fixture()
		logger, obs := log.NewObservedLogger(log.LevelInfo)
		removed, err := SweepExpiredDiskTransferLocks(at(now), &parkerLockClient{pools: pools}, logger)
		if err != nil || removed != 1 {
			t.Fatalf("removed=%d err=%v, want one sentinel removed", removed, err)
		}
		if _, ok := pools.pools[diskPool("expired")]; ok {
			t.Fatal("the expired sentinel is still there")
		}
		for _, id := range []string{diskPool("live"), diskPool("garbled"), ClusterLockPoolName("vm-90000"), "bosh-parkers"} {
			if _, ok := pools.pools[id]; !ok {
				t.Fatalf("the sweep deleted %s", id)
			}
		}
		if obs.Len() != 1 {
			t.Fatalf("log = %+v, want one line for the removed sentinel", obs.All())
		}
	})

	t.Run("a claim that expired just now is left to a stealer", func(t *testing.T) {
		t.Parallel()
		pools := fixture()
		// The comment keeps whole seconds, so the sweep runs at a whole second.
		base := now.Truncate(time.Second)
		recent := encodeLockComment("crashed", base.Add(-sweepExpiredLockAge()))
		pools.pools[diskPool("recent")] = recent
		removed, err := SweepExpiredDiskTransferLocks(at(base), &parkerLockClient{pools: pools}, nil)
		if err != nil || removed != 1 {
			t.Fatalf("removed=%d err=%v, want only the long-expired sentinel removed", removed, err)
		}
		if pools.pools[diskPool("recent")] != recent {
			t.Fatal("the sweep deleted a claim that expired inside the steal budget and grace")
		}
		// Once the claim is older than the steal budget and grace, the sweep
		// takes it.
		removed, err = SweepExpiredDiskTransferLocks(at(base.Add(time.Second)), &parkerLockClient{pools: pools}, nil)
		if err != nil || removed != 1 {
			t.Fatalf("removed=%d err=%v, want the now-old claim removed", removed, err)
		}
	})

	t.Run("claim changed after the listing", func(t *testing.T) {
		t.Parallel()
		pools := fixture()
		pools.getFn = func(id string) (string, bool, error, bool) {
			if id == diskPool("expired") {
				return live, true, nil, true
			}
			return "", false, nil, false
		}
		removed, err := SweepExpiredDiskTransferLocks(at(now), &parkerLockClient{pools: pools}, nil)
		if err != nil || removed != 0 || pools.deleteN != 0 {
			t.Fatalf("removed=%d deletes=%d err=%v, want a fresh claim left alone", removed, pools.deleteN, err)
		}
	})

	t.Run("re-read over the steal budget", func(t *testing.T) {
		t.Parallel()
		pools := fixture()
		var ticks atomic.Int64
		ctx := withTestParkerLockClock(context.Background(), lockClock{
			now: func() time.Time {
				return now.Add(time.Duration(ticks.Add(1)) * (clusterLockStealBudget + time.Second))
			},
			sleep: func(context.Context, time.Duration) error { return nil },
		})
		removed, err := SweepExpiredDiskTransferLocks(ctx, &parkerLockClient{pools: pools}, nil)
		if err != nil || pools.deleteN != 0 {
			t.Fatalf("removed=%d deletes=%d err=%v, want no delete after a slow re-read", removed, pools.deleteN, err)
		}
	})

	t.Run("a failed re-read is reported and the rest swept", func(t *testing.T) {
		t.Parallel()
		pools := fixture()
		pools.pools[diskPool("expired-2")] = expired
		pools.getFn = func(id string) (string, bool, error, bool) {
			if id == diskPool("expired") {
				return "", false, errors.New("connection reset"), true
			}
			return "", false, nil, false
		}
		removed, err := SweepExpiredDiskTransferLocks(at(now), &parkerLockClient{pools: pools}, nil)
		if err == nil || removed != 1 {
			t.Fatalf("removed=%d err=%v, want the other sentinel removed and the failure returned", removed, err)
		}
		if _, ok := pools.pools[diskPool("expired")]; !ok {
			t.Fatal("a sentinel whose re-read failed was deleted")
		}
	})

	t.Run("no lister", func(t *testing.T) {
		t.Parallel()
		pools := newFakeLockPools()
		pools.pools[diskPool("expired")] = expired
		removed, err := SweepExpiredDiskTransferLocks(at(now), &parkerLockClient{pools: pools}, nil)
		if err != nil || removed != 0 || len(pools.pools) != 1 {
			t.Fatalf("removed=%d err=%v, want nothing done without a pool listing", removed, err)
		}
	})

	t.Run("listing fails", func(t *testing.T) {
		t.Parallel()
		pools := fixture()
		pools.listErr = errors.New("connection refused")
		if _, err := SweepExpiredDiskTransferLocks(at(now), &parkerLockClient{pools: pools}, nil); err == nil {
			t.Fatal("a failed listing was not reported")
		}
	})
}

// TestTracedPoolService_ListPoolComments forwards the listing when the wrapped
// service offers one and fails it when it does not.
func TestTracedPoolService_ListPoolComments(t *testing.T) {
	t.Parallel()
	tracer, _ := newTestTracer(t)
	inner := &listingLockPools{fakeLockPools: newFakeLockPools()}
	inner.pools["bosh-lock-disk-x"] = "owner=a exp=1"
	traced := &tracedPoolService{PoolService: inner, tracer: tracer}
	got, err := traced.ListPoolComments(context.Background())
	if err != nil || got["bosh-lock-disk-x"] != "owner=a exp=1" {
		t.Fatalf("listing was not forwarded: %v, %v", got, err)
	}
	plain := &tracedPoolService{PoolService: &fakePoolService{}, tracer: tracer}
	if _, err := plain.ListPoolComments(context.Background()); err == nil {
		t.Fatal("a wrapped service without a listing answered one")
	}
}
