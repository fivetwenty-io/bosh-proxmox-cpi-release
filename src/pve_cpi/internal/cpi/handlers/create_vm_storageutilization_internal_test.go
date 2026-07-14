package handlers

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/placement"
)

const cvsuGiB = int64(1024 * 1024 * 1024)

// ---------------------------------------------------------------------------
// computeDiskFootprintBytes: raw footprint, no headroom margin
// ---------------------------------------------------------------------------

// TestComputeDiskFootprintBytes_RootOnly verifies the footprint is just the
// root disk in bytes when no ephemeral disk is requested — NO headroom term,
// unlike computeRequiredStorageBytes.
func TestComputeDiskFootprintBytes_RootOnly(t *testing.T) {
	t.Parallel()
	cp := createVMCloudProps{RootDiskSize: 10240} // 10 GiB in MiB
	got := computeDiskFootprintBytes(cp, "local-lvm")
	want := int64(10) * cvsuGiB
	if got != want {
		t.Errorf("computeDiskFootprintBytes(root-only) = %d; want %d", got, want)
	}
}

// TestComputeDiskFootprintBytes_RootAndEphemeralSamePool verifies both disks
// are counted when the ephemeral disk lands on the same pool.
func TestComputeDiskFootprintBytes_RootAndEphemeralSamePool(t *testing.T) {
	t.Parallel()
	cp := createVMCloudProps{RootDiskSize: 20480, EphemeralDiskSizeMB: 8192}
	got := computeDiskFootprintBytes(cp, "local-lvm")
	want := int64(20)*cvsuGiB + int64(8)*cvsuGiB
	if got != want {
		t.Errorf("computeDiskFootprintBytes(root+ephemeral) = %d; want %d", got, want)
	}
}

// TestComputeDiskFootprintBytes_EphemeralOnDifferentPool_Excluded verifies
// the ephemeral disk is excluded from the footprint when it targets a
// different pool than storageName, matching computeRequiredStorageBytes.
func TestComputeDiskFootprintBytes_EphemeralOnDifferentPool_Excluded(t *testing.T) {
	t.Parallel()
	cp := createVMCloudProps{RootDiskSize: 10240, EphemeralDiskSizeMB: 8192, EphemeralStoragePool: "fast-nvme"}
	got := computeDiskFootprintBytes(cp, "local-lvm")
	want := int64(10) * cvsuGiB
	if got != want {
		t.Errorf("computeDiskFootprintBytes(ephemeral different pool) = %d; want %d (ephemeral excluded)", got, want)
	}
}

// TestComputeDiskFootprintBytes_DefaultRootDisk verifies the 5 GiB floor when
// no disk size is specified.
func TestComputeDiskFootprintBytes_DefaultRootDisk(t *testing.T) {
	t.Parallel()
	got := computeDiskFootprintBytes(createVMCloudProps{}, "local-lvm")
	want := int64(defaultStemcellDiskGiB) * cvsuGiB
	if got != want {
		t.Errorf("computeDiskFootprintBytes(default) = %d; want %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// utilizationGateForRequest
// ---------------------------------------------------------------------------

// TestUtilizationGateForRequest_Disabled_ZeroFieldsAndNoopClosure verifies
// the gate being off (pct unset, the default) returns zero ceiling/addBytes
// and a closure that does nothing when invoked.
func TestUtilizationGateForRequest_Disabled_ZeroFieldsAndNoopClosure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	cfg := &config.CPIConfig{} // Storage nil -> disabled
	cp := createVMCloudProps{RootDiskSize: 10240}
	facts := []placement.NodeFacts{{Node: "pve01", TotalStorageBytes: 100 * cvsuGiB, FreeStorageBytes: 1 * cvsuGiB}}

	ceiling, addBytes, warnChosen := utilizationGateForRequest(cfg, cp, "local-lvm", facts, logger)
	if ceiling != 0 || addBytes != 0 {
		t.Errorf("disabled gate: ceiling=%d addBytes=%d; want 0, 0", ceiling, addBytes)
	}
	warnChosen("pve01") // must not panic, must not log
	if buf.String() != "" {
		t.Errorf("disabled gate: warnChosen must be a no-op, got log %q", buf.String())
	}
}

// TestUtilizationGateForRequest_Enforce_PopulatesHardFilterFields verifies
// enforce mode (the default when pct > 0) returns the ceiling and computed
// footprint for placement.Request, with a no-op closure (Filter already
// excludes any violator, so there is nothing left to warn about).
func TestUtilizationGateForRequest_Enforce_PopulatesHardFilterFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	pct := 80
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct}}
	cp := createVMCloudProps{RootDiskSize: 10240} // 10 GiB
	facts := []placement.NodeFacts{{Node: "pve01", TotalStorageBytes: 100 * cvsuGiB, FreeStorageBytes: 50 * cvsuGiB}}

	ceiling, addBytes, warnChosen := utilizationGateForRequest(cfg, cp, "local-lvm", facts, logger)
	if ceiling != 80 {
		t.Errorf("ceiling = %d; want 80", ceiling)
	}
	if addBytes != 10*cvsuGiB {
		t.Errorf("addBytes = %d; want %d (10 GiB root disk footprint)", addBytes, 10*cvsuGiB)
	}
	warnChosen("pve01") // no-op in enforce mode
	if buf.String() != "" {
		t.Errorf("enforce mode: warnChosen must be a no-op, got log %q", buf.String())
	}
}

