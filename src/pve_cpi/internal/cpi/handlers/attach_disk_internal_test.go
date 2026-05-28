package handlers

import "testing"

// TestDevicePathByID_BasicSlots verifies that diskID "scsi<N>" maps to the
// PVE-stable udev by-id symlink path. PVE configures virtio-scsi-pci disks
// with serial "drive-scsi<N>" and udev creates a matching by-id link.
func TestDevicePathByID_BasicSlots(t *testing.T) {
	t.Parallel()

	cases := []struct {
		diskID, want string
	}{
		{"scsi0", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0"},
		{"scsi1", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"},
		{"scsi3", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi3"},
		{"scsi30", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi30"},
	}

	for _, c := range cases {
		t.Run(c.diskID, func(t *testing.T) {
			t.Parallel()
			got, err := devicePathByID(c.diskID)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestDevicePathByID_NonSCSIRejected verifies that non-scsi diskIDs cause
// an explicit error. attach_disk asserts the bus is "scsi" before calling
// this helper, but the guard prevents silent misuse if that contract
// changes.
func TestDevicePathByID_NonSCSIRejected(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, diskID string }{
		{"virtio", "virtio0"},
		{"ide", "ide0"},
		{"sata", "sata0"},
		{"empty", ""},
		{"garbage", "garbage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := devicePathByID(c.diskID); err == nil {
				t.Errorf("devicePathByID(%q) succeeded, expected error", c.diskID)
			}
		})
	}
}
