// disk_transfer_races_internal_test.go covers what a detach-side transfer does
// when another operation got to its disk first: a second transfer that would
// pick another parker, a disk renamed on its source, a disk another VM or
// parker now holds, a record a crashed transfer left over a landed volume,
// and the reads and writes that can fail while the disk sits between source
// and parker.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// hookedScanClient is the scan fake with hooks on the node calls a test needs
// to change or fail at a chosen moment. Each hook runs before the fake's own
// handling and outside its mutex, and an error a hook returns fails the call.
type hookedScanClient struct {
	*scanFakeClient
	beforePending func(vmid string) error
	beforeUpdate  func(vmid string, params *sdknodes.UpdateQemuConfigParams) error
	beforeListing func() error
	// hideGuests, when set, returns the guests a node listing made under
	// ctx leaves out, so one transfer can see a different set of parkers
	// from another.
	hideGuests func(ctx context.Context) map[int]bool
	observedMu sync.Mutex
	observed   []string
}

func (h *hookedScanClient) Nodes() sdknodes.Service {
	inner := *(h.scanFakeClient.Nodes().(*fakeNodesService))
	pending, update, listing, listQemu := inner.listQemuPendingFn, inner.updateQemuConfigFn, inner.listStorageContentFn, inner.listQemuFn
	inner.listQemuPendingFn = func(ctx context.Context, node, vmid string) (*sdknodes.ListQemuPendingResponse, error) {
		if h.beforePending != nil {
			if err := h.beforePending(vmid); err != nil {
				return nil, err
			}
		}
		return pending(ctx, node, vmid)
	}
	inner.updateQemuConfigFn = func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
		if h.beforeUpdate != nil {
			if err := h.beforeUpdate(vmid, params); err != nil {
				return err
			}
		}
		return update(ctx, node, vmid, params)
	}
	inner.listStorageContentFn = func(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		if h.beforeListing != nil {
			if err := h.beforeListing(); err != nil {
				return nil, err
			}
		}
		return listing(ctx, node, storage, params)
	}
	inner.listQemuFn = func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		resp, err := listQemu(ctx, node, params)
		if err != nil || resp == nil || h.hideGuests == nil {
			return resp, err
		}
		hidden := h.hideGuests(ctx)
		kept := sdknodes.ListQemuResponse{}
		for _, row := range *resp {
			var item struct {
				Vmid int `json:"vmid"`
			}
			if jsonErr := json.Unmarshal(row, &item); jsonErr == nil && hidden[item.Vmid] {
				continue
			}
			kept = append(kept, row)
		}
		return &kept, nil
	}
	return &inner
}

// ObserveDiskRelocated records each relocation the transfer reports, the way
// the journal-managed lifecycle client follows the volume.
func (h *hookedScanClient) ObserveDiskRelocated(_ context.Context, stableID, from, to string) error {
	h.observedMu.Lock()
	defer h.observedMu.Unlock()
	h.observed = append(h.observed, stableID+":"+from+"->"+to)
	return nil
}

// recordDesc is a parker description carrying one record of the test disk.
func recordDesc(volid, slot string) string {
	return `<!--BOSH:{"bosh_parked_disks":{"` + transferStableID + `":{"disk_cid":"pvd-test","parked_at":"t",` +
		`"node":"pve1","volid":"` + volid + `","slot":"` + slot + `","source_vm_cid":"700"}}}-->`
}

// retriableNotLeftSource fails t unless err is retriable and is not the
// left-source answer the transfer re-resolves on.
func retriableNotLeftSource(t *testing.T, err error) {
	t.Helper()
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
		t.Fatalf("err = %v, want a retriable error", err)
	}
	if errors.Is(err, errDiskLeftSource) {
		t.Fatalf("err = %v, want an error that doesn't send the transfer to the re-resolve", err)
	}
}

type otherParkerKey struct{}

