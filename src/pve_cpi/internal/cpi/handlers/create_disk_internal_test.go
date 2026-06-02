package handlers

import (
	"strings"
	"testing"
)

// TestResolveStorage verifies the three-level precedence chain for storage
// pool selection in create_disk:
//
//  1. cloud_properties.storage_pool  (highest)
//  2. cloud_properties.storage       (backward-compat alias)
//  3. config.DiskStorage             (global default / lowest)
//
// Empty or whitespace-only values at any level are treated as unset and the
// next level is consulted. All three levels empty → error.
func TestResolveStorage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		storagePool      string // cloud_properties.storage_pool
		storage          string // cloud_properties.storage (alias)
		configDiskStorage string // config.DiskStorage
		wantStorage      string // expected resolved pool name; "" means expect error
		wantErr          bool
	}{
		{
			name:             "storage_pool wins over storage alias",
			storagePool:      "ceph-pool",
			storage:          "local-lvm",
			configDiskStorage: "config-default",
			wantStorage:      "ceph-pool",
		},
		{
			name:             "storage_pool wins over config default",
			storagePool:      "ceph-pool",
			storage:          "",
			configDiskStorage: "config-default",
			wantStorage:      "ceph-pool",
		},
		{
			name:             "storage alias used when storage_pool empty",
			storagePool:      "",
			storage:          "local-lvm",
			configDiskStorage: "config-default",
			wantStorage:      "local-lvm",
		},
		{
			name:             "storage alias wins over config default",
			storagePool:      "",
			storage:          "nfs-store",
			configDiskStorage: "config-default",
			wantStorage:      "nfs-store",
		},
		{
			name:             "config default used when both cloud props empty",
			storagePool:      "",
			storage:          "",
			configDiskStorage: "config-default",
			wantStorage:      "config-default",
		},
		{
			name:             "all three empty returns error",
			storagePool:      "",
			storage:          "",
			configDiskStorage: "",
			wantErr:          true,
		},
		{
			name:             "whitespace storage_pool treated as unset, falls through to alias",
			storagePool:      "   ",
			storage:          "local-lvm",
			configDiskStorage: "config-default",
			wantStorage:      "local-lvm",
		},
		{
			name:             "whitespace storage alias treated as unset, falls through to config",
			storagePool:      "",
			storage:          "\t  \n",
			configDiskStorage: "config-default",
			wantStorage:      "config-default",
		},
		{
			name:             "whitespace config default with non-empty alias returns alias",
			storagePool:      "",
			storage:          "local-lvm",
			configDiskStorage: "  ",
			wantStorage:      "local-lvm",
		},
		{
			name:             "all whitespace returns error",
			storagePool:      " ",
			storage:          " ",
			configDiskStorage: " ",
			wantErr:          true,
		},
		{
			name:             "storage_pool with leading/trailing whitespace is trimmed",
			storagePool:      "  ceph-pool  ",
			storage:          "",
			configDiskStorage: "",
			wantStorage:      "ceph-pool",
		},
		{
			name:             "storage alias with leading/trailing whitespace is trimmed",
			storagePool:      "",
			storage:          "  local-lvm  ",
			configDiskStorage: "",
			wantStorage:      "local-lvm",
		},
		{
			name:             "config default with leading/trailing whitespace is trimmed",
			storagePool:      "",
			storage:          "",
			configDiskStorage: "  data  ",
			wantStorage:      "data",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cloudProps := createDiskCloudProperties{
				StoragePool: tc.storagePool,
				Storage:     tc.storage,
			}

			got, err := resolveStorage(cloudProps, tc.configDiskStorage)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveStorage(%+v, %q): expected error, got nil (resolved %q)",
						cloudProps, tc.configDiskStorage, got)
				}
				// Error must mention "storage" to be actionable for the operator.
				if !strings.Contains(err.Error(), "storage") {
					t.Errorf("error message %q should mention 'storage'", err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("resolveStorage(%+v, %q): unexpected error: %v",
					cloudProps, tc.configDiskStorage, err)
			}
			if got != tc.wantStorage {
				t.Errorf("resolveStorage(%+v, %q) = %q, want %q",
					cloudProps, tc.configDiskStorage, got, tc.wantStorage)
			}
		})
	}
}

// TestResolveStorage_StoragePoolAliasIndependence verifies that storage_pool
// and storage are parsed as independent fields from distinct JSON keys. A
// manifest setting only "storage_pool" must not influence "storage" and vice
// versa — both keys must coexist without interference.
func TestResolveStorage_StoragePoolAliasIndependence(t *testing.T) {
	t.Parallel()

	// Only storage_pool set (storage absent / zero-value).
	cpOnlyPool := createDiskCloudProperties{StoragePool: "pool-a"}
	got, err := resolveStorage(cpOnlyPool, "fallback")
	if err != nil || got != "pool-a" {
		t.Errorf("only storage_pool set: got %q err %v, want pool-a nil", got, err)
	}

	// Only storage (alias) set (storage_pool absent / zero-value).
	cpOnlyAlias := createDiskCloudProperties{Storage: "pool-b"}
	got, err = resolveStorage(cpOnlyAlias, "fallback")
	if err != nil || got != "pool-b" {
		t.Errorf("only storage alias set: got %q err %v, want pool-b nil", got, err)
	}

	// Both set: storage_pool must win.
	cpBoth := createDiskCloudProperties{StoragePool: "pool-a", Storage: "pool-b"}
	got, err = resolveStorage(cpBoth, "fallback")
	if err != nil || got != "pool-a" {
		t.Errorf("both set: got %q err %v, want pool-a nil", got, err)
	}
}
