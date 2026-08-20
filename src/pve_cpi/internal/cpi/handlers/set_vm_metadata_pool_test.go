package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// Tests for set_vm_metadata's pool-membership reconciliation (the tail step
// run under the per-VMID lock): template-layer VMs converge to the current
// vm_pool_template render of their PERSISTED create-time tokens, every other
// layer is never touched, and legacy VMs are adopted only out of the static
// vm_pool. All assertions run through the full handler so the reconcile step
// is exercised exactly where production runs it.

// reconcileFixture bundles the mocks for a reconciliation test. events
// records pool-service calls (create:/get:/delete:/move:); descWrites records
// every description written via UpdateQemuConfig so sentinel persistence can
// be asserted.
type reconcileFixture struct {
	events     []string
	pools      *recordingPoolService
	descWrites []string
	deps       handlers.Deps
}

// newReconcileFixture builds a fixture for vmid 101 on pve-node1 whose QEMU
// config carries desc and whose cluster row reports clusterPool membership.
// The config enables the release-default pool properties (vm_pool "bosh",
// vm_pool_template "bosh-{director}-{deployment}").
func newReconcileFixture(t *testing.T, desc, clusterPool string) *reconcileFixture {
	t.Helper()
	const vmid = 101

	fx := &reconcileFixture{}
	fx.pools = newRecordingPoolService(&fx.events)

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"description": desc, "tags": ""}, nil
		},
	}
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params != nil && params.Description != nil {
				fx.descWrites = append(fx.descWrites, *params.Description)
			}
			return nil
		},
	}

	deps := testDepsFoundVMWithPools(vmid, qemuSvc, nodesSvc, fx.pools)
	if mc, ok := deps.PVE.(*mockPVEClient); ok {
		mc.clusterSvc = &mockClusterSvc{
			listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return clusterVMOnNodeWithPool(vmid, "pve-node1", clusterPool), nil
			},
		}
	}
	deps.Config.VMPool = "bosh"
	deps.Config.VMPoolTemplate = "bosh-{director}-{deployment}"
	deps.Config.StemcellTemplatePool = "bosh-templates"
	fx.deps = deps
	return fx
}

func (fx *reconcileFixture) run(t *testing.T, metadata map[string]any) {
	t.Helper()
	h := handlers.HandleSetVMMetadata(fx.deps)
	if _, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{}); err != nil {
		t.Fatalf("set_vm_metadata: unexpected error: %v", err)
	}
}

func (fx *reconcileFixture) moveEvents() []string {
	var moves []string
	for _, ev := range fx.events {
		if strings.HasPrefix(ev, "move:") {
			moves = append(moves, ev)
		}
	}
	return moves
}

// sentinelDesc builds a description string carrying only a bosh_pool record.
func sentinelDesc(t *testing.T, pm *pve.PoolMembership) string {
	t.Helper()
	desc, err := pve.SetPoolMembershipOnDescription("", pm)
	if err != nil {
		t.Fatalf("SetPoolMembershipOnDescription: %v", err)
	}
	return desc
}

func standardMetadata() map[string]any {
	return map[string]any{
		"director": "d1", "deployment": "dep1", "job": "web", "index": "0",
	}
}

func TestSetVMMetadata_PoolReconcile_TemplateVMInRightPool_NoMove(t *testing.T) {
	t.Parallel()

	desc := sentinelDesc(t, &pve.PoolMembership{
		Name: "bosh-d1-dep1", Layer: pve.PoolLayerTemplate,
		Director: "d1", Deployment: "dep1", InstanceGroup: "web",
	})
	fx := newReconcileFixture(t, desc, "bosh-d1-dep1")
	fx.run(t, standardMetadata())

	if moves := fx.moveEvents(); len(moves) != 0 {
		t.Errorf("expected no moves for a template VM already in its rendered pool, got %v", moves)
	}
	for _, ev := range fx.events {
		if ev == "create:bosh-d1-dep1" {
			t.Errorf("expected no EnsurePoolExists for an already-correct pool; events=%v", fx.events)
		}
	}
}

func TestSetVMMetadata_PoolReconcile_TemplateVMWrongPool_OneMove(t *testing.T) {
	t.Parallel()

	desc := sentinelDesc(t, &pve.PoolMembership{
		Name: "bosh", Layer: pve.PoolLayerTemplate,
		Director: "d1", Deployment: "dep1", InstanceGroup: "web",
	})
	fx := newReconcileFixture(t, desc, "bosh")
	fx.run(t, standardMetadata())

	moves := fx.moveEvents()
	if len(moves) != 1 || moves[0] != "move:bosh-d1-dep1:101" {
		t.Fatalf("expected exactly one move to bosh-d1-dep1, got %v", moves)
	}
	created := false
	for _, ev := range fx.events {
		if ev == "create:bosh-d1-dep1" {
			created = true
		}
	}
	if !created {
		t.Errorf("expected EnsurePoolExists(create) before the move; events=%v", fx.events)
	}
	updatedSentinel := false
	for _, d := range fx.descWrites {
		if strings.Contains(d, `"name":"bosh-d1-dep1"`) {
			updatedSentinel = true
		}
	}
	if !updatedSentinel {
		t.Errorf("expected the bosh_pool sentinel name to be rewritten after the move; writes=%v", fx.descWrites)
	}
}

