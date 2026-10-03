package pve

// These rows cover what a resume meets beyond a reused name. Two disks'
// unfinished transfers can share a parker, a source can migrate or be read
// stale between the proof and the move, and a stopped source can hold a
// pending delete the resume would apply.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// resumeSlotsOtherID is disk C's serial, a disk whose own transfer shares the
// parker with the resumed disk.
const resumeSlotsOtherID = "bpd-c0c0c0c0c0c0c0c0"

// parkerRecords renders a parker description that keeps the given records.
func parkerRecords(t *testing.T, records map[string]parkerProvEntry) string {
	t.Helper()
	desc, err := renderParkerSentinel("", records, nil)
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

// transferRecord is one unfinished transfer record on parker 90000.
func transferRecord(volid, slot, source string) parkerProvEntry {
	return parkerProvEntry{DiskCID: "pvd-x", ParkedAt: "t", Node: "pve1", Volid: volid, Slot: slot, SourceVMCID: source}
}

// fullLowSlots fills scsi0 through scsi3 of a parker with parked disks that
// carry their own serials, so scsi4 is the lowest free slot.
func fullLowSlots(cfg map[string]any) map[string]any {
	for i := range 4 {
		cfg[fmt.Sprintf("scsi%d", i)] = fmt.Sprintf("data:vm-90000-disk-%d,serial=bpd-00000000000000%02d", 20+i, i)
	}
	return cfg
}

// failSerialClient fails every write of one disk's serial onto a parker slot,
// which stands in for a crash between that disk's move and its serial write.
type failSerialClient struct {
	*scanFakeClient
	serial string
}

func (c *failSerialClient) QEMU() qemu.Service {
	inner := c.scanFakeClient.QEMU().(*fakeQEMUService)
	attach := inner.attachDiskFn
	inner.attachDiskFn = func(ctx context.Context, node string, vmid int, volid, bus string, opts *qemu.AttachOpts) (string, error) {
		if strings.Contains(volid, "serial="+c.serial) {
			return "", errors.New("the CPI process died before the serial write")
		}
		return attach(ctx, node, vmid, volid, bus, opts)
	}
	return inner
}

// serialSlots returns every disk key of parker 90000 that carries the
// transfer's serial, with its value.
func serialSlots(c *scanFakeClient) map[string]string {
	out := map[string]string{}
	for key, value := range c.configs[90000] {
		text, _ := value.(string)
		if found, has := StableIDFromDriveOptStr(text); has && found == transferStableID {
			out[key] = text
		}
	}
	return out
}

// TestResumeSlots_TwoUnfinishedTransfersOnOneParker is the sequence that put
// two disks on one slot. Disk A's record names scsi4, the lowest free slot of
// parker 90000, and A's volume still sits on 700's unused0. Disk C's transfer
// from 701 then picks a slot, lands, and dies before its serial write. Before
// the slot exclusion C took scsi4 as well. The proof then claimed C's landing
// as A's and wrote A's serial onto it, while A's own volume stayed on 700. Now
// C lands elsewhere. C's source names C's volume nowhere and storage no longer
// holds it, so the resume proves C's move ran, and A's resume moves A's own
// volume onto scsi4 and leaves C's landing alone.
func TestResumeSlots_TwoUnfinishedTransfersOnOneParker(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		701: {"scsi1": "data:vm-701-disk-0,serial=" + resumeSlotsOtherID + ",size=10G"},
		90000: fullLowSlots(map[string]any{
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID: transferRecord("data:vm-700-disk-1", "scsi4", "700"),
			}),
		}),
	})
	inner.renameCounter[90000] = 5
	c := &failSerialClient{scanFakeClient: inner, serial: resumeSlotsOtherID}

	_, err := transferIntoParker(noBackoff(), c, nil, "pve1", 90000, 701, "data:vm-701-disk-0", transferTestCfg,
		ParkContext{StableID: resumeSlotsOtherID, DiskCID: "pvd-c", SourceVMCID: "701"})
	if err == nil {
		t.Fatal("C's transfer succeeded, want it to die before its serial write")
	}
	cSlot := inner.parkedEntries(t)[resumeSlotsOtherID].Slot
	if cSlot == "" || cSlot == "scsi4" {
		t.Errorf("C's record names slot %q, want a slot other than A's scsi4", cSlot)
	}
	cLanding, _ := inner.configs[90000][cSlot].(string)
	if !strings.HasPrefix(cLanding, "data:vm-90000-disk-5") {
		t.Fatalf("parker %s = %q, want C's landing data:vm-90000-disk-5 with no serial", cSlot, cLanding)
	}

	landed, err := ResumeDiskTransferToParker(context.Background(), &storageFakeClient{scanFakeClient: inner}, nil,
		resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	if err != nil {
		t.Fatalf("A's resume: %v", err)
	}
	if inner.eventIndex("move:700:unused0->90000:scsi4:") < 0 {
		t.Errorf("events = %v, want A's own volume moved off 700's unused0 onto scsi4", inner.events)
	}
	if got := serialSlots(inner); len(got) != 1 || got["scsi4"] != landed+",serial="+transferStableID {
		t.Errorf("slots carrying A's serial = %v, want only scsi4 holding %s", got, landed)
	}
	if got, _ := inner.configs[90000][cSlot].(string); got != cLanding {
		t.Errorf("parker %s = %q, want C's landing %q left as it was", cSlot, got, cLanding)
	}
	if record := inner.parkedEntries(t)[resumeSlotsOtherID]; record.Slot != cSlot {
		t.Errorf("C's record = %+v, want it still naming %s", record, cSlot)
	}
}

