// parker_provenance_gc_internal_test.go — white-box tests for the parked-disk
// provenance store's two capacity behaviors: garbage collection of records
// whose disk left the parker by a route that never removed them, and the
// budget refusal that keeps a write from reaching PVE's description cap.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// provFakeClient is a single-parker PVE: one config map, and a recorder for
// the description the provenance write pushes back.
type provFakeClient struct {
	parkerLockClient

	cfg map[string]any
	// written is the description of the last successful UpdateQemuConfig.
	written string
	// writes counts UpdateQemuConfig calls, so a test can prove the budget
	// refusal happened before the API call rather than at it.
	writes int
}

func (c *provFakeClient) QEMU() qemu.Service {
	return &fakeQEMUService{
		configFn: func(context.Context, string, int) (map[string]any, error) {
			out := make(map[string]any, len(c.cfg))
			for k, v := range c.cfg {
				out[k] = v
			}
			return out, nil
		},
	}
}

func (c *provFakeClient) Nodes() sdknodes.Service {
	return &fakeNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			c.writes++
			if params != nil && params.Description != nil {
				c.written = *params.Description
				c.cfg["description"] = *params.Description
			}
			return nil
		},
	}
}

// provTestClock returns a ParkerConfig whose clock is pinned, so record ages
// are exact rather than wall-clock dependent.
func provTestClock(now time.Time) ParkerConfig {
	cfg := parkerTestCfgInternal()
	cfg.NowFunc = func() time.Time { return now }
	return cfg
}

// parkerTestCfgInternal mirrors parkerTestCfg from the external test package,
// which this internal package cannot import.
func parkerTestCfgInternal() ParkerConfig {
	return ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999}
}

// provSentinel renders a description carrying the given records.
func provSentinel(t *testing.T, disks map[string]parkerProvEntry) string {
	t.Helper()
	desc, err := renderParkerSentinel("", disks, nil)
	if err != nil {
		t.Fatalf("render sentinel: %v", err)
	}
	return desc
}

// provRecords reads the bosh_parked_disks map back out of a description.
func provRecords(t *testing.T, desc string) map[string]parkerProvEntry {
	t.Helper()
	_, disks, _ := parseParkerSentinel(desc)
	return disks
}

// ---------------------------------------------------------------------------
// Garbage collection
// ---------------------------------------------------------------------------

// TestWriteParkerProvenance_PrunesStaleUnreferencedRecords is the accumulation
// half of the lab-pmx report: parker 90472 held records dated three days back
// from a director that no longer existed, because removeParkerProvenance is
// the only deletion and it only ever removes the one volid leaving.
func TestWriteParkerProvenance_PrunesStaleUnreferencedRecords(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cfg := provTestClock(now)

	live := parkerProvEntry{
		DiskCID:  "rbd:vm-90472-disk-0",
		ParkedAt: now.Add(-72 * time.Hour).Format(time.RFC3339),
		Node:     "lab-pmx-0",
		Volid:    "rbd:vm-90472-disk-0",
		Slot:     "scsi0",
	}
	stale := parkerProvEntry{
		DiskCID:  "rbd:vm-90472-disk-7",
		ParkedAt: now.Add(-72 * time.Hour).Format(time.RFC3339),
		Node:     "lab-pmx-0",
		Volid:    "rbd:vm-90472-disk-7",
		Slot:     "scsi7",
	}
	inFlight := parkerProvEntry{
		DiskCID:  "rbd:vm-604-disk-0",
		ParkedAt: now.Add(-2 * time.Minute).Format(time.RFC3339),
		Node:     "lab-pmx-0",
		Volid:    "rbd:vm-604-disk-0",
		Slot:     "scsi9",
	}

	c := &provFakeClient{cfg: map[string]any{
		"scsi0":       "rbd:vm-90472-disk-0,size=10G",
		"tags":        ParkerTag,
		"description": provSentinel(t, map[string]parkerProvEntry{"bpd-live": live, "bpd-stale": stale, "bpd-inflight": inFlight}),
	}}

	fresh := parkerProvEntry{
		DiskCID:  "rbd:vm-701-disk-0",
		ParkedAt: now.Format(time.RFC3339),
		Node:     "lab-pmx-0",
		Volid:    "rbd:vm-701-disk-0",
		Slot:     "scsi1",
	}
	if err := writeParkerProvenance(context.Background(), c, nil, "lab-pmx-0", 90472, "bpd-fresh", fresh, cfg); err != nil {
		t.Fatalf("write must succeed: %v", err)
	}

	got := provRecords(t, c.written)
	if _, ok := got["bpd-stale"]; ok {
		t.Error("a record older than the grace window whose volume nothing on the parker references must be pruned")
	}
	for _, key := range []string{"bpd-live", "bpd-inflight", "bpd-fresh"} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s must survive garbage collection", key)
		}
	}
}