// TestTransferDiskToParker_RaceAcrossTwoParkers runs two transfers of one disk
// that pick different parkers, because the parker listing one of them reads
// first doesn't show parker 90000 yet. Without the per-disk lock each would
// move the disk into its own parker. The lock serializes them. The first moves
// the disk, and the second opens its window on the other parker, finds the
// source empty before it writes anything, and finds the disk on the first
// parker through the re-resolve.
func TestTransferDiskToParker_RaceAcrossTwoParkers(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700:   {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
		90001: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})
	inner.rows = []map[string]any{
		clusterRow(700, ""),
		clusterRow(90000, "bosh-cpi;bosh-parker"),
		clusterRow(90001, "bosh-cpi;bosh-parker"),
	}
	c := &hookedScanClient{scanFakeClient: inner}
	var hidOnce atomic.Bool
	c.hideGuests = func(ctx context.Context) map[int]bool {
		if ctx.Value(otherParkerKey{}) != nil && hidOnce.CompareAndSwap(false, true) {
			return map[int]bool{90000: true}
		}
		return nil
	}

	diskLock := ClusterLockPoolName(diskTransferLockName(transferStableID))
	var diskCreates atomic.Int32
	contended := make(chan struct{})
	var contendedOnce sync.Once
	var firstWindow, timedOut atomic.Bool
	pools := &gatedLockPools{fakeLockPools: newFakeLockPools()}
	pools.beforeCreate = func(poolID string) {
		if poolID == diskLock {
			if diskCreates.Add(1) >= 2 {
				contendedOnce.Do(func() { close(contended) })
			}
			return
		}
		if strings.HasPrefix(poolID, ClusterLockPoolName("vm-")) && firstWindow.CompareAndSwap(false, true) {
			select {
			case <-contended:
			case <-time.After(10 * time.Second):
				timedOut.Store(true)
			}
		}
	}
	inner.pools = pools

	base := WithClusterLockPollForTest(context.Background(), 5*time.Millisecond)
	ctxs := []context.Context{base, context.WithValue(base, otherParkerKey{}, true)}
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	landed := make([]string, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			landed[i], errs[i] = TransferDiskToParker(ctxs[i], c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
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
	// Each transfer opened a window on the parker it picked, so the two
	// really did go to different parkers.
	for _, vmid := range []string{"vm-90000", "vm-90001"} {
		if !slices.Contains(pools.calls, "create:"+ClusterLockPoolName(vmid)) {
			t.Fatalf("no window opened on %s; pool calls=%v", vmid, pools.calls)
		}
	}
	if landed[0] != landed[1] {
		t.Fatalf("landed = %v, want both transfers to report the one parked volume", landed)
	}
	moves := 0
	for _, e := range inner.events {
		if strings.HasPrefix(e, "move:") {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("moves = %d, want exactly one; events=%v", moves, inner.events)
	}
	// Exactly one parker carries the disk and keeps its record, and the other
	// was never written to.
	holders := 0
	for _, vmid := range []int{90000, 90001} {
		cfg := inner.configs[vmid]
		_, records, _ := parseParkerSentinel(DescriptionFromConfig(cfg))
		_, _, carried := parkerSlotCarryingSerial(cfg, transferStableID)
		_, recorded := records[transferStableID]
		if carried != recorded {
			t.Fatalf("parker %d carries the serial=%v but keeps a record=%v", vmid, carried, recorded)
		}
		if carried {
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("%d parkers carry the disk, want one", holders)
	}
}

// TestResumeDiskTransferToParker_RacesAFreshTransferAcrossTwoParkers runs the
// resume of a crashed transfer of one disk against a fresh transfer of the
// same disk that picks a different parker, because the parker listing it
// reads first doesn't show parker 90000. Without the per-disk lock in the
// resume, the two would each move the disk toward their own parker. The lock
// serializes them. The resume holds the disk lock while the fresh transfer
// waits on it, moves the disk onto 90000, and the fresh transfer then finds
// the source empty in its window on 90001 and the disk on 90000 through the
// re-resolve.
func TestResumeDiskTransferToParker_RacesAFreshTransferAcrossTwoParkers(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700:   {"unused0": "data:vm-700-disk-1"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
		90001: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})
	inner.rows = []map[string]any{
		clusterRow(700, ""),
		clusterRow(90000, "bosh-cpi;bosh-parker"),
		clusterRow(90001, "bosh-cpi;bosh-parker"),
	}
	c := &hookedScanClient{scanFakeClient: inner}
	var hidOnce atomic.Bool
	c.hideGuests = func(ctx context.Context) map[int]bool {
		if ctx.Value(otherParkerKey{}) != nil && hidOnce.CompareAndSwap(false, true) {
			return map[int]bool{90000: true}
		}
		return nil
	}

	// The resume's parker window waits until the fresh transfer has tried the
	// disk lock, so the two really contend for it.
	diskLock := ClusterLockPoolName(diskTransferLockName(transferStableID))
	var diskCreates atomic.Int32
	contended := make(chan struct{})
	var contendedOnce sync.Once
	var timedOut atomic.Bool
	resumeWindow := ClusterLockPoolName("vm-90000")
	var gated atomic.Bool
	pools := &gatedLockPools{fakeLockPools: newFakeLockPools()}
	pools.beforeCreate = func(poolID string) {
		if poolID == diskLock {
			if diskCreates.Add(1) >= 2 {
				contendedOnce.Do(func() { close(contended) })
			}
			return
		}
		if poolID == resumeWindow && gated.CompareAndSwap(false, true) {
			select {
			case <-contended:
			case <-time.After(10 * time.Second):
				timedOut.Store(true)
			}
		}
	}
	inner.pools = pools

	base := WithClusterLockPollForTest(context.Background(), 5*time.Millisecond)
	intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-700-disk-1", SourceVMCID: "700"}
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	var resumed, transferred string
	var resumeErr, transferErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		resumed, resumeErr = ResumeDiskTransferToParker(base, c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
	})
	wg.Go(func() {
		// Start the fresh transfer once the resume holds the disk lock.
		deadline := time.Now().Add(10 * time.Second)
		for diskCreates.Load() < 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		transferred, transferErr = TransferDiskToParker(context.WithValue(base, otherParkerKey{}, true),
			c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	})
	wg.Wait()

	if timedOut.Load() {
		t.Fatal("the fresh transfer never tried the per-disk lock while the resume held it")
	}
	if resumeErr != nil {
		t.Fatalf("resume: %v", resumeErr)
	}
	if transferErr != nil {
		t.Fatalf("fresh transfer: %v", transferErr)
	}
	if !slices.Contains(pools.calls, "create:"+ClusterLockPoolName("vm-90001")) {
		t.Fatalf("the fresh transfer never opened a window on parker 90001; pool calls=%v", pools.calls)
	}
	if !strings.HasPrefix(resumed, "data:vm-90000-disk-") || transferred != resumed {
		t.Fatalf("resumed=%q transferred=%q, want both to report the one volume parked on 90000", resumed, transferred)
	}
	moves := 0
	for _, e := range inner.events {
		if strings.HasPrefix(e, "move:") {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("moves = %d, want exactly one; events=%v", moves, inner.events)
	}
	holders := 0
	for _, vmid := range []int{90000, 90001} {
		if _, _, carried := parkerSlotCarryingSerial(inner.configs[vmid], transferStableID); carried {
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("%d parkers carry the disk, want one", holders)
	}
}

// TestTransferDiskToParker_RestartsUnderTheNameTheDiskHasNow covers the
// re-resolve of a disk that is still on its source under a name other than
// the one the transfer started with. The window writes nothing for the old
// name, the observer is told about the new name, and the transfer starts again
// under it and parks the disk.
func TestTransferDiskToParker_RestartsUnderTheNameTheDiskHasNow(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700:   {"scsi1": "data:vm-700-disk-5,serial=" + transferStableID + ",size=10G"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})
	c := &hookedScanClient{scanFakeClient: inner}
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	if err != nil {
		t.Fatalf("TransferDiskToParker: %v", err)
	}
	if !strings.HasPrefix(landed, "data:vm-90000-disk-") {
		t.Fatalf("landed = %q, want the disk parked on 90000", landed)
	}
	want := transferStableID + ":data:vm-700-disk-1->data:vm-700-disk-5"
	if len(c.observed) != 1 || c.observed[0] != want {
		t.Fatalf("observed = %v, want the one relocation %q", c.observed, want)
	}
	if entry := inner.parkedEntries(t)[transferStableID]; entry.Volid != landed {
		t.Fatalf("record = %+v, want it to name %q", entry, landed)
	}
}

// TestTransferDiskToParker_CrashAfterLandIsNeverOverwritten is the state a
// transfer leaves when it dies after its volume landed on the parker slot its
// record names and before it wrote the serial. A later transfer of the disk
// never writes over that record, because the resume finds the volume only
// through the slot it names. That holds whether the source still names the
// volume the later transfer starts with or not.
func TestTransferDiskToParker_CrashAfterLandIsNeverOverwritten(t *testing.T) {
	t.Parallel()
	const landedVolume = "data:vm-90000-disk-0"
	desc := recordDesc("data:vm-700-disk-1", "scsi0")
	cases := []struct {
		name   string
		source map[string]any
	}{
		{"source let the volume go", map[string]any{}},
		{"source still names the volume", map[string]any{"scsi1": "data:vm-700-disk-1,size=10G"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newScanFakeClient(map[int]map[string]any{
				700: tc.source,
				90000: {
					cfgKeyTags:      "bosh-cpi;bosh-parker",
					paramProtection: true,
					"scsi0":         landedVolume,
					"description":   desc,
				},
			})
			pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
			_, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
			var cpiErr *cpierrors.Error
			if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
				t.Fatalf("err = %v, want a retriable error that leaves the record to the resume", err)
			}
			if got, _ := c.configs[90000]["description"].(string); got != desc {
				t.Fatalf("the record of the landed volume was rewritten: %q", got)
			}
			if after := mutationEvents(c.events, 0); len(after) != 0 {
				t.Fatalf("the transfer changed state: %v", after)
			}
		})
	}

	t.Run("the intent write refuses it on its own read", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker", "scsi0": landedVolume, "description": desc},
		})
		intent := parkerProvEntry{DiskCID: "pvd-test", Node: "pve1", Volid: "data:vm-700-disk-1", Slot: "scsi1", SourceVMCID: "700"}
		err := writeParkerTransferIntent(context.Background(), c, nil, "pve1", 90000, transferStableID, intent, transferTestCfg)
		if !errors.Is(err, errDiskLeftSource) {
			t.Fatalf("err = %v, want the left-source refusal", err)
		}
		if got, _ := c.configs[90000]["description"].(string); got != desc {
			t.Fatalf("the record of the landed volume was rewritten: %q", got)
		}
	})
}

