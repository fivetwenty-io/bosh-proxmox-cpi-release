package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

type managedDiskMigration struct {
	locations []string
	scan      int
}
type managedDiskMigrationClient struct {
	managedDiskTestPVE
	migration *managedDiskMigration
}
type managedDiskMigrationNodes struct {
	managedDiskTestNodes
	migration *managedDiskMigration
}
type managedDiskMigrationCluster struct{ managedDiskTestCluster }

func (c managedDiskMigrationClient) Nodes() nodes.Service {
	return managedDiskMigrationNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, migration: c.migration}
}
func (c managedDiskMigrationClient) Cluster() cluster.Service {
	return managedDiskMigrationCluster{managedDiskTestCluster: managedDiskTestCluster{state: c.state}}
}
func (managedDiskMigrationCluster) ListConfigNodes(context.Context) (*cluster.ListConfigNodesResponse, error) {
	r := cluster.ListConfigNodesResponse{json.RawMessage(`{"name":"n1"}`), json.RawMessage(`{"name":"n2"}`)}
	return &r, nil
}
func (managedDiskMigrationCluster) ListResources(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	r := cluster.ListResourcesResponse{json.RawMessage(`{"vmid":321,"node":"n1","type":"qemu"}`)}
	return &r, nil
}
func (n managedDiskMigrationNodes) ListQemu(_ context.Context, node string, _ *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	i := n.migration.scan
	if i >= len(n.migration.locations) {
		i = len(n.migration.locations) - 1
	}
	location := n.migration.locations[i]
	r := nodes.ListQemuResponse{}
	if node == location {
		r = append(r, json.RawMessage(`{"vmid":321,"name":"hinted-vm"}`))
	}
	if node == "n2" {
		n.migration.scan++
	}
	return &r, nil
}

func TestManagedDiskHintUsesActualNodeAndReplansMigration(t *testing.T) {
	for _, locations := range [][]string{{"n2"}, {"n1", "n1", "n2", "n2"}} {
		t.Run(fmt.Sprint(locations), func(t *testing.T) {
			base, _, state := managedDiskFixture(t, "spread", true)
			base.deps.Config.Placement = &config.PlacementConfig{AZMap: map[string][]string{"z": {"n1", "n2"}}}
			migration := &managedDiskMigration{locations: locations}
			deps := base.deps
			deps.PVE = managedDiskMigrationClient{managedDiskTestPVE: managedDiskTestPVE{state: state}, migration: migration}
			r, err := newLayeredResolver(nil, deps.Config)
			if err != nil {
				t.Fatal(err)
			}
			m, err := prepareManagedDisk(t.Context(), deps, base.selection, 1025, createDiskCloudProperties{}, "321", r, nil)
			if err != nil {
				t.Fatal(err)
			}
			if m.plan.Node != "n2" {
				t.Fatalf("trusted stale cluster index: node=%s scans=%d", m.plan.Node, migration.scan)
			}
			if len(locations) > 1 && migration.scan < 4 {
				t.Fatal("migration did not cause fresh same-identity planning")
			}
			if len(state.created) > 0 {
				t.Fatal("planning mutated storage")
			}
		})
	}
}

func TestManagedDiskMissingHintAndNodePolicyFailBeforeMutation(t *testing.T) {
	base, _, state := managedDiskFixture(t, "spread", true)
	r, err := newLayeredResolver(nil, base.deps.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, hint := range []string{"invalid", "-1", "321"} {
		if _, err := prepareManagedDisk(t.Context(), base.deps, base.selection, 1025, createDiskCloudProperties{}, hint, r, nil); err == nil {
			t.Errorf("accepted missing hint %q", hint)
		}
	}
	if _, err := managedDiskNodes(t.Context(), base.deps, createDiskCloudProperties{AvailabilityZone: "unknown"}, ""); err == nil {
		t.Fatal("accepted unknown AZ")
	}
	if len(state.created) > 0 {
		t.Fatal("invalid hint mutated storage")
	}
}
