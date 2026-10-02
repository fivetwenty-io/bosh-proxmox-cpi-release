package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// pendingViewsClient serves one hand-built pending response, so a row can
// state exactly what PVE's pending endpoint sends.
type pendingViewsClient struct {
	parkerLockClient
	resp sdknodes.ListQemuPendingResponse
}

func (c *pendingViewsClient) Nodes() sdknodes.Service {
	return &fakeNodesService{
		listQemuPendingFn: func(context.Context, string, string) (*sdknodes.ListQemuPendingResponse, error) {
			out := append(sdknodes.ListQemuPendingResponse(nil), c.resp...)
			return &out, nil
		},
	}
}

// TestReadQemuViews_FourItemShapes builds the response config_with_pending_array
// gives, with every item shape it emits, and checks both views. A key with
// only its current value, a key with a pending value over its current one, a
// pending-only key, and a key with a delete flag, both on a current key and as
// a delete-only item and with the force flag 2. The applied view is the
// current value, overridden by any pending value, minus deleted keys, and the
// current view is the current values alone. A key the response leaves out is in
// neither view and never reads as a pending change.
func TestReadQemuViews_FourItemShapes(t *testing.T) {
	t.Parallel()
	c := &pendingViewsClient{resp: sdknodes.ListQemuPendingResponse{
		json.RawMessage(`{"key":"digest","value":"0123456789abcdef"}`),
		json.RawMessage(`{"key":"scsi0","value":"a:777/vm-777-disk-0.raw,size=5G"}`),
		json.RawMessage(`{"key":"cores","value":2,"pending":4}`),
		json.RawMessage(`{"key":"scsi2","pending":"a:777/vm-777-disk-2.raw,size=1G"}`),
		json.RawMessage(`{"key":"scsi1","value":"a:123/vm-123-disk-0.raw,serial=bpd-0011223344556677,size=5G","delete":1}`),
		json.RawMessage(`{"key":"virtio3","value":"a:124/vm-124-disk-0.raw","delete":2}`),
		json.RawMessage(`{"key":"net1","delete":1}`),
	}}
	views, err := ReadQemuViews(context.Background(), c, "n1", 777)
	if err != nil {
		t.Fatalf("ReadQemuViews: %v", err)
	}

	wantApplied := map[string]any{
		"digest": "0123456789abcdef",
		"scsi0":  "a:777/vm-777-disk-0.raw,size=5G",
		"cores":  "4",
		"scsi2":  "a:777/vm-777-disk-2.raw,size=1G",
	}
	if got := views.Applied(); !reflect.DeepEqual(got, wantApplied) {
		t.Errorf("applied view = %v, want %v", got, wantApplied)
	}
	wantCurrent := map[string]any{
		"digest":  "0123456789abcdef",
		"scsi0":   "a:777/vm-777-disk-0.raw,size=5G",
		"cores":   "2",
		"scsi1":   "a:123/vm-123-disk-0.raw,serial=bpd-0011223344556677,size=5G",
		"virtio3": "a:124/vm-124-disk-0.raw",
	}
	if got := views.Current(); !reflect.DeepEqual(got, wantCurrent) {
		t.Errorf("current view = %v, want %v", got, wantCurrent)
	}

	for key, want := range map[string]bool{
		"scsi1": true, "virtio3": true, "net1": true,
		"scsi0": false, "cores": false, "scsi2": false, "digest": false,
		// Absent from the response, so it is no pending change at all.
		"ide2": false,
	} {
		if got := views.PendingDelete(key); got != want {
			t.Errorf("PendingDelete(%q) = %v, want %v", key, got, want)
		}
	}

	for volid, want := range map[string][]string{
		"a:123/vm-123-disk-0.raw": {"scsi1"},
		"a:777/vm-777-disk-2.raw": {"scsi2"},
		"a:777/vm-777-disk-0.raw": {"scsi0"},
		"a:999/vm-999-disk-0.raw": nil,
	} {
		if got := views.SlotsNaming(volid); !reflect.DeepEqual(got, want) {
			t.Errorf("SlotsNaming(%q) = %v, want %v", volid, got, want)
		}
	}
	if slot, ok := views.BusSlotNaming("a:123/vm-123-disk-0.raw"); !ok || slot != "scsi1" {
		t.Errorf("BusSlotNaming of the pending-delete volume = %q, %v; want scsi1 counted as attached", slot, ok)
	}
}