// TestTransferDiskToParker_RecordSlotHoldingAnotherDiskIsReplaced covers a
// stale record of the disk whose slot now holds another disk, which its
// stable-ID serial says. That volume is not this disk's landing, so the record
// doesn't block the transfer. The transfer replaces the record, parks the
// disk on another slot, and leaves the other disk's drive alone.
func TestTransferDiskToParker_RecordSlotHoldingAnotherDiskIsReplaced(t *testing.T) {
	t.Parallel()
	const otherDrive = "data:vm-90000-disk-0,serial=bpd-99887766ffeeddcc,size=10G"
	desc := recordDesc("data:vm-700-disk-1", "scsi0")

	t.Run("the transfer parks the disk", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
			90000: {
				cfgKeyTags:      "bosh-cpi;bosh-parker",
				paramProtection: true,
				"scsi0":         otherDrive,
				"description":   desc,
			},
		})
		pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
		landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
		if err != nil {
			t.Fatalf("TransferDiskToParker: %v", err)
		}
		slot, parked, carried := parkerSlotCarryingSerial(c.configs[90000], transferStableID)
		if !carried || parked != landed || slot == "scsi0" {
			t.Fatalf("slot=%q parked=%q carried=%v landed=%q, want the disk on a slot other than scsi0", slot, parked, carried, landed)
		}
		if got, _ := c.configs[90000]["scsi0"].(string); got != otherDrive {
			t.Fatalf("scsi0 = %q, want the other disk's drive left alone", got)
		}
		if entry := c.parkedEntries(t)[transferStableID]; entry.Volid != landed || entry.Slot != slot {
			t.Fatalf("record = %+v, want it to name %q on %s", entry, landed, slot)
		}
	})

	t.Run("the intent write replaces the record", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker", "scsi0": otherDrive, "description": desc},
		})
		intent := parkerProvEntry{DiskCID: "pvd-test", Node: "pve1", Volid: "data:vm-700-disk-1", Slot: "scsi1", SourceVMCID: "700"}
		if err := writeParkerTransferIntent(context.Background(), c, nil, "pve1", 90000, transferStableID, intent, transferTestCfg); err != nil {
			t.Fatalf("writeParkerTransferIntent: %v", err)
		}
		if entry := c.parkedEntries(t)[transferStableID]; entry.Slot != "scsi1" {
			t.Fatalf("record = %+v, want the intent on scsi1", entry)
		}
	})

	t.Run("a drive with no serial still blocks the record", func(t *testing.T) {
		t.Parallel()
		_, records, _ := parseParkerSentinel(desc)
		cfg := map[string]any{"scsi0": "data:vm-90000-disk-0,size=10G"}
		if held, occupied := parkerRecordSlotHolds(cfg, records[transferStableID], transferStableID); !occupied || held != "data:vm-90000-disk-0" {
			t.Fatalf("held=%q occupied=%v, want a serial-less landing to hold the slot", held, occupied)
		}
		cfg["scsi0"] = "data:vm-90000-disk-0,serial=" + transferStableID + ",size=10G"
		if _, occupied := parkerRecordSlotHolds(cfg, records[transferStableID], transferStableID); !occupied {
			t.Fatal("a drive carrying this disk's own serial did not hold the slot")
		}
	})
}