// TestResumeSlots_AnotherRecordOnTheRecordedSlotRefusesTheClaim is the state
// two transfers leave when both records name scsi4 and a landing with no
// serial sits there. Nothing says whose landing it is, so the claim refuses
// and writes nothing. Before the change the proof claimed it as A's.
func TestResumeSlots_AnotherRecordOnTheRecordedSlotRefusesTheClaim(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   transferRecord("data:vm-700-disk-1", "scsi4", "700"),
				resumeSlotsOtherID: transferRecord("data:vm-701-disk-0", "scsi4", "701"),
			}),
			"scsi4": "data:vm-90000-disk-5",
		},
	})
	before := cloneConfig(c.configs[700])
	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `parker vmid 90000 also keeps an unfinished transfer record of disk `+resumeSlotsOtherID+` naming slot "scsi4"`)
	requireNothingMovedOrTagged(t, c)
	requireSourceUnchanged(t, c, before)
}

// TestResumeSlots_FallbackSkipsAnotherRecordsSlot is the resume's own slot
// choice. C's recorded scsi2 is taken, and A's unfinished record names the
// free scsi1. Before the change C's fallback took scsi1, A's landing spot.
func TestResumeSlots_FallbackSkipsAnotherRecordsSlot(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		701: {"unused0": "data:vm-701-disk-0"},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   transferRecord("data:vm-700-disk-1", "scsi1", "700"),
				resumeSlotsOtherID: transferRecord("data:vm-701-disk-0", "scsi2", "701"),
			}),
			"scsi0": "data:vm-90000-disk-20,serial=bpd-0000000000000000",
			"scsi2": "data:vm-90000-disk-22,serial=bpd-0000000000000002",
		},
	})
	intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi2", Volid: "data:vm-701-disk-0", SourceVMCID: "701"}
	if _, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, resumeSlotsOtherID, transferTestCfg, ParkContext{}); err != nil {
		t.Fatalf("C's resume: %v", err)
	}
	if c.eventIndex("move:701:unused0->90000:scsi3:") < 0 {
		t.Errorf("events = %v, want C moved onto scsi3, the first slot no record names", c.events)
	}
	if _, taken := c.configs[90000]["scsi1"]; taken {
		t.Errorf("parker scsi1 = %v, want A's recorded slot left free", c.configs[90000]["scsi1"])
	}
}

// TestResumeSlots_FallbackSlotGoesIntoTheRecordBeforeTheMove is a crash after
// a fallback. A's record names scsi4, which another disk's parked volume now
// holds, and A's volume still sits on 700's unused0. A's resume falls back to
// scsi5, moves the volume there, and dies before its serial write. The record
// now names scsi5, so the next resume finds the landing on the slot the record
// names and claims it. Before the change the record kept scsi4, and the next
// resume refused the landing on scsi5 and asked for an audit.
func TestResumeSlots_FallbackSlotGoesIntoTheRecordBeforeTheMove(t *testing.T) {
	t.Parallel()
	parked := "data:vm-90000-disk-24,serial=bpd-0000000000000004"
	inner := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		90000: fullLowSlots(map[string]any{
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID: transferRecord("data:vm-700-disk-1", "scsi4", "700"),
			}),
			"scsi4": parked,
		}),
	})
	inner.renameCounter[90000] = 5
	_, err := ResumeDiskTransferToParker(context.Background(), &failSerialClient{scanFakeClient: inner, serial: transferStableID},
		nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	if err == nil {
		t.Fatal("A's first resume succeeded, want it to die before its serial write")
	}
	landing, _ := inner.configs[90000]["scsi5"].(string)
	if !strings.HasPrefix(landing, "data:vm-90000-disk-5") || strings.Contains(landing, "serial=") {
		t.Fatalf("parker scsi5 = %q, want A's landing data:vm-90000-disk-5 with no serial", landing)
	}
	record := inner.parkedEntries(t)[transferStableID]
	if record.Slot != "scsi5" || record.Volid != "data:vm-700-disk-1" || record.SourceVMCID != "700" {
		t.Fatalf("A's record = %+v, want it naming fallback slot scsi5 with its recorded volume and source unchanged", record)
	}

	intent := resumeProofIntent
	intent.Slot = record.Slot
	landed, err := ResumeDiskTransferToParker(context.Background(), &storageFakeClient{scanFakeClient: inner}, nil,
		intent, transferStableID, transferTestCfg, ParkContext{})
	if err != nil {
		t.Fatalf("A's second resume: %v", err)
	}
	if got := serialSlots(inner); len(got) != 1 || got["scsi5"] != landed+",serial="+transferStableID {
		t.Errorf("slots carrying A's serial = %v, want only scsi5 holding %s", got, landed)
	}
	if got := inner.configs[90000]["scsi4"]; got != parked {
		t.Errorf("parker scsi4 = %v, want the other disk's %s left as it was", got, parked)
	}
}

// afterRecordWriteClient runs after on parker 90000's configuration each time
// a write of the parker's description goes through, the way another writer's
// change lands beside the resume's record write.
type afterRecordWriteClient struct {
	*storageFakeClient
	after func(configs map[int]map[string]any)
}

func (c *afterRecordWriteClient) Nodes() sdknodes.Service {
	inner := c.storageFakeClient.Nodes().(*fakeNodesService)
	update := inner.updateQemuConfigFn
	inner.updateQemuConfigFn = func(ctx context.Context, node, vmidText string, params *sdknodes.UpdateQemuConfigParams) error {
		if params.Description == nil || vmidText != "90000" {
			return update(ctx, node, vmidText, params)
		}
		if err := update(ctx, node, vmidText, params); err != nil {
			return err
		}
		if c.after != nil {
			c.mu.Lock()
			c.after(c.configs)
			c.mu.Unlock()
		}
		return nil
	}
	return inner
}

// editRecords rewrites the records in parker 90000's description with edit.
func editRecords(t *testing.T, configs map[int]map[string]any, edit func(records map[string]parkerProvEntry)) {
	t.Helper()
	desc, _ := configs[90000]["description"].(string)
	nonBOSH, records, raw := parseParkerSentinel(desc)
	edit(records)
	rendered, err := renderParkerSentinel(nonBOSH, records, raw)
	if err != nil {
		t.Fatal(err)
	}
	configs[90000]["description"] = rendered
}