// pendingDeleteWorld is one running VM 700 holding a disk on scsi1.
func pendingDeleteWorld(hotplug string) *scanFakeClient {
	cfg := map[string]any{"scsi1": "data:vm-9001-disk-0,size=10G"}
	if hotplug != "" {
		cfg["hotplug"] = hotplug
	}
	c := newScanFakeClient(map[int]map[string]any{700: cfg})
	c.running = map[int]bool{700: true}
	return c
}

func noBackoff() context.Context {
	return WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// TestDeleteDriveSlot_ReasonsAndRevert covers the helper's outcomes on a
// running VM. Each reason comes back as the typed error with no CPI class, so
// the caller chooses one, and every reverted delete leaves the slot attached in
// both views.
func TestDeleteDriveSlot_ReasonsAndRevert(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-9001-disk-0"

	t.Run("a stopped VM applies the delete", func(t *testing.T) {
		t.Parallel()
		c := pendingDeleteWorld("network,usb")
		c.running = nil
		if err := DeleteDriveSlot(noBackoff(), c, nil, "pve1", 700, "scsi1", volid, nil, 3); err != nil {
			t.Fatalf("DeleteDriveSlot on a stopped VM: %v", err)
		}
		if _, present := c.configs[700]["scsi1"]; present || len(c.held[700]) != 0 {
			t.Fatalf("slot after delete = %v, held = %v; want it gone and nothing pending", c.configs[700], c.held[700])
		}
	})

	cases := []deleteDriveSlotCase{
		{name: "hotplug lacks disk", hotplug: "network,usb", want: DriveDeletePendingHotplug, wantHotplug: "network,usb", wantReverted: true},
		{name: "hotplug disabled", hotplug: "0", want: DriveDeletePendingHotplug, wantHotplug: "0", wantReverted: true},
		{name: "busy guest", busy: true, want: DriveDeletePendingBusy, wantReverted: true},
		{name: "revert fails", hotplug: "network", revertErr: errors.New("revert refused"), want: DriveDeletePendingRevertUnconfirmed},
		{name: "revert leaves the delete pending", hotplug: "network", keepsPending: true, want: DriveDeletePendingRevertUnconfirmed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkDeleteDriveSlotCase(t, tc, volid)
		})
	}
}

// deleteDriveSlotCase is one running-VM outcome of DeleteDriveSlot.
type deleteDriveSlotCase struct {
	name         string
	hotplug      string
	busy         bool
	revertErr    error
	keepsPending bool
	want         DriveDeletePendingReason
	wantHotplug  string
	wantReverted bool
}

