package handlers

import "testing"

// TestDevicePathForSCSIRank_SingleDiskAtSCSI1 covers the typical case after
// scsi0 reservation: one persistent disk attached at scsi1 with a virtio root.
// Linux enumerates the single scsi device as /dev/sda regardless of slot ID.
func TestDevicePathForSCSIRank_SingleDiskAtSCSI1(t *testing.T) {
	t.Parallel()
	cfg := map[string]interface{}{
		"virtio0": "data:vm-100-disk-0",
		"scsi1":   "data:vm-9001-disk-0",
	}
	got, err := devicePathForSCSIRank(cfg, "scsi1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/dev/sda" {
		t.Errorf("got %q, want /dev/sda", got)
	}
}

// TestDevicePathForSCSIRank_TwoDisksRankByOrder verifies the rank-based
// formula: with scsi1 and scsi2 both attached, scsi1 → /dev/sda and
// scsi2 → /dev/sdb. The slot index would have given /dev/sdb and /dev/sdc.
func TestDevicePathForSCSIRank_TwoDisksRankByOrder(t *testing.T) {
	t.Parallel()
	cfg := map[string]interface{}{
		"scsi1": "data:a",
		"scsi2": "data:b",
	}
	for _, c := range []struct {
		diskID, want string
	}{
		{"scsi1", "/dev/sda"},
		{"scsi2", "/dev/sdb"},
	} {
		got, err := devicePathForSCSIRank(cfg, c.diskID)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.diskID, err)
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.diskID, got, c.want)
		}
	}
}

// TestDevicePathForSCSIRank_SparseSlots covers the case where slot numbers
// are not contiguous (e.g. scsi1 detached, scsi3 still attached). Rank of
// scsi3 in [3] is 0 → /dev/sda. The slot-based formula would have given /dev/sdd.
func TestDevicePathForSCSIRank_SparseSlots(t *testing.T) {
	t.Parallel()
	cfg := map[string]interface{}{
		"scsi3": "data:c",
	}
	got, err := devicePathForSCSIRank(cfg, "scsi3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/dev/sda" {
		t.Errorf("got %q, want /dev/sda", got)
	}
}

// TestDevicePathForSCSIRank_CdromExcluded verifies that media=cdrom entries
// do not consume an sd-letter (they become /dev/sr* in the guest). With
// scsi1 (disk) and scsi30 (cdrom), the disk at scsi1 ranks 0 → /dev/sda.
func TestDevicePathForSCSIRank_CdromExcluded(t *testing.T) {
	t.Parallel()
	cfg := map[string]interface{}{
		"scsi1":  "data:vm-9001-disk-0",
		"scsi30": "local:iso/cloud-init.iso,media=cdrom,size=10M",
	}
	got, err := devicePathForSCSIRank(cfg, "scsi1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/dev/sda" {
		t.Errorf("got %q, want /dev/sda (cdrom must not consume sd-letter)", got)
	}
}

// TestDevicePathForSCSIRank_DiskIDNotPresent verifies an error when diskID
// is not in cfg (race between AttachDisk and the path lookup).
func TestDevicePathForSCSIRank_DiskIDNotPresent(t *testing.T) {
	t.Parallel()
	cfg := map[string]interface{}{
		"scsi1": "data:a",
	}
	_, err := devicePathForSCSIRank(cfg, "scsi2")
	if err == nil {
		t.Error("expected error when diskID is absent from cfg")
	}
}

// TestDevicePathForSCSIRank_NonSCSIDiskID verifies an error when diskID is
// not a scsi slot (e.g. virtio0). The handler only routes scsi attachments
// to this helper, but the guard prevents silent misuse.
func TestDevicePathForSCSIRank_NonSCSIDiskID(t *testing.T) {
	t.Parallel()
	_, err := devicePathForSCSIRank(map[string]interface{}{"virtio0": "data:r"}, "virtio0")
	if err == nil {
		t.Error("expected error for non-scsi diskID")
	}
}