// fallbackWorld is the state where A's record names scsi4, another disk's
// parked volume holds scsi4, and A's volume still sits on 700's unused0, so
// A's resume has to fall back to scsi5 and rewrite A's record. Disk C's record
// names scsi9 and C's source 701 still names C's volume, so the write keeps
// C's record. When the stale argument is true, 701 no longer names C's volume,
// so the write collects C's record.
func fallbackWorld(t *testing.T, stale bool) *scanFakeClient {
	t.Helper()
	source701 := map[string]any{"unused0": "data:vm-701-disk-0"}
	if stale {
		source701 = map[string]any{}
	}
	inner := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		701: source701,
		90000: fullLowSlots(map[string]any{
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   transferRecord("data:vm-700-disk-1", "scsi4", "700"),
				resumeSlotsOtherID: transferRecord("data:vm-701-disk-0", "scsi9", "701"),
			}),
			"scsi4": "data:vm-90000-disk-24,serial=bpd-0000000000000004",
		}),
	})
	inner.renameCounter[90000] = 5
	return inner
}

// requireNoMoveOrSerialWrite fails when any of events is a move or a write of
// serial. The record write itself is allowed.
func requireNoMoveOrSerialWrite(t *testing.T, events []string, serial string) {
	t.Helper()
	for _, event := range events {
		if strings.HasPrefix(event, "move:") || strings.Contains(event, "serial="+serial) {
			t.Errorf("events = %v, want no move and no write of %s", events, serial)
			return
		}
	}
}

// TestResumeSlots_FallbackRecordWriteRefusesWhenTheParkerChanged covers the
// re-read after the resume points A's record at the fallback slot. The move is
// pinned to that read, so the resume refuses retriably, before it moves
// anything, when the parker changed in any way besides A's own record and the
// stale records the write collects. A change to another key refuses, and so
// does an edit, an addition, or a removal of another disk's record. A stale
// record that the write collects doesn't refuse. Before the change, any
// change to the parker's records went unnoticed.
func TestResumeSlots_FallbackRecordWriteRefusesWhenTheParkerChanged(t *testing.T) {
	t.Parallel()
	const refusal = "parker vmid 90000 changed while the resume pointed disk " + transferStableID + "'s record at slot scsi5"
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, configs map[int]map[string]any)
	}{
		{"another key changed", func(_ *testing.T, configs map[int]map[string]any) { configs[90000]["onboot"] = "1" }},
		{"another disk's record was edited", func(t *testing.T, configs map[int]map[string]any) {
			editRecords(t, configs, func(records map[string]parkerProvEntry) {
				record := records[resumeSlotsOtherID]
				record.Slot = "scsi10"
				records[resumeSlotsOtherID] = record
			})
		}},
		{"another disk's record was added", func(t *testing.T, configs map[int]map[string]any) {
			editRecords(t, configs, func(records map[string]parkerProvEntry) {
				records["bpd-d0d0d0d0d0d0d0d0"] = transferRecord("data:vm-702-disk-0", "scsi11", "702")
			})
		}},
		{"another disk's record was removed", func(t *testing.T, configs map[int]map[string]any) {
			editRecords(t, configs, func(records map[string]parkerProvEntry) { delete(records, resumeSlotsOtherID) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := fallbackWorld(t, false)
			c := &afterRecordWriteClient{
				storageFakeClient: &storageFakeClient{scanFakeClient: inner},
				after:             func(configs map[int]map[string]any) { tc.after(t, configs) },
			}
			before := cloneConfig(inner.configs[700])

			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			requireResumeRetry(t, err, refusal)
			requireNoMoveOrSerialWrite(t, inner.events, transferStableID)
			requireSourceUnchanged(t, inner, before)
		})
	}

	t.Run("the write collected a stale record", func(t *testing.T) {
		t.Parallel()
		inner := fallbackWorld(t, true)
		c := &afterRecordWriteClient{storageFakeClient: &storageFakeClient{scanFakeClient: inner}}

		if _, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{}); err != nil {
			t.Fatalf("A's resume: %v", err)
		}
		if got := serialSlots(inner); len(got) != 1 || got["scsi5"] == "" {
			t.Errorf("slots carrying A's serial = %v, want only scsi5", got)
		}
		if _, kept := inner.parkedEntries(t)[resumeSlotsOtherID]; kept {
			t.Error("C's stale record is still on the parker, want the write to have collected it")
		}
	})
}

