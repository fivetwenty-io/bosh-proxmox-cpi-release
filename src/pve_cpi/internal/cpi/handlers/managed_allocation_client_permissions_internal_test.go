package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// permissionClient is a client that can read the token's privileges and has
// no PVE services behind it.
type permissionClient struct {
	pve.Client
	privileges map[string]bool
}

func (c *permissionClient) Cluster() cluster.Service               { return nil }
func (c *permissionClient) Nodes() nodes.Service                   { return nil }
func (c *permissionClient) ClusterStorage() clusterstorage.Service { return nil }

func (c *permissionClient) EffectivePermissions(_ context.Context, _ string) (map[string]bool, error) {
	return c.privileges, nil
}

func TestManagedAllocationClientForwardsEffectivePermissions(t *testing.T) {
	wrapped := &managedAllocationClient{Client: &permissionClient{privileges: map[string]bool{"Sys.Console": true}}}

	deps := ha.DepsFromClient(wrapped, nil, nil)
	if deps.Permissions == nil {
		t.Fatal("the HA profile cannot read privileges through the wrapped client")
	}
	got, err := deps.Permissions.EffectivePermissions(context.Background(), "/")
	if err != nil || !got["Sys.Console"] {
		t.Fatalf("privileges = %v, err = %v; want Sys.Console", got, err)
	}
}

func TestManagedAllocationClientWithoutPermissionReaderFails(t *testing.T) {
	wrapped := &managedAllocationClient{Client: &crhListingClient{}}

	if _, err := wrapped.EffectivePermissions(context.Background(), "/"); err == nil {
		t.Fatal("a wrapped client that cannot read privileges must report an error")
	}
}
