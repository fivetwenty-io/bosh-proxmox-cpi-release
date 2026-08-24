// Package handlers -- internal tests for cleanupVM's DestroyUnreferencedDisks
// wiring: rollback must honor pve.destroy_unreferenced_disks (default false)
// like every other DeleteQemu call site, rather than hardcoding true.
package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// --------------------------------------------------------------------------
// rbNodesStub -- nodes.Service fake recording DeleteQemu call params.
// --------------------------------------------------------------------------

type rbNodesStub struct {
	sdknodes.Service // embedded nil: panics on any unconfigured method

	deleteCalls []*sdknodes.DeleteQemuParams
}

func (n *rbNodesStub) DeleteQemu(
	_ context.Context, _, _ string, params *sdknodes.DeleteQemuParams,
) (*sdknodes.DeleteQemuResponse, error) {
	n.deleteCalls = append(n.deleteCalls, params)
	return &sdknodes.DeleteQemuResponse{}, nil
}

func (n *rbNodesStub) UpdateQemuConfig(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
	return nil
}

// --------------------------------------------------------------------------
// rbQEMUStub -- qemu.Service fake for Stop + Config.
// --------------------------------------------------------------------------

type rbQEMUStub struct {
	qemu.Service // embedded nil: panics on any unconfigured method
}

func (q *rbQEMUStub) Stop(_ context.Context, _ string, _ int) (string, error) { return "", nil }

func (q *rbQEMUStub) Config(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

// --------------------------------------------------------------------------
// rbClient -- pve.Client fake wiring the stubs above.
// --------------------------------------------------------------------------

type rbClient struct {
	pve.Client
	nodes *rbNodesStub
	qemu  *rbQEMUStub
}

func (c *rbClient) Nodes() sdknodes.Service  { return c.nodes }
func (c *rbClient) QEMU() qemu.Service       { return c.qemu }
func (c *rbClient) Cluster() cluster.Service { return newNAStub() }

// Pools returns nil so tagFailedVM's withVMIDLock falls back to the
// best-effort unlocked path -- these tests focus on DestroyUnreferencedDisks
// wiring, not lock ordering.
func (c *rbClient) Pools() pve.PoolService { return nil }

func rbDeps(destroyUnreferencedDisks bool, nodes *rbNodesStub) Deps {
	return Deps{
		Config: &config.CPIConfig{DestroyUnreferencedDisks: destroyUnreferencedDisks},
		PVE:    &rbClient{nodes: nodes, qemu: &rbQEMUStub{}},
		Logger: log.NewNopLogger(),
	}
}

// TestCleanupVM_DestroyUnreferencedDisks_DefaultFalse verifies that a rollback
// with the config knob at its safe default (false, the zero value) passes
// DestroyUnreferencedDisks=false to DeleteQemu, rather than the previous
// hardcoded true that bypassed the knob on every failed create.
func TestCleanupVM_DestroyUnreferencedDisks_DefaultFalse(t *testing.T) {
	nodes := &rbNodesStub{}
	deps := rbDeps(false, nodes)

	cleanupVM(context.Background(), deps, "pve01", 100, nil, log.NewNopLogger())

	if len(nodes.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteQemu call, got %d", len(nodes.deleteCalls))
	}
	got := nodes.deleteCalls[0].DestroyUnreferencedDisks
	if got == nil || *got {
		t.Errorf("DestroyUnreferencedDisks: want false (config default), got %v", got)
	}
}

// TestCleanupVM_DestroyUnreferencedDisks_NilConfig_DefaultsFalse verifies
// that cleanupVM does not panic when deps.Config is nil (a shape other
// cleanupVM tests deliberately use to isolate config-independent behavior,
// e.g. TestCleanupVM_RemovesNodeAffinityPin_WithoutPinFlag) and treats a nil
// Config as the same safe default as an explicit false.
func TestCleanupVM_DestroyUnreferencedDisks_NilConfig_DefaultsFalse(t *testing.T) {
	nodes := &rbNodesStub{}
	deps := Deps{
		PVE:    &rbClient{nodes: nodes, qemu: &rbQEMUStub{}},
		Logger: log.NewNopLogger(),
	}

	cleanupVM(context.Background(), deps, "pve01", 100, nil, log.NewNopLogger())

	if len(nodes.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteQemu call, got %d", len(nodes.deleteCalls))
	}
	got := nodes.deleteCalls[0].DestroyUnreferencedDisks
	if got == nil || *got {
		t.Errorf("DestroyUnreferencedDisks: want false (nil Config defaults safe), got %v", got)
	}
}

// TestCleanupVM_DestroyUnreferencedDisks_ConfiguredTrue verifies that an
// operator who explicitly opts in to pve.destroy_unreferenced_disks sees that
// choice honored on the rollback path too.
func TestCleanupVM_DestroyUnreferencedDisks_ConfiguredTrue(t *testing.T) {
	nodes := &rbNodesStub{}
	deps := rbDeps(true, nodes)

	cleanupVM(context.Background(), deps, "pve01", 100, nil, log.NewNopLogger())

	if len(nodes.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteQemu call, got %d", len(nodes.deleteCalls))
	}
	got := nodes.deleteCalls[0].DestroyUnreferencedDisks
	if got == nil || !*got {
		t.Errorf("DestroyUnreferencedDisks: want true (operator opt-in), got %v", got)
	}
}