// TestResumeSlots_FallbackRecordWriteRefusesWhenTheRecordNoLongerMatches covers
// the check before the rewrite. The resume follows an intent that names scsi4,
// and A's record on the parker names scsi9 by the time the resume reads it, so
// the record isn't the one the intent came from. The resume refuses retriably
// and writes and moves nothing.
func TestResumeSlots_FallbackRecordWriteRefusesWhenTheRecordNoLongerMatches(t *testing.T) {
	t.Parallel()
	inner := fallbackWorld(t, false)
	editRecords(t, inner.configs, func(records map[string]parkerProvEntry) {
		record := records[transferStableID]
		record.Slot = "scsi9"
		records[transferStableID] = record
		delete(records, resumeSlotsOtherID)
	})
	before := cloneConfig(inner.configs[700])

	_, err := ResumeDiskTransferToParker(context.Background(), inner, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRetry(t, err, "parker vmid 90000 no longer keeps the record of disk "+transferStableID+" that the resume read")
	requireNothingMovedOrTagged(t, inner)
	requireSourceUnchanged(t, inner, before)
}

// failRecordWriteClient is a stoppedApplyingSourceClient whose parker rejects
// every write of its description.
type failRecordWriteClient struct {
	*stoppedApplyingSourceClient
}

func (c *failRecordWriteClient) Nodes() sdknodes.Service {
	inner := c.stoppedApplyingSourceClient.Nodes().(*fakeNodesService)
	update := inner.updateQemuConfigFn
	inner.updateQemuConfigFn = func(ctx context.Context, node, vmidText string, params *sdknodes.UpdateQemuConfigParams) error {
		if params.Description != nil && vmidText == "90000" {
			return errors.New("500 Internal Server Error")
		}
		return update(ctx, node, vmidText, params)
	}
	return inner
}

// TestResumeSlots_StoppedSourceRefusalAfterAnAppliedDeleteSaysSo covers the
// refusals of the move window that come after the resume applied a pending
// delete on a stopped source. The source no longer holds that slot, so each
// refusal says the delete was applied. That holds when the parker has no free
// slot, when the rewrite of A's record at a fallback slot fails, and when the
// move fails. Before the change, the first two left that out.
func TestResumeSlots_StoppedSourceRefusalAfterAnAppliedDeleteSaysSo(t *testing.T) {
	t.Parallel()
	pctx := ParkContext{DiskCID: "pvd-test", ApplyFoundPendingDelete: true}
	applied := "applied the pending delete of scsi1 on source vm 700, and then "
	for _, tc := range []struct {
		name   string
		parker func() map[string]any
		client func(inner *scanFakeClient) Client
		want   string
	}{
		{"the parker has no free slot",
			func() map[string]any {
				cfg := resumeProofParker(nil)
				for i := range 31 {
					cfg[fmt.Sprintf("scsi%d", i)] = fmt.Sprintf("data:vm-90000-disk-%d,serial=bpd-00000000000000%02d", 20+i, i)
				}
				return cfg
			},
			func(inner *scanFakeClient) Client { return &stoppedApplyingSourceClient{scanFakeClient: inner} },
			applied + "the choice of a slot on parker vmid 90000 did not go through"},
		{"the rewrite of the record fails",
			func() map[string]any {
				return fullLowSlots(resumeProofParker(map[string]any{"scsi4": "data:vm-90000-disk-24,serial=bpd-0000000000000004"}))
			},
			func(inner *scanFakeClient) Client {
				return &failRecordWriteClient{&stoppedApplyingSourceClient{scanFakeClient: inner}}
			},
			applied + "the rewrite of disk " + transferStableID + "'s record on parker vmid 90000 did not go through"},
		{"the move fails",
			func() map[string]any { return resumeProofParker(nil) },
			func(inner *scanFakeClient) Client {
				inner.moveErr = errors.New("move refused by the test")
				return &stoppedApplyingSourceClient{scanFakeClient: inner}
			},
			applied + "the move of unused0 did not go through"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := newScanFakeClient(map[int]map[string]any{700: {}, 90000: tc.parker()})
			inner.held = map[int]map[string]any{700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"}}
			c := tc.client(inner)

			_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, pctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resume error = %v, want it to contain %q", err, tc.want)
			}
			if _, held := inner.held[700]["scsi1"]; held {
				t.Error("the pending delete is still pending, want the resume to have applied it")
			}
			requireNoMoveOrSerialWrite(t, inner.events, transferStableID)
		})
	}
}

// staleSourceClient serves config reads of source 700 that differ from what
// its pending endpoint showed the proof, the way a change between the proof
// and the move does. edit rewrites the config read's copy.
type staleSourceClient struct {
	*scanFakeClient
	vmid int
	edit func(cfg map[string]any)
}

func (c *staleSourceClient) QEMU() qemu.Service {
	inner := c.scanFakeClient.QEMU().(*fakeQEMUService)
	read := inner.configFn
	inner.configFn = func(ctx context.Context, node string, vmid int) (map[string]any, error) {
		cfg, err := read(ctx, node, vmid)
		if err == nil && vmid == c.vmid {
			c.edit(cfg)
		}
		return cfg, err
	}
	return inner
}

