// Package handlers -- internal tests proving checkISOStorageForHA (via
// dlbStorageIsShared's SharedViaBacking classification) correctly handles
// the config-drift backing-identity scenario: two storage IDs registered
// against the same physical location where only one is flagged "shared" in
// storage.cfg.
package handlers

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// migrationSafetyWarnText is the leading fragment of the one Warn these tests
// are about (see checkISOStorageForHA). Both tests match on it rather than on
// buffer emptiness: checkISOStorageForHA also drives the once-per-process
// HA-vs-resurrector Warn (haResurrectorWarnOnce), so whichever test in the
// binary reaches that once FIRST captures an extra, unrelated line in its
// buffer. A length check therefore passes or fails on test ordering — it is
// why these two tests failed under -shuffle and in isolation. Matching the
// message keeps them order-independent without mutating process-wide state
// from a parallel test.
const migrationSafetyWarnText = "live migration and HA recovery of this VM will fail"

func countMigrationSafetyWarns(logged string) int {
	return strings.Count(logged, migrationSafetyWarnText)
}

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
	if n := countMigrationSafetyWarns(buf.String()); n != 0 {
		t.Errorf("expected no migration-safety Warn (iso-b treated as shared via backing), got %d: %s", n, buf.String())
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
	if countMigrationSafetyWarns(buf.String()) == 0 {
		t.Errorf("iso-c shares no backing with any shared entry: expected the migration-safety Warn to still fire, got: %s", buf.String())
	}
}
