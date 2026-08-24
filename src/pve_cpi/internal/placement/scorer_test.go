package placement_test

import (
	"math/rand"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

// GiB convenience constant.
const gib = int64(1024 * 1024 * 1024)

// ---------------------------------------------------------------------------
// Filter tests
// ---------------------------------------------------------------------------

func TestFilter_EmptyInput(t *testing.T) {
	t.Parallel()
	pass, rej := placement.Filter(nil, placement.Request{})
	if len(pass) != 0 {
		t.Errorf("pass len = %d; want 0", len(pass))
	}
	if rej != nil {
		t.Errorf("rejections want nil for empty input, got %v", rej)
	}
}

func TestFilter_OfflineNodeExcluded(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: false, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
		{Node: "pve2", Online: true, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
	}
	pass, rej := placement.Filter(facts, placement.Request{})
	if len(pass) != 1 || pass[0].Node != "pve2" {
		t.Errorf("expected only pve2 in pass; got %v", pass)
	}
	if rej["pve1"] == "" {
		t.Errorf("expected rejection reason for pve1")
	}
}

func TestFilter_CandidateSetRestricts(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
		{Node: "pve2", Online: true, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
		{Node: "pve3", Online: true, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
	}
	req := placement.Request{CandidateNodes: []string{"pve2"}}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "pve2" {
		t.Errorf("expected only pve2; got %v", nodeNames(pass))
	}
	if _, ok := rej["pve1"]; !ok {
		t.Error("pve1 should be in rejections")
	}
	if _, ok := rej["pve3"]; !ok {
		t.Error("pve3 should be in rejections")
	}
}

func TestFilter_CPUFilter(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, MaxCPU: 2, TotalMemBytes: gib, FreeMemBytes: gib},
		{Node: "pve2", Online: true, MaxCPU: 16, TotalMemBytes: gib, FreeMemBytes: gib},
	}
	req := placement.Request{RequiredCPU: 8}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "pve2" {
		t.Errorf("expected pve2 only; got %v", nodeNames(pass))
	}
	if rej["pve1"] == "" {
		t.Error("pve1 should be rejected for insufficient CPU")
	}
}

func TestFilter_MemFilter(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, MaxCPU: 8, TotalMemBytes: 4 * gib, FreeMemBytes: 512 * 1024 * 1024},
		{Node: "pve2", Online: true, MaxCPU: 8, TotalMemBytes: 4 * gib, FreeMemBytes: 3 * gib},
	}
	req := placement.Request{RequiredMemBytes: int64(2 * 1024 * 1024 * 1024)} // 2 GiB
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "pve2" {
		t.Errorf("expected pve2 only; got %v", nodeNames(pass))
	}
	if rej["pve1"] == "" {
		t.Error("pve1 should be rejected for insufficient memory")
	}
}