// TestWriteParkerProvenance_KeepsUnreferencedRecordInsideGraceWindow is the
// crash-window guard: a detach-side transfer writes its intent record BEFORE
// the disk lands on the parker, so "nothing references it" is the normal
// state of a young record, not evidence that it is stale.
func TestWriteParkerProvenance_KeepsUnreferencedRecordInsideGraceWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cfg := provTestClock(now)

	intent := parkerProvEntry{
		DiskCID:  "rbd:vm-604-disk-0",
		ParkedAt: now.Add(-parkerProvenanceGraceWindow + time.Minute).Format(time.RFC3339),
		Node:     "lab-pmx-0",
		Volid:    "rbd:vm-604-disk-0",
		Slot:     "scsi3",
	}
	c := &provFakeClient{cfg: map[string]any{
		"tags":        ParkerTag,
		"description": provSentinel(t, map[string]parkerProvEntry{"bpd-intent": intent}),
	}}

	other := parkerProvEntry{ParkedAt: now.Format(time.RFC3339), Node: "lab-pmx-0"}
	if err := writeParkerProvenance(context.Background(), c, nil, "lab-pmx-0", 90472, "bpd-other", other, cfg); err != nil {
		t.Fatalf("write must succeed: %v", err)
	}
	if _, ok := provRecords(t, c.written)["bpd-intent"]; !ok {
		t.Fatal("an in-flight intent record inside the grace window must not be pruned")
	}
}

// TestWriteParkerProvenance_KeepsRecordsForDemotedVolumes covers the other
// reference shape: a volume sitting on an unusedN key is still on the parker,
// mid-unpark, and its record is still live.
func TestWriteParkerProvenance_KeepsRecordsForDemotedVolumes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cfg := provTestClock(now)

	demoted := parkerProvEntry{
		DiskCID:  "rbd:vm-90472-disk-2",
		ParkedAt: now.Add(-96 * time.Hour).Format(time.RFC3339),
		Node:     "lab-pmx-0",
		Volid:    "rbd:vm-90472-disk-2",
	}
	c := &provFakeClient{cfg: map[string]any{
		"unused0":     "rbd:vm-90472-disk-2",
		"tags":        ParkerTag,
		"description": provSentinel(t, map[string]parkerProvEntry{"bpd-demoted": demoted}),
	}}

	other := parkerProvEntry{ParkedAt: now.Format(time.RFC3339), Node: "lab-pmx-0"}
	if err := writeParkerProvenance(context.Background(), c, nil, "lab-pmx-0", 90472, "bpd-other", other, cfg); err != nil {
		t.Fatalf("write must succeed: %v", err)
	}
	if _, ok := provRecords(t, c.written)["bpd-demoted"]; !ok {
		t.Fatal("a record whose volume is on an unusedN key must not be pruned")
	}
}