// TestResumeSlots_MoveIsPinnedToWhatTheProofRead covers the move after the
// proof. When the source's digest, the parker's digest, or the volume on the
// source key no longer matches the proof's read, the move doesn't go out and
// the resume fails retriably. Before the pin the move took whatever the key
// named at POST time.
func TestResumeSlots_MoveIsPinnedToWhatTheProofRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		vmid int
		edit func(cfg map[string]any)
		want string
	}{
		{"the source's digest changed", 700, func(cfg map[string]any) { cfg["digest"] = "s2" }, `source vm 700's digest is "s2"`},
		{"the parker's digest changed", 90000, func(cfg map[string]any) { cfg["digest"] = "p2" }, `target vm 90000's digest is "p2"`},
		{"the key names another volume", 700, func(cfg map[string]any) { cfg["unused0"] = "data:vm-700-disk-7" }, `unused0 of source vm 700 names "data:vm-700-disk-7"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := newScanFakeClient(map[int]map[string]any{
				700:   {"unused0": "data:vm-700-disk-1", "digest": "s1"},
				90000: resumeProofParker(map[string]any{"digest": "p1"}),
			})
			c := &staleSourceClient{scanFakeClient: inner, vmid: tc.vmid, edit: tc.edit}
			before := cloneConfig(inner.configs[700])
			_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			var typed *cpierrors.Error
			if !errors.As(err, &typed) || !typed.OkToRetry() || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("resume error = %v, want a retriable error naming %q", err, tc.want)
			}
			requireNothingMovedOrTagged(t, inner)
			requireSourceUnchanged(t, inner, before)
		})
	}
}

// migratedSourceClient puts one source VM on node pve2 of a two-node cluster,
// 700 unless vmid names another. Reads of it on any other node answer the way
// PVE does for a VM that isn't there, and every other guest lives on pve1.
type migratedSourceClient struct {
	*scanFakeClient
	vmid int
}

func (c *migratedSourceClient) nodeOf(vmid int) string {
	migrated := c.vmid
	if migrated == 0 {
		migrated = 700
	}
	if vmid == migrated {
		return "pve2"
	}
	return "pve1"
}

func (c *migratedSourceClient) missing(node string, vmid int) error {
	if c.nodeOf(vmid) == node {
		return nil
	}
	return fmt.Errorf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, vmid)
}

func (c *migratedSourceClient) QEMU() qemu.Service {
	inner := c.scanFakeClient.QEMU().(*fakeQEMUService)
	read := inner.configFn
	inner.configFn = func(ctx context.Context, node string, vmid int) (map[string]any, error) {
		if err := c.missing(node, vmid); err != nil {
			return nil, err
		}
		return read(ctx, node, vmid)
	}
	return inner
}

func (c *migratedSourceClient) Nodes() sdknodes.Service {
	inner := c.scanFakeClient.Nodes().(*fakeNodesService)
	inner.qemuConfigFn = c.QEMU().Config
	pending := inner.listQemuPendingFn
	inner.listQemuPendingFn = func(ctx context.Context, node, vmidText string) (*sdknodes.ListQemuPendingResponse, error) {
		vmid, _ := strconv.Atoi(vmidText)
		if err := c.missing(node, vmid); err != nil {
			return nil, err
		}
		return pending(ctx, node, vmidText)
	}
	inner.listQemuFn = func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		var resp sdknodes.ListQemuResponse
		for vmid, cfg := range c.configs {
			if c.nodeOf(vmid) != node {
				continue
			}
			tags, _ := cfg[cfgKeyTags].(string)
			raw, err := json.Marshal(map[string]any{"vmid": vmid, cfgKeyTags: tags})
			if err != nil {
				return nil, err
			}
			resp = append(resp, raw)
		}
		return &resp, nil
	}
	return inner
}

func (c *migratedSourceClient) Cluster() sdkcluster.Service {
	return &fakeClusterService{
		listConfigNodesFn: func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			resp := sdkcluster.ListConfigNodesResponse{json.RawMessage(`{"name": "pve1"}`), json.RawMessage(`{"name": "pve2"}`)}
			return &resp, nil
		},
		listResourcesFn: func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			var resp sdkcluster.ListResourcesResponse
			for vmid, cfg := range c.configs {
				tags, _ := ConfigString(cfg, cfgKeyTags)
				row := clusterRow(vmid, tags)
				row["node"] = c.nodeOf(vmid)
				raw, err := json.Marshal(row)
				if err != nil {
					return nil, err
				}
				resp = append(resp, raw)
			}
			return &resp, nil
		},
	}
}

// TestResumeSlots_ReleasedVolumeNeedsNoGuestNamingIt covers the config-edit
// park of a volume the source released. A source that migrated to pve2 with
// the volume on its unused0 reads as missing on the parker's node, and before
// the change that read alone counted as gone. Another guest that names the
// volume on an unused entry stops the park as well, although before the
// change only a parker that named the volume could do so. Each refuses, and
// the volume isn't attached to the parker with the disk's serial.
func TestResumeSlots_ReleasedVolumeNeedsNoGuestNamingIt(t *testing.T) {
	t.Parallel()

	t.Run("the source migrated to another node", func(t *testing.T) {
		t.Parallel()
		inner := newScanFakeClient(map[int]map[string]any{
			700:   {"unused0": "data:vm-700-disk-1"},
			90000: resumeProofParker(nil),
		})
		c := &migratedSourceClient{scanFakeClient: inner}
		before := cloneConfig(inner.configs[700])
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, "source vm 700 has no configuration on node pve1 and the cluster finds it on node pve2")
		requireNothingMovedOrTagged(t, inner)
		requireSourceUnchanged(t, inner, before)
		if len(qemu.ParseDisks(inner.configs[90000])) != 0 {
			t.Errorf("parker 90000 = %v, want nothing attached", inner.configs[90000])
		}
	})

	t.Run("another guest names the volume", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			701:   {"unused0": "data:vm-700-disk-1"},
			90000: resumeProofParker(nil),
		})
		c.configErr = map[int]error{
			700: errors.New("Configuration file 'nodes/pve1/qemu-server/700.conf' does not exist"),
		}
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, "vm 701 names volume data:vm-700-disk-1 on unused0")
		requireNothingMovedOrTagged(t, c)
		if len(qemu.ParseDisks(c.configs[90000])) != 0 {
			t.Errorf("parker 90000 = %v, want nothing attached", c.configs[90000])
		}
	})
}

// stoppedApplyingSourceClient reads every VM as stopped and applies a pending
// delete on one at once, the way PVE applies every pending change of a stopped
// VM with its next config write. An owned volume drops to the first free
// unused entry.
type stoppedApplyingSourceClient struct {
	*scanFakeClient
}

func (c *stoppedApplyingSourceClient) QEMU() qemu.Service {
	inner := c.scanFakeClient.QEMU().(*fakeQEMUService)
	inner.statusFn = func(context.Context, string, int) (map[string]any, error) {
		return map[string]any{"status": "stopped"}, nil
	}
	return inner
}

func (c *stoppedApplyingSourceClient) Nodes() sdknodes.Service {
	inner := c.scanFakeClient.Nodes().(*fakeNodesService)
	update := inner.updateQemuConfigFn
	inner.updateQemuConfigFn = func(ctx context.Context, node, vmidText string, params *sdknodes.UpdateQemuConfigParams) error {
		vmid, _ := strconv.Atoi(vmidText)
		if params.Delete != nil && params.Revert == nil {
			c.mu.Lock()
			value, held := c.held[vmid][*params.Delete]
			if held {
				delete(c.held[vmid], *params.Delete)
				c.configs[vmid][*params.Delete] = value
				c.deleteConfigKeyLocked(c.configs[vmid], vmid, *params.Delete)
				c.mu.Unlock()
				return nil
			}
			c.mu.Unlock()
		}
		return update(ctx, node, vmidText, params)
	}
	return inner
}

// TestResumeSlots_StoppedSourcePendingDelete covers the resume of a caller
// that applies a found pending delete on a stopped source, which detach_disk
// and attach_disk do. When the slot carries another disk's serial, the resume
// refuses before it applies the delete, and the delete stays pending. Before
// the proof the resume applied it and moved the other disk's volume. When the
// applied delete leaves two keys naming the volume, the refusal says the
// delete was applied.
func TestResumeSlots_StoppedSourcePendingDelete(t *testing.T) {
	t.Parallel()
	pctx := ParkContext{DiskCID: "pvd-test", ApplyFoundPendingDelete: true}

	t.Run("the slot carries another disk's serial", func(t *testing.T) {
		t.Parallel()
		inner := newScanFakeClient(map[int]map[string]any{
			700:   {},
			90000: resumeProofParker(nil),
		})
		inner.held = map[int]map[string]any{700: {"scsi1": "data:vm-700-disk-1,serial=" + resumeProofOtherID + ",size=10G"}}
		c := &stoppedApplyingSourceClient{scanFakeClient: inner}
		_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, pctx)
		requireResumeRefusal(t, err, "scsi1 of source vm 700 names volume data:vm-700-disk-1 with another disk's serial "+resumeProofOtherID)
		requireNothingMovedOrTagged(t, inner)
		if _, held := inner.held[700]["scsi1"]; !held {
			t.Error("the resume applied the other disk's pending delete, want it left pending")
		}
		if inner.eventIndex("config-delete:700") >= 0 {
			t.Errorf("events = %v, want no delete on source 700", inner.events)
		}
	})

	t.Run("the applied delete leaves two keys naming the volume", func(t *testing.T) {
		t.Parallel()
		inner := newScanFakeClient(map[int]map[string]any{
			700:   {"unused0": "data:vm-700-disk-1"},
			90000: resumeProofParker(nil),
		})
		inner.held = map[int]map[string]any{700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"}}
		c := &stoppedApplyingSourceClient{scanFakeClient: inner}
		_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, pctx)
		requireResumeRefusal(t, err, "It applied the pending delete of scsi1 on source vm 700, but it moved nothing and wrote no serial")
		requireNothingMovedOrTagged(t, inner)
	})
}

// resumeSlotsOtherIntent is disk C's transfer record as its own resume reads
// it, with C's volume recorded under its name on source 701.
func resumeSlotsOtherIntent(slot string) DiskTransferIntent {
	return DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: slot, Volid: "data:vm-701-disk-0", SourceVMCID: "701"}
}

// requireResumeRetry fails unless err is retriable and its text contains want.
func requireResumeRetry(t *testing.T, err error, want string) {
	t.Helper()
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Errorf("resume error = %v, want a retriable error", err)
		return
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("resume error = %q, want it to contain %q", err.Error(), want)
	}
}

// requireNoWriteBy fails when the events hold a move, a write of serial, or a
// record write.
func requireNoWriteBy(t *testing.T, c *scanFakeClient, serial string) {
	t.Helper()
	for _, event := range c.events {
		if strings.HasPrefix(event, "move:") || strings.Contains(event, "serial="+serial) || strings.HasPrefix(event, "description:") {
			t.Errorf("events = %v, want no move, no write of serial %s, and no record write", c.events, serial)
			return
		}
	}
}

// TestResumeSlots_LandingOnAnEmptySlotOfAnUnmovedRecord is the shape an older
// release's fallback leaves. A's record names scsi4, and A's volume still sits
// on 700's unused0. C's record names scsi2, which a park then took, so C's
// resume fell back to scsi4, landed there, and died before its serial write.
// Before the change A's resume claimed C's landing as A's and wrote A's serial
// onto it. Now A's resume refuses and asks for an audit. When C's source names
// C's volume nowhere and storage no longer holds it, C's move is proved and
// nothing on scsi2 accounts for it. When C's source names C's volume under
// another disk's serial, C's move can be neither proved nor ruled out, and A's
// own source still names A's volume. C's resume refuses as well, because the
// landing sits on A's slot while A's source still names A's volume.
func TestResumeSlots_LandingOnAnEmptySlotOfAnUnmovedRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		source map[string]any
		want   string
	}{
		{"C's source no longer names C's volume", map[string]any{},
			`disk ` + resumeSlotsOtherID + `'s unfinished record (disk cid pvd-c, slot "scsi2") reads as moved, because its source vm 701 ` +
				`names its recorded volume data:vm-701-disk-0 nowhere, and storage data no longer holds it, and no landing on its slot ` +
				`accounts for it, so data:vm-90000-disk-5 on scsi4 may be that disk's and not this disk's (disk cid pvd-a, slot "scsi4") ` +
				`(audit required)`},
		{"C's source names C's volume under another serial", map[string]any{"scsi1": "data:vm-701-disk-0,serial=bpd-dddddddddddddddd,size=10G"},
			`data:vm-90000-disk-5 on scsi4, the slot this record names, may be this disk's (disk cid pvd-a) or disk ` + resumeSlotsOtherID +
				`'s (disk cid pvd-c), because this disk's source vm 700 still names this disk's recorded volume data:vm-700-disk-1 on ` +
				`unused0, while disk ` + resumeSlotsOtherID + `'s unfinished record names slot "scsi2", which holds no landing, and its ` +
				`source vm 701 names its recorded volume data:vm-701-disk-0 on scsi1 under another disk's serial bpd-dddddddddddddddd ` +
				`(audit required)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recordA := transferRecord("data:vm-700-disk-1", "scsi4", "700")
			recordA.DiskCID = "pvd-a"
			recordC := transferRecord("data:vm-701-disk-0", "scsi2", "701")
			recordC.DiskCID = "pvd-c"
			inner := newScanFakeClient(map[int]map[string]any{
				700: {"unused0": "data:vm-700-disk-1"},
				701: tc.source,
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": parkerRecords(t, map[string]parkerProvEntry{
						transferStableID:   recordA,
						resumeSlotsOtherID: recordC,
					}),
					"scsi2": "data:vm-90000-disk-22,serial=bpd-0000000000000002",
					"scsi4": "data:vm-90000-disk-5",
				},
			})
			c := &storageFakeClient{scanFakeClient: inner}
			before := cloneConfig(inner.configs[700])

			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			requireResumeRefusal(t, err, tc.want)
			requireNothingMovedOrTagged(t, inner)
			requireSourceUnchanged(t, inner, before)

			_, err = ResumeDiskTransferToParker(context.Background(), c, nil, resumeSlotsOtherIntent("scsi2"), resumeSlotsOtherID, transferTestCfg, ParkContext{})
			requireResumeRefusal(t, err, `data:vm-90000-disk-5 on scsi4 sits on the slot disk `+transferStableID+`'s unfinished record names `+
				`(disk cid pvd-a), but its source vm 700 still names its recorded volume data:vm-700-disk-1 on unused0, so it can't count `+
				`as that disk's, and this disk's record (disk cid pvd-c) names slot "scsi2" (audit required)`)
			requireNoWriteBy(t, inner, resumeSlotsOtherID)
			if got := inner.configs[90000]["scsi4"]; got != "data:vm-90000-disk-5" {
				t.Errorf("parker scsi4 = %v, want C's landing left as it was", got)
			}
		})
	}
}