// sourceLetsGoAfterIntent returns a client whose source VM 700 stops naming
// its volume on the first source read after the transfer writes its intent,
// as if another operation took the volume between the two reads. Every parker
// description write after the intent fails when failTakeBack is set.
func sourceLetsGoAfterIntent(failTakeBack bool) *hookedScanClient {
	inner := newScanFakeClient(map[int]map[string]any{
		700:   {"scsi1": "data:vm-777-disk-1,serial=" + transferStableID + ",size=10G"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})
	var intentWritten atomic.Bool
	c := &hookedScanClient{scanFakeClient: inner}
	c.beforeUpdate = func(vmid string, params *sdknodes.UpdateQemuConfigParams) error {
		if vmid != "90000" || params.Description == nil {
			return nil
		}
		if intentWritten.CompareAndSwap(false, true) {
			return nil
		}
		if failTakeBack {
			return errors.New("fake: description write refused")
		}
		return nil
	}
	c.beforePending = func(vmid string) error {
		if vmid == "700" && intentWritten.Load() {
			inner.mu.Lock()
			delete(inner.configs[700], "scsi1")
			inner.mu.Unlock()
		}
		return nil
	}
	return c
}

// TestTransferDiskToParker_IntentTakeBack covers a source that lets the volume
// go between the transfer's intent write and its slot delete. The transfer
// takes its intent back and re-resolves the disk, and when the take-back fails
// it logs the failure and fails retriably without the re-resolve, keeping the
// record for the Director's retry to resume from.
func TestTransferDiskToParker_IntentTakeBack(t *testing.T) {
	t.Parallel()
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return time.Millisecond })

	t.Run("taken back", func(t *testing.T) {
		t.Parallel()
		c := sourceLetsGoAfterIntent(false)
		_, err := TransferDiskToParker(ctx, c, nil, "pve1", 700, "data:vm-777-disk-1", transferTestCfg, pctx)
		var cpiErr *cpierrors.Error
		if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
			t.Fatalf("err = %v, want a retriable error from the re-resolve", err)
		}
		if entries := c.parkedEntries(t); len(entries) != 0 {
			t.Fatalf("the intent of a disk the transfer never moved stayed: %+v", entries)
		}
		if c.attachCalls != 0 {
			t.Fatalf("attaches = %d, want none", c.attachCalls)
		}
	})

	t.Run("take-back fails", func(t *testing.T) {
		t.Parallel()
		c := sourceLetsGoAfterIntent(true)
		logger, obs := log.NewObservedLogger(log.LevelError)
		_, err := TransferDiskToParker(ctx, c, logger, "pve1", 700, "data:vm-777-disk-1", transferTestCfg, pctx)
		retriableNotLeftSource(t, err)
		if !strings.Contains(err.Error(), "could not be taken back") {
			t.Fatalf("err = %v, want it to say the intent stayed", err)
		}
		if _, ok := c.parkedEntries(t)[transferStableID]; !ok {
			t.Fatal("the intent record is gone, so the retry has nothing to resume from")
		}
		logged := false
		for _, e := range obs.All() {
			if strings.Contains(e.Message, "could not take back the intent record") {
				logged = true
			}
		}
		if !logged || c.attachCalls != 0 {
			t.Fatalf("logged=%v attaches=%d, want the failure logged and nothing attached; log=%+v", logged, c.attachCalls, obs.All())
		}
	})
}

