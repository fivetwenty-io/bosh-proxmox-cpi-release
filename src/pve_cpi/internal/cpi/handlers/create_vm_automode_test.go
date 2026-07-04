package handlers_test

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// buildAutoModeDeps constructs Deps with the given agent_mode, AgentMBus, and
// primary agent. Uses the minimal PVE mocks from buildVMDeps that succeed for
// the full create_vm path so agent selection and completeness assertions are
// the only failure modes under test.
//
// The registryAgent parameter is ignored — registry mode is removed. It is
// kept in the signature for callers that have not yet been updated; pass nil.
func buildAutoModeDeps(
	agentMode string,
	agentMBus string,
	primaryAgent *vmMockAgent,
	_ *vmMockAgent, // was registryAgent; registry mode removed
) handlers.Deps {
	d := buildVMDeps(&vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, primaryAgent)
	d.Config = &config.CPIConfig{
		Node:                "pve",
		VMStorage:           storageName,
		NetworkBridge:       "vmbr0",
		VMIDRangeStart:      100,
		AgentMode:           agentMode,
		AgentMBus:           agentMBus,
		ISOStorage:          "local",
		Placement:           &config.PlacementConfig{Enabled: placementDisabled},
		EnsureNoIPConflicts: placementDisabled,
	}
	d.Agent = primaryAgent
	d.Logger = log.NewNopLogger()
	return d
}

// mkCtxWithAPIVersion builds a jsonrpc.Context carrying stemcell api_version
// inside VM["stemcell"]["api_version"] as float64.
func mkCtxWithAPIVersion(requestID string, apiVersion float64) jsonrpc.Context {
	return jsonrpc.Context{
		RequestID: requestID,
		VM: map[string]any{
			"stemcell": map[string]any{
				"api_version": apiVersion,
			},
		},
	}
}

// mkCtxNoAPIVersion builds a jsonrpc.Context with no VM or Stemcell fields,
// matching the create-env path where no stemcell context is sent.
func mkCtxNoAPIVersion(requestID string) jsonrpc.Context {
	return jsonrpc.Context{RequestID: requestID}
}

// defaultNetwork returns a minimal valid BOSH network spec for tests that do
// not exercise network-specific behavior.
func defaultNetwork() map[string]any {
	return map[string]any{
		"default": map[string]any{
			"type":             "manual",
			"ip":               "10.0.0.5",
			"netmask":          "255.255.255.0",
			"gateway":          "10.0.0.1",
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		},
	}
}

// --------------------------------------------------------------------------
// selectAgentForCall — 7 matrix cases exercised via HandleCreateVM
// --------------------------------------------------------------------------

// TestAutoMode_V2Stemcell_NilReg verifies that auto mode with api_version=2
// uses the configdrive agent without error.
func TestAutoMode_V2Stemcell_NilReg(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("auto", "nats://10.0.0.1:4222", primary, nil))

	_, err := h.Handle(context.Background(),
		mkArgs("ag-1", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxWithAPIVersion("av2-nilreg", 2.0))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(primary.configureCalls) != 1 {
		t.Errorf("primary.configureCalls: want 1, got %d", len(primary.configureCalls))
	}
}

// TestAutoMode_V1Stemcell_UsesConfigDrive verifies that api_version=1 in auto
// mode selects the primary configdrive agent (registry path removed).
func TestAutoMode_V1Stemcell_UsesConfigDrive(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("auto", "nats://10.0.0.1:4222", primary, nil))

	_, err := h.Handle(context.Background(),
		mkArgs("ag-3", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxWithAPIVersion("av1-configdrive", 1.0))
	if err != nil {
		t.Fatalf("expected no error for v1 stemcell (configdrive path), got: %v", err)
	}
	if len(primary.configureCalls) != 1 {
		t.Errorf("primary.configureCalls: want 1, got %d", len(primary.configureCalls))
	}
}

// TestAutoMode_APIVersionAbsent_DefaultsConfigDrive verifies that absent
// api_version (nil VM context, create-env path) selects the primary configdrive
// agent and does not error.
func TestAutoMode_APIVersionAbsent_DefaultsConfigDrive(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("auto", "nats://10.0.0.1:4222", primary, nil))

	_, err := h.Handle(context.Background(),
		mkArgs("ag-5", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxNoAPIVersion("absent-apiversion"))
	if err != nil {
		t.Fatalf("expected no error for absent api_version (fail-open), got: %v", err)
	}
	if len(primary.configureCalls) != 1 {
		t.Errorf("primary.configureCalls: want 1, got %d", len(primary.configureCalls))
	}
}

