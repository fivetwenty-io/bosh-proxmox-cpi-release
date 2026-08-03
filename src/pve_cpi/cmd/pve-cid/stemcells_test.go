package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func templateCfg(t *testing.T, prov templateProvenance) map[string]any {
	t.Helper()
	b, err := json.Marshal(prov)
	if err != nil {
		t.Fatalf("marshal provenance: %v", err)
	}
	return map[string]any{"description": string(b)}
}

// TestBuildStemcellInventory_CorrelatedNonOrphan covers the healthy case: a
// template with a live director ref, correlated against its backing file.
func TestBuildStemcellInventory_CorrelatedNonOrphan(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 6042, Node: "pve1", Name: "bosh-stemcell-ubuntu-jammy-1.719-cafebabe",
				Tags: "bosh-stemcell;bosh-stemcell-name-ubuntu-jammy;bosh-stemcell-version-1.719;bosh-stemcell-sha-cafebabe"},
		},
		configs: map[string]map[string]any{
			"pve1/6042": templateCfg(t, templateProvenance{
				Name: "ubuntu-jammy", Version: "1.719", SHA8: "cafebabe", Kind: "heavy",
				CID:     ":heavy:local:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{"dir-1"},
			}),
		},
		content: map[string][]StorageContentItem{
			"pve1|local": {{VolID: "local:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2", Size: 123}},
		},
	}

	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "local")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.SHA8 != "cafebabe" {
		t.Errorf("SHA8 = %q, want %q", e.SHA8, "cafebabe")
	}
	if len(e.Templates) != 1 || len(e.Files) != 1 {
		t.Fatalf("Templates=%d Files=%d, want 1/1", len(e.Templates), len(e.Files))
	}
	if e.Orphan {
		t.Errorf("expected non-orphan, reasons=%v", e.OrphanReasons)
	}
}

// TestBuildStemcellInventory_ZeroRefsOrphan covers a template with zero
// director references.
func TestBuildStemcellInventory_ZeroRefsOrphan(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 7000, Node: "pve1", Name: "bosh-stemcell-x-1-deadbeef",
				Tags: "bosh-stemcell;bosh-stemcell-sha-deadbeef"},
		},
		configs: map[string]map[string]any{
			"pve1/7000": templateCfg(t, templateProvenance{
				Name: "x", Version: "1", SHA8: "deadbeef", Kind: "heavy",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{},
			}),
		},
		content: map[string][]StorageContentItem{
			"pve1|local": {{VolID: "local:import/bosh-stemcell-x-1-deadbeef.qcow2"}},
		},
	}

	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "local")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].Orphan {
		t.Fatal("expected orphan=true for a template with zero director refs")
	}
	if !containsSubstring(entries[0].OrphanReasons, "zero director references") {
		t.Errorf("OrphanReasons = %v, missing zero-refs reason", entries[0].OrphanReasons)
	}
}

// TestBuildStemcellInventory_UncorrelatedFileOrphan covers a qcow2 file with
// no matching cache template.
func TestBuildStemcellInventory_UncorrelatedFileOrphan(t *testing.T) {
	r := &fakeReader{
		content: map[string][]StorageContentItem{
			"pve1|local": {{VolID: "local:import/bosh-stemcell-orphan-1-ffffffff.qcow2"}},
		},
	}

	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "local")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if len(e.Templates) != 0 || len(e.Files) != 1 {
		t.Fatalf("Templates=%d Files=%d, want 0/1", len(e.Templates), len(e.Files))
	}
	if !e.Orphan {
		t.Fatal("expected orphan=true for an uncorrelated file")
	}
	if !containsSubstring(e.OrphanReasons, "kind (light/heavy) cannot be determined") {
		t.Errorf("OrphanReasons = %v, missing uncorrelated-file reason", e.OrphanReasons)
	}
}

// TestBuildStemcellInventory_MissingBackingFileOrphan covers a template
// whose backing qcow2 is gone from the scanned storage.
func TestBuildStemcellInventory_MissingBackingFileOrphan(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 8000, Node: "pve1", Name: "bosh-stemcell-y-1-11111111",
				Tags: "bosh-stemcell;bosh-stemcell-sha-11111111"},
		},
		configs: map[string]map[string]any{
			"pve1/8000": templateCfg(t, templateProvenance{
				Name: "y", Version: "1", SHA8: "11111111", Kind: "light",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{"dir-9"},
			}),
		},
		content: map[string][]StorageContentItem{
			"pve1|local": {}, // backing file absent
		},
	}

	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "local")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].Orphan {
		t.Fatal("expected orphan=true for a template with a missing backing file")
	}
	if !containsSubstring(entries[0].OrphanReasons, "backing qcow2 file not found") {
		t.Errorf("OrphanReasons = %v, missing backing-file reason", entries[0].OrphanReasons)
	}
}

