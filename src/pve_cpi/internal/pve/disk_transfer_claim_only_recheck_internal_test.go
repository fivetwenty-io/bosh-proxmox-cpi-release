package pve

// These rows cover what a claim-only resume checks under the parker's lock
// before it writes, which is the lock itself, the transfer record it reads
// there, and the volume it would claim.

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// claimOnlyLanded is the name claimOnlyIntent's volume took when its move
// landed it on parker 90000.
const claimOnlyLanded = "data:vm-90000-disk-3"

// claimOnlyParker is parker 90000's config with record as the disk's
// transfer record and with slots added.
func claimOnlyParker(t *testing.T, record DiskTransferIntent, slots map[string]any) map[string]any {
	t.Helper()
	entries, err := json.Marshal(map[string]parkerProvEntry{transferStableID: {
		DiskCID: "pvd-x", ParkedAt: "t", Node: record.ParkerNode, Volid: record.Volid, Slot: record.Slot, SourceVMCID: record.SourceVMCID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	desc, err := RenderSentinel("", map[string]json.RawMessage{"bosh_parked_disks": entries})
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{cfgKeyTags: "bosh-cpi;bosh-parker", "description": desc}
	maps.Copy(cfg, slots)
	return cfg
}

// requireRecordKept checks that the parker's transfer records are the ones
// the row started with.
func requireRecordKept(t *testing.T, c *scanFakeClient, before map[string]parkerProvEntry) {
	t.Helper()
	if after := c.parkedEntries(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("parker records after a claim-only resume = %+v, want %+v", after, before)
	}
}

// requireClaimOnlyStop checks that a claim-only resume stopped with the CPI
// class want and a message naming reason, and that the fake saw no write at
// all, so neither the parker's slots, its records, nor the source changed.
func requireClaimOnlyStop(t *testing.T, c *scanFakeClient, err error, want cpierrors.Type, reason string, slots map[string]string) {
	t.Helper()
	if err == nil || !cpierrors.IsType(err, want) || !strings.Contains(err.Error(), "claim-only") || !strings.Contains(err.Error(), reason) {
		t.Fatalf("resume error = %v, want the %s claim-only refusal naming %q", err, want, reason)
	}
	if len(c.events) != 0 {
		t.Fatalf("a claim-only resume wrote %v, want no write", c.events)
	}
	if disks := qemu.ParseDisks(c.configs[90000]); !maps.Equal(disks, slots) {
		t.Fatalf("parker slots after a claim-only resume = %v, want %v", disks, slots)
	}
}

// TestClaimOnlyResumeRereadsTheRecordUnderTheLock covers a transfer record
// that changed between the caller's read and the resume's read under the
// parker's lock, the way it does when another call finished or changed the
// transfer while this one waited for the lock. A claim-only resume compares
// the record it reads under the lock with the caller's intent and comes back
// retriable with no write, so it can't finalize a second time or claim on a
// record it no longer holds.
func TestClaimOnlyResumeRereadsTheRecordUnderTheLock(t *testing.T) {
	t.Parallel()
	finalized := claimOnlyIntent
	finalized.Volid = claimOnlyLanded
	otherSlot := claimOnlyIntent
	otherSlot.Slot = "scsi5"
	otherSource := claimOnlyIntent
	otherSource.SourceVMCID = "701"
	for _, tc := range []struct {
		name   string
		parker map[string]any
		slots  map[string]string
	}{
		{"finalized by another call", claimOnlyParker(t, finalized, map[string]any{"scsi4": claimOnlyLanded + ",serial=" + transferStableID}),
			map[string]string{"scsi4": claimOnlyLanded + ",serial=" + transferStableID}},
		{"retargeted to another slot", claimOnlyParker(t, otherSlot, map[string]any{"scsi4": claimOnlyLanded}),
			map[string]string{"scsi4": claimOnlyLanded}},
		{"recorded from another source", claimOnlyParker(t, otherSource, map[string]any{"scsi4": claimOnlyLanded}),
			map[string]string{"scsi4": claimOnlyLanded}},
		{"record gone", map[string]any{cfgKeyTags: "bosh-cpi;bosh-parker", "scsi4": claimOnlyLanded},
			map[string]string{"scsi4": claimOnlyLanded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newClaimOnlyClient(map[int]map[string]any{700: {}, 90000: tc.parker})
			before := c.parkedEntries(t)
			_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, claimOnlyIntent, transferStableID, transferTestCfg, ParkContext{ClaimOnly: true})
			requireClaimOnlyStop(t, c, err, cpierrors.TypeRetriableCloud, "moved underneath this call", tc.slots)
			requireRecordKept(t, c, before)
		})
	}
}

// TestClaimOnlyResumeLeavesAnUnrecordedLanding covers a recorded slot that
// holds a parker-named volume with no serial when the caller passes no move
// that landed it, such as another disk's landing after a second crash. A
// claim-only resume refuses for good and leaves that volume as it is, where a
// claim by the slot's shape alone would write this disk's serial onto it.
func TestClaimOnlyResumeLeavesAnUnrecordedLanding(t *testing.T) {
	t.Parallel()
	c := newClaimOnlyClient(map[int]map[string]any{
		700:   {},
		90000: claimOnlyParker(t, claimOnlyIntent, map[string]any{"scsi4": "data:vm-90000-disk-9"}),
	})
	before := c.parkedEntries(t)
	_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, claimOnlyIntent, transferStableID, transferTestCfg, ParkContext{ClaimOnly: true})
	requireClaimOnlyStop(t, c, err, cpierrors.TypeCloud, "no move the disk's record observed", map[string]string{"scsi4": "data:vm-90000-disk-9"})
	requireRecordKept(t, c, before)
}

// TestClaimOnlyResumeRefusesAnUnserializedLock covers a parker lock whose
// create PVE refused, as it does for an identity without Pool.Allocate. The
// protection window would then run unserialized, and a claim-only resume is
// safe to repeat only while the lock keeps a second resume out, so it comes
// back retriable with no write.
func TestClaimOnlyResumeRefusesAnUnserializedLock(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {},
		90000: claimOnlyParker(t, claimOnlyIntent, map[string]any{"scsi4": claimOnlyLanded}),
	})
	pools := newFakeLockPools()
	pools.createFn = func(string, string) error {
		return sdkerrors.ParseAPIError(403, []byte(`{"data":null,"message":"Permission check failed (/pool/bosh-lock-vm-90000, Pool.Allocate)\n"}`))
	}
	c.pools = pools
	before := c.parkedEntries(t)
	_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, claimOnlyIntent, transferStableID, transferTestCfg, ParkContext{ClaimOnly: true})
	requireClaimOnlyStop(t, c, err, cpierrors.TypeRetriableCloud, "refused the parker's lock", map[string]string{"scsi4": claimOnlyLanded})
	requireRecordKept(t, c, before)
	if pools.createN == 0 {
		t.Fatal("the resume never tried the parker's lock")
	}
}

// TestClaimOnlyResumeRefusesWithoutAPoolService covers a client with no pool
// service, which means nothing can grant the parker's lock. The
// protection window would then run unserialized, the same as when PVE
// refuses the lock, so a claim-only resume comes back retriable with no
// write instead of claiming the landed slot without the lock.
func TestClaimOnlyResumeRefusesWithoutAPoolService(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {},
		90000: claimOnlyParker(t, claimOnlyIntent, map[string]any{"scsi4": claimOnlyLanded}),
	})
	before := c.parkedEntries(t)
	pctx := ParkContext{ClaimOnly: true, ClaimLandings: []RecordedLanding{claimOnlyLanding}}
	_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, claimOnlyIntent, transferStableID, transferTestCfg, pctx)
	requireClaimOnlyStop(t, c, err, cpierrors.TypeRetriableCloud, "no pool service could take it", map[string]string{"scsi4": claimOnlyLanded})
	requireRecordKept(t, c, before)
	if refusal, ok := AsClaimOnlyRefusal(err); !ok || refusal.Window != ClaimOnlyUnserialized {
		t.Fatalf("resume error = %v, want the refusal tagged %s", err, ClaimOnlyUnserialized)
	}
}