// TestUtilizationGateForRequest_Warn_ZeroFieldsAndAdvisoryClosure verifies
// warn mode leaves the hard-filter fields at zero (byte-identical scoring)
// and returns a closure that warns when the chosen node would breach the
// ceiling.
func TestUtilizationGateForRequest_Warn_ZeroFieldsAndAdvisoryClosure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	pct := 50
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct, MaxUtilizationMode: suWarnMode}}
	cp := createVMCloudProps{RootDiskSize: 10240} // 10 GiB
	// used=80GiB of 100GiB (80%); +10GiB -> 90% > 50% ceiling.
	facts := []placement.NodeFacts{{Node: "pve01", TotalStorageBytes: 100 * cvsuGiB, FreeStorageBytes: 20 * cvsuGiB}}

	ceiling, addBytes, warnChosen := utilizationGateForRequest(cfg, cp, "local-lvm", facts, logger)
	if ceiling != 0 || addBytes != 0 {
		t.Errorf("warn mode: ceiling=%d addBytes=%d; want 0, 0 (hard filter must stay off)", ceiling, addBytes)
	}
	warnChosen("pve01")
	if !strings.Contains(buf.String(), "warn mode; proceeding") {
		t.Errorf("expected warnChosen to log a Warn for the breaching node, got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// warnIfNodeUtilizationExceeds
// ---------------------------------------------------------------------------

func TestWarnIfNodeUtilizationExceeds_BelowCeiling_NoWarn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	facts := []placement.NodeFacts{{Node: "pve01", TotalStorageBytes: 100 * cvsuGiB, FreeStorageBytes: 50 * cvsuGiB}}
	warnIfNodeUtilizationExceeds(facts, "pve01", 90, 10*cvsuGiB, logger)
	if buf.String() != "" {
		t.Errorf("expected no warn below ceiling, got %q", buf.String())
	}
}

func TestWarnIfNodeUtilizationExceeds_NodeNotInFacts_NoOp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	facts := []placement.NodeFacts{{Node: "other", TotalStorageBytes: 100 * cvsuGiB, FreeStorageBytes: 1}}
	warnIfNodeUtilizationExceeds(facts, "pve01", 1, 100*cvsuGiB, logger)
	if buf.String() != "" {
		t.Errorf("expected no warn for a node absent from facts, got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// End-to-end placement-filter integration: utilizationGateForRequest feeding
// placement.Filter directly, proving enforce mode actually removes the
// violating node from the candidate set and warn mode does not.
// ---------------------------------------------------------------------------

func TestUtilizationGate_PlacementFilterIntegration_EnforceRejects(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	pct := 90
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct}}
	cp := createVMCloudProps{RootDiskSize: 10240} // 10 GiB footprint
	facts := []placement.NodeFacts{
		// used=85GiB of 100GiB; +10GiB -> 95% > 90% -> must be rejected.
		{Node: "over", Online: true, TotalStorageBytes: 100 * cvsuGiB, FreeStorageBytes: 15 * cvsuGiB},
		// used=50GiB of 100GiB; +10GiB -> 60% <= 90% -> must pass.
		{Node: "under", Online: true, TotalStorageBytes: 100 * cvsuGiB, FreeStorageBytes: 50 * cvsuGiB},
	}

	ceiling, addBytes, _ := utilizationGateForRequest(cfg, cp, "local-lvm", facts, logger)
	pass, rej := placement.Filter(facts, placement.Request{MaxUtilizationPct: ceiling, PlannedAddBytes: addBytes})

	if len(pass) != 1 || pass[0].Node != "under" {
		t.Errorf("expected only 'under' to pass Filter; got %v", pass)
	}
	if rej["over"] != "storage utilization ceiling exceeded" {
		t.Errorf("'over' rejection = %q; want %q", rej["over"], "storage utilization ceiling exceeded")
	}
}

func TestUtilizationGate_PlacementFilterIntegration_WarnDoesNotReject(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	pct := 90
	cfg := &config.CPIConfig{Storage: &config.StorageConfig{MaxUtilizationPct: &pct, MaxUtilizationMode: suWarnMode}}
	cp := createVMCloudProps{RootDiskSize: 10240}
	facts := []placement.NodeFacts{
		// Would breach the ceiling in enforce mode, but warn mode must not filter it.
		{Node: "over", Online: true, TotalStorageBytes: 100 * cvsuGiB, FreeStorageBytes: 15 * cvsuGiB},
	}

	ceiling, addBytes, warnChosen := utilizationGateForRequest(cfg, cp, "local-lvm", facts, logger)
	pass, rej := placement.Filter(facts, placement.Request{MaxUtilizationPct: ceiling, PlannedAddBytes: addBytes})

	if len(pass) != 1 || pass[0].Node != "over" {
		t.Errorf("warn mode must not filter the candidate set; got pass=%v rej=%v", pass, rej)
	}
	// The caller invokes warnChosen only after a node is actually selected —
	// exercise that here to confirm the advisory Warn still fires.
	warnChosen("over")
	if !strings.Contains(buf.String(), "warn mode; proceeding") {
		t.Errorf("expected an advisory Warn for the chosen node, got %q", buf.String())
	}
}
