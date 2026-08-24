// Package handlers internal tests for the §7.40 ephemeral-disk minimum-size
// invariant (ephemeral >= ratio × RAM) wired into create_vm shape resolution.
package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestEnforceEphemeralMinSize_Disabled verifies that ratio 0 (default) and the
// no-ephemeral-disk case are both no-ops, so behavior is byte-identical.
func TestEnforceEphemeralMinSize_Disabled(t *testing.T) {
	t.Parallel()
	// ratio unset → no check even when the disk would be undersized.
	off := &config.CPIConfig{}
	if err := enforceEphemeralMinSize(off, log.NewNopLogger(), 1 /*GiB*/, 8192 /*MiB*/); err != nil {
		t.Fatalf("ratio 0 must be a no-op, got: %v", err)
	}
	// ratio set but no dedicated ephemeral disk (ephemeralGiB 0) → skipped.
	on := &config.CPIConfig{EphemeralDiskMinRatio: 2}
	if err := enforceEphemeralMinSize(on, log.NewNopLogger(), 0, 8192); err != nil {
		t.Fatalf("no ephemeral disk must be a no-op, got: %v", err)
	}
}

// TestEnforceEphemeralMinSize_Pass verifies the satisfied case and the exact
// boundary (ephemeral == ratio×RAM) both pass.
func TestEnforceEphemeralMinSize_Pass(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{EphemeralDiskMinRatio: 2}
	// 8GiB ephemeral, 4GiB RAM (4096MiB), ratio 2 → required 8.0; 8 >= 8 boundary passes.
	if err := enforceEphemeralMinSize(cfg, log.NewNopLogger(), 8, 4096); err != nil {
		t.Fatalf("boundary equality must pass, got: %v", err)
	}
	// Comfortably above.
	if err := enforceEphemeralMinSize(cfg, log.NewNopLogger(), 20, 4096); err != nil {
		t.Fatalf("above-minimum must pass, got: %v", err)
	}
}

// TestEnforceEphemeralMinSize_FloatBoundary verifies a fractional ratio whose
// product with RAM is a mathematical integer but drifts to N+1e-15 in IEEE-754
// does not falsely reject a disk sized to exactly N GiB.
func TestEnforceEphemeralMinSize_FloatBoundary(t *testing.T) {
	t.Parallel()
	// 0.07 × 100GiB = 7.0 mathematically, but computes to 7.0000000000000009.
	cfg := &config.CPIConfig{EphemeralDiskMinRatio: 0.07}
	if err := enforceEphemeralMinSize(cfg, log.NewNopLogger(), 7 /*GiB*/, 100*1024 /*MiB*/); err != nil {
		t.Fatalf("exact float-boundary disk must pass, got: %v", err)
	}
}

// TestEnforceEphemeralMinSize_Enforce verifies the default mode rejects an
// undersized ephemeral disk with a non-retriable cloud error naming the deficit.
func TestEnforceEphemeralMinSize_Enforce(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{EphemeralDiskMinRatio: 2} // empty mode → enforce
	// 2GiB ephemeral, 8GiB RAM (8192MiB), ratio 2 → required 16; 2 < 16 violates.
	err := enforceEphemeralMinSize(cfg, log.NewNopLogger(), 2, 8192)
	if err == nil {
		t.Fatal("enforce mode must reject an undersized ephemeral disk")
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("ephemeral-min violation is a config error, must NOT be retriable: %v", err)
	}
	if !strings.Contains(err.Error(), "ephemeral") {
		t.Errorf("error should name the ephemeral deficit, got: %v", err)
	}
}

// TestEnforceEphemeralMinSize_Warn verifies warn mode logs and proceeds (no
// error) on the same undersized input.
func TestEnforceEphemeralMinSize_Warn(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{EphemeralDiskMinRatio: 2, EphemeralDiskMinMode: "warn"}
	if err := enforceEphemeralMinSize(cfg, log.NewNopLogger(), 2, 8192); err != nil {
		t.Fatalf("warn mode must not block, got: %v", err)
	}
}

// TestEnforceEphemeralMinSize_NilLoggerWarn verifies warn mode is safe with a
// nil logger (defensive — some early paths leave deps.Logger unset).
func TestEnforceEphemeralMinSize_NilLoggerWarn(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{EphemeralDiskMinRatio: 2, EphemeralDiskMinMode: "warn"}
	if err := enforceEphemeralMinSize(cfg, nil, 2, 8192); err != nil {
		t.Fatalf("warn mode with nil logger must not panic or error, got: %v", err)
	}
}
