package pve_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestIsTrimCapable_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		storageType string
		format      string
		want        bool
	}{
		{"lvmthin_any_format", "lvmthin", "raw", true},
		{"lvmthin_empty_format", "lvmthin", "", true},
		{"zfspool_any_format", "zfspool", "raw", true},
		{"rbd_any_format", "rbd", "", true},
		{"lvm_thick_never", "lvm", "raw", false},
		{"dir_qcow2", "dir", "qcow2", true},
		{"dir_raw", "dir", "raw", false},
		{"dir_empty_format", "dir", "", false},
		{"nfs_qcow2", "nfs", "qcow2", true},
		{"nfs_raw", "nfs", "raw", false},
		{"cifs_qcow2", "cifs", "qcow2", true},
		{"cifs_vmdk", "cifs", "vmdk", false},
		{"cephfs_qcow2_not_trim", "cephfs", "qcow2", false},
		{"glusterfs_qcow2_not_trim", "glusterfs", "qcow2", false},
		{"pbs_not_trim", "pbs", "qcow2", false},
		{"unknown_type", "made-up-backend", "qcow2", false},
		{"empty_storage_type", "", "qcow2", false},
		{"mixed_case_lvmthin", "LVMThin", "raw", true},
		{"mixed_case_dir_qcow2", "Dir", "QCOW2", true},
		{"whitespace_padded", "  dir  ", "  qcow2  ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pve.IsTrimCapable(tc.storageType, tc.format)
			if got != tc.want {
				t.Errorf("IsTrimCapable(%q, %q) = %v; want %v", tc.storageType, tc.format, got, tc.want)
			}
		})
	}
}