func TestFilter_AllPass(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, MaxCPU: 8, TotalMemBytes: 4 * gib, FreeMemBytes: 2 * gib},
		{Node: "pve2", Online: true, MaxCPU: 8, TotalMemBytes: 4 * gib, FreeMemBytes: 2 * gib},
	}
	pass, rej := placement.Filter(facts, placement.Request{})
	if len(pass) != 2 {
		t.Errorf("expected 2 pass; got %d", len(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections; got %v", rej)
	}
}

// ---------------------------------------------------------------------------
// Filter — maintenance node exclusion
// ---------------------------------------------------------------------------

func TestFilter_MaintenanceNodeExcluded(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, InMaintenance: true, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
		{Node: "pve2", Online: true, InMaintenance: false, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
	}
	req := placement.Request{ExcludeMaintenanceNodes: true}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "pve2" {
		t.Errorf("expected only pve2 in pass; got %v", nodeNames(pass))
	}
	reason, ok := rej["pve1"]
	if !ok {
		t.Error("pve1 should be in rejections")
	}
	if reason != "node in maintenance" {
		t.Errorf("rejection reason = %q; want %q", reason, "node in maintenance")
	}
}

func TestFilter_MaintenanceExcludeDisabled(t *testing.T) {
	t.Parallel()
	// When ExcludeMaintenanceNodes=false, InMaintenance nodes pass through.
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, InMaintenance: true, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
	}
	req := placement.Request{ExcludeMaintenanceNodes: false}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "pve1" {
		t.Errorf("expected pve1 to pass when ExcludeMaintenanceNodes=false; got %v", nodeNames(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections; got %v", rej)
	}
}

func TestFilter_OnlineAndMaintenanceBothRejected(t *testing.T) {
	t.Parallel()
	// Offline check comes first; InMaintenance is secondary.
	facts := []placement.NodeFacts{
		{Node: "offline-maint", Online: false, InMaintenance: true, TotalMemBytes: gib, FreeMemBytes: gib, MaxCPU: 4},
		{Node: "online-maint", Online: true, InMaintenance: true, TotalMemBytes: gib, FreeMemBytes: gib, MaxCPU: 4},
		{Node: "online-clean", Online: true, InMaintenance: false, TotalMemBytes: gib, FreeMemBytes: gib, MaxCPU: 4},
	}
	req := placement.Request{ExcludeMaintenanceNodes: true}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "online-clean" {
		t.Errorf("expected only online-clean; got %v", nodeNames(pass))
	}
	if rej["offline-maint"] != "node offline" {
		t.Errorf("offline-maint rejection = %q; want %q", rej["offline-maint"], "node offline")
	}
	if rej["online-maint"] != "node in maintenance" {
		t.Errorf("online-maint rejection = %q; want %q", rej["online-maint"], "node in maintenance")
	}
}

// ---------------------------------------------------------------------------
// Filter — storage-capacity hard filter tests (§7.58)
// ---------------------------------------------------------------------------

// TestFilter_StorageFilter_RejectsWhenInsufficient verifies that a node with
// FreeStorageBytes < RequiredStorageBytes is rejected with "insufficient free
// storage" when TotalStorageBytes > 0 (facts available).
func TestFilter_StorageFilter_RejectsWhenInsufficient(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "low", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 1 * gib},
		{Node: "high", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 50 * gib},
	}
	req := placement.Request{RequiredStorageBytes: 10 * gib}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "high" {
		t.Errorf("expected only high; got %v", nodeNames(pass))
	}
	if rej["low"] != "insufficient free storage" {
		t.Errorf("low rejection = %q; want %q", rej["low"], "insufficient free storage")
	}
}