// TestWriteParkerProvenance_PrunesLegacyVolidKeyedRecords proves the
// reference test reads the key as the volid for pre-stable-ID records, which
// omit the volid field entirely.
func TestWriteParkerProvenance_PrunesLegacyVolidKeyedRecords(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cfg := provTestClock(now)

	held := parkerProvEntry{DiskCID: "rbd:vm-90472-disk-0", ParkedAt: now.Add(-72 * time.Hour).Format(time.RFC3339), Node: "lab-pmx-0"}
	gone := parkerProvEntry{DiskCID: "rbd:vm-90472-disk-9", ParkedAt: now.Add(-72 * time.Hour).Format(time.RFC3339), Node: "lab-pmx-0"}

	c := &provFakeClient{cfg: map[string]any{
		"scsi0": "rbd:vm-90472-disk-0,size=10G",
		"tags":  ParkerTag,
		"description": provSentinel(t, map[string]parkerProvEntry{
			"rbd:vm-90472-disk-0": held,
			"rbd:vm-90472-disk-9": gone,
		}),
	}}

	other := parkerProvEntry{ParkedAt: now.Format(time.RFC3339), Node: "lab-pmx-0"}
	if err := writeParkerProvenance(context.Background(), c, nil, "lab-pmx-0", 90472, "bpd-other", other, cfg); err != nil {
		t.Fatalf("write must succeed: %v", err)
	}
	got := provRecords(t, c.written)
	if _, ok := got["rbd:vm-90472-disk-0"]; !ok {
		t.Error("a legacy record whose volid is on a bus slot must survive")
	}
	if _, ok := got["rbd:vm-90472-disk-9"]; ok {
		t.Error("a legacy record whose volid nothing references must be pruned")
	}
}

// ---------------------------------------------------------------------------
// Budget refusal
// ---------------------------------------------------------------------------

// TestWriteParkerProvenance_RefusesOverBudgetBeforeCallingPVE is the ceiling
// half of the report. A parker whose live records genuinely fill the store
// must be refused locally, with a capacity error the park loops can act on —
// not handed to PVE to reject with "value may only be 8192 characters long"
// after the caller has committed to this parker.
func TestWriteParkerProvenance_RefusesOverBudgetBeforeCallingPVE(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cfg := provTestClock(now)

	// Every record is referenced by a bus slot and therefore un-prunable.
	disks := make(map[string]parkerProvEntry, parkerMaxSlots)
	vmCfg := map[string]any{"tags": ParkerTag}
	for i := range parkerMaxSlots {
		volid := fmt.Sprintf("rbd:vm-90472-disk-%d", i)
		vmCfg[fmt.Sprintf("scsi%d", i)] = volid + ",size=100G"
		disks[fmt.Sprintf("bpd-%016x", i)] = parkerProvEntry{
			DiskCID:     "pvd-" + strings.Repeat("A", 180),
			SourceVMCID: fmt.Sprintf("vm-49e0b1c2-3f4a-4b5c-8d9e-%012d", i),
			ParkedAt:    now.Format(time.RFC3339),
			Node:        "lab-pmx-0",
			DirectorID:  "49e0b1c2-3f4a-4b5c-8d9e-000000000001",
			Volid:       volid,
			Slot:        fmt.Sprintf("scsi%d", i),
		}
	}
	vmCfg["description"] = provSentinel(t, disks)

	c := &provFakeClient{cfg: vmCfg}
	entry := parkerProvEntry{ParkedAt: now.Format(time.RFC3339), Node: "lab-pmx-0", Volid: "rbd:vm-701-disk-0", Slot: "scsi31"}
	err := writeParkerProvenance(context.Background(), c, nil, "lab-pmx-0", 90472, "bpd-new", entry, cfg)
	if err == nil {
		t.Fatal("a store that cannot fit the record must not report success")
	}
	if !errors.Is(err, ErrProvenanceFull) {
		t.Fatalf("want ErrProvenanceFull so the park loops can move to another parker, got %v", err)
	}
	if c.writes != 0 {
		t.Errorf("the refusal must happen before the config PUT, got %d writes", c.writes)
	}
}

