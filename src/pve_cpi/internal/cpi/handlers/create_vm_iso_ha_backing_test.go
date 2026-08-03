// Package handlers -- internal tests proving checkISOStorageForHA (via
// dlbStorageIsShared's SharedViaBacking classification) correctly handles
// the config-drift backing-identity scenario: two storage IDs registered
// against the same physical location where only one is flagged "shared" in
// storage.cfg.
package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// TestCheckISOStorageForHA_ConfigDriftBacking_TreatedAsShared verifies that
// when iso_storage ("iso-b") is NOT itself flagged shared but shares a dir
// path with another registered storage ("iso-a") that IS flagged shared,
// checkISOStorageForHA treats iso-b as shared: no Warn, no error.
func TestCheckISOStorageForHA_ConfigDriftBacking_TreatedAsShared(t *testing.T) {
	t.Parallel()
	enabled := true
	cfg := isoHABaseConfig("iso-b")
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	storageSvc := &dlbMultiStorageStub{entries: map[string]dlbStorageEntry{
		"iso-a": {storageType: "dir", shared: true, path: "/mnt/pve/iso-export"},
		"iso-b": {storageType: "dir", shared: false, path: "/mnt/pve/iso-export"},
	}}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{}

	var buf bytes.Buffer
	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, capturingLogger(t, &buf))
	if err != nil {
		t.Fatalf("iso-b shares iso-a's backing and iso-a is shared: expected nil error, got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no migration-safety Warn (iso-b treated as shared via backing), got: %s", buf.String())
	}
}

// TestCheckISOStorageForHA_DistinctBacking_StillWarns is the distinct-backing
// counterpart: iso_storage shares no backing with any other entry and is not
// itself flagged shared, so the pre-existing Warn-only behavior is unchanged.
func TestCheckISOStorageForHA_DistinctBacking_StillWarns(t *testing.T) {
	t.Parallel()
	enabled := true
	cfg := isoHABaseConfig("iso-c")
	cfg.Placement = &config.PlacementConfig{DLB: &config.DLBConfig{Enabled: &enabled}}
	storageSvc := &dlbMultiStorageStub{entries: map[string]dlbStorageEntry{
		"iso-a": {storageType: "dir", shared: true, path: "/mnt/pve/iso-export"},
		"iso-c": {storageType: "dir", shared: false, path: "/mnt/pve/iso-c-distinct"},
	}}
	deps := dlbDeps(nil, nil, storageSvc, cfg)
	cp := createVMCloudProps{}

	var buf bytes.Buffer
	err := checkISOStorageForHA(context.Background(), deps, 100, cp, "pve01", nil, capturingLogger(t, &buf))
	if err != nil {
		t.Fatalf("require_shared_iso_for_ha is false: expected nil error (warn-only), got %v", err)
	}
	if buf.Len() == 0 {
		t.Error("iso-c shares no backing with any shared entry: expected the migration-safety Warn to still fire")
	}
}