// TestFilter_StorageFilter_PassesWhenSufficient verifies that a node with
// FreeStorageBytes >= RequiredStorageBytes passes the filter.
func TestFilter_StorageFilter_PassesWhenSufficient(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "ok", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 50 * gib},
	}
	req := placement.Request{RequiredStorageBytes: 10 * gib}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "ok" {
		t.Errorf("expected ok to pass; got %v", nodeNames(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections; got %v", rej)
	}
}

// TestFilter_StorageFilter_ExactlyEqual passes when free == required.
func TestFilter_StorageFilter_ExactlyEqual(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "exact", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 10 * gib},
	}
	req := placement.Request{RequiredStorageBytes: 10 * gib}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "exact" {
		t.Errorf("expected exact to pass (free == required); got %v", nodeNames(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections; got %v", rej)
	}
}

// TestFilter_StorageFilter_FailOpenWhenNoFacts verifies the fail-open behavior:
// when TotalStorageBytes == 0 (storage facts unavailable), the node is NOT
// rejected regardless of RequiredStorageBytes. This matches the soft-axis skip
// semantics in Score (zero TotalStorageBytes disables the Storage axis).
func TestFilter_StorageFilter_FailOpenWhenNoFacts(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		// TotalStorageBytes=0 means ListStorage failed or no matching pool.
		{Node: "nofacts", Online: true, TotalStorageBytes: 0, FreeStorageBytes: 0},
	}
	req := placement.Request{RequiredStorageBytes: 100 * gib}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "nofacts" {
		t.Errorf("expected nofacts to pass (fail-open on missing facts); got %v", nodeNames(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections for fail-open path; got %v", rej)
	}
}

// TestFilter_StorageFilter_ZeroRequired_NoFilter verifies that
// RequiredStorageBytes == 0 disables the storage filter entirely —
// byte-identical behavior to pre-feature releases.
func TestFilter_StorageFilter_ZeroRequired_NoFilter(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		// Only 1 GiB free — would fail any positive required value.
		{Node: "almost-full", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 1 * gib},
	}
	req := placement.Request{RequiredStorageBytes: 0}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "almost-full" {
		t.Errorf("expected almost-full to pass (RequiredStorageBytes==0 means no filter); got %v", nodeNames(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections when RequiredStorageBytes==0; got %v", rej)
	}
}

// ---------------------------------------------------------------------------
// MaxUtilizationPct / PlannedAddBytes (storage.max_utilization_pct) tests
// ---------------------------------------------------------------------------

// TestFilter_UtilizationCeiling_RejectsWhenProjectedOverCeiling verifies a
// node is rejected when (used+PlannedAddBytes)/total×100 exceeds
// MaxUtilizationPct, and that a node whose projected utilization stays at or
// under the ceiling passes.
func TestFilter_UtilizationCeiling_RejectsWhenProjectedOverCeiling(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		// used=85GiB of 100GiB (85%); +10GiB add -> 95% > 90% ceiling -> reject.
		{Node: "over", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 15 * gib},
		// used=50GiB of 100GiB (50%); +10GiB add -> 60% <= 90% ceiling -> pass.
		{Node: "under", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 50 * gib},
	}
	req := placement.Request{MaxUtilizationPct: 90, PlannedAddBytes: 10 * gib}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "under" {
		t.Errorf("expected only under to pass; got %v", nodeNames(pass))
	}
	if rej["over"] != "storage utilization ceiling exceeded" {
		t.Errorf("over rejection = %q; want %q", rej["over"], "storage utilization ceiling exceeded")
	}
}

// TestFilter_UtilizationCeiling_ExactlyAtCeiling_Passes verifies the boundary:
// a projected utilization exactly equal to the ceiling passes (only strictly
// greater-than is rejected).
func TestFilter_UtilizationCeiling_ExactlyAtCeiling_Passes(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		// used=80GiB of 100GiB; +10GiB add -> exactly 90%.
		{Node: "exact", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 20 * gib},
	}
	req := placement.Request{MaxUtilizationPct: 90, PlannedAddBytes: 10 * gib}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "exact" {
		t.Errorf("expected exact to pass (projected == ceiling); got %v", nodeNames(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections; got %v", rej)
	}
}

// TestFilter_UtilizationCeiling_FailOpenWhenNoFacts verifies fail-open: when
// TotalStorageBytes == 0 (storage facts unavailable), the node is NOT
// rejected regardless of MaxUtilizationPct, matching the RequiredStorageBytes
// fail-open behavior.
func TestFilter_UtilizationCeiling_FailOpenWhenNoFacts(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "nofacts", Online: true, TotalStorageBytes: 0, FreeStorageBytes: 0},
	}
	req := placement.Request{MaxUtilizationPct: 1, PlannedAddBytes: 1}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "nofacts" {
		t.Errorf("expected nofacts to pass (fail-open on missing facts); got %v", nodeNames(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections for fail-open path; got %v", rej)
	}
}

// TestFilter_UtilizationCeiling_ZeroCeiling_NoFilter verifies
// MaxUtilizationPct == 0 disables the gate entirely — byte-identical to the
// gate being absent, even on a nearly-full pool.
func TestFilter_UtilizationCeiling_ZeroCeiling_NoFilter(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "almost-full", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 1 * gib},
	}
	req := placement.Request{MaxUtilizationPct: 0, PlannedAddBytes: 50 * gib}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 1 || pass[0].Node != "almost-full" {
		t.Errorf("expected almost-full to pass (MaxUtilizationPct==0 means no filter); got %v", nodeNames(pass))
	}
	if len(rej) != 0 {
		t.Errorf("expected no rejections when MaxUtilizationPct==0; got %v", rej)
	}
}

// TestFilter_UtilizationCeiling_ComposesWithHeadroomFilter verifies both
// gates can independently reject the same node when both are configured: a
// node with enough free bytes to satisfy RequiredStorageBytes can still be
// rejected by the pct ceiling, and vice versa is exercised by the "over" case
// in TestFilter_UtilizationCeiling_RejectsWhenProjectedOverCeiling.
func TestFilter_UtilizationCeiling_ComposesWithHeadroomFilter(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		// 20GiB free easily satisfies a 10GiB RequiredStorageBytes floor, but
		// used=80GiB of 100GiB (80%); +10GiB add -> 90% > 50% ceiling -> reject
		// on the utilization gate despite passing the headroom gate.
		{Node: "n1", Online: true, TotalStorageBytes: 100 * gib, FreeStorageBytes: 20 * gib},
	}
	req := placement.Request{
		RequiredStorageBytes: 10 * gib,
		MaxUtilizationPct:    50,
		PlannedAddBytes:      10 * gib,
	}
	pass, rej := placement.Filter(facts, req)
	if len(pass) != 0 {
		t.Errorf("expected n1 to be rejected by the utilization ceiling; got pass=%v", nodeNames(pass))
	}
	if rej["n1"] != "storage utilization ceiling exceeded" {
		t.Errorf("n1 rejection = %q; want %q", rej["n1"], "storage utilization ceiling exceeded")
	}
}

