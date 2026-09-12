package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"strings"
	"testing"
)

type managedVMRouteClient struct {
	pve.Client
	service *managedVMRouteCluster
}

func (c *managedVMRouteClient) Cluster() cluster.Service { return c.service }

type managedVMRouteCluster struct {
	cluster.Service
	created, running, unknown bool
	creates, applies, deletes int
}

func (c *managedVMRouteCluster) GetSdnVnets(context.Context, string, *cluster.GetSdnVnetsParams) (*cluster.GetSdnVnetsResponse, error) {
	raw := json.RawMessage(`{"vnet":"router","zone":"evpn"}`)
	return &raw, nil
}
func (c *managedVMRouteCluster) GetSdnZones(context.Context, string, *cluster.GetSdnZonesParams) (*cluster.GetSdnZonesResponse, error) {
	raw := json.RawMessage(`{"zone":"evpn","type":"evpn"}`)
	return &raw, nil
}
func (c *managedVMRouteCluster) ListSdnVnetsSubnets(_ context.Context, _ string, p *cluster.ListSdnVnetsSubnetsParams) (*cluster.ListSdnVnetsSubnetsResponse, error) {
	rows := cluster.ListSdnVnetsSubnetsResponse{}
	present := c.created
	if p.Running != nil && *p.Running {
		present = c.running
	}
	if present {
		rows = append(rows, json.RawMessage(`{"subnet":"evpn-10.60.0.0-16","vnet":"router","type":"subnet"}`))
	}
	return &rows, nil
}
func (c *managedVMRouteCluster) CreateSdnVnetsSubnets(_ context.Context, vnet string, p *cluster.CreateSdnVnetsSubnetsParams) error {
	c.creates++
	if vnet != "router" || p.Subnet != "10.60.0.0/16" {
		return fmt.Errorf("wrong requested route")
	}
	c.created = true
	return nil
}
func (c *managedVMRouteCluster) UpdateSdn(context.Context, *cluster.UpdateSdnParams) (*cluster.UpdateSdnResponse, error) {
	c.applies++
	c.running = c.created
	if c.unknown {
		return nil, fmt.Errorf("connection lost after apply")
	}
	return nil, nil
}
func (c *managedVMRouteCluster) DeleteSdnVnetsSubnets(context.Context, string, string, *cluster.DeleteSdnVnetsSubnetsParams) error {
	c.deletes++
	c.created = false
	return nil
}
func TestManagedVMRouteRequiresRunningEvidence(t *testing.T) {
	c := &managedVMRouteCluster{created: true}
	m := &managedVMAllocation{deps: Deps{PVE: &managedVMRouteClient{service: c}}, parsed: &createVMParsedArgs{cloudProps: createVMCloudProps{AdvertisedRoutes: []AdvertisedRoute{{VNet: "router", Destination: "10.60.0.0/16"}}}}}
	var raw *json.RawMessage
	err := m.observeSDNMutation(context.Background(), ManagedAllocationMutation{Method: "UpdateSdn"}, "step", raw)
	if err == nil || !strings.Contains(err.Error(), "running configuration") {
		t.Fatalf("pending subnet mistaken for applied: %v", err)
	}
	c.running = true
	if err := m.observeSDNMutation(context.Background(), ManagedAllocationMutation{Method: "UpdateSdn"}, "step", raw); err != nil {
		t.Fatal(err)
	}
}