// TestBuildStemcellInventory_LocalStorageReplicaNotFalseOrphan covers a
// node-local storage scan: a replica's backing qcow2 living on its own node
// (not the single node an old single-node scan would have checked) must not
// be reported as a missing-backing orphan.
func TestBuildStemcellInventory_LocalStorageReplicaNotFalseOrphan(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 6042, Node: "pve1", Name: "bosh-stemcell-ubuntu-jammy-1.719-cafebabe",
				Tags: "bosh-stemcell;bosh-stemcell-name-ubuntu-jammy;bosh-stemcell-version-1.719;bosh-stemcell-sha-cafebabe"},
			{VMID: 6043, Node: "pve2", Name: "bosh-stemcell-ubuntu-jammy-1.719-cafebabe",
				Tags: "bosh-stemcell;bosh-stemcell-name-ubuntu-jammy;bosh-stemcell-version-1.719;bosh-stemcell-sha-cafebabe"},
		},
		configs: map[string]map[string]any{
			"pve1/6042": templateCfg(t, templateProvenance{
				Name: "ubuntu-jammy", Version: "1.719", SHA8: "cafebabe", Kind: "heavy",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{"dir-1"},
			}),
			"pve2/6043": templateCfg(t, templateProvenance{
				Name: "ubuntu-jammy", Version: "1.719", SHA8: "cafebabe", Kind: "heavy",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{"dir-1"},
			}),
		},
		content: map[string][]StorageContentItem{
			// Each node reports only its own local copy — the defining
			// property of node-local storage that made the old single-node
			// scan misclassify every non-scanned-node replica as orphaned.
			"pve1|local": {{VolID: "local:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2", Size: 123}},
			"pve2|local": {{VolID: "local:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2", Size: 123}},
		},
		nodes: []string{"pve1", "pve2"},
		storageSharedKnown: map[string]bool{
			"local": true,
		},
		storageShared: map[string]bool{
			"local": false,
		},
	}

	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "local")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if len(e.Files) != 2 {
		t.Fatalf("expected 2 files (one per node), got %d: %+v", len(e.Files), e.Files)
	}
	if e.Orphan {
		t.Errorf("expected non-orphan (both replicas' backing files found on their own nodes), reasons=%v", e.OrphanReasons)
	}
}

// TestBuildStemcellInventory_LocalStorageTrulyMissingBackingIsOrphan covers
// the other side of node-local scanning: a template on node-local storage
// whose own node genuinely lacks the backing file is still reported as an
// orphan candidate, even though a different node happens to hold a file of
// the same sha8 (a stale/foreign copy must not mask a genuinely missing one).
func TestBuildStemcellInventory_LocalStorageTrulyMissingBackingIsOrphan(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 7042, Node: "pve1", Name: "bosh-stemcell-x-1-deadbeef",
				Tags: "bosh-stemcell;bosh-stemcell-sha-deadbeef"},
		},
		configs: map[string]map[string]any{
			"pve1/7042": templateCfg(t, templateProvenance{
				Name: "x", Version: "1", SHA8: "deadbeef", Kind: "heavy",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{"dir-1"},
			}),
		},
		content: map[string][]StorageContentItem{
			"pve1|local": {}, // pve1's own copy is genuinely gone
			"pve2|local": {{VolID: "local:import/bosh-stemcell-x-1-deadbeef.qcow2"}},
		},
		nodes: []string{"pve1", "pve2"},
		storageSharedKnown: map[string]bool{
			"local": true,
		},
		storageShared: map[string]bool{
			"local": false,
		},
	}

	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "local")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].Orphan {
		t.Fatal("expected orphan=true: pve1's own copy of the backing file is genuinely absent")
	}
	if !containsSubstring(entries[0].OrphanReasons, "not found on that node's copy") {
		t.Errorf("OrphanReasons = %v, missing node-scoped not-found reason", entries[0].OrphanReasons)
	}
}