// TestAutoMode_ExplicitCloudinit_Unchanged verifies that explicit agent_mode=cloudinit
// always uses deps.Agent regardless of api_version.
func TestAutoMode_ExplicitCloudinit_Unchanged(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	reg := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("cloudinit", "nats://10.0.0.1:4222", primary, reg))

	_, err := h.Handle(context.Background(),
		mkArgs("ag-6", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxWithAPIVersion("explicit-ci", 1.0))
	if err != nil {
		t.Fatalf("expected no error for explicit cloudinit, got: %v", err)
	}
	if len(primary.configureCalls) != 1 {
		t.Errorf("primary.configureCalls: want 1, got %d", len(primary.configureCalls))
	}
	if len(reg.configureCalls) != 0 {
		t.Errorf("registry agent must NOT be called for explicit cloudinit, got %d calls", len(reg.configureCalls))
	}
}

// TestAutoMode_ExplicitNoagent_Unchanged verifies that explicit agent_mode=noagent
// is a no-op (no Configure call) regardless of api_version.
func TestAutoMode_ExplicitNoagent_Unchanged(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("noagent", "", primary, nil))

	_, err := h.Handle(context.Background(),
		mkArgs("ag-7", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxWithAPIVersion("explicit-noagent", 2.0))
	if err != nil {
		t.Fatalf("expected no error for explicit noagent, got: %v", err)
	}
	if len(primary.configureCalls) != 0 {
		t.Errorf("noagent must not call Configure, got %d calls", len(primary.configureCalls))
	}
}

// --------------------------------------------------------------------------
// assertRegistryLessCompleteness — 6 cases
// --------------------------------------------------------------------------

// TestCompletenessAssertion_MissingMBus verifies that a cloudinit agent with no
// mbus (neither env nor config.AgentMBus) returns a Cloud error. The mbus
// assertion fires inside configureAgent, after VM creation; rollback destroys
// the VM. Configure is never reached.
func TestCompletenessAssertion_MissingMBus(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("cloudinit", "", primary, nil)) // AgentMBus intentionally empty

	_, err := h.Handle(context.Background(),
		mkArgs("ag-assert-mbus", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxNoAPIVersion("assert-mbus"))
	if err == nil {
		t.Fatal("expected Cloud error for empty mbus")
	}
	if !isCloudError(err) {
		t.Errorf("expected Cloud error, got: %v", err)
	}
	if len(primary.configureCalls) != 0 {
		t.Errorf("Configure must not be called when assertion fails, got %d calls", len(primary.configureCalls))
	}
}

// TestCompletenessAssertion_EmptyNetworks verifies that a cloudinit agent with no
// networks returns a Cloud error (empty networks arg → empty agent network map).
func TestCompletenessAssertion_EmptyNetworks(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("cloudinit", "nats://10.0.0.1:4222", primary, nil))

	// Pass empty networks map — buildAgentNetworks produces an empty map.
	_, err := h.Handle(context.Background(),
		mkArgs("ag-assert-nets", testStemcellCID, map[string]any{}, map[string]any{}, []string{}, map[string]any{}),
		mkCtxNoAPIVersion("assert-nets"))
	if err == nil {
		t.Fatal("expected Cloud error for empty networks")
	}
	if !isCloudError(err) {
		t.Errorf("expected Cloud error, got: %v", err)
	}
	if len(primary.configureCalls) != 0 {
		t.Errorf("Configure must not be called when assertion fails, got %d calls", len(primary.configureCalls))
	}
}

// TestCompletenessAssertion_NoagentNoAssert verifies that agent_mode=noagent with
// an empty mbus does NOT trigger the completeness assertion (noagent skips Configure).
func TestCompletenessAssertion_NoagentNoAssert(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("noagent", "", primary, nil))

	_, err := h.Handle(context.Background(),
		mkArgs("ag-noagent-noassert", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxNoAPIVersion("noagent-noassert"))
	if err != nil {
		t.Fatalf("noagent must not trigger completeness assertion, got: %v", err)
	}
	if len(primary.configureCalls) != 0 {
		t.Errorf("noagent Configure must not be called, got %d", len(primary.configureCalls))
	}
}

// TestCompletenessAssertion_HappyPath verifies that a fully configured cloudinit
// agent results in exactly one Configure call and no error.
func TestCompletenessAssertion_HappyPath(t *testing.T) {
	t.Parallel()
	primary := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildAutoModeDeps("cloudinit", "nats://10.0.0.1:4222", primary, nil))

	_, err := h.Handle(context.Background(),
		mkArgs("ag-happy", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxNoAPIVersion("assert-happy"))
	if err != nil {
		t.Fatalf("expected no error for fully configured cloudinit, got: %v", err)
	}
	if len(primary.configureCalls) != 1 {
		t.Errorf("configureCalls: want 1, got %d", len(primary.configureCalls))
	}
}