// ---------------------------------------------------------------------------
// Score tests
// ---------------------------------------------------------------------------

func TestScore_EmptyInput(t *testing.T) {
	t.Parallel()
	scored := placement.Score(nil, placement.DefaultWeights(), nil)
	if len(scored) != 0 {
		t.Errorf("expected empty scored; got %d", len(scored))
	}
}

func TestScore_MemAxisDominant(t *testing.T) {
	t.Parallel()
	// Only Mem weight set; node with more free memory should score higher.
	w := placement.Weights{Mem: 1.0}
	facts := []placement.NodeFacts{
		{Node: "low-mem", Online: true, FreeMemBytes: 1 * gib, TotalMemBytes: 8 * gib},
		{Node: "high-mem", Online: true, FreeMemBytes: 6 * gib, TotalMemBytes: 8 * gib},
	}
	scored := placement.Score(facts, w, nil)
	if len(scored) != 2 {
		t.Fatalf("expected 2 scored nodes")
	}
	if scored[0].Node != "high-mem" {
		t.Errorf("first node should be high-mem; got %s", scored[0].Node)
	}
}

func TestScore_StorageAxisDominant(t *testing.T) {
	t.Parallel()
	w := placement.Weights{Storage: 1.0}
	facts := []placement.NodeFacts{
		{Node: "low-stor", Online: true, FreeStorageBytes: 10 * gib, TotalStorageBytes: 100 * gib},
		{Node: "high-stor", Online: true, FreeStorageBytes: 80 * gib, TotalStorageBytes: 100 * gib},
	}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "high-stor" {
		t.Errorf("first node should be high-stor; got %s", scored[0].Node)
	}
}

func TestScore_CPUAxisDominant(t *testing.T) {
	t.Parallel()
	w := placement.Weights{CPU: 1.0}
	facts := []placement.NodeFacts{
		{Node: "busy-cpu", Online: true, MaxCPU: 8, CPUUsed: 0.9},
		{Node: "idle-cpu", Online: true, MaxCPU: 8, CPUUsed: 0.1},
	}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "idle-cpu" {
		t.Errorf("first node should be idle-cpu; got %s", scored[0].Node)
	}
}

func TestScore_GuestCountAxisDominant(t *testing.T) {
	t.Parallel()
	w := placement.Weights{GuestCount: 1.0}
	facts := []placement.NodeFacts{
		{Node: "crowded", Online: true, GuestCount: 20},
		{Node: "empty", Online: true, GuestCount: 0},
	}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "empty" {
		t.Errorf("first node should be empty; got %s", scored[0].Node)
	}
}

