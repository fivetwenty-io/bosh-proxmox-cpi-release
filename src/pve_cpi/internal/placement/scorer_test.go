package placement_test

import (
	"math/rand"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/placement"
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