func TestSetVMMetadata_PoolReconcile_PersistedTokensWinOverMetadata(t *testing.T) {
	t.Parallel()

	// The metadata map claims deployment dep2, but the persisted sentinel
	// says dep1 and the VM already sits in bosh-d1-dep1. Reconciliation must
	// render from the sentinel — no move, ever — so alternating metadata
	// cannot oscillate a VM between two pools.
	desc := sentinelDesc(t, &pve.PoolMembership{
		Name: "bosh-d1-dep1", Layer: pve.PoolLayerTemplate,
		Director: "d1", Deployment: "dep1", InstanceGroup: "web",
	})
	fx := newReconcileFixture(t, desc, "bosh-d1-dep1")
	md := standardMetadata()
	md["deployment"] = "dep2"
	fx.run(t, md)

	if moves := fx.moveEvents(); len(moves) != 0 {
		t.Errorf("expected no moves when persisted tokens already match the current pool, got %v", moves)
	}
}

func TestSetVMMetadata_PoolReconcile_NonTemplateLayersNeverMoved(t *testing.T) {
	t.Parallel()

	for _, layer := range []string{pve.PoolLayerCall, pve.PoolLayerVMType, pve.PoolLayerStatic} {
		layer := layer
		t.Run(layer, func(t *testing.T) {
			t.Parallel()

			desc := sentinelDesc(t, &pve.PoolMembership{
				Name: "team-a", Layer: layer,
				Director: "d1", Deployment: "dep1", InstanceGroup: "web",
			})
			fx := newReconcileFixture(t, desc, "team-a")
			fx.run(t, standardMetadata())

			if moves := fx.moveEvents(); len(moves) != 0 {
				t.Errorf("layer %q must never be moved, got %v", layer, moves)
			}
		})
	}
}

func TestSetVMMetadata_PoolReconcile_LegacyAdoptedOnlyFromStaticPool(t *testing.T) {
	t.Parallel()

	// No sentinel + current pool == cfg.VMPool: the VM was created by a
	// pre-provenance release under the static default. Adopt: move to the
	// template render of the metadata tokens and persist the sentinel.
	fx := newReconcileFixture(t, "", "bosh")
	fx.run(t, standardMetadata())

	moves := fx.moveEvents()
	if len(moves) != 1 || moves[0] != "move:bosh-d1-dep1:101" {
		t.Fatalf("expected legacy adoption move to bosh-d1-dep1, got %v", moves)
	}
	wroteSentinel := false
	for _, d := range fx.descWrites {
		if strings.Contains(d, "bosh_pool") && strings.Contains(d, `"layer":"template"`) {
			wroteSentinel = true
		}
	}
	if !wroteSentinel {
		t.Errorf("expected adoption to persist a template-layer bosh_pool sentinel; writes=%v", fx.descWrites)
	}
}

func TestSetVMMetadata_PoolReconcile_LegacyInOtherPoolUntouched(t *testing.T) {
	t.Parallel()

	fx := newReconcileFixture(t, "", "ops-pool")
	fx.run(t, standardMetadata())

	if moves := fx.moveEvents(); len(moves) != 0 {
		t.Errorf("a legacy VM outside the static vm_pool must never be adopted, got %v", moves)
	}
	for _, d := range fx.descWrites {
		if strings.Contains(d, "bosh_pool") {
			t.Errorf("no bosh_pool sentinel may be written for an untouched legacy VM; writes=%v", fx.descWrites)
		}
	}
}

func TestSetVMMetadata_PoolReconcile_TemplateDisabled_NothingHappens(t *testing.T) {
	t.Parallel()

	fx := newReconcileFixture(t, "", "bosh")
	fx.deps.Config.VMPoolTemplate = ""
	fx.run(t, standardMetadata())

	if moves := fx.moveEvents(); len(moves) != 0 {
		t.Errorf("vm_pool_template \"\" must disable reconciliation entirely, got %v", moves)
	}
}

func TestSetVMMetadata_PoolReconcile_RenderHittingStemcellPoolSkipped(t *testing.T) {
	t.Parallel()

	// Persisted tokens that render the stemcell pool's name must be refused
	// by the shared validator — Warn and skip, never move, never fail the
	// handler (fx.run fails the test on a handler error).
	desc := sentinelDesc(t, &pve.PoolMembership{
		Name: "bosh", Layer: pve.PoolLayerTemplate,
		Director: "", Deployment: "templates", InstanceGroup: "web",
	})
	fx := newReconcileFixture(t, desc, "bosh")
	fx.deps.Config.VMPoolTemplate = "bosh-{director}-{deployment}"
	fx.run(t, standardMetadata())

	if moves := fx.moveEvents(); len(moves) != 0 {
		t.Errorf("a stemcell-pool-colliding render must never move, got %v", moves)
	}
}