func TestScore_AntiAffinityPenaltyOrders(t *testing.T) {
	t.Parallel()
	// Node pve1 and pve2 have equal memory; pve1 has 2 same-group VMs.
	// Anti-affinity penalty should push pve1 lower.
	w := placement.Weights{Mem: 1.0, AntiAffinity: 5.0}
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, FreeMemBytes: 4 * gib, TotalMemBytes: 8 * gib, SameGroupCount: 2},
		{Node: "pve2", Online: true, FreeMemBytes: 4 * gib, TotalMemBytes: 8 * gib, SameGroupCount: 0},
	}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "pve2" {
		t.Errorf("pve2 should win (no anti-affinity penalty); got %s first", scored[0].Node)
	}
}

func TestScore_AvoidNodesPenalty(t *testing.T) {
	t.Parallel()
	// avoidNodes provides an override count higher than SameGroupCount.
	w := placement.Weights{AntiAffinity: 5.0}
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, SameGroupCount: 0},
		{Node: "pve2", Online: true, SameGroupCount: 0},
	}
	avoidNodes := map[string]int{"pve1": 3}
	scored := placement.Score(facts, w, avoidNodes)
	if scored[0].Node != "pve2" {
		t.Errorf("pve2 should win (pve1 penalized via avoidNodes); got %s first", scored[0].Node)
	}
}

func TestScore_ZeroTotalMemNoDiv(t *testing.T) {
	t.Parallel()
	// When TotalMemBytes == 0, score must not panic (no divide-by-zero).
	w := placement.DefaultWeights()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, TotalMemBytes: 0, FreeMemBytes: 0},
	}
	scored := placement.Score(facts, w, nil)
	if len(scored) != 1 {
		t.Fatalf("expected 1 scored node")
	}
	// Score should be finite (not NaN/Inf).
	if isInfOrNaN(scored[0].Score) {
		t.Errorf("score is NaN or Inf: %v", scored[0].Score)
	}
}

func TestScore_ZeroTotalStorageNoDiv(t *testing.T) {
	t.Parallel()
	w := placement.Weights{Storage: 1.0}
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, TotalStorageBytes: 0, FreeStorageBytes: 0},
	}
	scored := placement.Score(facts, w, nil)
	if len(scored) != 1 {
		t.Fatalf("expected 1 scored node")
	}
	if isInfOrNaN(scored[0].Score) {
		t.Errorf("score is NaN or Inf: %v", scored[0].Score)
	}
}

func TestScore_ZeroMaxCPUNoDiv(t *testing.T) {
	t.Parallel()
	w := placement.Weights{CPU: 1.0}
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, MaxCPU: 0, CPUUsed: 0},
	}
	scored := placement.Score(facts, w, nil)
	if len(scored) != 1 {
		t.Fatalf("expected 1 scored node")
	}
	if isInfOrNaN(scored[0].Score) {
		t.Errorf("score is NaN or Inf: %v", scored[0].Score)
	}
}

func TestScore_RankedDescending(t *testing.T) {
	t.Parallel()
	w := placement.Weights{Mem: 1.0}
	facts := []placement.NodeFacts{
		{Node: "c", FreeMemBytes: 1 * gib, TotalMemBytes: 8 * gib},
		{Node: "a", FreeMemBytes: 7 * gib, TotalMemBytes: 8 * gib},
		{Node: "b", FreeMemBytes: 4 * gib, TotalMemBytes: 8 * gib},
	}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "a" || scored[1].Node != "b" || scored[2].Node != "c" {
		t.Errorf("expected a, b, c order; got %v", nodeNamesFromScored(scored))
	}
}

func TestScore_TieBrokenByNodeName(t *testing.T) {
	t.Parallel()
	// Equal scores → tie broken by node name (lexicographic ascending).
	w := placement.Weights{} // all zero → all scores == 0
	facts := []placement.NodeFacts{
		{Node: "zz"},
		{Node: "aa"},
		{Node: "mm"},
	}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "aa" {
		t.Errorf("first tied node should be aa (lex first); got %s", scored[0].Node)
	}
}

// ---------------------------------------------------------------------------
// Pick tests
// ---------------------------------------------------------------------------

func TestPick_EmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	result := placement.Pick(nil, nil)
	if result != "" {
		t.Errorf("expected empty string; got %q", result)
	}
}

