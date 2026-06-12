package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// --------------------------------------------------------------------------
// mergeHotplugToken unit tests
// --------------------------------------------------------------------------

// TestMergeHotplugToken_AddToEmpty verifies that adding a token to an empty
// hotplug string produces just that token.
func TestMergeHotplugToken_AddToEmpty(t *testing.T) {
	t.Parallel()

	got := mergeHotplugToken("", "cpu", true)
	if got != "cpu" {
		t.Errorf("mergeHotplugToken(\"\", \"cpu\", true) = %q; want %q", got, "cpu")
	}
}

// TestMergeHotplugToken_AddToExisting verifies that a new token is appended
// with a comma when the string is non-empty and the token is absent.
func TestMergeHotplugToken_AddToExisting(t *testing.T) {
	t.Parallel()

	got := mergeHotplugToken("disk,network", "cpu", true)
	if got != "disk,network,cpu" {
		t.Errorf("mergeHotplugToken(\"disk,network\", \"cpu\", true) = %q; want %q", got, "disk,network,cpu")
	}
}

// TestMergeHotplugToken_AddIdempotent verifies that adding an already-present
// token does not duplicate it.
func TestMergeHotplugToken_AddIdempotent(t *testing.T) {
	t.Parallel()

	got := mergeHotplugToken("disk,cpu,network", "cpu", true)
	if got != "disk,cpu,network" {
		t.Errorf("mergeHotplugToken idempotent = %q; want %q", got, "disk,cpu,network")
	}
}

// TestMergeHotplugToken_RemovePresent verifies that a present token is removed.
func TestMergeHotplugToken_RemovePresent(t *testing.T) {
	t.Parallel()

	got := mergeHotplugToken("disk,cpu,network", "cpu", false)
	// order preserved, no doubles
	if strings.Contains(got, "cpu") {
		t.Errorf("mergeHotplugToken remove: 'cpu' still present in %q", got)
	}
	if !strings.Contains(got, "disk") || !strings.Contains(got, "network") {
		t.Errorf("mergeHotplugToken remove: other tokens lost in %q", got)
	}
}

// TestMergeHotplugToken_RemoveAbsent verifies that removing a token that is
// not present is a no-op.
func TestMergeHotplugToken_RemoveAbsent(t *testing.T) {
	t.Parallel()

	got := mergeHotplugToken("disk,network", "cpu", false)
	if got != "disk,network" {
		t.Errorf("mergeHotplugToken remove-absent = %q; want %q", got, "disk,network")
	}
}

// TestMergeHotplugToken_RemoveOnly verifies that removing the sole token
// yields an empty string.
func TestMergeHotplugToken_RemoveOnly(t *testing.T) {
	t.Parallel()

	got := mergeHotplugToken("cpu", "cpu", false)
	if got != "" {
		t.Errorf("mergeHotplugToken remove-only = %q; want empty string", got)
	}
}

// TestMergeHotplugToken_RemoveFromEmpty verifies that removing a token from
// an empty string is a no-op.
func TestMergeHotplugToken_RemoveFromEmpty(t *testing.T) {
	t.Parallel()

	got := mergeHotplugToken("", "cpu", false)
	if got != "" {
		t.Errorf("mergeHotplugToken remove-from-empty = %q; want empty string", got)
	}
}

// TestMergeHotplugToken_DisabledString_AddStill verifies that "0" (PVE-style
// disable) is treated as an ordinary token, so adding "cpu" appends normally.
func TestMergeHotplugToken_DisabledString_AddStill(t *testing.T) {
	t.Parallel()

	got := mergeHotplugToken("0", "cpu", true)
	// "0" is a valid token that disables hotplug on PVE; merging adds cpu.
	if !strings.Contains(got, "cpu") {
		t.Errorf("mergeHotplugToken(\"0\", \"cpu\", true) = %q; want cpu present", got)
	}
}

// --------------------------------------------------------------------------
// resolveVMShapeHotplugNUMAWithError — CPUHotplug / MemoryHotplug fields
// --------------------------------------------------------------------------

// TestResolveHotplugNUMA_BothNil_ByteIdentical verifies that nil/nil produces
// the same output as the pre-feature baseline (config defaults, no changes).
func TestResolveHotplugNUMA_BothNil_ByteIdentical(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,network")
	cp := createVMCloudProps{} // CPUHotplug nil, MemoryHotplug nil
	cpMap := map[string]any{}

	hotplug, numa, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hotplug != "disk,network" {
		t.Errorf("hotplug = %q; want %q (byte-identical baseline)", hotplug, "disk,network")
	}
	if numa != cfg.NUMAValue() {
		t.Errorf("numa = %v; want %v (byte-identical baseline)", numa, cfg.NUMAValue())
	}
}

// TestResolveHotplugNUMA_CPUTrue_TokenAdded verifies that cpu_hotplug=true
// ensures the "cpu" token is present in the resolved hotplug string.
func TestResolveHotplugNUMA_CPUTrue_TokenAdded(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,network")
	cp := createVMCloudProps{CPUHotplug: boolPtr(true)}
	cpMap := map[string]any{}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(hotplug, "cpu") {
		t.Errorf("hotplug = %q; want 'cpu' token present (cpu_hotplug=true)", hotplug)
	}
}