// TestResumeSlots_LandingOnTheSlotOfAnUnmovedRecord is the same gap the other
// way round. A's record names scsi4, and an older release's fallback landed A's
// volume on scsi5, the slot C's record names, before A's serial write. C's
// volume still sits on 701's unused0, so C hasn't moved. Before the change, C's
// resume refused retriably, which never clears, and A's resume refused because
// the landing didn't sit on A's own slot. Now C's resume refuses and asks for
// an audit, because A's source names A's volume nowhere and storage no longer
// holds it, so A's move is proved and nothing on scsi4 accounts for it. A's
// resume refuses and asks for an audit too, because the landing sits on C's
// slot while C's source still names C's volume.
func TestResumeSlots_LandingOnTheSlotOfAnUnmovedRecord(t *testing.T) {
	t.Parallel()
	recordA := transferRecord("data:vm-700-disk-1", "scsi4", "700")
	recordA.DiskCID = "pvd-a"
	recordC := transferRecord("data:vm-701-disk-0", "scsi5", "701")
	recordC.DiskCID = "pvd-c"
	inner := newScanFakeClient(map[int]map[string]any{
		700: {},
		701: {"unused0": "data:vm-701-disk-0"},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   recordA,
				resumeSlotsOtherID: recordC,
			}),
			"scsi5": "data:vm-90000-disk-6",
		},
	})
	c := &storageFakeClient{scanFakeClient: inner}

	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeSlotsOtherIntent("scsi5"), resumeSlotsOtherID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `disk `+transferStableID+`'s unfinished record (disk cid pvd-a, slot "scsi4") reads as moved, because `+
		`its source vm 700 names its recorded volume data:vm-700-disk-1 nowhere, and storage data no longer holds it, and no landing `+
		`on its slot accounts for it, so data:vm-90000-disk-6 on scsi5 may be that disk's and not this disk's (disk cid pvd-c, `+
		`slot "scsi5") (audit required)`)
	requireNoWriteBy(t, inner, resumeSlotsOtherID)

	_, err = ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `data:vm-90000-disk-6 on scsi5 sits on the slot disk `+resumeSlotsOtherID+`'s unfinished record names `+
		`(disk cid pvd-c), but its source vm 701 still names its recorded volume data:vm-701-disk-0 on unused0, so it can't count as `+
		`that disk's, and this disk's record (disk cid pvd-a) names slot "scsi4" (audit required)`)
	requireNothingMovedOrTagged(t, inner)

	if got := inner.configs[90000]["scsi5"]; got != "data:vm-90000-disk-6" {
		t.Errorf("parker scsi5 = %v, want A's landing left as it was", got)
	}
	for key, value := range inner.configs[90000] {
		if text, ok := value.(string); ok && bareDriveVolid(text) == "data:vm-700-disk-1" {
			t.Errorf("parker %s = %s, want nothing attached under A's recorded name", key, text)
		}
	}
	if got := inner.configs[701]["unused0"]; got != "data:vm-701-disk-0" {
		t.Errorf("source 701 unused0 = %v, want C's volume left where it is", got)
	}
}