func TestPick_SingleNode(t *testing.T) {
	t.Parallel()
	scored := []placement.ScoredNode{{Node: "only", Score: 1.0}}
	result := placement.Pick(scored, nil)
	if result != "only" {
		t.Errorf("expected only; got %q", result)
	}
}

func TestPick_NoTie_ReturnsHighestScore(t *testing.T) {
	t.Parallel()
	scored := []placement.ScoredNode{
		{Node: "best", Score: 10.0},
		{Node: "second", Score: 5.0},
	}
	result := placement.Pick(scored, nil)
	if result != "best" {
		t.Errorf("expected best; got %q", result)
	}
}

func TestPick_DeterministicTieBreak(t *testing.T) {
	t.Parallel()
	// Same seed → same result across multiple calls.
	scored := []placement.ScoredNode{
		{Node: "alpha", Score: 1.0},
		{Node: "beta", Score: 1.0},
		{Node: "gamma", Score: 1.0},
	}
	rng1 := rand.New(rand.NewSource(42)) //nolint:gosec // math/rand fixed seed for deterministic test
	rng2 := rand.New(rand.NewSource(42)) //nolint:gosec // math/rand fixed seed for deterministic test
	r1 := placement.Pick(scored, rng1)
	r2 := placement.Pick(scored, rng2)
	if r1 != r2 {
		t.Errorf("expected deterministic result; got %q vs %q", r1, r2)
	}
	if r1 == "" {
		t.Error("pick returned empty string for tied nodes")
	}
}

func TestPick_TieBreakCoversBothOptions(t *testing.T) {
	t.Parallel()
	// With enough seeds, both tied nodes should be selected at least once.
	scored := []placement.ScoredNode{
		{Node: "A", Score: 1.0},
		{Node: "B", Score: 1.0},
	}
	seen := map[string]bool{}
	for seed := int64(0); seed < 50; seed++ {
		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // math/rand fixed seed for deterministic test
		seen[placement.Pick(scored, rng)] = true
	}
	if !seen["A"] || !seen["B"] {
		t.Errorf("expected both A and B to be selected across seeds; got %v", seen)
	}
}

// ---------------------------------------------------------------------------
// DefaultWeights tests
// ---------------------------------------------------------------------------

func TestDefaultWeights_Values(t *testing.T) {
	t.Parallel()
	w := placement.DefaultWeights()
	if w.Mem != 1.0 {
		t.Errorf("Mem default = %v; want 1.0", w.Mem)
	}
	if w.Storage != 0.5 {
		t.Errorf("Storage default = %v; want 0.5", w.Storage)
	}
	if w.CPU != 0.5 {
		t.Errorf("CPU default = %v; want 0.5", w.CPU)
	}
	if w.GuestCount != 0.3 {
		t.Errorf("GuestCount default = %v; want 0.3", w.GuestCount)
	}
	if w.AntiAffinity != 5.0 {
		t.Errorf("AntiAffinity default = %v; want 5.0", w.AntiAffinity)
	}
}

// ---------------------------------------------------------------------------
// Equivalence test: mem-only weights picks same node as original pickBestNode
// ---------------------------------------------------------------------------

// TestScore_MemOnlyEquivalence verifies that Score with Mem=1.0 and all other
// weights=0 picks the node with the highest FreeMemBytes, matching the original
// pickBestNode behavior in calculate_vm_cloud_properties.go.
func TestScore_MemOnlyEquivalence(t *testing.T) {
	t.Parallel()
	w := placement.Weights{Mem: 1.0}
	facts := []placement.NodeFacts{
		{Node: "pve1", FreeMemBytes: 4 * gib, TotalMemBytes: 16 * gib},
		{Node: "pve2", FreeMemBytes: 12 * gib, TotalMemBytes: 16 * gib},
		{Node: "pve3", FreeMemBytes: 8 * gib, TotalMemBytes: 16 * gib},
	}
	scored := placement.Score(facts, w, nil)
	winner := placement.Pick(scored, nil)
	if winner != "pve2" {
		t.Errorf("mem-only picker should select pve2 (most free mem); got %s", winner)
	}
}

