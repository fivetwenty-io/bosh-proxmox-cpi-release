// Package handlers -- internal tests for liveStorageInfo, lookupVMStorageType,
// and needsReplicaCheck: the consolidation of these two previously
// independent ad-hoc /storage readers onto the shared pve.ParseStorageEntry
// decoder.
package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

func storageLookupDeps(entries map[string]dlbStorageEntry) Deps {
	cfg := &config.CPIConfig{Node: "pve"}
	cfg.ApplyDefaults()
	return Deps{
		Config: cfg,
		PVE: &cloneClient{
			etClient:          etClient{},
			clusterStorageSvc: &dlbMultiStorageStub{entries: entries},
			clusterSvc:        &cloneClusterSvc{nodeCount: 1},
		},
		Logger: log.NewNopLogger(),
	}
}

func TestLookupVMStorageType(t *testing.T) {
	t.Parallel()
	deps := storageLookupDeps(map[string]dlbStorageEntry{
		"nfs-a":  {storageType: "nfs", shared: true},
		"vg-lvm": {storageType: "lvm"},
	})

	if got := lookupVMStorageType(context.Background(), deps, "nfs-a"); got != "nfs" {
		t.Errorf("lookupVMStorageType(nfs-a) = %q, want %q", got, "nfs")
	}
	if got := lookupVMStorageType(context.Background(), deps, "vg-lvm"); got != "lvm" {
		t.Errorf("lookupVMStorageType(vg-lvm) = %q, want %q", got, "lvm")
	}
	if got := lookupVMStorageType(context.Background(), deps, "missing"); got != "" {
		t.Errorf("lookupVMStorageType(missing) = %q, want empty (fail-open)", got)
	}
	if got := lookupVMStorageType(context.Background(), deps, ""); got != "" {
		t.Errorf("lookupVMStorageType(\"\") = %q, want empty", got)
	}
}

func TestLookupVMStorageType_NilPVE(t *testing.T) {
	t.Parallel()
	deps := Deps{Logger: log.NewNopLogger()}
	if got := lookupVMStorageType(context.Background(), deps, "any"); got != "" {
		t.Errorf("lookupVMStorageType with nil PVE = %q, want empty", got)
	}
}

func TestNeedsReplicaCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries map[string]dlbStorageEntry
		storage string
		want    bool
	}{
		{
			name:    "local lvm needs the guard",
			entries: map[string]dlbStorageEntry{"local-lvm": {storageType: "lvm", shared: false}},
			storage: "local-lvm",
			want:    true,
		},
		{
			name:    "shared-flagged local type skips the guard",
			entries: map[string]dlbStorageEntry{"local-lvm": {storageType: "lvm", shared: true}},
			storage: "local-lvm",
			want:    false,
		},
		{
			name: "nfs without the shared flag still skips the guard (type-based sharing)",
			entries: map[string]dlbStorageEntry{
				"nfs-a": {storageType: "nfs", shared: false}, // PVE auto-shares nfs; flag left unset
			},
			storage: "nfs-a",
			want:    false,
		},
		{
			name:    "storage absent from index fails open (no guard)",
			entries: map[string]dlbStorageEntry{"other": {storageType: "lvm"}},
			storage: "missing",
			want:    false,
		},
		{
			name:    "empty storage name fails open",
			entries: map[string]dlbStorageEntry{},
			storage: "",
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			deps := storageLookupDeps(c.entries)
			if got := needsReplicaCheck(context.Background(), deps, c.storage); got != c.want {
				t.Errorf("needsReplicaCheck(%q) = %v, want %v", c.storage, got, c.want)
			}
		})
	}
}

func TestLiveStorageInfo_CarriesBackingFields(t *testing.T) {
	t.Parallel()
	deps := storageLookupDeps(map[string]dlbStorageEntry{
		"nfs-a": {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/proxmox"},
	})
	info, ok := liveStorageInfo(context.Background(), deps, "nfs-a")
	if !ok {
		t.Fatal("liveStorageInfo: expected ok=true")
	}
	want := "nfs://10.0.0.5/tank/proxmox"
	if got := info.BackingKey(); got != want {
		t.Errorf("BackingKey() = %q, want %q — liveStorageInfo must decode server/export via pve.ParseStorageEntry", got, want)
	}
}
