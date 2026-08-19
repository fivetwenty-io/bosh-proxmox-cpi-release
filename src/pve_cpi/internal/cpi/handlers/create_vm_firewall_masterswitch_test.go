// Package handlers_test -- black-box tests for the §1.4 datacenter firewall
// master-switch probe: create_vm, when any firewall feature is in play,
// verifies once per process that PVE's cluster-wide firewall master switch is
// actually enabled, since VM-level firewall rules are silently unenforced
// otherwise.
package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// masterSwitchOptionsFn returns a listFirewallOptionsFn reporting the
// datacenter firewall master switch as enabled (enable != 0) or disabled
// (enable == 0), matching PVE's integer-boolean encoding.
func masterSwitchOptionsFn(enable int64) func(context.Context) (*sdkcluster.ListFirewallOptionsResponse, error) {
	return func(context.Context) (*sdkcluster.ListFirewallOptionsResponse, error) {
		enabled := sdkclient.PVEInt(enable)
		return &sdkcluster.ListFirewallOptionsResponse{Enable: &enabled}, nil
	}
}

// countWarnsContaining returns how many Warn-level entries in obs contain substr.
func countWarnsContaining(obs *log.Observer, substr string) int {
	n := 0
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, substr) {
			n++
		}
	}
	return n
}

// netMapWithVIPsOnly returns a single-NIC args[3] map carrying
// allowed_address_pairs but no per-NIC or global firewall flag -- isolates
// the third firewallFeatureInPlay trigger (ipfilter/allowed_address_pairs
// seeding) from the other two (security_groups, vm_firewall).
func netMapWithVIPsOnly() map[string]any {
	return map[string]any{
		"default": map[string]any{
			"type": "manual", "ip": "10.0.0.5",
			"netmask": "255.255.255.0", "gateway": "10.0.0.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{
				"bridge":                "vmbr0",
				"allowed_address_pairs": []any{"10.0.0.9"},
			},
		},
	}
}

func TestCreateVM_FirewallMasterSwitchDisabled_WarnsOnce(t *testing.T) {
	// Not parallel -- manipulates the package-level sync.Once.
	handlers.ResetFirewallMasterSwitchProbeOnce()
	t.Cleanup(handlers.ResetFirewallMasterSwitchProbeOnce)

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("web")}
	a := &vmMockAgent{}
	obsLogger, obs := log.NewObservedLogger(log.LevelWarn)

	deps := buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{securityGroups: []string{"web"}})
	deps.Logger = obsLogger
	h := handlers.HandleCreateVM(deps)

	c.listFirewallOptionsFn = masterSwitchOptionsFn(0)

	args := mkArgs("agent-fw-mswitch-off", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("fw-mswitch-off")); err != nil {
		t.Fatalf("master switch disabled must not fail create_vm: %v", err)
	}
	if c.listFirewallOptionsCalls != 1 {
		t.Errorf("listFirewallOptionsCalls = %d; want 1", c.listFirewallOptionsCalls)
	}
	if got := countWarnsContaining(obs, "master switch is disabled"); got != 1 {
		t.Errorf("expected exactly 1 master-switch-disabled Warn, got %d (entries: %+v)", got, obs.All())
	}
}

func TestCreateVM_FirewallMasterSwitchEnabled_NoWarn(t *testing.T) {
	handlers.ResetFirewallMasterSwitchProbeOnce()
	t.Cleanup(handlers.ResetFirewallMasterSwitchProbeOnce)

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("web")}
	a := &vmMockAgent{}
	obsLogger, obs := log.NewObservedLogger(log.LevelWarn)

	deps := buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{securityGroups: []string{"web"}})
	deps.Logger = obsLogger
	h := handlers.HandleCreateVM(deps)

	c.listFirewallOptionsFn = masterSwitchOptionsFn(1)

	args := mkArgs("agent-fw-mswitch-on", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("fw-mswitch-on")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.listFirewallOptionsCalls != 1 {
		t.Errorf("listFirewallOptionsCalls = %d; want 1 (probe still runs to verify)", c.listFirewallOptionsCalls)
	}
	if got := countWarnsContaining(obs, "master switch"); got != 0 {
		t.Errorf("master switch enabled: expected no master-switch Warn, got %d (entries: %+v)", got, obs.All())
	}
}