// ---------------------------------------------------------------------------
// MemorySignal tests
// ---------------------------------------------------------------------------

// TestScore_MemorySignalUnset_DefaultsToLegacyResident asserts the backward-
// compatibility guarantee the reserved-memory feature depends on: the zero
// value of Weights.MemorySignal ("") must produce byte-identical scores to
// explicit MemorySignalResident, so every pre-existing caller that builds a
// Weights literal without knowing about MemorySignal keeps today's exact
// numbers. The "reserved" default lives at the config layer (which always
// sets this field explicitly), not in the package's zero value.
func TestScore_MemorySignalUnset_DefaultsToLegacyResident(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, FreeMemBytes: 12 * gib, TotalMemBytes: 16 * gib, CommittedMemBytes: 14 * gib},
		{Node: "pve2", Online: true, FreeMemBytes: 4 * gib, TotalMemBytes: 16 * gib, CommittedMemBytes: 1 * gib},
	}
	unset := placement.Score(facts, placement.Weights{Mem: 1.0}, nil)
	resident := placement.Score(facts, placement.Weights{Mem: 1.0, MemorySignal: placement.MemorySignalResident}, nil)
	if len(unset) != len(resident) {
		t.Fatalf("length mismatch: unset=%d resident=%d", len(unset), len(resident))
	}
	for i := range unset {
		if unset[i] != resident[i] {
			t.Errorf("unset MemorySignal diverges from explicit resident at index %d: %+v vs %+v",
				i, unset[i], resident[i])
		}
	}
}

// TestScore_MemorySignalResident_IgnoresCommittedMemBytes confirms resident
// mode ranks purely by FreeMemBytes/TotalMemBytes (today's formula) and never
// consults CommittedMemBytes, even when CommittedMemBytes would flip the
// ranking under reserved mode.
func TestScore_MemorySignalResident_IgnoresCommittedMemBytes(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		// High resident-free but heavily committed — would lose under reserved mode.
		{Node: "pve1", Online: true, FreeMemBytes: 12 * gib, TotalMemBytes: 16 * gib, CommittedMemBytes: 14 * gib},
		// Low resident-free but lightly committed — would win under reserved mode.
		{Node: "pve2", Online: true, FreeMemBytes: 4 * gib, TotalMemBytes: 16 * gib, CommittedMemBytes: 1 * gib},
	}
	w := placement.Weights{Mem: 1.0, MemorySignal: placement.MemorySignalResident}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "pve1" {
		t.Errorf("resident mode should rank by FreeMemBytes only; expected pve1 first, got %s", scored[0].Node)
	}
	legacyScored := placement.Score(facts, placement.Weights{Mem: 1.0}, nil)
	for i := range scored {
		if scored[i] != legacyScored[i] {
			t.Errorf("resident mode diverges from legacy unset mode at index %d: %+v vs %+v",
				i, scored[i], legacyScored[i])
		}
	}
}

// TestScore_MemorySignalReserved_FlipsRankingVsResident uses the identical
// facts from TestScore_MemorySignalResident_IgnoresCommittedMemBytes to prove
// reserved mode ranks by committed headroom, not resident free memory —
// the two modes genuinely diverge on facts engineered to disagree.
func TestScore_MemorySignalReserved_FlipsRankingVsResident(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", Online: true, FreeMemBytes: 12 * gib, TotalMemBytes: 16 * gib, CommittedMemBytes: 14 * gib},
		{Node: "pve2", Online: true, FreeMemBytes: 4 * gib, TotalMemBytes: 16 * gib, CommittedMemBytes: 1 * gib},
	}
	w := placement.Weights{Mem: 1.0, MemorySignal: placement.MemorySignalReserved}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "pve2" {
		t.Errorf("reserved mode should rank by committed headroom; expected pve2 first, got %s", scored[0].Node)
	}
}