// TestWriteParkerProvenance_GCMakesRoomBeforeRefusing proves the two halves
// compose: a store full of stale records is not a full store. This is the
// observed lab-pmx state, and the write must go through.
func TestWriteParkerProvenance_GCMakesRoomBeforeRefusing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cfg := provTestClock(now)

	disks := make(map[string]parkerProvEntry, parkerMaxSlots)
	for i := range parkerMaxSlots {
		disks[fmt.Sprintf("bpd-%016x", i)] = parkerProvEntry{
			DiskCID:     "pvd-" + strings.Repeat("A", 180),
			SourceVMCID: fmt.Sprintf("vm-49e0b1c2-3f4a-4b5c-8d9e-%012d", i),
			ParkedAt:    now.Add(-72 * time.Hour).Format(time.RFC3339),
			Node:        "lab-pmx-0",
			DirectorID:  "49e0b1c2-3f4a-4b5c-8d9e-000000000001",
			Volid:       fmt.Sprintf("rbd:vm-90472-disk-%d", i),
			Slot:        fmt.Sprintf("scsi%d", i),
		}
	}
	// Nothing on the parker references any of them.
	c := &provFakeClient{cfg: map[string]any{"tags": ParkerTag, "description": provSentinel(t, disks)}}

	entry := parkerProvEntry{ParkedAt: now.Format(time.RFC3339), Node: "lab-pmx-0", Volid: "rbd:vm-701-disk-0", Slot: "scsi0"}
	if err := writeParkerProvenance(context.Background(), c, nil, "lab-pmx-0", 90472, "bpd-new", entry, cfg); err != nil {
		t.Fatalf("collecting stale records must make room: %v", err)
	}
	got := provRecords(t, c.written)
	if len(got) != 1 {
		t.Fatalf("want only the new record left, got %d", len(got))
	}
	if _, ok := got["bpd-new"]; !ok {
		t.Fatal("the record being written must be the survivor")
	}
	if len(c.written) > parkerDescriptionBudget {
		t.Errorf("written description is %d bytes, over the %d budget", len(c.written), parkerDescriptionBudget)
	}
}

// TestWriteParkerProvenance_PreservesForeignSentinelKeys guards the codec
// contract through the new read-modify-write: collection touches only
// bosh_parked_disks.
func TestWriteParkerProvenance_PreservesForeignSentinelKeys(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cfg := provTestClock(now)

	desc := `operator note` + "\n" + `<!--BOSH:{"bosh_parked_disks":{"bpd-old":{"disk_cid":"rbd:vm-90472-disk-9","parked_at":"2026-08-21T12:00:00Z","node":"lab-pmx-0","volid":"rbd:vm-90472-disk-9"}},"bosh_attached_disks":{"keep":"me"}}-->`
	c := &provFakeClient{cfg: map[string]any{"tags": ParkerTag, "description": desc}}

	entry := parkerProvEntry{ParkedAt: now.Format(time.RFC3339), Node: "lab-pmx-0"}
	if err := writeParkerProvenance(context.Background(), c, nil, "lab-pmx-0", 90472, "bpd-new", entry, cfg); err != nil {
		t.Fatalf("write must succeed: %v", err)
	}
	nonBOSH, _, raw := parseParkerSentinel(c.written)
	if nonBOSH != "operator note" {
		t.Errorf("operator description text must survive, got %q", nonBOSH)
	}
	var attached map[string]string
	if err := json.Unmarshal(raw["bosh_attached_disks"], &attached); err != nil {
		t.Fatalf("foreign sentinel key must survive: %v", err)
	}
	if attached["keep"] != "me" {
		t.Errorf("foreign sentinel key must survive untouched, got %v", attached)
	}
}

// ---------------------------------------------------------------------------
// Capacity routing
// ---------------------------------------------------------------------------

