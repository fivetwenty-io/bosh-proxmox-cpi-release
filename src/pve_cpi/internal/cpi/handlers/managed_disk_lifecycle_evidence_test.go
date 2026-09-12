package handlers

import "testing"

func TestManagedAttachedVolumeRequiresExactIdentity(t *testing.T) {
	const volume = "pool:100/vm-100-disk-0.raw"
	const token = "bpd-0123456789abcdef"
	cases := []struct {
		name, drive, slot, wantVolume, wantToken string
		fail                                     bool
	}{
		{"exact", volume + ",serial=" + token, "scsi1", volume, token, false},
		{"wrong volume", volume + "2,serial=" + token, "scsi1", volume, token, true},
		{"wrong slot", volume + ",serial=" + token, "scsi2", volume, token, true},
		{"missing serial", volume, "scsi1", volume, token, true},
		{"wrong serial", volume + ",serial=bpd-fedcba9876543210", "scsi1", volume, token, true},
		{"unserialized VM root", volume, "scsi1", volume, "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := managedAttachedVolume(map[string]any{"scsi1": tt.drive}, tt.slot, tt.wantVolume, tt.wantToken)
			if (err != nil) != tt.fail {
				t.Fatalf("error=%v expected=%v", err, tt.fail)
			}
		})
	}
}