// checkDeleteDriveSlotCase runs one case on a running VM 700 and checks the
// typed error, the single revert, and where the slot ends up.
func checkDeleteDriveSlotCase(t *testing.T, tc deleteDriveSlotCase, volid string) {
	t.Helper()
	c := pendingDeleteWorld(tc.hotplug)
	c.busy = map[int]bool{700: tc.busy}
	c.revertErr = tc.revertErr
	c.revertKeepsPending = tc.keepsPending
	err := DeleteDriveSlot(noBackoff(), c, nil, "pve1", 700, "scsi1", volid, nil, 3)
	pending, ok := IsDriveDeletePending(err)
	if !ok {
		t.Fatalf("DeleteDriveSlot = %v, want a *DriveDeletePendingError", err)
	}
	if pending.Reason != tc.want || pending.VMID != 700 || pending.Node != "pve1" || pending.Slot != "scsi1" || pending.Hotplug != tc.wantHotplug {
		t.Fatalf("pending error = %+v, want reason %q on VM 700 node pve1 slot scsi1 hotplug %q", pending, tc.want, tc.wantHotplug)
	}
	var typed *cpierrors.Error
	if errors.As(err, &typed) {
		t.Fatalf("the helper chose class %s; the caller chooses it", typed.Type())
	}
	if tc.revertErr != nil && !strings.Contains(err.Error(), "revert refused") {
		t.Errorf("error %q doesn't carry the revert's failure", err)
	}
	if reverts := countEvents(c, "revert:700:scsi1"); reverts != 1 {
		t.Errorf("reverts = %d, want exactly 1; events %v", reverts, c.events)
	}
	_, attached := c.configs[700]["scsi1"]
	_, held := c.held[700]["scsi1"]
	if tc.wantReverted && (!attached || held) {
		t.Fatalf("after the revert scsi1 attached=%v pending=%v; want it attached in both views", attached, held)
	}
	if !tc.wantReverted && (attached || !held) {
		t.Fatalf("after an unconfirmed revert scsi1 attached=%v pending=%v; want the pending delete still reported", attached, held)
	}
	if tc.busy {
		if deletes := countEvents(c, "pending-delete:700:scsi1"); deletes != 3 {
			t.Errorf("busy deletes sent = %d, want the 3 the budget allows", deletes)
		}
	}
}

// countEvents counts the fake's events that start with prefix.
func countEvents(c *scanFakeClient, prefix string) int {
	n := 0
	for _, event := range c.events {
		if strings.HasPrefix(event, prefix) {
			n++
		}
	}
	return n
}

// TestTransferDiskToParker_PendingDeleteReachesTheCaller covers the transfer's
// slot delete on a running source whose hotplug setting lacks disk. The delete
// is reverted, nothing lands on the parker, and the typed error comes out of
// TransferDiskToParker where errors.As finds it, so each caller chooses its
// class.
func TestTransferDiskToParker_PendingDeleteReachesTheCaller(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-700-disk-1"
	c := newScanFakeClient(map[int]map[string]any{
		700: {"scsi1": volid + ",serial=" + transferStableID + ",size=10G", "hotplug": "network,usb"},
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
		},
	})
	c.running = map[int]bool{700: true}
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	_, err := TransferDiskToParker(noBackoff(), c, nil, "pve1", 700, volid, transferTestCfg, pctx)
	pending, ok := IsDriveDeletePending(err)
	if !ok {
		t.Fatalf("TransferDiskToParker = %v, want the typed pending-delete error through its wrapping", err)
	}
	if pending.Reason != DriveDeletePendingHotplug || pending.VMID != 700 || pending.Slot != "scsi1" {
		t.Fatalf("pending error = %+v", pending)
	}
	if value, _ := c.configs[700]["scsi1"].(string); !strings.HasPrefix(value, volid+",") || len(c.held[700]) != 0 {
		t.Fatalf("source after the transfer = %v, held %v; want scsi1 back with nothing pending", c.configs[700], c.held[700])
	}
	if disks := qemu.ParseDisks(c.configs[90000]); len(disks) != 0 {
		t.Fatalf("parker slots after a pending delete = %v, want none", disks)
	}
	if c.eventIndex("move:") >= 0 || c.eventIndex("attach:90000") >= 0 {
		t.Fatalf("something moved or attached to the parker: %v", c.events)
	}
}