// fullProvenanceDescription renders a sentinel of live, un-prunable records
// large enough that no further record fits under the budget. Every record is
// referenced by a bus slot in cfg, which the caller installs alongside it.
func fullProvenanceDescription(t *testing.T, parkerVMID int, cfg map[string]any, now time.Time) string {
	t.Helper()
	disks := make(map[string]parkerProvEntry, parkerMaxSlots)
	for i := range parkerMaxSlots {
		volid := fmt.Sprintf("data:vm-%d-disk-%d", parkerVMID, i)
		cfg[fmt.Sprintf("scsi%d", i)] = volid + ",size=100G"
		disks[fmt.Sprintf("bpd-%016x", i)] = parkerProvEntry{
			DiskCID:     "pvd-" + strings.Repeat("A", 180),
			SourceVMCID: fmt.Sprintf("vm-49e0b1c2-3f4a-4b5c-8d9e-%012d", i),
			ParkedAt:    now.Format(time.RFC3339),
			Node:        "pve1",
			DirectorID:  "49e0b1c2-3f4a-4b5c-8d9e-000000000001",
			Volid:       volid,
			Slot:        fmt.Sprintf("scsi%d", i),
		}
	}
	return provSentinel(t, disks)
}

// TestTransferDiskToParker_FullProvenanceStoreMovesToNextParker is the
// end-to-end shape of the lab-pmx failure, fixed: the first parker cannot
// record the intent, so the transfer moves to the next parker rather than
// failing the detach. The refusal happens at step 2, before the source slot
// is touched, so the disk is never at risk while the loop is choosing.
func TestTransferDiskToParker_FullProvenanceStoreMovesToNextParker(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	full := map[string]any{cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true}
	full["description"] = fullProvenanceDescription(t, 90000, full, now)

	c := newScanFakeClient(map[int]map[string]any{
		700:   {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
		90000: full,
		90001: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})

	cfg := transferTestCfg
	cfg.NowFunc = func() time.Time { return now }
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}

	landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", cfg, pctx)
	if err != nil {
		t.Fatalf("a full provenance store on one parker must not fail the transfer: %v", err)
	}
	if !strings.HasPrefix(landed, "data:vm-90001-disk-") {
		t.Fatalf("landed volid = %q, want the disk on parker 90001", landed)
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volumes destroyed while routing around a full store: %v", c.destroyed)
	}
	if _, ok := parseSentinelDisks(t, c, 90000)[transferStableID]; ok {
		t.Error("the full parker must carry no record for a disk it never received")
	}
	if _, ok := parseSentinelDisks(t, c, 90001)[transferStableID]; !ok {
		t.Error("the receiving parker must carry the finalized record")
	}
}

// parseSentinelDisks reads one parker's bosh_parked_disks map out of the fake.
func parseSentinelDisks(t *testing.T, c *scanFakeClient, vmid int) map[string]parkerProvEntry {
	t.Helper()
	desc, _ := c.configs[vmid]["description"].(string)
	_, disks, _ := parseParkerSentinel(desc)
	return disks
}

// TestIsParkerCapacityError_CoversBothExhaustionShapes pins the contract the
// park loops rely on: a parker is unusable for this disk either because every
// slot is taken or because its provenance store is full, and both mean "try
// another parker", never "fail the detach".
func TestIsParkerCapacityError_CoversBothExhaustionShapes(t *testing.T) {
	t.Parallel()

	if !isParkerCapacityError(fmt.Errorf("wrapped: %w", ErrNoSlots)) {
		t.Error("ErrNoSlots must read as a capacity condition")
	}
	if !isParkerCapacityError(fmt.Errorf("wrapped: %w", ErrProvenanceFull)) {
		t.Error("ErrProvenanceFull must read as a capacity condition")
	}
	if isParkerCapacityError(errors.New("connection refused")) {
		t.Error("an ordinary failure must not be mistaken for a capacity condition")
	}
	if isParkerCapacityError(nil) {
		t.Error("nil must not read as a capacity condition")
	}
}