// TestResumeSlots_ParkByConfigEditSkipsAnotherRecordsSlot covers a park by config
// edit. Disk C's unfinished transfer record names scsi0, the lowest free slot.
// Before the change the park took scsi0, and C's resume then had to fall back
// to another slot. Now the park takes scsi1 and leaves scsi0 for C.
func TestResumeSlots_ParkByConfigEditSkipsAnotherRecordsSlot(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				resumeSlotsOtherID: transferRecord("data:vm-701-disk-0", "scsi0", "701"),
			}),
		},
	})
	slot, err := attachToParkerLocked(context.Background(), c, nil, "pve1", 90000, "data:vm-9001-disk-0", transferStableID)
	if err != nil {
		t.Fatalf("attachToParkerLocked: %v", err)
	}
	if slot != "scsi1" {
		t.Errorf("slot = %q, want scsi1, the first slot no record names", slot)
	}
	if _, taken := c.configs[90000]["scsi0"]; taken {
		t.Errorf("parker scsi0 = %v, want C's recorded slot left free", c.configs[90000]["scsi0"])
	}
}

// requireNoMoveOrSerial fails when any of events is a move, a record write, or
// a write of either serial.
func requireNoMoveOrSerial(t *testing.T, events []string, serials ...string) {
	t.Helper()
	for _, event := range events {
		bad := strings.HasPrefix(event, "move:") || strings.HasPrefix(event, "description:")
		for _, serial := range serials {
			bad = bad || strings.Contains(event, "serial="+serial)
		}
		if bad {
			t.Errorf("events = %v, want no move, no record write, and no write of %v", events, serials)
			return
		}
	}
}

// gapClaimRefusal is the refusal C's resume returns when it would claim the
// landing on its own slot while A reads as unmoved with nothing on scsi4.
func gapClaimRefusal(cSlot string) string {
	return `data:vm-90000-disk-6 on ` + cSlot + `, the slot this record names, may be this disk's (disk cid pvd-c) or disk ` +
		transferStableID + `'s (disk cid pvd-a), because this disk's source vm 701 still names this disk's recorded volume ` +
		`data:vm-701-disk-0 on unused0, while disk ` + transferStableID + `'s unfinished record names slot "scsi4", which holds no ` +
		`landing, and its source vm 700 still names its recorded volume data:vm-700-disk-1 on unused0 (audit required)`
}