// TestBuildStemcellInventory_SharedStorageSingleNodeScanSuffices covers the
// shared-storage path: only one content listing call is issued (even though
// multiple cluster nodes exist), and a file found via that single scan
// still proves the backing exists.
func TestBuildStemcellInventory_SharedStorageSingleNodeScanSuffices(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 6042, Node: "pve1", Name: "bosh-stemcell-x-1-cafebabe",
				Tags: "bosh-stemcell;bosh-stemcell-sha-cafebabe"},
		},
		configs: map[string]map[string]any{
			"pve1/6042": templateCfg(t, templateProvenance{
				Name: "x", Version: "1", SHA8: "cafebabe", Kind: "heavy",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{"dir-1"},
			}),
		},
		content: map[string][]StorageContentItem{
			"pve1|cephfs": {{VolID: "cephfs:import/bosh-stemcell-x-1-cafebabe.qcow2"}},
			// No "pve2|cephfs" entry at all: if buildStemcellInventory scanned
			// pve2 for a shared storage it would get zero content back from
			// the fake (an unconfigured map key), which would prove the scan
			// scope leaked beyond node — the assertion below on Files count
			// (not "no false orphan") is what catches that.
		},
		nodes: []string{"pve1", "pve2"},
		storageSharedKnown: map[string]bool{
			"cephfs": true,
		},
		storageShared: map[string]bool{
			"cephfs": true,
		},
	}

	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "cephfs")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if len(entries[0].Files) != 1 {
		t.Fatalf("expected exactly 1 file from the single shared-storage scan, got %d", len(entries[0].Files))
	}
	if entries[0].Orphan {
		t.Errorf("expected non-orphan, reasons=%v", entries[0].OrphanReasons)
	}
}

// TestBuildStemcellInventory_UntaggedTemplatesGetSeparateEntries covers
// grouping: two unrelated templates that both carry no sha tag must not
// collapse into one SHA8=="" group.
func TestBuildStemcellInventory_UntaggedTemplatesGetSeparateEntries(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 9001, Node: "pve1", Name: "bosh-stemcell-a-1-00000000", Tags: "bosh-stemcell"},
			{VMID: 9002, Node: "pve1", Name: "bosh-stemcell-b-1-00000000", Tags: "bosh-stemcell"},
		},
		configs: map[string]map[string]any{
			"pve1/9001": templateCfg(t, templateProvenance{
				Name: "a", Version: "1", Kind: "heavy",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{"dir-1"},
			}),
			"pve1/9002": templateCfg(t, templateProvenance{
				Name: "b", Version: "1", Kind: "heavy",
				Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{"dir-2"},
			}),
		},
	}

	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "local")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 separate entries for 2 unrelated untagged templates, got %d: %+v", len(entries), entries)
	}
	seenVMIDs := map[int]bool{}
	for _, e := range entries {
		if !e.Untagged {
			t.Errorf("entry %+v: expected Untagged=true", e)
		}
		if len(e.Templates) != 1 {
			t.Fatalf("entry %+v: expected exactly 1 template per untagged entry", e)
		}
		seenVMIDs[e.Templates[0].VMID] = true
		if !containsSubstring(e.OrphanReasons, "no sha tag") {
			t.Errorf("entry %+v: expected an untagged orphan reason", e)
		}
	}
	if !seenVMIDs[9001] || !seenVMIDs[9002] {
		t.Errorf("expected entries for both VMIDs 9001 and 9002, seen=%v", seenVMIDs)
	}
}

func TestBuildStemcellInventory_IgnoresNonStemcellTemplatesAndFiles(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 9000, Node: "pve1", Name: "some-other-template", Tags: "not-a-stemcell"},
		},
		content: map[string][]StorageContentItem{
			"pve1|local": {{VolID: "local:import/not-a-stemcell-file.qcow2"}},
		},
	}
	entries, err := buildStemcellInventory(context.Background(), r, "pve1", "local")
	if err != nil {
		t.Fatalf("buildStemcellInventory error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected zero entries, got %d: %+v", len(entries), entries)
	}
}

func TestFilterOrphans(t *testing.T) {
	entries := []StemcellInventoryEntry{
		{SHA8: "a", Orphan: false},
		{SHA8: "b", Orphan: true, OrphanReasons: []string{"x"}},
	}
	got := filterOrphans(entries)
	if len(got) != 1 || got[0].SHA8 != "b" {
		t.Errorf("filterOrphans = %+v", got)
	}
}

func containsSubstring(items []string, substr string) bool {
	for _, item := range items {
		if strings.Contains(item, substr) {
			return true
		}
	}
	return false
}
