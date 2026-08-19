package handlers

import (
	"context"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// ============================================================
// Minimal test doubles for stemcell_refs unit tests
// ============================================================

// refsPoolSvc is a PoolService that always succeeds — allows lock acquisition.
type refsPoolSvc struct{}

func (p *refsPoolSvc) AddVM(_ context.Context, _ string, _ int64) error { return nil }
func (p *refsPoolSvc) CreatePool(_ context.Context, _, _ string) error  { return nil }
func (p *refsPoolSvc) DeletePool(_ context.Context, _ string) error     { return nil }
func (p *refsPoolSvc) GetPoolComment(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// refsQEMUSvc satisfies sdkqemu.Service for stemcell_refs tests.
// Only Config is overridden; all other methods panic via the nil embed.
type refsQEMUSvc struct {
	sdkqemu.Service // nil embed — panics on unneeded methods
	configFn        func(ctx context.Context, node string, vmid int) (map[string]any, error)
}

func (q *refsQEMUSvc) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.configFn != nil {
		return q.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
}

// refsNodesSvc satisfies sdknodes.Service for stemcell_refs tests.
// Only UpdateQemuConfig and DeleteQemu are overridden.
type refsNodesSvc struct {
	sdknodes.Service // nil embed
	updateFn         func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	deleteCalled     bool
}

func (n *refsNodesSvc) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if n.updateFn != nil {
		return n.updateFn(ctx, node, vmid, params)
	}
	return nil
}

// DeleteQemu records the destroy call; a nil response means the destroy
// completed synchronously (no UPID to await).
func (n *refsNodesSvc) DeleteQemu(_ context.Context, _, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	n.deleteCalled = true
	return nil, nil
}

// refsClient satisfies pve.Client using the test doubles above.
// Unneeded methods panic via the nil embed.
type refsClient struct {
	pve.Client // nil embed
	q          *refsQEMUSvc
	n          *refsNodesSvc
	p          *refsPoolSvc
}

func (c *refsClient) QEMU() sdkqemu.Service                     { return c.q }
func (c *refsClient) Nodes() sdknodes.Service                   { return c.n }
func (c *refsClient) Pools() pve.PoolService                    { return c.p }
func (c *refsClient) Storage() sdkstorage.Service               { return nil }
func (c *refsClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *refsClient) Tasks() sdktasks.Service                   { return nil }
func (c *refsClient) Cluster() sdkcluster.Service               { return nil }
func (c *refsClient) ClusterStorage() sdkclusterstorage.Service { return nil }