// gapSlotRefusal is the refusal A's resume returns when the landing sits on
// C's slot while C's source still names C's volume.
func gapSlotRefusal(cSlot string) string {
	return `data:vm-90000-disk-6 on ` + cSlot + ` sits on the slot disk ` + resumeSlotsOtherID + `'s unfinished record names ` +
		`(disk cid pvd-c), but its source vm 701 still names its recorded volume data:vm-701-disk-0 on unused0, so it can't count as ` +
		`that disk's, and this disk's record (disk cid pvd-a) names slot "scsi4" (audit required)`
}

// TestResumeSlots_LandingOnTheSlotOfARecordThatLooksUnmoved builds the end
// state of the gap the source check alone leaves, because the older release's
// fallback that drove it no longer exists. A's record names scsi4, and C's
// record names scsi5 while C's volume still sits on 701's unused0. An older
// release's fallback landed A's volume on scsi5, and the CPI died before A's
// serial write. A new disk then took A's freed name on 700, and its own
// interrupted detach left it on 700's unused0 with no serial, so A looks
// unmoved. Before the change C's resume claimed A's landing and wrote C's
// serial onto it. Now C's resume refuses, because its own source still names
// its volume while A reads as unmoved with nothing on scsi4, and A's resume
// refuses, because the landing sits on C's slot while C's source still names
// C's volume.
func TestResumeSlots_LandingOnTheSlotOfARecordThatLooksUnmoved(t *testing.T) {
	t.Parallel()
	recordA := transferRecord("data:vm-700-disk-1", "scsi4", "700")
	recordA.DiskCID = "pvd-a"
	recordC := transferRecord("data:vm-701-disk-0", "scsi5", "701")
	recordC.DiskCID = "pvd-c"
	c := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		701: {"unused0": "data:vm-701-disk-0"},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   recordA,
				resumeSlotsOtherID: recordC,
			}),
			"scsi5": "data:vm-90000-disk-6",
		},
	})
	before700, before701 := cloneConfig(c.configs[700]), cloneConfig(c.configs[701])

	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeSlotsOtherIntent("scsi5"), resumeSlotsOtherID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, gapClaimRefusal("scsi5"))

	_, err = ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, gapSlotRefusal("scsi5"))

	requireNoMoveOrSerial(t, c.events, transferStableID, resumeSlotsOtherID)
	if got := c.configs[90000]["scsi5"]; got != "data:vm-90000-disk-6" {
		t.Errorf("parker scsi5 = %v, want A's landing left bare", got)
	}
	requireSourceUnchanged(t, c, before700)
	if got := c.configs[701]; fmt.Sprint(got) != fmt.Sprint(before701) {
		t.Errorf("source 701 = %v, want it unchanged from %v", got, before701)
	}
}

// TestResumeSlots_OwnLandingBesideARecordThatLooksUnmovedNeedsAnOperator is
// the legitimate state that looks the same from every record. A's record names
// scsi4 and A's volume still sits on 700's unused0. C's transfer from 701 then
// lands on its own slot and dies before its serial write, and a new disk takes
// C's freed name on 701 and is left on 701's unused0 with no serial. The
// metadata can't tell this from the gap above, so both resumes refuse with
// the same pair of refusals, and an operator who reads the volumes settles it.
func TestResumeSlots_OwnLandingBesideARecordThatLooksUnmovedNeedsAnOperator(t *testing.T) {
	t.Parallel()
	recordA := transferRecord("data:vm-700-disk-1", "scsi4", "700")
	recordA.DiskCID = "pvd-a"
	inner := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		701: {"scsi1": "data:vm-701-disk-0,serial=" + resumeSlotsOtherID + ",size=10G"},
		90000: {
			cfgKeyTags:    "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{transferStableID: recordA}),
		},
	})
	inner.renameCounter[90000] = 6
	c := &failSerialClient{scanFakeClient: inner, serial: resumeSlotsOtherID}

	_, err := transferIntoParker(noBackoff(), c, nil, "pve1", 90000, 701, "data:vm-701-disk-0", transferTestCfg,
		ParkContext{StableID: resumeSlotsOtherID, DiskCID: "pvd-c", SourceVMCID: "701"})
	if err == nil {
		t.Fatal("C's transfer succeeded, want it to die before its serial write")
	}
	cSlot := inner.parkedEntries(t)[resumeSlotsOtherID].Slot
	cLanding, _ := inner.configs[90000][cSlot].(string)
	if cSlot == "" || cSlot == "scsi4" || cLanding != "data:vm-90000-disk-6" {
		t.Fatalf("C's record names slot %q holding %q, want C's bare landing data:vm-90000-disk-6 on a slot other than scsi4", cSlot, cLanding)
	}
	inner.configs[701]["unused0"] = "data:vm-701-disk-0"
	before700, before701 := cloneConfig(inner.configs[700]), cloneConfig(inner.configs[701])
	seen := len(inner.events)

	_, err = ResumeDiskTransferToParker(context.Background(), inner, nil, resumeSlotsOtherIntent(cSlot), resumeSlotsOtherID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, gapClaimRefusal(cSlot))

	_, err = ResumeDiskTransferToParker(context.Background(), inner, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, gapSlotRefusal(cSlot))

	requireNoMoveOrSerial(t, inner.events[seen:], transferStableID, resumeSlotsOtherID)
	if got := inner.configs[90000][cSlot]; got != cLanding {
		t.Errorf("parker %s = %v, want C's landing left bare", cSlot, got)
	}
	requireSourceUnchanged(t, inner, before700)
	if got := inner.configs[701]; fmt.Sprint(got) != fmt.Sprint(before701) {
		t.Errorf("source 701 = %v, want it unchanged from %v", got, before701)
	}
}
