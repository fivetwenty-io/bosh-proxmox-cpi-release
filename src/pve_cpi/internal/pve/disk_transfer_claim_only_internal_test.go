package pve

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// claimOnlyIntent is a transfer record from source VM 700 to parker 90000
// whose recorded volume still carries its source name.
var claimOnlyIntent = DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-700-disk-1", SourceVMCID: "700"}

// claimOnlyLanding is the move the disk's allocation record observed when it
// landed claimOnlyIntent's volume on the parker as data:vm-90000-disk-3.
var claimOnlyLanding = RecordedLanding{SourceVMID: 700, From: claimOnlyIntent.Volid, To: claimOnlyLanded}

// newClaimOnlyClient is the scan fake with a pool service that grants the
// parker's lock. A claim-only resume needs that lock before it reads or
// writes anything under it.
func newClaimOnlyClient(configs map[int]map[string]any) *scanFakeClient {
	c := newScanFakeClient(configs)
	c.pools = newFakeLockPools()
	return c
}

// requireClaimOnlyRefusal checks that a claim-only resume stopped at window
// with the CPI class want, and that the fake saw no write at all, so neither
// the parker's slots, its records, nor the source changed.
func requireClaimOnlyRefusal(t *testing.T, c *scanFakeClient, err error, window ClaimOnlyWindow, want cpierrors.Type, slots map[string]string) {
	t.Helper()
	refusal, ok := AsClaimOnlyRefusal(err)
	if !ok || refusal.Window != window || !cpierrors.IsType(err, want) || !strings.Contains(err.Error(), "claim-only") {
		t.Fatalf("resume error = %v, want the %s claim-only refusal at %s", err, want, window)
	}
	if len(c.events) != 0 {
		t.Fatalf("a claim-only resume wrote %v, want no write", c.events)
	}
	if disks := qemu.ParseDisks(c.configs[90000]); !maps.Equal(disks, slots) {
		t.Fatalf("parker slots after a claim-only resume = %v, want %v", disks, slots)
	}
}

// stoppedSourceClient is a scanFakeClient whose status read reports every VM
// stopped, which is what the full resume needs before it applies a pending
// delete it found.
type stoppedSourceClient struct{ *scanFakeClient }

func (c stoppedSourceClient) QEMU() qemu.Service {
	svc, _ := c.scanFakeClient.QEMU().(*fakeQEMUService)
	svc.statusFn = func(context.Context, string, int) (map[string]any, error) {
		return map[string]any{"status": "stopped"}, nil
	}
	return svc
}

// TestClaimOnlyResumeRefusesTheMoveWindow covers a source that still names the
// recorded volume on an unused entry. The full resume moves it, but a
// claim-only resume returns retriable before the move and leaves the answer
// to its caller. The identity check resolves the disk once more, goes on when
// storage lists the old name again, and refuses for good when it doesn't.
func TestClaimOnlyResumeRefusesTheMoveWindow(t *testing.T) {
	t.Parallel()
	c := newClaimOnlyClient(map[int]map[string]any{
		700:   {"unused0": claimOnlyIntent.Volid},
		90000: claimOnlyParker(t, claimOnlyIntent, nil),
	})
	before := c.parkedEntries(t)
	_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, claimOnlyIntent, transferStableID, transferTestCfg, ParkContext{ClaimOnly: true})
	requireClaimOnlyRefusal(t, c, err, ClaimOnlyMove, cpierrors.TypeRetriableCloud, map[string]string{})
	requireRecordKept(t, c, before)
	if got := FindUnusedDiskEntries(c.configs[700]); got["unused0"] != claimOnlyIntent.Volid {
		t.Fatalf("source unused entries = %v, want the volume left on unused0", got)
	}
}

// TestClaimOnlyResumeRefusesThePendingDeleteApply covers a stopped source whose
// delete of the volume's slot is pending. The full resume applies that delete
// when its caller asks, but a claim-only resume refuses for good first, even
// with the apply asked for, because no later call gets past the identity
// check to apply it.
func TestClaimOnlyResumeRefusesThePendingDeleteApply(t *testing.T) {
	t.Parallel()
	c := newClaimOnlyClient(map[int]map[string]any{
		700:   {},
		90000: claimOnlyParker(t, claimOnlyIntent, nil),
	})
	c.held = map[int]map[string]any{700: {"scsi1": claimOnlyIntent.Volid + ",serial=" + transferStableID + ",size=10G"}}
	before := c.parkedEntries(t)
	pctx := ParkContext{ApplyFoundPendingDelete: true, ClaimOnly: true}
	_, err := ResumeDiskTransferToParker(noBackoff(), stoppedSourceClient{c}, nil, claimOnlyIntent, transferStableID, transferTestCfg, pctx)
	requireClaimOnlyRefusal(t, c, err, ClaimOnlyPendingDelete, cpierrors.TypeCloud, map[string]string{})
	requireRecordKept(t, c, before)
	if _, held := c.held[700]["scsi1"]; !held {
		t.Fatal("a claim-only resume applied the pending delete it found")
	}
}