// TestScore_MemorySignalReserved_ClampsOvercommitToZero verifies an
// overcommitted node (CommittedMemBytes > TotalMemBytes — e.g. facts
// gathered mid-migration, or thin-provisioned memory) clamps its Mem-axis
// contribution to exactly 0 rather than going negative, which would
// otherwise let a single overcommitted node's Mem axis invert the sign of
// the whole weighted sum for any other positively-weighted axis.
func TestScore_MemorySignalReserved_ClampsOvercommitToZero(t *testing.T) {
	t.Parallel()
	w := placement.Weights{Mem: 1.0, MemorySignal: placement.MemorySignalReserved}
	facts := []placement.NodeFacts{
		{Node: "overcommitted", Online: true, TotalMemBytes: 16 * gib, CommittedMemBytes: 20 * gib},
		{Node: "healthy", Online: true, TotalMemBytes: 16 * gib, CommittedMemBytes: 4 * gib},
	}
	scored := placement.Score(facts, w, nil)
	if scored[0].Node != "healthy" {
		t.Errorf("healthy should outrank overcommitted; got %s first", scored[0].Node)
	}
	var overcommittedScore float64
	for _, s := range scored {
		if s.Node == "overcommitted" {
			overcommittedScore = s.Score
		}
	}
	if overcommittedScore != 0 {
		t.Errorf("overcommitted score = %v; want 0 (clamped; Mem is the only weighted axis)", overcommittedScore)
	}
}

// TestScore_MemorySignalReserved_SequentialCreatesFanOut is the regression
// test for the bug this feature fixes: under the legacy resident-memory
// signal, a sequence of freshly-booted VMs touches only a fraction of its
// reserved RAM, so FreeMemBytes barely moves between creates and the
// deterministic scorer keeps picking the same node for the whole sequence.
// Reserved mode fixes this because each create raises the winner's
// CommittedMemBytes by the full reservation immediately (no dependency on the
// guest actually touching that memory), so the *next* pick's score for that
// node visibly drops. This simulates 9 sequential single-VM creates across 3
// identical, initially-empty nodes and asserts they fan out evenly (3 apiece)
// rather than stacking on one node.
func TestScore_MemorySignalReserved_SequentialCreatesFanOut(t *testing.T) {
	t.Parallel()
	const totalMemBytes = 32 * gib
	const perCreateReservation = 2 * gib
	const numNodes = 3
	const numCreates = 9 // 3 full rounds across 3 equal nodes

	w := placement.Weights{Mem: 1.0, MemorySignal: placement.MemorySignalReserved}
	committed := map[string]int64{"pve1": 0, "pve2": 0, "pve3": 0}
	picks := make(map[string]int, numNodes)

	for i := 0; i < numCreates; i++ {
		facts := []placement.NodeFacts{
			{Node: "pve1", Online: true, TotalMemBytes: totalMemBytes, CommittedMemBytes: committed["pve1"]},
			{Node: "pve2", Online: true, TotalMemBytes: totalMemBytes, CommittedMemBytes: committed["pve2"]},
			{Node: "pve3", Online: true, TotalMemBytes: totalMemBytes, CommittedMemBytes: committed["pve3"]},
		}
		scored := placement.Score(facts, w, nil)
		winner := placement.Pick(scored, nil)
		if winner == "" {
			t.Fatalf("create %d: Pick returned empty string", i)
		}
		picks[winner]++
		// Simulate the create landing on winner: its reservation grows by one
		// guest's worth of committed memory immediately, dropping its score
		// for the next iteration — this is the mechanism under test.
		committed[winner] += perCreateReservation
	}

	for _, node := range []string{"pve1", "pve2", "pve3"} {
		want := numCreates / numNodes
		if picks[node] != want {
			t.Errorf("picks[%s] = %d; want %d (even fan-out across %d equal nodes)",
				node, picks[node], want, numNodes)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nodeNames(facts []placement.NodeFacts) []string {
	names := make([]string, len(facts))
	for i, f := range facts {
		names[i] = f.Node
	}
	return names
}

func nodeNamesFromScored(scored []placement.ScoredNode) []string {
	names := make([]string, len(scored))
	for i, s := range scored {
		names[i] = s.Node
	}
	return names
}

func isInfOrNaN(f float64) bool {
	return f != f || f > 1e308 || f < -1e308
}
