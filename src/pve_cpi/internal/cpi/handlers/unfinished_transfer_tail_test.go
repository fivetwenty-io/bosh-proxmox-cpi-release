package handlers

import (
	"context"
	"sort"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// metadataHolders lists the VMs whose description carries a disk metadata
// record.
func (s *strandedDisk) metadataHolders() []int {
	var out []int
	for vmid, cfg := range s.client.state.configs {
		if strings.Contains(pve.DescriptionFromConfig(cfg), sentinelKeyDiskMetadata) {
			out = append(out, vmid)
		}
	}
	sort.Ints(out)
	return out
}

// TestUnfinishedTransferTailFailureStopsTheHandler covers a journal-managed
// disk whose park a snapshot deferred, so its transfer to a parker is still
// unfinished, and whose source VM changes before every write the tail sends.
// resize_disk, snapshot_disk, and set_disk_metadata each resume the transfer
// inside the disk's lifecycle, and the tail gives up after three refused
// writes. Each handler returns a retriable error before it resizes,
// snapshots, or writes metadata. The disk stays landed on the parker, 777
// keeps its entry, and no write is left planned. Once 777 settles down,
// delete_disk removes 777's entry and the volume.
func TestUnfinishedTransferTailFailureStopsTheHandler(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(t *testing.T, s *strandedDisk) error
		acts func(s *strandedDisk) int
	}{
		{"resize_disk", birthNameCall(t, "resize_disk"), func(s *strandedDisk) int { return s.client.resizeCalls }},
		{"snapshot_disk", birthNameCall(t, "snapshot_disk"), func(s *strandedDisk) int { return len(s.client.snapshots) }},
		{"set_disk_metadata", birthNameCall(t, "set_disk_metadata"), func(s *strandedDisk) int { return len(s.metadataHolders()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id, w := buildTailDisk(t)
			s.deferBySnapshot(t, id)
			if s.intentParker() == 0 {
				t.Fatal("the deferred park left no transfer record")
			}
			w.conflictAlways = true
			err := tc.call(t, s)
			requireRetriable(t, err, tc.name+" whose tail never finishes")
			if w.conflicts != 3 || w.removals != 0 {
				t.Fatalf("conflicts=%d removals=%d, want three refused writes and no removal", w.conflicts, w.removals)
			}
			if acted := tc.acts(s); acted != 0 {
				t.Fatalf("%s acted %d times although its tail failed", tc.name, acted)
			}
			if !s.hasEntry(777) {
				t.Fatal("777's entry went missing although every write was refused")
			}
			if planned, _ := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); planned != 0 {
				t.Fatalf("%d refused writes were left planned, want each settled", planned)
			}
			s.requireParkedUnderItsOwnName(t)
			w.conflictAlways = false
			if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
				t.Fatalf("delete_disk once 777 settles down: %v", err)
			}
			s.requireRecord(t, id, aj.Deleted)
			s.requireSourceClean(t)
		})
	}
}

// TestUnfinishedTransferSetDiskMetadataRunsTheTail is the same deferred park
// with a source VM that takes the tail's write. set_disk_metadata resumes the
// transfer inside the disk's lifecycle, so the tail removes 777's entry under
// the journal's lock and the record goes back ready to return. The disk then
// sits on a parker, which the hosting scan skips, and 777 no longer names it,
// so the handler writes no description at all and can't put back what the
// tail removed.
func TestUnfinishedTransferSetDiskMetadataRunsTheTail(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildTailDisk(t)
	s.deferBySnapshot(t, id)
	if s.intentParker() == 0 {
		t.Fatal("the deferred park left no transfer record")
	}
	if err := birthNameCall(t, "set_disk_metadata")(t, s); err != nil {
		t.Fatalf("set_disk_metadata: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
	s.requireSourceClean(t)
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once", w.removals)
	}
	if _, observed := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); observed == 0 {
		t.Fatal("the tail's write left no step in the disk's lifecycle")
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
	if holders := s.metadataHolders(); len(holders) != 0 {
		t.Fatalf("the metadata sits on %v, want no VM written for a parked disk", holders)
	}
}