// releasedSourceFixture is a source VM 700 holding a volume named for VM 777,
// so the slot delete leaves no unused entry and the parker attaches the volume
// by config edit after the storage listing shows it.
func releasedSourceFixture() *hookedScanClient {
	return &hookedScanClient{scanFakeClient: newScanFakeClient(map[int]map[string]any{
		700:   {"scsi1": "data:vm-777-disk-1,serial=" + transferStableID + ",size=10G"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})}
}

// TestTransferDiskToParker_ReleasedVolumeProbeRetries covers the storage
// listing a config-edit attach needs after the source slot is gone. A read
// that fails is tried again, and one that keeps failing fails the transfer
// retriably without the re-resolve, with the intent record kept for the
// resume, because the volume is off the source and not yet on the parker.
func TestTransferDiskToParker_ReleasedVolumeProbeRetries(t *testing.T) {
	t.Parallel()
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return time.Millisecond })

	t.Run("a read that fails once", func(t *testing.T) {
		t.Parallel()
		c := releasedSourceFixture()
		var reads atomic.Int32
		c.beforeListing = func() error {
			if reads.Add(1) == 1 {
				return errors.New("fake: connection reset")
			}
			return nil
		}
		landed, err := TransferDiskToParker(ctx, c, nil, "pve1", 700, "data:vm-777-disk-1", transferTestCfg, pctx)
		if err != nil {
			t.Fatalf("TransferDiskToParker: %v", err)
		}
		if landed != "data:vm-777-disk-1" || c.attachCalls != 1 || reads.Load() < 2 {
			t.Fatalf("landed=%q attaches=%d reads=%d, want the volume attached after a second read", landed, c.attachCalls, reads.Load())
		}
	})

	t.Run("a read that keeps failing", func(t *testing.T) {
		t.Parallel()
		c := releasedSourceFixture()
		var reads atomic.Int32
		c.beforeListing = func() error {
			reads.Add(1)
			return errors.New("fake: connection reset")
		}
		_, err := TransferDiskToParker(ctx, c, nil, "pve1", 700, "data:vm-777-disk-1", transferTestCfg, pctx)
		retriableNotLeftSource(t, err)
		if reads.Load() < int32(parkerWindowMaxAttempts) {
			t.Fatalf("reads = %d, want %d", reads.Load(), parkerWindowMaxAttempts)
		}
		if c.attachCalls != 0 {
			t.Fatalf("attaches = %d, want none without the listing", c.attachCalls)
		}
		entry, ok := c.parkedEntries(t)[transferStableID]
		if !ok || entry.Volid != "data:vm-777-disk-1" {
			t.Fatalf("intent = %+v (found %v), want it kept for the resume", entry, ok)
		}
	})
}