// TestResumeDiskTransferToParker_PendingDeleteAttachesNothing covers an intent
// record an earlier attempt left while the source's delete is still pending.
// The resume reads the source in both views, finds the slot still attached,
// and attaches nothing to the parker in either the move window or the
// released-source window. It leaves the pending delete alone, with no revert,
// and returns the typed error with the found-and-left-alone reason, so the
// caller chooses the class.
func TestResumeDiskTransferToParker_PendingDeleteAttachesNothing(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-9001-disk-0"
	c := newScanFakeClient(map[int]map[string]any{
		700: {"hotplug": "network,usb"},
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
		},
	})
	c.held = map[int]map[string]any{700: {"scsi1": volid + ",serial=" + transferStableID + ",size=10G"}}
	intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi0", Volid: volid, SourceVMCID: "700"}
	_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{DiskCID: "pvd-test"})
	pending, ok := IsDriveDeletePending(err)
	if !ok {
		t.Fatalf("resume error = %v, want the typed pending-delete error", err)
	}
	if pending.Reason != DriveDeletePendingFound || pending.Node != "pve1" || pending.VMID != 700 || pending.Slot != "scsi1" || pending.Cause != nil {
		t.Fatalf("pending error = %+v, want the found reason on VM 700 node pve1 slot scsi1 with no cause", pending)
	}
	var typed *cpierrors.Error
	if errors.As(err, &typed) {
		t.Fatalf("the resume chose class %s; the caller chooses it", typed.Type())
	}
	if c.eventIndex("revert:") >= 0 {
		t.Fatalf("the resume reverted a pending delete it didn't send: %v", c.events)
	}
	if _, held := c.held[700]["scsi1"]; !held {
		t.Fatal("the resume cleared the pending delete it found")
	}
	if disks := qemu.ParseDisks(c.configs[90000]); len(disks) != 0 {
		t.Fatalf("parker slots after the resume = %v, want none", disks)
	}
}

