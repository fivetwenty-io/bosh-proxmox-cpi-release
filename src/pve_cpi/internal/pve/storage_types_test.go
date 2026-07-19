package pve

import "testing"

func TestStorageUsesFileVolumes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		storageType string
		want        bool
	}{
		{StorageTypeDir, true},
		{StorageTypeNFS, true},
		{StorageTypeCIFS, true},
		{StorageTypeGlusterFS, true},
		{StorageTypeBTRFS, true},
		{"NFS", true},
		{" nfs ", true},
		{StorageTypeLVM, false},
		{StorageTypeLVMThin, false},
		{StorageTypeZFSPool, false},
		{StorageTypeRBD, false},
		{StorageTypeCephFS, false},
		{StorageTypePBS, false},
		{"", false},
		{"unknown-plugin", false},
	}
	for _, tc := range cases {
		if got := StorageUsesFileVolumes(tc.storageType); got != tc.want {
			t.Errorf("StorageUsesFileVolumes(%q) = %v; want %v", tc.storageType, got, tc.want)
		}
	}
}