// TestTransferDiskToParker_DiskAttachedElsewhere covers a disk that left its
// source before the transfer moved it and is now on another, non-parker VM.
// The transfer moves nothing and returns the typed answer the handlers treat
// as a disk the source no longer holds.
func TestTransferDiskToParker_DiskAttachedElsewhere(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {},
		701:   {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	_, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	elsewhere, ok := IsDiskAttachedElsewhere(err)
	if !ok || elsewhere.VMID != 701 || elsewhere.Volid != "data:vm-700-disk-1" {
		t.Fatalf("err = %v, want the disk reported attached to vm 701", err)
	}
	if after := mutationEvents(c.events, 0); len(after) != 0 {
		t.Fatalf("the transfer changed state: %v", after)
	}
}

// TestTransferDiskToParker_BirthUnknownRefusalIsPlainRetriable covers a
// re-resolve without the disk's CID, where the volid the transfer started with
// stands in for the birth name. Another guest naming that volume says nothing
// about this disk, so the transfer fails with a plain retriable error rather
// than the birth-name refusal.
func TestTransferDiskToParker_BirthUnknownRefusalIsPlainRetriable(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {},
		701:   {"scsi1": "data:vm-700-disk-1,size=10G"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})
	pctx := ParkContext{SourceVMCID: "700", StableID: transferStableID}
	_, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	retriableNotLeftSource(t, err)
	if !strings.Contains(err.Error(), "without its CID") {
		t.Fatalf("err = %v, want the refusal that names the missing CID", err)
	}
	var held *DiskBirthNameHeldError
	if errors.As(err, &held) {
		t.Fatalf("err = %v, want no birth-name refusal without the disk's CID", err)
	}
}

// TestTransferDiskToParker_ParkedWithoutRecordTakesTheSourceOptions covers a
// disk the re-resolve finds on a parker that keeps no record of it. The
// transfer writes the record inside that parker's window, and its option
// overrides come from the source VM's record of them, so an operator's
// update_disk survives. A source it can't read fails the transfer retriably
// rather than record a disk without its overrides.
func TestTransferDiskToParker_ParkedWithoutRecordTakesTheSourceOptions(t *testing.T) {
	t.Parallel()
	const parked = "data:vm-90001-disk-0"
	fixture := func(t *testing.T) *scanFakeClient {
		c := newScanFakeClient(map[int]map[string]any{
			700:   {},
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
			90001: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true, "scsi0": parked + ",serial=" + transferStableID},
		})
		c.rows = []map[string]any{
			clusterRow(700, ""),
			clusterRow(90000, "bosh-cpi;bosh-parker"),
			clusterRow(90001, "bosh-cpi;bosh-parker"),
		}
		if err := SetVMDiskOptOverlay(context.Background(), c, "pve1", 700, transferStableID, map[string]string{"cache": "writeback"}); err != nil {
			t.Fatalf("SetVMDiskOptOverlay: %v", err)
		}
		return c
	}
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}

	t.Run("source overrides recorded", func(t *testing.T) {
		t.Parallel()
		c := fixture(t)
		landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
		if err != nil {
			t.Fatalf("TransferDiskToParker: %v", err)
		}
		if landed != parked {
			t.Fatalf("landed = %q, want %q", landed, parked)
		}
		_, records, _ := parseParkerSentinel(DescriptionFromConfig(c.configs[90001]))
		record, ok := records[transferStableID]
		if !ok || record.Volid != parked || record.Slot != "scsi0" || record.Opts["cache"] != "writeback" {
			t.Fatalf("record = %+v (found %v), want the landed slot and the source's overrides", record, ok)
		}
	})

	t.Run("source unreadable", func(t *testing.T) {
		t.Parallel()
		c := fixture(t)
		c.configErr = map[int]error{700: errors.New("fake: connection reset")}
		_, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
		if err == nil {
			t.Fatal("the transfer succeeded without the source's overrides")
		}
		_, records, _ := parseParkerSentinel(DescriptionFromConfig(c.configs[90001]))
		if _, ok := records[transferStableID]; ok {
			t.Fatalf("a record was written without the source's overrides: %+v", records)
		}
	})
}