// TestFindVolumeReferences_PendingDeleteIsAReference covers a slot whose delete
// is pending, which the config endpoint hides while the running guest still
// has the disk. It still counts as a reference.
func TestFindVolumeReferences_PendingDeleteIsAReference(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-9001-disk-0"
	c := pendingDeleteWorld("network,usb")
	if err := DeleteDriveSlot(noBackoff(), c, nil, "pve1", 700, "scsi1", volid, nil, 1); err == nil {
		t.Fatal("setup: the delete applied")
	}
	// The helper reverted its own delete, so leave one pending the way a crash
	// before the revert would.
	c.mu.Lock()
	if held, err := c.holdDeleteLocked(c.configs[700], 700, "scsi1"); !held || err != nil {
		c.mu.Unlock()
		t.Fatalf("setup: hold the delete: held=%v err=%v", held, err)
	}
	c.mu.Unlock()
	refs, err := FindVolumeReferences(context.Background(), c, volid)
	if err != nil {
		t.Fatalf("FindVolumeReferences: %v", err)
	}
	want := []VolumeReference{{VMID: 700, Node: "pve1", Slot: "scsi1"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("references = %+v, want %+v", refs, want)
	}
}

// TestKeepRule_PendingDeleteOnTheSourceKeepsTheRecord covers a stale transfer
// record whose source VM has the volume's slot delete pending. The config
// endpoint no longer shows the slot, but the running guest still has the disk,
// so the record stays.
func TestKeepRule_PendingDeleteOnTheSourceKeepsTheRecord(t *testing.T) {
	t.Parallel()
	w := newKeepWorld()
	w.withParker(t, map[string]parkerProvEntry{keepKey: strandedIntent(2 * time.Hour)})
	w.configs["n1"][keepSourceVMID] = map[string]any{"hotplug": "network"}
	w.pending = map[string]map[int]map[string]any{
		"n1": {keepSourceVMID: {"scsi1": keepStranded + ",serial=" + keepKey + ",size=5G"}},
	}
	if err := w.writeFresh(t); err != nil {
		t.Fatal(err)
	}
	if !w.survived(t, keepKey) {
		t.Fatalf("the transfer record was collected although VM %d's delete of the slot naming %s is still pending", keepSourceVMID, keepStranded)
	}
}

// TestRetryLoops_StopOnAPendingDeleteError covers every retry loop in
// retry.go. A *DriveDeletePendingError is a verdict on a slot delete whose
// busy retries and revert have already run, and its text carries the cause's
// text, which the classifiers read. So each loop returns it at once, before
// any classifier runs, even when its cause carries the busy text or the
// lock-timeout text a classifier would retry.
func TestRetryLoops_StopOnAPendingDeleteError(t *testing.T) {
	t.Parallel()
	loops := map[string]func(ctx context.Context, op func() error) error{
		"RetryOnTransient": func(ctx context.Context, op func() error) error {
			return RetryOnTransient(ctx, nil, "row", 5, op)
		},
		"RetryOnTransientOrLock": func(ctx context.Context, op func() error) error {
			return RetryOnTransientOrLock(ctx, nil, "row", 5, op)
		},
		"RetryOnTransientOrUnplugBusy": func(ctx context.Context, op func() error) error {
			return RetryOnTransientOrUnplugBusy(ctx, nil, "row", 5, op)
		},
	}
	causes := map[string]error{
		"busy cause": errors.New("API request failed: parameter error: Parameter verification failed. (code: 0, errors: " +
			"scsi1: hotplug problem - error on hot-unplugging device 'virtioscsi1' - still busy in guest?)"),
		"lock-timeout cause":    errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout"),
		"worker pushback cause": errors.New("got timeout"),
	}
	for loopName, loop := range loops {
		for causeName, cause := range causes {
			t.Run(loopName+" "+causeName, func(t *testing.T) {
				t.Parallel()
				pending := &DriveDeletePendingError{Reason: DriveDeletePendingBusy, Node: "pve1", VMID: 700, Slot: "scsi1", Cause: cause}
				// The classifiers would retry the cause on its own, which is
				// what makes the stop the loop's and not the classifiers'.
				if !IsHotUnplugBusy(cause) && !IsStorageLockTimeout(cause) && !IsPVEPushback(cause) {
					t.Fatalf("setup: no classifier retries %q", cause)
				}
				for _, wrapped := range []error{pending, cpierrors.Wrap(pending, "transfer in: detach")} {
					calls := 0
					err := loop(noBackoff(), func() error {
						calls++
						return wrapped
					})
					if calls != 1 {
						t.Fatalf("op ran %d times, want exactly once", calls)
					}
					if got, ok := IsDriveDeletePending(err); !ok || got != pending {
						t.Fatalf("loop returned %v, want the pending-delete error back", err)
					}
				}
			})
		}
	}
}

// TestDeleteDriveSlot_RefusesAReplacedKey covers a slot whose pending value
// names a different volume than its current drive, which an attach onto a
// slot whose delete was pending could leave on a running VM. A delete there
// would either re-arm the old drive's pending delete or drop the pending
// value's reference, so the helper reads the pending view first, sends no
// delete and no revert, and returns the replaced reason with no CPI class.
func TestDeleteDriveSlot_RefusesAReplacedKey(t *testing.T) {
	t.Parallel()
	const current, replacement = "data:vm-9001-disk-0", "data:vm-9002-disk-0"
	for name, volid := range map[string]string{"the current drive": current, "the pending value": replacement} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := pendingDeleteWorld("network,usb")
			c.configs[700]["scsi1"] = replacement + ",size=10G"
			c.replaced = map[int]map[string]any{700: {"scsi1": current + ",size=10G"}}
			err := DeleteDriveSlot(noBackoff(), c, nil, "pve1", 700, "scsi1", volid, nil, 3)
			pending, ok := IsDriveDeletePending(err)
			if !ok || pending.Reason != DriveDeletePendingReplaced || pending.VMID != 700 || pending.Node != "pve1" || pending.Slot != "scsi1" || pending.Cause != nil {
				t.Fatalf("DeleteDriveSlot = %v, want the replaced reason on VM 700 node pve1 slot scsi1 with no cause", err)
			}
			var typed *cpierrors.Error
			if errors.As(err, &typed) {
				t.Fatalf("the helper chose class %s; the caller chooses it", typed.Type())
			}
			if len(c.events) != 0 {
				t.Fatalf("events = %v, want no delete and no revert sent", c.events)
			}
			if value, _ := c.configs[700]["scsi1"].(string); value != replacement+",size=10G" || c.replaced[700]["scsi1"] != current+",size=10G" {
				t.Fatalf("scsi1 = %q over %v, want both values left as they were", value, c.replaced[700]["scsi1"])
			}
		})
	}
}