// TestCreateVM_FirewallMasterSwitchProbe_OnceAcrossTwoCalls verifies the
// probe's core once-per-process contract: two separate create_vm calls, both
// requesting a firewall feature, must result in exactly one
// GET /cluster/firewall/options across the pair.
func TestCreateVM_FirewallMasterSwitchProbe_OnceAcrossTwoCalls(t *testing.T) {
	handlers.ResetFirewallMasterSwitchProbeOnce()
	t.Cleanup(handlers.ResetFirewallMasterSwitchProbeOnce)

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("web")}
	a := &vmMockAgent{}
	c.listFirewallOptionsFn = masterSwitchOptionsFn(0)

	deps := buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{securityGroups: []string{"web"}})
	h := handlers.HandleCreateVM(deps)

	args1 := mkArgs("agent-fw-once-1", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})
	if _, err := h.Handle(context.Background(), args1, mkCtx("fw-once-1")); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}

	args2 := mkArgs("agent-fw-once-2", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})
	if _, err := h.Handle(context.Background(), args2, mkCtx("fw-once-2")); err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}

	if c.listFirewallOptionsCalls != 1 {
		t.Errorf("listFirewallOptionsCalls across 2 create_vm calls = %d; want exactly 1", c.listFirewallOptionsCalls)
	}
}

// TestCreateVM_FirewallMasterSwitchProbe_FailOpenOnError verifies that a probe
// failure (e.g. 403 from a token lacking Sys.Audit) never fails create_vm and
// logs exactly one "could not verify" Warn instead of the enable/disable message.
func TestCreateVM_FirewallMasterSwitchProbe_FailOpenOnError(t *testing.T) {
	handlers.ResetFirewallMasterSwitchProbeOnce()
	t.Cleanup(handlers.ResetFirewallMasterSwitchProbeOnce)

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{listFirewallGroupsFn: firewallGroupsClusterFn("web")}
	a := &vmMockAgent{}
	obsLogger, obs := log.NewObservedLogger(log.LevelWarn)

	deps := buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{securityGroups: []string{"web"}})
	deps.Logger = obsLogger
	h := handlers.HandleCreateVM(deps)

	c.listFirewallOptionsFn = func(context.Context) (*sdkcluster.ListFirewallOptionsResponse, error) {
		return nil, &permissionDeniedErr{}
	}

	args := mkArgs("agent-fw-probe-error", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("fw-probe-error")); err != nil {
		t.Fatalf("probe error must fail open, not fail create_vm: %v", err)
	}
	if got := countWarnsContaining(obs, "could not verify"); got != 1 {
		t.Errorf("expected exactly 1 could-not-verify Warn, got %d (entries: %+v)", got, obs.All())
	}
	if got := countWarnsContaining(obs, "master switch is disabled"); got != 0 {
		t.Errorf("probe error path must not also log the disabled message, got %d", got)
	}
}

// permissionDeniedErr is a minimal error used to simulate a 403 from PVE
// without depending on the SDK's APIError construction helpers.
type permissionDeniedErr struct{}

func (e *permissionDeniedErr) Error() string { return "403 Forbidden: Sys.Audit required" }

// TestCreateVM_FirewallMasterSwitchProbe_NoFeatureInPlay_NoProbe verifies that
// a VM requesting no firewall feature at all triggers zero
// GET /cluster/firewall/options calls -- the byte-identical-when-unused
// contract this CPI applies to every optional feature.
func TestCreateVM_FirewallMasterSwitchProbe_NoFeatureInPlay_NoProbe(t *testing.T) {
	handlers.ResetFirewallMasterSwitchProbeOnce()
	t.Cleanup(handlers.ResetFirewallMasterSwitchProbeOnce)

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{} // no listFirewallOptionsFn wired -- would panic-free-return but must not be called
	a := &vmMockAgent{}

	deps := buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-fw-none", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("fw-none")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.listFirewallOptionsCalls != 0 {
		t.Errorf("no firewall feature in play: expected 0 ListFirewallOptions calls, got %d", c.listFirewallOptionsCalls)
	}
}

// TestCreateVM_FirewallMasterSwitchProbe_VIPOnlyTriggersProbe verifies that a
// NIC declaring allowed_address_pairs alone (no security_groups, no global
// vm_firewall flag) is sufficient to trigger the master-switch probe -- the
// third firewallFeatureInPlay condition, isolated from the other two.
func TestCreateVM_FirewallMasterSwitchProbe_VIPOnlyTriggersProbe(t *testing.T) {
	handlers.ResetFirewallMasterSwitchProbeOnce()
	t.Cleanup(handlers.ResetFirewallMasterSwitchProbeOnce)

	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	c.listFirewallOptionsFn = masterSwitchOptionsFn(1)

	deps := buildVMDepsFirewall(q, n, c, a, vmFirewallDepsOpts{})
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-fw-vip-only", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		netMapWithVIPsOnly(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("fw-vip-only")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.listFirewallOptionsCalls != 1 {
		t.Errorf("allowed_address_pairs alone must trigger the probe: listFirewallOptionsCalls = %d, want 1", c.listFirewallOptionsCalls)
	}
}
