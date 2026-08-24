// Package handlers internal tests for pve_config passthrough.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// --------------------------------------------------------------------------
// Stub helpers
// --------------------------------------------------------------------------

// pveConfigNodesStub records UpdateQemuConfig calls and returns a configurable
// error. All other nodes methods panic via the nil-embedded sdknodes.Service.
type pveConfigNodesStub struct {
	sdknodes.Service
	calls  []*sdknodes.UpdateQemuConfigParams
	retErr error
}

func (s *pveConfigNodesStub) UpdateQemuConfig(
	_ context.Context, _ string, _ string,
	p *sdknodes.UpdateQemuConfigParams,
) error {
	s.calls = append(s.calls, p)
	return s.retErr
}

// newPVEConfigDeps builds a minimal Deps with the given nodes stub.
func newPVEConfigDeps(ns *pveConfigNodesStub) Deps {
	return Deps{
		PVE:    &icPVEClient{nodesSvc: ns},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// validatePVEConfig — pre-clone validation (F1: runs before any VM is created)
// --------------------------------------------------------------------------

// TestPVEConfig_ValidateNilNoError verifies nil map passes validation.
func TestPVEConfig_ValidateNilNoError(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(nil); err != nil {
		t.Fatalf("unexpected error for nil map: %v", err)
	}
}

// TestPVEConfig_ValidateEmptyNoError verifies empty map passes validation.
func TestPVEConfig_ValidateEmptyNoError(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(map[string]string{}); err != nil {
		t.Fatalf("unexpected error for empty map: %v", err)
	}
}

// TestPVEConfig_ValidateAllowlistedKeysPass verifies that all four allowlisted
// keys with valid values pass pre-clone validation.
func TestPVEConfig_ValidateAllowlistedKeysPass(t *testing.T) {
	t.Parallel()
	err := validatePVEConfig(map[string]string{"machine": "q35", "bios": "ovmf", "cpu": "host", "serial0": "/dev/ttyS0"})
	if err != nil {
		t.Fatalf("unexpected error for valid keys: %v", err)
	}
}

// TestPVEConfig_ValidateUnlistedKeyRejected verifies that a key not in the
// allowlist ("vga") is rejected by validatePVEConfig (pre-clone, no orphan).
func TestPVEConfig_ValidateUnlistedKeyRejected(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(map[string]string{"vga": "std"}); err == nil {
		t.Fatal("expected error for unlisted key 'vga', got nil")
	}
}

// TestPVEConfig_ValidateBalloonRejected_PointsAtKnob verifies that "balloon"
// (CPI-managed since the ballooning knob shipped) is rejected pre-clone with
// a message redirecting the operator to pve.balloon / cloud_properties.balloon.
func TestPVEConfig_ValidateBalloonRejected_PointsAtKnob(t *testing.T) {
	t.Parallel()
	err := validatePVEConfig(map[string]string{"balloon": "512"})
	if err == nil {
		t.Fatal("expected error for CPI-managed key 'balloon', got nil")
	}
	if !strings.Contains(err.Error(), "managed by the CPI") {
		t.Errorf("error %q should identify balloon as CPI-managed", err)
	}
	if !strings.Contains(err.Error(), "pve.balloon") || !strings.Contains(err.Error(), "cloud_properties.balloon") {
		t.Errorf("error %q should redirect to pve.balloon / cloud_properties.balloon", err)
	}
}

// TestPVEConfig_ValidateBlocklistedCoresRejected verifies that "cores" (CPI-managed)
// is rejected pre-clone.
func TestPVEConfig_ValidateBlocklistedCoresRejected(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(map[string]string{"cores": "4"}); err == nil {
		t.Fatal("expected error for blocklisted key 'cores', got nil")
	}
}

// TestPVEConfig_ValidateBlocklistedNumaRejected verifies that "numa" is rejected
// pre-clone. The hotplug/NUMA resolver owns numa=1; passthrough would conflict.
func TestPVEConfig_ValidateBlocklistedNumaRejected(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(map[string]string{"numa": "1"}); err == nil {
		t.Fatal("expected error for blocklisted key 'numa', got nil")
	}
}

// TestPVEConfig_ValidateArgsRejected verifies that "args" is rejected pre-clone
// as an execution surface.
func TestPVEConfig_ValidateArgsRejected(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(map[string]string{"args": "-device usb-tablet"}); err == nil {
		t.Fatal("expected error for 'args', got nil")
	}
}

// TestPVEConfig_ValidateSemicolonValueRejected verifies that a value containing
// ";" is rejected pre-clone by the shell metachar guard.
func TestPVEConfig_ValidateSemicolonValueRejected(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(map[string]string{"machine": "q35;evil"}); err == nil {
		t.Fatal("expected error for value containing ';', got nil")
	}
}

// TestPVEConfig_ValidateBacktickValueRejected verifies that a value containing
// a backtick is rejected pre-clone.
func TestPVEConfig_ValidateBacktickValueRejected(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(map[string]string{"cpu": "host`id`"}); err == nil {
		t.Fatal("expected error for value containing backtick, got nil")
	}
}

// TestPVEConfig_ValidateEmptyValueRejected verifies that an empty string value
// is rejected (would blank the PVE field).
func TestPVEConfig_ValidateEmptyValueRejected(t *testing.T) {
	t.Parallel()
	if err := validatePVEConfig(map[string]string{"machine": ""}); err == nil {
		t.Fatal("expected error for empty value, got nil")
	}
}

// --------------------------------------------------------------------------
// applyPVEConfigPassthrough — API-call behavior with pre-validated input
// --------------------------------------------------------------------------

// TestPVEConfig_AllowlistedMachine verifies that "machine" is applied via
// UpdateQemuConfig with the supplied value.
func TestPVEConfig_AllowlistedMachine(t *testing.T) {
	t.Parallel()

	ns := &pveConfigNodesStub{}
	deps := newPVEConfigDeps(ns)

	err := applyPVEConfigPassthrough(context.Background(), deps, "node1", 100,
		map[string]string{"machine": "q35"}, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ns.calls) != 1 {
		t.Fatalf("expected 1 UpdateQemuConfig call, got %d", len(ns.calls))
	}
	got := ns.calls[0]
	if got.Machine == nil || *got.Machine != "q35" {
		t.Errorf("Machine: got %v, want %q", got.Machine, "q35")
	}
}

// TestPVEConfig_AllowlistedBios verifies that "bios" is applied correctly.
func TestPVEConfig_AllowlistedBios(t *testing.T) {
	t.Parallel()

	ns := &pveConfigNodesStub{}
	deps := newPVEConfigDeps(ns)

	err := applyPVEConfigPassthrough(context.Background(), deps, "node1", 101,
		map[string]string{"bios": "ovmf"}, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ns.calls) != 1 {
		t.Fatalf("expected 1 UpdateQemuConfig call, got %d", len(ns.calls))
	}
	got := ns.calls[0]
	if got.Bios == nil || *got.Bios != "ovmf" {
		t.Errorf("Bios: got %v, want %q", got.Bios, "ovmf")
	}
}

// TestPVEConfig_AllowlistedCPU verifies that "cpu" is applied correctly.
func TestPVEConfig_AllowlistedCPU(t *testing.T) {
	t.Parallel()

	ns := &pveConfigNodesStub{}
	deps := newPVEConfigDeps(ns)

	err := applyPVEConfigPassthrough(context.Background(), deps, "node1", 102,
		map[string]string{"cpu": "host"}, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ns.calls) != 1 {
		t.Fatalf("expected 1 UpdateQemuConfig call, got %d", len(ns.calls))
	}
	got := ns.calls[0]
	if got.Cpu == nil || *got.Cpu != "host" {
		t.Errorf("Cpu: got %v, want %q", got.Cpu, "host")
	}
}

// TestPVEConfig_NilMapNoCall verifies that a nil cfg produces zero API calls.
func TestPVEConfig_NilMapNoCall(t *testing.T) {
	t.Parallel()

	ns := &pveConfigNodesStub{}
	deps := newPVEConfigDeps(ns)

	err := applyPVEConfigPassthrough(context.Background(), deps, "node1", 103,
		nil, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ns.calls) != 0 {
		t.Errorf("expected 0 UpdateQemuConfig calls for nil map, got %d", len(ns.calls))
	}
}

// TestPVEConfig_EmptyMapNoCall verifies that an empty cfg produces zero API calls.
func TestPVEConfig_EmptyMapNoCall(t *testing.T) {
	t.Parallel()

	ns := &pveConfigNodesStub{}
	deps := newPVEConfigDeps(ns)

	err := applyPVEConfigPassthrough(context.Background(), deps, "node1", 104,
		map[string]string{}, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ns.calls) != 0 {
		t.Errorf("expected 0 UpdateQemuConfig calls for empty map, got %d", len(ns.calls))
	}
}

// TestPVEConfig_MultipleValidKeysSingleCall verifies that multiple valid keys
// are applied in a SINGLE UpdateQemuConfig call (not one call per key).
func TestPVEConfig_MultipleValidKeysSingleCall(t *testing.T) {
	t.Parallel()

	ns := &pveConfigNodesStub{}
	deps := newPVEConfigDeps(ns)

	err := applyPVEConfigPassthrough(context.Background(), deps, "node1", 105,
		map[string]string{"machine": "q35", "bios": "ovmf", "cpu": "host"},
		log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ns.calls) != 1 {
		t.Fatalf("expected exactly 1 UpdateQemuConfig call for multiple valid keys, got %d", len(ns.calls))
	}
	got := ns.calls[0]
	if got.Machine == nil || *got.Machine != "q35" {
		t.Errorf("Machine: got %v, want %q", got.Machine, "q35")
	}
	if got.Bios == nil || *got.Bios != "ovmf" {
		t.Errorf("Bios: got %v, want %q", got.Bios, "ovmf")
	}
	if got.Cpu == nil || *got.Cpu != "host" {
		t.Errorf("Cpu: got %v, want %q", got.Cpu, "host")
	}
}

// TestPVEConfig_Serial0OverrideAppliesAsFinalWrite verifies that
// pve_config.serial0 is accepted by the allowlist and mapped onto the
// UpdateQemuConfig Serial[0] index — the same call that runs after both
// create paths' default serial0=socket write, so it is the final value
// applied.
func TestPVEConfig_Serial0OverrideAppliesAsFinalWrite(t *testing.T) {
	t.Parallel()

	ns := &pveConfigNodesStub{}
	deps := newPVEConfigDeps(ns)

	err := applyPVEConfigPassthrough(context.Background(), deps, "node1", 106,
		map[string]string{"serial0": "/dev/ttyS0"}, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ns.calls) != 1 {
		t.Fatalf("expected exactly 1 UpdateQemuConfig call, got %d", len(ns.calls))
	}
	got := ns.calls[0]
	if got.Serial == nil {
		t.Fatal("Serial map is nil; want index 0 set")
	}
	if v := got.Serial[0]; v != "/dev/ttyS0" {
		t.Errorf("Serial[0]: got %q, want %q", v, "/dev/ttyS0")
	}
}

// TestPVEConfig_APIErrorPropagated verifies that an error from UpdateQemuConfig
// is wrapped and returned (not swallowed).
func TestPVEConfig_APIErrorPropagated(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("PVE API fault")
	ns := &pveConfigNodesStub{retErr: sentinel}
	deps := newPVEConfigDeps(ns)

	err := applyPVEConfigPassthrough(context.Background(), deps, "node1", 113,
		map[string]string{"machine": "q35"}, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error from API call, got nil")
	}
}

// --------------------------------------------------------------------------
// F2: integration-level tests — bad key fails before clone; API error
// triggers VM cleanup (via parseCreateVMArgs and attemptCreateVM).
// --------------------------------------------------------------------------

// marshalCreateVMArgs marshals a 6-element slice into []json.RawMessage for
// use with parseCreateVMArgs. Each element is marshalled independently.
func marshalCreateVMArgs(t *testing.T, elems []any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(elems))
	for i, e := range elems {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshalCreateVMArgs[%d]: %v", i, err)
		}
		out[i] = b
	}
	return out
}