// TestFindVMByDiskVolid_ReplacedKeyHoldsBothVolumes covers the same slot seen
// by the identity scan. The config endpoint shows the pending value there,
// while the running guest still has the current drive. A scan for either
// volume returns the VM and the slot, and the scan for the current drive says
// the slot carries a replacement, which is how delete_disk knows to refuse it.
func TestFindVMByDiskVolid_ReplacedKeyHoldsBothVolumes(t *testing.T) {
	t.Parallel()
	const current, replacement = "data:vm-9001-disk-0", "data:vm-9002-disk-0"
	c := pendingDeleteWorld("")
	c.configs[700]["scsi1"] = replacement + ",size=10G"
	c.replaced = map[int]map[string]any{700: {"scsi1": current + ",size=10G"}}
	for volid, want := range map[string]PendingChange{current: PendingChangeReplaced, replacement: PendingChangeNone} {
		hit, found, err := findVMByDiskVolidHit(context.Background(), c, volid)
		if err != nil || !found {
			t.Fatalf("scan for %s: found=%v err=%v, want VM 700", volid, found, err)
		}
		if hit.VMID != 700 || hit.Node != "pve1" || hit.Slot != "scsi1" || hit.Volid != volid || hit.PendingChange != want {
			t.Fatalf("scan for %s = %+v, want VM 700 node pve1 slot scsi1 with pending change %q", volid, hit, want)
		}
	}
}

// replacedTransferWorld is VM 700 running with scsi1 keeping current as its
// drive while a pending value names replacement, and an empty parker 90000.
func replacedTransferWorld(current, replacement string) *scanFakeClient {
	c := newScanFakeClient(map[int]map[string]any{
		700: {"scsi1": replacement + ",size=10G"},
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
		},
	})
	c.running = map[int]bool{700: true}
	c.replaced = map[int]map[string]any{700: {"scsi1": current + ",serial=" + transferStableID + ",size=10G"}}
	return c
}

// TestTransferDiskToParker_RefusesAReplacedSlotBeforeWriting covers a source
// slot whose pending value names a different volume than its current drive.
// The transfer can't detach the volume there until PVE applies the change, so
// it refuses with the replaced reason before it creates a parker or writes an
// intent record, whichever side of the replacement the moved volume is on.
// The current side is the drive the running guest has, and the pending side is
// the volume an attach onto a slot whose delete was pending can leave there.
func TestTransferDiskToParker_RefusesAReplacedSlotBeforeWriting(t *testing.T) {
	t.Parallel()
	const current, replacement = "data:vm-9001-disk-0", "data:vm-9002-disk-0"
	for name, volid := range map[string]string{"current side": current, "pending side": replacement} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := replacedTransferWorld(current, replacement)
			pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
			_, err := TransferDiskToParker(noBackoff(), c, nil, "pve1", 700, volid, transferTestCfg, pctx)
			pending, ok := IsDriveDeletePending(err)
			if !ok || pending.Reason != DriveDeletePendingReplaced || pending.VMID != 700 || pending.Slot != "scsi1" {
				t.Fatalf("TransferDiskToParker = %v, want the replaced reason on VM 700 slot scsi1", err)
			}
			if len(c.events) != 0 {
				t.Fatalf("events = %v, want no parker write, intent record, delete, or move", c.events)
			}
			if len(c.configs) != 2 {
				t.Fatalf("guests after the refusal = %d, want no parker created", len(c.configs))
			}
			if desc, _ := c.configs[90000]["description"].(string); desc != "" {
				t.Fatalf("parker 90000 description = %q, want no intent record", desc)
			}
		})
	}
}