// fullStoreFreeSlotsDescription fills a parker's provenance store past the
// budget while leaving bus slots free. This is the shape the lab found: parker
// 90472 held 24 of its 31 slots in 7964 bytes of description, so it could
// still take a disk it could no longer record.
func fullStoreFreeSlotsDescription(t *testing.T, parkerVMID int, cfg map[string]any, now time.Time) string {
	t.Helper()
	const records = 24
	disks := make(map[string]parkerProvEntry, records)
	for i := range records {
		volid := fmt.Sprintf("data:vm-%d-disk-%d", parkerVMID, i)
		cfg[fmt.Sprintf("scsi%d", i)] = volid + ",size=1G"
		disks[fmt.Sprintf("bpd-%016x", i)] = parkerProvEntry{
			DiskCID:     "pvd-" + strings.Repeat("A", 180),
			SourceVMCID: fmt.Sprintf("vm-49e0b1c2-3f4a-4b5c-8d9e-%012d", i),
			ParkedAt:    now.Format(time.RFC3339),
			Node:        "pve1",
			Volid:       volid,
			Slot:        fmt.Sprintf("scsi%d", i),
		}
	}
	desc := provSentinel(t, disks)
	if len(desc) <= parkerDescriptionBudget {
		t.Fatalf("fixture description is %d bytes, needs to exceed the %d budget", len(desc), parkerDescriptionBudget)
	}
	if records >= parkerMaxSlots {
		t.Fatalf("fixture must leave free slots, filled %d of %d", records, parkerMaxSlots)
	}
	return desc
}

// TestParkDisk_FullProvenanceStoreMovesToNextParker is the ordinary detach
// park, the half the transfer path does not cover. A parker with free slots
// and a full store can still take the disk, so nothing refuses it -- and the
// record that carries the disk's CID, its option overlay, and its source VM
// is dropped on the floor with a warning. Capacity is capacity: the park has
// to move to a parker that can hold both halves.
func TestParkDisk_FullProvenanceStoreMovesToNextParker(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	full := map[string]any{cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true}
	full["description"] = fullStoreFreeSlotsDescription(t, 90000, full, now)

	c := newScanFakeClient(map[int]map[string]any{
		90000: full,
		90001: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	})

	cfg := transferTestCfg
	cfg.NowFunc = func() time.Time { return now }
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}

	if err := ParkDisk(context.Background(), c, nil, "pve1", "data:vm-700-disk-1", cfg, pctx); err != nil {
		t.Fatalf("a full provenance store on one parker must not fail the park: %v", err)
	}

	if _, ok := parseSentinelDisks(t, c, 90000)[transferStableID]; ok {
		t.Error("the full parker must carry no record for a disk it never received")
	}
	if _, ok := parseSentinelDisks(t, c, 90001)[transferStableID]; !ok {
		t.Error("the receiving parker must carry the parked-disk record")
	}
	if slot := parkerSlotHolding(t, c, 90000, "data:vm-700-disk-1"); slot != "" {
		t.Errorf("the disk parked on the full parker at %s; a park it cannot record is a park it cannot take", slot)
	}
	if slot := parkerSlotHolding(t, c, 90001, "data:vm-700-disk-1"); slot == "" {
		t.Error("the disk must land on the parker that can hold both the volume and its record")
	}
}

// parkerSlotHolding reports the bus slot on vmid holding bareVolid, or "".
func parkerSlotHolding(t *testing.T, c *scanFakeClient, vmid int, bareVolid string) string {
	t.Helper()
	cfg, ok := c.configCopy(vmid)
	if !ok {
		return ""
	}
	for slot, entry := range qemu.ParseDisks(cfg) {
		if strings.SplitN(entry, ",", 2)[0] == bareVolid {
			return slot
		}
	}
	return ""
}