// TestPVEConfig_BadKeyRejectedPreClone verifies that parseCreateVMArgs returns
// a non-retriable CloudError for a blocklisted pve_config key before any clone
// can occur. The error fires inside parseCreateVMArgs, so no clone call is
// ever reached — zero orphans.
func TestPVEConfig_BadKeyRejectedPreClone(t *testing.T) {
	t.Parallel()

	args := marshalCreateVMArgs(t, []any{
		"agent-id-1",
		":light:test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2",
		map[string]any{
			"cpu":    1,
			"memory": 512,
			"pve_config": map[string]string{
				"cores": "8", // blocklisted — CPI-managed
			},
		},
		map[string]any{},
		[]string{},
		map[string]any{},
	})

	_, err := parseCreateVMArgs(args)
	if err == nil {
		t.Fatal("expected CloudError for blocklisted pve_config key 'cores', got nil")
	}
}

// TestPVEConfig_EmptyValueRejectedPreClone verifies that an empty pve_config
// value is rejected by parseCreateVMArgs before any clone.
func TestPVEConfig_EmptyValueRejectedPreClone(t *testing.T) {
	t.Parallel()

	args := marshalCreateVMArgs(t, []any{
		"agent-id-2",
		":light:test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2",
		map[string]any{
			"cpu":    1,
			"memory": 512,
			"pve_config": map[string]string{
				"machine": "", // empty value — would blank PVE field
			},
		},
		map[string]any{},
		[]string{},
		map[string]any{},
	})

	_, err := parseCreateVMArgs(args)
	if err == nil {
		t.Fatal("expected CloudError for empty pve_config value, got nil")
	}
}