// TestTransferDiskToParker_PreCheckReadFailureLeavesTheWindowToDecide covers
// a pending read that fails before the transfer opens its parker window. The
// pre-check leaves the decision to the source read inside the window, as the
// transfer made it before the pre-check existed. On a replaced slot that read
// leads to the slot delete's own refusal, after the intent record, with no
// delete sent. On an ordinary slot the transfer parks the disk.
func TestTransferDiskToParker_PreCheckReadFailureLeavesTheWindowToDecide(t *testing.T) {
	t.Parallel()
	const current, replacement = "data:vm-9001-disk-0", "data:vm-9002-disk-0"
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	readErr := errors.New("pending read failed once")

	t.Run("replaced slot", func(t *testing.T) {
		t.Parallel()
		c := replacedTransferWorld(current, replacement)
		c.pendingErrOnce = map[int]error{700: readErr}
		_, err := TransferDiskToParker(noBackoff(), c, nil, "pve1", 700, current, transferTestCfg, pctx)
		pending, ok := IsDriveDeletePending(err)
		if !ok || pending.Reason != DriveDeletePendingReplaced {
			t.Fatalf("TransferDiskToParker = %v, want the slot delete's replaced refusal", err)
		}
		if c.eventIndex("description:90000") < 0 {
			t.Fatalf("events = %v, want the intent record written inside the window", c.events)
		}
		if c.eventIndex("pending-delete:") >= 0 || c.eventIndex("config-delete:") >= 0 || c.eventIndex("move:") >= 0 {
			t.Fatalf("events = %v, want no delete or move sent", c.events)
		}
	})

	t.Run("ordinary slot", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
			90000: {
				cfgKeyTags:      "bosh-cpi;bosh-parker",
				paramProtection: true,
			},
		})
		c.pendingErrOnce = map[int]error{700: readErr}
		landed, err := TransferDiskToParker(noBackoff(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
		if err != nil {
			t.Fatalf("TransferDiskToParker after a failed pre-check read: %v", err)
		}
		if !strings.HasPrefix(landed, "data:vm-90000-disk-") {
			t.Fatalf("landed volid = %q, want a parker-named volume", landed)
		}
	})
}

// TestDetachDriveSlot_SweepsOnlyTheUnusedEntryPVEKeeps covers the second step of
// the detach. PVE keeps an unused entry only for a volume the VM owns by name,
// so the sweep removes that entry with one more config delete. A volume named
// for another VM leaves no entry, and the sweep then has nothing to remove and
// sends no second delete.
func TestDetachDriveSlot_SweepsOnlyTheUnusedEntryPVEKeeps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, volid string
		wantEvents  []string
	}{
		{"owned volume", "data:vm-700-disk-0", []string{"config-delete:700:scsi1:data:vm-700-disk-0", "config-delete:700:unused0:data:vm-700-disk-0"}},
		{"another VM's volume", "data:vm-9001-disk-0", []string{"config-delete:700:scsi1:data:vm-9001-disk-0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newScanFakeClient(map[int]map[string]any{700: {"scsi1": tc.volid + ",size=10G"}})
			if err := DetachDriveSlot(noBackoff(), c, nil, "pve1", 700, "scsi1", tc.volid, 3); err != nil {
				t.Fatalf("DetachDriveSlot: %v", err)
			}
			for key, value := range c.configs[700] {
				if strings.Split(fmt.Sprint(value), ",")[0] == tc.volid {
					t.Fatalf("VM 700 still names %s on %s", tc.volid, key)
				}
			}
			var deletes []string
			for _, event := range c.events {
				if strings.HasPrefix(event, "config-delete:") {
					deletes = append(deletes, event)
				}
			}
			if !reflect.DeepEqual(deletes, tc.wantEvents) {
				t.Fatalf("config deletes = %v, want %v", deletes, tc.wantEvents)
			}
		})
	}
}