// TestClaimOnlyResumeRefusesTheConfigEditAttach covers a source that released
// the volume without a move. The full resume attaches the recorded volume to
// the parker by config edit, but a claim-only resume refuses for good first,
// because no later call gets past the identity check to attach it.
func TestClaimOnlyResumeRefusesTheConfigEditAttach(t *testing.T) {
	t.Parallel()
	intent := claimOnlyIntent
	intent.Volid = "data:vm-11949-disk-0"
	c := newClaimOnlyClient(map[int]map[string]any{
		700:   {},
		90000: claimOnlyParker(t, intent, nil),
	})
	before := c.parkedEntries(t)
	_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{ClaimOnly: true})
	requireClaimOnlyRefusal(t, c, err, ClaimOnlyConfigEdit, cpierrors.TypeCloud, map[string]string{})
	requireRecordKept(t, c, before)
}

// TestClaimOnlyResumeStillClaimsAndFinalizes shows the two windows a claim-only
// resume still runs. A slot that already carries the serial only needs the
// finalize, and the recorded slot gets the serial and the finalize when the
// disk's record observed the move that landed its volume.
func TestClaimOnlyResumeStillClaimsAndFinalizes(t *testing.T) {
	t.Parallel()

	t.Run("finalize only", func(t *testing.T) {
		t.Parallel()
		c := newClaimOnlyClient(map[int]map[string]any{
			90000: claimOnlyParker(t, claimOnlyIntent, map[string]any{"scsi4": "data:vm-90000-disk-5,serial=" + transferStableID}),
		})
		landed, err := ResumeDiskTransferToParker(noBackoff(), c, nil, claimOnlyIntent, transferStableID, transferTestCfg, ParkContext{ClaimOnly: true})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if entry := c.parkedEntries(t)[transferStableID]; landed != "data:vm-90000-disk-5" || entry.Volid != landed {
			t.Fatalf("landed %q with record %+v, want the finalize on the landed name", landed, entry)
		}
	})

	t.Run("claim", func(t *testing.T) {
		t.Parallel()
		c := newClaimOnlyClient(map[int]map[string]any{
			700:   {},
			90000: claimOnlyParker(t, claimOnlyIntent, map[string]any{"scsi4": claimOnlyLanding.To}),
		})
		pctx := ParkContext{ClaimOnly: true, ClaimLandings: []RecordedLanding{claimOnlyLanding}}
		landed, err := ResumeDiskTransferToParker(noBackoff(), c, nil, claimOnlyIntent, transferStableID, transferTestCfg, pctx)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if got, _ := c.configs[90000]["scsi4"].(string); landed != claimOnlyLanding.To || !strings.Contains(got, "serial="+transferStableID) {
			t.Fatalf("landed %q with slot %q, want the serial on the claimed slot", landed, got)
		}
		if entry := c.parkedEntries(t)[transferStableID]; entry.Volid != landed {
			t.Fatalf("finalized record = %+v, want the landed name", entry)
		}
		if c.eventIndex("move:") >= 0 {
			t.Fatalf("a claim moved a volume: %v", c.events)
		}
	})
}

// TestClaimOnlyResumeClaimsOnlyARecordedLanding covers a recorded slot that
// holds a parker-named volume with no serial when the moves the caller passes
// don't match it. A move that landed another name, a move of another volume,
// and a move off another VM each prove a different landing, so a claim-only
// resume refuses for good and leaves the slot's volume as it is.
func TestClaimOnlyResumeClaimsOnlyARecordedLanding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		landings []RecordedLanding
	}{
		{"another disk's landing on the slot", []RecordedLanding{{SourceVMID: 700, From: claimOnlyIntent.Volid, To: "data:vm-90000-disk-9"}}},
		{"a move of another volume", []RecordedLanding{{SourceVMID: 700, From: "data:vm-700-disk-2", To: claimOnlyLanding.To}}},
		{"a move off another VM", []RecordedLanding{{SourceVMID: 701, From: claimOnlyIntent.Volid, To: claimOnlyLanding.To}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newClaimOnlyClient(map[int]map[string]any{
				700:   {},
				90000: claimOnlyParker(t, claimOnlyIntent, map[string]any{"scsi4": claimOnlyLanding.To}),
			})
			before := c.parkedEntries(t)
			pctx := ParkContext{ClaimOnly: true, ClaimLandings: tc.landings}
			_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, claimOnlyIntent, transferStableID, transferTestCfg, pctx)
			requireClaimOnlyRefusal(t, c, err, ClaimOnlyUnprovenLanding, cpierrors.TypeCloud, map[string]string{"scsi4": claimOnlyLanding.To})
			requireRecordKept(t, c, before)
		})
	}
}

// TestClaimOnlyResumeTagsAStateNoWindowConverges covers a transfer record that
// no window of the resume converges. A claim-only resume returns the same
// permanent error the full resume does, tagged so the identity check can
// resolve the disk once more before it gives that answer.
func TestClaimOnlyResumeTagsAStateNoWindowConverges(t *testing.T) {
	t.Parallel()
	intent := claimOnlyIntent
	intent.SourceVMCID = ""
	c := newClaimOnlyClient(map[int]map[string]any{90000: claimOnlyParker(t, intent, nil)})
	before := c.parkedEntries(t)
	_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{ClaimOnly: true})
	refusal, ok := AsClaimOnlyRefusal(err)
	if !ok || refusal.Window != ClaimOnlyNoWindow || !cpierrors.IsType(err, cpierrors.TypeCloud) || !strings.Contains(err.Error(), "intent record") {
		t.Fatalf("resume error = %v, want the permanent unconvergeable-state error tagged %s", err, ClaimOnlyNoWindow)
	}
	if len(c.events) != 0 {
		t.Fatalf("a claim-only resume wrote %v, want no write", c.events)
	}
	requireRecordKept(t, c, before)
}