// TestResolveHotplugNUMA_CPUFalse_TokenRemoved verifies that cpu_hotplug=false
// removes the "cpu" token even when the config default includes it.
func TestResolveHotplugNUMA_CPUFalse_TokenRemoved(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,cpu,network")
	cp := createVMCloudProps{CPUHotplug: boolPtr(false)}
	cpMap := map[string]any{}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(hotplug, "cpu") {
		t.Errorf("hotplug = %q; 'cpu' must be absent (cpu_hotplug=false)", hotplug)
	}
}

// TestResolveHotplugNUMA_MemoryTrue_TokenAddedAndNUMA verifies that
// memory_hotplug=true ensures the "memory" token and forces numaEnabled=true.
func TestResolveHotplugNUMA_MemoryTrue_TokenAddedAndNUMA(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,network") // no memory token, NUMA off
	cp := createVMCloudProps{MemoryHotplug: boolPtr(true)}
	cpMap := map[string]any{}

	hotplug, numa, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(hotplug, "memory") {
		t.Errorf("hotplug = %q; want 'memory' token (memory_hotplug=true)", hotplug)
	}
	if !numa {
		t.Errorf("numaEnabled = false; want true (memory_hotplug=true forces NUMA)")
	}
}

// TestResolveHotplugNUMA_MemoryFalse_TokenRemoved verifies that
// memory_hotplug=false removes the "memory" token when present.
func TestResolveHotplugNUMA_MemoryFalse_TokenRemoved(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,cpu,memory,network")
	cp := createVMCloudProps{MemoryHotplug: boolPtr(false)}
	cpMap := map[string]any{}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(hotplug, "memory") {
		t.Errorf("hotplug = %q; 'memory' must be absent (memory_hotplug=false)", hotplug)
	}
}

// TestResolveHotplugNUMA_BothFalse_TokensRemoved verifies that cpu_hotplug=false
// and memory_hotplug=false both remove their respective tokens.
func TestResolveHotplugNUMA_BothFalse_TokensRemoved(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,cpu,memory,network")
	cp := createVMCloudProps{
		CPUHotplug:    boolPtr(false),
		MemoryHotplug: boolPtr(false),
	}
	cpMap := map[string]any{}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hotplug != "disk,network" {
		t.Errorf("hotplug = %q; want exactly %q (both tokens removed, survivor order preserved)", hotplug, "disk,network")
	}
}

// TestResolveHotplugNUMA_PreExistingDiskNetwork_CPUTrue_TokenAppended verifies
// that cpu_hotplug=true appends "cpu" to an existing "disk,network" string.
func TestResolveHotplugNUMA_PreExistingDiskNetwork_CPUTrue_TokenAppended(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,network")
	cp := createVMCloudProps{CPUHotplug: boolPtr(true)}
	cpMap := map[string]any{}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(hotplug, "disk") || !strings.Contains(hotplug, "network") {
		t.Errorf("hotplug = %q; pre-existing 'disk,network' tokens must be preserved", hotplug)
	}
	if !strings.Contains(hotplug, "cpu") {
		t.Errorf("hotplug = %q; 'cpu' must be appended (cpu_hotplug=true)", hotplug)
	}
}

// TestResolveHotplugNUMA_MemoryTrue_ConflictsWithExplicitNUMAFalse_MemoryHotplugWins
// verifies that memory_hotplug=true overrides an explicit cp.NUMA=false because
// PVE requires NUMA for memory hotplug.
func TestResolveHotplugNUMA_MemoryTrue_ConflictsWithExplicitNUMAFalse_MemoryHotplugWins(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,network")
	cp := createVMCloudProps{
		NUMA:          boolPtr(false), // explicit disable
		MemoryHotplug: boolPtr(true),  // requires NUMA
	}
	cpMap := map[string]any{}

	hotplug, numa, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(hotplug, "memory") {
		t.Errorf("hotplug = %q; 'memory' token must be present (memory_hotplug=true)", hotplug)
	}
	// memory_hotplug=true wins over explicit numa=false — PVE requires it.
	if !numa {
		t.Errorf("numaEnabled = false; memory_hotplug=true must force NUMA even when cp.NUMA=false")
	}
}

// TestResolveHotplugNUMA_CPUTrue_IdempotentWhenAlreadyPresent verifies that
// adding "cpu" when it is already in the resolved string does not duplicate it.
func TestResolveHotplugNUMA_CPUTrue_IdempotentWhenAlreadyPresent(t *testing.T) {
	t.Parallel()

	cfg := minimalHotplugConfig("disk,cpu,network")
	cp := createVMCloudProps{CPUHotplug: boolPtr(true)}
	cpMap := map[string]any{}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	count := strings.Count(hotplug, "cpu")
	if count != 1 {
		t.Errorf("hotplug = %q; 'cpu' appears %d times, want exactly 1 (idempotent)", hotplug, count)
	}
}

// --------------------------------------------------------------------------
// helpers local to this file
// --------------------------------------------------------------------------

// minimalHotplugConfig builds a CPIConfig with the given hotplug default and
// NUMA off, used in tests that need a baseline hotplug string.
func minimalHotplugConfig(hotplug string) *config.CPIConfig {
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		Hotplug:   &hotplug,
	}
	return cfg
}
