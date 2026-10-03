package pve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func lifecycleEntry() DiskAllocationProvenance {
	return DiskAllocationProvenance{Version: 1, AllocationID: "01234567-89ab-4def-8123-456789abcdef", AllocationNamespace: "director-one", Volid: "pool:100/vm-100-disk-0.raw", Node: "node-a", Backing: "dir:/mnt/pool"}
}
func TestDiskAllocationLifecyclePreservesSharedMetadata(t *testing.T) {
	entry := lifecycleEntry()
	desc := `before <!--BOSH:{"bosh_attached_disks":{"token":"encoded-cid"}}--> after-allocation-marker`
	c := newScanFakeClient(map[int]map[string]any{100: {"description": desc}})
	if err := WriteDiskAllocationProvenance(context.Background(), c, "node-a", 100, "token", entry); err != nil {
		t.Fatal(err)
	}
	got := c.configs[100]["description"].(string)
	if !strings.Contains(got, "before") || !strings.Contains(got, "after-allocation-marker") {
		t.Fatalf("lost surrounding provenance: %s", got)
	}
	_, raw, err := strictDiskAllocationSentinel(got)
	if err != nil {
		t.Fatal(err)
	}
	var attached map[string]string
	if err := json.Unmarshal(raw["bosh_attached_disks"], &attached); err != nil || attached["token"] != "encoded-cid" {
		t.Fatalf("legacy mapping changed: %v %v", attached, err)
	}
	found, ok, err := FindDiskAllocationProvenance(got, "token")
	if err != nil || !ok || found != entry {
		t.Fatalf("provenance=%+v found=%v err=%v", found, ok, err)
	}
	read := func() map[string]any {
		return map[string]any{"description": c.configs[100]["description"], "digest": "d1"}
	}
	foreign := entry
	foreign.AllocationNamespace = "another"
	if err := RemoveDiskAllocationEntry(context.Background(), c, "node-a", 100, "token", foreign, read(), nil, nil); err == nil {
		t.Fatal("foreign identity erased")
	}
	stale := entry
	stale.Volid = "pool:200/vm-200-disk-0.raw"
	if err := RemoveDiskAllocationEntry(context.Background(), c, "node-a", 100, "token", stale, read(), nil, nil); err == nil {
		t.Fatal("stale location erased current provenance")
	}
	if err := RemoveDiskAllocationEntry(context.Background(), c, "node-a", 100, "token", entry, read(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FindDiskAllocationProvenance(c.configs[100]["description"].(string), "token"); err != nil || ok {
		t.Fatalf("removed identity remains: %v %v", ok, err)
	}
}
func TestDiskAllocationLifecycleRejectsAmbiguousEvidence(t *testing.T) {
	entry := lifecycleEntry()
	b, _ := json.Marshal(entry)
	valid := `<!--BOSH:{"bosh_disk_allocations":{"token":` + string(b) + `}}-->`
	cases := []string{
		`<!--BOSH:broken-->`,
		valid + valid,
		`<!--BOSH:{"bosh_disk_allocations":{},"bosh_disk_allocations":{}}-->`,
		strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, `"node":"node-a"`, `"node":""`, 1),
		strings.Replace(valid, `"backing":"dir:/mnt/pool"`, `"backing":""`, 1),
	}
	for _, desc := range cases {
		if _, err := ParseDiskAllocationProvenance(desc); err == nil {
			t.Fatalf("accepted malformed identity: %s", desc)
		}
	}
}

// TestDiskAllocationProvenanceRefusesANodeLongerThanPVEAllows pins the bound
// on the provenance node. PVE names a node for its host, and a hostname label
// holds at most 63 bytes, so a longer node is malformed in a current entry
// and in a parked one.
func TestDiskAllocationProvenanceRefusesANodeLongerThanPVEAllows(t *testing.T) {
	longest := lifecycleEntry()
	longest.Node = strings.Repeat("n", 63)
	b, _ := json.Marshal(longest)
	if _, err := ParseDiskAllocationProvenance(`<!--BOSH:{"bosh_disk_allocations":{"token":` + string(b) + `}}-->`); err != nil {
		t.Fatalf("63-byte node refused: %v", err)
	}
	overlong := lifecycleEntry()
	overlong.Node = strings.Repeat("n", 64)
	b, _ = json.Marshal(overlong)
	if _, err := ParseDiskAllocationProvenance(`<!--BOSH:{"bosh_disk_allocations":{"token":` + string(b) + `}}-->`); err == nil || err.Error() != "invalid managed disk provenance" {
		t.Fatalf("64-byte node: err = %v, want invalid managed disk provenance", err)
	}
	if err := WriteDiskAllocationProvenance(context.Background(), newScanFakeClient(map[int]map[string]any{100: {"description": ""}}), "node-a", 100, "token", overlong); err == nil {
		t.Fatal("a 64-byte node was written")
	}
	parked, _ := json.Marshal(map[string]any{"token": map[string]any{"allocation_id": overlong.AllocationID, "allocation_namespace": overlong.AllocationNamespace, "allocation_backing": overlong.Backing, "volid": overlong.Volid, "node": overlong.Node}})
	if _, _, err := FindDiskAllocationProvenance(`<!--BOSH:{"bosh_parked_disks":`+string(parked)+`}-->`, "token"); err == nil || err.Error() != "malformed parked allocation provenance" {
		t.Fatalf("parked 64-byte node: err = %v, want malformed parked allocation provenance", err)
	}
}

func TestDiskAllocationLifecycleLegacyAttachedMapIsUnmanaged(t *testing.T) {
	_, ok, err := FindDiskAllocationProvenance(`<!--BOSH:{"bosh_attached_disks":{"token":"cid"}}-->`, "token")
	if err != nil || ok {
		t.Fatalf("legacy map treated as managed: %v %v", ok, err)
	}
}

func TestManagedTransferFinalizationCannotLoseProvenance(t *testing.T) {
	c := newScanFakeClient(map[int]map[string]any{100: {}})
	c.configErr = map[int]error{100: errors.New("injected unavailable config")}
	intent := DiskTransferIntent{ParkerNode: "node-a", ParkerVMID: 100}
	ctx := context.Background()
	entry := lifecycleEntry()
	managed := ParkContext{AllocationID: entry.AllocationID, AllocationNamespace: entry.AllocationNamespace, AllocationBacking: entry.Backing}
	if err := finalizeResumedTransfer(ctx, c, nil, intent, "token", "scsi1", entry.Volid, ParkerConfig{}, managed); err == nil {
		t.Fatal("managed finalize accepted missing provenance")
	}
	if err := finalizeResumedTransfer(ctx, c, nil, intent, "token", "scsi1", entry.Volid, ParkerConfig{}, ParkContext{}); err != nil {
		t.Fatalf("legacy best-effort changed: %v", err)
	}
}