// TestFinishOnParkerHolding covers the re-resolve's finish on a parker the
// disk landed on. It finishes the record inside that parker's window, logs the
// sweep debt of a parker on another node than the source's, and fails
// retriably when the window no longer reads the serial there.
func TestFinishOnParkerHolding(t *testing.T) {
	t.Parallel()
	const parked = "data:vm-90001-disk-0"
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID, Opts: map[string]string{"cache": "none"}}
	holder := DiskHolder{Found: true, IsParker: true, VMID: 90001, Node: "pve2", Slot: "scsi0"}

	t.Run("cross-node parker", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90001: {
				cfgKeyTags:      "bosh-cpi;bosh-parker",
				"scsi0":         parked + ",serial=" + transferStableID,
				"description":   recordDesc("data:vm-700-disk-1", "scsi3"),
				paramProtection: true,
			},
		})
		logger, obs := log.NewObservedLogger(log.LevelWarn)
		landed, err := finishOnParkerHolding(context.Background(), c, logger, "pve1", 700, "data:vm-700-disk-1", holder, transferTestCfg, pctx)
		if err != nil || landed != parked {
			t.Fatalf("landed=%q err=%v, want the parked volume", landed, err)
		}
		_, records, _ := parseParkerSentinel(DescriptionFromConfig(c.configs[90001]))
		if record := records[transferStableID]; record.Volid != parked || record.Slot != "scsi0" {
			t.Fatalf("record = %+v, want it finished on the landed slot", record)
		}
		debt := false
		for _, e := range obs.All() {
			if strings.Contains(e.Message, "parked on another node") && e.Attrs["parker_node"] == "pve2" {
				debt = true
			}
		}
		if !debt {
			t.Fatalf("log = %+v, want the cross-node sweep debt logged", obs.All())
		}
	})

	t.Run("serial gone by the window", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90001: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
		})
		_, err := finishOnParkerHolding(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", holder, transferTestCfg, pctx)
		retriableNotLeftSource(t, err)
	})
}

// TestTransferDiskToParker_ManagedTransferRacingDeleteVM runs a
// journal-managed transfer, the kind detach_disk makes for a managed disk, at
// the same time as the plain transfer delete_vm makes to preserve the same
// disk. Both take the per-disk lock, so whichever holds it first moves the
// disk and the other waits, then finds the disk parked and moves nothing.
// When delete_vm's transfer moved it, the managed transfer stamps its
// allocation onto the record delete_vm left, which is the only write it makes,
// so the parker's record ends up naming the allocation in both orders.
func TestTransferDiskToParker_ManagedTransferRacingDeleteVM(t *testing.T) {
	t.Parallel()
	managed := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID, AllocationID: "alloc-1", AllocationNamespace: "ns-1"}
	plain := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	for name, order := range map[string][2]ParkContext{
		"managed transfer holds the lock first":   {managed, plain},
		"delete_vm transfer holds the lock first": {plain, managed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			race := raceTwoTransfers(t, order)
			if race.errs[0] != nil || !strings.HasPrefix(race.landed[0], "data:vm-90000-disk-") {
				t.Fatalf("the mover returned %q, %v", race.landed[0], race.errs[0])
			}
			if race.errs[1] != nil || race.landed[1] != race.landed[0] {
				t.Fatalf("the waiter returned %q, %v; want the mover's %q", race.landed[1], race.errs[1], race.landed[0])
			}
			if race.moves != 1 {
				t.Fatalf("moves = %d, want exactly 1; events=%v", race.moves, race.events)
			}
			wantAfter := 0
			if order[0].AllocationID == "" {
				wantAfter = 1
			}
			if len(race.after) != wantAfter || (wantAfter == 1 && !strings.HasPrefix(race.after[0], "description:90000")) {
				t.Fatalf("the waiter's writes after the mover finished = %v, want %d record stamp", race.after, wantAfter)
			}
			entry, ok := race.client.parkedEntries(t)[transferStableID]
			if !ok || entry.Volid != race.landed[0] || entry.AllocationID != managed.AllocationID || entry.AllocationNamespace != managed.AllocationNamespace {
				t.Fatalf("parker record = %+v (found %v), want a record naming %q and the allocation", entry, ok, race.landed[0])
			}
		})
	}
}

// transferRace is what raceTwoTransfers saw.
type transferRace struct {
	client *scanFakeClient
	landed [2]string
	errs   [2]error
	events []string
	// moves counts every move, and after holds the waiter's mutations,
	// which are the ones made after the mover released the disk lock.
	moves int
	after []string
}

// raceTwoTransfers runs a transfer of the usual fixture's disk with
// order[0]'s context, and once it holds the per-disk lock and has reached its
// parker window, a second one with order[1]'s. The first waits in its window
// until the second has tried the disk lock, so the two really overlap.
func raceTwoTransfers(t *testing.T, order [2]ParkContext) transferRace {
	t.Helper()
	c := transferSourceAndParker()
	diskLock := ClusterLockPoolName(diskTransferLockName(transferStableID))
	parkerLock := ClusterLockPoolName("vm-90000")

	var diskCreates atomic.Int32
	inWindow := make(chan struct{})
	contended := make(chan struct{})
	var contendedOnce sync.Once
	var firstWindow, timedOut atomic.Bool
	pools := &gatedLockPools{fakeLockPools: newFakeLockPools()}
	pools.beforeCreate = func(poolID string) {
		if poolID == diskLock && diskCreates.Add(1) >= 2 {
			contendedOnce.Do(func() { close(contended) })
		}
		if poolID != parkerLock || !firstWindow.CompareAndSwap(false, true) {
			return
		}
		close(inWindow)
		select {
		case <-contended:
		case <-time.After(10 * time.Second):
			timedOut.Store(true)
		}
	}
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
	race := transferRace{client: c}
	var wg sync.WaitGroup
	wg.Go(func() {
		race.landed[0], race.errs[0] = TransferDiskToParker(ctx, c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, order[0])
	})
	select {
	case <-inWindow:
	case <-time.After(10 * time.Second):
		t.Fatal("the first transfer never reached its parker window")
	}
	wg.Go(func() {
		race.landed[1], race.errs[1] = TransferDiskToParker(ctx, c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, order[1])
	})
	wg.Wait()

	if timedOut.Load() {
		t.Fatal("the second transfer never tried the per-disk lock while the first held it")
	}
	if releasedAt < 0 {
		t.Fatalf("the per-disk lock was never released; pool calls=%v", pools.calls)
	}
	c.mu.Lock()
	race.events = append([]string(nil), c.events...)
	c.mu.Unlock()
	for _, e := range race.events {
		if strings.HasPrefix(e, "move:") {
			race.moves++
		}
	}
	race.after = mutationEvents(race.events, releasedAt)
	return race
}

// TestTransferDiskToParker_AlreadyParkedUnderAnotherAllocation pins that a
// managed transfer never stamps its allocation over a record that already
// names a different allocation or namespace. It moves nothing, writes
// nothing, and refuses permanently with audit required, because two
// allocations claim the disk.
func TestTransferDiskToParker_AlreadyParkedUnderAnotherAllocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		id, space string
	}{
		{"another allocation", "alloc-2", "ns-1"},
		{"another namespace", "alloc-1", "ns-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := transferSourceAndParker()
			first, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg,
				ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID, AllocationID: "alloc-1", AllocationNamespace: "ns-1"})
			if err != nil {
				t.Fatalf("first transfer: %v", err)
			}
			before := c.parkedEntries(t)[transferStableID]
			mark := len(c.events)

			_, err = TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg,
				ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID, AllocationID: tc.id, AllocationNamespace: tc.space})
			var cpiErr *cpierrors.Error
			if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() || !strings.HasSuffix(err.Error(), "provenance conflicts; audit required") {
				t.Fatalf("a transfer under another allocation returned %v, want a permanent audit-required refusal", err)
			}
			if after := mutationEvents(c.events, mark); len(after) != 0 {
				t.Fatalf("the refused transfer changed state: %v", after)
			}
			if after := c.parkedEntries(t)[transferStableID]; !reflect.DeepEqual(after, before) || after.Volid != first {
				t.Fatalf("the record changed from %+v to %+v", before, after)
			}
		})
	}
}
